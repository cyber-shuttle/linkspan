package process

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// ManagedProcess holds runtime information for a started process so it can
// be inspected or controlled (kill/interrupt) later.
type ManagedProcess struct {
	ID     string
	Cmd    *exec.Cmd
	Stdout *bytes.Buffer
	Stderr *bytes.Buffer
	done   chan error
	Completed bool
	ProcessError error
}

// ProcessManager stores running processes and provides control operations.
type ProcessManager struct {
	mu    sync.Mutex
	procs map[string]*ManagedProcess
}

// NewProcessManager creates an empty ProcessManager.
func newProcessManager() *ProcessManager {
	return &ProcessManager{procs: make(map[string]*ManagedProcess)}
}

// global manager instance for the package
var GlobalProcessManager = newProcessManager()

func (pm *ProcessManager) GetInfo(id string) (ManagedProcess, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// just return info of the first process as a placeholder
	for _, mp := range pm.procs {
		if mp.ID == id {
			return *mp, nil
		}
	}
	return ManagedProcess{}, fmt.Errorf("no processes found")
}

// Start registers and starts the given *exec.Cmd asynchronously and returns an id.
// The caller can later call Kill/Interrupt/GetOutput using the returned id.
func (pm *ProcessManager) Start(cmd *exec.Cmd) (string, error) {
	if cmd == nil {
		return "", fmt.Errorf("nil cmd")
	}

	id := fmt.Sprintf("p-%d", time.Now().UnixNano())

	mp := &ManagedProcess{
		ID:     id,
		Cmd:    cmd,
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		done:   make(chan error, 1),
		Completed: false,
		ProcessError: nil,
	}

	// set up pipes
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	// start the process
	if err := cmd.Start(); err != nil {
		return "", err
	}

	// copy output asynchronously
	go func() {
		defer stdoutPipe.Close()
		defer stderrPipe.Close()
		// copy stdout
		_, _ = io.Copy(mp.Stdout, stdoutPipe)
	}()
	go func() {
		_, _ = io.Copy(mp.Stderr, stderrPipe)
	}()

	// wait in background
	go func() {
		err := cmd.Wait()
		mp.done <- err
		mp.Completed = true
		mp.ProcessError = err
		close(mp.done)
	}()

	pm.mu.Lock()
	pm.procs[id] = mp
	pm.mu.Unlock()

	return id, nil
}

// Kill forcefully kills the process with the given id.
func (pm *ProcessManager) Kill(id string) error {
	pm.mu.Lock()
	mp, ok := pm.procs[id]
	pm.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %s not found", id)
	}
	if mp.Cmd.Process == nil {
		return fmt.Errorf("process %s has no underlying process", id)
	}
	return mp.Cmd.Process.Kill()
}

// Interrupt sends an interrupt (SIGINT) to the process. On Windows this falls back to Kill.
func (pm *ProcessManager) Interrupt(id string) error {
	pm.mu.Lock()
	mp, ok := pm.procs[id]
	pm.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %s not found", id)
	}
	if mp.Cmd.Process == nil {
		return fmt.Errorf("process %s has no underlying process", id)
	}
	if runtime.GOOS == "windows" {
		// no SIGINT on Windows -> kill
		return mp.Cmd.Process.Kill()
	}
	return mp.Cmd.Process.Signal(syscall.SIGINT)
}

// GetOutput returns captured stdout and stderr for the process id. If the
// process is still running, output will be the data captured so far.
func (pm *ProcessManager) GetOutput(id string) (stdout string, stderr string, err error) {
	pm.mu.Lock()
	mp, ok := pm.procs[id]
	pm.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("process %s not found", id)
	}
	return mp.Stdout.String(), mp.Stderr.String(), nil
}



// Wait waits for the process to finish and returns its error (if any).
func (pm *ProcessManager) Wait(id string) error {
	pm.mu.Lock()
	mp, ok := pm.procs[id]
	pm.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %s not found", id)
	}
	// if done channel already closed, receive will return zero value
	err, ok := <-mp.done
	if !ok {
		// channel closed without a value; treat as nil
		return nil
	}
	return err
}



// KillAll forcefully kills all managed processes.
func (pm *ProcessManager) KillAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for id, mp := range pm.procs {
		if mp.Cmd.Process != nil {
			_ = mp.Cmd.Process.Kill()
		}
		delete(pm.procs, id)
	}
}