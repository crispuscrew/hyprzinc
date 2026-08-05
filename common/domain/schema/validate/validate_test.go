package validate

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// A newline in an Install step would break out of the derived-image RUN line and let a
// crafted config inject its own Containerfile directives (a second FROM that swaps the
// base to an unpinned image), defeating the digest pin. It must be rejected.
func TestInstallControlCharRejected(t *testing.T) {
	cfg := baseCfg()
	cfg.ImageMeta.Install = []string{"true\nFROM docker.io/attacker/x:latest\nRUN :"}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("Install with a newline: want a control-characters error, got: %v", err)
	}
}

// A clean multi-step install (spaces allowed, one step per entry) passes.
func TestInstallCleanOK(t *testing.T) {
	cfg := baseCfg()
	cfg.ImageMeta.Install = []string{"apk add --no-cache firefox", "adduser -D app"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("clean multi-step Install: want nil, got: %v", err)
	}
}

// A DependsOn name is joined into a store path, so a "../.." value could read an app
// definition from outside the apps directory. It must be charset-checked like AppNameID.
func TestDependsOnTraversalRejected(t *testing.T) {
	cfg := baseCfg()
	cfg.StartConditions.DependsOn = []string{"../../../../etc/evil"}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "DependsOn") {
		t.Fatalf("DependsOn with a traversal path: want a DependsOn error, got: %v", err)
	}
}

func TestDependsOnValidOK(t *testing.T) {
	cfg := baseCfg()
	cfg.StartConditions.DependsOn = []string{"vpn", "db-1"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid DependsOn names: want nil, got: %v", err)
	}
}

// A filtered app (one with NetworkLists) runs in the pod netns that carries the nft
// egress lock-down; granting NET_ADMIN (or the superset SYS_ADMIN) would let it flush
// the ruleset and escape the filter. Both bare and CAP_-prefixed forms are rejected.
func TestNetworkCapabilityOnFilteredAppRejected(t *testing.T) {
	for _, capability := range []string{"NET_ADMIN", "CAP_SYS_ADMIN"} {
		cfg := withList(schema.NetworkList{Ingress: true, Ports: []int{5432}})
		cfg.Capabilities = []string{capability}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "escape the network filter") {
			t.Fatalf("%s on a filtered app: want an egress-escape error, got: %v", capability, err)
		}
	}
}

// An isolated app (no NetworkLists) runs with --network none, so NET_ADMIN reaches
// nothing; it is not rejected.
func TestNetworkCapabilityOnIsolatedAppOK(t *testing.T) {
	cfg := baseCfg()
	cfg.Capabilities = []string{"NET_ADMIN"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("NET_ADMIN on an isolated app: want nil, got: %v", err)
	}
}

// A benign capability on a filtered app is fine.
func TestBenignCapabilityOnFilteredAppOK(t *testing.T) {
	cfg := withList(schema.NetworkList{Ingress: true, Ports: []int{5432}})
	cfg.Capabilities = []string{"NET_BIND_SERVICE"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("benign cap on a filtered app: want nil, got: %v", err)
	}
}

// Swap is granted on top of the memory limit, because podman only takes the total of the
// two. On its own there is nothing to add it to, and the runtime would either drop it or
// hand podman a figure that caps the app far below what it asked for.
func TestResources_SwapNeedsAMemoryLimit(t *testing.T) {
	cfg := baseCfg()
	cfg.ResourcesMeta.MaxSwapMiB = 512
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "MaxSwapMiB") {
		t.Fatalf("swap without a memory limit: want a MaxSwapMiB error, got: %v", err)
	}

	cfg.ResourcesMeta.MaxRamMiB = 2048
	if err := Validate(cfg); err != nil {
		t.Fatalf("swap alongside a memory limit should pass, got: %v", err)
	}
}

// The two halves of the user setting have to agree. Each on its own describes something the
// launch will not do, which is exactly the failure these fields had for five releases.
func TestInternalUser_BothHalvesOrNeither(t *testing.T) {
	asking := baseCfg()
	asking.InternalUserMeta.UseNonRootUser = true
	err := Validate(asking)
	if err == nil || !strings.Contains(err.Error(), "NonRootUserName") {
		t.Fatalf("UseNonRootUser with no name: want a NonRootUserName error, got: %v", err)
	}

	named := baseCfg()
	named.InternalUserMeta.NonRootUserName = "app"
	err = Validate(named)
	if err == nil || !strings.Contains(err.Error(), "no effect") {
		t.Fatalf("a name without UseNonRootUser: want a no-effect error, got: %v", err)
	}

	both := baseCfg()
	both.InternalUserMeta.UseNonRootUser = true
	both.InternalUserMeta.NonRootUserName = "app"
	if err := Validate(both); err != nil {
		t.Fatalf("both halves set should pass, got: %v", err)
	}

	// KeepUserID answers a different question (host/container uid agreement) and stands
	// alone.
	keep := baseCfg()
	keep.InternalUserMeta.KeepUserID = true
	if err := Validate(keep); err != nil {
		t.Fatalf("KeepUserID on its own should pass, got: %v", err)
	}
}

// Nothing in Zinc proxies or filters notifications, so every field in this block is inert.
// Accepting Silenced would tell an author their app is muted while it notifies freely; an
// unimplemented mechanism is refused rather than mis-enforced.
func TestNotifications_RefusedUntilImplemented(t *testing.T) {
	cfg := baseCfg()
	cfg.NotificationMeta.Silenced = true
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "NotificationMeta") {
		t.Fatalf("a set notification field: want a NotificationMeta error, got: %v", err)
	}

	// The zero value is what every existing app has, and must stay legal.
	if err := Validate(baseCfg()); err != nil {
		t.Fatalf("an untouched notification block should pass, got: %v", err)
	}
}

// The readiness gate: a probe is exec form and every word must be a word, and a timeout
// with no probe is the inert-field case - nothing waits, so the number would read as a
// bound that is not in force.
func TestReadyCheck_ProbeAndTimeoutAgree(t *testing.T) {
	empty := baseCfg()
	empty.StartConditions.ReadyCheck = []string{"test", ""}
	err := Validate(empty)
	if err == nil || !strings.Contains(err.Error(), "ReadyCheck[1]") {
		t.Fatalf("an empty word in ReadyCheck: want a ReadyCheck error, got: %v", err)
	}

	orphan := baseCfg()
	orphan.StartConditions.ReadyTimeoutSec = 30
	err = Validate(orphan)
	if err == nil || !strings.Contains(err.Error(), "no effect without ReadyCheck") {
		t.Fatalf("a timeout with no probe: want a no-effect error, got: %v", err)
	}

	negative := baseCfg()
	negative.StartConditions.ReadyCheck = []string{"true"}
	negative.StartConditions.ReadyTimeoutSec = -1
	err = Validate(negative)
	if err == nil || !strings.Contains(err.Error(), "ReadyTimeoutSec") {
		t.Fatalf("a negative timeout: want a ReadyTimeoutSec error, got: %v", err)
	}

	both := baseCfg()
	both.StartConditions.ReadyCheck = []string{"sh", "-c", "ip link show wg0 | grep -q UP"}
	both.StartConditions.ReadyTimeoutSec = 90
	if err := Validate(both); err != nil {
		t.Fatalf("a probe with a timeout should pass, got: %v", err)
	}
}

// A mount source has to name the same directory for everyone who reads the config. Podman
// resolves a relative source against its own working directory, and a source with no
// separator becomes a named volume it creates, so neither is what the YAML appears to say.
func TestVolumeHostSourceMustBeAbsolute(t *testing.T) {
	for _, source := range []string{"Downloads", "./data", "~/.ssh", "../secrets"} {
		cfg := baseCfg()
		cfg.Volumes = []schema.Volume{{HostMounted: true, HostMount: source, InnerMount: "/data"}}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Errorf("HostMount %q: want an absolute-path error, got: %v", source, err)
		}
	}
}

// Mounting the runtime directory hands the app the raw session bus and the raw compositor
// socket while DBusMeta stays empty, so every Zinc report - zcr where, zcr bus, the Wayland
// label - describes an app that has neither. The grant is invisible where a reviewer looks.
func TestVolumeCannotMountTheBrokeredSockets(t *testing.T) {
	for _, source := range []string{
		"/run/user/1000",
		"/run/user/1000/bus",
		"/run/user/1000/wayland-0",
		"/proc",
		"/sys/fs/cgroup",
	} {
		cfg := baseCfg()
		cfg.Volumes = []schema.Volume{{HostMounted: true, HostMount: source, InnerMount: "/x", Writable: true}}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "brokers") {
			t.Errorf("HostMount %q: want a refusal naming the brokered sockets, got: %v", source, err)
		}
	}
	// An ordinary host path is still an ordinary explicit grant.
	cfg := baseCfg()
	cfg.Volumes = []schema.Volume{{HostMounted: true, HostMount: "/home/user/Downloads", InnerMount: "/data"}}
	if err := Validate(cfg); err != nil {
		t.Errorf("an ordinary absolute mount should be allowed, got: %v", err)
	}
}

// The key's destination is built from the last element of Path, and Base("/..") is "/", so
// a ".." tail mounts the source OVER the container home rather than into it.
func TestKeyPathMustBeAbsoluteAndWithoutDotDot(t *testing.T) {
	for _, path := range []string{"~/.ssh/id_ed25519", "keys/id_ed25519", "/home/u/.ssh/.."} {
		cfg := baseCfg()
		cfg.Keys = []schema.Key{{Type: schema.SSH, Path: path}}
		if err := Validate(cfg); err == nil {
			t.Errorf("Keys.Path %q was accepted", path)
		}
	}
}

// digestRE was anchored only at the tail, so a reference merely had to END in something
// digest-shaped. The image is the one config value that reaches podman as a bare positional,
// where pflag reads a leading '-' as a flag rather than as an image name.
func TestImageCannotBeShapedLikeAFlag(t *testing.T) {
	const digest = "@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	for _, image := range []string{"-v/:/host" + digest, "--privileged" + digest} {
		cfg := baseCfg()
		cfg.ImageMeta.Image = image
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
			t.Errorf("Image %q was accepted, got: %v", image, err)
		}
	}
	cfg := baseCfg()
	cfg.ImageMeta.Image = "docker.io/library/alpine" + digest
	if err := Validate(cfg); err != nil {
		t.Errorf("an ordinary pinned image should validate, got: %v", err)
	}
}
