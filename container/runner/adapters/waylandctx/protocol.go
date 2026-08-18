package waylandctx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// ErrUnsupported is the one failure the caller is expected to survive: the compositor did
// not advertise wp_security_context_manager_v1, so there is no security context to be had
// on this desktop. It is a sentinel rather than a message because the launch path treats it
// differently from every other error - see Establish.
var ErrUnsupported = errors.New("the compositor does not implement wp_security_context_v1")

// The protocol constants, spelled out rather than generated. A scanner would be a build-time
// dependency and a code generator for four requests and three events, none of which will
// change: the interface is version 1 and any incompatible change becomes a new interface
// name by the protocol's own rules.
const (
	// wl_display is always object 1 - the one id a client does not have to be told.
	displayObject = 1

	displaySync        = 0 // wl_display.sync(new_id callback)
	displayGetRegistry = 1 // wl_display.get_registry(new_id registry)

	displayErrorEvent    = 0 // wl_display.error(object_id, code, message)
	displayDeleteIDEvent = 1 // wl_display.delete_id(id)

	registryBind        = 0 // wl_registry.bind(name, new_id) - the new_id is UNTYPED, see bindRequest
	registryGlobalEvent = 0 // wl_registry.global(name, interface, version)

	callbackDoneEvent = 0 // wl_callback.done(data)

	managerCreateListener = 1 // wp_security_context_manager_v1.create_listener(new_id, fd, fd)

	contextSetSandboxEngine = 1 // wp_security_context_v1.set_sandbox_engine(string)
	contextSetAppID         = 2 // .set_app_id(string)
	contextSetInstanceID    = 3 // .set_instance_id(string)
	contextCommit           = 4 // .commit()
)

// managerInterface is the global to look for, and managerVersion the version to bind. The
// interface is at version 1 and the requests used here are all it has, so binding higher
// than the compositor advertises is never useful - bindVersion takes the lower of the two.
const (
	managerInterface = "wp_security_context_manager_v1"
	managerVersion   = 1
)

// firstClientID is the first object id a client may allocate. Ids 1..0xfeffffff are the
// client's half of the id space and 1 is wl_display, so counting starts at 2.
const firstClientID = 2

// headerSize is the fixed part of every message: the object id, then the size and opcode
// packed into one word. The size INCLUDES these 8 bytes, which is the single easiest thing
// to get wrong here - a header excluded from the size desynchronises the stream and the
// compositor answers with a protocol error about an object that has nothing to do with it.
const headerSize = 8

// replyTimeout bounds every read from the compositor. Nothing here is a long operation - the
// whole exchange is two roundtrips - so a compositor that has not answered in this long is
// wedged, and a launch must not hang on it forever. It is generous because the compositor
// may be busy rendering when we ask.
const replyTimeout = 5 * time.Second

// wire is the byte order of the Wayland wire format: the HOST's, not a fixed one, so this is
// NativeEndian rather than LittleEndian. Every platform Zinc runs on today is little-endian;
// using the native order means a big-endian build is correct instead of silently producing
// byte-swapped headers that the compositor would read as absurd message sizes.
var wire = binary.NativeEndian

// request frames one request: the target object, the opcode, and the already-encoded
// argument body. Everything sent to the compositor goes through here so the size field is
// computed in exactly one place.
func request(object uint32, opcode uint16, body []byte) []byte {
	msg := make([]byte, headerSize+len(body))
	wire.PutUint32(msg[0:4], object)
	wire.PutUint32(msg[4:8], uint32(len(msg))<<16|uint32(opcode))
	copy(msg[headerSize:], body)
	return msg
}

// appendUint32 adds one uint/int/new_id argument. All three are the same on the wire.
func appendUint32(body []byte, val uint32) []byte { return wire.AppendUint32(body, val) }

// appendString adds one string argument: a uint32 length INCLUDING the trailing NUL, the bytes, that
// NUL, then padding to the next 4-byte boundary. The message is a sequence of 32-bit words, so an
// unpadded string shifts every argument after it.
func appendString(body []byte, str string) []byte {
	body = wire.AppendUint32(body, uint32(len(str)+1))
	body = append(body, str...)
	body = append(body, 0)
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	return body
}

// bindRequest is wl_registry.bind. Its new_id argument is the one place the wire format is
// not obvious: bind's id is declared without an interface, so the client has to say which
// interface and version it is binding, and an untyped new_id encodes as string(interface) +
// uint32(version) + uint32(id) rather than the bare id a typed new_id gets.
func bindRequest(registry, name uint32, iface string, version, newID uint32) []byte {
	body := appendUint32(nil, name)
	body = appendString(body, iface)
	body = appendUint32(body, version)
	body = appendUint32(body, newID)
	return request(registry, registryBind, body)
}

// stringRequest is any of the security context's three setters: one string argument.
func stringRequest(object uint32, opcode uint16, value string) []byte {
	return request(object, opcode, appendString(nil, value))
}

// message is one decoded message. body is the argument bytes with the header stripped.
type message struct {
	object uint32
	opcode uint16
	body   []byte
}

// nextMessage splits one message off the front of buf and reports how many bytes it took.
// A zero size with a nil error means buf does not hold a whole message yet and the caller
// should read more - the stream is a byte stream, so a short read in the middle of a message
// is ordinary rather than exceptional.
func nextMessage(buf []byte) (message, int, error) {
	if len(buf) < headerSize {
		return message{}, 0, nil
	}
	object := wire.Uint32(buf[0:4])
	sizeOpcode := wire.Uint32(buf[4:8])
	size, opcode := int(sizeOpcode>>16), uint16(sizeOpcode)
	if size < headerSize || size%4 != 0 {
		return message{}, 0, fmt.Errorf("malformed wayland message: object %d declares size %d (must be at least %d and a multiple of 4)", object, size, headerSize)
	}
	if len(buf) < size {
		return message{}, 0, nil
	}
	return message{object: object, opcode: opcode, body: buf[headerSize:size]}, size, nil
}

// takeString reads a string argument at offset off and returns the offset after it, padding
// included. It refuses a length that runs past the message rather than slicing blindly: the
// length is a number the compositor sent, and everything else here trusts it.
func takeString(body []byte, off int) (string, int, error) {
	if off+4 > len(body) {
		return "", 0, fmt.Errorf("wayland string at offset %d: message ends before its length", off)
	}
	length := int(wire.Uint32(body[off : off+4]))
	off += 4
	if length < 1 || off+length > len(body) {
		return "", 0, fmt.Errorf("wayland string at offset %d: declared length %d does not fit in the remaining %d bytes", off, length, len(body)-off)
	}
	str := string(body[off : off+length-1]) // drop the trailing NUL the length counts
	off += length
	for off%4 != 0 {
		off++
	}
	return str, off, nil
}

// decodeGlobal reads a wl_registry.global event: which name to bind, what it is, and how new
// the compositor's copy is.
func decodeGlobal(body []byte) (name uint32, iface string, version uint32, err error) {
	if len(body) < 4 {
		return 0, "", 0, fmt.Errorf("wl_registry.global: body is %d bytes, too short for a name", len(body))
	}
	name = wire.Uint32(body[0:4])
	iface, off, err := takeString(body, 4)
	if err != nil {
		return 0, "", 0, fmt.Errorf("wl_registry.global: %w", err)
	}
	if off+4 > len(body) {
		return 0, "", 0, fmt.Errorf("wl_registry.global %q: body ends before its version", iface)
	}
	return name, iface, wire.Uint32(body[off : off+4]), nil
}

// protocolError is a wl_display.error turned into a Go error. Decoding it is the difference
// between "the compositor closed the connection" and knowing which object and which rule -
// and since a security context is committed and then dropped, an undecoded protocol error
// would be the entire diagnosis of a launch that silently produced no context at all.
type protocolError struct {
	object  uint32
	code    uint32
	message string
}

func (perr protocolError) Error() string {
	return fmt.Sprintf("the compositor rejected the request: object %d, code %d: %s", perr.object, perr.code, perr.message)
}

// decodeDisplayError turns a wl_display.error event body into a protocolError. A body that
// cannot be decoded still produces an error, because the event itself is the news.
func decodeDisplayError(body []byte) error {
	if len(body) < 8 {
		return fmt.Errorf("the compositor sent a protocol error, but its body is %d bytes and cannot be decoded", len(body))
	}
	perr := protocolError{object: wire.Uint32(body[0:4]), code: wire.Uint32(body[4:8])}
	text, _, err := takeString(body, 8)
	if err != nil {
		perr.message = "(undecodable message: " + err.Error() + ")"
	} else {
		perr.message = text
	}
	return perr
}

// client is one connection to the compositor. It exists for the length of the exchange and
// nothing outlives it: the protocol guarantees the compositor keeps accepting on the
// listening fd after the creating client disconnects, which is what lets `zcr` hold the
// security context with a pipe instead of a live Wayland connection.
type client struct {
	conn   *net.UnixConn
	buf    []byte // bytes read but not yet parsed into whole messages
	nextID uint32
}

func dial(path string) (*client, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect to the compositor at %s: %w", path, err)
	}
	return &client{conn: conn, nextID: firstClientID}, nil
}

func (cli *client) Close() error { return cli.conn.Close() }

// newID allocates the next client-side object id.
func (cli *client) newID() uint32 {
	id := cli.nextID
	cli.nextID++
	return id
}

// send writes one message, optionally carrying file descriptors. An fd argument is NOT in
// the message body - it travels as SCM_RIGHTS ancillary data on the message that declares
// it, and the compositor pairs the two by position. So the fds go on this write and no
// other, which is why create_listener is sent on its own rather than batched.
func (cli *client) send(msg []byte, fds ...int) error {
	if err := cli.conn.SetWriteDeadline(time.Now().Add(replyTimeout)); err != nil {
		return err
	}
	if len(fds) == 0 {
		_, err := cli.conn.Write(msg)
		return err
	}
	_, _, err := cli.conn.WriteMsgUnix(msg, syscall.UnixRights(fds...), nil)
	return err
}

// next reads from the socket until one message is complete. Plain Read rather than ReadMsgUnix: the
// only global bound here is the security context manager, which sends no events at all, so a control
// buffer would only be a thing to get wrong.
func (cli *client) next() (message, error) {
	for {
		msg, size, err := nextMessage(cli.buf)
		if err != nil {
			return message{}, err
		}
		if size > 0 {
			// Copy the body out: the buffer below is appended to and re-sliced, and the
			// caller keeps the message past the next read.
			msg.body = append([]byte(nil), msg.body...)
			cli.buf = cli.buf[size:]
			return msg, nil
		}
		if err := cli.conn.SetReadDeadline(time.Now().Add(replyTimeout)); err != nil {
			return message{}, err
		}
		chunk := make([]byte, 4096)
		read, err := cli.conn.Read(chunk)
		if read > 0 {
			cli.buf = append(cli.buf, chunk[:read]...)
		}
		if err != nil {
			return message{}, fmt.Errorf("read from the compositor: %w", err)
		}
	}
}

// roundtrip sends wl_display.sync and reads events until the compositor answers, handing the rest to
// handle. It is how a request-only protocol is made observable: without a sync a protocol error would
// arrive after this process had disconnected. wl_display.error is checked on every event, not only at
// the end, or a precise error becomes a timeout waiting for a callback that will never come.
func (cli *client) roundtrip(handle func(message) error) error {
	callback := cli.newID()
	if err := cli.send(request(displayObject, displaySync, appendUint32(nil, callback))); err != nil {
		return fmt.Errorf("wl_display.sync: %w", err)
	}
	for {
		msg, err := cli.next()
		if err != nil {
			return err
		}
		switch {
		case msg.object == displayObject && msg.opcode == displayErrorEvent:
			return decodeDisplayError(msg.body)
		case msg.object == callback && msg.opcode == callbackDoneEvent:
			return nil
		case msg.object == displayObject && msg.opcode == displayDeleteIDEvent:
			// The compositor reclaiming an id we are done with (the sync callback of a
			// previous roundtrip). Nothing here reuses ids, so there is nothing to record.
		default:
			if handle != nil {
				if err := handle(msg); err != nil {
					return err
				}
			}
		}
	}
}

// register performs the whole exchange against a running compositor: find the manager, bind
// it, hand over the listening socket and the revocation pipe, attach the identity, commit,
// and confirm. listenFD must already be bound and listening; both descriptors are the
// caller's to close once this returns, because the compositor has its own copies.
func register(compositor string, listenFD, closeFD int, engine, appID, instanceID string) error {
	cli, err := dial(compositor)
	if err != nil {
		return err
	}
	defer cli.Close()

	registry := cli.newID()
	if err := cli.send(request(displayObject, displayGetRegistry, appendUint32(nil, registry))); err != nil {
		return fmt.Errorf("wl_display.get_registry: %w", err)
	}
	var (
		name      uint32
		advertise uint32
		found     bool
	)
	// The globals arrive as a burst right after get_registry, but "right after" is not a
	// guarantee - the roundtrip is what makes the set complete, because the compositor
	// answers a sync only once it has sent everything queued before it.
	if err := cli.roundtrip(func(msg message) error {
		if msg.object != registry || msg.opcode != registryGlobalEvent {
			return nil
		}
		globalName, iface, version, err := decodeGlobal(msg.body)
		if err != nil {
			return err
		}
		if iface == managerInterface {
			name, advertise, found = globalName, version, true
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return ErrUnsupported
	}

	bindVersion := advertise
	if bindVersion > managerVersion {
		bindVersion = managerVersion
	}
	manager := cli.newID()
	if err := cli.send(bindRequest(registry, name, managerInterface, bindVersion, manager)); err != nil {
		return fmt.Errorf("wl_registry.bind %s: %w", managerInterface, err)
	}

	context := cli.newID()
	if err := cli.send(request(manager, managerCreateListener, appendUint32(nil, context)), listenFD, closeFD); err != nil {
		return fmt.Errorf("wp_security_context_manager_v1.create_listener: %w", err)
	}

	for _, setter := range []struct {
		opcode uint16
		value  string
		what   string
	}{
		{contextSetSandboxEngine, engine, "set_sandbox_engine"},
		{contextSetAppID, appID, "set_app_id"},
		{contextSetInstanceID, instanceID, "set_instance_id"},
	} {
		if err := cli.send(stringRequest(context, setter.opcode, setter.value)); err != nil {
			return fmt.Errorf("wp_security_context_v1.%s: %w", setter.what, err)
		}
	}
	if err := cli.send(request(context, contextCommit, nil)); err != nil {
		return fmt.Errorf("wp_security_context_v1.commit: %w", err)
	}
	// The second roundtrip is the whole point of committing before disconnecting: without
	// it, a rejected metadata set (or a listen fd the compositor would not take) would be
	// reported into a connection this process had already dropped, and the launch would
	// carry on believing the app was contained.
	return cli.roundtrip(nil)
}
