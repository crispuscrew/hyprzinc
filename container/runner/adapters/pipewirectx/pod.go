package pipewirectx

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PipeWire's wire format is POD: an 8-byte header of size and type, the body, then padding to
// the next 8-byte boundary. size counts the body alone, which is why every reader here pads on
// the way past and no writer ever states the padded length.
//
// Only the types Zinc sends or reads are here. Fd carries an INDEX into the message's
// SCM_RIGHTS array rather than a descriptor number, which is what lets one message hand over
// two descriptors without either side naming a number the other cannot resolve.
const (
	podInt    uint32 = 4
	podString uint32 = 8
	podStruct uint32 = 14
	podFd     uint32 = 18
)

var wire = binary.LittleEndian

func pad8(size int) int { return (size + 7) &^ 7 }

// pod frames one value. The body is copied, so a caller may reuse its buffer.
func pod(typ uint32, body []byte) []byte {
	out := make([]byte, 8, 8+pad8(len(body)))
	wire.PutUint32(out[0:], uint32(len(body)))
	wire.PutUint32(out[4:], typ)
	out = append(out, body...)
	for len(out) < cap(out) {
		out = append(out, 0)
	}
	return out
}

func intPod(val int32) []byte {
	body := make([]byte, 4)
	wire.PutUint32(body, uint32(val))
	return pod(podInt, body)
}

// fdPod names a descriptor by its position in the message's SCM_RIGHTS array, not by number.
func fdPod(index int64) []byte {
	body := make([]byte, 8)
	wire.PutUint64(body, uint64(index))
	return pod(podFd, body)
}

// stringPod writes a NUL-terminated string; the length INCLUDES that NUL.
func stringPod(val string) []byte { return pod(podString, append([]byte(val), 0)) }

func structPod(fields ...[]byte) []byte {
	var body []byte
	for _, field := range fields {
		body = append(body, field...)
	}
	return pod(podStruct, body)
}

// dictPod writes a property dictionary, which on the wire is a struct of a count followed by
// that many key/value string pairs. Keys and values alternate in kv.
func dictPod(kv ...string) []byte {
	fields := [][]byte{intPod(int32(len(kv) / 2))}
	for _, item := range kv {
		fields = append(fields, stringPod(item))
	}
	return structPod(fields...)
}

var errShortPod = errors.New("pipewire: truncated POD")

// reader walks the fields of one struct body in order.
type reader struct {
	buf []byte
	off int
}

func (rdr *reader) next() (typ uint32, body []byte, err error) {
	if rdr.off+8 > len(rdr.buf) {
		return 0, nil, errShortPod
	}
	size := int(wire.Uint32(rdr.buf[rdr.off:]))
	typ = wire.Uint32(rdr.buf[rdr.off+4:])
	if size < 0 || rdr.off+8+size > len(rdr.buf) {
		return 0, nil, errShortPod
	}
	body = rdr.buf[rdr.off+8 : rdr.off+8+size]
	rdr.off += 8 + pad8(size)
	return typ, body, nil
}

func (rdr *reader) int32() (int32, error) {
	typ, body, err := rdr.next()
	if err != nil {
		return 0, err
	}
	if typ != podInt || len(body) < 4 {
		return 0, fmt.Errorf("pipewire: want an int, got type %d", typ)
	}
	return int32(wire.Uint32(body)), nil
}

func (rdr *reader) string() (string, error) {
	typ, body, err := rdr.next()
	if err != nil {
		return "", err
	}
	if typ != podString {
		return "", fmt.Errorf("pipewire: want a string, got type %d", typ)
	}
	// The stated length includes the terminating NUL, which is not part of the value.
	if len(body) > 0 && body[len(body)-1] == 0 {
		body = body[:len(body)-1]
	}
	return string(body), nil
}

// skip advances past one field whose value is not needed.
func (rdr *reader) skip() error {
	_, _, err := rdr.next()
	return err
}

// structure descends into a nested struct.
func (rdr *reader) structure() (*reader, error) {
	typ, body, err := rdr.next()
	if err != nil {
		return nil, err
	}
	if typ != podStruct {
		return nil, fmt.Errorf("pipewire: want a struct, got type %d", typ)
	}
	return &reader{buf: body}, nil
}

// dict reads a property dictionary. A count that outruns the body is an error rather than a
// short map: these properties decide what an app is allowed to reach.
func (rdr *reader) dict() (map[string]string, error) {
	inner, err := rdr.structure()
	if err != nil {
		return nil, err
	}
	count, err := inner.int32()
	if err != nil {
		return nil, err
	}
	props := make(map[string]string, count)
	for item := int32(0); item < count; item++ {
		key, err := inner.string()
		if err != nil {
			return nil, err
		}
		value, err := inner.string()
		if err != nil {
			return nil, err
		}
		props[key] = value
	}
	return props, nil
}
