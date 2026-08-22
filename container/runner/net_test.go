package main

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/nftrules"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// A runtime name has to be read back as the address a person types, and the podman name is
// lossy about it: app and instance are joined with a dot, and an app name may legally
// contain one. Only the set of defined apps can settle which is which.
func TestAddressOf(t *testing.T) {
	defined := []string{"firefox", "media.server", "notes"}
	cases := []struct {
		runtime  string
		address  string
		instance string
		known    bool
	}{
		{runtime: "firefox", address: "firefox", known: true},                                       // un-instanced
		{runtime: "firefox.work", address: "firefox@work", instance: "work", known: true},           // instanced
		{runtime: "media.server", address: "media.server", known: true},                             // a dot in the app name
		{runtime: "media.server.home", address: "media.server@home", instance: "home", known: true}, // both
		{runtime: "zinc-dbus-firefox"},                                                              // Zinc's own proxy, not an app
		{runtime: "someones-postgres"},                                                              // a container Zinc does not manage
		{runtime: "firefox.", known: false},                                                         // a trailing separator names no instance
		{runtime: "unknown.work"},                                                                   // an instance of an app that is not defined
	}
	for _, testCase := range cases {
		addr, ok := addressOf(testCase.runtime, defined)
		if ok != testCase.known {
			t.Errorf("%q: known = %v, want %v", testCase.runtime, ok, testCase.known)
			continue
		}
		if !ok {
			continue
		}
		if addr.String() != testCase.address || addr.Instance != testCase.instance {
			t.Errorf("%q: got %q (instance %q), want %q (instance %q)",
				testCase.runtime, addr.String(), addr.Instance, testCase.address, testCase.instance)
		}
		// The round trip is the actual contract: what comes back must name the same
		// runtime object, or the netns reported beside it belongs to something else.
		if addr.Runtime() != testCase.runtime {
			t.Errorf("%q: round trip gave %q", testCase.runtime, addr.Runtime())
		}
	}
}

// The netns is named from the runtime form and the address from the human form, and an
// instanced app must get both right: reporting "firefox@work" beside "firefox-pod" would
// point a reader at another instance's firewall.
func TestEntryFor(t *testing.T) {
	instanced := paths.Address{App: "firefox", Instance: "work"}
	entry := entryFor(instanced, instanced.Runtime(), true)
	if entry.Address != "firefox@work" || entry.Netns != "firefox.work-pod" || entry.Posture != postureFiltered {
		t.Errorf("instanced filtered app: %+v", entry)
	}

	plain := entryFor(paths.Address{App: "notes"}, "notes", false)
	if plain.Address != "notes" || plain.Posture != postureIsolated {
		t.Errorf("un-instanced isolated app: %+v", plain)
	}
	// No netns, and not an empty-looking one: an isolated app has no pod, and naming a pod
	// that does not exist would invite someone to go looking for its rules.
	if plain.Netns != "" {
		t.Errorf("an isolated app has no netns, got %q", plain.Netns)
	}
	if plain.Instance != "" {
		t.Errorf("an un-instanced app has no instance, got %q", plain.Instance)
	}
}

// The enumeration's whole job is to distinguish the two postures. Listing an isolated app as
// though it had a locked netns, or an enforced one as though it had none, inverts the
// security meaning of the table - so the words and the legend that defines them are asserted
// together.
func TestPrintEntries_DistinguishesThePostures(t *testing.T) {
	out := captureStdout(t, func() {
		err := printEntries([]netEntry{
			{Address: "firefox@work", App: "firefox", Instance: "work", Posture: postureFiltered, Netns: "firefox.work-pod"},
			{Address: "firefox", App: "firefox", Posture: postureFiltered, Netns: "firefox-pod"},
			{Address: "notes", App: "notes", Posture: postureIsolated},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"firefox@work", "firefox.work-pod", // instanced, in the form a person types
		"firefox-pod",     // un-instanced, beside it
		"filtered",        //
		"isolated",        //
		"no NetworkLists", // the legend, without which the two words are guesses
		"--network none",  //
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enumeration missing %q:\n%s", want, out)
		}
	}
	// The internal runtime form must not leak: "firefox.work" is what podman calls it,
	// "firefox@work" is what a person types and what every other command accepts.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "firefox.work ") {
			t.Errorf("the runtime form leaked into the address column: %q", line)
		}
	}
	if len(captureStdout(t, func() { _ = printEntries(nil) })) != 0 {
		t.Error("nothing running should print nothing, as `zcr ps` does")
	}
}

// The readout has to be honest about what a counter is. A number with no context reads as a
// lifetime total, and it is not one - it is gone the moment the pod is.
func TestPrintReport_SaysWhatACounterMeans(t *testing.T) {
	out := captureStdout(t, func() {
		err := printReport(netReport{
			netEntry: netEntry{Address: "firefox@work", App: "firefox", Instance: "work",
				Posture: postureFiltered, Netns: "firefox.work-pod"},
			Note: countersNote,
			Counters: []nftrules.RuleCounter{
				{Chain: "output", Verdict: "accept", Label: "list[0] ip tcp", Packets: 3, Bytes: 180},
				{Chain: "output", Verdict: "drop", Label: "default policy", Packets: 9, Bytes: 652},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"firefox@work", "firefox.work-pod", "since this launch",
		"list[0] ip tcp", "3", "180", "default policy", "652"} {
		if !strings.Contains(out, want) {
			t.Errorf("readout missing %q:\n%s", want, out)
		}
	}

	// An isolated app gets an explanation instead of an empty table, which would read as
	// "a ruleset that has seen nothing" rather than "no ruleset".
	isolated := captureStdout(t, func() {
		_ = printReport(netReport{netEntry: netEntry{Address: "notes", App: "notes", Posture: postureIsolated}})
	})
	if !strings.Contains(isolated, "no NetworkLists") || strings.Contains(isolated, "PACKETS") {
		t.Errorf("an isolated app should be explained, not tabulated:\n%s", isolated)
	}
}

func TestNetDispatch_RejectsBadArguments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := run([]string{"net", "--nope"}); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("an unknown flag should be rejected, got: %v", err)
	}
	if err := run([]string{"net", "one", "two"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("a second app should be rejected, got: %v", err)
	}
}
