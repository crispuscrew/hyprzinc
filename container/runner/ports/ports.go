// Package ports declares the contracts between the runner's application core (app)
// and the outside world (adapters/*). It is the hexagon's boundary: the app layer
// depends only on these interfaces, never on a concrete podman/nft/fs
// implementation, so a mechanism can be swapped by writing a new adapter - the
// motivating case being egress enforcement (NetEnforcer), where "not pasta" later is
// one more adapter, not a cross-cutting edit (docs/architecture.md section 5.3, section 13).
//
// ports depends only on pure types - the shared schema (common) and the runner's own
// HostOptions - and performs no I/O itself.
package ports

import (
	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// Command is one runtime instruction - the args passed to the container runtime,
// with optional stdin (used to pipe the nft ruleset into the lock-down step) and a
// short human label for dry-run output. It is the neutral unit a NetEnforcer emits
// and a Runtime executes, so neither side hardcodes the other's CLI.
type Command struct {
	Args  []string // arguments to the runtime (e.g. podman)
	Stdin string   // optional stdin
	Desc  string   // short human label (shown in dry-run)
}

// Result is one image-registry search hit.
type Result struct {
	Name        string
	Description string
}

// Store persists app definitions and provides the YAML codec for the editor
// round-trip (Marshal a draft, LoadFile it back). Adapter: adapters/fs.
type Store interface {
	List() ([]string, error)
	// Load returns an app as its file was written; LoadResolved returns what it actually
	// is, with any Inherits chain merged in. A launch reads the resolved form; anything
	// that will write the config back reads the raw one, since a resolved config saved
	// over its source would flatten the inheritance away.
	Load(name string) (schema.AppConfig, error)
	LoadResolved(name string) (schema.AppConfig, error)
	LoadFileResolved(path string) (schema.AppConfig, error)
	Save(cfg schema.AppConfig) error
	Delete(name string) error
	Exists(name string) bool
	Path(name string) string
	Marshal(cfg schema.AppConfig) ([]byte, error)   // encode a draft to YAML (for $EDITOR)
	LoadFile(path string) (schema.AppConfig, error) // decode an arbitrary .yaml path (CLI path arg, editor round-trip)
}

// Runtime drives the container engine. Adapter: adapters/podman. AppRunArgs is pure
// (builds argv, no I/O) so plans can be inspected/dry-run; everything else performs
// I/O. netFlags are supplied by a NetEnforcer, so the runtime never knows which
// egress mechanism is in play.
type Runtime interface {
	AppRunArgs(cfg schema.AppConfig, opt options.HostOptions, netFlags []string) ([]string, error)
	Exec(cmd Command) error // run one prepared command (pod create / nft / holder); capture output on failure
	// Capture runs one prepared command and returns its standard output. Exec is the wrong
	// tool for a command whose output IS the answer: it keeps the output only to put it in
	// an error, so on success - the case that matters here - it is already gone.
	Capture(cmd Command) (string, error)
	// StartApp starts the app container detached (Setsid), terminal-wrapped if
	// StartConditions.Terminal. It returns once the process is forked, before `podman
	// run` succeeds; onFail is invoked from the reaping goroutine if the app exits with
	// an error, so a post-fork failure can tear down the prepared (still-filtered) netns.
	StartApp(cfg schema.AppConfig, opt options.HostOptions, runArgs []string, onFail func()) error
	OpenSession(app string, cmd []string, opt options.HostOptions, hold bool) error // blocking `exec -it` into a holder, in a terminal window (multiterminal); hold keeps the window open after cmd exits
	// HealthProbe answers "is this app ready right now?" once, without blocking: nil
	// means ready. The app layer polls it to hold a launch until the apps it depends on
	// can actually serve it (StartConditions.ReadyCheck); how the answer is obtained is
	// the adapter's business.
	HealthProbe(name string) error
	Exists(name string) bool // does a container with this name exist (running or not)?
	// IsRunning is Exists narrowed to "and is it actually running". A container that runs
	// without --rm (KeepAlive, Autorestart) survives its own exit, so Exists cannot tell an
	// alive holder from a dead one. A query failure answers false, which for the callers here
	// means "start one" rather than "attach to it", and is the safe direction.
	IsRunning(name string) bool
	Do(args []string) error            // user-facing passthrough (stop/restart/inspect/logs) with host stdio
	Running() (map[string]bool, error) // names the runtime reports as running (list view)
	// PIDs is the host PID of each running container's main process, by container name.
	// Rootless podman does not remap pids, so these are the numbers other host tools report
	// for the same processes - which is what makes a container identifiable from outside the
	// runtime. Bus attribution needs exactly that: the session bus answers
	// GetConnectionUnixProcessID with a host pid, and this is what turns that pid back into
	// a container Zinc named.
	PIDs() (map[string]int, error)
	Logs(name string, tail int) (string, error) // last N log lines (logs view)
}

// ImageBuilder builds an app's derived image (FROM ImageMeta.Image + the install
// layer). Adapter: adapters/podman.
type ImageBuilder interface {
	Build(cfg schema.AppConfig) error       // force a build
	Fingerprint(ref string) (string, error) // read the build label; error if the image is absent
}

// ImageResolver discovers images and pins tags to digests (section 5.5). Adapter:
// adapters/podman.
type ImageResolver interface {
	Search(term string) ([]Result, error)
	Resolve(ref string) (string, error)
}

// DBusBroker gives an app a filtered session bus (DBusMeta): a socket of its own, served by
// a proxy Zinc owns, carrying only the names the config named. Adapter:
// adapters/dbusproxy.
//
// It is a sibling of NetEnforcer rather than part of it, and shaped the same way, because it
// is the same kind of problem: a capability the app must never hold directly, established
// before the app exists and removed after it dies. The proxy holds the real socket; the app
// holds only what the proxy chooses to forward. Swapping xdg-dbus-proxy for another
// mechanism is one more adapter, not a cross-cutting edit.
type DBusBroker interface {
	// RunFlags are the app-container flags that attach the filtered socket: the bind mount
	// and DBUS_SESSION_BUS_ADDRESS pointing at it. Empty when the app asked for no bus, so
	// an app without DBusMeta is not handed a bus address that resolves to nothing.
	// The host-side facts a broker needs (the runtime dir, the real bus path) are given to
	// the adapter when it is built rather than passed per call, so Teardown stays reachable
	// from Stop, which knows an app config and nothing about the host.
	RunFlags(cfg schema.AppConfig) []string
	// Prepare returns the steps that create the app's socket directory and start the proxy,
	// to run BEFORE the app. It can fail: an app that asked for a bus when the host has no
	// resolvable session bus must not launch as though it had one.
	Prepare(cfg schema.AppConfig) ([]Command, error)
	// Teardown removes the proxy container and the socket directory. Separate steps, since a
	// proxy that stopped on its own still leaves the directory behind, and one would
	// otherwise accumulate per app that ever ran.
	Teardown(cfg schema.AppConfig) []Command
}

// DisplayBroker gives an app a Wayland socket of its own, one the compositor has attached a
// wp_security_context_v1 to (section 5.2). Adapter: adapters/waylandctx.
//
// A sibling of NetEnforcer and DBusBroker, and the same shape for the same reason: the app is
// handed a derived socket rather than the compositor's own, and what the display server
// believes about it is decided before it exists. Unlike those two it has no Teardown, because
// the thing that has to be undone is a descriptor held by a process that watches the app and
// exits with it - there is nothing left for a Stop to remove.
type DisplayBroker interface {
	// Establish creates the app's socket, registers it with the compositor and returns the
	// host path to bind-mount. An empty path means "mount the compositor's own socket" and is
	// not an error: it is the answer both for an app that opted out
	// (DisplayMeta.DisableSecurityContext) and on a compositor that does not implement the
	// protocol, where refusing to launch would buy nothing and cost the whole desktop.
	// Everything else - a socket that cannot be bound, a compositor that cannot be reached, a
	// rejected request - fails the launch rather than silently degrading it.
	Establish(addr paths.Address, cfg schema.AppConfig, opt options.HostOptions) (string, error)
}

// NetEnforcer establishes and enforces an app's network egress - THE swap point.
// The one adapter today (adapters/netenforce) drives NetworkLists onto the app's own
// pasta netns via nft (or --network none when there are no lists). A future
// mechanism is one more implementation; the app layer is agnostic. Callers gate
// unsupported configs before invoking it (the app layer's checkNetwork).
type NetEnforcer interface {
	RunFlags(cfg schema.AppConfig) []string // app container network attach (--pod ... / --network ...)
	// Prepare returns the steps that establish and LOCK the netns before the app starts.
	// It can fail: an allowlist given by name has to be resolved first, and a launch whose
	// allowlist could not be resolved must not proceed with a shorter one.
	Prepare(cfg schema.AppConfig, opt options.HostOptions) ([]Command, error)
	// Teardown returns the steps that remove everything the launch created, in order. More
	// than one, because a pod is not the only thing an app can own: the per-app egress
	// bridge outlives it otherwise, and one podman network accumulates per app that ever ran.
	Teardown(cfg schema.AppConfig) []Command
	// Counters returns the command that reads back what the enforcement has actually seen,
	// and false when this app has nothing to ask (no lists, so no netns of its own). It
	// belongs on this port rather than beside the runtime because "what did enforcement
	// do" is part of the mechanism: another NetEnforcer answers it in its own terms, or
	// says it cannot. The output's format is likewise the adapter's - the app layer passes
	// it through rather than learning to read one adapter's JSON.
	Counters(cfg schema.AppConfig, opt options.HostOptions) (Command, bool)
}
