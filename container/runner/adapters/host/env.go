// Package host is the environment adapter: it resolves the host-side launch options
// (Wayland/runtime sockets, theme bundle, terminal emulator, netfilter image) from
// environment variables into an options.HostOptions. It is the one place env → options
// lives, so every front-end wires the host identically and the argv-building adapters
// stay pure (docs/architecture.md section 9.1, section 13).
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

// netfilterImage resolves ZINC_NETFILTER_IMAGE, ignoring a value that is not a reference.
//
// This names the most privileged image Zinc runs: the helper that holds CAP_NET_ADMIN in the
// app's namespace and loads the nft ruleset, and the one that carries xdg-dbus-proxy holding
// the real session bus. It was passed through unchecked, which meant it was the one image
// reference in the product held to a weaker standard than the validator applies to an app's
// own image - and a value beginning with '-' would land in podman's flag position.
//
// An unusable value falls back to the built-in default rather than failing the launch: this
// is an override for developing on the helper image, not a config field, and the default is
// the safe answer.
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

// sessionBusPath resolves the host session bus socket for the D-Bus proxy (DBusMeta).
//
// Only the "unix:path=" form is understood, and anything else resolves to empty rather than
// being guessed at. DBUS_SESSION_BUS_ADDRESS is a comma-separated list of addresses in
// several transports (unix:, tcp:, autolaunch:, and unix:abstract= among them), and the proxy
// needs a filesystem socket it can bind-mount. An abstract socket has no path to mount, and a
// tcp bus is not something to hand a sandbox by inference. Empty makes the launch of an app
// that asked for a bus fail and say so, which is the fail-closed answer; the alternative -
// starting it with no bus - looks like the app is broken.
//
// The fallback is the standard rootless location, $XDG_RUNTIME_DIR/bus, which is where the
// per-user bus lives when the variable is unset (a login shell that never sourced the session
// environment, which is exactly the case for an app launched from a hotkey).
func sessionBusPath() string {
	// Split on ';', which is what separates ADDRESSES. ',' separates the key=value pairs
	// INSIDE one address, so splitting on it made "unix:path=/a;unix:path=/b" come back as a
	// single bogus path, and made every form this function does not understand fall through
	// to the fallback below.
	//
	// That fall-through is the part that mattered: an abstract-socket, tcp, or
	// "unix:guid=...,path=..." address all resolved to $XDG_RUNTIME_DIR/bus, so a user who
	// had deliberately pointed the session at a nested or restricted bus (dbus-run-session,
	// a test bus) got a sandbox proxied onto the MAIN user bus instead - strictly more than
	// the environment named, and silently. Set but unparseable now returns empty, which
	// fails the launch and says so; only UNSET takes the fallback.
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
