package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNetworkPolicy(t *testing.T) {
	for _, ok := range []string{"", "reconstruct", "migrate"} {
		if err := ValidateNetworkPolicy(ok); err != nil {
			t.Fatalf("expected policy %q to be valid: %v", ok, err)
		}
	}
	if err := ValidateNetworkPolicy("carry-over"); err == nil {
		t.Fatalf("expected an unknown network policy to be rejected")
	}
}

/*
The whole point of stage 9's network change: --tcp-established is no longer
hardcoded onto every application. Reconstruction is the default because most
linkspan sessions rebuild their networking in the new allocation.
*/
func TestReconstructIsTheDefaultAndOmitsTcpEstablished(t *testing.T) {
	if DefaultNetworkPolicy != NetworkReconstruct {
		t.Fatalf("expected reconstruction to be the default policy, got %q", DefaultNetworkPolicy)
	}

	dump := strings.Join(buildDumpArgs(dumpOptions{PID: 1, ImagesDir: "/i", WorkDir: "/w", LogFile: "d.log", Network: NetworkReconstruct}), " ")
	if strings.Contains(dump, "--tcp-established") {
		t.Fatalf("a reconstruct-policy dump must not pass --tcp-established, got %s", dump)
	}
	restore := strings.Join(buildRestoreArgs(restoreOptions{ImagesDir: "/i", WorkDir: "/w", LogFile: "r.log", PidFile: "r.pid", Network: NetworkReconstruct}), " ")
	if strings.Contains(restore, "--tcp-established") {
		t.Fatalf("a reconstruct-policy restore must not pass --tcp-established, got %s", restore)
	}

	// An unset policy on the checkpointer resolves to the default, not to "".
	c := &criuCheckpointer{}
	if c.networkPolicy() != NetworkReconstruct {
		t.Fatalf("expected an unset policy to resolve to reconstruct, got %q", c.networkPolicy())
	}
}

func TestMigratePolicyPassesTcpEstablished(t *testing.T) {
	dump := strings.Join(buildDumpArgs(dumpOptions{PID: 1, ImagesDir: "/i", WorkDir: "/w", LogFile: "d.log", Network: NetworkMigrate}), " ")
	if !strings.Contains(dump, "--tcp-established") {
		t.Fatalf("a migrate-policy dump must pass --tcp-established, got %s", dump)
	}
	restore := strings.Join(buildRestoreArgs(restoreOptions{ImagesDir: "/i", WorkDir: "/w", LogFile: "r.log", PidFile: "r.pid", Network: NetworkMigrate}), " ")
	if !strings.Contains(restore, "--tcp-established") {
		t.Fatalf("a migrate-policy restore must pass --tcp-established, got %s", restore)
	}
}

/*
A restore has to use the policy the dump was taken with, not this allocation's
flags: the allocation doing the restore is a different one, often started by a
different person, and a mismatch silently changes what CRIU expects to find.
*/
func TestRestoreTakesTheNetworkPolicyFromTheCheckpoint(t *testing.T) {
	root := t.TempDir()
	dir := checkpointDirPath(root, "wl-net", "ckpt-net")
	if err := os.MkdirAll(imagesDirPath(dir), 0755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	// The checkpoint was taken with migrate; this allocation defaults to
	// reconstruct, and must still restore as migrate.
	m := &Manifest{Schema: ManifestSchema, CheckpointID: "ckpt-net", WorkloadID: "wl-net", Network: NetworkMigrate, State: StateComplete}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	c := &criuCheckpointer{CheckpointRoot: root, Network: NetworkReconstruct}
	if got := c.restoreNetworkPolicy(dir); got != NetworkMigrate {
		t.Fatalf("expected the checkpoint's own policy %q, got %q", NetworkMigrate, got)
	}

	// A checkpoint predating the policy field falls back to this allocation's.
	old := checkpointDirPath(root, "wl-old", "ckpt-old")
	if err := os.MkdirAll(imagesDirPath(old), 0755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}
	if err := writeManifest(old, &Manifest{Schema: ManifestSchema, CheckpointID: "ckpt-old", WorkloadID: "wl-old", State: StateComplete}); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}
	if got := c.restoreNetworkPolicy(old); got != NetworkReconstruct {
		t.Fatalf("expected a policy-less checkpoint to fall back to this allocation's, got %q", got)
	}

	// An unreadable checkpoint must not panic or return an empty policy.
	if got := c.restoreNetworkPolicy(filepath.Join(root, "does-not-exist")); got != NetworkReconstruct {
		t.Fatalf("expected a fallback policy for an unreadable checkpoint, got %q", got)
	}
}

// The options that are genuinely universal stay hardcoded; only the network
// policy became configurable.
func TestUniversalCriuOptionsRemain(t *testing.T) {
	dump := strings.Join(buildDumpArgs(dumpOptions{PID: 1, ImagesDir: "/i", WorkDir: "/w", LogFile: "d.log"}), " ")
	for _, want := range []string{"--shell-job", "--unprivileged"} {
		if !strings.Contains(dump, want) {
			t.Fatalf("expected %s to remain in the dump args, got %s", want, dump)
		}
	}
}
