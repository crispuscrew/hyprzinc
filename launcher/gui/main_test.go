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
		t.Fatalf("zlg firefox: %v", err)
	}
}

func TestRun_DirectLaunchSurfacesError(t *testing.T) {
	fakeZcr(t)
	err := run([]string{"bad"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want the zcr error surfaced, got %v", err)
	}
}

// ZLG_OPACITY accepts both forms people reach for - a percentage and a fraction - and
// reports the ones it cannot use instead of ignoring them silently.
func TestParseOpacity(t *testing.T) {
	valid := map[string]float64{
		"20":    0.2, // percentage
		"0.2":   0.2, // the same value as a fraction
		"90":    0.9,
		"0.9":   0.9,
		"1":     1, // above-1 means percent, so 1 stays a fraction: fully opaque
		"1.0":   1,
		"100":   1,
		"0":     0,
		" 35 ":  0.35, // surrounding whitespace is tolerated
		"12.5":  0.125,
		"0.125": 0.125,
	}
	for raw, want := range valid {
		got, ok := parseOpacity(raw)
		if !ok {
			t.Errorf("parseOpacity(%q) reported invalid, want %v", raw, want)
			continue
		}
		if got != want {
			t.Errorf("parseOpacity(%q) = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{"", "abc", "20%", "-1", "101", "1e9", "0.2.3"} {
		if got, ok := parseOpacity(raw); ok {
			t.Errorf("parseOpacity(%q) = %v, want it reported invalid", raw, got)
		}
	}
}

// An unusable ZLG_OPACITY leaves the overlay opaque rather than applying a garbage value,
// and a usable one reaches menu.Options.
func TestMenuOptions_Opacity(t *testing.T) {
	t.Setenv("ZLG_OPACITY", "20")
	if got := menuOptions().Opacity; got != 0.2 {
		t.Errorf("Opacity = %v for ZLG_OPACITY=20, want 0.2", got)
	}
	t.Setenv("ZLG_OPACITY", "0.2")
	if got := menuOptions().Opacity; got != 0.2 {
		t.Errorf("Opacity = %v for ZLG_OPACITY=0.2, want 0.2", got)
	}
	t.Setenv("ZLG_OPACITY", "not-a-number")
	if got := menuOptions().Opacity; got != 0 {
		t.Errorf("Opacity = %v for an unusable value, want 0 (opaque)", got)
	}
}

// The remaining env knobs reach menu.Options, and an unset environment leaves the defaults.
func TestMenuOptions_Flags(t *testing.T) {
	for _, name := range []string{"ZLG_OPACITY", "ZLG_NO_ANIM", "ZLG_DEBUG", "ZLG_FONT"} {
		t.Setenv(name, "") // an empty value reads the same as unset, and survives an exported one
	}
	opts := menuOptions()
	if opts.NoAnim || opts.Debug || opts.FontPath != "" || opts.Opacity != 0 {
		t.Errorf("unset environment should leave the defaults, got %+v", opts)
	}
	if opts.AppID != "zinc.launcher" {
		t.Errorf("AppID = %q, want zinc.launcher (compositors match window rules on it)", opts.AppID)
	}
	t.Setenv("ZLG_NO_ANIM", "1")
	t.Setenv("ZLG_DEBUG", "1")
	t.Setenv("ZLG_FONT", "/usr/share/fonts/x.ttf")
	opts = menuOptions()
	if !opts.NoAnim || !opts.Debug || opts.FontPath != "/usr/share/fonts/x.ttf" {
		t.Errorf("env knobs did not reach the options, got %+v", opts)
	}
}

// loadItems lists every app, and a file that fails to decode is still shown by name (with
// an empty description) rather than hidden.
func TestLoadItems_ListsUndecodableByName(t *testing.T) {
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

	items, err := loadItems()
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	found := map[string]bool{}
	for _, item := range items {
		found[item.Label] = true
		desc[item.Label] = item.Description
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
