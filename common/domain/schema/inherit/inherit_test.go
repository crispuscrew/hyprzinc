package inherit

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// resolveFrom runs Resolve over an in-memory set of app files and decodes the result, which
// is what every store does with its own files.
func resolveFrom(t *testing.T, app string, files map[string]string) schema.AppConfig {
	t.Helper()
	data, err := Resolve([]byte(files[app]), func(name string) ([]byte, error) {
		text, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("no app %q defined", name)
		}
		return []byte(text), nil
	})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", app, err)
	}
	var cfg schema.AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode resolved %s: %v", app, err)
	}
	return cfg
}

func resolveErr(t *testing.T, app string, files map[string]string) error {
	t.Helper()
	_, err := Resolve([]byte(files[app]), func(name string) ([]byte, error) {
		text, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("no app %q defined", name)
		}
		return []byte(text), nil
	})
	if err == nil {
		t.Fatalf("Resolve(%s): want an error, got none", app)
	}
	return err
}

// The base case: a child takes what it does not state and overrides what it does.
func TestResolve_ChildOverridesBase(t *testing.T) {
	cfg := resolveFrom(t, "child", map[string]string{
		"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
ResourcesMeta:
  MaxRamMiB: 256
  PIDsLimit: 64
`,
		"child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
ResourcesMeta:
  MaxRamMiB: 1024
`,
	})
	if cfg.AppNameID != "child" {
		t.Errorf("AppNameID = %q, want child", cfg.AppNameID)
	}
	if cfg.ImageMeta.Image != "localhost/base:local" {
		t.Errorf("Image = %q, want the base's", cfg.ImageMeta.Image)
	}
	if cfg.ResourcesMeta.MaxRamMiB != 1024 {
		t.Errorf("MaxRamMiB = %d, want the child's 1024", cfg.ResourcesMeta.MaxRamMiB)
	}
	// The sharpest property of merging at the node level: a nested block merges field by
	// field, so restating one field does not discard the block's other fields.
	if cfg.ResourcesMeta.PIDsLimit != 64 {
		t.Errorf("PIDsLimit = %d, want the base's 64 - a nested block must merge, not replace", cfg.ResourcesMeta.PIDsLimit)
	}
}

// The reason the merge is done on YAML rather than on a decoded struct. A decoded false is
// indistinguishable from an absent field, so struct-level merging could only ever read it as
// "inherit" - and a child could never turn a base's flag off. On a sandboxing tool that
// means a base could loosen containment irreversibly.
func TestResolve_ChildCanTurnAFlagOff(t *testing.T) {
	cfg := resolveFrom(t, "child", map[string]string{
		"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
HostTheme: true
DisplayMeta:
  DisableSecurityContext: true
`,
		"child": `
AppNameID: child
Inherits: base
HostTheme: false
DisplayMeta:
  DisableSecurityContext: false
`,
	})
	if cfg.HostTheme {
		t.Error("a child stating HostTheme: false must win over the base's true")
	}
	if cfg.DisplayMeta.DisableSecurityContext {
		t.Error("a child must be able to turn a base's DisableSecurityContext back off")
	}
}

// The same argument for lists: a stated empty list is a statement, and a child that cannot
// empty an inherited Capabilities list cannot walk back what a base granted.
func TestResolve_ChildCanEmptyAnInheritedList(t *testing.T) {
	cfg := resolveFrom(t, "child", map[string]string{
		"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
Capabilities:
  - NET_RAW
  - SYS_PTRACE
`,
		"child": `
AppNameID: child
Inherits: base
Capabilities: []
`,
	})
	if len(cfg.Capabilities) != 0 {
		t.Errorf("Capabilities = %v, want the child's empty list to win", cfg.Capabilities)
	}
}

// A stated list replaces rather than appends. Appending would mean a child could never
// remove an inherited volume or capability, and capabilities that only accumulate down a
// chain are the wrong direction for this tool.
func TestResolve_StatedListReplaces(t *testing.T) {
	cfg := resolveFrom(t, "child", map[string]string{
		"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
Capabilities: [NET_RAW]
NetworkMeta:
  NetworkLists:
    - Ingress: true
      Ports: [5432]
`,
		"child": `
AppNameID: child
Inherits: base
Capabilities: [SYS_PTRACE]
`,
	})
	if want := []string{"SYS_PTRACE"}; !slices.Equal(cfg.Capabilities, want) {
		t.Errorf("Capabilities = %v, want %v (replace, not append)", cfg.Capabilities, want)
	}
	// And what the child did not state still comes from the base.
	if len(cfg.NetworkMeta.NetworkLists) != 1 || cfg.NetworkMeta.NetworkLists[0].Ports[0] != 5432 {
		t.Errorf("NetworkLists = %+v, want the base's", cfg.NetworkMeta.NetworkLists)
	}
}

// A chain resolves ancestor-first, so the app being read has the last word at every level.
func TestResolve_Chain(t *testing.T) {
	cfg := resolveFrom(t, "leaf", map[string]string{
		"root": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: root
ImageMeta:
  Image: localhost/root:local
ResourcesMeta:
  MaxRamMiB: 128
  PIDsLimit: 16
`,
		"middle": `
AppNameID: middle
Inherits: root
ResourcesMeta:
  MaxRamMiB: 512
`,
		"leaf": `
AppNameID: leaf
Inherits: middle
Icon: firefox
`,
	})
	if cfg.ImageMeta.Image != "localhost/root:local" {
		t.Errorf("Image = %q, want the root's", cfg.ImageMeta.Image)
	}
	if cfg.ResourcesMeta.MaxRamMiB != 512 {
		t.Errorf("MaxRamMiB = %d, want the middle's 512", cfg.ResourcesMeta.MaxRamMiB)
	}
	if cfg.ResourcesMeta.PIDsLimit != 16 {
		t.Errorf("PIDsLimit = %d, want the root's 16", cfg.ResourcesMeta.PIDsLimit)
	}
	if cfg.Icon != "firefox" {
		t.Errorf("Icon = %q, want the leaf's", cfg.Icon)
	}
}

// A config that cannot be fully resolved must not be run: what it is missing could be the
// part that contains it.
func TestResolve_FailsClosed(t *testing.T) {
	cycle := resolveErr(t, "a", map[string]string{
		"a": "AppNameID: a\nInherits: b\n",
		"b": "AppNameID: b\nInherits: a\n",
	})
	if !strings.Contains(cycle.Error(), "cycle") {
		t.Errorf("want a cycle error, got %v", cycle)
	}

	missing := resolveErr(t, "child", map[string]string{
		"child": "AppNameID: child\nInherits: ghost\n",
	})
	if !strings.Contains(missing.Error(), "ghost") {
		t.Errorf("want the missing base named, got %v", missing)
	}

	// The name is joined into a path by the store, so a traversal value must be refused
	// before any file is opened.
	traversal := resolveErr(t, "child", map[string]string{
		"child": "AppNameID: child\nInherits: ../../etc/evil\n",
	})
	if !strings.Contains(traversal.Error(), "Inherits") {
		t.Errorf("want an Inherits charset error, got %v", traversal)
	}
}

// A chain long enough to be pathological is cut off rather than followed.
func TestResolve_DepthBounded(t *testing.T) {
	files := map[string]string{}
	for index := 0; index <= maxDepth+2; index++ {
		files[fmt.Sprintf("a%d", index)] = fmt.Sprintf("AppNameID: a%d\nInherits: a%d\n", index, index+1)
	}
	err := resolveErr(t, "a0", files)
	if !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("want a depth error, got %v", err)
	}
}

// An app that inherits from nothing must come back byte-identical: the overwhelmingly common
// case must not be re-encoded, reordered, or otherwise touched on its way through.
func TestResolve_NoInheritanceIsUntouched(t *testing.T) {
	const text = "SchemaVersion: 3\nAppNameID: solo\nImageMeta:\n  Image: localhost/solo:local\n"
	out, err := Resolve([]byte(text), func(string) ([]byte, error) {
		t.Fatal("a config with no Inherits must not load a base")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != text {
		t.Errorf("resolved = %q, want it unchanged", out)
	}
}

// Merge is defined on mappings; a file that is a list or a bare scalar is an error rather
// than something quietly accepted.
func TestMerge_RejectsNonMappings(t *testing.T) {
	if _, err := Merge([]byte("- a\n- b\n"), []byte("AppNameID: x\n")); err == nil {
		t.Error("a list base must be refused")
	}
	if _, err := Merge([]byte("AppNameID: x\n"), []byte("just a string\n")); err == nil {
		t.Error("a scalar child must be refused")
	}
	// An empty file on either side is not an error - it simply contributes nothing.
	if _, err := Merge(nil, []byte("AppNameID: x\n")); err != nil {
		t.Errorf("an empty base should merge to the child, got %v", err)
	}
}

func TestParent(t *testing.T) {
	for _, testCase := range []struct {
		text, want string
		wantErr    bool
	}{
		{text: "AppNameID: a\nInherits: base\n", want: "base"},
		{text: "AppNameID: a\n", want: ""},
		{text: "AppNameID: a\nInherits: \"  \"\n", want: ""},
		{text: "AppNameID: a\nInherits: ../evil\n", wantErr: true},
		{text: "AppNameID: a\nInherits: UPPER\n", wantErr: true},
	} {
		got, err := Parent([]byte(testCase.text))
		if testCase.wantErr {
			if err == nil {
				t.Errorf("Parent(%q) = %q, want an error", testCase.text, got)
			}
			continue
		}
		if err != nil || got != testCase.want {
			t.Errorf("Parent(%q) = %q, %v; want %q", testCase.text, got, err, testCase.want)
		}
	}
}

// Env's keys belong to the author, not the schema, so a child that states it replaces it.
// Merging made it the one field a child could not narrow: a base setting LD_PRELOAD survived
// `Env: {}` while the child's own file read as though it granted nothing.
func TestResolve_StatedEnvReplaces(t *testing.T) {
	base := `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
Env:
  LANG: en_US.UTF-8
  LD_PRELOAD: /opt/hook.so
`
	cfg := resolveFrom(t, "child", map[string]string{"base": base, "child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
Env:
  LANG: C
`})
	if cfg.Env["LANG"] != "C" {
		t.Errorf("LANG = %q, want the child's", cfg.Env["LANG"])
	}
	if _, ok := cfg.Env["LD_PRELOAD"]; ok {
		t.Errorf("a stated Env must replace the base's, got %v", cfg.Env)
	}

	cleared := resolveFrom(t, "child", map[string]string{"base": base, "child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
Env: {}
`})
	if len(cleared.Env) != 0 {
		t.Errorf("Env: {} should clear the base's, got %v", cleared.Env)
	}

	silent := resolveFrom(t, "child", map[string]string{"base": base, "child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
`})
	if silent.Env["LD_PRELOAD"] != "/opt/hook.so" {
		t.Errorf("a silent child should inherit Env, got %v", silent.Env)
	}
}

// A scalar and a list are different shapes for one field, and the child must win either way.
func TestResolve_AudioDeviceSwapsShapeInBothDirections(t *testing.T) {
	toList := resolveFrom(t, "child", map[string]string{"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
AudioMeta:
  Microphone: default
`, "child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
AudioMeta:
  Microphone: [/dev/snd/pcmC0D0c]
`})
	if toList.AudioMeta.Microphone.Default || len(toList.AudioMeta.Microphone.Devices) != 1 {
		t.Errorf("a child's device list must replace the base's default, got %+v", toList.AudioMeta.Microphone)
	}

	toNone := resolveFrom(t, "child", map[string]string{"base": `
SchemaVersion: 3
Type: ZincContainer
AppNameID: base
ImageMeta:
  Image: localhost/base:local
AudioMeta:
  Microphone: [/dev/snd/pcmC0D0c]
`, "child": `
SchemaVersion: 3
AppNameID: child
Inherits: base
AudioMeta:
  Microphone: none
`})
	if !toNone.AudioMeta.Microphone.IsZero() {
		t.Errorf("a child stating none must clear the base's list, got %+v", toNone.AudioMeta.Microphone)
	}
}
