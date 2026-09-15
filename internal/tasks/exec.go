// Exec is the one fork path: every child Linkspan starts goes through it, so cancellation reaches the child and
// its helpers alike, and stdio is wired at each call site. spawn is the Run of a task given a Spawn: the step runs
// under the task's context and returns the command, bound to the port the task released. The task is starting
// until the port accepts, however long that takes, then running; the Run ends as the child does, and Start
// records the exit, never fatal for a child.
//
//	stdioGrace    Bounds the wait for a child's pipes after it exits, which an orphan holding them would otherwise
//	              keep open.
//	pollInterval
//	start         What Exec and child share: the start, and the kill on cancellation.
//	Exec          Runs the command in its own process group, kills the group on cancellation so helpers die with
//	              the child, and waits for the child. A setsid daemon leaves the group and survives.
//	Task          The receiver of spawn and child.
//	child         Publishes the pid under the registry lock, as the state.
//	spawn         Releases the port to the command and dials it until it accepts.
package tasks

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os/exec"
	"syscall"
	"time"
)

var stdioGrace = 2 * time.Second

var pollInterval = 500 * time.Millisecond

func start(ctx context.Context, cmd *exec.Cmd) (func() bool, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = stdioGrace
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// A negative pid addresses the process group
	return context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }), nil
}

func Exec(ctx context.Context, cmd *exec.Cmd) error {
	stop, err := start(ctx, cmd)
	if err != nil {
		return err
	}
	defer stop()
	return cmd.Wait()
}

func (t *Task) child(ctx context.Context) error {
	cmd, err := t.Child(ctx)
	if err != nil {
		return err
	}
	stop, err := start(ctx, cmd)
	if err != nil {
		return err
	}
	defer stop()
	if pid := t.Pid; pid != 0 {
		defer context.AfterFunc(ctx, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })()
	}
	registry.mu.Lock()
	t.Pid, t.State = cmp.Or(t.Pid, cmd.Process.Pid), StateRunning
	registry.mu.Unlock()
	return cmd.Wait()
}

func (t *Task) spawn(ctx context.Context) error {
	_ = t.ln.Close()
	cmd, err := t.Spawn(ctx, t.Port())
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- Exec(ctx, cmd) }()
	for {
		if c, err := net.Dial("tcp", t.Addr); err == nil {
			_ = c.Close()
			break
		}
		select {
		case err := <-exited:
			return fmt.Errorf("exited before answering: %w", err)
		case <-time.After(pollInterval):
		}
	}
	t.setState(StateRunning, nil)
	return <-exited
}
