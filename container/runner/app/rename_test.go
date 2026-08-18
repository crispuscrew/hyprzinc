package app

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/adapters/fs"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// renameSvc wires a real on-disk store (so the load → rewrite-name → save → delete
// round-trip is exercised for real) with a fake runtime carrying the running set.
// depApp / digestPin are shared with the package's other tests.
func renameSvc(t *testing.T, engine ports.Runtime, apps ...schema.AppConfig) (Service, *fs.Store) {
	t.Helper()
	sto := &fs.Store{Root: t.TempDir()}
	for _, cfg := range apps {
		if err := sto.Save(cfg); err != nil {
			t.Fatalf("seed %s: %v", cfg.AppNameID, err)
		}
	}
	return New(sto, engine, nil, nil, nil, nil, nil, nil), sto
}

// A plain rename rewrites AppNameID, persists under the new name, and removes the old
// definition; the new file decodes with the new name inside it (not just a moved file).
func TestRename(t *testing.T) {
	svc, sto := renameSvc(t, newFakeRuntime(), depApp("old"))
	if err := svc.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	if sto.Exists("old") {
		t.Fatal("old definition should be gone")
	}
	cfg, err := sto.Load("new")
	if err != nil {
		t.Fatalf("new definition should load: %v", err)
	}
	if cfg.AppNameID != "new" {
		t.Fatalf("AppNameID inside the file should be rewritten, got %q", cfg.AppNameID)
	}
}

// Renaming onto an existing name must not clobber it, and must leave the source intact.
func TestRenameRefusesExistingTarget(t *testing.T) {
	svc, sto := renameSvc(t, newFakeRuntime(), depApp("old"), depApp("taken"))
	err := svc.Rename("old", "taken")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected an already-exists error, got %v", err)
	}
	if !sto.Exists("old") {
		t.Fatal("a refused rename must leave the source untouched")
	}
}

// A running app can't be renamed - its container is named after the old name.
func TestRenameRefusesRunning(t *testing.T) {
	svc, sto := renameSvc(t, newFakeRuntime("old"), depApp("old"))
	err := svc.Rename("old", "new")
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected a running-app error, got %v", err)
	}
	if !sto.Exists("old") || sto.Exists("new") {
		t.Fatal("a refused rename must change nothing on disk")
	}
}

// An invalid new name is rejected by Save's validation before the old file is removed.
func TestRenameRejectsInvalidName(t *testing.T) {
	svc, sto := renameSvc(t, newFakeRuntime(), depApp("old"))
	err := svc.Rename("old", "Bad Name")
	if err == nil {
		t.Fatal("an invalid new name should fail")
	}
	if !sto.Exists("old") {
		t.Fatal("the old definition must survive a failed rename")
	}
}
