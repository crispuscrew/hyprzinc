// Package app is the runner's application layer - the hexagon's "inside". A Service
// orchestrates a launch by composing the ports (Store, Runtime, ImageBuilder,
// ImageResolver, NetEnforcer) and depends on none of their concrete adapters. The
// front-ends build the adapters, hand them to New, and drive everything through this
// one facade.
//
// This is where the launch sequence lives - validate, build the derived image if
// needed, run the egress lock-down through the NetEnforcer, then start the app - so
// there is exactly one launch path to get right (docs/architecture.md section 9.1, section 13).
package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
	"github.com/crispuscrew/zinc/container/runner/domain/derived"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Service is the application facade. Construct it with New.
type Service struct {
	store    ports.Store
	runtime  ports.Runtime
	builder  ports.ImageBuilder
	resolver ports.ImageResolver
	net      ports.NetEnforcer
	bus      ports.DBusBroker
	display  ports.DisplayBroker
}

// New wires the ports into a Service.
func New(store ports.Store, runtime ports.Runtime, builder ports.ImageBuilder, resolver ports.ImageResolver, net ports.NetEnforcer, bus ports.DBusBroker, display ports.DisplayBroker) Service {
	return Service{store: store, runtime: runtime, builder: builder, resolver: resolver, net: net, bus: bus, display: display}
}

// address recovers the app and instance halves of a runtime name. The instance rides on
// AppNameID from the moment a command resolves an address, which is what makes every runtime
// object per-instance without threading a second identifier through every adapter; the
// Wayland security context is the one consumer that needs the halves back, because app_id
// must be the same for every instance and instance_id must not be (section 5.2).
//
// The store is the authority on which readings of a dotted name are real apps. A Service
// built without one (the plan-only test wiring) gets the whole name as the app, which is the
// same answer a config run from a file path gets and is correct for both.
func (svc Service) address(name string) paths.Address {
	defined := func(string) bool { return false }
	if svc.store != nil {
		defined = svc.store.Exists
	}
	return paths.ParseRuntime(name, defined)
}

// withDisplay establishes the app's Wayland security context and returns the options its
// container should be built from. It runs immediately before the container is created: the
// socket it produces is a bind-mount source, so it must exist first, and nothing later in the
// launch can invalidate it.
//
// opt is taken and returned by value, so a dependency launched from the same launch never
// inherits the socket of the app that depends on it - a per-app result must not travel down a
// recursion as though it were a host fact.
// withBundle resolves this app's bundle directory, which is where its authored Configs live.
//
// It has to happen here rather than in the argv builder because by then AppNameID carries the
// instance: `zcr run notes@work` rewrites it to "notes.work", and a bundle is per APP, since a
// config file is content the app was authored with and every instance reads the same one.
// Deriving it downstream sent every instanced app at apps/notes.work/configs, which nothing
// creates. opt is taken and returned by value, so a dependency gets its own.
func (svc Service) withBundle(cfg schema.AppConfig, opt options.HostOptions) options.HostOptions {
	opt.BundleDir = paths.BundleDir(opt.ConfigHome, svc.address(cfg.AppNameID).App)
	return opt
}

func (svc Service) withDisplay(cfg schema.AppConfig, opt options.HostOptions) (options.HostOptions, error) {
	if svc.display == nil {
		return opt, nil
	}
	socket, err := svc.display.Establish(svc.address(cfg.AppNameID), cfg, opt)
	if err != nil {
		return opt, err
	}
	opt.WaylandSocket = socket
	return opt, nil
}

// attachFlags are the app-container flags that attach it to everything Zinc prepared on its
// behalf: the network attachment from the enforcer, and the filtered bus socket from the
// broker. Composed in one place so Plan, launch and OpenTerminal cannot drift apart on what
// the app is actually attached to.
func (svc Service) attachFlags(cfg schema.AppConfig) []string {
	return append(svc.net.RunFlags(cfg), svc.bus.RunFlags(cfg)...)
}

// prepareSteps are the ordered pre-app steps: the enforcer's (establish and lock the netns)
// followed by the broker's (socket dir, then the proxy). Both fail closed, and neither has
// created anything when it returns an error.
func (svc Service) prepareSteps(cfg schema.AppConfig, opt options.HostOptions) ([]ports.Command, error) {
	steps, err := svc.net.Prepare(cfg, opt)
	if err != nil {
		return nil, err
	}
	busSteps, err := svc.bus.Prepare(cfg)
	if err != nil {
		return nil, err
	}
	return append(steps, busSteps...), nil
}

// teardownSteps undo a launch: the proxy and its socket directory first, then the pod and its
// netns. Bus before net because the proxy is a container of its own that outlives the pod
// otherwise, and removing the pod does not touch it.
func (svc Service) teardownSteps(cfg schema.AppConfig) []ports.Command {
	return append(svc.bus.Teardown(cfg), svc.net.Teardown(cfg)...)
}

// Plan returns the ordered runtime commands a launch would run, without running them
// - the NetEnforcer's pre-steps (establish + lock the netns) followed by the app
// container. Used for dry-run so what will happen is fully visible.
//
// It shows the compositor's own Wayland socket even for an app that will get a security
// context, because creating one is a side effect a dry run must not have and the derived
// socket would not exist for anyone who pasted the printed command. The dry run says so in
// as many words instead (see cmdRun); what is printed stays something that can be run.
func (svc Service) Plan(cfg schema.AppConfig, opt options.HostOptions) ([]ports.Command, error) {
	if err := validate.Validate(cfg); err != nil { // never compose commands from unvalidated config (section 3)
		return nil, fmt.Errorf("%s: %w", cfg.AppNameID, err)
	}
	opt = svc.withBundle(cfg, opt) // the dry run must show the same source the launch mounts
	if err := checkNetwork(cfg); err != nil {
		return nil, err
	}
	appArgs, err := svc.runtime.AppRunArgs(cfg, opt, svc.attachFlags(cfg))
	if err != nil {
		return nil, err
	}
	desc := "run " + cfg.AppNameID
	if cfg.StartConditions.Multiterminal {
		desc = "run holder for " + cfg.AppNameID + " (terminals exec in)"
	}
	steps, err := svc.prepareSteps(cfg, opt)
	if err != nil {
		return nil, err
	}
	return append(steps, ports.Command{Args: appArgs, Desc: desc}), nil
}

// Launch validates cfg, auto-starts its depends_on apps (section 6.6), ensures its derived
// image (if ImageMeta.Install is set), runs the egress lock-down through the
// NetEnforcer (fail-closed: a half-built netns is torn down on any error), then
// starts the app container detached. A multiterminal app launches by opening its
// first terminal instead (the holder + a `podman exec`).
func (svc Service) Launch(cfg schema.AppConfig, opt options.HostOptions) error {
	return svc.launch(cfg, opt, nil, map[string]bool{})
}

// launch is Launch's recursive core. chain is the stack of apps already mid-launch
// (root → cfg's parent); it lets depends_on auto-start detect cycles. started is the
// set of apps already brought up in THIS launch, shared across the whole recursion:
// StartApp is detached, so a just-started app is not yet visible to runtime.Running(),
// and without this shared set a dependency reached by two branches (a diamond) would
// have its pod created twice - the second create failing and tearing the first down.
// The public Launch starts the recursion with a nil chain and an empty started set.
func (svc Service) launch(cfg schema.AppConfig, opt options.HostOptions, chain []string, started map[string]bool) error {
	if started[cfg.AppNameID] {
		return nil // already brought up earlier in this launch
	}
	if err := validate.Validate(cfg); err != nil { // launch-time check catches drift (section 3)
		return fmt.Errorf("%s: %w", cfg.AppNameID, err)
	}
	opt = svc.withBundle(cfg, opt)
	if err := checkNetwork(cfg); err != nil { // fail closed on not-yet-supported network shapes
		return err
	}
	// Refuse before preparing anything, because the fail-closed teardown below cannot tell
	// "the object I just tried to create already exists" from "the object I created is
	// broken". Without this, a second launch of a running app failed on its first prepare
	// step and then tore down the pod, the D-Bus proxy and the socket directory belonging to
	// the FIRST, healthy launch - so a double Enter in a launcher killed the app the person
	// was working in. Two concurrent launches were worse: each removed the other's objects
	// and nothing survived.
	//
	// Checked here rather than inside the loop so it also covers the un-instanced app whose
	// container merely exists in an Exited state; that is a name collision either way.
	if running, err := svc.runtime.Running(); err == nil && running[cfg.AppNameID] {
		return fmt.Errorf("%s is already running; stop it first, or run another instance with %s@<instance>",
			cfg.AppNameID, cfg.AppNameID)
	}
	started[cfg.AppNameID] = true
	if err := svc.startDependencies(cfg, opt, chain, started); err != nil { // section 6.6: dependencies first
		return err
	}
	if cfg.StartConditions.Multiterminal {
		return svc.OpenTerminal(cfg, opt, false) // ensures the image itself
	}
	if err := svc.ensureImage(cfg); err != nil {
		return err
	}
	steps, err := svc.prepareSteps(cfg, opt)
	if err != nil {
		return err // nothing has been created yet, so there is nothing to tear down
	}
	for _, cmd := range steps {
		if err := svc.runtime.Exec(cmd); err != nil {
			return errors.Join(fmt.Errorf("launch %s (%s): %w", cfg.AppNameID, cmd.Desc, err), svc.teardown(cfg, len(steps) > 0))
		}
	}
	opt, err = svc.withDisplay(cfg, opt)
	if err != nil {
		return errors.Join(fmt.Errorf("launch %s: %w", cfg.AppNameID, err), svc.teardown(cfg, len(steps) > 0))
	}
	appArgs, err := svc.runtime.AppRunArgs(cfg, opt, svc.attachFlags(cfg))
	if err != nil {
		return errors.Join(err, svc.teardown(cfg, len(steps) > 0))
	}
	// StartApp returns before `podman run` succeeds; if the app dies post-fork the
	// prepared pod/netns would leak, so onFail tears it down from the reaping goroutine.
	onFail := func() { _ = svc.teardown(cfg, len(steps) > 0) }
	if err := svc.runtime.StartApp(cfg, opt, appArgs, onFail); err != nil {
		return errors.Join(err, svc.teardown(cfg, len(steps) > 0))
	}
	return nil
}

// Stop tears a running app down via the enforcer's Teardown (the pod and its filtered
// netns for a filtered app, the container otherwise). Output is captured, so it is
// safe to call from a UI.
func (svc Service) Stop(cfg schema.AppConfig) error {
	return svc.runAll(svc.teardownSteps(cfg))
}

// teardown removes a half-built netns after a failed launch (fail-closed). It only
// fires when the enforcer had pre-steps to undo (a filtered app); an unfiltered app
// has nothing half-built. It returns any error so a failed/leaked teardown is visible
// to the caller (joined into the launch error) rather than silently swallowed.
func (svc Service) teardown(cfg schema.AppConfig, hadSteps bool) error {
	if !hadSteps {
		return nil
	}
	return svc.runAll(svc.teardownSteps(cfg))
}

// runAll executes steps in order and joins whatever failed. Teardown is the caller that
// needs the joining: removing the pod and removing the bridge it used are two commands, and
// a failure of the second must not hide the first having worked - or the other way round.
func (svc Service) runAll(steps []ports.Command) error {
	var errs []error
	for _, step := range steps {
		if err := svc.runtime.Exec(step); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NetCounters reads back what an app's enforced ruleset has actually seen, returning the
// enforcer's own output and whether the app has a ruleset at all. An app with no
// NetworkLists is the second case: it has no netns of its own, so there is nothing to count,
// and saying so is an answer rather than an error.
//
// The output is deliberately unparsed. What it means belongs to the enforcement mechanism -
// today nft's JSON - and the app layer teaching itself to read one adapter's format on a
// caller's behalf is exactly the coupling the NetEnforcer port exists to avoid (section 13).
func (svc Service) NetCounters(cfg schema.AppConfig, opt options.HostOptions) (string, bool, error) {
	cmd, filtered := svc.net.Counters(cfg, opt)
	if !filtered {
		return "", false, nil
	}
	out, err := svc.runtime.Capture(cmd)
	if err != nil {
		return "", true, err
	}
	return out, true, nil
}

// Rename changes an app's identity from oldName to newName. There is no atomic file
// rename, because the name lives in two places - the filename and AppNameID inside
// the YAML - so this loads the definition, rewrites AppNameID, saves it under the new
// name (which re-validates the name), and removes the old definition.
//
// It refuses to overwrite an existing app, and to rename a running one - its
// container is named after the old name and would be orphaned (the renamed definition
// could no longer stop it); stop it first.
func (svc Service) Rename(oldName, newName string) error {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	switch {
	case newName == "":
		return fmt.Errorf("rename %s: new name must not be empty", oldName)
	case newName == oldName:
		return fmt.Errorf("rename %s: new name is unchanged", oldName)
	case svc.store.Exists(newName):
		return fmt.Errorf("rename %s: %q already exists", oldName, newName)
	}
	if running, err := svc.runtime.Running(); err == nil && running[oldName] {
		return fmt.Errorf("rename %s: app is running - stop it first (its container is named %q)", oldName, oldName)
	}
	cfg, err := svc.store.Load(oldName)
	if err != nil {
		return fmt.Errorf("rename %s: %w", oldName, err)
	}
	cfg.AppNameID = newName
	if err := svc.store.Save(cfg); err != nil { // validates the new name before anything is removed
		return fmt.Errorf("rename %s -> %s: %w", oldName, newName, err)
	}
	if err := svc.store.Delete(oldName); err != nil {
		return fmt.Errorf("rename %s -> %s: saved new definition but could not remove the old one: %w", oldName, newName, err)
	}
	return nil
}

// Build force-rebuilds an app's derived image (the explicit-rebuild path).
func (svc Service) Build(cfg schema.AppConfig) error { return svc.builder.Build(cfg) }

// ensureImage builds the derived image when ImageMeta.Install is set and the live
// image is missing or stale (its fingerprint label differs) - the auto-on-run trigger
// (section 9.1).
func (svc Service) ensureImage(cfg schema.AppConfig) error {
	if !derived.HasInstall(cfg) {
		return nil
	}
	want := derived.BuildFingerprint(cfg)
	if got, err := svc.builder.Fingerprint(derived.DerivedImageRef(cfg.AppNameID)); err == nil && got == want {
		return nil // already current
	}
	return svc.builder.Build(cfg)
}

// --- thin passthroughs so the front-ends drive everything through one facade ---

func (svc Service) List() ([]string, error)                    { return svc.store.List() }
func (svc Service) Load(name string) (schema.AppConfig, error) { return svc.store.Load(name) }

// LoadResolved returns what an app actually is, with any Inherits chain merged in - the form
// a launch must read. Load stays the file as written, for Rename, which writes it back.
func (svc Service) LoadResolved(name string) (schema.AppConfig, error) {
	return svc.store.LoadResolved(name)
}

func (svc Service) LoadFileResolved(path string) (schema.AppConfig, error) {
	return svc.store.LoadFileResolved(path)
}
func (svc Service) Save(cfg schema.AppConfig) error { return svc.store.Save(cfg) }
func (svc Service) Delete(name string) error        { return svc.store.Delete(name) }
func (svc Service) Exists(name string) bool         { return svc.store.Exists(name) }
func (svc Service) Path(name string) string         { return svc.store.Path(name) }

func (svc Service) Marshal(cfg schema.AppConfig) ([]byte, error)   { return svc.store.Marshal(cfg) }
func (svc Service) LoadFile(path string) (schema.AppConfig, error) { return svc.store.LoadFile(path) }

func (svc Service) Search(term string) ([]ports.Result, error) { return svc.resolver.Search(term) }
func (svc Service) Resolve(ref string) (string, error)         { return svc.resolver.Resolve(ref) }

func (svc Service) Running() (map[string]bool, error)          { return svc.runtime.Running() }
func (svc Service) PIDs() (map[string]int, error)              { return svc.runtime.PIDs() }
func (svc Service) Logs(name string, tail int) (string, error) { return svc.runtime.Logs(name, tail) }

// Do runs a user-facing runtime command (restart/inspect/logs passthrough) with the
// host's stdio - for the CLI, where streaming output is wanted.
func (svc Service) Do(args []string) error { return svc.runtime.Do(args) }
