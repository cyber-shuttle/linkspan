// Processes are listed from every cgroup.procs below the job cgroup, since the job cgroup itself holds none.
//
//	Process, readProcesses
package usage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Process struct {
	PID       int    `json:"pid"`
	Name      string `json:"name"`
	RSSBytes  *int64 `json:"rssBytes,omitempty"`
	GPUMemMiB *int64 `json:"gpuMemMiB,omitempty"`
}

func readProcesses(j job) []Process {
	gpuMem := j.gpuMem()
	procs := []Process{}
	_ = filepath.WalkDir(j.cgroup, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "cgroup.procs" {
			b, _ := os.ReadFile(path)
			for f := range strings.FieldsSeq(string(b)) {
				dir := procRoot + "/" + f
				if comm, err := os.ReadFile(dir + "/comm"); err == nil {
					pid, _ := strconv.Atoi(f)
					p := Process{PID: pid, Name: strings.TrimSpace(string(comm)), RSSBytes: scale(readInt(dir+"/status", "VmRSS:"), 1024)}
					if gpuMem != nil {
						mib := gpuMem[pid]
						p.GPUMemMiB = &mib
					}
					procs = append(procs, p)
				}
			}
		}
		return nil
	})
	return procs
}
