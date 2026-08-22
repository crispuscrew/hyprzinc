package pipewirectx

import (
	"testing"
	"time"

	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

var testAddr = paths.Address{App: "recorder", Instance: "one"}

// appClient is the app's own client as the daemon reports it, carrying the identity a security
// context stamped on it.
func appClient(id uint32, app, instance string) global {
	return global{id: id, iface: interfaceClient, props: map[string]string{
		"pipewire.sec.engine":      SandboxEngine,
		"pipewire.sec.app-id":      app,
		"pipewire.sec.instance-id": instance,
	}}
}

func captureNode(id uint32, name string) global {
	return global{id: id, iface: interfaceNode, props: map[string]string{
		"media.class": "Audio/Source",
		"node.name":   name,
	}}
}

func sinkNode(id uint32, name string) global {
	return global{id: id, iface: interfaceNode, props: map[string]string{
		"media.class": "Audio/Sink",
		"node.name":   name,
	}}
}

// runEnforcer starts the enforcer against the fake and stops it when the test ends.
func runEnforcer(t *testing.T, fake *fakeDaemon, want grant) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan error, 1)
	// enforce resolves the manager socket under the directory it is given, and the fake listens
	// on the session name, so the test points both at the same file.
	go func() { done <- enforceAt(testAddr, want, fake.path, stop) }()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the enforcer did not stop")
		}
	})
}

// With no microphone granted, every capture node has to be taken to nothing, and the default
// left at what the session manager grants. This is the rule the whole package exists for.
func TestEnforce_DeniesEveryCaptureNode(t *testing.T) {
	fake := startFakeDaemon(t, []global{
		captureNode(10, "built-in-mic"),
		captureNode(11, "usb-mic"),
		sinkNode(12, "speakers"),
		appClient(20, testAddr.App, testAddr.Runtime()),
	})
	runEnforcer(t, fake, grant{capture: false})

	set, ok := fake.lastApplied(t, 5*time.Second)
	if !ok {
		t.Fatal("the enforcer set no permissions at all")
	}
	if got := set.perms[permIDAny]; got != permRX {
		t.Errorf("default permission = %d, want rx (%d)", got, permRX)
	}
	for _, id := range []int32{10, 11} {
		perm, listed := set.perms[id]
		if !listed {
			t.Errorf("capture node %d was not named in the permission set", id)
			continue
		}
		if perm != permNone {
			t.Errorf("capture node %d = %d, want none", id, perm)
		}
	}
	// The sink is what the app plays to, and denying it would deny playback.
	if _, listed := set.perms[12]; listed {
		t.Error("the sink was denied, which would take playback away with it")
	}
}

// A microphone grant means there is nothing to hold the app to, and the enforcer must not send a
// permission set at all: narrowing an app that asked for capture would be second-guessing it.
func TestEnforce_GrantedMicrophoneIsLeftAlone(t *testing.T) {
	fake := startFakeDaemon(t, []global{
		captureNode(10, "built-in-mic"),
		appClient(20, testAddr.App, testAddr.Runtime()),
	})
	runEnforcer(t, fake, grant{capture: true})

	if _, ok := fake.lastApplied(t, 1500*time.Millisecond); ok {
		t.Fatal("permissions were set for an app that was granted a microphone")
	}
}

// Another app's client must not be touched. Both halves of the identity are checked, so a second
// instance of the same app - which has its own grant - is somebody else here too.
func TestEnforce_OnlyActsOnItsOwnClient(t *testing.T) {
	fake := startFakeDaemon(t, []global{
		captureNode(10, "built-in-mic"),
		appClient(20, "another-app", "another-app.one"),
		appClient(21, testAddr.App, "recorder.two"), // same app, different instance
	})
	runEnforcer(t, fake, grant{capture: false})

	if set, ok := fake.lastApplied(t, 1500*time.Millisecond); ok {
		t.Fatalf("permissions were set on a client that is not ours: %+v", set)
	}
}

// A microphone plugged in an hour into a session is a new global the app would otherwise hold
// the default permission on, so the enforcer has to notice and deny that one too.
func TestEnforce_DeniesACaptureNodeThatAppearsLater(t *testing.T) {
	fake := startFakeDaemon(t, []global{
		captureNode(10, "built-in-mic"),
		appClient(20, testAddr.App, testAddr.Runtime()),
	})
	runEnforcer(t, fake, grant{capture: false})

	first, ok := fake.lastApplied(t, 5*time.Second)
	if !ok {
		t.Fatal("no permissions were set for the graph as it started")
	}
	if _, listed := first.perms[10]; !listed {
		t.Fatalf("the node present at start was not denied: %+v", first)
	}

	fake.appear(captureNode(30, "hot-plugged-mic"))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		set, _ := fake.lastApplied(t, 200*time.Millisecond)
		if perm, listed := set.perms[30]; listed && perm == permNone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a capture node that appeared after the app started was never denied")
}

// The context has to carry the identity, and both descriptors: the listening socket the daemon
// serves on, and the pipe whose closing revokes it.
func TestCreate_SendsTheIdentityAndBothDescriptors(t *testing.T) {
	fake := startFakeDaemon(t, []global{
		{id: 3, iface: interfaceSecurityContext, props: map[string]string{}},
	})
	dir := t.TempDir()
	lis, err := createAt(testAddr, dir, fake.path)
	if err != nil {
		t.Fatalf("create a security context: %v", err)
	}
	defer lis.close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		props, fds := fake.created, fake.createFDs
		fake.mu.Unlock()
		if props != nil {
			if props["pipewire.sec.engine"] != SandboxEngine {
				t.Errorf("sec.engine = %q, want %q", props["pipewire.sec.engine"], SandboxEngine)
			}
			if props["pipewire.sec.app-id"] != testAddr.App {
				t.Errorf("sec.app-id = %q, want %q", props["pipewire.sec.app-id"], testAddr.App)
			}
			if props["pipewire.sec.instance-id"] != testAddr.Runtime() {
				t.Errorf("sec.instance-id = %q, want %q", props["pipewire.sec.instance-id"], testAddr.Runtime())
			}
			if fds != 2 {
				t.Errorf("the daemon received %d descriptors, want 2 (the socket and the revocation pipe)", fds)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no security context was created")
}

// A daemon with no security context must be reported as such rather than left to fail later,
// since the launch decides what to tell the user from exactly this.
func TestCreate_ReportsADaemonWithNoSecurityContext(t *testing.T) {
	fake := startFakeDaemon(t, []global{sinkNode(12, "speakers")})
	_, err := createAt(testAddr, t.TempDir(), fake.path)
	if err == nil {
		t.Fatal("a daemon with no security context should be refused")
	}
	if err.Error() != ErrUnsupported.Error() {
		t.Fatalf("want ErrUnsupported, got: %v", err)
	}
}
