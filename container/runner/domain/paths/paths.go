// Package paths decides two things a container app needs agreed in exactly one place: what
// an instance of an app is called, and where that instance keeps its state.
//
// Both exist because one app definition can be run more than once - a browser for work and
// one for personal are the same config and must not be the same running thing. The address
// "app@instance" is what a person types; the runtime name and the state directory are
// derived from it, here, so `zcr run` creates what `zcr stop` removes and `zcr where`
// reports.
//
// It is pure string work over the environment, so the layout is unit-testable and no caller
// has to reimplement it. That last part is the point: a desktop that hardcodes the layout
// becomes a second source of truth the moment either side changes, which is why `zcr where`
// exists rather than a documented constant.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// instanceRE is the charset for an instance name. Narrower than an app name on purpose: no
// dots, because the runtime name joins app and instance with one, and no uppercase, so the
// address a person types is the address they get back.
var instanceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Separator joins app and instance into a runtime object name. Not "@", which is what the
// address uses, because podman rejects it:
//
//	names must match [a-zA-Z0-9][a-zA-Z0-9_.-]*
//
// So "@" is the human form and "." is the runtime form, and this package is the only place
// that knows both.
const Separator = "."

// Address identifies one running thing: an app definition, and optionally which instance of
// it. A zero Instance means the un-instanced app, which is what every config authored before
// instances existed resolves to - so those keep their current runtime name and nothing
// already running is renamed out from under itself.
type Address struct {
	App      string
	Instance string
}

// ParseAddress reads "app" or "app@instance". The app half is returned unvalidated, because
// what makes an app name legal belongs to the schema validator and duplicating it here would
// give two answers that can disagree; the instance half is validated, because nothing else
// will.
func ParseAddress(spec string) (Address, error) {
	app, instance, found := strings.Cut(strings.TrimSpace(spec), "@")
	if !found {
		return Address{App: app}, nil
	}
	switch {
	case app == "":
		return Address{}, fmt.Errorf("%q: an instance needs an app before the @", spec)
	case instance == "":
		return Address{}, fmt.Errorf("%q: the @ is there but no instance follows it; drop the @ to address the app itself", spec)
	case !instanceRE.MatchString(instance):
		return Address{}, fmt.Errorf("%q: instance %q must be lowercase letters, digits, '_' or '-', starting with a letter or digit", spec, instance)
	}
	return Address{App: app, Instance: instance}, nil
}

// String renders the address back in the form a person types.
func (addr Address) String() string {
	if addr.Instance == "" {
		return addr.App
	}
	return addr.App + "@" + addr.Instance
}

// Runtime is the name podman objects take: the container, its pod, and anything named after
// it. An un-instanced app keeps the bare app name it has always had.
func (addr Address) Runtime() string {
	if addr.Instance == "" {
		return addr.App
	}
	return addr.App + Separator + addr.Instance
}

// ParseRuntime recovers the address a runtime name was built from: Runtime() run backwards.
//
// It cannot be done on the string alone. An app name may contain dots and an instance may
// not, so "notes.work" reads either as the app "notes.work" or as "notes" running as
// instance "work" - two readings of one string, and only the set of defined apps can say
// which was meant. defined answers that. It is a function rather than a list because the
// authority is the store, which this package must not depend on.
//
// The fallback is the whole string as an app name with no instance, which is the reading
// that existed before instances did, so a runtime name from anywhere else (a raw container,
// an app since deleted) comes back as itself rather than as an invented instance.
//
// The one case it cannot decide: an app literally named "notes.work" AND an app "notes" run
// as instance "work", both defined at once. The whole-name reading wins there, because that
// name is definitely an app; nothing in the runtime name distinguishes the two, so this is
// stated rather than papered over. Those two apps already collide on their podman container
// name, so that ambiguity is a symptom of a naming conflict Zinc cannot support rather than
// a decision made here.
//
// Two callers need this. Attribution maps a running proxy back to the app it serves, and the
// Wayland security context needs the halves apart after they were folded into AppNameID:
// app_id must be the SAME string for every instance of an app, and instance_id must differ
// between them (section 5.2).
func ParseRuntime(name string, defined func(string) bool) Address {
	name = strings.TrimSpace(name)
	if defined == nil || defined(name) {
		return Address{App: name}
	}
	// The instance is what follows the LAST separator, because an instance may not contain
	// one and an app name may. Checked against instanceRE as well, so a dotted app name that
	// is simply not in the store cannot come back with its own last segment as an instance.
	if idx := strings.LastIndex(name, Separator); idx > 0 {
		app, instance := name[:idx], name[idx+1:]
		if instanceRE.MatchString(instance) && defined(app) {
			return Address{App: app, Instance: instance}
		}
	}
	return Address{App: name}
}

// StateDir is where this instance's own files live, under $XDG_STATE_HOME (falling back to
// ~/.local/state, which is what the XDG spec says that variable defaults to). State rather
// than data because it is what the app accumulates by running - reproducible only in the
// sense that deleting it resets the instance.
//
// Per instance, not per app: the whole reason to have two instances is that they do not
// share what they accumulate.
func StateDir(addr Address) (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateHome, "zinc", addr.App)
	if addr.Instance != "" {
		dir = filepath.Join(dir, addr.Instance)
	}
	return dir, nil
}

// Template placeholders a mount path may use. They exist so one app definition can serve many
// instances: ZDE's desk manifests carry a mounts field per desk, and without templating every
// desk would need its own copy of the app just to point at its own directory.
//
// {state} is the one that matters, and it is why this lives next to StateDir rather than in a
// string-utility package: it expands to the same directory `zcr where` reports, so a mount and
// the answer to "where does this instance keep things" cannot disagree.
const (
	PlaceholderApp      = "{app}"
	PlaceholderInstance = "{instance}"
	PlaceholderState    = "{state}"
)

// Expand substitutes the placeholders in one path. An un-instanced app expands {instance} to
// the empty string, which collapses "…/{instance}" to a trailing separator rather than to the
// literal text - a path with "{instance}" left in it would be created on disk under that name
// and look like a Zinc bug from the outside.
//
// It reports an error rather than silently leaving a placeholder unexpanded, because a mount
// that was meant to be per-instance and quietly is not would share one directory between two
// instances that exist precisely so they do not share.
func (addr Address) Expand(path string) (string, error) {
	if !strings.Contains(path, "{") {
		return path, nil
	}
	stateDir, err := StateDir(addr)
	if err != nil {
		return "", err
	}
	expanded := strings.NewReplacer(
		PlaceholderState, stateDir,
		PlaceholderApp, addr.App,
		PlaceholderInstance, addr.Instance,
	).Replace(path)
	expanded = filepath.Clean(expanded)
	if strings.Contains(expanded, "{") {
		return "", fmt.Errorf("%q: unknown placeholder; the ones that exist are %s, %s and %s",
			path, PlaceholderState, PlaceholderApp, PlaceholderInstance)
	}
	return expanded, nil
}

// BundleDir is where an app's authored files live: the configs it ships with, resolved from
// the app's name rather than from anything a config states.
//
// Per app, not per instance. A config file is content the app was authored WITH, so every
// instance of an app reads the same one; per-instance content is runtime state and belongs
// under StateDir.
//
// This is deliberately beside the app definition, so `apps/notes.yaml` and its bundle at
// `apps/notes/` travel together and a reviewer finds one by looking at the other. The path
// was already named in validation messages before anything resolved it; this is that path.
func BundleDir(configHome, app string) string {
	if configHome == "" {
		return ""
	}
	return filepath.Join(configHome, "zinc", "apps", app, "configs")
}
