package main

import (
	"bytes"
	"flag"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFlagsAreTheSurfaceClientsShipAgainst(t *testing.T) {
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

func TestVersionFlagPrintsOnlyTheInjectedVersion(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "linkspan")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "9.9.9\n" {
		t.Errorf("consumers match --version output whole and anchored; got %q", out)
	}
}

func TestHelpKeepsTheFlagSpellingConsumersGrepFor(t *testing.T) {
	fs := flag.NewFlagSet("linkspan", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	registerFlags(fs)
	fs.PrintDefaults()
	if !strings.Contains(out.String(), "-tunnel-host-token") {
		t.Error("cs-control greps --help for -tunnel-host-token before submitting a job")
	}
	if !strings.Contains(out.String(), "-socket string") {
		t.Error("a backquoted word in a usage string renames the printed argument")
	}
}

func TestFlagsAcceptBothSingleAndDoubleDashSpellings(t *testing.T) {
	for _, spelling := range []string{"-tunnel-enable", "--tunnel-enable"} {
		fs := flag.NewFlagSet("linkspan", flag.ContinueOnError)
		opts := registerFlags(fs)
		if err := fs.Parse([]string{spelling}); err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if !opts.tunnelEnable {
			t.Errorf("%s did not set the flag", spelling)
		}
	}
}
