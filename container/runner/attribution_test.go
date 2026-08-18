package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreApp writes an app definition into the store the test's XDG_CONFIG_HOME points at,
// so a command can be driven by NAME. `where` resolves an address against the store, which a
// path argument does not exercise.
func writeStoreApp(t *testing.T, name, body string) {
	t.Helper()
	apps := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "zinc", "apps")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apps, name+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// busAppYAML is an app that asked for a filtered bus. KeepUserID rides along because
// validation refuses the pair without it (the proxy serves the socket as the invoking user).
func busAppYAML(name string) string {
	return "SchemaVersion: 3\nType: ZincContainer\nAppNameID: " + name + "\n" +
		"ImageMeta:\n  Image: docker.io/library/alpine" + digestPin + "\n" +
		"InternalUserMeta:\n  KeepUserID: true\n" +
		"DBusMeta:\n  Talk:\n    - org.freedesktop.portal.Desktop\n"
}

// fixture points the store, the runtime dir and the state dir at known values, so every path
// this reports is fully determined by the test rather than by the machine running it.
func fixture(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
}

// THE contract for the desktop: the exact JSON `zcr where --json` emits for an instance with a
// filtered bus. Asserted byte for byte, because a consumer scripts against these field names
// and this nesting, and a rename that "reads better" is a break they discover at runtime.
func TestWhereJSONIsTheContract(t *testing.T) {
	fixture(t)
	writeStoreApp(t, "notes", busAppYAML("notes"))

	var err error
	out := captureStdout(t, func() { err = run([]string{"where", "notes@work", "--json"}) })
	if err != nil {
		t.Fatalf("where --json: %v", err)
	}
	want := `{
  "address": "notes@work",
  "app": "notes",
  "instance": "work",
  "container": "notes.work",
  "state": "/state/zinc/notes/work",
  "bus": {
    "socket": "/run/user/1000/zinc/dbus/notes.work/bus",
    "proxy": "zinc-dbus-notes.work"
  }
}
`
	if out != want {
		t.Errorf("where --json output changed:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// The un-instanced app: same command, empty instance, and a container name that is still the
// bare app name. An app authored before instances existed must not be reported under a
// different container than the one it actually runs as.
func TestWhereJSONUninstanced(t *testing.T) {
	fixture(t)
	writeStoreApp(t, "notes", busAppYAML("notes"))

	var err error
	out := captureStdout(t, func() { err = run([]string{"where", "notes", "--json"}) })
	if err != nil {
		t.Fatalf("where --json: %v", err)
	}
	var report whereReport
	if uerr := json.Unmarshal([]byte(out), &report); uerr != nil {
		t.Fatalf("output is not JSON: %v\n%s", uerr, out)
	}
	if report.Address != "notes" || report.Instance != "" || report.Container != "notes" {
		t.Errorf("un-instanced report = %+v", report)
	}
	if report.Bus == nil || report.Bus.Proxy != "zinc-dbus-notes" ||
		report.Bus.Socket != "/run/user/1000/zinc/dbus/notes/bus" {
		t.Errorf("un-instanced bus = %+v", report.Bus)
	}
}

// An app with no DBusMeta has no proxy and no socket - not an empty path, and not a path
// under a directory that was never created. A consumer that saw a path here would go looking
// for a file that cannot exist, and blame the wrong side when it is missing.
func TestWhereReportsNoBusForAnAppThatAskedForNone(t *testing.T) {
	fixture(t)
	writeStoreApp(t, "plain", "SchemaVersion: 3\nType: ZincContainer\nAppNameID: plain\n"+
		"ImageMeta:\n  Image: docker.io/library/alpine"+digestPin+"\n")

	var err error
	out := captureStdout(t, func() { err = run([]string{"where", "plain", "--json"}) })
	if err != nil {
		t.Fatalf("where --json: %v", err)
	}
	if !strings.Contains(out, `"bus": null`) {
		t.Errorf("an app with no DBusMeta must report a null bus, got:\n%s", out)
	}

	text := captureStdout(t, func() { err = run([]string{"where", "plain"}) })
	if err != nil {
		t.Fatalf("where: %v", err)
	}
	want := "state: /state/zinc/plain\ncontainer: plain\nbus-socket: none\nbus-proxy: none\n"
	if text != want {
		t.Errorf("where text output:\ngot:\n%s\nwant:\n%s", text, want)
	}
}

// The labelled text form is the other half of the contract: the labels are what a shell cuts
// on, and every one of them is present whether or not the app has a bus.
func TestWhereTextForm(t *testing.T) {
	fixture(t)
	writeStoreApp(t, "notes", busAppYAML("notes"))

	var err error
	out := captureStdout(t, func() { err = run([]string{"where", "notes@work"}) })
	if err != nil {
		t.Fatalf("where: %v", err)
	}
	want := "state: /state/zinc/notes/work\n" +
		"container: notes.work\n" +
		"bus-socket: /run/user/1000/zinc/dbus/notes.work/bus\n" +
		"bus-proxy: zinc-dbus-notes.work\n"
	if out != want {
		t.Errorf("where text output:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// The bus answer comes from the app's config, so an app that is not defined has no answer.
// Guessing would report a socket path with the same confidence as a real one.
func TestWhereRefusesAnUndefinedApp(t *testing.T) {
	fixture(t)
	if err := run([]string{"where", "ghost"}); err == nil {
		t.Fatal("where on an undefined app: want an error, got nil")
	}
}

// A proxy container name plus a pid is the whole of the reverse lookup. Containers that are
// not proxies must not appear: the app's own container is running too, and attributing a bus
// connection to it would name the right app for the wrong reason.
func TestBusRowsOnlyProxies(t *testing.T) {
	defined := func(name string) bool { return name == "notes" }
	pids := map[string]int{
		"notes.work":               4001, // the app container itself
		"zinc-dbus-notes.work":     4002,
		"zinc-dbus-notes":          4003,
		"some-unrelated-container": 4004,
	}
	rows := busRows(pids, "/run/user/1000", defined)

	if len(rows) != 2 {
		t.Fatalf("busRows returned %d rows, want 2 (the two proxies): %+v", len(rows), rows)
	}
	// Sorted by address, so "notes" precedes "notes@work" and the output is reproducible.
	if rows[0].Address != "notes" || rows[1].Address != "notes@work" {
		t.Fatalf("rows are not sorted by address: %+v", rows)
	}
	if rows[1].App != "notes" || rows[1].Instance != "work" {
		t.Errorf("the instance was not recovered from the proxy name: %+v", rows[1])
	}
	if rows[1].PID != 4002 || rows[1].Proxy != "zinc-dbus-notes.work" {
		t.Errorf("row does not carry the proxy's own pid and name: %+v", rows[1])
	}
	if rows[1].Socket != "/run/user/1000/zinc/dbus/notes.work/bus" {
		t.Errorf("row socket = %q, want the app's filtered socket", rows[1].Socket)
	}
}

// THE contract for the desktop's reverse lookup: given a pid observed on the host bus, this
// is the shape it selects on.
func TestBusJSONIsTheContract(t *testing.T) {
	rows := busRows(map[string]int{"zinc-dbus-notes.work": 4002}, "/run/user/1000",
		func(name string) bool { return name == "notes" })
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `[
  {
    "address": "notes@work",
    "app": "notes",
    "instance": "work",
    "container": "notes.work",
    "proxy": "zinc-dbus-notes.work",
    "pid": 4002,
    "socket": "/run/user/1000/zinc/dbus/notes.work/bus"
  }
]`
	if string(encoded) != want {
		t.Errorf("bus --json shape changed:\ngot:\n%s\nwant:\n%s", encoded, want)
	}
}

// Nothing running is an empty table, not a null one: a consumer iterating the JSON must not
// have to special-case "no app has a bus right now".
func TestBusJSONEmptyIsAnArray(t *testing.T) {
	encoded, err := json.Marshal(busRows(map[string]int{}, "/run/user/1000", nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Errorf("empty table = %s, want []", encoded)
	}
}

// The text form is what a shell reads. pid is the second column because it is the one a
// consumer looks a connection up by.
func TestBusTextForm(t *testing.T) {
	rows := busRows(map[string]int{"zinc-dbus-notes.work": 4002}, "/run/user/1000",
		func(name string) bool { return name == "notes" })
	want := "notes@work\t4002\tzinc-dbus-notes.work\t/run/user/1000/zinc/dbus/notes.work/bus\n"
	if got := renderBus(rows); got != want {
		t.Errorf("renderBus = %q, want %q", got, want)
	}
}

// A proxy for an app that is no longer defined (deleted while running) must still be
// attributable: the runtime name is reported as the app, rather than the row being dropped and
// a live bus connection becoming unattributable.
func TestBusRowsKeepsProxiesForUndefinedApps(t *testing.T) {
	rows := busRows(map[string]int{"zinc-dbus-gone": 4005}, "/run/user/1000", func(string) bool { return false })
	if len(rows) != 1 || rows[0].Address != "gone" || rows[0].PID != 4005 {
		t.Errorf("busRows dropped or mangled a proxy whose app is undefined: %+v", rows)
	}
}

func TestSplitJSONFlag(t *testing.T) {
	// The flag may lead or trail; both spellings reach the same place.
	for _, argv := range [][]string{{"notes", "--json"}, {"--json", "notes"}} {
		rest, asJSON, err := splitJSONFlag(argv)
		if err != nil || !asJSON || len(rest) != 1 || rest[0] != "notes" {
			t.Errorf("splitJSONFlag(%v) = %v, %v, %v", argv, rest, asJSON, err)
		}
	}
	if _, _, err := splitJSONFlag([]string{"--nope"}); err == nil {
		t.Error("an unknown flag should be refused, not treated as an app name")
	}
}
