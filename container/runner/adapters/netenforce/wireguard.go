package netenforce

import (
	"fmt"
	"os"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/wgconf"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// tunnelIface is the name of the interface Zinc builds. One per app, so it needs no
// disambiguating: the app has its own network namespace and nothing else is in it.
const tunnelIface = "wg0"

// tunnelCommand returns the step that creates and configures the app's WireGuard interface
// inside its netns, or nil when the app asked for no tunnel.
//
// It runs in the same privileged helper as the ruleset, with NET_ADMIN and nothing else, and
// it exits before the app is started. That is the point of the whole feature: a tunnel needs
// NET_ADMIN to exist, an app with NetworkLists may never hold NET_ADMIN (it could flush the
// ruleset that contains it), and so before this a gateway could only ever be a NAT hop. The
// capability lives in a container that is gone by the time the app exists.
//
// The private key travels on STDIN. Not in the argv, which every process on the host can
// read out of /proc; not in the image; not in a mount the app could open. The same channel
// the nft ruleset already uses.
func tunnelCommand(cfg schema.AppConfig, image string) (*ports.Command, error) {
	if cfg.NetworkMeta.Tunnel.IsZero() {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.NetworkMeta.Tunnel.WireGuardConf)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: reading WireGuardConf: %w", cfg.AppNameID, err)
	}
	conf, err := wgconf.Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", cfg.AppNameID, path, err)
	}

	var script strings.Builder
	// Fail on the first error. A half-built tunnel is the worst outcome available here: the
	// app would start with an interface that exists and carries nothing, and its traffic
	// would take whatever route was left rather than the one it asked for.
	script.WriteString("set -e\n")
	fmt.Fprintf(&script, "ip link add %s type wireguard\n", tunnelIface)
	// /dev/stdin, so the key is never a file on disk in the container either.
	fmt.Fprintf(&script, "wg setconf %s /dev/stdin\n", tunnelIface)
	for _, address := range conf.Addresses {
		fmt.Fprintf(&script, "ip address add %s dev %s\n", address, tunnelIface)
	}
	if conf.MTU > 0 {
		fmt.Fprintf(&script, "ip link set mtu %d dev %s\n", conf.MTU, tunnelIface)
	}
	fmt.Fprintf(&script, "ip link set %s up\n", tunnelIface)

	// The endpoint must NOT be routed through the tunnel: the encrypted packets carrying the
	// tunnel would be sent into it. Pin each one to whatever the netns already routes by,
	// captured before the routes below replace it.
	if len(conf.Endpoints) > 0 {
		script.WriteString("existing=$(ip route show default | awk '{print $3; exit}')\n")
		for _, endpoint := range conf.Endpoints {
			fmt.Fprintf(&script,
				"[ -n \"$existing\" ] && ip route replace %s via \"$existing\" || true\n",
				hostRoute(endpoint.Host))
		}
	}
	// The peers' AllowedIPs are what the tunnel carries, so they are what is routed into it.
	for _, route := range conf.Routes {
		if route == "0.0.0.0/0" {
			script.WriteString("ip route replace default dev " + tunnelIface + "\n")
			continue
		}
		if route == "::/0" {
			script.WriteString("ip -6 route replace default dev " + tunnelIface + "\n")
			continue
		}
		fmt.Fprintf(&script, "ip route replace %s dev %s\n", route, tunnelIface)
	}

	return &ports.Command{
		Args: []string{
			"run", "--pod", PodName(cfg.AppNameID), "--rm", "-i", "--pull", "never",
			"--security-opt", "no-new-privileges", "--cap-drop", "all", "--cap-add", "NET_ADMIN",
			// --user 0 is root OF THE POD'S user namespace, which is what owns the netns.
			// Without it a keep-id pod runs this helper as an ordinary uid, and nft cannot
			// touch the namespace at all ("cache initialization failed: Operation not
			// permitted"). A no-op for a pod that is not keep-id, where the default already is
			// that root.
			"--user", "0",
			image, "sh", "-c", script.String(),
		},
		Stdin: conf.SetConf,
		Desc:  "build the wireguard tunnel (before app)",
	}, nil
}

func hasTunnel(cfg schema.AppConfig) bool { return !cfg.NetworkMeta.Tunnel.IsZero() }
