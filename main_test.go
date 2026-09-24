// Tests for the surface docs/COMPATIBILITY.md freezes.
//
//	binary                       Built once per run.
//	TestFlagSurface, TestRoutesFollowConfig
//	TestRoutesCoverCommands      Every command a subsystem exports is behind one of its routes.
//	TestVersionIsOneLine, TestArchiveName
//	TestExampleWorkflowLoads     examples/workflow.yml must name only commands the subsystems export.
//	TestBindsLoopbackAndUnwinds  Sends SIGTERM once the port answers, and reads stderr to its end before Wait.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		"tunnel-devtunnel-args",
		"tunnel-enable",
		"tunnel-mode",
		"tunnel-websocket-args",
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
	get := func(cfg config, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		routes(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get(config{}, "/api/v1/health"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health answered %d %s, want the documented literal", rec.Code, rec.Body)
	}
	var snap map[string]any
	if rec := get(config{}, "/api/v1/metrics"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snap) != nil {
		t.Fatalf("metrics answered %d %s, want an object", rec.Code, rec.Body)
	}
	all := config{"workflow": true, "vscode": true, "jupyter": true, "terminal": true, "filesystem": true, "checkpoint": true}
	for _, path := range []string{"/api/v1/vscode/sessions", "/api/v1/jupyter/sessions", "/api/v1/terminal/sessions"} {
		if rec := get(all, path); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d with its subsystem enabled, want 200", path, rec.Code)
		}
		if rec := get(config{}, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d with its subsystem disabled, want 404", path, rec.Code)
		}
	}
	post := func(cfg config, path string) int {
		rec := httptest.NewRecorder()
		routes(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec.Code
	}
	if post(all, "/api/v1/filesystem/mount") != http.StatusBadRequest || post(config{}, "/api/v1/filesystem/mount") != http.StatusNotFound {
		t.Error("a filesystem route must want its params when enabled and answer 404 when disabled")
	}
	if post(all, "/api/v1/workflow/shell/exec") != http.StatusBadRequest || post(config{}, "/api/v1/workflow/shell/exec") != http.StatusNotFound {
		t.Error("the workflow route must refuse an empty command when enabled and answer 404 when disabled")
	}
}

func TestRoutesCoverCommands(t *testing.T) {
	for name, sub := range subsystems {
		for command, c := range sub.commands {
			routed := false
			for _, r := range sub.router.Routes {
				routed = routed || reflect.ValueOf(r).Pointer() == reflect.ValueOf(c).Pointer()
			}
			if !routed {
				t.Errorf("%s.%s is a command but not a route", name, command)
			}
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
		t.Errorf("cs-plane reads the first line as one X.Y.Z token; got %q", out)
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
	all := config{"vscode": true, "jupyter": true, "terminal": true, "filesystem": true, "checkpoint": true}
	for _, example := range []string{"examples/workflow.yml", "examples/checkpoint.yml", "examples/restore.yml"} {
		if err := workflow.Load(example, commands(all)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBindsLoopbackAndUnwinds(t *testing.T) {
	bin, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--port", "0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	timer := time.AfterFunc(20*time.Second, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() { timer.Stop() })

	lines := bufio.NewScanner(stderr)
	var addr string
	for addr == "" && lines.Scan() {
		_, addr, _ = strings.Cut(lines.Text(), "listening on ")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("bound %q, want loopback", addr)
	}
	resp, err := http.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health answered %d", resp.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for lines.Scan() {
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("a signalled shutdown must exit zero: %v", err)
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		_ = c.Close()
		t.Fatal("the port outlived shutdown, so its listener was never closed")
	}
}
