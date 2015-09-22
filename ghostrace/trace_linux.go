package ghostrace

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"./memio"
	"./process"
	"./sys"
	"./sys/call"
)

type execCb func(c *call.Execve) bool

type Tracer interface {
	ExecFilter(cb execCb)
	Spawn(cmd string, args ...string) (chan *Event, error)
	Trace(pid int) (chan *Event, error)
}

type LinuxTracer struct {
	execFilter execCb
}

func NewTracer() Tracer {
	return &LinuxTracer{}
}

func (t *LinuxTracer) ExecFilter(cb execCb) {
	t.execFilter = cb
}

func (t *LinuxTracer) Spawn(cmd string, args ...string) (chan *Event, error) {
	pid, err := syscall.ForkExec(cmd, args, &syscall.ProcAttr{
		Sys:   &syscall.SysProcAttr{Ptrace: true},
		Files: []uintptr{0, 1, 2},
	})
	if err != nil {
		return nil, err
	}
	return t.traceProcess(pid, true)
}

func (t *LinuxTracer) Trace(pid int) (chan *Event, error) {
	return t.traceProcess(pid, false)
}

func (t *LinuxTracer) traceProcess(pid int, spawned bool) (chan *Event, error) {
	ret := make(chan *Event)
	errChan := make(chan error)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(ret)

		spawnChild := -1
		if spawned {
			spawnChild = pid
		} else {
			if err := syscall.PtraceAttach(pid); err != nil {
				errChan <- err
				return
			}
		}
		errChan <- nil

		first := true
		table := make(map[int]*tracedProc)
		// we need to catch interrupts so we don't leave other processes in a bad state
		// TODO: make the interrupt catching behavior optional (but default)?
		var interrupted syscall.Signal
		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGPIPE, syscall.SIGQUIT)
		go func() {
			for sig := range signalChan {
				// TODO: send an interrupt event back over the channel?
				// otherwise just make the other side also listen for interrupts
				interrupted = sig.(syscall.Signal)
			}
		}()
		for interrupted == 0 {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WALL, nil)
			if err != nil {
				if err == syscall.EINTR {
					continue
				} else if err == syscall.ECHILD && len(table) == 0 {
					break
				}
				fmt.Println("DEBUG:", err)
				break
			}
			traced, ok := table[pid]
			if status.Exited() && ok {
				// process exit
				// TODO: send an event into channel here
				delete(table, pid)
				continue
			}
			if !status.Stopped() {
				continue
			}
			sig := status.StopSignal()
			if !ok {
				proc, err := process.FindPid(pid)
				if err != nil {
					fmt.Println("DEBUG:", err)
					continue
				}
				t, err := newTracedProc(proc, first && !spawned)
				if err != nil {
					fmt.Println("DEBUG:", err)
					continue
				}
				first = false
				traced = t
				table[pid] = t
			}
			if status.TrapCause() != -1 {
				// PTRACE_EVENT_*
				sig = syscall.Signal(0)
			} else if sig == syscall.SIGSTOP && traced.EatOneSigstop {
				// look for a SIGSTOP
				traced.EatOneSigstop = false
				sig = syscall.Signal(0)
			} else if interrupted != 0 {
				break
			}
			if sig == syscall.SIGTRAP|0x80 {
				// handle a syscall
				var sc sys.Syscall
				var err error
				if sc, err = traced.Syscall(); err != nil {
					fmt.Println("DEBUG:", err)
					continue
				}
				if sc != nil {
					ret <- &Event{
						Process: traced.Process,
						Syscall: sc,
					}
					// TODO: need to update the proc's exe/cmdline after execve
					// maybe add a proc.Reset()?
					if execve, ok := sc.(*call.Execve); ok {
						if t.execFilter != nil && !t.execFilter(execve) {
							syscall.PtraceDetach(pid)
						}
					}
				}
				sig = syscall.Signal(0)
			}
			// TODO: send events upstream for signals

			// continue and pass signal
			if err = syscall.PtraceSyscall(pid, int(sig)); err != nil {
				break
			}
		}
		exitSig := interrupted
		if exitSig == 0 {
			exitSig = syscall.SIGTERM
		}
		if spawnChild >= 0 {
			syscall.Kill(pid, syscall.SIGCONT)
			syscall.Kill(pid, exitSig)
		}
		if interrupted != 0 {
			for pid, traced := range table {
				// if we're expecting a SIGSTOP, skip this and wait for it
				// otherwise the child will be detached into a an immediate SIGSTOP which is awkward for everyone
				if !traced.EatOneSigstop {
					if err := syscall.PtraceDetach(pid); err == nil || err != syscall.ESRCH {
						continue
					}
					if err := syscall.Kill(pid, 0); err != nil && err != syscall.ESRCH {
						continue
					}
					if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil && err != syscall.ESRCH {
						continue
					}
				}
				for {
					var status syscall.WaitStatus
					if _, err := syscall.Wait4(pid, &status, syscall.WALL, nil); err != nil {
						if err == syscall.EINTR {
							continue
						}
						break
					}
					if !status.Stopped() {
						break
					}
					// Linux wants a SIGSTOP before you detach from a process
					sig := status.StopSignal()
					if sig == syscall.SIGSTOP {
						syscall.PtraceDetach(pid)
						break
					}
					if sig&syscall.SIGTRAP != 0 {
						sig = syscall.Signal(0)
					}
					if err := syscall.PtraceCont(pid, int(sig)); err != nil && err != syscall.ESRCH {
						break
					}
				}
			}
		}
	}()
	err := <-errChan
	if err != nil {
		return nil, err
	}
	return ret, nil
}

type tracedProc struct {
	Process       process.Process
	Codec         *sys.Codec
	StopSig       syscall.Signal
	NewSyscall    bool
	SavedRegs     syscall.PtraceRegs
	EatOneSigstop bool
}

func newTracedProc(proc process.Process, eat bool) (*tracedProc, error) {
	pid := proc.Pid()
	options := syscall.PTRACE_O_TRACECLONE | syscall.PTRACE_O_TRACEFORK | syscall.PTRACE_O_TRACEVFORK | syscall.PTRACE_O_TRACESYSGOOD
	if err := syscall.PtraceSetOptions(pid, options); err != nil {
		return nil, err
	}
	var readMem = func(p []byte, addr uint64) (int, error) {
		return syscall.PtracePeekData(pid, uintptr(addr), p)
	}
	var writeMem = func(p []byte, addr uint64) (int, error) {
		return syscall.PtracePokeData(pid, uintptr(addr), p)
	}
	codec, err := sys.NewCodec(sys.ARCH_X86_64, sys.OS_LINUX, memio.NewMemIO(readMem, writeMem))
	if err != nil {
		return nil, err
	}
	return &tracedProc{
		Process:       proc,
		Codec:         codec,
		NewSyscall:    true,
		EatOneSigstop: eat,
	}, nil
}

func (t *tracedProc) Syscall() (ret sys.Syscall, err error) {
	pid := t.Process.Pid()
	if t.NewSyscall {
		t.NewSyscall = false
		if err = syscall.PtraceGetRegs(pid, &t.SavedRegs); err != nil {
			return
		}
	} else {
		t.NewSyscall = true
	}
	name := t.Codec.GetName(int(t.SavedRegs.Orig_rax))
	if t.NewSyscall != (name == "execve") {
		regs := &t.SavedRegs
		var newRegs syscall.PtraceRegs
		syscall.PtraceGetRegs(pid, &newRegs)
		args := []uint64{regs.Rdi, regs.Rsi, regs.Rdx, regs.R10, regs.R8, regs.R9}
		sc, err := t.Codec.DecodeRet(int(regs.Orig_rax), args, newRegs.Rax)
		if err != nil {
			fmt.Println(err)
		} else {
			ret = sc
		}
	}
	return
}
