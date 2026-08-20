package netenforce

import (
	"slices"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
)

// The readout takes the same route as the lock-down: same helper image, same pod, same one
// capability, and a fixed argv. Reading nftables is not a lesser privilege than writing it.
func TestCounters_TakesTheSamePathAsTheLockDown(t *testing.T) {
	cmd, filtered := (Enforcer{}).Counters(pastaApp(), options.HostOptions{})
	if !filtered {
		t.Fatal("a filtered app has a ruleset to read")
	}
	assertContainsSeq(t, cmd.Args, "--pod", PodName("browser"))
	assertContainsSeq(t, cmd.Args, "--cap-add", "NET_ADMIN")
	assertContainsSeq(t, cmd.Args, "--cap-drop", "all")
	assertContainsSeq(t, cmd.Args, "--pull", "never")
	assertContainsSeq(t, cmd.Args, "--user", "0")
	if tail := cmd.Args[len(cmd.Args)-4:]; !slices.Equal(tail, []string{"nft", "-j", "list", "ruleset"}) {
		t.Fatalf("the read step should end with `nft -j list ruleset`, got %v", tail)
	}
	if cmd.Stdin != "" {
		t.Errorf("nothing is piped into a read, got stdin %q", cmd.Stdin)
	}

	override, _ := (Enforcer{}).Counters(pastaApp(), options.HostOptions{NetfilterImage: "my/nft:local"})
	if !slices.Contains(override.Args, "my/nft:local") {
		t.Errorf("the read step should use the override image, got %v", override.Args)
	}
}

// An app with no NetworkLists has no netns of its own, so there is no ruleset and no counter.
// That is an answer, not a failure: the caller says so rather than running a command that
// would fail with podman's "no such pod" and read as something being broken.
func TestCounters_UnfilteredAppHasNothingToAsk(t *testing.T) {
	cmd, filtered := (Enforcer{}).Counters(schema.AppConfig{AppNameID: "solo"}, options.HostOptions{})
	if filtered {
		t.Fatalf("an unfiltered app has no ruleset to read, got %v", cmd.Args)
	}
}
