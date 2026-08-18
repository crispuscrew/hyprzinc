package notifyfilter

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
)

// relay carries one app's bus connection through to the real one, rewriting only the
// notification calls the config asked to be rewritten.
//
// Descriptors are carried across too. That is not a refinement: the same connection carries an
// app's portal traffic, which passes file descriptors, so a relay that dropped them would break
// file dialogs and screen sharing for every app that also happens to filter its notifications.
type relay struct {
	pol policy
	// nextID answers a silenced app with a plausible notification id. Real servers hand out
	// small increasing numbers, so an app that tracks them sees nothing unusual.
	nextID atomic.Uint32
}

// maxFDs is how many descriptors one message may carry. The specification's own limit is 16;
// this is the buffer the relay reserves for the control message.
const maxFDs = 64

// serve accepts connections until the listener closes, relaying each to upstream.
func (rly *relay) serve(listener *net.UnixListener, upstream string) error {
	for {
		app, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			if err := rly.session(app, upstream); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "zcr: notification filter: %v\n", err)
			}
		}()
	}
}

// session wires one app connection to its own upstream connection.
func (rly *relay) session(app *net.UnixConn, upstream string) error {
	defer app.Close()
	addr, err := net.ResolveUnixAddr("unix", upstream)
	if err != nil {
		return err
	}
	up, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return fmt.Errorf("connect to the bus at %s: %w", upstream, err)
	}
	defer up.Close()

	done := make(chan error, 2)
	// Server to app is never rewritten: nothing the bus says back is this filter's business.
	go func() { done <- pipe(up, app) }()
	go func() { done <- rly.filter(app, up) }()

	err = <-done
	// Closing both ends unblocks the other direction, which is what makes this return.
	app.Close()
	up.Close()
	<-done
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// filter carries the app's side, applying the policy to the one call it acts on.
//
// The authentication handshake is relayed verbatim first. It is line-based text and ends when
// the app sends BEGIN; the app and the real bus negotiate their own descriptor passing through
// it, which is why the relay does not need to take part.
func (rly *relay) filter(app, up *net.UnixConn) error {
	var buf []byte
	var fds []int
	authenticated := false

	for {
		if !authenticated {
			line, rest, found := bytes.Cut(buf, []byte("\r\n"))
			if found {
				out := append(append([]byte{}, line...), '\r', '\n')
				if err := writeMsg(up, out, nil); err != nil {
					return err
				}
				buf = rest
				if bytes.EqualFold(bytes.TrimSpace(line), []byte("BEGIN")) {
					authenticated = true
				}
				continue
			}
		} else if hdr, err := parseHeader(buf); err == nil {
			raw := buf[:hdr.size]
			carried := take(&fds, int(hdr.unixFDs))
			if err := rly.forward(app, up, hdr, raw, carried); err != nil {
				return err
			}
			buf = buf[hdr.size:]
			continue
		} else if !errors.Is(err, errShort) {
			return err
		}

		if err := readMore(app, &buf, &fds); err != nil {
			return err
		}
	}
}

// forward sends one message on, or answers it here when the policy says the app may not send it.
func (rly *relay) forward(app, up *net.UnixConn, hdr header, raw []byte, fds []int) error {
	if !hdr.isNotify() || !rly.pol.applies() {
		return writeMsg(up, raw, fds)
	}
	// A notification carrying descriptors is forwarded untouched: re-encoding would renumber
	// the indices its body uses to refer to them.
	if len(fds) > 0 {
		return writeMsg(up, raw, fds)
	}
	call, out, err := rly.pol.decide(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcr: notification filter: %v\n", err)
	}
	switch call {
	case verdictSilence:
		reply, err := silenceReply(hdr, raw, rly.nextID.Add(1))
		if err != nil {
			return err
		}
		return writeMsg(app, reply, nil)
	case verdictRefuse:
		reply, err := refuseReply(hdr, raw)
		if err != nil {
			return err
		}
		return writeMsg(app, reply, nil)
	}
	return writeMsg(up, out, nil)
}

// pipe copies one direction verbatim, descriptors included.
func pipe(from, to *net.UnixConn) error {
	var buf []byte
	var fds []int
	for {
		if err := readMore(from, &buf, &fds); err != nil {
			return err
		}
		if len(buf) == 0 && len(fds) == 0 {
			continue
		}
		// Everything read so far goes on together: this direction is not parsed, so there is
		// no message boundary to line the descriptors up with beyond the one the kernel kept.
		if err := writeMsg(to, buf, fds); err != nil {
			return err
		}
		buf, fds = buf[:0], nil
	}
}

// readMore reads bytes and any descriptors that came with them.
func readMore(from *net.UnixConn, buf *[]byte, fds *[]int) error {
	data := make([]byte, 64<<10)
	oob := make([]byte, syscall.CmsgSpace(maxFDs*4))
	read, oobRead, _, _, err := from.ReadMsgUnix(data, oob)
	if read > 0 {
		*buf = append(*buf, data[:read]...)
	}
	if oobRead > 0 {
		scms, scmErr := syscall.ParseSocketControlMessage(oob[:oobRead])
		if scmErr == nil {
			for _, scm := range scms {
				if got, rightsErr := syscall.ParseUnixRights(&scm); rightsErr == nil {
					*fds = append(*fds, got...)
				}
			}
		}
	}
	if err != nil {
		return err
	}
	if read == 0 && oobRead == 0 {
		return io.EOF
	}
	return nil
}

// writeMsg sends bytes and hands on any descriptors, closing this process's copies once the
// kernel has taken them. Leaving them open would leak a descriptor per portal call.
func writeMsg(to *net.UnixConn, data []byte, fds []int) error {
	if len(fds) == 0 {
		if len(data) == 0 {
			return nil
		}
		_, err := to.Write(data)
		return err
	}
	_, _, err := to.WriteMsgUnix(data, syscall.UnixRights(fds...), nil)
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
	return err
}

// take removes the first count descriptors, which are the ones belonging to the message about
// to be forwarded.
func take(fds *[]int, count int) []int {
	if count <= 0 || len(*fds) == 0 {
		return nil
	}
	if count > len(*fds) {
		count = len(*fds)
	}
	out := (*fds)[:count]
	*fds = (*fds)[count:]
	return out
}
