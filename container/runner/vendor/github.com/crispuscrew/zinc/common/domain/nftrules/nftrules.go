// Package nftrules renders the nftables ruleset for the self-scoped egress lists an app
// declares. It is pure: a validated config in, ruleset text out, no I/O and no knowledge of what
// will load it.
//
// It lives in common because both runtimes need the same answer. A container's rules are loaded
// into the netns of its pod; a guest's are loaded into the namespace its qemu runs in. What a
// NetworkList MEANS cannot differ between the two, or the same config would contain an app
// differently depending on which runtime read it, and the network model documented for one would
// be a description of neither.
//
// The container adapter still renders the wider vocabulary itself - sibling links, routing
// through a gateway, forwarding, the DNS redirect - because none of it applies to a guest, which
// has no siblings and no pod to link to. This package is the subset they share, and the shape
// the container renderer can collapse into when that vocabulary reaches guests too.
package nftrules

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// TableName is the table both runtimes load, so `nft list table inet zinc` answers the same
// question wherever it is asked.
const TableName = "zinc"

// Render returns the ruleset for an app's egress lists, or "" when it declares none.
//
// An app with no lists gets no ruleset because it gets no network at all: the caller gives it an
// empty namespace rather than a filtered one, which is the stronger answer and needs no rules.
func Render(cfg schema.AppConfig) string {
	lists := egressLists(cfg)
	if len(lists) == 0 {
		return ""
	}
	var bld strings.Builder

	// Replace, never merge. `nft -f -` loads INTO whatever is already in the namespace, so a
	// leftover table would keep its rules and evaluate above these. create-then-delete is the
	// idempotent way to start from nothing, and the whole file is one transaction.
	fmt.Fprintf(&bld, "table inet %s\n", TableName)
	fmt.Fprintf(&bld, "delete table inet %s\n", TableName)
	fmt.Fprintf(&bld, "table inet %s {\n", TableName)

	policy := chainPolicy(lists)
	bld.WriteString("\tchain output {\n")
	fmt.Fprintf(&bld, "\t\ttype filter hook output priority 0; policy %s;\n", policy)
	// No loopback accept, in either direction. A guest needs none: it reaches the world through
	// its emulated NIC, and qemu's control sockets are unix sockets rather than TCP. What is left
	// is the conntrack accept, so replies to what the rules below allowed can come back.
	bld.WriteString("\t\tct state established,related accept\n")

	writeDNS(&bld, cfg.NetworkMeta.DNSServers)
	for _, rule := range lists {
		verdict := verdictFor(rule.list)
		label := "list[" + strconv.Itoa(rule.index) + "]"
		writeRules(&bld, "ip", rule.list.IPv4CIDR, rule.list.Ports, verdict, label)
		writeRules(&bld, "ip6", rule.list.IPv6CIDR, rule.list.Ports, verdict, label)
	}
	writeBackstop(&bld, policy)
	bld.WriteString("\t}\n")

	// Inbound is closed except for what the app published. A chain that is absent is not closed -
	// nftables applies no policy to a hook with no base chain - so it is written even when every
	// rule in it is a refusal.
	//
	// This drop is also what closes the loopback hole, which is worth knowing before adding to the
	// chain. pasta splices the namespace's loopback to the host's, and the splice works by pasta
	// ACCEPTING the connection inside the namespace, so refusing inbound is what stops it.
	// Measured: this ruleset plus one `tcp dport <p> accept` here reached a host service on p.
	// Every accept below is loopback exposure on its port; the only ones are the published ports,
	// which pasta itself binds on the host, so nothing else of the user's can be behind them.
	//
	// They have to be accepted: a forward arrives as a NEW inbound connection, so a chain taking
	// only established traffic drops the very connection ForwardPorts exists to allow.
	bld.WriteString("\tchain input {\n")
	bld.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	bld.WriteString("\t\tct state established,related accept\n")
	for _, port := range publishedPorts(cfg) {
		for _, proto := range []string{"tcp", "udp"} {
			fmt.Fprintf(&bld, "\t\t%s dport { %d } %s\n", proto, port,
				counted("accept", fmt.Sprintf("published %d %s", port, proto)))
		}
	}
	bld.WriteString("\t}\n")
	bld.WriteString("}\n")
	return bld.String()
}

// publishedPorts are the host-side ports a guest's forwards land on. They are the app's explicit
// inbound grant, and the only thing this ruleset lets in.
func publishedPorts(cfg schema.AppConfig) []int {
	ports := make([]int, 0, len(cfg.VirtualizationMeta.ForwardPorts))
	for _, forward := range cfg.VirtualizationMeta.ForwardPorts {
		if forward.HostPort > 0 {
			ports = append(ports, forward.HostPort)
		}
	}
	sort.Ints(ports)
	return ports
}

// listRule pairs a list with its position in the config, which is what a counter is labelled
// with: a number in a readout is worth reading only if it points back at the line that produced
// the rule.
type listRule struct {
	index int
	list  schema.NetworkList
}

// egressLists are the self-scoped egress lists, in the order the config states them. Anything
// else is not this package's business and the caller's validation refuses it.
func egressLists(cfg schema.AppConfig) []listRule {
	var out []listRule
	for index, list := range cfg.NetworkMeta.NetworkLists {
		if list.Ingress || list.Host || strings.TrimSpace(list.AppName) != "" {
			continue
		}
		out = append(out, listRule{index: index, list: list})
	}
	return out
}

// DefaultDrop reports whether an app's egress chain defaults to drop, which any list that is not
// a blacklist makes it. Exported because what such a guest cannot reach is worth warning about at
// authoring time, and that warning must not drift from the rule it describes.
func DefaultDrop(cfg schema.AppConfig) bool {
	lists := egressLists(cfg)
	return len(lists) > 0 && chainPolicy(lists) == "drop"
}

// chainPolicy is drop unless every list is a blacklist, which is the only shape that means
// "everything except". One whitelist among them makes the chain default-drop.
func chainPolicy(rules []listRule) string {
	for _, rule := range rules {
		if !rule.list.Blacklist {
			return "drop"
		}
	}
	return "accept"
}

func verdictFor(list schema.NetworkList) string {
	if list.Blacklist {
		return "drop"
	}
	return "accept"
}

// writeRules emits one list's addresses and ports for one family.
func writeRules(bld *strings.Builder, family string, cidrs []string, ports []int, verdict, label string) {
	if len(cidrs) == 0 {
		return
	}
	set := strings.Join(cidrs, ", ")
	if len(ports) == 0 {
		fmt.Fprintf(bld, "\t\t%s daddr { %s } %s\n", family, set, counted(verdict, label+" "+family))
		return
	}
	for _, proto := range []string{"tcp", "udp"} {
		fmt.Fprintf(bld, "\t\t%s daddr { %s } %s dport { %s } %s\n",
			family, set, proto, portList(ports), counted(verdict, label+" "+family+" "+proto))
	}
}

// writeDNS permits DNS to the declared resolvers and drops it everywhere else. The drop is the
// point: an app is free to ignore what its resolv.conf says.
func writeDNS(bld *strings.Builder, servers []string) {
	if len(servers) == 0 {
		return
	}
	set := strings.Join(servers, ", ")
	// The accepts are counted too, so "is this guest resolving at all" is a number rather than an
	// inference from the drops being zero.
	for _, proto := range []string{"udp", "tcp"} {
		fmt.Fprintf(bld, "\t\tip daddr { %s } %s dport { 53, 853 } %s\n",
			set, proto, counted("accept", "declared dns "+proto))
	}
	for _, proto := range []string{"udp", "tcp"} {
		fmt.Fprintf(bld, "\t\t%s dport { 53, 853 } %s\n", proto, counted("drop", "undeclared dns "+proto))
	}
}

// writeBackstop writes a drop policy out as an explicit rule, so the number that matters most is
// not permanently zero: nftables counts rules, not policies. Only for a drop policy - on an
// all-blacklist chain the same line would turn allow-all-except into deny-all.
func writeBackstop(bld *strings.Builder, policy string) {
	if policy != "drop" {
		return
	}
	fmt.Fprintf(bld, "\t\t%s\n", counted("drop", "default"))
}

// counted attaches a counter and the label a readout reports it under.
func counted(verdict, label string) string {
	return fmt.Sprintf("counter %s comment %q", verdict, label)
}

func portList(ports []int) string {
	out := make([]int, len(ports))
	copy(out, ports)
	sort.Ints(out)
	parts := make([]string, 0, len(out))
	for _, port := range out {
		parts = append(parts, strconv.Itoa(port))
	}
	return strings.Join(parts, ", ")
}
