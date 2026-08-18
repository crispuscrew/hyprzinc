//go:build e2e

// Package e2e drives the real zc and zcr binaries against rootless podman and asserts
// that an app actually runs and that the network lock-down is actually enforced - the
// guarantees unit tests cannot prove. It is black-box: everything goes through os/exec,
// nothing is imported from the tools under test.
//
// Run with `make e2e` (which sets the build tag and a generous timeout). Requires podman;
// the test skips if it is absent. The heavy lifting (build the binaries and helper images
// if missing) happens in setup, so the test is self-contained.
package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	appImage = "localhost/zinc/e2e-app:local"
	nftImage = "localhost/zinc/netfilter:local"
)

// tool runs a command, returning combined output and any error. The whole harness is
// this one primitive - no shell, no quoting, real errors.
func tool(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// must runs a command and fails the test if it errors, surfacing the output.
func must(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := tool(name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func TestE2E(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not found on $PATH; skipping end-to-end tests")
	}

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The creator sits at the repo root, not beside the container tools: it authors both
	// app kinds, so it is not a container-specific tool.
	creator := filepath.Join(here, "..", "..", "creator")
	runner := filepath.Join(here, "..", "runner")
	zc := filepath.Join(creator, "bin", "zc")
	zcr := filepath.Join(runner, "bin", "zcr")

	// Build what's missing: the two binaries, the nft helper image, and the test app image.
	if _, err := os.Stat(zc); err != nil {
		must(t, "make", "-C", creator, "build")
	}
	if _, err := os.Stat(zcr); err != nil {
		must(t, "make", "-C", runner, "build")
	}
	if _, err := tool("podman", "image", "exists", nftImage); err != nil {
		must(t, "make", "-C", runner, "netfilter-image")
	}
	must(t, "podman", "build", "-t", appImage, here)

	// Isolate the store and running state; zc delegates runtime actions to zcr on $PATH.
	cfg := t.TempDir()
	apps := filepath.Join(cfg, "zinc", "apps")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sleeper", "producer", "consumer", "capped", "slowdep", "waiter", "scratch"} {
		data, err := os.ReadFile(filepath.Join(here, "apps", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(apps, name+".yaml"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("PATH", filepath.Join(runner, "bin")+string(os.PathListSeparator)+
		filepath.Join(creator, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Cleanup(func() {
		for _, app := range []string{"consumer", "producer", "sleeper", "waiter", "slowdep"} {
			tool(zcr, "stop", app)
			tool("podman", "pod", "rm", "-f", app+"-pod")
			tool("podman", "rm", "-f", app)
		}
		tool("podman", "network", "rm", "-f", "zinc-link-producer")
	})

	running := func(name string) bool {
		out, _ := tool(zcr, "ps")
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == name {
				return true
			}
		}
		return false
	}
	// waitFor polls the predicate for up to 20s (podman pod bring-up + nft load is not
	// instant), returning whether it became true.
	waitFor := func(cond func() bool) bool {
		for i := 0; i < 40; i++ {
			if cond() {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}

	t.Run("authoring", func(t *testing.T) {
		must(t, zc, "new", "authored", "--image", appImage)
		if _, err := os.Stat(filepath.Join(apps, "authored.yaml")); err != nil {
			t.Fatal("zc new should have written authored.yaml")
		}
		must(t, zc, "validate", "authored")
	})

	t.Run("lifecycle", func(t *testing.T) {
		must(t, zc, "run", "sleeper", "--exec")
		if !waitFor(func() bool { return running("sleeper") }) {
			t.Fatal("sleeper should be running after `zc run --exec`")
		}
		out, _ := tool(zc, "logs", "sleeper")
		if !strings.Contains(out, "sleeper up") {
			t.Fatalf("zc logs should return the app's output, got:\n%s", out)
		}
		must(t, zc, "stop", "sleeper")
		if !waitFor(func() bool { return !running("sleeper") }) {
			t.Fatal("sleeper should be stopped after `zc stop`")
		}
	})

	t.Run("tier2_enforcement", func(t *testing.T) {
		// This scenario applies real nftables rules in a rootless pod netns. On a runner
		// without that support, set ZINC_E2E_NO_NET=1 to skip just this scenario.
		if os.Getenv("ZINC_E2E_NO_NET") != "" {
			t.Skip("ZINC_E2E_NO_NET set; skipping the network-enforcement scenario")
		}
		must(t, zc, "run", "producer", "--exec")
		if !waitFor(func() bool { return running("producer") }) {
			t.Fatal("producer should be running")
		}
		must(t, zc, "run", "consumer", "--exec")
		waitFor(func() bool { return running("consumer") })

		// The consumer's entrypoint probes the producer and prints the verdict to its logs.
		var verdict string
		waitFor(func() bool {
			out, _ := tool(zc, "logs", "consumer")
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "PROBE ") {
					verdict = strings.TrimSpace(line)
				}
			}
			return verdict != ""
		})
		t.Logf("consumer reported: %q", verdict)

		if !strings.Contains(verdict, "5432=open") {
			t.Error("consumer should REACH the producer's published port 5432")
		}
		if !strings.Contains(verdict, "9999=closed") {
			t.Error("consumer should be DROPPED on the unpublished port 9999")
		}
	})

	t.Run("counters", func(t *testing.T) {
		// Runs on what tier2_enforcement left behind: the producer is still up, and the
		// consumer has already knocked on both a published port and an unpublished one. That
		// traffic is the whole point here - a counter is only worth anything if a real packet
		// moves it, and these are the only two packets in the suite whose fate is known.
		if os.Getenv("ZINC_E2E_NO_NET") != "" {
			t.Skip("ZINC_E2E_NO_NET set; skipping the network-enforcement scenario")
		}
		if !running("producer") {
			t.Skip("producer is not up (the tier-2 scenario is what starts it)")
		}

		// The enumeration has to distinguish the two postures. sleeper has no NetworkLists at
		// all, so it has no netns of its own; reporting it beside the producer as though both
		// were locked down would invert what the table means.
		must(t, zc, "run", "sleeper", "--exec")
		if !waitFor(func() bool { return running("sleeper") }) {
			t.Fatal("sleeper should be running")
		}
		defer func() { _, _ = tool(zc, "stop", "sleeper") }()

		listing := must(t, zcr, "net")
		t.Logf("zcr net:\n%s", listing)
		for _, want := range []string{"producer", "filtered", "producer-pod", "sleeper", "isolated"} {
			if !strings.Contains(listing, want) {
				t.Errorf("`zcr net` missing %q:\n%s", want, listing)
			}
		}

		// And the readout itself, against the real ruleset in the real netns.
		raw := must(t, zcr, "net", "producer", "--json")
		t.Logf("zcr net producer --json:\n%s", raw)
		var report struct {
			Address  string `json:"address"`
			Posture  string `json:"posture"`
			Netns    string `json:"netns"`
			Counters []struct {
				Chain   string `json:"chain"`
				Verdict string `json:"verdict"`
				Label   string `json:"label"`
				Packets uint64 `json:"packets"`
			} `json:"counters"`
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			t.Fatalf("`zcr net --json` should be machine-readable: %v\n%s", err, raw)
		}
		if report.Netns != "producer-pod" || report.Posture != "filtered" {
			t.Errorf("producer should report its own locked netns, got %+v", report)
		}

		// The consumer reached 5432 and was dropped on 9999, so exactly two numbers must have
		// moved: the rule that published the port, and the input chain's default-drop. The
		// second is why the backstop rule exists at all - nftables counts rules and not
		// policies, so without it a fail-closed chain reports nothing whatever it refuses.
		published, refused := uint64(0), uint64(0)
		for _, counter := range report.Counters {
			switch {
			case counter.Chain == "input" && strings.Contains(counter.Label, "link tcp"):
				published += counter.Packets
			case counter.Chain == "input" && counter.Label == "default policy":
				refused += counter.Packets
			}
		}
		if published == 0 {
			t.Errorf("the consumer reached port 5432, so the rule that published it must have counted: %+v", report.Counters)
		}
		if refused == 0 {
			t.Errorf("the consumer was dropped on port 9999, so the input chain's backstop must have counted: %+v", report.Counters)
		}
		t.Logf("producer counted %d packets accepted on its published port and %d refused by policy", published, refused)

		// An isolated app is not a filtered one with an empty ruleset, and must not be
		// described as though it were.
		isolated := must(t, zcr, "net", "sleeper")
		if !strings.Contains(isolated, "isolated") || strings.Contains(isolated, "PACKETS") {
			t.Errorf("an app with no NetworkLists has no ruleset to tabulate:\n%s", isolated)
		}
	})

	t.Run("containment", func(t *testing.T) {
		// ResourcesMeta and InternalUserMeta were validated and dropped on the floor until
		// 0.7. The runtime's unit tests prove the flags are emitted; only the kernel can say
		// they took effect, so the app reports its own cgroup values and uid back through
		// the logs. Rootless podman needs cgroup v2 delegation for any of this to be real -
		// a host without it would give the app no limits and say nothing.
		must(t, zc, "run", "capped", "--exec")
		if !waitFor(func() bool { return running("capped") }) {
			t.Fatal("capped should be running after `zc run --exec`")
		}
		defer func() { _, _ = tool(zc, "stop", "capped") }()

		var out string
		waitFor(func() bool {
			out, _ = tool(zc, "logs", "capped")
			return strings.Contains(out, "capped up")
		})
		t.Logf("capped reported:\n%s", out)

		// 128 MiB, and swap on top of it rather than instead of it: --memory-swap is the
		// total of the two, so a 128 MiB limit with 32 MiB of swap leaves a 32 MiB swap
		// ceiling. Passing the swap figure through unsummed would have capped the app at
		// 32 MiB of memory outright.
		for _, want := range []string{
			"UID=65534",            // nobody, not root
			"MEMORY_MAX=134217728", // 128 MiB
			"SWAP_MAX=33554432",    // 160 MiB total - 128 MiB memory
			"PIDS_MAX=50",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("expected %q in the app's own report of what it was granted", want)
			}
		}
	})

	t.Run("scratch_volumes", func(t *testing.T) {
		// A Volume with no host path produced no podman argument at all until 0.10.0, so
		// SizeLimited and SizeLimitMiB were validated and did nothing. The runtime's unit
		// tests prove the --mount is emitted; only the kernel can say the ceiling holds, so
		// the app reports its own mount back through the logs the way capped.sh does.
		must(t, zc, "run", "scratch", "--exec")
		if !waitFor(func() bool { return running("scratch") }) {
			t.Fatal("scratch should be running after `zc run --exec`")
		}
		defer func() { _, _ = tool(zc, "stop", "scratch") }()

		var out string
		waitFor(func() bool {
			out, _ = tool(zc, "logs", "scratch")
			return strings.Contains(out, "scratch up")
		})
		t.Logf("scratch reported:\n%s", out)

		for _, want := range []string{
			"SCRATCH_FS=tmpfs", // scratch space, not a host path
			"SCRATCH_MB=8",     // SizeLimitMiB reached the kernel
			"WROTE_MB=8",       // and it bites: 32 MiB in, 8 MiB written
			"READONLY=refused", // Writable defaults off, as a bind mount's does
		} {
			if !strings.Contains(out, want) {
				t.Errorf("expected %q in the app's report of the volume it was given", want)
			}
		}
		// nosuid and nodev always: scratch space is never a place to gain privilege.
		for _, want := range []string{"nosuid", "nodev", "noexec"} {
			if !strings.Contains(out, want) {
				t.Errorf("the scratch mount should carry %q: %s", want, out)
			}
		}
	})

	t.Run("dbus", func(t *testing.T) {
		// Needs two things this suite cannot create: a real session bus to proxy, and the
		// helper image carrying xdg-dbus-proxy (the proxy runs --pull never, by design). A
		// GitHub runner has neither, so this scenario is mostly a local gate - which is worth
		// saying out loud rather than having it quietly always-skip and look like coverage.
		busPath := os.Getenv("XDG_RUNTIME_DIR")
		if busPath == "" {
			t.Skip("no XDG_RUNTIME_DIR; skipping the session-bus scenario")
		}
		busPath = filepath.Join(busPath, "bus")
		if _, err := os.Stat(busPath); err != nil {
			t.Skipf("no session bus at %s; skipping the session-bus scenario", busPath)
		}
		if _, err := tool("podman", "image", "exists", "localhost/zinc/netfilter:local"); err != nil {
			t.Skip("localhost/zinc/netfilter:local absent (make -C container/runner netfilter-image); skipping the session-bus scenario")
		}

		must(t, zc, "new", "busapp", "--image", appImage,
			"--entrypoint", "/sleeper.sh",
			"--dbus-talk", "org.freedesktop.portal.Desktop",
			"--dbus-own", "org.mpris.MediaPlayer2.busapp")

		// zc new is expected to have set KeepUserID itself: a filtered bus is a uid agreement
		// with the proxy, and validation refuses the pair without it.
		authored, rerr := os.ReadFile(filepath.Join(apps, "busapp.yaml"))
		if rerr != nil {
			t.Fatalf("reading the authored app: %v", rerr)
		}
		if !strings.Contains(string(authored), "KeepUserID: true") {
			t.Error("zc new --dbus-talk did not set KeepUserID, so this app could not have saved")
		}

		defer func() { _, _ = tool(zc, "stop", "busapp") }()
		must(t, zc, "run", "busapp", "--exec")
		if !waitFor(func() bool { return running("busapp") }) {
			t.Fatal("busapp should be running: its launch waits for the proxy, so a failure here is the proxy or the readiness probe")
		}

		// The proxy is a container of its own, NOT a member of the app's pod - the app must
		// not share a PID namespace with the process filtering it.
		proxy := "zinc-dbus-busapp"
		if !waitFor(func() bool { return running(proxy) }) {
			t.Fatalf("%s should be running alongside the app", proxy)
		}
		pod, _ := tool("podman", "inspect", "--format", "{{.Pod}}", proxy)
		if strings.TrimSpace(pod) != "" {
			t.Errorf("%s joined a pod (%q); it must stay outside the app's pod", proxy, strings.TrimSpace(pod))
		}

		// The app is pointed at the filtered socket and never at the host's.
		env, _ := tool("podman", "inspect", "--format", "{{.Config.Env}}", "busapp")
		if !strings.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/zinc-bus/bus") {
			t.Errorf("busapp is not pointed at the filtered socket, env = %s", env)
		}
		mounts, _ := tool("podman", "inspect", "--format", "{{range .Mounts}}{{.Source}} {{end}}", "busapp")
		if strings.Contains(mounts, busPath) {
			t.Errorf("busapp mounts the REAL session bus %s: %s", busPath, mounts)
		}

		// Attribution: the mapping Zinc publishes must be the one that is actually true of the
		// running system. `zcr bus` says a pid belongs to busapp; podman is asked the same
		// question about the container Zinc created, and the two have to agree - otherwise a
		// desktop resolving a bus connection would name the wrong app with full confidence.
		table := must(t, zcr, "bus")
		var reported string
		for _, line := range strings.Split(table, "\n") {
			if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "busapp" {
				reported = fields[1]
			}
		}
		if reported == "" {
			t.Fatalf("zcr bus does not list the running app's proxy:\n%s", table)
		}
		actual, _ := tool("podman", "inspect", "--format", "{{.State.Pid}}", proxy)
		if strings.TrimSpace(actual) != reported {
			t.Errorf("zcr bus reports pid %s for busapp, podman reports %s for %s", reported, strings.TrimSpace(actual), proxy)
		}

		// The other half of the published mapping: the socket `zcr where` names has to be the
		// socket that is really there. A reported path that does not exist would send a desktop
		// looking for a file and blaming the wrong side when it is missing.
		reportJSON := must(t, zcr, "where", "busapp", "--json")
		var report struct {
			Address string `json:"address"`
			Bus     *struct {
				Socket string `json:"socket"`
				Proxy  string `json:"proxy"`
			} `json:"bus"`
		}
		if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
			t.Fatalf("zcr where --json is not parseable JSON: %v\n%s", err, reportJSON)
		}
		if report.Address != "busapp" || report.Bus == nil || report.Bus.Proxy != proxy {
			t.Fatalf("zcr where --json does not describe the running app: %s", reportJSON)
		}
		if _, err := os.Stat(report.Bus.Socket); err != nil {
			t.Errorf("the socket zcr where reports (%s) is not on disk while the app runs: %v", report.Bus.Socket, err)
		}

		// And the last link, on the real bus: hold a client on the filtered socket, then walk
		// the chain a desktop walks - a connection it can see on the HOST bus, to a pid, to an
		// app. Its own subtest so that a machine without the host bus tools skips this half
		// only, rather than taking the teardown checks below with it. Skipped rather than
		// faked: a fake would prove only that the test agrees with itself.
		t.Run("host_bus_resolution", func(t *testing.T) {
			for _, needed := range []string{"gdbus", "busctl"} {
				if _, err := exec.LookPath(needed); err != nil {
					t.Skipf("no %s on PATH; skipping the host-bus resolution half", needed)
				}
			}
			// xdg-dbus-proxy opens one upstream connection PER CLIENT, so with nothing attached
			// to the filtered socket there is no connection on the host bus to attribute at all.
			client := exec.Command("gdbus", "wait", "--address", "unix:path="+report.Bus.Socket,
				"--timeout", "20", "org.example.NeverAppears")
			if err := client.Start(); err != nil {
				t.Fatalf("holding a client on the filtered socket: %v", err)
			}
			defer func() { _ = client.Process.Kill() }()

			found := waitFor(func() bool {
				names, _ := tool("busctl", "--user", "list", "--unique")
				for _, line := range strings.Split(names, "\n") {
					// "<unique name> <pid> <process> ..." - the pid the bus itself took from
					// SO_PEERCRED, which is the one thing about the peer the app cannot assert.
					if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == reported {
						t.Logf("host bus connection %s resolves to pid %s, which zcr bus attributes to busapp", fields[0], reported)
						return true
					}
				}
				return false
			})
			if !found {
				t.Error("no host-bus connection carries the proxy's pid, so nothing could be attributed to busapp")
			}
		})

		// Teardown removes the proxy too: it is --rm, but an app whose proxy name is still
		// taken cannot be relaunched.
		must(t, zc, "stop", "busapp")
		if !waitFor(func() bool { return !running(proxy) }) {
			t.Errorf("%s survived `zc stop`; the next launch would collide on its name", proxy)
		}
	})

	t.Run("readiness", func(t *testing.T) {
		// DependsOn used to mean "running", and slowdep is the case where running is the
		// wrong question: its container is up five seconds before the file its ReadyCheck
		// looks for exists, the way a VPN container is up before its tunnel is. The launch
		// of waiter must sit in that gap rather than start into it.
		defer func() {
			_, _ = tool(zc, "stop", "waiter")
			_, _ = tool(zc, "stop", "slowdep")
		}()

		start := time.Now()
		must(t, zc, "run", "waiter", "--exec")
		waited := time.Since(start)

		// The fixture sleeps 5s before declaring itself ready. Asserting against a floor
		// well under that keeps the test honest about the wait without making it a race on
		// a loaded machine.
		if waited < 3*time.Second {
			t.Errorf("launch returned after %s: waiter did not wait for slowdep's readiness", waited)
		}
		t.Logf("waiter's launch waited %s for slowdep", waited)

		// The other half: the wait ended because the dependency became ready, not because
		// it timed out and gave up. podman's own view is the one being waited on.
		health, _ := tool("podman", "inspect", "--format", "{{.State.Health.Status}}", "slowdep")
		if strings.TrimSpace(health) != "healthy" {
			t.Errorf("slowdep health = %q, want healthy - the ReadyCheck is the container's healthcheck", strings.TrimSpace(health))
		}
		if !waitFor(func() bool { return running("waiter") }) {
			t.Fatal("waiter should be running once its dependency is ready")
		}
	})
}
