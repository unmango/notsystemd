package supervisor

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// reaper collects the exit status of every child, including orphans
// reparented to notsystemd when it is PID 1 or a subreaper, and hands each
// status to whoever watches that pid.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
	// exited holds statuses nobody watched yet, because a process can exit
	// before its pid is known: a forking daemon's pid comes from its PID
	// file, and MAINPID= arrives over the notify socket.
	exited map[int]syscall.WaitStatus
	idle   []chan struct{}

	sigs chan os.Signal
	done chan struct{}
}

func newReaper() *reaper {
	r := &reaper{
		waiters: map[int]chan syscall.WaitStatus{},
		exited:  map[int]syscall.WaitStatus{},
		sigs:    make(chan os.Signal, 1),
		done:    make(chan struct{}),
	}
	signal.Notify(r.sigs, syscall.SIGCHLD)
	go r.loop()
	return r
}

func (r *reaper) loop() {
	// SIGCHLD can coalesce, and a child can exit between a wait4 and the
	// signal handler being installed, so poll as well.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-r.sigs:
		case <-tick.C:
		}
		r.reap()
	}
}

func (r *reaper) reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.ECHILD {
			r.mu.Lock()
			for _, ch := range r.idle {
				close(ch)
			}
			r.idle = nil
			r.mu.Unlock()
			return
		}
		if err != nil || pid <= 0 {
			return
		}

		r.mu.Lock()
		if ch, ok := r.waiters[pid]; ok {
			ch <- ws
			delete(r.waiters, pid)
		} else {
			if len(r.exited) > 4096 {
				clear(r.exited)
			}
			r.exited[pid] = ws
		}
		r.mu.Unlock()
	}
}

// watch returns a channel that receives pid's exit status. pid must be a
// child of this process, or reparented to it.
func (r *reaper) watch(pid int) <-chan syscall.WaitStatus {
	ch := make(chan syscall.WaitStatus, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ws, ok := r.exited[pid]; ok {
		ch <- ws
		delete(r.exited, pid)
		return ch
	}
	r.waiters[pid] = ch
	return ch
}

// watchAny is watch for a pid that may not be our child, such as one a
// service reported with MAINPID=. Its exit status cannot be collected then,
// so it polls for the pid disappearing and reports a clean exit.
func (r *reaper) watchAny(pid int) <-chan syscall.WaitStatus {
	ch := make(chan syscall.WaitStatus, 1)
	reaped := r.watch(pid)
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case ws := <-reaped:
				ch <- ws
				return
			case <-r.done:
				return
			case <-tick.C:
				// A child that exited stays visible as a zombie until it is
				// reaped, so ESRCH means it was never ours.
				if syscall.Kill(pid, 0) == syscall.ESRCH {
					r.mu.Lock()
					delete(r.waiters, pid)
					r.mu.Unlock()
					select {
					case ws := <-reaped:
						ch <- ws
					default:
						ch <- 0
					}
					return
				}
			}
		}
	}()
	return ch
}

// whenIdle returns a channel closed once this process has no children left.
func (r *reaper) whenIdle() <-chan struct{} {
	ch := make(chan struct{})
	r.mu.Lock()
	r.idle = append(r.idle, ch)
	r.mu.Unlock()
	select {
	case r.sigs <- syscall.SIGCHLD:
	default:
	}
	return ch
}

func (r *reaper) stop() {
	signal.Stop(r.sigs)
	close(r.done)
}
