// Package dbusproxy is the D-Bus adapter: it implements ports.DBusBroker by running xdg-dbus-proxy
// in a container Zinc owns, so an app with DBusMeta gets a bus carrying only the names it named
// (section 5.7).
//
// Two properties are load bearing. The proxy is NOT in the app's pod - a pod shares the PID
// namespace, so the app could signal or ptrace what filters it. And the app never receives the real
// bus socket, only the proxy's own. Everything here is argv-building, so it is pure and testable.
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

// ctrSocketDir is the container-side mirror of HostSocketDir, kept here so the two cannot drift.
// The empty return is a guard: this string is the operand of an `rm -rf` in a helper with the host
// XDG_RUNTIME_DIR mounted read-write. Teardown skips on empty; Prepare fails the launch closed.
func ctrSocketDir(app string) string {
	dir := filepath.Join(ctrRuntimeRoot, "zinc", "dbus", app)
	if !strings.HasPrefix(dir, ctrRuntimeRoot+"/zinc/dbus/") {
		return ""
	}
	return dir
}

// Broker implements ports.DBusBroker. The host facts are held rather than passed per call, so
// Teardown is reachable from Stop, which knows an app config and nothing about the host.
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

// AppOfProxy is ContainerName run backwards: the app a proxy container was named for. The
// load-bearing half of bus attribution - Zinc named it from an app it had already resolved, so
// nothing here asks the app who it is.
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

// Prepare creates the app's socket directory and starts the proxy, before the app. A missing host
// bus is an error rather than a silent skip: the app asked for bus access.
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

	// The mkdir runs in a helper container rather than as a syscall here, because Prepare is also what
	// Plan renders for a dry run. The mount is XDG_RUNTIME_DIR itself, since on a first launch the
	// app's own directory does not exist yet and podman cannot bind-mount a missing source; the helper
	// does one `mkdir -p` with no capability and no network, and the APP never gets this mount.
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

	// The proxy itself: detached, holding the real bus, serving the filtered one. --cap-drop all
	// applies to it as much as to the app. keep-id is why validation requires it on the app too: the
	// socket is created as the host uid, and an app in another user namespace could not connect.
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

	// Then WAIT for it. `podman run -d` returns when the container started, not when xdg-dbus-proxy has
	// bound its socket, and the window is load-dependent: it passes on a quiet machine and fails on a
	// busy one. The probe is a real method call, since the file appears at bind() and the proxy is only
	// useful once it answers.
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

// readyScript polls the filtered socket until it answers, and fails the launch if it never does
// rather than handing the app a socket nothing is listening on. 100 attempts at 50ms.
const readyScript = `probe="dbus-send --bus=unix:path=` + ctrProxyDir + `/` + proxySocket + ` --dest=org.freedesktop.DBus --type=method_call --print-reply /org/freedesktop/DBus org.freedesktop.DBus.ListNames"
for attempt in $(seq 1 100); do
	if $probe >/dev/null 2>&1; then
		exit 0
	fi
	sleep 0.05
done
echo "the filtered dbus socket did not begin answering within 5s - the proxy failed to start; check: podman logs zinc-dbus-<app>" >&2
exit 1`

// FilterArgs renders DBusMeta as xdg-dbus-proxy filter options, Talk before Own. Exported so a test
// can assert the exact grants a config produces. --filter comes from the caller: it is what makes
// these an allowlist rather than annotations on a fully open bus.
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

// Teardown removes the proxy and then the socket directory. `rm -f` rather than `stop`, so it also
// cleans up after a proxy that already exited - otherwise the name stays taken and the app cannot
// be relaunched.
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
