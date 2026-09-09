// Package procmgr runs and stops Linkspan's background work: child processes,
// servers and goroutines that outlive a request. Each is a Process with its
// own context. StopAll cancels every context and waits for every task to
// return. A task that ends on its own is removed; nothing restarts.
//
//	Kind, Task, Server, Process  A Task must return once its context is done, or
//	                             StopAll hangs.
//	stdioGrace                   It bounds how long Exec waits for a child's
//	                             pipes after the child has exited, since an
//	                             orphan that inherited them would otherwise
//	                             hold Wait open.
//	Start                        It runs the task on its own goroutine. A
//	                             cancelled task's error is dropped, and a panic
//	                             is an error. A repeated id cancels and replaces
//	                             the earlier process, and an entry is removed
//	                             only by the process that owns it. The context is
//	                             cancelled after the task returns, so an
//	                             AfterFunc always fires.
//	Failed                       It carries the first fatal error, and main exits
//	                             on it.
//	Running
//	StopAll                      It waits on a copy of the registry, so no wait
//	                             holds the lock.
//	Serve                        It serves a listener and closes both listener
//	                             and server on cancellation, because gliderlabs
//	                             drops a Close received while it holds no
//	                             listener.
//	Exec                         It runs a command in its own process group,
//	                             kills the group on cancellation so helpers die
//	                             with the child, and waits for the child. A
//	                             setsid daemon leaves the group and survives.
package procmgr

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os/exec"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

type Kind string

const (
	KindHTTP     Kind = "http"
	KindSSHD     Kind = "sshd"
	KindTunnel   Kind = "tunnel"
	KindWorkflow Kind = "workflow"
)

type Task func(ctx context.Context) error

type Server interface {
	Serve(net.Listener) error
	Close() error
}

type Process struct {
	ID   string
	Addr string
	Kind Kind

	cancel context.CancelFunc
	done   chan struct{}
}

var stdioGrace = 2 * time.Second

var registry = struct {
	mu sync.Mutex
	m  map[string]*Process
}{m: map[string]*Process{}}

var Failed = make(chan error, 1)

func Start(kind Kind, id, addr string, task Task) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Process{ID: id, Addr: addr, Kind: kind, cancel: cancel, done: make(chan struct{})}
	registry.mu.Lock()
	if old := registry.m[id]; old != nil {
		old.cancel()
	}
	registry.m[id] = p
	registry.mu.Unlock()
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			}
			if err != nil && ctx.Err() == nil {
				select {
				case Failed <- fmt.Errorf("%s: %w", id, err):
				default:
				}
			}
			cancel()
			registry.mu.Lock()
			if registry.m[id] == p {
				delete(registry.m, id)
			}
			registry.mu.Unlock()
			close(p.done)
		}()
		err = task(ctx)
	}()
}

func Running(kind Kind) []*Process {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	var out []*Process
	for _, p := range registry.m {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func StopAll() {
	registry.mu.Lock()
	live := maps.Clone(registry.m)
	registry.mu.Unlock()
	for _, p := range live {
		p.cancel()
		<-p.done
	}
}

func Serve(ln net.Listener, srv Server) Task {
	return func(ctx context.Context) error {
		context.AfterFunc(ctx, func() { _ = ln.Close(); _ = srv.Close() })
		return srv.Serve(ln)
	}
}

func Exec(ctx context.Context, cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = stdioGrace
	if err := cmd.Start(); err != nil {
		return err
	}
	// A negative pid addresses the process group
	stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer stop()
	return cmd.Wait()
}
