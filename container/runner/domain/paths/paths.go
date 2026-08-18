// Package paths decides what an instance of an app is called and where it keeps its state, in
// exactly one place. One definition can run more than once, so the address "app@instance" is what a
// person types and the runtime name and state directory are derived from it here - which is why
// `zcr where` exists rather than a documented constant a desktop would copy and drift from.
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

// Separator joins app and instance into a runtime object name. Not "@", which podman rejects: names
// must match [a-zA-Z0-9][a-zA-Z0-9_.-]*. So "@" is the human form and "." the runtime form.
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

// ParseRuntime is Runtime() run backwards. It cannot be done on the string alone: an app name may
// contain dots and an instance may not, so "notes.work" reads either as one app or as "notes"
// running as instance "work". defined answers which; it is a function because the authority is the
// store, which this package must not depend on.
//
// The fallback is the whole string as an app name, so a raw container or a deleted app comes back
// as itself. Undecidable when both readings are defined apps at once: the whole-name reading wins,
// and those two apps already collide on their podman container name anyway.
//
// Two callers need it: attribution, and the Wayland context, whose app_id must be the same for
// every instance while instance_id must differ (section 5.2).
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

// StateDir is where this instance's files live, under $XDG_STATE_HOME (falling back to
// ~/.local/state). Per instance, not per app: the reason to have two is that they do not share what
// they accumulate.
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

// Template placeholders a mount path may use, so one definition can serve many instances. {state}
// is why this lives next to StateDir: it expands to the same directory `zcr where` reports, so a
// mount and that answer cannot disagree.
const (
	PlaceholderApp      = "{app}"
	PlaceholderInstance = "{instance}"
	PlaceholderState    = "{state}"
)

// Expand substitutes the placeholders in one path. An un-instanced app expands {instance} to empty,
// collapsing "…/{instance}" to a trailing separator rather than leaving literal text that would be
// created on disk under that name. An unexpanded placeholder is an error, not a silent pass: a
// mount meant to be per-instance that quietly is not shares a directory between two instances.
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

// BundleDir is where an app's authored files live, resolved from the app's name rather than from
// anything a config states. Per app, not per instance: a config file is content the app was
// authored WITH. Beside the app definition, so `apps/notes.yaml` and `apps/notes/` travel together.
func BundleDir(configHome, app string) string {
	if configHome == "" {
		return ""
	}
	return filepath.Join(configHome, "zinc", "apps", app, "configs")
}
