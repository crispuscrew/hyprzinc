package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const digestPin = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// quiet redirects stdout to /dev/null for the duration of the test, so the dry-run plan
// and validate output don't clutter test output.
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

// writeApp writes an app file to a temp dir and returns its path (an argument with a
// path separator is read directly, no store lookup).
func writeApp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunUsageAndUnknown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "usage: zcr") {
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

func TestValidateDispatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)

	good := writeApp(t, "SchemaVersion: 3\nType: ZincContainer\nAppNameID: demo\nImageMeta:\n  Image: docker.io/library/alpine"+digestPin+"\n")
	if err := run([]string{"validate", good}); err != nil {
		t.Fatalf("validate of a good app should pass, got: %v", err)
	}

	bad := writeApp(t, "SchemaVersion: 3\nType: ZincContainer\nAppNameID: demo\nImageMeta:\n  Image: alpine:latest\n")
	if err := run([]string{"validate", bad}); err == nil {
		t.Fatal("validate of a non-digest-pinned image should fail")
	}

	if err := run([]string{"validate"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("validate with no arg should return usage, got: %v", err)
	}
}

// One store holds both app types, so zcr must refuse a VM app by name rather than trying
// to run a guest as a container: the image build, the pod and the nftables lock-down all
// mean nothing for a VM, and half-applying them is exactly the mis-enforcement the
// network model refuses elsewhere.
func TestVMAppRefusedByRunner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)

	vm := writeApp(t, "SchemaVersion: 3\nType: ZincVirtualization\nAppNameID: guest\n"+
		"ImageMeta:\n  Image: /var/lib/zinc/images/fedora.qcow2\n"+
		"VirtualizationMeta:\n  BaseDigest: sha256:"+strings.Repeat("a", 64)+
		"\n  MemoryMiB: 4096\n  VCPUs: 2\n  Display: None\n")

	for _, command := range []string{"validate", "run", "stop", "inspect"} {
		err := run([]string{command, vm})
		if err == nil || !strings.Contains(err.Error(), "zvr") {
			t.Errorf("zcr %s on a VM app: want an error pointing at zvr, got: %v", command, err)
		}
	}
}

// run without --exec is a dry run: it validates and prints the podman plan, touching no
// runtime, so it succeeds for a valid app under test.
func TestRunDryRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)

	good := writeApp(t, "SchemaVersion: 3\nType: ZincContainer\nAppNameID: demo\nImageMeta:\n  Image: docker.io/library/alpine"+digestPin+"\n")
	if err := run([]string{"run", good}); err != nil {
		t.Fatalf("dry-run of a good app should succeed, got: %v", err)
	}
}

// captureStdout redirects os.Stdout through a pipe for the duration of fn and returns
// everything written, so a test can assert a runtime -v mount reaches the printed plan.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	fn()
	writer.Close()
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRunRuntimeVolumeInPlan is the end-to-end check that a runtime -v/--volume mount
// flows through a dry run into the printed podman plan, with the same ro,noexec / rw
// mapping as a configured Volume - proving the appended-before-validation design wires
// through the existing arg-builder.
func TestRunRuntimeVolumeInPlan(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	appPath := writeApp(t, "SchemaVersion: 3\nType: ZincContainer\nAppNameID: demo\nImageMeta:\n  Image: docker.io/library/alpine"+digestPin+"\n")
	var runErr error
	out := captureStdout(t, func() {
		runErr = run([]string{"run", appPath, "-v", "/host/dl:/downloads:rw", "--volume", "/etc/hosts:/etc/hosts"})
	})
	if runErr != nil {
		t.Fatalf("dry-run with runtime volumes should succeed, got: %v", runErr)
	}
	if !strings.Contains(out, "-v /host/dl:/downloads:rw,noexec") {
		t.Fatalf("writable runtime volume missing from plan:\n%s", out)
	}
	if !strings.Contains(out, "-v /etc/hosts:/etc/hosts:ro,noexec") {
		t.Fatalf("default read-only runtime volume missing from plan:\n%s", out)
	}
}

// A runtime volume whose spec would shift podman's -v fields (a ':' or whitespace in the
// host path) must be rejected, not silently mounted: it is appended to the config and the
// existing validation screens it before any arg is built.
func TestRunRuntimeVolumeRejectedByValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quiet(t)

	appPath := writeApp(t, "SchemaVersion: 3\nType: ZincContainer\nAppNameID: demo\nImageMeta:\n  Image: docker.io/library/alpine"+digestPin+"\n")
	// A trailing ':' segment reads as an empty CONTAINER path (four fields would be
	// rejected at parse time); an unsafe char inside a field is caught at validation.
	if err := run([]string{"run", appPath, "-v", "/ho st:/inner"}); err == nil {
		t.Fatal("a host path with whitespace should be rejected by validation")
	}
}

func TestParseVolumeSpec(t *testing.T) {
	valid := []struct {
		spec       string
		host       string
		inner      string
		writable   bool
		executable bool
	}{
		{"/h:/c", "/h", "/c", false, false},
		{"/h:/c:ro", "/h", "/c", false, false},
		{"/h:/c:rw", "/h", "/c", true, false},
		{"/h:/c:exec", "/h", "/c", false, true},
		{"/h:/c:noexec", "/h", "/c", false, false},
		{"/h:/c:rw,exec", "/h", "/c", true, true},
		{"/h:/c:rw,noexec", "/h", "/c", true, false},
	}
	for _, testCase := range valid {
		vol, err := parseVolumeSpec(testCase.spec)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", testCase.spec, err)
		}
		if !vol.HostMounted || vol.HostMount != testCase.host || vol.InnerMount != testCase.inner ||
			vol.Writable != testCase.writable || vol.Executable != testCase.executable {
			t.Fatalf("%q: got %+v", testCase.spec, vol)
		}
	}

	bad := []string{
		"/onlyhost",      // missing :CONTAINER
		":/c",            // empty HOST
		"/h:",            // empty CONTAINER
		"/h:/c:rw:extra", // too many fields
		"/h:/c:bogus",    // unknown option
		"",               // empty spec
	}
	for _, spec := range bad {
		if _, err := parseVolumeSpec(spec); err == nil {
			t.Fatalf("%q: expected an error, got nil", spec)
		}
	}
}

func TestParseRunArgs(t *testing.T) {
	// App name only: no flags, no volumes.
	name, execute, vols, err := parseRunArgs([]string{"firefox"})
	if err != nil || name != "firefox" || execute || len(vols) != 0 {
		t.Fatalf("plain: name=%q exec=%v vols=%v err=%v", name, execute, vols, err)
	}

	// --exec plus repeated volumes, flags interleaved after the name.
	name, execute, vols, err = parseRunArgs([]string{"firefox", "--exec", "-v", "/a:/a", "--volume", "/b:/b:rw"})
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if name != "firefox" || !execute || len(vols) != 2 {
		t.Fatalf("mixed: name=%q exec=%v vols=%v", name, execute, vols)
	}
	if vols[0].HostMount != "/a" || vols[1].HostMount != "/b" || !vols[1].Writable {
		t.Fatalf("mixed vols: %+v", vols)
	}

	// Attached form, flag before the name.
	name, _, vols, err = parseRunArgs([]string{"-v=/c:/c", "firefox"})
	if err != nil || name != "firefox" || len(vols) != 1 || vols[0].HostMount != "/c" {
		t.Fatalf("attached: name=%q vols=%+v err=%v", name, vols, err)
	}

	// --volume= attached form.
	if _, _, vols, err = parseRunArgs([]string{"firefox", "--volume=/d:/d"}); err != nil || len(vols) != 1 || vols[0].HostMount != "/d" {
		t.Fatalf("--volume=: vols=%+v err=%v", vols, err)
	}

	if _, _, _, err := parseRunArgs([]string{"firefox", "-v"}); err == nil {
		t.Fatal("trailing -v with no value should error")
	}
	if _, _, _, err := parseRunArgs([]string{"firefox", "--nope"}); err == nil {
		t.Fatal("unknown flag should error")
	}
	if _, _, _, err := parseRunArgs([]string{"-v", "/a:/a"}); err == nil {
		t.Fatal("missing app name should error")
	}
	if _, _, _, err := parseRunArgs([]string{"firefox", "bar"}); err == nil {
		t.Fatal("a second positional argument should error")
	}
	if _, _, _, err := parseRunArgs([]string{"firefox", "-v", "/onlyhost"}); err == nil {
		t.Fatal("a malformed volume spec should bubble up as an error")
	}
}
