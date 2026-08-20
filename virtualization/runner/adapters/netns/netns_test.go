package netns

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

func vmWith(lists ...schema.NetworkList) schema.AppConfig {
	return schema.AppConfig{
		AppNameID:   "guest",
		Type:        schema.ZincVirtualization,
		NetworkMeta: schema.NetworkMeta{NetworkLists: lists},
	}
}

// An app that declares no lists runs exactly as it did before: no namespace, no wrapper, no
// change to the argv qemu is given.
func TestCommand_UnfilteredAppIsUntouched(t *testing.T) {
	qemu := []string{"qemu-system-x86_64", "-name", "guest"}
	argv, stdin, err := Command(vmWith(), qemu, "")
	if err != nil {
		t.Fatal(err)
	}
	if stdin != "" {
		t.Errorf("no lists means no ruleset, got: %q", stdin)
	}
	if strings.Join(argv, " ") != strings.Join(qemu, " ") {
		t.Errorf("the argv should be untouched, got: %v", argv)
	}
}

// The ordering is the guarantee: the ruleset loads and only then does qemu exec, so a guest
// never exists on an unfiltered network. `set -e` is what makes a ruleset that will not load
// stop the launch rather than boot the guest into the namespace anyway.
func TestCommand_LoadsTheRulesetBeforeQemuExecs(t *testing.T) {
	argv, stdin, err := Command(vmWith(schema.NetworkList{
		IPv4CIDR: []string{"1.1.1.1/32"},
		Ports:    []int{443},
	}), []string{"qemu-system-x86_64", "-name", "guest"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// argv[0] is what the caller execs, so it has to be the program and not its first flag.
	if argv[0] != Binary {
		t.Fatalf("argv[0] must be %q or nothing runs, got: %v", Binary, argv)
	}
	if argv[1] != "--config-net" {
		t.Errorf("the guest must run in a namespace of its own, got: %v", argv)
	}
	script := argv[len(argv)-1]
	nft := strings.Index(script, "nft -f -")
	exec := strings.Index(script, "exec ")
	switch {
	case nft < 0:
		t.Fatalf("the ruleset is never loaded: %s", script)
	case exec < 0:
		t.Fatalf("qemu is never started: %s", script)
	case nft > exec:
		t.Fatalf("qemu starts before the ruleset loads, which is the window this closes: %s", script)
	}
	if !strings.HasPrefix(script, "set -e") {
		t.Errorf("a ruleset that fails to load must stop the launch: %s", script)
	}
	if !strings.Contains(stdin, "policy drop;") {
		t.Errorf("the ruleset should be the rendered one, got: %s", stdin)
	}
}

// A path with a space in it stays one argument through the shell that loads the ruleset.
func TestCommand_QuotesTheQemuArgv(t *testing.T) {
	argv, _, err := Command(vmWith(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}}),
		[]string{"qemu-system-x86_64", "-drive", "file=/home/a b/disk.qcow2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	script := argv[len(argv)-1]
	if !strings.Contains(script, `'file=/home/a b/disk.qcow2'`) {
		t.Errorf("a path with a space must survive as one argument: %s", script)
	}
}

// qemu's own hostfwd binds inside the namespace now, where the host cannot reach it, so the
// forward has to be made by the thing that owns the namespace boundary.
func TestCommand_ForwardPortsArePublishedByTheNamespace(t *testing.T) {
	cfg := vmWith(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}})
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 2222, GuestPort: 22}}
	argv, _, err := Command(cfg, []string{"qemu-system-x86_64"}, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-t 2222") {
		t.Errorf("the published port should be forwarded into the namespace: %v", argv)
	}
}

// DNSServers was enforced and never delivered: the ruleset permits DNS to the declared server,
// but qemu's user networking takes its upstream resolver from /etc/resolv.conf, which the rules
// then drop. Measured, with a whitelist guest that resolved nothing until this bind mount.
func TestCommand_PointsQemusResolverAtTheDeclaredServers(t *testing.T) {
	cfg := vmWith(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}})
	cfg.NetworkMeta.DNSServers = []string{"1.1.1.1"}

	argv, _, err := Command(cfg, []string{"qemu-system-x86_64"}, "/run/zinc/guest.resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	script := argv[len(argv)-1]
	if !strings.Contains(script, "mount --bind '/run/zinc/guest.resolv.conf' /etc/resolv.conf") {
		t.Errorf("the declared resolver must be bound over the namespace's own:\n%s", script)
	}
	// Before qemu, or qemu reads the host's resolver and the rules drop what it sends.
	if strings.Index(script, "mount --bind") > strings.Index(script, "exec ") {
		t.Errorf("the resolver must be bound before qemu execs:\n%s", script)
	}
	if ResolvConf(cfg) != "nameserver 1.1.1.1\n" {
		t.Errorf("resolv.conf body = %q", ResolvConf(cfg))
	}
}

// An app that declares no resolver keeps the namespace's own, so the wrapper stays a ruleset
// and an exec, with nothing mounted.
func TestCommand_NoDeclaredResolverMountsNothing(t *testing.T) {
	cfg := vmWith(schema.NetworkList{IPv4CIDR: []string{"1.1.1.1/32"}})
	argv, _, err := Command(cfg, []string{"qemu-system-x86_64"}, "/run/zinc/guest.resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(argv[len(argv)-1], "mount") {
		t.Errorf("nothing was declared, so nothing should be mounted:\n%s", argv[len(argv)-1])
	}
	if ResolvConf(cfg) != "" {
		t.Errorf("no servers means no file, got %q", ResolvConf(cfg))
	}
}
