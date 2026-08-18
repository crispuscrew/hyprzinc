package validate

import (
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// checkNetworkList validates one entry (list order = priority, first wins). Directional: Ingress=false
// is egress (Ports = destinations the app may reach), Ingress=true publishes the app's own listeners.
// Scope: Host=true is the host netns or a host-interface bind; Host=false with no AppName is this app;
// Host=false with one is a sibling.
func checkNetworkList(index int, netList schema.NetworkList, add addFunc) {
	for _, cidr := range netList.IPv4CIDR {
		if !validCIDR(cidr, false) {
			add("NetworkLists[%d].IPv4CIDR %q: not a valid IPv4 CIDR", index, cidr)
		}
	}
	for _, cidr := range netList.IPv6CIDR {
		if !validCIDR(cidr, true) {
			add("NetworkLists[%d].IPv6CIDR %q: not a valid IPv6 CIDR", index, cidr)
		}
	}
	for _, port := range netList.Ports {
		if port < 1 || port > 65535 {
			add("NetworkLists[%d].Ports %d: out of range 1-65535", index, port)
		}
	}
	if iface := netList.Interface; iface != "" && !ifaceRE.MatchString(iface) {
		add("NetworkLists[%d].Interface %q: only [A-Za-z0-9._-] allowed (no commas or spaces)", index, iface)
	}

	// Egress: a port carve-out attaches to a destination CIDR (nft `daddr ... dport ...`), so ports with
	// no CIDR emit nothing and revert to the chain's default policy - a blacklist [53,853] with no CIDR
	// silently keeps DNS open. An ingress list needs no CIDR: empty means "any source".
	if !netList.Ingress && len(netList.Ports) > 0 &&
		len(netList.IPv4CIDR) == 0 && len(netList.IPv6CIDR) == 0 && len(netList.Domains) == 0 {
		add("NetworkLists[%d].Ports %s: set without any IPv4CIDR/IPv6CIDR/Domains; an egress port rule needs destinations (use 0.0.0.0/0 and/or ::/0 for all of them)", index, joinPorts(netList.Ports))
	}

	self := !netList.Host && strings.TrimSpace(netList.AppName) == ""
	if !netList.Host && netList.AppName != "" && !nameRE.MatchString(netList.AppName) {
		add("NetworkLists[%d].AppName %q: invalid app name; allowed [a-z0-9._-], must start alphanumeric", index, netList.AppName)
	}

	// One Via list resolves ONE gateway address, from a single `getent hosts` answer, and
	// uses it for every CIDR on the list. A v6 CIDR routed via a v4 gateway is rejected by
	// `ip route` ("Nexthop has invalid gateway") and the launch aborts, so the two families
	// need a list each.
	if netList.Via && len(netList.IPv4CIDR) > 0 && len(netList.IPv6CIDR) > 0 {
		add("NetworkLists[%d]: a routed (Via) list carries both IPv4CIDR and IPv6CIDR, but one list resolves one gateway address and cannot route both families through it; use one Via list per family", index)
	}
	checkForwarding(index, netList, add)
	checkDomains(index, netList, add)
	checkRouting(index, netList, add)
	checkGateway(index, netList, self, add)
}

// checkTunnel screens the WireGuard interface Zinc builds for an app. The file's own contents
// are the runner's business - it parses them at launch, where a bad line can name its line
// number - so this checks only what the schema can: that the path is usable, and that the app
// is one a tunnel can be built for.
func checkTunnel(cfg schema.AppConfig, add addFunc) {
	tunnel := cfg.NetworkMeta.Tunnel
	if tunnel.IsZero() {
		return
	}
	path := strings.TrimSpace(tunnel.WireGuardConf)
	switch {
	case !strings.HasPrefix(path, "/"):
		// Resolved by the runner, which runs from wherever it was invoked.
		add("NetworkMeta.Tunnel.WireGuardConf %q: must be an absolute path", path)
	case hasUnsafe(path):
		add("NetworkMeta.Tunnel.WireGuardConf %q: must be a single-line path (no whitespace or control characters)", path)
	}
	if len(cfg.NetworkMeta.NetworkLists) == 0 {
		add("NetworkMeta.Tunnel: needs at least one NetworkList - an app with none runs with no network at all, so there is nothing to build a tunnel in")
	}
}

// checkForwarding screens a gateway's agreement to route for its siblings. ForwardPorts is
// the gateway's half of the bound - what it will carry - and is meaningless without the
// agreement itself, so a value there with no Forward would read as a restriction on
// forwarding this app does not do.
func checkForwarding(index int, netList schema.NetworkList, add addFunc) {
	for _, port := range netList.ForwardPorts {
		if port < 1 || port > 65535 {
			add("NetworkLists[%d].ForwardPorts %d: out of range 1-65535", index, port)
		}
	}
	if len(netList.ForwardPorts) > 0 && !netList.Forward {
		add("NetworkLists[%d].ForwardPorts: has no effect without Forward - it narrows what this app carries for its siblings, and without Forward it carries nothing", index)
	}
	if netList.Forward && !(netList.Ingress && !netList.Host && strings.TrimSpace(netList.AppName) == "") {
		add("NetworkLists[%d].Forward: belongs on this app's OWN link ingress list (Ingress, no Host, no AppName) - it is this app agreeing to route for the siblings that join its link", index)
	}
}

// domainRE is a hostname in the form the resolver will be handed: lowercase labels, no
// scheme, no port, no path, no trailing dot. Lowercase because a config that differs from
// another only in case would read as two different rules while behaving as one.
var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// checkDomains refuses the shapes where naming a domain would promise something enforcement cannot
// deliver. Enforcement is at the IP layer - a domain is resolved at launch and its addresses join the
// allowed set - which is a real allowance and a real restriction, but not hostname filtering.
func checkDomains(index int, netList schema.NetworkList, add addFunc) {
	if len(netList.Domains) == 0 {
		return
	}
	for _, domain := range netList.Domains {
		trimmed := strings.TrimSpace(domain)
		switch {
		case trimmed == "":
			add("NetworkLists[%d].Domains: must not be empty", index)
		case len(trimmed) > 253:
			add("NetworkLists[%d].Domains %q: longer than a hostname may be (253 characters)", index, trimmed)
		case !domainRE.MatchString(trimmed):
			add("NetworkLists[%d].Domains %q: must be a plain lowercase hostname - no scheme, port, path or trailing dot", index, trimmed)
		}
	}
	switch {
	case netList.Ingress:
		add("NetworkLists[%d].Domains: only an egress list can allow by name - an ingress list matches the source address of an incoming packet, which carries no name", index)
	case netList.Blacklist:
		add("NetworkLists[%d].Domains: cannot be used on a blacklist - blocking a name would mean blocking every address it is not resolved to, and the rule would read as a ban while stopping only today's addresses", index)
	case strings.TrimSpace(netList.AppName) != "":
		add("NetworkLists[%d].Domains: has no meaning on a sibling link - a link is gated by its interface and its published ports, not by destination address", index)
	case netList.Host:
		add("NetworkLists[%d].Domains: has no meaning on a host-scoped list", index)
	}
}

// checkDNS requires resolvers where the app cannot otherwise resolve anything. An app routed through a
// sibling is that case: its --internal bridge resolver answers sibling names and forwards nothing
// (measured: NXDOMAIN), so without DNSServers it meets this as every lookup failing rather than as a
// missing setting.
func checkDNS(netMeta schema.NetworkMeta, add addFunc) {
	routed := false
	for _, netList := range netMeta.NetworkLists {
		if netList.Via {
			routed = true
		}
	}
	for index, server := range netMeta.DNSServers {
		address := net.ParseIP(strings.TrimSpace(server))
		if address == nil {
			add("NetworkMeta.DNSServers[%d] %q: not a valid IP address", index, server)
			continue
		}
		// A routed app's first resolver is written into a `dnat to` rule inside `table ip
		// nat`, which is IPv4-only. An IPv6 address there makes nft refuse the whole ruleset,
		// so the launch fails with a parse error naming neither the field nor the reason.
		if index == 0 && routed && address.To4() == nil {
			add("NetworkMeta.DNSServers[0] %q: a routed app's FIRST resolver must be IPv4 - it is redirected through an IPv4 nat rule, and an IPv6 address there makes the whole ruleset fail to load", server)
		}
	}
	if len(netMeta.DNSServers) > 0 {
		return
	}
	for index, netList := range netMeta.NetworkLists {
		if netList.Via {
			add("NetworkLists[%d].Via: needs NetworkMeta.DNSServers - a routed app's link is an internal bridge whose resolver answers only sibling names, so without one it cannot resolve anything external at all", index)
			return
		}
	}
}

// checkRouting screens the two halves of routing through a sibling. Each is refused in the
// shapes where it would describe something the launch cannot do, because the whole value of
// the feature is that a client cannot reach its destinations any other way - a half-stated
// config that still runs is a config that leaks.
func checkRouting(index int, netList schema.NetworkList, add addFunc) {
	if netList.Via {
		if strings.TrimSpace(netList.AppName) == "" {
			add("NetworkLists[%d].Via: needs an AppName - routing through a sibling has to name which one", index)
		}
		if netList.Host {
			add("NetworkLists[%d].Via: cannot be host-scoped - the route goes to a sibling over their private link, not to the host", index)
		}
		if netList.Ingress {
			add("NetworkLists[%d].Via: is an egress property - an ingress list describes who reaches this app, which is not something to route", index)
		}
		if netList.Blacklist {
			add("NetworkLists[%d].Via: cannot be a blacklist - its CIDRs are the destinations to send through the sibling, and a blacklist would state the ones not to route while routing nothing", index)
		}
		if len(netList.IPv4CIDR) == 0 && len(netList.IPv6CIDR) == 0 {
			add("NetworkLists[%d].Via: needs IPv4CIDR/IPv6CIDR destinations to route (use 0.0.0.0/0 and/or ::/0 to send everything through the sibling)", index)
		}
	}
	if netList.Forward {
		// Forward belongs on the producer's own link ingress: it is this app saying that
		// siblings joining its link may route out through it.
		if !netList.Ingress || netList.Host || strings.TrimSpace(netList.AppName) != "" {
			add("NetworkLists[%d].Forward: belongs on this app's own link ingress list (Ingress: true, no Host, no AppName) - it says siblings on that link may route through this app", index)
		}
	}
}

// checkGateway validates routing gateways and gates the multi-homing they imply. A
// gateway is one next-hop per family (not a range), so it needs a reachable link
// (host/sibling) and same-family destination CIDRs to carry.
func checkGateway(index int, netList schema.NetworkList, self bool, add addFunc) {
	hasV4, hasV6 := netList.GatewayV4 != "", netList.GatewayV6 != ""
	if !hasV4 && !hasV6 {
		return
	}

	if hasV4 {
		if ip := net.ParseIP(netList.GatewayV4); ip == nil || ip.To4() == nil {
			add("NetworkLists[%d].GatewayV4 %q: not a valid IPv4 address", index, netList.GatewayV4)
		} else if len(netList.IPv4CIDR) == 0 {
			add("NetworkLists[%d].GatewayV4: set but no IPv4CIDR destinations to route through it", index)
		}
	}
	if hasV6 {
		if ip := net.ParseIP(netList.GatewayV6); ip == nil || ip.To4() != nil {
			add("NetworkLists[%d].GatewayV6 %q: not a valid IPv6 address", index, netList.GatewayV6)
		} else if len(netList.IPv6CIDR) == 0 {
			add("NetworkLists[%d].GatewayV6: set but no IPv6CIDR destinations to route through it", index)
		}
	}
	if self {
		add("NetworkLists[%d]: a gateway needs a host or sibling AppName link, not the app's own netns", index)
	}

	// Multi-homing (extra interface plus ip-rule policy routing) is not implemented; the fields are
	// schema-legal, so this is the gate.
	add("NetworkLists[%d]: routing through a gateway (multi-homing) is not supported in this build yet", index)
}

// joinPorts formats a port list for a message, e.g. []int{53, 853} -> "53, 853".
func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for idx, port := range ports {
		parts[idx] = strconv.Itoa(port)
	}
	return strings.Join(parts, ", ")
}
