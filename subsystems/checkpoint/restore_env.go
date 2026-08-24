package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
)

/*
prepareRestoreEnvironment reconstructs what the application expects to find
on this allocation before CRIU runs. CRIU restores the process, not the
world around it: a restore onto a node missing the workspace mount succeeds
and then faults the application.

Linkspan's own surface (tunnels, VS Code servers, HTTP state) is not
reconstructed here; the new allocation recreates it fresh.
*/
func prepareRestoreEnvironment(ctx context.Context, m *Manifest, opts RestoreOptions) *RestoreValidation {
	v := &RestoreValidation{}

	// Commands first: they bring up what everything below verifies.
	for _, cmd := range opts.PreRestoreCommands {
		if err := runPreRestoreCommand(ctx, cmd, m); err != nil {
			v.errorf("pre-restore command %q failed: %v", cmd, err)
			return v
		}
	}

	verifyMounts(m, v)
	ensureDirs(m, opts, v)
	verifyRequiredFiles(opts, v)
	verifyModules(m, v)

	return v
}

func runPreRestoreCommand(ctx context.Context, command string, m *Manifest) error {
	log.Printf("[Checkpoint] running pre-restore command: %s", command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = preRestoreCommandEnv(m)
	if m != nil && m.WorkingDir != "" {
		if info, err := os.Stat(m.WorkingDir); err == nil && info.IsDir() {
			cmd.Dir = m.WorkingDir
		}
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		log.Printf("[Checkpoint] pre-restore command output: %s", s)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// preRestoreCommandEnv layers the checkpointed environment over linkspan's,
// so a recorded `module load` resolves as it originally did.
func preRestoreCommandEnv(m *Manifest) []string {
	env := os.Environ()
	if m == nil {
		return env
	}
	keys := make([]string, 0, len(m.Environment))
	for k := range m.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+m.Environment[k])
	}
	return env
}

// verifyMounts checks the filesystems the process depended on are mounted
// here. Fatal if not: a directory in their place would let the restore
// succeed against empty local storage.
func verifyMounts(m *Manifest, v *RestoreValidation) {
	if m == nil || len(m.Mounts) == 0 {
		return
	}
	present := make(map[string]bool)
	for _, mp := range readMountInfo("self") {
		present[mp.Target] = true
	}
	for _, want := range m.Mounts {
		if want.Target == "/" || present[want.Target] {
			continue
		}
		v.errorf("required mount %s (%s, from %s) is not mounted on this host; mount it with --restore-pre-command before restoring", want.Target, want.FSType, want.Source)
	}
}

// ensureDirs creates the workspace directories the application expects. The
// working directory is created rather than required, since a fresh
// allocation's scratch tree is legitimately empty.
func ensureDirs(m *Manifest, opts RestoreOptions, v *RestoreValidation) {
	dirs := append([]string{}, opts.EnsureDirs...)
	if m != nil && m.WorkingDir != "" {
		dirs = append(dirs, m.WorkingDir)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil {
			if !info.IsDir() {
				v.errorf("%s exists but is not a directory", dir)
			}
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			v.errorf("failed to create required directory %s: %v", dir, err)
			continue
		}
		log.Printf("[Checkpoint] created missing directory %s for restore", dir)
	}
}

// verifyRequiredFiles checks prerequisites only the new allocation can
// supply: credentials, keytabs, config.
func verifyRequiredFiles(opts RestoreOptions, v *RestoreValidation) {
	for _, f := range opts.RequireFiles {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			v.errorf("required file %s is not present on this host: %v", f, err)
		}
	}
}

// verifyModules warns about modules loaded at checkpoint time but not here.
// Only a warning: it matters for what the restored process execs later, not
// for the process itself, whose environment CRIU restores.
func verifyModules(m *Manifest, v *RestoreValidation) {
	if m == nil || len(m.Modules) == 0 {
		return
	}
	loaded := make(map[string]bool)
	for _, mod := range loadedModules(map[string]string{"LOADEDMODULES": os.Getenv("LOADEDMODULES")}) {
		loaded[mod] = true
	}
	var missing []string
	for _, mod := range m.Modules {
		if !loaded[mod] {
			missing = append(missing, mod)
		}
	}
	if len(missing) > 0 {
		v.warnf("environment modules loaded at checkpoint time are not loaded here: %s", strings.Join(missing, ", "))
	}
}
