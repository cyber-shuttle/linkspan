package main

import (
	"bytes"
	"flag"
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

func TestVersionPrintsAsOneBareLine(t *testing.T) {
	if version == "" || version != strings.TrimSpace(version) || strings.ContainsAny(version, "\n\r") {
		t.Errorf("consumers match --version output whole and anchored; got %q", version)
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
