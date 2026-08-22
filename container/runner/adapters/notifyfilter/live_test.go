package notifyfilter

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// sessionBus resolves the real bus, or skips. The hermetic tests above prove the rewriting;
// these prove the relay survives contact with a real client library and a real bus daemon,
// which a stand-in cannot.
func sessionBus(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	path := ""
	for _, part := range strings.Split(addr, ",") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(part), "unix:path="); ok {
			path = rest
			break
		}
	}
	if path == "" {
		if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
			path = filepath.Join(runtime, "bus")
		}
	}
	if path == "" {
		t.Skip("no session bus address")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no session bus socket on this machine")
	}
	return path
}

// liveFilter puts a filter in front of the real bus and returns a connection made through it by
// a real D-Bus client, authentication, Hello and all.
func liveFilter(t *testing.T, meta schema.NotificationMeta) *dbus.Conn {
	t.Helper()
	upstream := sessionBus(t)
	path := filepath.Join(t.TempDir(), "filtered")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	rly := &relay{pol: policyOf(meta)}
	go func() { _ = rly.serve(listener, upstream) }()

	conn, err := dbus.Dial("unix:path=" + path)
	if err != nil {
		t.Fatalf("dial the filtered socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Auth(nil); err != nil {
		t.Fatalf("authenticate through the filter: %v", err)
	}
	if err := conn.Hello(); err != nil {
		t.Fatalf("Hello through the filter: %v", err)
	}
	return conn
}

// A real client completing authentication and Hello through the relay is the proof that the
// handshake is passed through faithfully - it is line-based text with its own leading NUL, and
// a relay that mangled any of it would never reach the point of carrying a message.
func TestLive_RealClientReachesTheRealBus(t *testing.T) {
	conn := liveFilter(t, schema.NotificationMeta{Disabled: true})

	var owner string
	err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, "org.freedesktop.DBus").Store(&owner)
	if err != nil {
		t.Fatalf("an ordinary call did not survive the relay: %v", err)
	}
	if owner == "" {
		t.Fatal("the bus answered with no owner")
	}
	t.Logf("through the filter, the bus answers: %q", owner)
}

// The end-to-end one: a rewritten notification has to be a message the real notification server
// accepts. The hermetic tests decode it with the same library that wrote it, so only this can
// show that a live server takes it.
func TestLive_RewrittenNotificationIsAcceptedByTheServer(t *testing.T) {
	conn := liveFilter(t, schema.NotificationMeta{
		UseCustomPrefix: true,
		CustomPrefix:    "[zinc-selftest]",
	})
	server := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	if err := server.Call("org.freedesktop.DBus.Peer.Ping", 0).Store(); err != nil {
		t.Skipf("no notification server on this session: %v", err)
	}

	var id uint32
	err := server.Call("org.freedesktop.Notifications.Notify", 0,
		"zinc-selftest", uint32(0), "",
		"Zinc self-test", `a link: <a href="http://example.invalid">click</a>`,
		[]string{"open", "Open"},
		map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(0))},
		int32(0), // never expire: the policy has to clamp this
	).Store(&id)
	if err != nil {
		t.Fatalf("the server refused a rewritten notification: %v", err)
	}
	// Taken straight back down: a test must not leave something on the user's screen.
	_ = server.Call("org.freedesktop.Notifications.CloseNotification", 0, id).Store()
	t.Logf("the notification server accepted the rewritten call and returned id %d", id)
}
