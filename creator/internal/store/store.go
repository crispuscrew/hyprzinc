// Package store persists app definitions as <name>.yaml files under the user's config
// directory (~/.config/zinc/apps) and provides the YAML decode/encode used by the
// $EDITOR round-trip.
//
// It is the creator's own copy of the on-disk format - deliberately independent of the
// runner so zc never imports zcr (zc authors app files; zcr runs them). Both sides
// read/write the exact same layout: the shared schema (common) plus this identical
// atomic-write + KnownFields codec, so a file zc writes is one zcr loads verbatim.
//
// Save validates (validate.Validate) before writing, so invalid config never lands on
// disk, and writes are atomic (temp file + rename) so a crash can't leave a
// half-written definition. Load only decodes - zcr runs validate.Validate again at
// launch time, which catches drift from hand edits (docs/architecture.md section 3).
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/inherit"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
)

// Load reads and decodes an app YAML from disk. It does NOT apply semantic rules -
// call validate.Validate on the result. Unknown keys (typos, stale fields after a hand
// edit) are reported as an error so dead config can't silently accumulate.
func Load(path string) (schema.AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return schema.AppConfig{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	return decode(data, path)
}

// decode turns app YAML into a config. Unknown keys (typos, stale fields after a hand edit)
// are reported as an error so dead config can't silently accumulate. origin names the file
// for the error message; a merged config still names the app it was read for.
func decode(data []byte, origin string) (schema.AppConfig, error) {
	var cfg schema.AppConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return schema.AppConfig{}, fmt.Errorf("config: %s: empty file", origin)
		}
		return schema.AppConfig{}, fmt.Errorf("config: decode %s: %w", origin, err)
	}
	return cfg, nil
}

// Marshal encodes an app config back to YAML - used to hand a draft to $EDITOR (the
// "advanced" form action) and round-trip it back through Load.
func Marshal(cfg schema.AppConfig) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return data, nil
}

// Store is a directory of app definitions.
type Store struct{ Root string }

// Default resolves the standard apps directory: $XDG_CONFIG_HOME/zinc/apps, falling
// back to ~/.config/zinc/apps.
func Default() (*Store, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("store: locate home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return &Store{Root: filepath.Join(base, "zinc", "apps")}, nil
}

// Path is the on-disk location of the named app's definition.
func (sto *Store) Path(name string) string {
	return filepath.Join(sto.Root, name+".yaml")
}

// keyRE is the app-name charset the schema enforces (lowercase [a-z0-9._-], starting
// alphanumeric). A file zc wrote always matches it. List skips anything that does not, so a
// hand-dropped or shared file with a flag-like name ("--net=host.yaml") or a path-like one
// ("notes.yaml.yaml", listed as "notes.yaml", which zcr re-reads as ./notes.yaml) never
// becomes a row that can be run. The launcher has had this guard since it shipped; the
// authoring tool drives the same runner and needs the same one.
var keyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// safeName rejects a name that is not a plain store key - one with a path separator
// or a ".." segment - so a crafted name (a CLI delete argument, an unvalidated
// dependency) cannot escape the apps directory when joined into Path.
//
// The ".." test compares SEGMENTS rather than searching the string: "my..app" is a legal
// schema name and contains "..", so a substring test refuses a name the validator accepts,
// leaving an app that zc can create and then never delete, edit or validate again.
func safeName(name string) error {
	if name == "" || name != filepath.Base(name) {
		return fmt.Errorf("store: invalid app name %q", name)
	}
	for _, segment := range strings.Split(name, string(filepath.Separator)) {
		if segment == ".." || segment == "." {
			return fmt.Errorf("store: invalid app name %q", name)
		}
	}
	return nil
}

// List returns the names of all defined apps, sorted. A missing store directory is
// treated as empty, not an error.
func (sto *Store) List() ([]string, error) {
	entries, err := os.ReadDir(sto.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", sto.Root, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(entry.Name(), ".yaml"); ok && keyRE.MatchString(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Exists reports whether an app with the given name is defined. An unsafe name is
// treated as not-defined rather than stat'd through a traversal path.
func (sto *Store) Exists(name string) bool {
	if safeName(name) != nil {
		return false
	}
	_, err := os.Stat(sto.Path(name))
	return err == nil
}

// Load decodes the named app. It does NOT validate - validate.Validate runs before
// launching (zcr) and before saving (below), which is what catches drift from hand
// edits (section 3). The name must be a plain store key (safeName), so it cannot read
// a file outside the apps directory.
func (sto *Store) Load(name string) (schema.AppConfig, error) {
	if err := safeName(name); err != nil {
		return schema.AppConfig{}, err
	}
	return Load(sto.Path(name))
}

// readRaw returns the named app's file as written, before any decoding. Inheritance is
// resolved on the YAML rather than on decoded structs - only the bytes record which keys the
// app actually STATED, and a decoded false is indistinguishable from an absent field.
func (sto *Store) readRaw(name string) ([]byte, error) {
	if err := safeName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(sto.Path(name))
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", name, err)
	}
	return data, nil
}

// LoadResolved decodes the named app with its Inherits chain applied: what the app does not
// state is taken from the base it starts from. This is what a launch reads, because it is
// what the app actually is - Load returns the file as written, which is what an editor needs
// and what must be written back.
func (sto *Store) LoadResolved(name string) (schema.AppConfig, error) {
	data, err := sto.readRaw(name)
	if err != nil {
		return schema.AppConfig{}, err
	}
	merged, err := inherit.Resolve(data, sto.readRaw)
	if err != nil {
		return schema.AppConfig{}, fmt.Errorf("config: %s: %w", name, err)
	}
	resolved, derr := decode(merged, sto.Path(name))
	if derr != nil {
		return schema.AppConfig{}, derr
	}
	// An app must not be able to resolve into another app's identity. A child that omits
	// AppNameID inherits its base's, and AppNameID is what the runner names the container,
	// the pod and the derived image after - so `zcr run notes` would build, and `zcr stop
	// notes` would destroy, whatever `browser` is. Inheriting apps are hand-written (Save
	// refuses to rewrite one), so nothing else keeps the filename and the name in step.
	if resolved.AppNameID != name {
		return schema.AppConfig{}, fmt.Errorf("config: %s: resolves to AppNameID %q - an app must keep its own name; state AppNameID in the app rather than taking the base's", name, resolved.AppNameID)
	}
	return resolved, nil
}

// LoadFileResolved decodes an app YAML at an arbitrary path with its Inherits chain applied.
// The base is still looked up in the store: a config given by path is being read as an app,
// and where its base lives does not change because of how the app itself was named.
func (sto *Store) LoadFileResolved(path string) (schema.AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return schema.AppConfig{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	merged, err := inherit.Resolve(data, sto.readRaw)
	if err != nil {
		return schema.AppConfig{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return decode(merged, path)
}

// LoadFile decodes an arbitrary .yaml path (a CLI path argument, or the editor
// round-trip temp file) - same codec as Load, no store lookup.
func (sto *Store) LoadFile(path string) (schema.AppConfig, error) {
	return Load(path)
}

// Marshal encodes a draft to YAML for the $EDITOR round-trip (see the package Marshal
// function).
func (sto *Store) Marshal(cfg schema.AppConfig) ([]byte, error) {
	return Marshal(cfg)
}

// Save validates cfg and atomically writes it to <cfg.AppNameID>.yaml. Invalid config
// is rejected before anything touches disk.
//
// An app that inherits is refused, and that is a data-loss guard rather than a limitation
// of the format. Inheritance is recorded in which keys a file STATES, and a decoded
// AppConfig no longer knows: every field it did not state has become an ordinary zero value,
// indistinguishable from one stated as zero. Writing that struct back would state all of
// them, so the child would stop inheriting anything and would instead override its base with
// zeros - silently, and looking entirely normal on disk. Refusing costs an inheriting app
// the struct-based editors; writing would cost it its meaning.
func (sto *Store) Save(cfg schema.AppConfig) error {
	if base := strings.TrimSpace(cfg.Inherits); base != "" {
		return fmt.Errorf("store: %s inherits from %q, so it is edited as a file rather than rewritten from a form: %s\n"+
			"       (a form knows the app's values but not which of them it stated, and writing them all back would replace what it inherits with zeros)",
			cfg.AppNameID, base, sto.Path(cfg.AppNameID))
	}
	if err := validate.Validate(cfg); err != nil {
		return fmt.Errorf("store: refusing to save invalid config: %w", err)
	}
	if err := os.MkdirAll(sto.Root, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", sto.Root, err)
	}

	data, err := Marshal(cfg)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", cfg.AppNameID, err)
	}

	tmp, err := os.CreateTemp(sto.Root, cfg.AppNameID+".*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write %s: %w", cfg.AppNameID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: write %s: %w", cfg.AppNameID, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("store: chmod %s: %w", cfg.AppNameID, err)
	}
	if err := os.Rename(tmpName, sto.Path(cfg.AppNameID)); err != nil {
		return fmt.Errorf("store: install %s: %w", cfg.AppNameID, err)
	}
	return nil
}

// Delete removes the named app definition. A missing definition is not an error. The
// name must be a plain store key (safeName), so it cannot remove a file outside the
// apps directory.
func (sto *Store) Delete(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	err := os.Remove(sto.Path(name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: delete %s: %w", name, err)
	}
	return nil
}
