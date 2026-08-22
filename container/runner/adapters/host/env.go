// Package host is the environment adapter: it resolves the host-side launch options (sockets, theme
// bundle, terminal emulator, netfilter image) into an options.HostOptions. The one place env ->
// options lives, so every front-end wires the host identically (docs section 9.1, section 13).
package host

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/crispuscrew/zinc/container/runner/domain/options"
)

// Options resolves the host launch options from the environment. NetfilterImage is
// left empty when unset, so the enforcer falls back to its built-in default.
func Options() options.HostOptions {
	return options.HostOptions{
		RuntimeDir:     os.Getenv("XDG_RUNTIME_DIR"),
		WaylandDisplay: os.Getenv("WAYLAND_DISPLAY"),
		ThemeBundleDir: os.Getenv("ZINC_THEME_BUNDLE"),
		ConfigHome:     configHome(),
		HomeDir:        "/root",
		NetfilterImage: netfilterImage(),
		Terminal:       terminalArgv(),
		SessionBusPath: sessionBusPath(),
	}
}

// netfilterImageRE is the same rule the validator applies to an app image, with the local
// exemption spelled out: either a localhost/ reference, or a canonical digest pin.
var netfilterImageRE = regexp.MustCompile(
	`^(localhost/[A-Za-z0-9][A-Za-z0-9._/-]*(:[A-Za-z0-9._-]+)?|[A-Za-z0-9][A-Za-z0-9._/-]*@sha256:[0-9a-f]{64})$`)

// netfilterImage resolves ZINC_NETFILTER_IMAGE, ignoring a value that is not a reference. This names
// the most privileged image Zinc runs - the helper holding CAP_NET_ADMIN in the app's namespace, and
// the one carrying xdg-dbus-proxy - so it gets the same screening as an app's own image, not least
// because a value beginning with '-' would land in podman's flag position. An unusable value falls
// back to the built-in default: this is a development override, not a config field.
func netfilterImage() string {
	image := strings.TrimSpace(os.Getenv("ZINC_NETFILTER_IMAGE"))
	if image == "" {
		return ""
	}
	if !netfilterImageRE.MatchString(image) {
		fmt.Fprintf(os.Stderr,
			"warning: ignoring ZINC_NETFILTER_IMAGE=%q - it must be a localhost/ reference or a digest pin (@sha256:<64 hex>); using the built-in default\n",
			image)
		return ""
	}
	return image
}

// sessionBusPath resolves the host session bus socket for the D-Bus proxy. Only the "unix:path=" form
// is understood: the proxy needs a filesystem socket it can bind-mount, an abstract socket has no
// path, and a tcp bus is not something to hand a sandbox by inference. Empty fails the launch of an
// app that asked for a bus, which is the fail-closed answer.
//
// The fallback is $XDG_RUNTIME_DIR/bus, where the per-user bus lives when the variable is unset - a
// login shell that never sourced the session environment, which is the hotkey-launch case.
func sessionBusPath() string {
	// Split on ';', which separates ADDRESSES; ',' separates the key=value pairs INSIDE one. Splitting on
	// ',' made every form this function does not understand fall through to the fallback, so a user who
	// had deliberately pointed the session at a nested or restricted bus got a sandbox proxied onto the
	// MAIN user bus instead, silently. Set but unparseable now returns empty; only UNSET takes the
	// fallback.
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address != "" {
		for _, candidate := range strings.Split(address, ";") {
			for _, pair := range strings.Split(strings.TrimSpace(candidate), ",") {
				if path, ok := strings.CutPrefix(strings.TrimSpace(pair), "unix:path="); ok {
					return path
				}
			}
		}
		return ""
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "bus")
	}
	return ""
}

// terminalArgv resolves the terminal emulator for terminal apps: $ZINC_TERMINAL, else
// $TERMINAL, split on spaces so both "foot" and "xterm -e" work. Empty when neither is
// set - launching a terminal app then fails with a clear message.
func terminalArgv() []string {
	spec := os.Getenv("ZINC_TERMINAL")
	if spec == "" {
		spec = os.Getenv("TERMINAL")
	}
	return strings.Fields(spec)
}

// configHome resolves XDG_CONFIG_HOME with its specified default, since an app's bundle is
// found relative to it and the variable is unset on most desktops.
func configHome() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}
