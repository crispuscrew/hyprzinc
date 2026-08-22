package pipewirectx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// The native protocol's object ids and opcodes. Only what Zinc sends or reads.
//
// A client does not choose its ids freely: the core is 0, the client proxy is 1, and a proxy a
// client allocates must be the next free number. Asking for anything else is refused with
// "can't add id N for client: No space left on device" (measured), so ids are handed out by
// nextID rather than written as literals.
const (
	coreID   uint32 = 0
	clientID uint32 = 1
	firstID  uint32 = 2
)

const (
	coreMethodHello       uint8 = 1
	coreMethodSync        uint8 = 2
	coreMethodGetRegistry uint8 = 5

	coreEventInfo  uint8 = 0
	coreEventDone  uint8 = 1
	coreEventPing  uint8 = 2
	coreEventError uint8 = 3

	clientMethodUpdateProperties  uint8 = 2
	clientMethodUpdatePermissions uint8 = 4

	registryMethodBind uint8 = 1

	registryEventGlobal       uint8 = 0
	registryEventGlobalRemove uint8 = 1

	clientEventInfo uint8 = 0

	securityContextMethodCreate uint8 = 1
)

// coreVersion is PW_VERSION_CORE. Version 4 is what pipewire 1.x speaks; announcing 3 gets
// BoundId events where 4 gets BoundProps, and nothing else Zinc uses differs.
const coreVersion int32 = 4

const (
	interfaceRegistry        = "PipeWire:Interface:Registry"
	interfaceClient          = "PipeWire:Interface:Client"
	interfaceNode            = "PipeWire:Interface:Node"
	interfaceSecurityContext = "PipeWire:Interface:SecurityContext"
)

// permNone and permRX are PipeWire permission masks. R is "may see it", W "may change it",
// X "may call it", M "may set metadata". Removing R alone hides an object from the registry;
// removing X as well is what stops a client that already learned an id from acting on it.
const (
	permNone int32 = 0
	permRX   int32 = 0o5 // PW_PERM_R | PW_PERM_X
)

// permIDAny is PW_ID_ANY: the default applied to every object without its own entry.
const permIDAny int32 = -1

const headerSize = 16

// replyTimeout bounds a single exchange. The daemon is a local process, so a wait this long
// means something is wrong rather than busy.
const replyTimeout = 5 * time.Second

var errClosed = errors.New("pipewire: the connection closed before the reply arrived")

type message struct {
	id     uint32
	opcode uint8
	body   []byte
}

// conn is one connection to a PipeWire socket.
type conn struct {
	sock   *net.UnixConn
	buf    []byte
	nextID uint32
	seq    uint32
}

func dial(path string) (*conn, error) {
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("pipewire socket %s: %w", path, err)
	}
	sock, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	return &conn{sock: sock, nextID: firstID}, nil
}

func (cnn *conn) close() { _ = cnn.sock.Close() }

// take allocates the next proxy id. Sequential because the daemon requires it.
func (cnn *conn) take() uint32 {
	id := cnn.nextID
	cnn.nextID++
	return id
}

// send writes one message. fds travel as SCM_RIGHTS and the header states how many, which is
// how the Fd PODs in the payload resolve: they carry an index into this array.
func (cnn *conn) send(id uint32, opcode uint8, payload []byte, fds ...int) error {
	head := make([]byte, headerSize)
	wire.PutUint32(head[0:], id)
	wire.PutUint32(head[4:], uint32(opcode)<<24|uint32(len(payload)))
	wire.PutUint32(head[8:], cnn.seq)
	wire.PutUint32(head[12:], uint32(len(fds)))
	cnn.seq++

	frame := append(head, payload...)
	if len(fds) == 0 {
		_, err := cnn.sock.Write(frame)
		return err
	}
	_, _, err := cnn.sock.WriteMsgUnix(frame, syscall.UnixRights(fds...), nil)
	return err
}

// recv returns the next message, reading more from the socket when the buffer holds no whole
// one. Ancillary data is discarded: nothing Zinc binds sends a descriptor back.
func (cnn *conn) recv(deadline time.Time) (message, error) {
	for {
		if msg, size, ok := parse(cnn.buf); ok {
			cnn.buf = cnn.buf[size:]
			return msg, nil
		}
		if err := cnn.sock.SetReadDeadline(deadline); err != nil {
			return message{}, err
		}
		chunk := make([]byte, 64<<10)
		read, err := cnn.sock.Read(chunk)
		if read > 0 {
			cnn.buf = append(cnn.buf, chunk[:read]...)
			continue
		}
		if err != nil {
			return message{}, err
		}
		return message{}, errClosed
	}
}

// parse reads one message out of buf, reporting how many bytes it consumed.
func parse(buf []byte) (message, int, bool) {
	if len(buf) < headerSize {
		return message{}, 0, false
	}
	id := wire.Uint32(buf)
	opcodeSize := wire.Uint32(buf[4:])
	size := int(opcodeSize & 0xffffff)
	total := headerSize + size
	if size < 0 || len(buf) < total {
		return message{}, 0, false
	}
	return message{id: id, opcode: uint8(opcodeSize >> 24), body: buf[headerSize:total]}, total, true
}

// hello performs the opening exchange and returns a connection the registry will actually
// answer on.
//
// The Client.UpdateProperties is not politeness, it is load bearing: without it the session
// manager never grants this client any permission, and a registry created afterwards reports
// no globals at all - no error, no close, simply nothing (measured against wireplumber 0.5).
func (cnn *conn) hello(props ...string) error {
	if err := cnn.send(coreID, coreMethodHello, structPod(intPod(coreVersion))); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if err := cnn.send(clientID, clientMethodUpdateProperties, structPod(dictPod(props...))); err != nil {
		return fmt.Errorf("update properties: %w", err)
	}
	return nil
}

// sync asks the daemon to echo a sequence back, which is how a caller knows every event
// caused by what it sent so far has already arrived.
func (cnn *conn) sync() error {
	return cnn.send(coreID, coreMethodSync, structPod(intPod(0), intPod(int32(cnn.seq))))
}

// getRegistry binds the registry and returns its proxy id.
func (cnn *conn) getRegistry() (uint32, error) {
	id := cnn.take()
	err := cnn.send(coreID, coreMethodGetRegistry, structPod(intPod(coreVersion), intPod(int32(id))))
	if err != nil {
		return 0, fmt.Errorf("get registry: %w", err)
	}
	return id, nil
}

// bind binds one global and returns the proxy id it was bound to.
func (cnn *conn) bind(registry, global uint32, iface string, version int32) (uint32, error) {
	id := cnn.take()
	payload := structPod(intPod(int32(global)), stringPod(iface), intPod(version), intPod(int32(id)))
	if err := cnn.send(registry, registryMethodBind, payload); err != nil {
		return 0, fmt.Errorf("bind %s: %w", iface, err)
	}
	return id, nil
}

// global is one entry the registry reported.
type global struct {
	id    uint32
	iface string
	props map[string]string
}

// decodeGlobal reads a registry global event: id, permissions, type, version, properties.
func decodeGlobal(body []byte) (global, error) {
	top := &reader{buf: body}
	fields, err := top.structure()
	if err != nil {
		return global{}, err
	}
	id, err := fields.int32()
	if err != nil {
		return global{}, err
	}
	if err := fields.skip(); err != nil { // permissions
		return global{}, err
	}
	iface, err := fields.string()
	if err != nil {
		return global{}, err
	}
	if err := fields.skip(); err != nil { // version
		return global{}, err
	}
	props, err := fields.dict()
	if err != nil {
		// A global with unreadable properties is still a global worth knowing about; the
		// caller matches on the interface for some of them.
		props = map[string]string{}
	}
	return global{id: uint32(id), iface: iface, props: props}, nil
}

// decodeGlobalRemove reads the id out of a registry global_remove event.
func decodeGlobalRemove(body []byte) (uint32, error) {
	fields, err := (&reader{buf: body}).structure()
	if err != nil {
		return 0, err
	}
	id, err := fields.int32()
	return uint32(id), err
}

// decodeClientInfo reads a client info event: id, change mask, properties.
//
// The identity a security context stamps lands HERE and not in the registry's global event,
// which carries only a subset - measured: a client whose info says
// pipewire.sec.app-id="zinc-diag" shows an empty one in its global, and so does every flatpak
// client on the system. Matching an app on the global props therefore matches nothing.
func decodeClientInfo(body []byte) (map[string]string, error) {
	fields, err := (&reader{buf: body}).structure()
	if err != nil {
		return nil, err
	}
	if err := fields.skip(); err != nil { // id
		return nil, err
	}
	if err := fields.skip(); err != nil { // change mask
		return nil, err
	}
	return fields.dict()
}

type protocolError struct {
	id      uint32
	code    int32
	message string
}

func (perr protocolError) Error() string {
	return fmt.Sprintf("pipewire: object %d: %s (code %d)", perr.id, perr.message, perr.code)
}

// decodeError reads a core error event: id, seq, res, message.
func decodeError(body []byte) error {
	fields, err := (&reader{buf: body}).structure()
	if err != nil {
		return fmt.Errorf("pipewire: unreadable error event: %w", err)
	}
	id, err := fields.int32()
	if err != nil {
		return err
	}
	if err := fields.skip(); err != nil { // seq
		return err
	}
	code, err := fields.int32()
	if err != nil {
		return err
	}
	text, err := fields.string()
	if err != nil {
		return err
	}
	return protocolError{id: uint32(id), code: code, message: text}
}

// pump reads events until handle says to stop, turning a core error into a returned error so a
// caller never waits for a reply the daemon has already refused to send.
func (cnn *conn) pump(deadline time.Time, handle func(message) (bool, error)) error {
	for {
		msg, err := cnn.recv(deadline)
		if err != nil {
			return err
		}
		if msg.id == coreID && msg.opcode == coreEventError {
			return decodeError(msg.body)
		}
		if msg.id == coreID && msg.opcode == coreEventPing {
			continue
		}
		done, err := handle(msg)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// runtimeSocket resolves one of the session's PipeWire sockets by name.
func runtimeSocket(runtimeDir, name string) (string, error) {
	if runtimeDir == "" {
		return "", errors.New("pipewire: no XDG_RUNTIME_DIR, so the session's socket cannot be found")
	}
	path := runtimeDir + "/" + name
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("pipewire socket %s: %w", path, err)
	}
	return path, nil
}
