package pipewirectx

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// These run only where a PipeWire daemon is listening. They are the proof the security context
// is created and that the permissions are honoured, which no test over an argv can give.
func liveOpts(t *testing.T) options.HostOptions {
	t.Helper()
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		t.Skip("no XDG_RUNTIME_DIR")
	}
	if _, err := os.Stat(dir + "/" + sessionSocket); err != nil {
		t.Skip("no PipeWire daemon on this machine")
	}
	return options.HostOptions{RuntimeDir: dir}
}

// watcher reads a registry in the background, tracking what it can currently see. It is how a
// test observes the daemon taking an object away, which is what a permission drop looks like
// from the client's side.
type watcher struct {
	cnn      *conn
	registry uint32
	sources  map[uint32]bool
	bound    map[uint32]bool
	client   map[string]string
}

func watch(t *testing.T, path string, props ...string) *watcher {
	t.Helper()
	cnn, err := dial(path)
	if err != nil {
		t.Fatalf("connect to %s: %v", path, err)
	}
	t.Cleanup(cnn.close)
	if err := cnn.hello(props...); err != nil {
		t.Fatalf("handshake on %s: %v", path, err)
	}
	registry, err := cnn.getRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return &watcher{cnn: cnn, registry: registry, sources: map[uint32]bool{}, bound: map[uint32]bool{}}
}

// drain reads for the given window, applying every registry event. Time-based because these
// events are not replies: the daemon emits them when the session manager gets round to
// granting, and there is nothing to synchronise on.
func (wtc *watcher) drain(window time.Duration, appID string) {
	deadline := time.Now().Add(window)
	_ = wtc.cnn.pump(deadline, func(msg message) (bool, error) {
		// A bound client's info arrives on the proxy's id, not the registry's, so this is
		// checked before the registry filter below rather than after it.
		if wtc.bound[msg.id] && msg.opcode == clientEventInfo {
			if props, err := decodeClientInfo(msg.body); err == nil &&
				appID != "" && props["pipewire.sec.app-id"] == appID {
				wtc.client = props
			}
			return false, nil
		}
		if msg.id != wtc.registry {
			return false, nil
		}
		switch msg.opcode {
		case registryEventGlobal:
			item, err := decodeGlobal(msg.body)
			if err != nil {
				return false, nil
			}
			if item.iface == interfaceNode && captureClasses[item.props["media.class"]] {
				wtc.sources[item.id] = true
			}
			if item.iface == interfaceClient && appID != "" {
				// Bound because the identity is in the info event, not in this one.
				if id, err := wtc.cnn.bind(wtc.registry, item.id, interfaceClient, 3); err == nil {
					wtc.bound[id] = true
				}
			}
		case registryEventGlobalRemove:
			if id, err := decodeGlobalRemove(msg.body); err == nil {
				delete(wtc.sources, id)
			}
		}
		return false, nil
	})
}

// The context is created, the daemon serves the app's own socket, and a client arriving on it
// carries the identity Zinc stamped - which is what makes the app attributable at all.
func TestLive_SecurityContextStampsIdentity(t *testing.T) {
	opt := liveOpts(t)
	addr := paths.Address{App: "zinc-selftest", Instance: "ident"}

	lis, err := create(addr, opt)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("this daemon implements no security context")
	}
	if err != nil {
		t.Fatalf("create a security context: %v", err)
	}
	defer lis.close()
	if _, err := os.Stat(lis.socketPath); err != nil {
		t.Fatalf("the context socket is not on disk: %v", err)
	}

	// Connect as the app would, on the socket the context serves.
	app := watch(t, lis.socketPath, "application.name", "selftest-app")

	// Read the identity from the MANAGER, not from the app: a restricted client is not
	// necessarily allowed to see its own client object, and what matters is what the rest of
	// the session is told about it.
	manager := watch(t, opt.RuntimeDir+"/"+managerSocket, "application.name", "selftest-manager")
	manager.drain(3*time.Second, addr.App)
	_ = app

	if manager.client == nil {
		t.Fatal("no client carrying our app-id reached the daemon: the context did not take")
	}
	if got := manager.client["pipewire.sec.engine"]; got != SandboxEngine {
		t.Errorf("sec.engine = %q, want %q", got, SandboxEngine)
	}
	if got := manager.client["pipewire.sec.instance-id"]; got != addr.Runtime() {
		t.Errorf("sec.instance-id = %q, want %q", got, addr.Runtime())
	}
	t.Logf("identity as the session sees it: engine=%q app-id=%q instance-id=%q access=%q",
		manager.client["pipewire.sec.engine"], manager.client["pipewire.sec.app-id"],
		manager.client["pipewire.sec.instance-id"], manager.client["pipewire.access"])
}

// The one that matters: with no microphone granted, the app must not be able to see a single
// capture node. A security context alone does not do this - the session manager grants a
// restricted client rx on everything - so this is the test that the enforcement half works.
func TestLive_NoMicrophoneGrantHidesEveryCaptureNode(t *testing.T) {
	opt := liveOpts(t)
	addr := paths.Address{App: "zinc-selftest", Instance: "nomic"}

	lis, err := create(addr, opt)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("this daemon implements no security context")
	}
	if err != nil {
		t.Fatalf("create a security context: %v", err)
	}
	defer lis.close()

	app := watch(t, lis.socketPath, "application.name", "selftest-app")
	app.drain(3*time.Second, "")
	before := len(app.sources)
	if before == 0 {
		t.Skip("this session exposes no capture nodes, so there is nothing to deny")
	}
	t.Logf("before enforcement the app can see %d capture node(s)", before)

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- enforce(addr, grant{capture: false}, opt.RuntimeDir, stop) }()
	defer func() {
		close(stop)
		if err := <-done; err != nil && !errors.Is(err, os.ErrClosed) {
			t.Logf("enforcer returned: %v", err)
		}
	}()

	// The daemon takes an object away from a client that loses R on it, so the app's own
	// registry is what reports the result.
	app.drain(5*time.Second, "")
	if after := len(app.sources); after != 0 {
		t.Fatalf("the app can still see %d capture node(s) after enforcement (was %d); "+
			"Microphone: none is not being enforced", after, before)
	}
	t.Logf("after enforcement the app can see 0 capture nodes")
}
