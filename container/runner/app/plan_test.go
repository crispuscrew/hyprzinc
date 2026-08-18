package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/adapters/netenforce"
	"github.com/crispuscrew/zinc/container/runner/adapters/podman"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
)

// planSvc wires the real podman runtime + the nft enforcer - but no store / builder /
// display broker - which is all Plan and (validation-only) Launch need. Plan is pure
// (AppRunArgs builds argv without I/O), so these tests run with no podman present.
func planSvc() Service {
	return New(nil, podman.Runtime{}, nil, nil, netenforce.Enforcer{}, dbusproxy.Broker{}, nil, nil, nil)
}

func baseOpts() options.HostOptions {
	return options.HostOptions{RuntimeDir: "/run/user/1000", WaylandDisplay: "wayland-1", HomeDir: "/root"}
}

// digestPin is a canonical sha256 digest (64 hex chars) used by the test fixtures so
// they satisfy the section 5.5 digest-pin rule that Plan/Launch now validate.
const digestPin = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// pastaApp is a filtered app: one self-scoped whitelist NetworkList.
func pastaApp() schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     "browser",
		ImageMeta:     schema.ImageMeta{Image: "docker.io/library/firefox" + digestPin},
		NetworkMeta: schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
			IPv4CIDR: []string{"1.1.1.1/32", "9.9.9.9/32"},
			Ports:    []int{443, 80},
		}}},
	}
}

func TestPlan_NonPasta(t *testing.T) {
	cfg := schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     "tool",
		ImageMeta:     schema.ImageMeta{Image: "img" + digestPin},
	}
	plan, err := planSvc().Plan(cfg, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Args[0] != "run" || plan[0].Stdin != "" {
		t.Fatalf("unfiltered plan should be one stdin-less run, got %+v", plan)
	}
	assertContainsSeq(t, plan[0].Args, "--network", "none")
}

func TestPlan_Pasta(t *testing.T) {
	cfg := pastaApp()
	plan, err := planSvc().Plan(cfg, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 {
		t.Fatalf("pasta plan should be three steps, got %d", len(plan))
	}
	pod := netenforce.PodName(cfg.AppNameID)

	// 1. pod create with pasta networking
	assertContainsSeq(t, plan[0].Args, "pod", "create")
	assertContainsSeq(t, plan[0].Args, "--name", pod)
	assertContainsSeq(t, plan[0].Args, "--network", "pasta")

	// 2. nft lock-down: NET_ADMIN only, joined to the pod, ruleset on stdin, local-only helper
	assertContainsSeq(t, plan[1].Args, "--pod", pod)
	assertContainsSeq(t, plan[1].Args, "--cap-add", "NET_ADMIN")
	assertContainsSeq(t, plan[1].Args, "--pull", "never")
	if plan[1].Stdin != netenforce.NFTRuleset(cfg) {
		t.Fatal("nft step must carry the ruleset on stdin")
	}
	if tail := plan[1].Args[len(plan[1].Args)-3:]; !slices.Equal(tail, []string{"nft", "-f", "-"}) {
		t.Fatalf("nft step should end with `nft -f -`, got %v", tail)
	}

	// 3. app joins the locked pod with caps dropped and NO net caps / no --network
	assertContainsSeq(t, plan[2].Args, "--pod", pod)
	assertContainsSeq(t, plan[2].Args, "--cap-drop", "all")
	mustNotContain(t, plan[2].Args, "--network")
	mustNotContain(t, plan[2].Args, "NET_ADMIN")
	if got := plan[2].Args[len(plan[2].Args)-1]; got != cfg.ImageMeta.Image {
		t.Fatalf("image must be the last app arg, got %q", got)
	}
}

// A multiterminal pasta app keeps the same lock-down ordering - pod create → nft lock
// → app - but the final step is the detached holder, so there is still no
// unfiltered-egress window.
func TestPlan_PastaMultiterminal(t *testing.T) {
	cfg := pastaApp()
	cfg.StartConditions.Terminal = true
	cfg.StartConditions.Multiterminal = true
	cfg.StartConditions.Entrypoint = "htop"
	plan, err := planSvc().Plan(cfg, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 {
		t.Fatalf("pasta plan should still be three steps, got %d", len(plan))
	}
	pod := netenforce.PodName(cfg.AppNameID)
	assertContainsSeq(t, plan[2].Args, "--pod", pod) // holder joins the locked pod
	assertContainsSeq(t, plan[2].Args, "-d", "--rm") // detached keep-alive
	mustNotContain(t, plan[2].Args, "-it")
	mustNotContain(t, plan[2].Args, "htop") // app cmd runs per-terminal, not as PID 1
	wantTail := append([]string{cfg.ImageMeta.Image}, podman.HolderCmd()...)
	if tail := plan[2].Args[len(plan[2].Args)-len(wantTail):]; !slices.Equal(tail, wantTail) {
		t.Fatalf("holder cmd must follow the image, got tail %v want %v", tail, wantTail)
	}
}

// Launch must validate before it ever touches a port - an invalid definition can't
// reach podman (here the ports are nil, so any use would panic).
func TestLaunch_InvalidConfigNeverLaunches(t *testing.T) {
	cfg := schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     "demo",
		ImageMeta:     schema.ImageMeta{Image: "alpine:latest"}, // section 5.5 violation
	}
	err := planSvc().Launch(cfg, options.HostOptions{})
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("expected validation failure before launch, got %v", err)
	}
}

func assertContainsSeq(t *testing.T, args []string, first, second string) {
	t.Helper()
	for index := 0; index+1 < len(args); index++ {
		if args[index] == first && args[index+1] == second {
			return
		}
	}
	t.Fatalf("expected adjacent %q %q in %v", first, second, args)
}

func mustNotContain(t *testing.T, args []string, bad string) {
	t.Helper()
	if slices.Contains(args, bad) {
		t.Fatalf("did not expect args to contain %q; got %v", bad, args)
	}
}
