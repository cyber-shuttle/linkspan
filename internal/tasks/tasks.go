// Package tasks is the registry of Linkspan's background work: every goroutine, server and child process that
// outlives a request. A Task is a Run under its own context, with an id, a kind, an address, a state and an error.
// Its work is one of three things: a Run, Linkspan's own code, whose error is fatal; a Server, served on the bound
// listener; or a Spawn, a child process on the bound port, whose life Linkspan only observes, so its end is
// recorded and never fatal. Start is the one entry: it binds the address, if any, registers the task and runs it
// on its own goroutine. A task stays listed after its work ends, as failed or exited, until Stop removes it;
// StopAll cancels every task, waits for each and empties the registry. Nothing restarts. exec.go holds the fork.
//
//	Kind         Declared by whoever starts the task.
//	State*       Starting until the port accepts, then running; failed carries the error; exited is a nil return.
//	Server       What an http.Server and a gliderlabs Server share.
//	Task         Exactly one of Run, Server and Spawn is set. Run must return once its context is done, or StopAll
//	             hangs. Server and Spawn default Addr to loopback on any port. Attrs are the caller's wire fields,
//	             computed from the bound task.
//	registry     One lock, which also guards each task's state.
//	Failed       The first fatal error; main exits on it.
//	listen       An Addr that is not host:port is a unix path: a stale socket there is unlinked, any other file
//	             fails the bind, and the socket is created with mode 0600.
//	setState     The one writer of State and Error.
//	Port         0 for a unix socket.
//	MarshalJSON  The wire object: id, addr, state and error, then the attrs.
//	Start        Binds Addr when set; a failed bind is the error and registers nothing. The id defaults to the
//	             kind's initial with the port or socket path, else to the kind; the state to running unless a Spawn
//	             set it to starting; a repeated id cancels and replaces the earlier task. Run goes on its own
//	             goroutine; the listener closes on cancellation, and a Server after it. A returned error sets
//	             failed and is fatal unless the task is
//	             a Spawn; a panic is an error; a cancelled Run's error is dropped; a nil return is exited. The
//	             context is cancelled after Run returns, so every AfterFunc fires. The returned copy is the
//	             caller's; the registry entry changes under the lock.
//	Select       Copies of one kind, ordered by id.
//	Stop         Cancels one task, waits for it and forgets it; an unknown id is false.
//	StopAll      Waits on a copy of the registry, so no wait holds the lock.
package tasks

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type Kind string

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
	StateExited   State = "exited"
)

type Server interface {
	Serve(net.Listener) error
	Close() error
}

type Task struct {
	ID     string
	Kind   Kind
	Addr   string
	State  State
	Error  string
	Attrs  func(Task) map[string]string
	Run    func(ctx context.Context) error
	Server Server
	Spawn  func(ctx context.Context, port int) (*exec.Cmd, error)
	ln     net.Listener
	cancel context.CancelFunc
	done   chan struct{}
}

var registry = struct {
	mu sync.Mutex
	m  map[string]*Task
}{m: map[string]*Task{}}

var Failed = make(chan error, 1)

func listen(addr string) (net.Listener, error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return net.Listen("tcp", addr)
	}
	if fi, err := os.Lstat(addr); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(addr)
	}
	ln, err := net.Listen("unix", addr)
	if err == nil {
		if err = os.Chmod(addr, 0o600); err != nil {
			_ = ln.Close()
		}
	}
	return ln, err
}

func (t *Task) setState(state State, err error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	t.State = state
	if err != nil {
		t.Error = err.Error()
	}
}

func (t Task) Port() int {
	_, port, _ := net.SplitHostPort(t.Addr)
	n, _ := strconv.Atoi(port)
	return n
}

func (t Task) MarshalJSON() ([]byte, error) {
	out := map[string]any{"id": t.ID, "addr": t.Addr, "state": t.State, "error": t.Error}
	if t.Attrs != nil {
		for k, v := range t.Attrs(t) {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

func (t *Task) Start() (Task, error) {
	switch {
	case t.Server != nil:
		t.Addr, t.Run = cmp.Or(t.Addr, "127.0.0.1:0"), func(ctx context.Context) error {
			context.AfterFunc(ctx, func() { _ = t.Server.Close() })
			return t.Server.Serve(t.ln)
		}
	case t.Spawn != nil:
		t.Addr, t.State, t.Run = cmp.Or(t.Addr, "127.0.0.1:0"), StateStarting, t.spawn
	}
	if t.Addr != "" {
		ln, err := listen(t.Addr)
		if err != nil {
			return Task{}, fmt.Errorf("tasks: listen: %w", err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		t.ln, t.Addr = ln, ln.Addr().String()
		t.ID = cmp.Or(t.ID, fmt.Sprintf("%c-%s", t.Kind[0], cmp.Or(port, t.Addr)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.ID, t.State, t.cancel, t.done = cmp.Or(t.ID, string(t.Kind)), cmp.Or(t.State, StateRunning), cancel, make(chan struct{})
	registry.mu.Lock()
	if old := registry.m[t.ID]; old != nil {
		old.cancel()
	}
	registry.m[t.ID] = t
	registry.mu.Unlock()
	created := *t
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			}
			if err != nil {
				t.setState(StateFailed, err)
			} else {
				t.setState(StateExited, nil)
			}
			if err != nil && ctx.Err() == nil && t.Spawn == nil {
				select {
				case Failed <- fmt.Errorf("%s: %w", t.ID, err):
				default:
				}
			}
			cancel()
			close(t.done)
		}()
		if t.ln != nil {
			context.AfterFunc(ctx, func() { _ = t.ln.Close() })
		}
		err = t.Run(ctx)
	}()
	return created, nil
}

func Select(kind Kind) []Task {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := []Task{}
	for _, t := range slices.SortedFunc(maps.Values(registry.m), func(a, b *Task) int { return strings.Compare(a.ID, b.ID) }) {
		if t.Kind == kind {
			out = append(out, *t)
		}
	}
	return out
}

func Stop(id string) bool {
	registry.mu.Lock()
	t := registry.m[id]
	delete(registry.m, id)
	registry.mu.Unlock()
	if t == nil {
		return false
	}
	t.cancel()
	<-t.done
	return true
}

func StopAll() {
	registry.mu.Lock()
	live := maps.Clone(registry.m)
	clear(registry.m)
	registry.mu.Unlock()
	for _, t := range live {
		t.cancel()
		<-t.done
	}
}
