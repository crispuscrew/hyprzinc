package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZcr is a stand-in for the zcr binary: it canned-responds to the subcommands the
// delegate calls so the parsing/exit-code handling can be exercised without the real
// runtime. Each case mirrors what zcr actually prints (ps: one name per line; image
// search: name<TAB>desc; image resolve: the pinned ref).
const fakeZcr = `#!/bin/sh
case "$1" in
ps)
	printf 'alpha\nbeta\n'
	;;
run)
	if [ "$2" = "bad" ]; then echo "zcr: boom" >&2; exit 1; fi
	;;
stop|term)
	;;
logs)
	printf 'line1\nline2\n'
	;;
image)
	case "$2" in
	search)
		if [ "$3" = "none" ]; then echo "no images found"; exit 0; fi
		printf 'docker.io/a\tfirst\ndocker.io/b\tsecond\n'
		;;
	resolve)
		printf '  %s@sha256:deadbeef  \n' "$3"
		;;
	esac
	;;
boom)
	exit 7
	;;
esac
`

// withFakeZcr installs the fake zcr as the only thing on $PATH for the test, so
// exec.LookPath("zcr") finds it (and nothing else leaks in).
func withFakeZcr(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "zcr")
	if err := os.WriteFile(path, []byte(fakeZcr), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRunningParsesPs(t *testing.T) {
	withFakeZcr(t)
	got, err := Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if len(got) != 2 || !got["alpha"] || !got["beta"] {
		t.Fatalf("Running = %v, want {alpha,beta}", got)
	}
}

func TestSearchParsesRows(t *testing.T) {
	withFakeZcr(t)
	got, err := Search("editor")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 || got[0].Name != "docker.io/a" || got[0].Description != "first" || got[1].Name != "docker.io/b" {
		t.Fatalf("Search = %+v, want two name/desc rows", got)
	}

	empty, err := Search("none")
	if err != nil {
		t.Fatalf("Search(none): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("Search(none) should be empty, got %+v", empty)
	}
}

func TestResolveTrims(t *testing.T) {
	withFakeZcr(t)
	got, err := Resolve("docker.io/x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "docker.io/x@sha256:deadbeef" {
		t.Fatalf("Resolve = %q, want the trimmed pinned ref", got)
	}
}

func TestLaunchStopLogs(t *testing.T) {
	withFakeZcr(t)
	if err := Launch("ok"); err != nil {
		t.Fatalf("Launch(ok): %v", err)
	}
	if err := Stop("ok"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	body, err := Logs("ok")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if body != "line1\nline2\n" {
		t.Fatalf("Logs = %q", body)
	}
}

// A nonzero zcr exit with a stderr message is surfaced as the delegate's error.
func TestLaunchFoldsStderr(t *testing.T) {
	withFakeZcr(t)
	err := Launch("bad")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Launch(bad) should fold zcr's stderr, got: %v", err)
	}
}

// Passthrough preserves zcr's exit status.
func TestPassthroughExit(t *testing.T) {
	withFakeZcr(t)
	if err := Passthrough("boom"); err == nil {
		t.Fatal("Passthrough should return zcr's nonzero exit as an error")
	}
	if err := Passthrough("stop", "x"); err != nil {
		t.Fatalf("Passthrough of a succeeding command should be nil, got: %v", err)
	}
}

// With no zcr on $PATH, every runtime action fails with one actionable message.
func TestMissingZcr(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir → LookPath("zcr") fails
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"Launch", func() error { return Launch("x") }},
		{"Stop", func() error { return Stop("x") }},
		{"Passthrough", func() error { return Passthrough("run", "x") }},
		{"Running", func() error { _, err := Running(); return err }},
		{"Resolve", func() error { _, err := Resolve("x"); return err }},
	} {
		err := tc.run()
		if err == nil || !strings.Contains(err.Error(), "not found on $PATH") {
			t.Fatalf("%s with no zcr: want an actionable not-found error, got: %v", tc.name, err)
		}
	}
}

// zc hands the app name to another program's command line, so it has to be a name and not a
// flag or a path. The launcher has had these two guards since it shipped; zc drives the same
// runner from the same kind of list and had neither.
func TestLaunchRejectsFlagShapedName(t *testing.T) {
	withFakeZcr(t) // without a zcr on PATH, LookPath fails first and the guard is never reached
	if err := Launch("--net=host"); err == nil {
		t.Fatal("a flag-shaped app name was accepted")
	}
}

// zcr reads an argument ending in ".yaml" as a filesystem path, resolved against zc's own
// working directory. The store key "notes.yaml" comes from a dropped file "notes.yaml.yaml",
// so this would run ./notes.yaml: a config the store never held and the TUI never showed.
func TestLaunchRejectsYAMLSuffixedName(t *testing.T) {
	withFakeZcr(t)
	if err := Launch("notes.yaml"); err == nil {
		t.Fatal("a .yaml-suffixed app name was accepted")
	}
}
