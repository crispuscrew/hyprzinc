// Package store reads app definitions from ~/.config/zinc/apps - the same files zc writes and zcr
// runs. Read-only: the launcher lists apps and hands the chosen one to zcr. Same schema and the same
// KnownFields codec as the other tools.
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
)

// keyRE is the app-name charset the schema enforces (lowercase [a-z0-9._-], starting
// alphanumeric). A file zc wrote always matches it. List skips anything that does not,
// so a hand-dropped or shared file with a flag-like name (e.g. "--net=host.yaml") never
// becomes a picker entry that could be launched as a zcr flag rather than an app.
var keyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Load reads and decodes an app YAML from disk. It does NOT apply semantic rules (zcr
// validates at launch time). Unknown keys are reported as an error so a stale/typo
// field surfaces rather than being silently ignored.
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

// safeName rejects a name carrying a path separator or a "." / ".." segment, so a name from the CLI
// cannot read outside the apps directory. A segment comparison rather than a substring test, which
// would also reject an ordinary name like "my..app" and leave it listed but unloadable.
func safeName(name string) error {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return fmt.Errorf("store: invalid app name %q", name)
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

// Load decodes the named app. The name must be a plain store key (safeName), so it
// cannot read a file outside the apps directory.
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
	// An app must not be able to resolve into another app's identity: a child omitting AppNameID inherits
	// its base's, and that is what names the container, the pod and the derived image.
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
