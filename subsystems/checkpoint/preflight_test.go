package checkpoint

import (
	"os"
	"os/exec"
	"os/user"
	"testing"
)

func TestCheckDirWritable(t *testing.T) {
	if err := checkDirWritable(t.TempDir()); err != nil {
		t.Fatalf("expected a fresh temp dir to be writable: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not restrict writes")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("failed to chmod test dir read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	if err := checkDirWritable(dir); err == nil {
		t.Fatalf("expected a read-only dir to fail the writable check")
	}
}

func TestCheckPidExists(t *testing.T) {
	if err := checkPidExists(os.Getpid()); err != nil {
		t.Fatalf("expected our own pid to exist: %v", err)
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	gonePid := cmd.Process.Pid

	if err := checkPidExists(gonePid); err == nil {
		t.Fatalf("expected an exited pid to be reported as not existing")
	}

	if err := checkPidExists(0); err == nil {
		t.Fatalf("expected pid 0 to be rejected as invalid")
	}
}

func TestCheckAllowedUser(t *testing.T) {
	self := os.Getpid()

	if err := checkAllowedUser(self, nil); err != nil {
		t.Fatalf("default (empty) allow list should permit checkpointing our own process: %v", err)
	}

	if err := checkAllowedUser(self, []string{"*"}); err != nil {
		t.Fatalf("wildcard allow list should permit any user: %v", err)
	}

	u, err := user.Current()
	if err != nil {
		t.Skipf("could not resolve current user: %v", err)
	}
	if err := checkAllowedUser(self, []string{u.Username}); err != nil {
		t.Fatalf("allow list containing our own username should permit our own process: %v", err)
	}
	if err := checkAllowedUser(self, []string{u.Uid}); err != nil {
		t.Fatalf("allow list containing our own uid should permit our own process: %v", err)
	}

	if err := checkAllowedUser(self, []string{"definitely-not-a-real-user", "999999"}); err == nil {
		t.Fatalf("allow list not containing our user should reject our own process")
	}
}

func TestProcessOwnerGID(t *testing.T) {
	if _, err := processOwnerGID(os.Getpid()); err != nil {
		t.Fatalf("expected to determine our own gid: %v", err)
	}
}
