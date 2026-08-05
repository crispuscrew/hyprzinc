// Package netenforce holds the NetEnforcer adapter - the swappable egress mechanism.
// It implements ports.NetEnforcer: how the app container attaches to the network
// (RunFlags), what must happen to establish and LOCK the netns before the app starts
// (Prepare), and how to tear it down (Teardown).
//
// One mechanism ships today: an app's NetworkLists are enforced as an nftables
// allow/deny ruleset on the app's own pasta netns (a pod). An app with no
// NetworkLists gets --network none. A future mechanism - eBPF egress, a proxy
// sidecar, an external traffic controller - is one more file here implementing the
// same interface; nothing in app or the podman runtime changes (docs section 5.3, section 13).
//
// Scope (this build): self-scoped egress lists (Host=false, empty AppName), tier-3 LAN
// publishing (Ingress && Host - an nft input chain plus pod `-p` forwards), and tier-2
// sibling links (a private --internal bridge per producer, interface-gated per-port nft;
// a producer's self-scoped ingress + a consumer's egress naming its AppName). checkNetwork
// forbids mixing tier-2 with other networking, and still rejects host-scoped egress and
// gateway (multi-homing) lists before this adapter runs.
package netenforce

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Compile-time check that the enforcer satisfies ports.NetEnforcer.
var _ ports.NetEnforcer = Enforcer{}

// DefaultNetfilterImage is the locally built helper that carries nft. It runs once
// per filtered launch to lock the pod's netns before the app starts. Build it with
// `make netfilter-image`. The nft step runs it with --pull=never (see nftApplyArgs):
// the privileged helper is always the locally vetted build, never pulled from a
// registry, and a missing image fails fast with a clear error. The tag must match the
// netfilter image build (runner side).
const DefaultNetfilterImage = "localhost/zinc/netfilter:local"

// Enforcer drives an app's NetworkLists onto the network. It satisfies ports.NetEnforcer.
//
// Lookup resolves the Domains an egress list allows by name; a zero Enforcer uses the host
// resolver, which is what production wants and what every existing caller gets. It is a
// field so the resolution can be driven without a network in tests.
type Enforcer struct{ Lookup LookupFunc }

// PodName is the pod that owns a filtered app's netns.
func PodName(app string) string { return app + "-pod" }

// LinkNetwork is the private, --internal bridge that connects a producer to the siblings
// that consume it (tier 2). A producer owns zinc-link-<self>; a consumer attaches to the
// producer's. The name is deterministic so both sides agree without coordination.
func LinkNetwork(producer string) string { return "zinc-link-" + producer }

// EgressNetwork is an app's own bridge to the outside, used only when it has both sibling
// links and networking that must leave them. One per app rather than the shared default
// bridge, so two apps are never on the same L2 segment.
func EgressNetwork(app string) string { return "zinc-egress-" + app }

// egressIface is the fixed in-container name of that bridge, so the ruleset and the pod
// attach agree on it the way they do for zlink0..N.
const egressIface = "zegress0"

// linkEntry is one bridge a tier-2 app attaches to, paired with the fixed in-container
// interface name (zlink0, zlink1, ...) used both for `--network name:interface_name=` and
// for the nft rules that gate that interface (validated: podman names it exactly that).
type linkEntry struct {
	network string
	iface   string
}

// links returns the ordered bridges a tier-2 app attaches to: one per self-scoped
// ingress list (its own link, as a producer) and one per sibling it consumes (an egress
// list naming an AppName), de-duplicated. Empty for a non-tier-2 app. The slice index
// fixes each link's interface name, so the ruleset and the pod attach agree.
func links(cfg schema.AppConfig) []linkEntry {
	var out []linkEntry
	seen := map[string]bool{}
	add := func(network string) {
		if seen[network] {
			return
		}
		seen[network] = true
		out = append(out, linkEntry{network: network, iface: "zlink" + strconv.Itoa(len(out))})
	}
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		appName := strings.TrimSpace(netList.AppName)
		switch {
		case netList.Ingress && !netList.Host && appName == "":
			add(LinkNetwork(cfg.AppNameID)) // producer: own link
		case !netList.Ingress && !netList.Host && appName != "":
			add(LinkNetwork(appName)) // consumer: the producer's link
		}
	}
	return out
}

// ownLinkIface is the interface of an app's own producer link (zinc-link-<self>), or ""
// when the app is not a producer. The app's published ports are accepted on it.
func ownLinkIface(cfg schema.AppConfig) string {
	own := LinkNetwork(cfg.AppNameID)
	for _, entry := range links(cfg) {
		if entry.network == own {
			return entry.iface
		}
	}
	return ""
}

// filtered reports whether cfg needs a netns + nft: any NetworkList present. An app with
// none gets --network none. checkNetwork (app layer) has already rejected the scopes this
// build can't enforce, so every list reaching here is one the enforcer handles.
func filtered(cfg schema.AppConfig) bool {
	return len(cfg.NetworkMeta.NetworkLists) > 0
}

// RunFlags attaches the app container to the network. Filtered: join the pasta pod
// (its infra container owns networking and the nft ruleset is already in place from
// Prepare, so the app only joins the locked netns - no per-app --network, no net
// caps). Unfiltered: --network none.
func (Enforcer) RunFlags(cfg schema.AppConfig) []string {
	if filtered(cfg) {
		return []string{"--pod", PodName(cfg.AppNameID)}
	}
	return []string{"--network", "none"}
}

// Prepare returns the steps that guarantee no unfiltered window (section 5.3): ensure any tier-2
// link bridges exist, create the pod (its netns), then lock the netns with nft *before
// any app starts*. The app run itself is appended by the caller (app layer) using
// RunFlags. An unfiltered app has nothing to prepare. Link networks are created
// idempotently (--ignore) and left in place on teardown - a sibling may still use one.
func (enf Enforcer) Prepare(cfg schema.AppConfig, opt options.HostOptions) ([]ports.Command, error) {
	if !filtered(cfg) {
		return nil, nil
	}
	// Names become addresses before anything is rendered, so the ruleset is only ever built
	// from addresses and the renderer stays pure. It happens here rather than inside the
	// renderer because it is the one part of building a ruleset that can fail, and a launch
	// whose allowlist could not be resolved must not proceed with a shorter one.
	cfg, err := resolveDomains(cfg, enf.Lookup)
	if err != nil {
		return nil, err
	}
	pod := PodName(cfg.AppNameID)
	image := opt.NetfilterImage
	if image == "" {
		image = DefaultNetfilterImage
	}
	var steps []ports.Command
	if entries := links(cfg); len(entries) > 0 && needsOwnEgress(cfg) {
		// Not --internal: this is the one bridge such an app reaches the outside through.
		steps = append(steps, ports.Command{
			Args: []string{"network", "create", "--ignore", EgressNetwork(cfg.AppNameID)},
			Desc: "ensure egress network " + EgressNetwork(cfg.AppNameID),
		})
	}
	for _, entry := range links(cfg) {
		steps = append(steps, ports.Command{
			Args: []string{"network", "create", "--ignore", "--internal", entry.network},
			Desc: "ensure link network " + entry.network,
		})
	}
	steps = append(steps, ports.Command{Args: podCreateArgs(cfg, pod), Desc: "create pod (netns)"})
	// Routes first, rules second: resolving the gateway needs DNS, and the ruleset that
	// follows closes the netns. Both are done before the app starts, so the app still never
	// sees an unlocked network.
	// The tunnel goes up before the routes that may point into it, and both before the
	// ruleset - building it needs the netns open (the handshake is real traffic), and the
	// ruleset is what closes it. All of it still happens before the app exists.
	tunnelStep, err := tunnelCommand(cfg, image)
	if err != nil {
		return nil, err
	}
	if tunnelStep != nil {
		steps = append(steps, *tunnelStep)
	}
	steps = append(steps, routeCommands(cfg, image)...)
	return append(steps,
		ports.Command{Args: nftApplyArgs(pod, image), Stdin: NFTRuleset(cfg), Desc: "lock netns with nft (before app)"},
	), nil
}

// Teardown removes the pod (owns the filtered netns - app and firewall go in one
// step, no stale rule-less netns left behind), or just stops the container for an
// unfiltered app.
func (Enforcer) Teardown(cfg schema.AppConfig) []ports.Command {
	if !filtered(cfg) {
		// rm -f, not stop. A KeepAlive app runs without --rm, so `stop` leaves an Exited
		// container holding the name, and the next `podman run --name <app>` fails with
		// "name already in use" - permanently, since every later launch hits the same thing.
		// StartApp is detached, so the CLI reports success while nothing starts. --ignore
		// makes it idempotent, the same reason dbusproxy.Teardown uses it for the proxy.
		return []ports.Command{{
			Args: []string{"rm", "-f", "--ignore", cfg.AppNameID},
			Desc: "remove " + cfg.AppNameID,
		}}
	}
	steps := []ports.Command{{
		Args: []string{"pod", "rm", "-f", PodName(cfg.AppNameID)},
		Desc: "remove pod " + PodName(cfg.AppNameID),
	}}
	// The per-app egress bridge goes with it. It is this app's alone - a bridge per app is
	// what keeps apps off each other's L2 - so nothing else can be using it, and left behind
	// it would accumulate one podman network per app that ever ran. The LINK networks are
	// deliberately not removed: a sibling may still be on one.
	//
	// -f rather than --ignore, which `podman network rm` does not have (checked, not assumed
	// - the flag exists on other podman subcommands, which is how one talks oneself into it).
	// -f returns 0 for a network that is already gone, so a second teardown is not an error,
	// and its other half - removing containers still on the network - has nothing to act on
	// here, because the pod that used it was removed by the step above.
	if needsOwnEgress(cfg) && len(links(cfg)) > 0 {
		steps = append(steps, ports.Command{
			Args: []string{"network", "rm", "-f", EgressNetwork(cfg.AppNameID)},
			Desc: "remove egress network " + EgressNetwork(cfg.AppNameID),
		})
	}
	return steps
}

// viaLists returns the egress lists that route through a sibling, paired with the link
// interface their traffic leaves by. The interface comes from links(), so the route and the
// ruleset always agree on which bridge is which.
func viaLists(cfg schema.AppConfig) []viaRoute {
	byNetwork := map[string]string{}
	for _, entry := range links(cfg) {
		byNetwork[entry.network] = entry.iface
	}
	var out []viaRoute
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if !netList.Via {
			continue
		}
		gateway := strings.TrimSpace(netList.AppName)
		out = append(out, viaRoute{
			gateway: gateway,
			iface:   byNetwork[LinkNetwork(gateway)],
			cidrs:   append(append([]string{}, netList.IPv4CIDR...), netList.IPv6CIDR...),
		})
	}
	return out
}

// viaRoute is one "send these destinations through that sibling" instruction.
type viaRoute struct {
	gateway string
	iface   string
	cidrs   []string
}

// forwards reports whether this app has agreed to route for the siblings on its link.
func forwards(cfg schema.AppConfig) bool {
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if netList.Forward {
			return true
		}
	}
	return false
}

// routeCommands installs the sibling routes inside the app's netns, before the app starts.
//
// The gateway's address is resolved at this moment rather than written into the config,
// because podman assigns it and it changes when the gateway is recreated. It resolves by the
// network alias podman already gives every app on a link (its AppNameID), using the link's
// own DNS - so a config never carries an address, and a gateway that restarts on a new one
// is picked up the next time a client starts.
//
// The step runs in the same helper as the ruleset and before it, so DNS is still reachable:
// afterwards the netns is default-drop. Both run before the app, so the app still never sees
// an unlocked network.
func routeCommands(cfg schema.AppConfig, image string) []ports.Command {
	var steps []ports.Command
	routes := viaLists(cfg)
	for index, route := range routes {
		cidrs := route.cidrs
		// The redirected resolver has to be routed through the sibling too, and it is not
		// covered by the list's own CIDRs unless the author happened to include it. Without
		// this, an app routing only some destinations through a VPN sends every DNS query -
		// rewritten to the declared resolver by the nat rule - out its OWN egress in the
		// clear, while the schema and this file both say the queries travel inside the
		// tunnel. It goes on the first Via route, since the dnat sends all of them there.
		if index == 0 {
			if server := dnsRedirect(cfg); server != "" {
				cidrs = append(append([]string{}, cidrs...), hostRoute(server))
			}
		}
		if len(cidrs) == 0 || route.iface == "" {
			continue
		}
		var script strings.Builder
		// Fail on the first error: a route that did not install would leave the app sending
		// those destinations out its own egress instead - the leak this feature exists to
		// prevent - so the launch must stop rather than continue quietly.
		script.WriteString("set -e\n")
		fmt.Fprintf(&script, "gateway=$(getent hosts %s | awk '{print $1; exit}')\n", route.gateway)
		fmt.Fprintf(&script, "test -n \"$gateway\" || { echo \"cannot resolve sibling %s on its link\" >&2; exit 1; }\n", route.gateway)
		for _, cidr := range cidrs {
			// replace, not add: a re-run of a resumed launch must not fail on an existing
			// route, and the default route already exists on a bridge-attached pod.
			fmt.Fprintf(&script, "ip route replace %s via \"$gateway\" dev %s\n", cidr, route.iface)
		}
		steps = append(steps, ports.Command{
			Args: []string{
				"run", "--pod", PodName(cfg.AppNameID), "--rm", "--pull", "never",
				"--security-opt", "no-new-privileges", "--cap-drop", "all", "--cap-add", "NET_ADMIN",
				// --user 0 is root OF THE POD'S user namespace, which is what owns the netns.
				// Without it a keep-id pod runs this helper as an ordinary uid, and nft cannot
				// touch the namespace at all ("cache initialization failed: Operation not
				// permitted"). A no-op for a pod that is not keep-id, where the default already is
				// that root.
				"--user", "0",
				image, "sh", "-c", script.String(),
			},
			Desc: "route through sibling " + route.gateway,
		})
	}
	return steps
}

// hostRoute turns a bare address into the single-host CIDR `ip route` wants.
func hostRoute(address string) string {
	if strings.Contains(address, ":") {
		return address + "/128"
	}
	return address + "/32"
}

// isLinkList reports whether a list is a tier-2 sibling link: a producer's self-scoped
// ingress, or a consumer's egress naming a sibling. Mirrors app.isLinkList; the two live
// apart because the app layer decides what is allowed and this layer decides what is
// rendered.
func isLinkList(netList schema.NetworkList) bool {
	appName := strings.TrimSpace(netList.AppName)
	producer := netList.Ingress && !netList.Host && appName == ""
	consumer := !netList.Ingress && !netList.Host && appName != ""
	return producer || consumer
}

// NFTRuleset renders the nftables ruleset locked into an app's netns before it starts
// (section 5.3). Pure over the validated config.
//
// The rules that decide something carry a counter and a comment naming them, which is what
// Counters reads back; which ones, and why not all of them, is at "counters" below.
//
// One app can now be gated both ways at once, which is what routing through a sibling
// needs: the private zlink* bridges are accepted by INTERFACE, and everything else is
// gated by ADDRESS and port. Before, an app was one or the other and mixing them was
// rejected at launch, because whichever ruleset ran would have ignored the other kind of
// list entirely - the address rules of a linked app simply vanished.
//
// Chain policy comes from the NON-link lists alone. A link list is structurally a
// whitelist (validation refuses a blacklist one), so folding it into the policy decision
// would flip an app that pairs a link with an all-blacklist egress from default-accept to
// default-drop and silently deny everything the blacklist meant to leave open. With no
// non-link lists in a direction the policy stays drop, which is what a link-only app has
// always had.
func NFTRuleset(cfg schema.AppConfig) string {
	var egress, ingress []listRule
	for index, netList := range cfg.NetworkMeta.NetworkLists {
		if isLinkList(netList) {
			continue // gated by interface below, not by address
		}
		rule := listRule{index: index, netList: netList}
		if netList.Ingress {
			ingress = append(ingress, rule)
		} else {
			egress = append(egress, rule)
		}
	}
	linkEntries := links(cfg)

	var bld strings.Builder

	// Replace, never merge. `nft -f -` loads INTO the namespace's existing ruleset, so
	// without this a table that is already there keeps its rules and they evaluate ABOVE
	// everything below - which would make the ruleset a function of the config plus whatever
	// was there first, rather than of the validated config alone. The create-then-delete pair
	// is the idempotent way to say "start from nothing": `table` is a no-op when it already
	// exists, so the `delete` that follows always has something to remove and never errors on
	// a clean namespace. The whole file is one atomic transaction, so there is no window in
	// which the table is gone.
	// Both tables Zinc writes are cleared, not just the filter one: the DNS redirect lives in
	// `ip nat`, and a stale rule there would send the app's queries somewhere the config never
	// named. The netns is created fresh for this launch, so nothing else owns either table.
	bld.WriteString("table inet zinc\ndelete table inet zinc\n")
	bld.WriteString("table ip nat\ndelete table ip nat\n")
	bld.WriteString("table inet zinc {\n")

	// output (egress): where the app may connect out to.
	egressPolicy := chainPolicy(egress)
	bld.WriteString("\tchain output {\n")
	fmt.Fprintf(&bld, "\t\ttype filter hook output priority 0; policy %s;\n", egressPolicy)
	bld.WriteString("\t\toif \"lo\" accept\n")
	bld.WriteString("\t\tct state established,related accept\n")
	// DNS first, so it is decided before the broad accepts below can let a query past.
	// The declared resolvers are reachable and every other one is not, which is what stops
	// an app that carries a hardcoded resolver from stepping around them - it matters once
	// an app has direct egress of its own alongside a routed link.
	writeDNSRules(&bld, cfg.NetworkMeta.DNSServers)
	// The link bridges are accepted whole: they are private and --internal, so what rides
	// them can only reach the siblings attached to them.
	for _, entry := range linkEntries {
		fmt.Fprintf(&bld, "\t\toifname %q %s\n", entry.iface, counted("accept", "link "+entry.iface))
	}
	// The tunnel is accepted as a whole, like a link bridge and for the same reason: what
	// rides it is already bounded, here by the peers' AllowedIPs, and the app cannot reach
	// anything through it that the tunnel does not carry.
	if hasTunnel(cfg) {
		fmt.Fprintf(&bld, "\t\toifname %q %s\n", tunnelIface, counted("accept", "tunnel "+tunnelIface))
	}
	for _, rule := range egress {
		verdict := verdictFor(rule.netList)
		writeRules(&bld, "ip", rule.netList.IPv4CIDR, rule.netList.Ports, verdict, rule.label())
		writeRules(&bld, "ip6", rule.netList.IPv6CIDR, rule.netList.Ports, verdict, rule.label())
	}
	writeBackstop(&bld, egressPolicy)
	bld.WriteString("\t}\n")

	// input (ingress): who may reach the app's published ports. Emitted when the app
	// publishes to the LAN or serves siblings on its own link; without either there is no
	// input base chain at all, so ingress stays closed.
	// An input chain is emitted whenever anything could arrive: a published or sibling-facing
	// list, or ANY link at all. The last clause is not redundant - an app that only consumes a
	// sibling still sits on that shared bridge, where the producer and every other consumer
	// can reach it. Leaving the chain out does not leave inbound closed, it leaves inbound
	// UNFILTERED, because nftables applies no policy to a hook that has no base chain.
	// hasTunnel is in this condition for exactly the reason the paragraph above gives about
	// links: an app with a WireGuard tunnel sits on an interface its peer can address, so
	// leaving the chain out leaves that direction UNFILTERED rather than closed. With
	// AllowedIPs 0.0.0.0/0 the peer can reach any port on the app's tunnel address, and the
	// reply leaves through the tunnel accept in the output chain. `zc new --tunnel` authors
	// precisely this shape, so it was the common case that was open.
	own := ownLinkIface(cfg)
	if len(ingress) > 0 || own != "" || len(linkEntries) > 0 || hasTunnel(cfg) {
		ingressPolicy := chainPolicy(ingress)
		bld.WriteString("\tchain input {\n")
		fmt.Fprintf(&bld, "\t\ttype filter hook input priority 0; policy %s;\n", ingressPolicy)
		// Scoped to genuinely loopback-addressed traffic, NOT to the interface alone. pasta
		// delivers a connection forwarded from the host's loopback INTO the namespace over lo,
		// so a bare `iif "lo" accept` took those before the source rules below were ever
		// consulted - meaning a publish that named one remote /24 was in fact reachable by
		// every process on the host. Matching the addresses too leaves the app talking to
		// itself accepted, which is all this rule was ever for, and lets a spliced inbound
		// connection fall through to the rules that decide about it.
		bld.WriteString("\t\tiif \"lo\" ip saddr 127.0.0.0/8 ip daddr 127.0.0.0/8 accept\n")
		bld.WriteString("\t\tiif \"lo\" ip6 saddr ::1 ip6 daddr ::1 accept\n")
		bld.WriteString("\t\tct state established,related accept\n")
		if own != "" {
			for index, netList := range cfg.NetworkMeta.NetworkLists {
				if isLinkList(netList) && netList.Ingress && len(netList.Ports) > 0 {
					label := listRule{index: index}.label() + " link"
					for _, proto := range []string{"tcp", "udp"} {
						fmt.Fprintf(&bld, "\t\tiifname %q %s dport { %s } %s\n",
							own, proto, portList(netList.Ports), counted("accept", label+" "+proto))
					}
				}
			}
		}
		for _, rule := range ingress {
			writeIngressRules(&bld, rule, verdictFor(rule.netList))
		}
		writeBackstop(&bld, ingressPolicy)
		bld.WriteString("\t}\n")
	}

	// A forwarding app needs two more things, and neither is optional: a filter rule for
	// traffic it routes on behalf of siblings (the forward hook is a separate chain from
	// output, so the egress rules above never see it), and NAT out of its own bridge, or
	// replies would be addressed to a private link address the outside cannot route back to.
	if forwards(cfg) {
		bld.WriteString("\tchain forward {\n")
		bld.WriteString("\t\ttype filter hook forward priority 0; policy drop;\n")
		bld.WriteString("\t\tct state established,related accept\n")
		writeForwardRules(&bld, cfg)
		writeBackstop(&bld, "drop")
		bld.WriteString("\t}\n")
	}
	bld.WriteString("}\n")

	if forwards(cfg) || dnsRedirect(cfg) != "" {
		bld.WriteString("table ip nat {\n")
		if forwards(cfg) {
			bld.WriteString("\tchain postrouting {\n")
			bld.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
			// One per interface forwarded traffic can leave by, not just this app's own
			// bridge: a gateway that is itself routed through another sibling sends what it
			// forwards out that link, and without NAT there the upstream would see a private
			// address on a bridge it is not attached to.
			for _, iface := range forwardExits(cfg) {
				fmt.Fprintf(&bld, "\t\toifname %q masquerade\n", iface)
			}
			bld.WriteString("\t}\n")
		}
		// A routed app's resolver is not ours to choose: podman writes resolv.conf and
		// points it at the network's own DNS, which on an --internal bridge answers sibling
		// names and forwards nothing (measured: an external name returns NXDOMAIN). Rather
		// than fight over the file, the query is redirected here, to a resolver the app
		// reaches through its sibling - so it travels inside the tunnel and stops with it.
		// dstnat runs before the filter hook, so the rules above then see the new address.
		if server := dnsRedirect(cfg); server != "" {
			bld.WriteString("\tchain output {\n")
			bld.WriteString("\t\ttype nat hook output priority dstnat; policy accept;\n")
			for _, proto := range []string{"udp", "tcp"} {
				fmt.Fprintf(&bld, "\t\t%s dport { 53, 853 } dnat to %s\n", proto, server)
			}
			bld.WriteString("\t}\n")
		}
		bld.WriteString("}\n")
	}
	return bld.String()
}

// writeForwardRules emits what a gateway will carry for its siblings: from its own link, out
// to each interface its routes can send that traffic by, narrowed to ForwardPorts.
//
// The two ends of the bound answer different questions and come from different apps. WHERE
// is the client's: only the CIDRs its Via list names are routed to this gateway at all, and
// the client cannot change that - the runner installs those routes and the app has no
// capability to alter them. WHAT is the gateway's, and it is the ports below. This app's own
// egress rules are deliberately not consulted: they say where THIS app may go, and forwarded
// traffic is somebody else's.
func writeForwardRules(bld *strings.Builder, cfg schema.AppConfig) {
	own := ownLinkIface(cfg)
	if own == "" {
		return // nothing arrives to forward
	}
	ports := forwardPorts(cfg)
	for _, exit := range forwardExits(cfg) {
		if len(ports) == 0 {
			fmt.Fprintf(bld, "\t\tiifname %q oifname %q accept\n", own, exit)
			continue
		}
		for _, proto := range []string{"tcp", "udp"} {
			fmt.Fprintf(bld, "\t\tiifname %q oifname %q %s dport { %s } accept\n",
				own, exit, proto, portList(ports))
		}
	}
}

// forwardPorts is what a gateway agreed to carry, or nil for any port.
func forwardPorts(cfg schema.AppConfig) []int {
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if netList.Forward {
			return netList.ForwardPorts
		}
	}
	return nil
}

// forwardExits lists the interfaces forwarded traffic may leave by: this app's own egress
// bridge, plus every link it routes through as a client of another gateway. The second kind
// is what lets gateways chain - a hop that passes its clients' traffic onward into a sibling
// rather than out to the network itself. Without it the forward chain would name only the
// egress bridge and drop everything the gateway's own Via routes sent to a link.
func forwardExits(cfg schema.AppConfig) []string {
	var exits []string
	if hasTunnel(cfg) {
		// First: a gateway with a tunnel exists to send its clients into it, and a rule list
		// is read in order.
		exits = append(exits, tunnelIface)
	}
	if needsOwnEgress(cfg) {
		exits = append(exits, egressIface)
	}
	own := LinkNetwork(cfg.AppNameID)
	byNetwork := map[string]string{}
	for _, entry := range links(cfg) {
		byNetwork[entry.network] = entry.iface
	}
	for _, route := range viaLists(cfg) {
		iface := byNetwork[LinkNetwork(route.gateway)]
		if iface == "" || LinkNetwork(route.gateway) == own || slices.Contains(exits, iface) {
			continue
		}
		exits = append(exits, iface)
	}
	return exits
}

// dnsRedirect returns the resolver a routed app's DNS is rewritten to, or "" when the app
// is not routed. Only a routed app: for an ordinary one the network's resolver works and is
// the only thing that knows its siblings' names, so redirecting would take that away for
// nothing. A routed app has already lost it - that resolver cannot answer anything external
// from an internal bridge - which is why this is a repair rather than a restriction.
//
// The first declared server: validation requires a routed app to name one, and one address
// is what a dnat rule takes.
func dnsRedirect(cfg schema.AppConfig) string {
	if len(cfg.NetworkMeta.DNSServers) == 0 {
		return ""
	}
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if netList.Via {
			return strings.TrimSpace(cfg.NetworkMeta.DNSServers[0])
		}
	}
	return ""
}

// writeDNSRules permits DNS to the declared resolvers and drops it everywhere else. The
// drop is the point: without it, naming a resolver would be a suggestion rather than a
// restriction, and an app is free to ignore what its /etc/resolv.conf says.
//
// Emitted only when the app declares resolvers - an app that names none keeps whatever DNS
// its network gives it, exactly as before.
func writeDNSRules(bld *strings.Builder, servers []string) {
	if len(servers) == 0 {
		return
	}
	var v4, v6 []string
	for _, server := range servers {
		address := strings.TrimSpace(server)
		if strings.Contains(address, ":") {
			v6 = append(v6, address)
		} else {
			v4 = append(v4, address)
		}
	}
	for _, family := range []struct {
		name      string
		addresses []string
	}{{"ip", v4}, {"ip6", v6}} {
		if len(family.addresses) == 0 {
			continue
		}
		for _, proto := range []string{"udp", "tcp"} {
			fmt.Fprintf(bld, "\t\t%s daddr { %s } %s dport { 53, 853 } accept\n",
				family.name, strings.Join(family.addresses, ", "), proto)
		}
	}
	// Counted, unlike the accepts above it: a non-zero number here is an app resolving
	// somewhere it was not given, which is the one thing this pair of rules exists to catch.
	for _, proto := range []string{"udp", "tcp"} {
		fmt.Fprintf(bld, "\t\t%s dport { 53, 853 } %s\n", proto, counted("drop", "undeclared dns "+proto))
	}
}

// verdictFor is the terminal verdict a list contributes: a whitelist accepts its
// matches, a blacklist drops them.
func verdictFor(netList schema.NetworkList) string {
	if netList.Blacklist {
		return "drop"
	}
	return "accept"
}

// listRule pairs a list with its position in NetworkMeta.NetworkLists. The index travels
// this far only because it is what a counter is labelled with, and a number in the readout
// is worth reading only if it points back at the line of config that produced the rule. The
// position in the filtered egress/ingress slice is not that line: link lists are dropped on
// the way here, so the two disagree the moment an app has one.
type listRule struct {
	index   int
	netList schema.NetworkList
}

// label is how this list's rules are named in the counter readout.
func (rule listRule) label() string { return "list[" + strconv.Itoa(rule.index) + "]" }

// chainPolicy is the default policy for one direction's lists: default-accept only when
// there is at least one list and every one is a blacklist (allow-all-except); otherwise
// default-drop. An empty direction is default-drop (closed).
func chainPolicy(rules []listRule) string {
	if len(rules) > 0 && allBlacklist(rules) {
		return "accept"
	}
	return "drop"
}

// allBlacklist reports whether every list is a blacklist. A single whitelist present
// returns false, so the direction is restrictive (default-drop) and the blacklist lists
// become high-priority deny carve-outs above the whitelist's accepts.
func allBlacklist(rules []listRule) bool {
	for _, rule := range rules {
		if !rule.netList.Blacklist {
			return false
		}
	}
	return true
}

// --- counters (section 5.3) ---
//
// Every rule that answers a policy question carries a `counter` statement and a comment
// naming it, so `zcr net <app>` can report what the ruleset has actually seen without
// rebuilding nft syntax out of `nft -j` output. The comment is the label; the chain and the
// verdict the readout shows come from the JSON itself.
//
// What is counted is a deliberate subset, because a counter on every rule buries the two
// numbers worth reading. `oif "lo"` (the app talking to itself) and `ct state
// established,related` (every packet of every flow already allowed) are between them almost
// all the traffic and say nothing about policy, so both stay bare. So do the DNS accepts to
// the resolvers the app declared, the forward chain's accepts (that traffic is a sibling's,
// and the sibling's own counters already show it leaving) and the nat rules (a rewrite is
// not a decision to allow or refuse). What is left is exactly the decisions:
//
//   - every rule a NetworkList produced, labelled with that list's index in the config, so
//     "which of my rules is actually being used" has an answer pointing at a line;
//   - the DNS deny, which is how an app carrying a hardcoded resolver announces itself;
//   - the default-drop backstop below, which is the whole "what is my sandbox refusing".
//
// One consequence is worth knowing before reading a number: the conntrack accept sits above
// all of these and is bare, so an accept counter counts the packets that OPENED flows, not
// the traffic those flows carried. That is the more useful reading ("was this rule
// exercised, and how often"), and it is why the byte column is small.

// labelPolicy names the trailing rule that makes a chain's default policy countable.
const labelPolicy = "default policy"

// counted renders the tail of a counted rule: the counter, the verdict, and the comment
// that names the rule. Statement order matters - a counter placed ahead of the matches
// would count every packet that reached the rule rather than the ones it acts on.
func counted(verdict, label string) string {
	return fmt.Sprintf("counter %s comment %q", verdict, label)
}

// writeBackstop writes a default-drop policy out as an explicit rule, so the number that
// matters most - what the sandbox is actually refusing - is not permanently zero. nftables
// counts rules, not policies, and a fail-closed chain refuses by policy, so without this a
// working lock-down and a broken one both report nothing. The rule changes no behaviour: a
// packet that reaches the end of the chain was dropped by the policy anyway.
//
// Only for a drop policy. On an all-blacklist (default-accept) chain the same line would
// turn allow-all-except into deny-all - the config silently inverted, which is worse than
// having no counter at all.
func writeBackstop(bld *strings.Builder, policy string) {
	if policy != "drop" {
		return
	}
	fmt.Fprintf(bld, "\t\t%s\n", counted("drop", labelPolicy))
}

// writeRules emits the verdict rules for one address family. No CIDRs → nothing.
// Ports listed → only those ports (tcp+udp); otherwise all ports to the listed CIDRs.
// label is the list's name in the readout; the family and protocol this rule matches are
// appended to it, so each of the (up to four) rules one list produces counts separately.
func writeRules(bld *strings.Builder, family string, cidrs []string, ports []int, verdict, label string) {
	if len(cidrs) == 0 {
		return
	}
	daddr := family + " daddr { " + strings.Join(cidrs, ", ") + " }"
	if len(ports) == 0 {
		fmt.Fprintf(bld, "\t\t%s %s\n", daddr, counted(verdict, label+" "+family))
		return
	}
	portsList := portList(ports)
	for _, proto := range []string{"tcp", "udp"} {
		fmt.Fprintf(bld, "\t\t%s %s dport { %s } %s\n",
			daddr, proto, portsList, counted(verdict, label+" "+family+" "+proto))
	}
}

// writeIngressRules emits input-chain rules for one ingress list: match the app's own
// published Ports, restricted to the source CIDRs (saddr). Unlike egress, an empty CIDR
// is legal and means "any source" (validate exempts ingress from the ports-need-CIDR
// rule), so a list with ports but no CIDR opens those ports to anyone the pod forwards.
// v4 and v6 source sets are emitted separately so a v4 CIDR never gates v6 traffic.
func writeIngressRules(bld *strings.Builder, rule listRule, verdict string) {
	netList := rule.netList
	ports := portList(netList.Ports)
	emit := func(saddr, family string) {
		label := rule.label()
		if family != "" {
			label += " " + family
		}
		switch {
		case ports == "" && saddr == "":
			return // nothing to match
		case ports == "":
			fmt.Fprintf(bld, "\t\t%s %s\n", saddr, counted(verdict, label))
		case saddr == "":
			for _, proto := range []string{"tcp", "udp"} {
				fmt.Fprintf(bld, "\t\t%s dport { %s } %s\n", proto, ports, counted(verdict, label+" "+proto))
			}
		default:
			for _, proto := range []string{"tcp", "udp"} {
				fmt.Fprintf(bld, "\t\t%s %s dport { %s } %s\n", saddr, proto, ports, counted(verdict, label+" "+proto))
			}
		}
	}
	hasV4, hasV6 := len(netList.IPv4CIDR) > 0, len(netList.IPv6CIDR) > 0
	if !hasV4 && !hasV6 {
		emit("", "") // any source
		return
	}
	if hasV4 {
		emit("ip saddr { "+strings.Join(netList.IPv4CIDR, ", ")+" }", "ip")
	}
	if hasV6 {
		emit("ip6 saddr { "+strings.Join(netList.IPv6CIDR, ", ")+" }", "ip6")
	}
}

func portList(ports []int) string {
	strs := make([]string, len(ports))
	for idx, port := range ports {
		strs[idx] = strconv.Itoa(port)
	}
	return strings.Join(strs, ", ")
}

// podCreateArgs builds `podman pod create` for a filtered app's netns. A tier-2 app
// attaches to its private link bridge(s), each pinned to a fixed interface name the nft
// rules match (no pasta, no host publish - checkNetwork forbids mixing). Otherwise the
// pod is a pasta netns: a list naming a host interface makes pasta copy its addressing
// (first wins), which also scopes tier-3 publishing, and tier-3 (LAN) ingress lists add
// their ports as `-p` forwards here (pod ports live on the pod, not the container).
func podCreateArgs(cfg schema.AppConfig, pod string) []string {
	args := []string{"pod", "create", "--name", pod}
	// The pod owns the user namespace of everything that joins it: podman refuses --userns on
	// a container joining a pod, so an app that asked to keep its uid gets it here or not at
	// all. Before this it got the flag on the container, and the launch failed with nothing
	// said - StartApp is detached, so podman's refusal went nowhere.
	if cfg.InternalUserMeta.KeepUserID {
		args = append(args, "--userns=keep-id")
	}
	entries := links(cfg)
	// The declared resolvers are handed to every filtered app, not only a linked one. The
	// ruleset restricts DNS to them for ALL of them, so an app that was restricted and never
	// given them keeps podman's own resolver in resolv.conf and has no working DNS at all -
	// fail-closed, but only because the setting half-worked.
	args = append(args, dnsArgs(cfg)...)
	if len(entries) == 0 {
		netspec := "pasta"
		if iface := firstInterface(cfg); iface != "" {
			netspec = "pasta:--interface," + iface
		}
		args = append(args, "--network", netspec)
		return append(args, publishArgs(cfg)...)
	}

	// A linked app needs its own egress too when it has non-link lists - a gateway app has
	// to reach the outside to be worth routing through, and a client that sends only some
	// destinations through a sibling still goes direct with the rest. pasta cannot do this:
	// podman refuses outright ("cannot set multiple networks without bridge network mode"),
	// so such an app is put on a bridge instead.
	//
	// Its OWN bridge, not the default one. Apps sharing a bridge can reach each other over
	// L2, which would leave isolation resting on the nft rules alone - and an app whose
	// egress list is an all-blacklist runs default-accept, so it would reach every other
	// app on that bridge. A bridge per app keeps them apart whatever their rules say.
	if needsOwnEgress(cfg) {
		args = append(args, "--network", EgressNetwork(cfg.AppNameID)+":interface_name="+egressIface)
	}
	if forwards(cfg) {
		// Set here because a container cannot set it itself: /proc/sys is read-only in the
		// namespace, so an app that agreed to route for its siblings would silently drop
		// every packet it was meant to forward.
		args = append(args, "--sysctl", "net.ipv4.ip_forward=1", "--sysctl", "net.ipv6.conf.all.forwarding=1")
	}
	// alias=<AppNameID>: podman resolves the network alias but NOT the pod's app
	// container name, so this makes each app reachable on the link at its AppNameID
	// (a consumer connects to "<producer>:<port>") instead of the pod name.
	for _, entry := range entries {
		args = append(args, "--network", entry.network+":interface_name="+entry.iface+",alias="+cfg.AppNameID)
	}
	return append(args, publishArgs(cfg)...)
}

// dnsArgs hands the app its declared resolvers. For a linked app these replace the resolver
// podman would put on its link, which on an --internal bridge answers sibling names and
// forwards nothing; for any other filtered app they are simply the resolvers its ruleset
// already restricts it to.
func dnsArgs(cfg schema.AppConfig) []string {
	var args []string
	for _, server := range cfg.NetworkMeta.DNSServers {
		args = append(args, "--dns", strings.TrimSpace(server))
	}
	return args
}

// needsOwnEgress reports whether a linked app also carries networking that has to leave the
// private bridges - any list that is not itself a link.
func needsOwnEgress(cfg schema.AppConfig) bool {
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if !isLinkList(netList) {
			return true
		}
	}
	return false
}

// publishArgs maps tier-3 (LAN) ingress lists - Ingress && Host - onto pod `-p` port
// forwards so the LAN can reach the app's published ports; the nft input chain then
// restricts who (source CIDR) actually gets through, and pasta binds the pod's interface
// (firstInterface). Each port is forwarded for both tcp and udp to match the input
// chain; there is no host-port remap (published port == container port). Self-scoped
// ingress (tier 2) publishes nothing to the host and never reaches here - checkNetwork
// rejects it in this build.
func publishArgs(cfg schema.AppConfig) []string {
	var args []string
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if !netList.Ingress || !netList.Host {
			continue
		}
		for _, port := range netList.Ports {
			mapping := strconv.Itoa(port) + ":" + strconv.Itoa(port)
			args = append(args, "-p", mapping+"/tcp", "-p", mapping+"/udp")
		}
	}
	return args
}

// firstInterface returns the first non-blank Interface across the app's NetworkLists.
func firstInterface(cfg schema.AppConfig) string {
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if iface := strings.TrimSpace(netList.Interface); iface != "" {
			return iface
		}
	}
	return ""
}

// nftApplyArgs builds the one-shot init `podman run` that loads the ruleset into the
// pod's netns. It carries only NET_ADMIN - namespaced to the pod's userns, so
// harmless on the host (section 5.3) - reads the ruleset from stdin, and exits.
func nftApplyArgs(pod, image string) []string {
	return []string{
		"run", "--pod", pod, "--rm", "-i", "--pull", "never",
		"--security-opt", "no-new-privileges", "--cap-drop", "all", "--cap-add", "NET_ADMIN",
		// --user 0 is root OF THE POD'S user namespace, which is what owns the netns.
		// Without it a keep-id pod runs this helper as an ordinary uid, and nft cannot
		// touch the namespace at all ("cache initialization failed: Operation not
		// permitted"). A no-op for a pod that is not keep-id, where the default already is
		// that root.
		"--user", "0",
		image, "nft", "-f", "-",
	}
}
