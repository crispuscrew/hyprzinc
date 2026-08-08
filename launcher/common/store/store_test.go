package store

import (
	"os"
	"path/filepath"
	"testing"
)

func writeApp(t *testing.T, dir, name, desc string) {
	t.Helper()
	body := "SchemaVersion: 3\nType: ZincContainer\nAppNameID: " + name +
		"\nDescription: " + desc + "\nImageMeta:\n  Image: localhost/app:local\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// List returns app names (without the .yaml suffix), sorted, ignoring non-yaml files.
func TestList(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "firefox", "browser")
	writeApp(t, dir, "alacritty", "terminal")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	sto := &Store{Root: dir}

	names, err := sto.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alacritty" || names[1] != "firefox" {
		t.Fatalf("List = %v, want [alacritty firefox]", names)
	}
}

// A missing store directory lists as empty, not an error.
func TestList_MissingDirIsEmpty(t *testing.T) {
	sto := &Store{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	names, err := sto.List()
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("missing dir should be empty, got %v", names)
	}
}

// Load decodes an app file and its display fields.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "firefox", "Web browser")
	sto := &Store{Root: dir}

	cfg, err := sto.Load("firefox")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppNameID != "firefox" || cfg.Description != "Web browser" {
		t.Fatalf("Load = %+v, want AppNameID=firefox Description='Web browser'", cfg)
	}
}

// List skips files whose name is not a valid app key, so a flag-like or otherwise
// crafted filename never becomes a launchable picker entry.
func TestList_SkipsNonKeyNames(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "firefox", "browser")
	for _, bad := range []string{"--net=host", "Bad-Name", "has space", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, bad+".yaml"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names, err := (&Store{Root: dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "firefox" {
		t.Fatalf("List = %v, want only [firefox] (non-key names skipped)", names)
	}
}

// Load rejects a file with unknown keys (a stale/typo field) rather than ignoring them.
func TestLoad_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte("Bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Store{Root: dir}).Load("x"); err == nil {
		t.Fatal("Load should reject an unknown key")
	}
}

// An empty file is a clear error, not a zero-value config.
func TestLoad_EmptyFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Store{Root: dir}).Load("x"); err == nil {
		t.Fatal("Load should reject an empty file")
	}
}

// A crafted name (path separator or ".." segment) cannot read a file outside the store.
func TestLoad_RejectsUnsafeNames(t *testing.T) {
	sto := &Store{Root: t.TempDir()}
	for _, bad := range []string{"../evil", "sub/app", "..", ".", ""} {
		if _, err := sto.Load(bad); err == nil {
			t.Errorf("Load(%q): want error, got nil", bad)
		}
	}
}

// A name that merely contains two dots is a legal store key, not traversal: the separator
// check already confines a name to one path segment. Rejecting it as a substring left such an
// app listed by the picker (List's charset allows dots) but impossible to load or launch.
func TestLoad_AllowsDotsInsideAName(t *testing.T) {
	dir := t.TempDir()
	body := "SchemaVersion: 3\nType: ZincContainer\nAppNameID: my..app\nDescription: dotted\nImageMeta:\n  Image: localhost/x:local\n"
	if err := os.WriteFile(filepath.Join(dir, "my..app.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sto := &Store{Root: dir}
	names, err := sto.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "my..app" {
		t.Fatalf("List = %v, want [my..app]", names)
	}
	cfg, err := sto.Load("my..app")
	if err != nil {
		t.Fatalf("Load(%q) failed for an app the picker lists: %v", "my..app", err)
	}
	if cfg.Description != "dotted" {
		t.Errorf("Description = %q, want %q", cfg.Description, "dotted")
	}
}
