package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	creatorstore "github.com/crispuscrew/zinc/creator/internal/store"
)

const digestPin = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// quiet redirects stdout to /dev/null for the duration of the test, so the commands'
// success prints don't clutter test output.
func quiet(t *testing.T) {
	t.Helper()
	old := os.Stdout
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = null
	t.Cleanup(func() { os.Stdout = old; null.Close() })
}

func TestRunUsageAndUnknown(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "usage: zc") {
		t.Fatalf("no args should return usage, got: %v", err)
	}
	if err := run([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("a bogus command should be rejected, got: %v", err)
	}
}

func TestVersionDispatch(t *testing.T) {
	quiet(t)
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("--version: %v", err)
	}
}

// The authoring commands (new/list/validate/delete) work locally against the store, with
// no runtime needed. XDG_CONFIG_HOME isolates the store to a temp dir.
func TestAuthoringLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)

	if err := run([]string{"new", "demo", "--image", "docker.io/library/alpine" + digestPin}); err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := run([]string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := run([]string{"validate", "demo"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// A duplicate name is refused.
	if err := run([]string{"new", "demo", "--image", "docker.io/library/alpine" + digestPin}); err == nil {
		t.Fatal("new should refuse an existing name")
	}
	if err := run([]string{"delete", "demo"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := run([]string{"validate", "demo"}); err == nil {
		t.Fatal("validate should fail after the app is deleted")
	}
}

func TestInitSeedsValidAppsAndPreservesExistingFiles(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	quiet(t)

	if err := run([]string{"init"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, seed := range seedApps {
		if err := run([]string{"validate", seed.name}); err != nil {
			t.Fatalf("validate %s: %v", seed.name, err)
		}
		configPath := filepath.Join(configRoot, "zinc", "apps", seed.name+".yaml")
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("stat %s: %v", seed.name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode: got %o, want 600", seed.name, info.Mode().Perm())
		}
		cfg, err := creatorstore.Load(configPath)
		if err != nil {
			t.Fatalf("load %s: %v", seed.name, err)
		}
		if !cfg.DisplayMeta.DisableGpuAccess {
			t.Fatalf("%s grants the GPU despite being terminal-only", seed.name)
		}
	}

	shellPath := filepath.Join(configRoot, "zinc", "apps", "example-shell.yaml")
	if err := os.WriteFile(shellPath, []byte("keep this file\n"), 0o600); err != nil {
		t.Fatalf("replace fixture: %v", err)
	}
	if err := run([]string{"init"}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	preserved, err := os.ReadFile(shellPath)
	if err != nil {
		t.Fatalf("read preserved fixture: %v", err)
	}
	if string(preserved) != "keep this file\n" {
		t.Fatalf("init replaced an existing app: %q", preserved)
	}
	if err := run([]string{"init", "--force"}); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	if err := run([]string{"validate", "example-shell"}); err != nil {
		t.Fatalf("validate forced seed: %v", err)
	}
}

// new with a non-digest-pinned third-party image is rejected by validation (section 5.5).
func TestNewRejectsUnpinnedImage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)
	if err := run([]string{"new", "demo", "--image", "alpine:latest"}); err == nil {
		t.Fatal("new should reject a non-digest-pinned third-party image")
	}
}

// The runtime commands delegate to zcr; with none on $PATH they fail with the delegate's
// actionable error rather than doing anything locally.
func TestRuntimeDelegateNeedsZcr(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir → no zcr
	for _, cmd := range []string{"run", "build", "stop", "logs", "image"} {
		err := run([]string{cmd, "demo"})
		if err == nil || !strings.Contains(err.Error(), "not found on $PATH") {
			t.Fatalf("%s with no zcr: want the delegate not-found error, got: %v", cmd, err)
		}
	}
}
