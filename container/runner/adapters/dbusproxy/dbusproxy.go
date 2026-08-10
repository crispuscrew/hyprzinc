// Package dbusproxy is the D-Bus adapter: it implements ports.DBusBroker by running
// xdg-dbus-proxy in a container Zinc owns, so an app with DBusMeta gets a session bus
// carrying only the names its config named (docs/architecture.md section 5.7).
//
// The shape is the same one the egress lock-down uses: the dangerous thing is established
// outside the app and the app is handed only the filtered result. Two properties are load
// bearing, and both are about what the app can reach rather than what it is allowed to call.
//
// The proxy is NOT in the app's pod. A pod shares the PID namespace, so a proxy inside it
// would be a process the app could signal or ptrace - the filter and the thing being
// filtered, in one blast radius. It is a standalone container instead, and shares with the
// app exactly one thing: the socket, through a bind mount.
//
// The app never receives the real bus socket. Only the proxy mounts it, read-write because a
// bus client must write to connect, and the app's mount is the proxy's own socket.
//
// Everything here is argv-building and therefore pure and testable; the Runtime executes
// what this returns.
package dbusproxy

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// DefaultImage carries xdg-dbus-proxy. It is the same helper image the netfilter steps use,
// referenced by local tag and run with --pull never (section 5.5): a locally built, vetted
// image, never something fetched at launch.
const DefaultImage = "localhost/zinc/netfilter:local"

// ctrPaths inside the proxy container. The real bus and the served socket are kept in
// separate directories so the mount that carries the host bus can never be the directory the
// app also mounts.
const (
	ctrHostBus  = "/run/zinc-host-bus" // the real session bus, proxy-only
	ctrProxyDir = "/run/zinc-proxy"    // where the proxy writes the filtered socket
	proxySocket = "bus"                // socket filename, in both the proxy and the app
)

// ctrAppSocket is where the filtered socket lands in the APP container. It sits outside
// /run/zinc (the XDG runtime dir the Wayland and Pipewire sockets share) because
// DBUS_SESSION_BUS_ADDRESS names this path explicitly and nothing benefits from it being
// adjacent to sockets the app reaches by a different convention.
const ctrAppSocket = "/run/zinc-bus/bus"

// ctrRuntimeRoot is where the host XDG_RUNTIME_DIR is mounted for the mkdir/rm helper steps.
// Only those two ever see it; the app does not.
const ctrRuntimeRoot = "/run/zinc-runtime"

// ctrSocketDir is an app's socket directory as the mkdir/rm helper sees it: the container-side
// mirror of HostSocketDir, kept here so the two cannot drift into naming different directories.
//
// The empty return is a guard, not a code path anyone should reach: this string is the
// argument to an `rm -rf` in a helper that has the host XDG_RUNTIME_DIR bind-mounted
// read-write, so a name that walks out of the app's own directory would delete the session's
// sockets. Validation refuses such a name long before here (nameRE has no '/' and cannot
// start with '.'), and Teardown skips the step on empty. Prepare does not: an empty operand
// makes mkdir fail and the launch fail closed, which is the right outcome but is worth knowing
// is where it comes from.
func ctrSocketDir(app string) string {
	dir := filepath.Join(ctrRuntimeRoot, "zinc", "dbus", app)
	if !strings.HasPrefix(dir, ctrRuntimeRoot+"/zinc/dbus/") {
		return ""
	}
	return dir
}

// Broker implements ports.DBusBroker. The host facts are held rather than passed per call,
// so Teardown is reachable from Stop, which knows an app config and nothing about the host.
//
//   - Image: the helper carrying xdg-dbus-proxy; empty means DefaultImage.
//   - RuntimeDir: host XDG_RUNTIME_DIR, the parent of every app's socket directory.
//   - SessionBusPath: the real session bus socket. Only the proxy ever sees it.
type Broker struct {
	Image          string
	RuntimeDir     string
	SessionBusPath string
}

// New builds a Broker from the resolved host options. image may be empty (DefaultImage).
func New(image string, opt options.HostOptions) Broker {
	return Broker{Image: image, RuntimeDir: opt.RuntimeDir, SessionBusPath: opt.SessionBusPath}
}

func (brk Broker) image() string {
	if strings.TrimSpace(brk.Image) == "" {
		return DefaultImage
	}
	return brk.Image
}

// proxyPrefix marks a container as one of ours. An app name is [a-z0-9][a-z0-9._-]*, so no
// app container can carry it, which is what makes the prefix both collision-free and a
// reliable filter when reading the runtime's container list back.
const proxyPrefix = "zinc-dbus-"

// ContainerName is the proxy container's name, derived from the app's. It is a separate
// object in the runtime's namespace from the app itself, so it needs a name that cannot
// collide with an app's and is recoverable from the app name alone at teardown.
func ContainerName(app string) string { return proxyPrefix + app }

// AppOfProxy is ContainerName run backwards: the app (runtime) name a proxy container was
// named for, and whether the container is a Zinc proxy at all.
//
// This is the load-bearing half of bus attribution. Zinc named this container when it
// created it, from an app it had already resolved, so reading the name back yields the app
// that proxy serves without asking the app anything - which is the whole point, since an app
// asserting its own identity on the bus is exactly what cannot be trusted.
func AppOfProxy(container string) (string, bool) {
	app, found := strings.CutPrefix(container, proxyPrefix)
	if !found || app == "" {
		return "", false
	}
	return app, true
}

// HostSocketDir is the per-app directory on the host holding that app's filtered socket. Per
// app, not shared: two apps with different grants must not be able to reach each other's
// socket, and the whole point is that this one carries only what this app was given.
func HostSocketDir(runtimeDir, app string) string {
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, "zinc", "dbus", app)
}

// HostSocketPath is the filtered socket itself: the one file the app's
// DBUS_SESSION_BUS_ADDRESS resolves to. Exported because `zcr where` reports it, and a
// reporter that rebuilt the path from a filename only this package knows would start
// answering a different question the moment either changed.
func HostSocketPath(runtimeDir, app string) string {
	dir := HostSocketDir(runtimeDir, app)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, proxySocket)
}

// RunFlags attaches the filtered socket to the app: the bind mount and the address pointing
// at it. Read-write, because connecting to a unix socket is a write operation - a read-only
// mount would present the app with a socket it cannot use.
func (brk Broker) RunFlags(cfg schema.AppConfig) []string {
	socket := HostSocketPath(brk.RuntimeDir, cfg.AppNameID)
	if cfg.DBusMeta.IsZero() || socket == "" {
		return nil
	}
	return []string{
		"-v", socket + ":" + ctrAppSocket + ":rw",
		"-e", "DBUS_SESSION_BUS_ADDRESS=unix:path=" + ctrAppSocket,
	}
}

// Prepare creates the app's socket directory and starts the proxy. It runs before the app, so
// the socket exists by the time the app looks for it.
//
// A missing host bus is an error rather than a silent skip: the app asked for bus access, and
// starting it with no bus at all would surface as the app being broken in a way that points
// nowhere near the config.
func (brk Broker) Prepare(cfg schema.AppConfig) ([]ports.Command, error) {
	if cfg.DBusMeta.IsZero() {
		return nil, nil
	}
	dir := HostSocketDir(brk.RuntimeDir, cfg.AppNameID)
	if dir == "" {
		return nil, fmt.Errorf("%s: DBusMeta needs XDG_RUNTIME_DIR set, to place the app's bus socket", cfg.AppNameID)
	}
	if strings.TrimSpace(brk.SessionBusPath) == "" {
		return nil, fmt.Errorf("%s: DBusMeta asked for a filtered session bus, but no host session bus could be resolved - set DBUS_SESSION_BUS_ADDRESS to a unix:path= address", cfg.AppNameID)
	}

	// The socket directory is created by a helper-image container rather than by this process,
	// because Prepare is also what Plan renders for a dry run: doing the mkdir here as a syscall
	// would mean `zcr run` without --exec left directories behind, and would leave a launch step
	// invisible in the plan the user is shown.
	//
	// The mount is XDG_RUNTIME_DIR itself, not the app's directory or its parent, because on a
	// first launch neither exists and podman cannot bind-mount a source that is not there.
	// Mounting the runtime dir is broader than this step needs, and what makes it acceptable is
	// narrow: our own vetted image, one `mkdir -p`, no capability, no network, and it exits. The
	// APP never gets this mount - it receives only its own socket.
	steps := []ports.Command{{
		Args: []string{
			"run", "--rm", "--pull", "never",
			"--userns=keep-id",
			"--security-opt", "no-new-privileges", "--cap-drop", "all",
			"--network", "none",
			"-v", brk.RuntimeDir + ":" + ctrRuntimeRoot + ":rw",
			brk.image(),
			// Each level is its own operand because -m applies only to the last component
			// of each one: `mkdir -m 700 -p a/b/c` leaves a and a/b at the image's umask
			// (0755), which is not what "the socket directory is 700" is supposed to mean.
			"mkdir", "-m", "700", "-p",
			ctrRuntimeRoot + "/zinc",
			ctrRuntimeRoot + "/zinc/dbus",
			ctrSocketDir(cfg.AppNameID),
		},
		Desc: "create bus socket dir for " + cfg.AppNameID,
	}}

	// The proxy itself: detached, holding the real bus, serving the filtered one. --cap-drop
	// all and no-new-privileges apply to the proxy as much as to the app - it is a helper Zinc
	// runs, not a trusted component, and it needs no capability to relay a socket.
	//
	// keep-id is why validation requires it on the app too: the socket is created by this
	// container as the host uid, and an app in a different user namespace could not connect.
	proxyArgs := []string{
		"run", "-d", "--rm", "--pull", "never",
		// --replace because Zinc owns this name by construction. Without it a proxy left
		// behind by an app that exited on its own blocks every future launch of that app
		// with a name collision, and the failure is confusing: the message names a
		// container the user never created.
		"--replace", "--name", ContainerName(cfg.AppNameID),
		"--userns=keep-id",
		"--security-opt", "no-new-privileges", "--cap-drop", "all",
		"--network", "none", // a bus relay needs no network, and this is the app's most privileged neighbour
		"-v", brk.SessionBusPath + ":" + ctrHostBus + ":rw",
		"-v", dir + ":" + ctrProxyDir + ":rw",
		brk.image(),
		"xdg-dbus-proxy",
		"unix:path=" + ctrHostBus,
		filepath.Join(ctrProxyDir, proxySocket),
		"--filter", // without this the proxy forwards everything and the grants below are decoration
	}
	proxyArgs = append(proxyArgs, FilterArgs(cfg.DBusMeta)...)
	steps = append(steps, ports.Command{Args: proxyArgs, Desc: "start filtered dbus proxy for " + cfg.AppNameID})

	// Then WAIT for it, before the app is allowed to start. `podman run -d` returns when the
	// container has started, not when xdg-dbus-proxy has bound and begun serving its socket,
	// so without this the app can reach its first bus call before the socket exists and die
	// with a bare connection error that points nowhere near the cause. The window is small and
	// load-dependent, which is the worst kind: it passes on a quiet machine and fails on a busy
	// one.
	//
	// The probe is a real method call rather than a test for the socket file, because the file
	// appears at bind() and the proxy is only useful once it answers - and an app that starts
	// between those two points fails exactly as if the file had been missing.
	steps = append(steps, ports.Command{
		Args: []string{
			"run", "--rm", "--pull", "never",
			"--userns=keep-id",
			"--security-opt", "no-new-privileges", "--cap-drop", "all",
			"--network", "none",
			"-v", dir + ":" + ctrProxyDir + ":rw",
			brk.image(),
			"sh", "-c", readyScript,
		},
		Desc: "wait for the dbus proxy of " + cfg.AppNameID + " to serve",
	})
	return steps, nil
}

// readyScript polls the filtered socket with a real bus call until it answers, and fails the
// launch if it never does. Fail-closed: a proxy that never came up must stop the launch, not
// hand the app a socket nothing is listening on.
//
// 100 attempts at 50ms is a five-second ceiling. Long enough for a loaded machine to start a
// container, short enough that a genuinely broken proxy reports itself instead of hanging a
// launch the user is waiting on.
const readyScript = `probe="dbus-send --bus=unix:path=` + ctrProxyDir + `/` + proxySocket + ` --dest=org.freedesktop.DBus --type=method_call --print-reply /org/freedesktop/DBus org.freedesktop.DBus.ListNames"
for attempt in $(seq 1 100); do
	if $probe >/dev/null 2>&1; then
		exit 0
	fi
	sleep 0.05
done
echo "the filtered dbus socket did not begin answering within 5s - the proxy failed to start; check: podman logs zinc-dbus-<app>" >&2
exit 1`

// FilterArgs renders DBusMeta as xdg-dbus-proxy filter options, Talk before Own. Exported so
// a test can assert the exact grants a config produces - the thing that decides what the app
// can reach - without reconstructing the whole launch.
//
// --filter is supplied by the caller, not here: it is the switch that makes these grants an
// allowlist rather than annotations on a fully open bus, so it belongs with the invocation
// that must not omit it.
func FilterArgs(bus schema.DBusMeta) []string {
	args := make([]string, 0, len(bus.Talk)+len(bus.Own))
	for _, name := range bus.Talk {
		args = append(args, "--talk="+name)
	}
	for _, name := range bus.Own {
		args = append(args, "--own="+name)
	}
	return args
}

// Teardown removes the proxy and the socket directory. Both, and in this order: the proxy is
// --rm so it usually removes itself, but "usually" leaves an app that cannot be relaunched
// because its proxy name is taken, and the directory outlives the proxy either way.
//
// `rm -f` rather than `stop`, because this must also clean up after a proxy that already
// exited, where stop would fail and stop the teardown before the directory was removed.
func (brk Broker) Teardown(cfg schema.AppConfig) []ports.Command {
	if cfg.DBusMeta.IsZero() {
		return nil
	}
	steps := []ports.Command{{
		Args: []string{"rm", "-f", "--ignore", ContainerName(cfg.AppNameID)},
		Desc: "remove dbus proxy for " + cfg.AppNameID,
	}}
	if socketDir := ctrSocketDir(cfg.AppNameID); brk.RuntimeDir != "" && socketDir != "" {
		steps = append(steps, ports.Command{
			Args: []string{
				"run", "--rm", "--pull", "never",
				"--userns=keep-id",
				"--security-opt", "no-new-privileges", "--cap-drop", "all",
				"--network", "none",
				"-v", brk.RuntimeDir + ":" + ctrRuntimeRoot + ":rw",
				brk.image(),
				"rm", "-rf", socketDir,
			},
			Desc: "remove bus socket dir for " + cfg.AppNameID,
		})
	}
	return steps
}
