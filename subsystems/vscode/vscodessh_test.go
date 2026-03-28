package vscode

import (
	"log"
	"testing"
	"time"
)

func TestVSCodeSSHServer(t *testing.T) {
	// This is a placeholder for actual tests.
	sshServer := StartSSHServerForVSCodeConnection("test-session", ":2222", "testpassword")
	if sshServer == nil {
		t.Fatalf("failed to start SSH server for VSCode connection")
	}

	// wait 3 seconds to simulate server running
	time.Sleep(3 * time.Second)

	status, err := getSessionStatus("test-session")
	if err != nil {
		t.Fatalf("failed to get session status: %v", err)
	}
	if !status.Active {
		t.Fatalf("expected session to be active")
	}

	log.Printf("SSH server for VSCode status: %+v", status)

	err = stopSSHServerBySessionID("test-session")
	if err != nil {
		t.Fatalf("failed to stop SSH server for VSCode connection: %v", err)
	}
}
