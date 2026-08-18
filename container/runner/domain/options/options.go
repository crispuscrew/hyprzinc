package options

// HostOptions carries the host-side values a launch needs. Passed explicitly so the argv-building
// adapters never read the environment and stay pure. Empty fields disable the matching wiring;
// adapters/host resolves these from the environment.
type HostOptions struct {
	RuntimeDir     string // host XDG_RUNTIME_DIR (wayland/pipewire sockets)
	WaylandDisplay string // host WAYLAND_DISPLAY, e.g. "wayland-1"
	ThemeBundleDir string // host path to the generated curated theme bundle (section 5.6)
	ConfigHome     string // host XDG_CONFIG_HOME, the root an app's bundle is resolved from
	// BundleDir is THIS app's bundle, resolved per launch from the app half of its address.
	// It cannot be derived inside the argv builder: by then AppNameID carries the instance
	// (notes.work), and a bundle is per app, so deriving it there sent every instanced app at
	// apps/notes.work/configs, which nothing creates.
	BundleDir      string
	NetfilterImage string   // image carrying nft for the pasta lock-down step (section 5.3); empty → adapter default
	HomeDir        string   // container-side home for key mounts (.ssh/.gnupg); empty → /root
	Terminal       []string // terminal-emulator argv for terminal apps, e.g. ["foot"] or ["xterm","-e"] (section 11)
	// SessionBusPath is the host path of the real session bus socket, read out of
	// DBUS_SESSION_BUS_ADDRESS (the "unix:path=" form). Only the D-Bus proxy sees it; the
	// app never does. Empty disables DBusMeta wiring, which fails the launch of an app that
	// asked for a bus rather than starting it without one.
	SessionBusPath string
	// WaylandSocket is the host path of the Wayland socket to bind-mount in: the per-instance one a
	// wp_security_context_v1 was attached to (section 5.2). Empty means the compositor's own socket. The
	// one field here that is a RESULT rather than a host fact - filled in after the context exists, so an
	// argv can never claim a socket that was never made.
	WaylandSocket string
}
