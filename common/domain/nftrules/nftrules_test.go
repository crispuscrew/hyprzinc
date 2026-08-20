package nftrules

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

func withLists(lists ...schema.NetworkList) schema.AppConfig {
	return schema.AppConfig{
		AppNameID:   "guest",
		NetworkMeta: schema.NetworkMeta{NetworkLists: lists},
	}
}

// An app that declares nothing gets no ruleset, because it gets no network: the caller hands it
// an empty namespace, which is stronger than any rule.
func TestRender_NoListsIsNoRuleset(t *testing.T) {
	if got := Render(schema.AppConfig{AppNameID: "guest"}); got != "" {
		t.Fatalf("want no ruleset, got:\n%s", got)
	}
}

// A whitelist means default-drop, and what is not named must not be reachable.
func TestRender_WhitelistIsDefaultDrop(t *testing.T) {
	got := Render(withLists(schema.NetworkList{
		IPv4CIDR: []string{"10.0.0.0/8"},
		Ports:    []int{443},
	}))
	if !strings.Contains(got, "hook output priority 0; policy drop;") {
		t.Errorf("a whitelist must make the chain default-drop:\n%s", got)
	}
	if !strings.Contains(got, `ip daddr { 10.0.0.0/8 } tcp dport { 443 } counter accept comment "list[0]"`) {
		t.Errorf("the allowed destination is missing:\n%s", got)
	}
	// The backstop is what makes "what is my sandbox refusing" a number rather than a
	// permanently-zero policy, which nftables does not count.
	if !strings.Contains(got, `counter drop comment "default"`) {
		t.Errorf("a drop policy needs its explicit backstop:\n%s", got)
	}
}

// An all-blacklist config means allow-all-except, and the backstop must NOT be written: the same
// line would turn it into deny-all, silently inverting what the config says.
func TestRender_AllBlacklistIsAcceptWithNoBackstop(t *testing.T) {
	got := Render(withLists(schema.NetworkList{
		Blacklist: true,
		IPv4CIDR:  []string{"10.0.0.0/8"},
	}))
	if !strings.Contains(got, "hook output priority 0; policy accept;") {
		t.Errorf("an all-blacklist config must default to accept:\n%s", got)
	}
	if strings.Contains(got, `counter drop comment "default"`) {
		t.Errorf("a backstop on an accept policy inverts the config:\n%s", got)
	}
}

// One whitelist among blacklists makes the whole chain default-drop: the presence of anything
// that means "only these" cannot be weakened by the lists beside it.
func TestRender_OneWhitelistMakesItDrop(t *testing.T) {
	got := Render(withLists(
		schema.NetworkList{Blacklist: true, IPv4CIDR: []string{"10.0.0.0/8"}},
		schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}},
	))
	if !strings.Contains(got, "policy drop;") {
		t.Errorf("a whitelist anywhere must make the chain drop:\n%s", got)
	}
}

// The one that was measured against a real namespace. pasta splices a namespace's loopback to
// the host's, so accepting loopback-addressed traffic hands the guest every service the person
// running it has bound to 127.0.0.1 - which is the hole this package exists to close.
func TestRender_NeverAcceptsLoopback(t *testing.T) {
	got := Render(withLists(schema.NetworkList{
		IPv4CIDR: []string{"1.1.1.1/32"},
		Ports:    []int{443},
	}))
	for _, forbidden := range []string{`oif "lo"`, `iif "lo"`, "127.0.0.0/8", "::1"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the ruleset accepts loopback (%q), which pasta splices to the host:\n%s", forbidden, got)
		}
	}
}

// Inbound is closed with a base chain rather than by omission: nftables applies no policy to a
// hook that has no chain, so leaving it out leaves inbound unfiltered, not closed.
func TestRender_InboundIsClosedWithAChain(t *testing.T) {
	got := Render(withLists(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}}))
	if !strings.Contains(got, "hook input priority 0; policy drop;") {
		t.Errorf("inbound needs a base chain to be closed at all:\n%s", got)
	}
}

// The table is replaced rather than merged into: nft loads INTO what is already there, so a
// leftover table would keep rules that evaluate above these.
func TestRender_ReplacesTheTable(t *testing.T) {
	got := Render(withLists(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}}))
	if !strings.HasPrefix(got, "table inet zinc\ndelete table inet zinc\ntable inet zinc {") {
		t.Fatalf("the ruleset must start from nothing:\n%s", got)
	}
}

// Naming resolvers is a restriction as well as a setting: DNS to anything else is dropped, or an
// app that carries a hardcoded resolver simply ignores what the config says.
func TestRender_DNSIsARestriction(t *testing.T) {
	cfg := withLists(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}})
	cfg.NetworkMeta.DNSServers = []string{"9.9.9.9"}
	got := Render(cfg)
	if !strings.Contains(got, "ip daddr { 9.9.9.9 } udp dport { 53, 853 } accept") {
		t.Errorf("the declared resolver must be reachable:\n%s", got)
	}
	if !strings.Contains(got, `udp dport { 53, 853 } counter drop comment "dns"`) {
		t.Errorf("DNS to anything else must be dropped:\n%s", got)
	}
}

// Lists that are not self-scoped egress are not this renderer's business, and must not silently
// contribute rules: the caller's validation refuses them, and a rule emitted here would be a
// second, quieter answer.
func TestRender_IgnoresListsItDoesNotOwn(t *testing.T) {
	got := Render(withLists(
		schema.NetworkList{Ingress: true, Ports: []int{8080}},
		schema.NetworkList{Host: true, IPv4CIDR: []string{"0.0.0.0/0"}},
		schema.NetworkList{AppName: "vpn", IPv4CIDR: []string{"0.0.0.0/0"}},
	))
	if got != "" {
		t.Fatalf("only self-scoped egress lists produce rules, got:\n%s", got)
	}
}

// The label carries the list's position in the config, so a counter points back at the line that
// produced it rather than at its position in some filtered slice.
func TestRender_LabelsCarryTheConfigIndex(t *testing.T) {
	got := Render(withLists(
		schema.NetworkList{Host: true, IPv4CIDR: []string{"0.0.0.0/0"}}, // index 0, ignored
		schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}},            // index 1, rendered
	))
	if !strings.Contains(got, `comment "list[1]"`) {
		t.Errorf("the counter should name the config index it came from:\n%s", got)
	}
}

// A forward arrives in the namespace as a NEW inbound connection, so a chain that accepts only
// established traffic drops the very connection ForwardPorts exists to allow. A guest booted and
// then never answered on its published port until this was here.
func TestRender_PublishedPortsAreAcceptedInbound(t *testing.T) {
	cfg := withLists(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}})
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 2222, GuestPort: 22}}
	got := Render(cfg)
	if !strings.Contains(got, `tcp dport { 2222 } counter accept comment "published"`) {
		t.Errorf("the published port must be let in:\n%s", got)
	}
	// And nothing else is.
	if strings.Contains(got, "dport { 22 }") {
		t.Errorf("the guest-side port is not what arrives here:\n%s", got)
	}
}

// Every accept in the input chain is loopback exposure on its port. pasta splices the namespace's
// loopback to the host's, and the splice works by pasta accepting a connection inside the
// namespace, so this chain's default drop is what closes that hole - measured, by adding one
// `dport accept` here and reaching a host service. The published ports are the only accepts that
// belong, and pasta binds those on the host itself.
func TestRender_InputChainAcceptsNothingBeyondThePublishedPorts(t *testing.T) {
	cfg := withLists(schema.NetworkList{Blacklist: true, IPv4CIDR: []string{"192.0.2.0/24"}})
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 8080, GuestPort: 80}}

	_, after, found := strings.Cut(Render(cfg), "chain input {")
	if !found {
		t.Fatal("no input chain was rendered")
	}
	body, _, _ := strings.Cut(after, "\n\t}")
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "accept") ||
			strings.HasPrefix(line, "ct state established,related") ||
			strings.Contains(line, `comment "published"`) {
			continue
		}
		t.Errorf("this accept reopens the host's loopback on its port: %q", line)
	}
}
