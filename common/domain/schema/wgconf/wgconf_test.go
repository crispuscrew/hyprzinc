package wgconf

import (
	"slices"
	"strings"
	"testing"
)

const sample = `
[Interface]
PrivateKey = ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqr0123=
Address = 10.7.0.2/32, fd00::2/128
MTU = 1420
ListenPort = 51820

[Peer]
PublicKey = ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvutsrqponmlkji9876=
Endpoint = 203.0.113.7:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
`

// parseErr is Parse with the config dropped, for the error-only cases.
func parseErr(text string) error { _, err := Parse(text); return err }

// The split that makes this package exist: `wg setconf` takes only what it takes, and the
// address, routes and endpoint are read out for the interface and the routing.
func TestParse_SplitsTheFile(t *testing.T) {
	cfg, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.7.0.2/32", "fd00::2/128"}; !slices.Equal(cfg.Addresses, want) {
		t.Errorf("Addresses = %v, want %v", cfg.Addresses, want)
	}
	if want := []string{"0.0.0.0/0", "::/0"}; !slices.Equal(cfg.Routes, want) {
		t.Errorf("Routes = %v, want %v", cfg.Routes, want)
	}
	if want := []Endpoint{{Host: "203.0.113.7", Port: 51820}}; !slices.Equal(cfg.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v", cfg.Endpoints, want)
	}
	if cfg.MTU != 1420 {
		t.Errorf("MTU = %d, want 1420", cfg.MTU)
	}
	// Address and MTU are wg-quick's, not `wg setconf`'s: passing them through would make
	// the tool reject the whole file.
	for _, banned := range []string{"address", "Address", "MTU", "mtu"} {
		if strings.Contains(cfg.SetConf, banned) {
			t.Errorf("SetConf must not carry %q:\n%s", banned, cfg.SetConf)
		}
	}
	for _, wanted := range []string{"[Interface]", "privatekey =", "[Peer]", "publickey =", "allowedips =", "endpoint ="} {
		if !strings.Contains(cfg.SetConf, wanted) {
			t.Errorf("SetConf missing %q:\n%s", wanted, cfg.SetConf)
		}
	}
}

// The script hooks are refused, not ignored. The helper that would run them holds NET_ADMIN
// in the app's network namespace, and a config file is not a place to accept code from.
func TestParse_RefusesScriptHooks(t *testing.T) {
	for _, hook := range []string{"PostUp", "PreUp", "PostDown", "PreDown", "SaveConfig", "Table"} {
		text := "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n" + hook +
			" = anything\n[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n"
		err := parseErr(text)
		if err == nil {
			t.Errorf("%s must be refused", hook)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(hook)) {
			t.Errorf("%s: the refusal should name it, got %v", hook, err)
		}
	}
}

// One place to name resolvers, not two.
func TestParse_RefusesDNS(t *testing.T) {
	err := parseErr("[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\nDNS = 1.1.1.1\n" +
		"[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n")
	if err == nil || !strings.Contains(err.Error(), "DNSServers") {
		t.Fatalf("DNS should be refused and point at DNSServers, got %v", err)
	}
}

// An endpoint is pinned to a route before the namespace closes, so it must already be an
// address: a name could resolve elsewhere by then, and the encrypted packets would be sent
// into the tunnel that carries them.
func TestParse_EndpointMustBeAnAddress(t *testing.T) {
	err := parseErr("[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n" +
		"[Peer]\nPublicKey = p\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0\n")
	if err == nil || !strings.Contains(err.Error(), "not a name") {
		t.Fatalf("a hostname endpoint should be refused, got %v", err)
	}

	cfg, err := Parse("[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n" +
		"[Peer]\nPublicKey = p\nEndpoint = [2001:db8::7]:51820\nAllowedIPs = ::/0\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []Endpoint{{Host: "2001:db8::7", Port: 51820}}; !slices.Equal(cfg.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v", cfg.Endpoints, want)
	}
}

// The Endpoint host is interpolated into the tunnel-build script the netfilter helper runs
// with CAP_NET_ADMIN in the app's namespace, before the nft ruleset closes it. A .conf from
// a VPN provider is exactly the artefact this feature exists to consume, so the host has to
// be an address and nothing else. These all passed the older hand-rolled check.
func TestParse_EndpointCannotCarryShellMetacharacters(t *testing.T) {
	for _, endpoint := range []string{
		"[::$(touch /tmp/pwn)::]:51820",
		"[::`id`::]:51820",
		"[::;id;::]:51820",
		"[::a::]:51820",
		"[2001:db8::7%eth0]:51820",
	} {
		err := parseErr("[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n" +
			"[Peer]\nPublicKey = p\nEndpoint = " + endpoint + "\nAllowedIPs = 0.0.0.0/0\n")
		if err == nil {
			t.Errorf("Endpoint %q was accepted; it reaches a privileged sh -c unquoted", endpoint)
		}
	}
}

// This file decides what an app can reach, so a typo must not quietly change that.
func TestParse_RejectsTheMalformed(t *testing.T) {
	for name, text := range map[string]string{
		"unknown interface key": "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\nWhatever = 1\n[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n",
		"unknown peer key":      "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n[Peer]\nPublicKey = p\nNonsense = 1\nAllowedIPs = 0.0.0.0/0\n",
		"unknown section":       "[Nope]\nKey = 1\n",
		"no peer":               "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n",
		"no address":            "[Interface]\nPrivateKey = k\n[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n",
		"key before section":    "PrivateKey = k\n[Interface]\n",
		"not a pair":            "[Interface]\nPrivateKey\n",
		"bad mtu":               "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\nMTU = huge\n[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n",
		"endpoint with no port": "[Interface]\nPrivateKey = k\nAddress = 10.7.0.2/32\n[Peer]\nPublicKey = p\nEndpoint = 203.0.113.7\nAllowedIPs = 0.0.0.0/0\n",
	} {
		if err := parseErr(text); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Comments and blank lines are ordinary in these files, and a comment must not ride into the
// body handed to `wg setconf`.
func TestParse_CommentsAndBlanks(t *testing.T) {
	cfg, err := Parse("# my vpn\n\n[Interface]\nPrivateKey = k   # the secret\nAddress = 10.7.0.2/32\n\n" +
		"[Peer]\nPublicKey = p\nAllowedIPs = 0.0.0.0/0\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg.SetConf, "#") {
		t.Errorf("comments must not reach wg setconf:\n%s", cfg.SetConf)
	}
}
