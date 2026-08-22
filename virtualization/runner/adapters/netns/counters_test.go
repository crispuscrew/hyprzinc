package netns

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The flags are the whole trick, so they are asserted rather than assumed. --preserve-credentials
// is not optional: the namespace maps this user to its uid 0, and without it nsenter attempts a
// setgroups the kernel refuses. --user as well as --net, because reading nftables needs
// CAP_NET_ADMIN in the user namespace that owns the netns. Measured - the other forms fail with
// EPERM, which would read as "no ruleset" if it were not distinguished.
func TestCountersArgs_EntersBothNamespacesKeepingCredentials(t *testing.T) {
	args := countersArgs(4242)
	for _, want := range []string{"--net", "--user", "--preserve-credentials"} {
		if !slices.Contains(args, want) {
			t.Errorf("countersArgs is missing %s: %v", want, args)
		}
	}
	if !slices.Contains(args, "4242") {
		t.Errorf("the target pid is missing: %v", args)
	}
	// The same table Render writes, or the readout answers about something else.
	joined := strings.Join(args, " ")
	if !strings.HasSuffix(joined, "nft -j list table inet zinc") {
		t.Errorf("should read the zinc table as JSON: %v", args)
	}
}

// An unwrapped guest shares this process's network namespace, and reading counters there would
// answer with the host's own ruleset rather than the guest's.
func TestNamespaced_ThisProcessIsNotInOneOfItsOwn(t *testing.T) {
	own, err := Namespaced(os.Getpid())
	if err != nil {
		t.Fatalf("Namespaced: %v", err)
	}
	if own {
		t.Error("this test process shares zvr's network namespace, so it is not namespaced")
	}
}

// A pid that is not there is an error, not a quiet "unfiltered": reporting a weaker posture than
// a guest may actually have is the answer a reader would act on.
func TestNamespaced_MissingProcessIsAnError(t *testing.T) {
	if _, err := Namespaced(-1); err == nil {
		t.Error("a pid that cannot be read should be an error, not false")
	}
}
