// Package netns gives a guest the network model a container already has: qemu runs inside a
// network namespace of its own, with an nftables ruleset loaded before the guest starts
// (docs/architecture.md section 5.3).
//
// A VM app had no egress control at all. `-netdev user` was built unconditionally and
// ForwardPorts only added inbound entries, so every guest got unrestricted outbound plus
// whatever the host had bound on its loopback - which inverted the advice in the known-issues
// table, where a VM is offered as the stronger boundary for an untrusted GUI app. On the network
// axis a container got a fail-closed ruleset and a guest got nothing.
//
// The mechanism is pasta, which Zinc already depends on for containers. `pasta --config-net`
// makes a namespace with working connectivity and runs a command inside it as uid 0 of a user
// namespace, so nft can load a ruleset there without any privilege on the host. qemu then runs in
// that namespace: its slirp backend opens ordinary sockets, and those sockets are subject to the
// ruleset. Nothing about the guest changes, which is the point - a guest cannot be asked to
// cooperate in its own confinement.
//
// The window a container closes with `pod create` before the app starts is closed here by
// ordering inside the namespace: nft loads, and only then does qemu exec. A guest never sees an
// unfiltered network because it does not exist until the ruleset is in place.
package netns

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/nftrules"
	"github.com/crispuscrew/zinc/common/domain/schema"
)

// Binary is the tool that makes the namespace. Zinc already depends on it for containers,
// where podman uses it for rootless networking.
const Binary = "pasta"

// Applies reports whether this app asked for egress control. Without it the caller runs qemu
// directly, exactly as before, so an app that states no lists is unaffected.
func Applies(cfg schema.AppConfig) bool {
	return len(cfg.NetworkMeta.NetworkLists) > 0
}

// Command wraps a qemu argv so it runs inside a filtered namespace.
//
// The ruleset is passed on stdin rather than written to a file: it is generated per launch from
// the config, and a file would be one more thing to create, secure and remove - and one more
// thing that could be edited between being written and being read.
func Command(cfg schema.AppConfig, qemu []string, resolvConf string) (argv []string, stdin string, err error) {
	if !Applies(cfg) {
		return qemu, "", nil
	}
	ruleset := nftrules.Render(cfg)
	if strings.TrimSpace(ruleset) == "" {
		return nil, "", fmt.Errorf("%s: declares network lists but they render no rules", cfg.AppNameID)
	}
	// `set -e` so a ruleset that will not load stops the launch instead of booting a guest into
	// the unfiltered namespace that failure would leave behind.
	script := "set -e\nnft -f -\n" + resolvLine(cfg, resolvConf) + "exec " + shellJoin(qemu) + "\n"

	// The program itself first: the caller execs argv[0], so a wrapper that names only its
	// flags runs nothing at all.
	args := []string{Binary, "--config-net"}
	args = append(args, forwardFlags(cfg)...)
	args = append(args, "--", "sh", "-c", script)
	return args, ruleset, nil
}

// resolvLine points qemu's own resolver at the servers the config declared.
//
// DNSServers was enforced and never delivered: qemu's user networking takes its upstream resolver
// from /etc/resolv.conf, and the rules drop DNS to anything undeclared, so a whitelist guest
// resolved nothing. A bind mount rather than a guest-side setting, so a guest with no cloud-init
// gets it too, and it is confined because pasta's mount namespace does not propagate.
func resolvLine(cfg schema.AppConfig, resolvConf string) string {
	if resolvConf == "" || len(cfg.NetworkMeta.DNSServers) == 0 {
		return ""
	}
	return "mount --bind " + shellQuote(resolvConf) + " /etc/resolv.conf\n"
}

// ResolvConf is what Resolv should hold: the declared servers, in order.
func ResolvConf(cfg schema.AppConfig) string {
	var bld strings.Builder
	for _, server := range cfg.NetworkMeta.DNSServers {
		fmt.Fprintf(&bld, "nameserver %s\n", server)
	}
	return bld.String()
}

// forwardFlags publishes the guest's ports on the host.
//
// qemu's own hostfwd binds inside the namespace now, where the host cannot reach it, so the
// forward has to be made by the thing that owns the namespace boundary. pasta's -t takes the
// same port numbers ForwardPorts already states.
func forwardFlags(cfg schema.AppConfig) []string {
	ports := make([]string, 0, len(cfg.VirtualizationMeta.ForwardPorts))
	for _, forward := range cfg.VirtualizationMeta.ForwardPorts {
		if forward.HostPort <= 0 {
			continue
		}
		ports = append(ports, strconv.Itoa(forward.HostPort))
	}
	if len(ports) == 0 {
		return nil
	}
	return []string{"-t", strings.Join(ports, ",")}
}

// shellJoin single-quotes every word, so a path with a space in it stays one argument when the
// shell that loads the ruleset hands the argv on to qemu.
func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
