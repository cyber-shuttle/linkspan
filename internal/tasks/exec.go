// start is the one fork path: every child Linkspan starts goes through it, so cancellation reaches the child and
// its helpers alike, and stdio is wired at each call site. spawn and child are the Runs of a task given a Spawn
// or a Child: the step runs under the task's context and returns the command, a Spawn's bound to the port the
// task released. The task is starting until the port accepts, however long that takes, or until the child has a
// pid, then running; the Run ends as the child does, and Start records the exit, never fatal for a child.
//
//	stdioGrace    Bounds the wait for a child's pipes after it exits, which an orphan holding them would otherwise
//	              keep open.
//	pollInterval
//	start         Starts the command in its own process group and arranges for the group to be killed on
//	              cancellation, so helpers die with the child; the caller waits, then stops that. A setsid daemon
//	              leaves the group and survives.
//	Exec          Starts and waits.
//	Task          The receiver of spawn and child.
//	child         Publishes the pid under the registry lock, as the state.
//	spawn         Releases the port to the command and dials it until it accepts.
package tasks

import (
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
	registry.mu.Lock()
	t.Pid, t.State = cmd.Process.Pid, StateRunning
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
