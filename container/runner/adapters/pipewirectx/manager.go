package pipewirectx

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// captureClasses are the media classes of a node an app can record a real input from. A sink's
// monitor is deliberately absent: it is not a node of its own but a set of ports on the sink,
// so it cannot be denied separately from the sink an app needs in order to play at all. See
// enforcer.apply.
var captureClasses = map[string]bool{
	"Audio/Source":         true,
	"Audio/Source/Virtual": true,
}

// pollInterval paces the re-apply below. It is also how long the holder takes to notice the
// connection has gone.
const pollInterval = time.Second

// reapplyCount is how many times a permission set is re-sent after something changed.
//
// The session manager grants a restricted client `rx` on any object from its own handler, and
// that handler and this one both fire on the client appearing. Whichever lands second wins, so
// Zinc sends its narrower set again over the next few seconds rather than assuming it won the
// race. After that nothing re-grants unless the client's properties change.
const reapplyCount = 3

// enforcer holds one app to its audio grant by setting the permissions of its client on the
// daemon, over the manager socket.
type enforcer struct {
	addr paths.Address
	want grant

	cnn      *conn
	registry uint32

	// sources are the global ids of every capture node currently in the graph.
	sources map[uint32]bool
	// candidates are client proxies bound but not yet identified. Every client has to be bound
	// to be identified at all: the security context's identity is in the client's info event
	// and not in its registry global, so there is nothing to match on before binding.
	candidates map[uint32]bool
	// client is the app's own client object, once it has connected.
	client     uint32
	haveClient bool
	// pending counts the re-applies still owed.
	pending int
}

// enforce connects to the manager socket and holds the app to its grant until stop closes or
// the daemon goes away.
//
// The manager socket rather than the session one: setting another client's permissions is a
// privileged act, and module-access treats `pipewire-0-manager` as unrestricted. The app never
// sees this connection; it exists in the holder, which the app cannot reach.
func enforce(addr paths.Address, want grant, runtimeDir string, stop <-chan struct{}) error {
	path, err := runtimeSocket(runtimeDir, managerSocket)
	if err != nil {
		return err
	}
	cnn, err := dial(path)
	if err != nil {
		return err
	}
	defer cnn.close()

	if err := cnn.hello(
		"application.name", "zinc-audio",
		"application.process.binary", "zcr",
		// Names this connection in `pw-dump` as what it is, so a person looking at the graph
		// can see which app's permissions it is holding.
		"zinc.app", addr.String(),
	); err != nil {
		return err
	}
	enf := &enforcer{
		addr:       addr,
		want:       want,
		cnn:        cnn,
		sources:    map[uint32]bool{},
		candidates: map[uint32]bool{},
	}
	if enf.registry, err = cnn.getRegistry(); err != nil {
		return err
	}
	return enf.run(stop)
}

func (enf *enforcer) run(stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		msg, err := enf.cnn.recv(time.Now().Add(pollInterval))
		switch {
		case err == nil:
			if err := enf.handle(msg); err != nil {
				return err
			}
		case errors.Is(err, os.ErrDeadlineExceeded):
			// The tick is what re-applies against the session manager's own grant.
			if enf.pending > 0 {
				enf.pending--
				if err := enf.apply(); err != nil {
					return err
				}
			}
		default:
			return err
		}
	}
}

func (enf *enforcer) handle(msg message) error {
	if msg.id == coreID && msg.opcode == coreEventError {
		return decodeError(msg.body)
	}
	if enf.candidates[msg.id] && msg.opcode == clientEventInfo {
		return enf.identify(msg)
	}
	if msg.id != enf.registry {
		return nil
	}
	switch msg.opcode {
	case registryEventGlobal:
		item, err := decodeGlobal(msg.body)
		if err != nil {
			return nil // a global that cannot be read is one Zinc has no opinion about
		}
		return enf.added(item)
	case registryEventGlobalRemove:
		id, err := decodeGlobalRemove(msg.body)
		if err != nil {
			return nil
		}
		if enf.sources[id] {
			delete(enf.sources, id)
		}
	}
	return nil
}

func (enf *enforcer) added(item global) error {
	switch {
	case item.iface == interfaceNode && captureClasses[item.props["media.class"]]:
		// A capture node appearing after the app started is the case a one-shot grant would
		// miss: plug in a USB microphone an hour in and it is a new global the app has the
		// default permission on.
		enf.sources[item.id] = true
		enf.pending = reapplyCount
		return enf.apply()
	case item.iface == interfaceClient && !enf.haveClient:
		// Bound rather than matched, because the identity is not in this event. Binding is
		// cheap and read-only; the proxy for a client that turns out to be somebody else is
		// simply never used again.
		proxy, err := enf.cnn.bind(enf.registry, item.id, interfaceClient, 3)
		if err != nil {
			return err
		}
		enf.candidates[proxy] = true
	}
	return nil
}

// identify reads a bound client's info and keeps it if it is the app this holder is for.
func (enf *enforcer) identify(msg message) error {
	props, err := decodeClientInfo(msg.body)
	if err != nil {
		return nil
	}
	if !enf.isOurs(props) {
		delete(enf.candidates, msg.id)
		return nil
	}
	enf.client, enf.haveClient = msg.id, true
	enf.pending = reapplyCount
	return enf.apply()
}

// isOurs reports whether a client is the app this holder is for, by the identity the security
// context stamped on it. Both halves are checked: app-id alone would match a second instance of
// the same app, whose grant is its own.
func (enf *enforcer) isOurs(props map[string]string) bool {
	return props["pipewire.sec.engine"] == SandboxEngine &&
		props["pipewire.sec.app-id"] == enf.addr.App &&
		props["pipewire.sec.instance-id"] == enf.addr.Runtime()
}

// apply sends the app's whole permission set. It is the complete set every time rather than a
// difference, because that is what the method means: the daemon replaces what it holds.
//
// The default is `rx` - the same the session manager grants a restricted client - and then every
// capture node is taken to nothing when the config granted no microphone. Nothing is denied when
// it did: the point is to hold the app to what it asked for, not to second-guess it.
func (enf *enforcer) apply() error {
	if !enf.haveClient || enf.want.capture {
		return nil
	}
	denied := make([]uint32, 0, len(enf.sources))
	for id := range enf.sources {
		denied = append(denied, id)
	}
	// Sorted, so two runs of the same graph send the same bytes and a log can be compared.
	sort.Slice(denied, func(one, two int) bool { return denied[one] < denied[two] })

	fields := [][]byte{intPod(int32(len(denied) + 1)), intPod(permIDAny), intPod(permRX)}
	for _, id := range denied {
		fields = append(fields, intPod(int32(id)), intPod(permNone))
	}
	if err := enf.cnn.send(enf.client, clientMethodUpdatePermissions, structPod(fields...)); err != nil {
		return fmt.Errorf("%s: set audio permissions: %w", enf.addr, err)
	}
	return nil
}
