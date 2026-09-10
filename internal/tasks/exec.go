// Exec is the one fork path: every child Linkspan starts goes through it, so cancellation reaches the child and
// its helpers alike, and stdio is wired at each call site. spawn is the Run of a task given a Spawn: the step runs
// under the task's context and returns the command, bound to the port the task released. The task is starting
// until the port accepts, however long that takes, then running; the Run ends as the child does, and Start
// records the exit, never fatal for a child.
//
//	stdioGrace    Bounds the wait for a child's pipes after it exits, which an orphan holding them would otherwise
//	              keep open.
//	pollInterval
//	Exec          Runs the command in its own process group, kills the group on cancellation so helpers die with
//	              the child, and waits for the child. A setsid daemon leaves the group and survives.
//	Task          The receiver of spawn.
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
