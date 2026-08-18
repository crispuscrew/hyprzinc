// Package pipewirectx is the audio adapter: it gives an app a PipeWire socket of its own,
// created under a PipeWire security context, and then holds that app to the directions its
// config actually granted (docs/architecture.md section 5.2, section 3 AudioMeta).
//
// The shape is waylandctx's, for the same reason: the privileged act happens outside the app
// and the app is handed only the result. Zinc binds a second socket, asks the daemon to accept
// connections on it under a security context, and the app mounts that socket instead of the
// session's own.
//
// The security context alone does NOT decide what the app may do, which is the difference from
// Wayland and the reason this package has a second half. A context sets identity
// (`pipewire.sec.app-id`) and marks the client restricted; the session manager then decides
// permissions, and wireplumber's shipped policy grants a restricted client `rx` on ANY object -
// enough to open a microphone. So Zinc also connects to the manager socket and sets the
// permissions itself: no permission at all on every capture node when the config granted no
// microphone. That is enforced by the daemon, which will not let a client see or link an object
// it holds no permission on.
package pipewirectx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// SandboxEngine is what Zinc calls itself to PipeWire, matching what it calls itself to the
// compositor so one identity answers for both.
const SandboxEngine = "com.github.crispuscrew.zinc"

// HoldCommand is the hidden subcommand that owns one app's audio context.
const HoldCommand = "__pipewire"

// statusFD is the descriptor the holder reports readiness on, as the Wayland holder does.
const statusFD = 3

// readyTimeout bounds the wait for the holder to report.
const readyTimeout = 10 * time.Second

// sessionSocket is the daemon socket applications use; managerSocket is the one that may set
// another client's permissions. module-access treats the second as unrestricted.
const (
	sessionSocket = "pipewire-0"
	managerSocket = "pipewire-0-manager"
)

// ErrUnsupported means the daemon advertises no security context interface, so a socket handed
// to the app could not be attributed or restricted.
var ErrUnsupported = errors.New("this PipeWire daemon does not implement a security context")

// Applies reports whether the app asked for session audio in any direction. A config that names
// device nodes instead needs no socket: those are passed with --device and the kernel enforces
// them, which is the stronger answer and does not involve this package at all.
func Applies(cfg schema.AppConfig) bool {
	audio := cfg.AudioMeta
	return audio.Playback.Default || audio.Microphone.Default || audio.Monitor.Default
}

// SocketDir is the per-instance directory holding this instance's own PipeWire socket, laid out
// like the Wayland and D-Bus ones. Per instance, so two instances of an app are two clients the
// daemon can tell apart.
func SocketDir(runtimeDir string, addr paths.Address) string {
	return filepath.Join(runtimeDir, "zinc", "pipewire", addr.Runtime())
}

// SocketPath is the socket inside that directory. The name is the one a PipeWire client looks
// for by default, so an app needs no variable set beyond its runtime directory.
func SocketPath(runtimeDir string, addr paths.Address) string {
	return filepath.Join(SocketDir(runtimeDir, addr), sessionSocket)
}

// grant is what one app is allowed, reduced to the two questions the daemon can answer.
type grant struct {
	// capture is whether any real microphone may be opened.
	capture bool
	// playback is whether the app may reach a sink at all.
	playback bool
}

func grantOf(cfg schema.AppConfig) grant {
	return grant{
		capture:  cfg.AudioMeta.Microphone.Default,
		playback: cfg.AudioMeta.Playback.Default || cfg.AudioMeta.Monitor.Default,
	}
}

// listener is a bound security context plus what has to outlive the exchange that created it.
type listener struct {
	socketPath string
	socketFile *os.File
	closeRead  *os.File
	closeWrite *os.File
	control    *conn
}

// create binds the app's socket and hands it to the daemon under a security context.
//
// listen_fd must already be listening when create is sent, so the socket is on disk before the
// container exists - which it has to be anyway, being a bind-mount source. close_fd is the
// revocation handle: when its write end closes, the daemon stops accepting on the socket.
func create(addr paths.Address, opt options.HostOptions) (*listener, error) {
	daemon, err := runtimeSocket(opt.RuntimeDir, sessionSocket)
	if err != nil {
		return nil, err
	}
	dir := SocketDir(opt.RuntimeDir, addr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	path := SocketPath(opt.RuntimeDir, addr)
	// A socket left by a previous run would make Listen fail on a path nothing is serving.
	_ = os.Remove(path)

	sock, err := listenUnix(path)
	if err != nil {
		return nil, err
	}
	lis := &listener{socketPath: path, socketFile: sock}

	lis.closeRead, lis.closeWrite, err = os.Pipe()
	if err != nil {
		lis.close()
		return nil, fmt.Errorf("create the revocation pipe: %w", err)
	}

	cnn, err := dial(daemon)
	if err != nil {
		lis.close()
		return nil, err
	}
	lis.control = cnn

	if err := cnn.hello(
		"application.name", "zinc",
		"application.process.binary", "zcr",
	); err != nil {
		lis.close()
		return nil, err
	}
	contextID, err := findSecurityContext(cnn)
	if err != nil {
		lis.close()
		return nil, err
	}
	payload := structPod(
		fdPod(0),
		fdPod(1),
		dictPod(
			"pipewire.sec.engine", SandboxEngine,
			"pipewire.sec.app-id", addr.App,
			"pipewire.sec.instance-id", addr.Runtime(),
		),
	)
	fds := []int{int(sock.Fd()), int(lis.closeRead.Fd())}
	if err := cnn.send(contextID, securityContextMethodCreate, payload, fds...); err != nil {
		lis.close()
		return nil, fmt.Errorf("create the security context: %w", err)
	}
	// Sync, so a refusal arrives as an error here rather than as an app that cannot connect.
	if err := cnn.sync(); err != nil {
		lis.close()
		return nil, err
	}
	err = cnn.pump(time.Now().Add(replyTimeout), func(msg message) (bool, error) {
		return msg.id == coreID && msg.opcode == coreEventDone, nil
	})
	if err != nil {
		lis.close()
		return nil, fmt.Errorf("the daemon refused the security context: %w", err)
	}
	return lis, nil
}

// findSecurityContext walks the registry for the security context global and binds it.
func findSecurityContext(cnn *conn) (uint32, error) {
	registry, err := cnn.getRegistry()
	if err != nil {
		return 0, err
	}
	if err := cnn.sync(); err != nil {
		return 0, err
	}
	// Read until the security context turns up, NOT until the sync completes. The globals are
	// not a reply to anything: the daemon emits them once the session manager has granted this
	// client permission to see them, which happens well after Done for the sync arrives. Taking
	// Done as the end of the listing reports every daemon as having no security context.
	var found uint32
	var seen bool
	err = cnn.pump(time.Now().Add(replyTimeout), func(msg message) (bool, error) {
		if msg.id != registry || msg.opcode != registryEventGlobal {
			return false, nil
		}
		item, err := decodeGlobal(msg.body)
		if err != nil {
			return false, nil // a global Zinc cannot read is a global it does not want
		}
		if item.iface == interfaceSecurityContext {
			found, seen = item.id, true
			return true, nil
		}
		return false, nil
	})
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, err
	}
	if !seen {
		return 0, ErrUnsupported
	}
	return cnn.bind(registry, found, interfaceSecurityContext, 3)
}

func (lis *listener) close() {
	// The write end first: that is what revokes the context.
	if lis.closeWrite != nil {
		_ = lis.closeWrite.Close()
	}
	if lis.closeRead != nil {
		_ = lis.closeRead.Close()
	}
	if lis.control != nil {
		lis.control.close()
	}
	if lis.socketFile != nil {
		_ = lis.socketFile.Close()
	}
	if lis.socketPath != "" {
		_ = os.Remove(lis.socketPath)
		_ = os.Remove(filepath.Dir(lis.socketPath))
	}
}
