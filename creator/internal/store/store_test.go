package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
)

// digestPin is a canonical sha256 digest (64 hex chars) - the form section 5.5 requires for
// third-party images, so saved/marshalled fixtures pass Validate.
const digestPin = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func sampleApp(name string) schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     name,
		ImageMeta:     schema.ImageMeta{Image: "docker.io/library/" + name + digestPin},
	}
}

func tempStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Root: t.TempDir()}
}

// A crafted name (a path separator or a ".." segment) must not escape the apps
// directory through Load / Delete / Exists.
func TestStore_RejectsUnsafeNames(t *testing.T) {
	sto := tempStore(t)
	for _, bad := range []string{"../evil", "sub/app", "..", ""} {
		if _, err := sto.Load(bad); err == nil {
			t.Errorf("Load(%q): want error, got nil", bad)
		}
		if err := sto.Delete(bad); err == nil {
			t.Errorf("Delete(%q): want error, got nil", bad)
		}
		if sto.Exists(bad) {
			t.Errorf("Exists(%q): want false, got true", bad)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	sto := tempStore(t)
	want := sampleApp("firefox")
	if err := sto.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := sto.Load("firefox")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AppNameID != want.AppNameID || got.ImageMeta.Image != want.ImageMeta.Image || got.Type != want.Type {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
	if err := validate.Validate(got); err != nil {
		t.Fatalf("round-tripped config does not validate: %v", err)
	}
}

func TestListExistsDelete(t *testing.T) {
	sto := tempStore(t)

	if names, err := sto.List(); err != nil || len(names) != 0 {
		t.Fatalf("empty store: names=%v err=%v", names, err)
	}
	if sto.Exists("firefox") {
		t.Fatal("Exists should be false before save")
	}

	for _, name := range []string{"zed", "firefox", "ghostty"} {
		if err := sto.Save(sampleApp(name)); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}

	names, err := sto.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := names; len(got) != 3 || got[0] != "firefox" || got[1] != "ghostty" || got[2] != "zed" {
		t.Fatalf("List not sorted/complete: %v", got)
	}
	if !sto.Exists("firefox") {
		t.Fatal("Exists should be true after save")
	}

	if err := sto.Delete("firefox"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if sto.Exists("firefox") {
		t.Fatal("Exists should be false after delete")
	}
	if err := sto.Delete("firefox"); err != nil {
		t.Fatalf("deleting a missing app should be a no-op, got: %v", err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	sto := tempStore(t)
	bad := sampleApp("firefox")
	bad.ImageMeta.Image = "alpine:latest" // third-party, not digest-pinned (section 5.5)

	if err := sto.Save(bad); err == nil {
		t.Fatal("Save should reject invalid config")
	}
	if sto.Exists("firefox") {
		t.Fatal("nothing should be written when validation fails")
	}
}

func TestMarshalLoadRoundtrip(t *testing.T) {
	// The $EDITOR flow marshals a draft, lets the user edit it, then re-reads via Load -
	// which rejects unknown keys. So Marshal's output must round-trip cleanly.
	cfg := sampleApp("rt")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
		IPv4CIDR: []string{"1.1.1.1/32"},
		Ports:    []int{443},
	}}}

	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "rt.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("round-trip Load failed (Marshal emitted a key Load rejects?): %v", err)
	}
	if got.ImageMeta.Image != cfg.ImageMeta.Image || len(got.NetworkMeta.NetworkLists) != 1 ||
		len(got.NetworkMeta.NetworkLists[0].IPv4CIDR) != 1 || len(got.NetworkMeta.NetworkLists[0].Ports) != 1 {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	const body = `SchemaVersion: 3
Type: ZincContainer
AppNameID: x
ImageMeta:
  Image: img@sha256:abc
typpo: drift
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "typpo") {
		t.Fatalf("expected unknown-key error mentioning the stray field, got: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	sto, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if want := filepath.Join("/xdg", "zinc", "apps"); sto.Root != want {
		t.Fatalf("Default root = %q, want %q", sto.Root, want)
	}
	if want := filepath.Join("/xdg", "zinc", "apps", "firefox.yaml"); sto.Path("firefox") != want {
		t.Fatalf("Path = %q, want %q", sto.Path("firefox"), want)
	}
}

// writeApp puts a raw app file in the store, which is how an inheriting app is authored:
// what a config STATES is a property of its text, and Save deliberately refuses to
// reconstruct that from a struct.
func writeApp(t *testing.T, sto *Store, name, text string) {
	t.Helper()
	if err := os.MkdirAll(sto.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sto.Path(name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Load returns the file as written and LoadResolved returns what the app actually is. The
// split is the whole safety property: the editing side reads raw, so what it writes back is
// still the child.
func TestLoadResolved_MergesTheBase(t *testing.T) {
	sto := tempStore(t)
	writeApp(t, sto, "base", "SchemaVersion: 3\nType: ZincContainer\nAppNameID: base\n"+
		"ImageMeta:\n  Image: localhost/base:local\n"+
		"ResourcesMeta:\n  MaxRamMiB: 256\n  PIDsLimit: 64\n"+
		"HostTheme: true\nCapabilities: [NET_RAW]\n")
	writeApp(t, sto, "child", "SchemaVersion: 3\nType: ZincContainer\nAppNameID: child\nInherits: base\n"+
		"ResourcesMeta:\n  MaxRamMiB: 1024\n"+
		"HostTheme: false\nCapabilities: []\n")

	raw, err := sto.Load("child")
	if err != nil {
		t.Fatal(err)
	}
	if raw.ImageMeta.Image != "" {
		t.Errorf("Load must return the file as written, got Image=%q", raw.ImageMeta.Image)
	}

	resolved, err := sto.LoadResolved("child")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ImageMeta.Image != "localhost/base:local" {
		t.Errorf("Image = %q, want the base's", resolved.ImageMeta.Image)
	}
	if resolved.ResourcesMeta.MaxRamMiB != 1024 {
		t.Errorf("MaxRamMiB = %d, want the child's", resolved.ResourcesMeta.MaxRamMiB)
	}
	if resolved.ResourcesMeta.PIDsLimit != 64 {
		t.Errorf("PIDsLimit = %d, want the base's - a nested block merges field by field", resolved.ResourcesMeta.PIDsLimit)
	}
	if resolved.HostTheme {
		t.Error("a child stating false must be able to turn a base's flag off")
	}
	if len(resolved.Capabilities) != 0 {
		t.Errorf("Capabilities = %v, want the child's empty list to win", resolved.Capabilities)
	}
}

// The data-loss guard. A decoded AppConfig no longer knows which fields its file stated, so
// writing one back over an inheriting app would state all of them - replacing everything it
// inherits with zeros, silently, in a file that looks perfectly normal afterwards.
func TestSave_RefusesToRewriteAnInheritingApp(t *testing.T) {
	sto := tempStore(t)
	writeApp(t, sto, "base", "SchemaVersion: 3\nType: ZincContainer\nAppNameID: base\n"+
		"ImageMeta:\n  Image: localhost/base:local\n")
	const childText = "SchemaVersion: 3\nType: ZincContainer\nAppNameID: child\nInherits: base\nIcon: firefox\n"
	writeApp(t, sto, "child", childText)

	cfg, err := sto.Load("child")
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Save(cfg); err == nil {
		t.Fatal("saving an inheriting app must be refused, not silently flattened")
	} else if !strings.Contains(err.Error(), "inherits from") {
		t.Errorf("the refusal should say why: %v", err)
	}

	// And nothing was written: the guard runs before anything touches disk.
	after, err := os.ReadFile(sto.Path("child"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != childText {
		t.Errorf("the file changed despite the refusal:\n%s", after)
	}
}

// An app that inherits nothing saves exactly as it always did - the guard must not cost
// every other app anything.
func TestSave_UnaffectedWithoutInheritance(t *testing.T) {
	sto := tempStore(t)
	if err := sto.Save(sampleApp("solo")); err != nil {
		t.Fatalf("a config with no Inherits must still save: %v", err)
	}
}

// A base that does not exist, a cycle, and a name that would escape the apps directory all
// fail the read rather than yielding a partial config - what is missing could be the part
// that contains the app.
func TestLoadResolved_FailsClosed(t *testing.T) {
	sto := tempStore(t)
	writeApp(t, sto, "orphan", "SchemaVersion: 3\nAppNameID: orphan\nInherits: ghost\n")
	writeApp(t, sto, "loop-a", "SchemaVersion: 3\nAppNameID: loop-a\nInherits: loop-b\n")
	writeApp(t, sto, "loop-b", "SchemaVersion: 3\nAppNameID: loop-b\nInherits: loop-a\n")
	writeApp(t, sto, "escape", "SchemaVersion: 3\nAppNameID: escape\nInherits: ../../etc/evil\n")

	for _, testCase := range []struct{ app, want string }{
		{"orphan", "ghost"},
		{"loop-a", "cycle"},
		{"escape", "Inherits"},
	} {
		_, err := sto.LoadResolved(testCase.app)
		if err == nil {
			t.Errorf("LoadResolved(%s): want an error", testCase.app)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("LoadResolved(%s) = %v, want it to mention %q", testCase.app, err, testCase.want)
		}
	}
}

// An app must not resolve into another app's identity. AppNameID is what the runner names
// the container, the pod and the derived image after, so a child that omitted it and took
// its base's name would make `zcr run notes` build, and `zcr stop notes` destroy, whatever
// `browser` is. Inheriting apps are hand-written, so nothing else keeps the two in step.
func TestLoadResolved_RefusesToTakeTheBasesIdentity(t *testing.T) {
	sto := tempStore(t)
	writeApp(t, sto, "browser", "SchemaVersion: 3\nType: ZincContainer\nAppNameID: browser\n"+
		"ImageMeta:\n  Image: localhost/browser:local\n")
	writeApp(t, sto, "notes", "SchemaVersion: 3\nInherits: browser\nIcon: notes\n")

	_, err := sto.LoadResolved("notes")
	if err == nil {
		t.Fatal("an app resolving to another app's AppNameID must be refused")
	}
	if !strings.Contains(err.Error(), "keep its own name") {
		t.Errorf("the refusal should say why: %v", err)
	}

	// Stating its own name is all it takes.
	writeApp(t, sto, "notes", "SchemaVersion: 3\nAppNameID: notes\nInherits: browser\nIcon: notes\n")
	cfg, err := sto.LoadResolved("notes")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageMeta.Image != "localhost/browser:local" {
		t.Errorf("it should still inherit everything else, got Image=%q", cfg.ImageMeta.Image)
	}
}

// A file dropped into the apps directory should not become a runnable row just by being
// there. Anything outside the schema's name charset is skipped, so a flag-shaped or
// path-shaped filename never reaches the runner.
func TestListSkipsNamesOutsideTheSchemaCharset(t *testing.T) {
	dir := t.TempDir()
	for _, filename := range []string{
		"notes.yaml",      // a normal app
		"--net=host.yaml", // would land in zcr's flag slot
		"Firefox.yaml",    // uppercase is not a legal app name
		".hidden.yaml",    // must start alphanumeric
	} {
		if err := os.WriteFile(filepath.Join(dir, filename), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sto := &Store{Root: dir}
	names, err := sto.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "notes" {
		t.Fatalf("List() = %v, want only [notes]", names)
	}
}

// A dot is legal in an app name, so "notes.yaml.yaml" does list as the key "notes.yaml":
// the charset cannot tell that one apart from a real app. It is refused one layer later, at
// the exec boundary, because that is where the ".yaml" suffix actually means something (zcr
// reads such an argument as a path). Recorded here so the split is deliberate rather than an
// oversight in whichever layer someone reads first.
func TestListKeepsDottedNamesForTheExecBoundaryToRefuse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sto := &Store{Root: dir}
	names, err := sto.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "notes.yaml" {
		t.Fatalf("List() = %v, want [notes.yaml]", names)
	}
}

// "my..app" is a legal schema name that contains "..", so a substring test refused a name
// the validator accepts, leaving an app zc could create and then never touch again.
func TestSafeNameAllowsDotsButRefusesTraversal(t *testing.T) {
	if err := safeName("my..app"); err != nil {
		t.Errorf("my..app is a legal app name, got: %v", err)
	}
	for _, name := range []string{"..", ".", "../evil", "a/b"} {
		if err := safeName(name); err == nil {
			t.Errorf("safeName(%q) accepted a name that escapes the store", name)
		}
	}
}
