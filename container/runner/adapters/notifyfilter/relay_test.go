package notifyfilter

import (
	"bytes"
	"encoding/binary"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// notifyCall builds the one message the filter acts on, in the shape a real client sends it.
func notifyCall(t *testing.T, summary, body string, actions []string, timeout int32) []byte {
	t.Helper()
	msg := &dbus.Message{
		Type: dbus.TypeMethodCall,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/Notifications")),
			dbus.FieldInterface:   dbus.MakeVariant(notifyInterface),
			dbus.FieldMember:      dbus.MakeVariant("Notify"),
			dbus.FieldDestination: dbus.MakeVariant(notifyInterface),
			dbus.FieldSignature:   dbus.MakeVariant(dbus.ParseSignatureMust("susssasa{sv}i")),
		},
		Body: []interface{}{
			"an-app", uint32(0), "an-icon", summary, body, actions,
			// A hint, because hints are what make patching bytes impossible: the dictionary's
			// padding moves when anything before it changes length.
			map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(1))},
			timeout,
		},
	}
	var buf bytes.Buffer
	if err := msg.EncodeTo(&buf, binary.LittleEndian); err != nil {
		t.Fatalf("encode a Notify call: %v", err)
	}
	raw := buf.Bytes()
	binary.LittleEndian.PutUint32(raw[8:12], 42) // a serial of its own, as a real client sets
	return raw
}

// upstream is a stand-in for the bus behind the filter. It completes the handshake and hands
// back the first message it is given, which is what the app's call became.
type upstream struct {
	path string
	got  chan *dbus.Message
}

func fakeUpstream(t *testing.T) *upstream {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upstream")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	up := &upstream{path: path, got: make(chan *dbus.Message, 4)}
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		authenticated := false
		for {
			read, err := conn.Read(chunk)
			if read > 0 {
				buf = append(buf, chunk[:read]...)
			}
			if err != nil {
				return
			}
			for !authenticated {
				line, rest, found := bytes.Cut(buf, []byte("\r\n"))
				if !found {
					break
				}
				buf = rest
				if bytes.EqualFold(bytes.TrimSpace(line), []byte("BEGIN")) {
					authenticated = true
				} else {
					_, _ = conn.Write([]byte("OK 1234deadbeef\r\n"))
				}
			}
			for authenticated {
				hdr, err := parseHeader(buf)
				if err != nil {
					break
				}
				msg, err := dbus.DecodeMessage(bytes.NewReader(buf[:hdr.size]))
				if err == nil {
					up.got <- msg
				}
				buf = buf[hdr.size:]
			}
		}
	}()
	return up
}

// startFilter runs a filter in front of the upstream and returns an app-side connection.
func startFilter(t *testing.T, meta schema.NotificationMeta, up *upstream) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "filtered")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	rly := &relay{pol: policyOf(meta)}
	go func() { _ = rly.serve(listener, up.path) }()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// The handshake the filter relays verbatim, leading NUL and all.
	if _, err := conn.Write([]byte("\x00AUTH EXTERNAL 31303030\r\nBEGIN\r\n")); err != nil {
		t.Fatal(err)
	}
	return conn
}

func (up *upstream) await(t *testing.T) *dbus.Message {
	t.Helper()
	select {
	case msg := <-up.got:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("nothing reached the bus behind the filter")
		return nil
	}
}

// The whole point: a policy rewrites the body, and the hints dictionary after it survives. That
// second half is what a byte patch would have broken, since D-Bus alignment is measured from the
// start of the message.
func TestFilter_RewritesSummaryActionsAndTimeout(t *testing.T) {
	up := fakeUpstream(t)
	app := startFilter(t, schema.NotificationMeta{
		UseCustomPrefix: true,
		CustomPrefix:    "[work]",
		// AllowedActions, AllowedProlonged and AllowedLinks all left off.
	}, up)

	call := notifyCall(t, "Build finished", `see <a href="http://x">the log</a>`, []string{"open", "Open"}, 0)
	if _, err := app.Write(call); err != nil {
		t.Fatal(err)
	}
	msg := up.await(t)

	if got := msg.Body[argSummary].(string); got != "[work] Build finished" {
		t.Errorf("summary = %q, want the prefix applied", got)
	}
	if got := msg.Body[argBody].(string); got != "see the log" {
		t.Errorf("body = %q, want the anchor markup stripped", got)
	}
	if got := msg.Body[argActions].([]string); len(got) != 0 {
		t.Errorf("actions = %v, want them dropped", got)
	}
	if got := msg.Body[argExpireTimeout].(int32); got != prolongedMs {
		t.Errorf("expire_timeout = %d, want it clamped to %d", got, prolongedMs)
	}
	// The hint after the rewritten fields must still decode, which is the alignment proof.
	hints, ok := msg.Body[6].(map[string]dbus.Variant)
	if !ok || hints["urgency"].Value() != byte(1) {
		t.Errorf("hints did not survive the rewrite: %#v", msg.Body[6])
	}
	// The app's own serial has to survive, or the reply it is waiting for never matches.
	if msg.Serial() != 42 {
		t.Errorf("serial = %d, want the app's own 42", msg.Serial())
	}
}

// A grant that allows everything must leave the message exactly as it was, byte for byte: an
// app that asked for nothing unusual should not be able to tell the filter is there.
func TestFilter_FullGrantForwardsUnchanged(t *testing.T) {
	up := fakeUpstream(t)
	app := startFilter(t, schema.NotificationMeta{
		AllowedActions:   true,
		AllowedProlonged: true,
		AllowedLinks:     true,
	}, up)

	call := notifyCall(t, "Summary", `a <a href="http://x">link</a>`, []string{"open", "Open"}, 0)
	if _, err := app.Write(call); err != nil {
		t.Fatal(err)
	}
	msg := up.await(t)
	if got := msg.Body[argSummary].(string); got != "Summary" {
		t.Errorf("summary = %q, want it untouched", got)
	}
	if got := msg.Body[argActions].([]string); len(got) != 2 {
		t.Errorf("actions = %v, want them kept", got)
	}
	if got := msg.Body[argExpireTimeout].(int32); got != 0 {
		t.Errorf("expire_timeout = %d, want it untouched", got)
	}
}

// Silenced answers the app itself: the call must not reach the bus, and the app must get a
// reply that looks like success, which is what distinguishes it from Disabled.
func TestFilter_SilencedAnswersTheAppAndSendsNothing(t *testing.T) {
	up := fakeUpstream(t)
	app := startFilter(t, schema.NotificationMeta{
		Silenced:         true,
		AllowedActions:   true,
		AllowedProlonged: true,
		AllowedLinks:     true,
	}, up)

	if _, err := app.Write(notifyCall(t, "Summary", "body", nil, -1)); err != nil {
		t.Fatal(err)
	}
	reply := readReply(t, app)
	if reply.Type != dbus.TypeMethodReply {
		t.Fatalf("want a method reply, got type %v", reply.Type)
	}
	if serial, ok := reply.Headers[dbus.FieldReplySerial].Value().(uint32); !ok || serial != 42 {
		t.Errorf("reply serial = %v, want the call's 42", reply.Headers[dbus.FieldReplySerial])
	}
	select {
	case msg := <-up.got:
		t.Fatalf("a silenced notification still reached the bus: %v", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

// Disabled refuses instead, so the app is told rather than left believing it notified.
func TestFilter_DisabledRefusesWithAnError(t *testing.T) {
	up := fakeUpstream(t)
	app := startFilter(t, schema.NotificationMeta{Disabled: true}, up)

	if _, err := app.Write(notifyCall(t, "Summary", "body", nil, -1)); err != nil {
		t.Fatal(err)
	}
	reply := readReply(t, app)
	if reply.Type != dbus.TypeError {
		t.Fatalf("want an error reply, got type %v", reply.Type)
	}
	if name, _ := reply.Headers[dbus.FieldErrorName].Value().(string); name != "org.freedesktop.DBus.Error.AccessDenied" {
		t.Errorf("error name = %q, want AccessDenied", name)
	}
	select {
	case msg := <-up.got:
		t.Fatalf("a disabled app's notification still reached the bus: %v", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

// Anything that is not a notification is not this filter's business and must pass through.
func TestFilter_OtherTrafficIsUntouched(t *testing.T) {
	up := fakeUpstream(t)
	app := startFilter(t, schema.NotificationMeta{Disabled: true}, up)

	msg := &dbus.Message{
		Type: dbus.TypeMethodCall,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/portal/desktop")),
			dbus.FieldInterface:   dbus.MakeVariant("org.freedesktop.portal.FileChooser"),
			dbus.FieldMember:      dbus.MakeVariant("OpenFile"),
			dbus.FieldDestination: dbus.MakeVariant("org.freedesktop.portal.Desktop"),
		},
	}
	var buf bytes.Buffer
	if err := msg.EncodeTo(&buf, binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	got := up.await(t)
	if member, _ := got.Headers[dbus.FieldMember].Value().(string); member != "OpenFile" {
		t.Fatalf("a portal call did not pass through: %v", got.Headers)
	}
}

func readReply(t *testing.T, conn *net.UnixConn) *dbus.Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		read, err := conn.Read(chunk)
		if read > 0 {
			buf = append(buf, chunk[:read]...)
		}
		// The bus's own authentication text comes back on this stream first, and it is not a
		// message. Step over it, or the first thing read is "OK <guid>" and no header parses.
		for len(buf) > 0 && buf[0] != 'l' && buf[0] != 'B' {
			line, rest, found := bytes.Cut(buf, []byte("\r\n"))
			if !found {
				break
			}
			_ = line
			buf = rest
		}
		if hdr, perr := parseHeader(buf); perr == nil {
			msg, derr := dbus.DecodeMessage(bytes.NewReader(buf[:hdr.size]))
			if derr != nil {
				t.Fatalf("decode the filter's reply: %v", derr)
			}
			return msg
		}
		if err != nil {
			t.Fatalf("no reply from the filter: %v", err)
		}
	}
}
