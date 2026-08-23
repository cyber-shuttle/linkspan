package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// capturedEnvPrefixes selects the environment a restoring allocation needs:
// toolchains, module state, library and interpreter paths. Nothing else is
// recorded — manifests live on shared storage and environments carry secrets.
var capturedEnvPrefixes = []string{
	"PATH", "LD_LIBRARY_PATH", "LD_PRELOAD",
	"MODULEPATH", "MODULESHOME", "LOADEDMODULES", "_LMFILES_", "LMOD_",
	"CONDA_PREFIX", "CONDA_DEFAULT_ENV", "VIRTUAL_ENV", "PYTHONPATH", "PYTHONHOME",
	"CUDA_", "NCCL_", "OMP_", "MPI_", "OMPI_", "HOME", "TMPDIR",
}

// virtualFilesystems are always present, so recording them as restore
// prerequisites would be noise.
var virtualFilesystems = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "cgroup": true, "cgroup2": true, "mqueue": true,
	"hugetlbfs": true, "debugfs": true, "tracefs": true, "securityfs": true,
	"pstore": true, "bpf": true, "configfs": true, "fusectl": true,
	"binfmt_misc": true, "autofs": true, "ramfs": true,
}

// openFiles returns the regular files pid has open. Pipes, sockets and
// anonymous inodes are skipped; only paths a restore host must provide
// matter here.
func openFiles(pid int) []string {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	for _, e := range entries {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, e.Name()))
		if err != nil || !filepath.IsAbs(target) || strings.HasSuffix(target, " (deleted)") {
			continue
		}
		if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[target] = true
	}

	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func capturedEnvironment(pid int) map[string]string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	return filterEnvironment(strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"))
}

// filterEnvironment keeps the KEY=VALUE entries matching capturedEnvPrefixes.
func filterEnvironment(entries []string) map[string]string {
	env := make(map[string]string)
	for _, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		for _, prefix := range capturedEnvPrefixes {
			if key == prefix || strings.HasPrefix(key, prefix) {
				env[key] = value
				break
			}
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// loadedModules reads the modules the process had loaded, as reported by
// Lmod/Environment Modules.
func loadedModules(env map[string]string) []string {
	loaded := env["LOADEDMODULES"]
	if loaded == "" {
		return nil
	}
	var modules []string
	for _, m := range strings.Split(loaded, ":") {
		if m = strings.TrimSpace(m); m != "" {
			modules = append(modules, m)
		}
	}
	return modules
}

// readMountInfo parses /proc/<pid>/mountinfo into real (non-virtual) mounts.
// "self" reads the current mount namespace.
func readMountInfo(pid string) []MountPoint {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%s/mountinfo", pid))
	if err != nil {
		return nil
	}

	var mounts []MountPoint
	for _, line := range strings.Split(string(data), "\n") {
		// Format: id parent major:minor root target opts [tags...] - fstype source superopts
		pre, post, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		preFields, postFields := strings.Fields(pre), strings.Fields(post)
		if len(preFields) < 5 || len(postFields) < 2 {
			continue
		}
		if virtualFilesystems[postFields[0]] {
			continue
		}
		mounts = append(mounts, MountPoint{
			Source: unescapeMountField(postFields[1]),
			Target: unescapeMountField(preFields[4]),
			FSType: postFields[0],
		})
	}
	return mounts
}

// unescapeMountField decodes the octal escapes the kernel writes into
// mountinfo.
func unescapeMountField(field string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(field)
}

// dependencyMounts narrows pid's mounts to those backing paths, so the
// manifest records the storage a restore host must provide rather than every
// mount that existed on the checkpointing node.
func dependencyMounts(pid int, paths []string) []MountPoint {
	mounts := readMountInfo(fmt.Sprintf("%d", pid))
	if len(mounts) == 0 {
		return nil
	}

	needed := make(map[string]MountPoint)
	for _, path := range paths {
		if path == "" {
			continue
		}
		if m, ok := longestMountFor(mounts, path); ok {
			needed[m.Target] = m
		}
	}

	targets := make([]string, 0, len(needed))
	for t := range needed {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	result := make([]MountPoint, 0, len(targets))
	for _, t := range targets {
		result = append(result, needed[t])
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// longestMountFor returns the most specific mount containing path.
func longestMountFor(mounts []MountPoint, path string) (MountPoint, bool) {
	var best MountPoint
	found := false
	for _, m := range mounts {
		if !pathUnder(path, m.Target) {
			continue
		}
		if !found || len(m.Target) > len(best.Target) {
			best, found = m, true
		}
	}
	return best, found
}

func pathUnder(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}
