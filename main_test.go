// Tests for the surface docs/COMPATIBILITY.md freezes.
//
//	binary                       Built once per run.
//	TestFlagSurface, TestRoutesFollowConfig, TestVersionIsOneLine, TestArchiveName
//	TestExampleWorkflowLoads     examples/workflow.yml must name only commands the subsystems export.
//	TestBindsLoopbackAndUnwinds  Sends SIGTERM once the socket answers, so every listener is up before the unwind.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/cyber-shuttle/linkspan/subsystems/workflow"
	"gopkg.in/yaml.v3"
)

var binary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "linkspan")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "linkspan")
	out, err := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9", "-o", path, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build failed: %w\n%s", err, out)
	}
	return path, nil
})

func TestFlagSurface(t *testing.T) {
	fs := flag.NewFlagSet("linkspan", flag.ContinueOnError)
	registerFlags(fs)

	want := []string{
		"port",
		"socket",
		"tunnel-cluster",
		"tunnel-enable",
		"tunnel-host-token",
		"tunnel-id",
		"version",
		"workflow",
	}
	var got []string
	fs.VisitAll(func(f *flag.Flag) { got = append(got, f.Name) })
	if !slices.Equal(got, want) {
		t.Errorf("the flag surface changed:\n got %q\nwant %q", got, want)
	}
}

func TestRoutesFollowConfig(t *testing.T) {
	get := func(cfg Config, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		routes(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get(Config{}, "/api/v1/health"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health answered %d %s, want the documented literal", rec.Code, rec.Body)
	}
	var snap map[string]any
	if rec := get(Config{}, "/api/v1/metrics"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snap) != nil {
		t.Fatalf("metrics answered %d %s, want an object", rec.Code, rec.Body)
	}
	all := Config{"vscode": true, "jupyter": true, "terminal": true, "filesystem": true}
	for _, path := range []string{"/api/v1/vscode/sessions", "/api/v1/jupyter/sessions", "/api/v1/terminal/sessions"} {
		if rec := get(all, path); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d with its subsystem enabled, want 200", path, rec.Code)
		}
		if rec := get(Config{}, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d with its subsystem disabled, want 404", path, rec.Code)
		}
	}
}

func TestVersionIsOneLine(t *testing.T) {
	bin, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "9.9.9\n" {
		t.Errorf("clients match --version output whole and anchored; got %q", out)
	}
}

func TestArchiveName(t *testing.T) {
	var cfg struct {
		ProjectName string `yaml:"project_name"`
		Builds      []struct {
			Binary string `yaml:"binary"`
		} `yaml:"builds"`
		Archives []struct {
			Formats      []string `yaml:"formats"`
			NameTemplate string   `yaml:"name_template"`
		} `yaml:"archives"`
	}
	b, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectName != "linkspan" || cfg.Builds[0].Binary != "linkspan" {
		t.Fatalf("project %q builds %q; clients untar and exec a member named linkspan",
			cfg.ProjectName, cfg.Builds[0].Binary)
	}
	if !slices.Contains(cfg.Archives[0].Formats, "tar.gz") {
		t.Fatalf("archive formats %q; clients curl a .tar.gz", cfg.Archives[0].Formats)
	}
	tmpl, err := template.New("archive").Funcs(template.FuncMap{
		"title": func(s string) string { return strings.ToUpper(s[:1]) + s[1:] },
	}).Parse(cfg.Archives[0].NameTemplate)
	if err != nil {
		t.Fatal(err)
	}
	for arch, want := range map[string]string{"amd64": "linkspan_Linux_x86_64", "arm64": "linkspan_Linux_arm64"} {
		var got strings.Builder
		if err := tmpl.Execute(&got, struct{ ProjectName, Os, Arch, Arm string }{"linkspan", "linux", arch, ""}); err != nil {
			t.Fatal(err)
		}
		if got.String() != want {
			t.Errorf("%s archive is %q, want %q", arch, got.String(), want)
		}
	}
}

func TestExampleWorkflowLoads(t *testing.T) {
	all := Config{"vscode": true, "jupyter": true, "terminal": true, "filesystem": true}
	if err := workflow.Load("examples/workflow.yml", commands(all)); err != nil {
		t.Fatal(err)
	}
	if sigs := workflow.Signals(); !slices.Equal(sigs, []string{"SIGUSR1"}) {
		t.Fatalf("the example watches %v, want SIGUSR1", sigs)
	}
}

func TestBindsLoopbackAndUnwinds(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sd.sock")

	bin, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--port", "0", "--socket", socket)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	timer := time.AfterFunc(20*time.Second, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() { timer.Stop() })

	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", socket); err == nil {
			_, _ = io.WriteString(c, "GET /api/v1/health HTTP/1.0\r\n\r\n")
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			answer, _ := io.ReadAll(c)
			_ = c.Close()
			if strings.Contains(string(answer), `"status":"ok"`) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket never answered a request, so no listener was up to unwind")
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("a signalled shutdown must exit zero: %v", err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket outlived shutdown, so its listener was never closed: %v", err)
	}
	listening := 0
	for _, line := range strings.Split(stderr.String(), "\n") {
		if _, addr, ok := strings.Cut(line, "listening on "); ok {
			listening++
			if !strings.HasPrefix(addr, "127.0.0.1:") && addr != socket {
				t.Fatalf("bound somewhere reachable off-node: %q", line)
			}
		}
	}
	if listening != 2 {
		t.Fatalf("%d listeners reported, want the port and the socket:\n%s", listening, stderr.String())
	}
}
