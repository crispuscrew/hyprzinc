package pipewirectx

import (
	"net"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A stand-in PipeWire daemon, so the part of this package that decides what an app may reach is
// checked wherever the tests run. The live tests beside these need a real daemon and skip
// without one, which on CI is always - and a permission rule nothing checks is a permission rule
// that quietly stops working.
//
// It speaks only the handful of messages Zinc sends, and it answers them the way the real daemon
// was measured to: globals arrive after the client states its properties, never before, and a
// client's identity is in its info event rather than in its registry global.

// longPod writes a 64-bit value, which is what a client info event's change mask is. The fake
// sends the real type rather than a convenient one, so a decoder that only works against a
// four-byte field fails here as it would against the daemon.
func longPod(val int64) []byte {
	body := make([]byte, 8)
	wire.PutUint64(body, uint64(val))
	return pod(podLong, body)
}

const podLong uint32 = 5

// permissionSet is one recorded UpdatePermissions call, in the order it was sent.
type permissionSet struct {
	client uint32
	perms  map[int32]int32
	order  []int32
}

type fakeDaemon struct {
	path string

	mu sync.Mutex
	// globals is what the registry has still to report; sent is what it already has.
	globals []global
	sent    []global
	// applied records every UpdatePermissions the daemon received.
	applied []permissionSet
	// created records the properties a security context was created with.
	created   map[string]string
	createFDs int

	// registry is the proxy id the client asked the registry to be bound to.
	registry uint32
	// bound maps a proxy id to the global it was bound to.
	bound map[uint32]global
	// stated is set once the client sends its properties, which is what the real daemon waits
	// for before the session manager grants it anything to see.
	stated bool
	conn   *net.UnixConn
}

func startFakeDaemon(t *testing.T, globals []global) *fakeDaemon {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipewire-0")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	daemon := &fakeDaemon{path: path, globals: globals, bound: map[uint32]global{}}
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go daemon.serve(conn)
		}
	}()
	return daemon
}

func (fake *fakeDaemon) serve(conn *net.UnixConn) {
	defer conn.Close()
	fake.mu.Lock()
	fake.conn = conn
	fake.mu.Unlock()

	var buf []byte
	chunk := make([]byte, 64<<10)
	oob := make([]byte, 4096)
	for {
		read, oobRead, _, _, err := conn.ReadMsgUnix(chunk, oob)
		if read > 0 {
			buf = append(buf, chunk[:read]...)
		}
		if oobRead > 0 {
			fake.mu.Lock()
			fake.createFDs += countRights(oob[:oobRead])
			fake.mu.Unlock()
		}
		for {
			msg, size, ok := parse(buf)
			if !ok {
				break
			}
			fake.handle(conn, msg)
			buf = buf[size:]
		}
		if err != nil {
			return
		}
	}
}

func (fake *fakeDaemon) handle(conn *net.UnixConn, msg message) {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	switch {
	case msg.id == clientID && msg.opcode == clientMethodUpdateProperties:
		// The measured behaviour: nothing is visible until this arrives.
		fake.stated = true
		fake.emitGlobals(conn)

	case msg.id == coreID && msg.opcode == coreMethodGetRegistry:
		if fields, err := (&reader{buf: msg.body}).structure(); err == nil {
			_, _ = fields.int32() // version
			if id, err := fields.int32(); err == nil {
				fake.registry = uint32(id)
			}
		}
		fake.emitGlobals(conn)

	case msg.id == coreID && msg.opcode == coreMethodSync:
		_ = send(conn, coreID, coreEventDone, structPod(intPod(0), intPod(1)))

	case msg.id == fake.registry && msg.opcode == registryMethodBind:
		fake.bind(conn, msg)

	case msg.opcode == clientMethodUpdatePermissions:
		fake.record(msg)

	case msg.opcode == securityContextMethodCreate:
		fake.recordCreate(msg)
	}
}

// emitGlobals sends the graph, but only once the client has stated its properties. Sending
// before that would let a bug that skips Client.UpdateProperties pass here and fail against a
// real daemon, which is the exact trap this package already fell into once.
func (fake *fakeDaemon) emitGlobals(conn *net.UnixConn) {
	if !fake.stated || fake.registry == 0 {
		return
	}
	for _, item := range fake.globals {
		_ = send(conn, fake.registry, registryEventGlobal, globalPod(item))
		fake.sent = append(fake.sent, item)
	}
	fake.globals = nil
}

func (fake *fakeDaemon) bind(conn *net.UnixConn, msg message) {
	fields, err := (&reader{buf: msg.body}).structure()
	if err != nil {
		return
	}
	globalID, err := fields.int32()
	if err != nil {
		return
	}
	if _, err := fields.string(); err != nil { // interface
		return
	}
	if _, err := fields.int32(); err != nil { // version
		return
	}
	proxy, err := fields.int32()
	if err != nil {
		return
	}
	for _, item := range fake.all() {
		if item.id != uint32(globalID) {
			continue
		}
		fake.bound[uint32(proxy)] = item
		// The identity lives in the info event, not in the global.
		_ = send(conn, uint32(proxy), clientEventInfo,
			structPod(intPod(globalID), longPod(0), dictPod(flatten(item.props)...)))
	}
}

func (fake *fakeDaemon) record(msg message) {
	fields, err := (&reader{buf: msg.body}).structure()
	if err != nil {
		return
	}
	count, err := fields.int32()
	if err != nil {
		return
	}
	set := permissionSet{client: msg.id, perms: map[int32]int32{}}
	for item := int32(0); item < count; item++ {
		id, err := fields.int32()
		if err != nil {
			return
		}
		perm, err := fields.int32()
		if err != nil {
			return
		}
		set.perms[id] = perm
		set.order = append(set.order, id)
	}
	fake.applied = append(fake.applied, set)
}

func (fake *fakeDaemon) recordCreate(msg message) {
	fields, err := (&reader{buf: msg.body}).structure()
	if err != nil {
		return
	}
	if err := fields.skip(); err != nil { // listen fd
		return
	}
	if err := fields.skip(); err != nil { // close fd
		return
	}
	if props, err := fields.dict(); err == nil {
		fake.created = props
	}
}

// all returns every global the daemon knows, whether or not it has been sent yet.
func (fake *fakeDaemon) all() []global {
	return append(append([]global{}, fake.globals...), fake.sent...)
}

// lastApplied returns the most recent permission set, waiting for one to arrive.
func (fake *fakeDaemon) lastApplied(t *testing.T, window time.Duration) (permissionSet, bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		count := len(fake.applied)
		var last permissionSet
		if count > 0 {
			last = fake.applied[count-1]
		}
		fake.mu.Unlock()
		if count > 0 {
			return last, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return permissionSet{}, false
}

func flatten(props map[string]string) []string {
	// Sorted, so the fake sends the same bytes twice for the same graph.
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sortStrings(keys)
	out := make([]string, 0, len(props)*2)
	for _, key := range keys {
		out = append(out, key, props[key])
	}
	return out
}

func globalPod(item global) []byte {
	return structPod(
		intPod(int32(item.id)),
		intPod(int32(permRX)),
		stringPod(item.iface),
		intPod(3),
		dictPod(flatten(item.props)...),
	)
}

func sortStrings(values []string) { sort.Strings(values) }

// countRights counts the descriptors in a control message, which is how the fake checks that a
// security context was handed both the listening socket and its revocation pipe.
func countRights(oob []byte) int {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	total := 0
	for _, scm := range scms {
		if fds, err := syscall.ParseUnixRights(&scm); err == nil {
			total += len(fds)
			for _, fd := range fds {
				_ = syscall.Close(fd)
			}
		}
	}
	return total
}

// appear adds a global after the fact, the way a microphone plugged in mid-session does.
func (fake *fakeDaemon) appear(item global) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	conn := fake.conn
	if conn == nil || fake.registry == 0 {
		fake.globals = append(fake.globals, item)
		return
	}
	fake.sent = append(fake.sent, item)
	_ = send(conn, fake.registry, registryEventGlobal, globalPod(item))
}

// send writes one message from the daemon's side.
func send(conn *net.UnixConn, id uint32, opcode uint8, payload []byte) error {
	head := make([]byte, headerSize)
	wire.PutUint32(head[0:], id)
	wire.PutUint32(head[4:], uint32(opcode)<<24|uint32(len(payload)))
	_, err := conn.Write(append(head, payload...))
	return err
}
