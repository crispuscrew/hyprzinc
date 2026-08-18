package notifyfilter

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Just enough of the D-Bus wire format to find a message's edges and read the few header fields
// that say what it is. Bodies are left alone here: the one message this package rewrites is
// decoded with a real marshaller (see rewrite.go), and everything else is forwarded as the bytes
// it arrived as.
//
// Forwarding untouched is not laziness, it is the safety property. A relay that re-encoded every
// message would have to be byte-perfect about every type on the bus - including the file
// descriptors a portal call carries - to avoid breaking traffic that has nothing to do with
// notifications.

const (
	fixedHeaderSize = 16

	msgTypeMethodCall byte = 1
)

// Header field codes, from the specification.
const (
	fieldPath        byte = 1
	fieldInterface   byte = 2
	fieldMember      byte = 3
	fieldDestination byte = 6
	fieldSignature   byte = 8
	fieldUnixFDs     byte = 9
)

var errShort = errors.New("dbus: message is shorter than its own header says")

// align rounds up to the next multiple of boundary, which is how every D-Bus type is placed.
func align(offset, boundary int) int {
	return (offset + boundary - 1) &^ (boundary - 1)
}

// header is what the relay needs to decide about a message without decoding its body.
type header struct {
	msgType byte
	// order is the byte order this message declared, which the sender chooses per message.
	order binary.ByteOrder
	// unixFDs is how many descriptors travel with it, so the relay hands on exactly that many.
	unixFDs uint32

	iface       string
	member      string
	destination string
	signature   string

	// size is the whole message, header and body, in bytes.
	size int
}

// parseHeader reads the fixed header and the header fields. It returns errShort when buf does
// not yet hold the whole message, which is the normal case on a stream.
func parseHeader(buf []byte) (header, error) {
	if len(buf) < fixedHeaderSize {
		return header{}, errShort
	}
	var hdr header
	switch buf[0] {
	case 'l':
		hdr.order = binary.LittleEndian
	case 'B':
		hdr.order = binary.BigEndian
	default:
		return header{}, fmt.Errorf("dbus: unknown byte order %q", buf[0])
	}
	hdr.msgType = buf[1]
	bodyLen := int(hdr.order.Uint32(buf[4:]))
	fieldsLen := int(hdr.order.Uint32(buf[12:]))

	// The fields array is followed by padding to 8, and the body starts there.
	fieldsEnd := fixedHeaderSize + fieldsLen
	bodyStart := align(fieldsEnd, 8)
	hdr.size = bodyStart + bodyLen
	if hdr.size < 0 || bodyLen < 0 || fieldsLen < 0 {
		return header{}, errors.New("dbus: message declares a negative length")
	}
	if len(buf) < hdr.size {
		return header{}, errShort
	}
	if err := hdr.readFields(buf[fixedHeaderSize:fieldsEnd]); err != nil {
		return header{}, err
	}
	return hdr, nil
}

// readFields walks the a(yv) header array. Only the codes this relay acts on are decoded; the
// rest are stepped over by their own type, so an unknown field cannot desynchronise the walk.
func (hdr *header) readFields(fields []byte) error {
	// Offsets inside the array are still measured from the start of the message, which is what
	// the +fixedHeaderSize below is for: alignment is absolute, never relative to the array.
	for off := 0; off < len(fields); {
		off = align(off+fixedHeaderSize, 8) - fixedHeaderSize
		if off+4 > len(fields) {
			return nil // trailing padding
		}
		code := fields[off]
		// The variant's signature: a length byte, that many bytes, and a NUL.
		sigLen := int(fields[off+1])
		if off+2+sigLen+1 > len(fields) {
			return errShort
		}
		sig := string(fields[off+2 : off+2+sigLen])
		off += 2 + sigLen + 1

		var err error
		switch sig {
		case "s", "o":
			var value string
			value, off, err = hdr.readString(fields, off)
			if err != nil {
				return err
			}
			switch code {
			case fieldInterface:
				hdr.iface = value
			case fieldMember:
				hdr.member = value
			case fieldDestination:
				hdr.destination = value
			case fieldPath:
				// read and discarded: the relay matches on interface and member
			}
		case "g":
			// A signature: one length byte, the bytes, a NUL.
			if off >= len(fields) {
				return errShort
			}
			length := int(fields[off])
			if off+1+length+1 > len(fields) {
				return errShort
			}
			if code == fieldSignature {
				hdr.signature = string(fields[off+1 : off+1+length])
			}
			off += 1 + length + 1
		case "u":
			off = align(off+fixedHeaderSize, 4) - fixedHeaderSize
			if off+4 > len(fields) {
				return errShort
			}
			value := hdr.order.Uint32(fields[off:])
			if code == fieldUnixFDs {
				hdr.unixFDs = value
			}
			off += 4
		default:
			// A field this relay does not know the type of cannot be stepped over safely, so
			// stop reading fields rather than guess. What was read already is enough to decide,
			// and the message is forwarded whole regardless.
			return nil
		}
	}
	return nil
}

// readString reads a length-prefixed, NUL-terminated string at off.
func (hdr *header) readString(fields []byte, off int) (string, int, error) {
	off = align(off+fixedHeaderSize, 4) - fixedHeaderSize
	if off+4 > len(fields) {
		return "", 0, errShort
	}
	length := int(hdr.order.Uint32(fields[off:]))
	off += 4
	if length < 0 || off+length+1 > len(fields) {
		return "", 0, errShort
	}
	return string(fields[off : off+length]), off + length + 1, nil
}

// isNotify reports whether this message is the one call the filter acts on.
func (hdr header) isNotify() bool {
	return hdr.msgType == msgTypeMethodCall &&
		hdr.member == "Notify" &&
		hdr.iface == notifyInterface
}
