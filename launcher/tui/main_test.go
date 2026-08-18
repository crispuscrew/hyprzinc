package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mainFakeZcr = `#!/bin/sh
case "$1" in
  run) if [ "$2" = "bad" ]; then echo "bad: nope" 1>&2; exit 1; fi; exit 0 ;;
  ps) exit 0 ;;
  *) exit 2 ;;
esac
`

func fakeZcr(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "zcr"), []byte(mainFakeZcr), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRun_Version(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("--version: %v", err)
	}
}

func TestRun_Usage(t *testing.T) {
	if err := run([]string{"-h"}); err != nil {
		t.Fatalf("-h: %v", err)
	}
}

func TestRun_TooManyArgs(t *testing.T) {
	err := run([]string{"one", "two"})
	if err == nil || !strings.Contains(err.Error(), "too many arguments") {
		t.Fatalf("want a too-many-arguments error, got %v", err)
	}
}

func TestRun_DirectLaunch(t *testing.T) {
	fakeZcr(t)
	if err := run([]string{"firefox"}); err != nil {
		t.Fatalf("zlt firefox: %v", err)
	}
}

func TestRun_DirectLaunchSurfacesError(t *testing.T) {
	fakeZcr(t)
	err := run([]string{"bad"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want the zcr error surfaced, got %v", err)
	}
}

// loadApps lists every app, and a file that fails to decode is still shown by name (with
// an empty description) rather than hidden.
func TestLoadApps_ListsUndecodableByName(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	appsDir := filepath.Join(cfg, "zinc", "apps")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := "SchemaVersion: 3\nType: ZincContainer\nAppNameID: good\nDescription: fine\nImageMeta:\n  Image: localhost/x:local\n"
	if err := os.WriteFile(filepath.Join(appsDir, "good.yaml"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appsDir, "broken.yaml"), []byte("Bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	apps, err := loadApps()
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	found := map[string]bool{}
	for _, app := range apps {
		found[app.Name] = true
		desc[app.Name] = app.Description
	}
	if !found["good"] || desc["good"] != "fine" {
		t.Fatalf("good app should be listed with its description, got desc=%q", desc["good"])
	}
	if !found["broken"] {
		t.Fatal("an undecodable file should still be listed by name")
	}
	if desc["broken"] != "" {
		t.Fatalf("an undecodable file should list with an empty description, got %q", desc["broken"])
	}
}
