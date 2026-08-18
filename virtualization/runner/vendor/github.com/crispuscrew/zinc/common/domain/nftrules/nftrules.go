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
	// No loopback accept, in either direction, and that is deliberate rather than an omission.
	//
	// pasta SPLICES a namespace's loopback to the host's: a connection to 127.0.0.1 inside the
	// namespace is delivered to 127.0.0.1 on the host. Measured, with this very ruleset - a rule
	// accepting loopback-addressed egress handed the guest a service bound to the host's
	// loopback, which is the hole this whole package exists to close. Scoping by address does
	// not help, because the address genuinely is 127.0.0.1 at both ends.
	//
	// A guest needs none of it: it reaches the world through its own emulated NIC, and qemu's
	// control sockets are unix sockets rather than TCP. What is left is the conntrack accept, so
	// the replies to what the rules below allowed can come back.
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

	// Inbound is closed outright. A guest publishes through its runtime's own forwarding rather
	// than by listening on this namespace, and a chain that is absent is not closed - nftables
	// applies no policy to a hook with no base chain - so the chain is written even though every
	// rule in it is a refusal.
	bld.WriteString("\tchain input {\n")
	bld.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	bld.WriteString("\t\tct state established,related accept\n")
	bld.WriteString("\t}\n")
	bld.WriteString("}\n")
	return bld.String()
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
		fmt.Fprintf(bld, "\t\t%s daddr { %s } %s\n", family, set, counted(verdict, label))
		return
	}
	for _, proto := range []string{"tcp", "udp"} {
		fmt.Fprintf(bld, "\t\t%s daddr { %s } %s dport { %s } %s\n",
			family, set, proto, portList(ports), counted(verdict, label))
	}
}

// writeDNS permits DNS to the declared resolvers and drops it everywhere else. The drop is the
// point: an app is free to ignore what its resolv.conf says.
func writeDNS(bld *strings.Builder, servers []string) {
	if len(servers) == 0 {
		return
	}
	set := strings.Join(servers, ", ")
	for _, proto := range []string{"udp", "tcp"} {
		fmt.Fprintf(bld, "\t\tip daddr { %s } %s dport { 53, 853 } accept\n", set, proto)
	}
	for _, proto := range []string{"udp", "tcp"} {
		fmt.Fprintf(bld, "\t\t%s dport { 53, 853 } %s\n", proto, counted("drop", "dns"))
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
