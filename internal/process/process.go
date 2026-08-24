// Package process tracks background CLI processes -- today only the devtunnel
// host relay -- so they can be killed and read while they still run.
package process

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

var Global = &Manager{procs: map[string]*proc{}}

type Manager struct {
	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	cmd            *exec.Cmd
	stdout, stderr *buffer
	done           chan struct{}
}

func (m *Manager) Start(cmd *exec.Cmd) (string, error) {
	if cmd == nil {
		return "", fmt.Errorf("nil cmd")
	}
	p := &proc{cmd: cmd, stdout: &buffer{}, stderr: &buffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { defer close(p.done); _ = cmd.Wait() }()

	id := fmt.Sprintf("p-%d", time.Now().UnixNano())
	m.mu.Lock()
	defer m.mu.Unlock()
	m.procs[id] = p
	return id, nil
}

func (m *Manager) Kill(id string) error {
	p, err := m.lookup(id)
	if err != nil {
		return err
	}
	return p.kill()
}

func (m *Manager) Output(id string) (stdout, stderr string, err error) {
	p, err := m.lookup(id)
	if err != nil {
		return "", "", err
	}
	return p.stdout.String(), p.stderr.String(), nil
}

// Waits briefly for each process to be reaped, so none lingers as a zombie.
func (m *Manager) KillAll() {
	m.mu.Lock()
	procs := m.procs
	m.procs = map[string]*proc{}
	m.mu.Unlock()

	for _, p := range procs {
		if p.kill() != nil {
			continue
		}
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	}
}

// Exited reports whether the process has finished. An unknown id counts as
// exited: nothing is running under it.
func (m *Manager) Exited(id string) bool {
	p, err := m.lookup(id)
	if err != nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (m *Manager) lookup(id string) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("process %s not found", id)
}

func (p *proc) kill() error {
	if p.cmd.Process == nil {
		return fmt.Errorf("process never started")
	}
	return p.cmd.Process.Kill()
}

// Output reads this while exec is still copying into it, so a plain
// bytes.Buffer would race.
type buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
