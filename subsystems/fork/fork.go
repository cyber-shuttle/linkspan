package fork

import (
	"fmt"
	"github.com/cyber-shuttle/linkspan/internal/controller"
	"github.com/cyber-shuttle/linkspan/internal/process"
	"log"
	"os/exec"
	"sync"
)

type ForkProcess struct {
	Command              string
	InternalProcessId    string
	ShutdownOnCompletion bool
}

type ForkProcessManager struct {
	mu    sync.Mutex
	procs map[string]*ForkProcess
}

// NewProcessManager creates an empty ProcessManager.
func newForkProcessManager() *ForkProcessManager {
	return &ForkProcessManager{procs: make(map[string]*ForkProcess)}
}

// global fork manager instance for the package
var GlobalForkProcessManager = newForkProcessManager()

func (m *ForkProcessManager) RunForkProcess(command string, shutdownOnCompletion bool) (*ForkProcess, error) {
	cmd := exec.Command("sh", "-c", command)
	internalProcessId, err := process.GlobalProcessManager.Start(cmd, true)

	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	fp := &ForkProcess{
		Command:              command,
		InternalProcessId:    internalProcessId,
		ShutdownOnCompletion: shutdownOnCompletion,
	}
	m.procs[internalProcessId] = fp

	proc, err := process.GlobalProcessManager.GetInfo(internalProcessId)
	if err != nil {
		return nil, fmt.Errorf("failed to get process info: %v", err)
	}

	go func() {
		done := <-proc.Done
		if done != nil {
			log.Printf("Fork process %s completed with error: %v", internalProcessId, done)
		} else {
			log.Printf("Fork process %s completed successfully", internalProcessId)
		}

		if shutdownOnCompletion {
			log.Printf("Fork process %s requested shutdown on completion", internalProcessId)
			controller.TriggerShutdown(fmt.Sprintf("Fork process %s completed", internalProcessId))
		}
	}()

	return fp, nil
}

func (m *ForkProcessManager) GetForkProcess(internalProcessId string) (*ForkProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fp, ok := m.procs[internalProcessId]
	if !ok {
		return nil, fmt.Errorf("fork process %s not found", internalProcessId)
	}
	return fp, nil
}

func (m *ForkProcessManager) ListForkProcesses() []*ForkProcess {
	m.mu.Lock()
	defer m.mu.Unlock()

	forkProcesses := make([]*ForkProcess, 0, len(m.procs))
	for _, fp := range m.procs {
		forkProcesses = append(forkProcesses, fp)
	}
	return forkProcesses
}

func (m *ForkProcessManager) RemoveForkProcess(internalProcessId string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.procs[internalProcessId]; !ok {
		return fmt.Errorf("fork process %s not found", internalProcessId)
	}

	intProcess, err := process.GlobalProcessManager.GetInfo(internalProcessId)
	if err != nil {
		return fmt.Errorf("failed to get process info for %s: %v", internalProcessId, err)
	}

	if !intProcess.Completed {
		log.Printf("Process %s is still running, attempting to kill it...", internalProcessId)
		err = process.GlobalProcessManager.Kill(internalProcessId)
		if err != nil {
			return fmt.Errorf("failed to kill process %s: %v", internalProcessId, err)
		}
	}

	delete(m.procs, internalProcessId)
	return nil
}

func (m *ForkProcessManager) KillAllForkProcesses() error {
	for internalProcessId := range m.procs {
		err := m.RemoveForkProcess(internalProcessId)
		if err != nil {
			log.Printf("failed to remove fork process %s: %v", internalProcessId, err)
		}
	}
	return nil
}
