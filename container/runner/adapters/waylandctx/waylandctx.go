// Package waylandctx is the display adapter: it implements ports.DisplayBroker by giving an
// app a Wayland socket of its own that the compositor has attached a wp_security_context_v1
// to, so what the compositor sees is a named, per-instance identity the app cannot forge
// (docs/architecture.md section 5.2).
//
// The shape is the one section 5.3 and section 5.7 use: the privileged act happens outside
// the app and the app is handed only the result. Zinc connects to the REAL compositor socket,
// binds a second socket of its own, and asks the compositor to accept connections on that
// second socket under a security context. The app mounts the second socket and never the
// compositor's, so its identity is fixed before it exists and there is no request it can send
// to change it. That is the difference from a label: a label describes a container, this
// decides what the display server believes about every connection the container makes.
//
// Two facts in the protocol are what make the design possible:
//
//   - listen_fd must already be bound and listening when create_listener is sent. So the
//     socket is on disk before the app container is created, which it has to be - it is a
//     bind-mount source, and podman cannot mount a path that is not there.
//   - the compositor must keep accepting on listen_fd after the client that created the
//     context disconnects. So the Wayland connection is a one-shot: opened, used, closed. The
//     only thing that must outlive it is the write end of close_fd, and holding a pipe for the
//     app's lifetime is far cheaper than holding a compositor connection.
//
// The manager global is deliberately hidden from clients that already have a security context
// (nesting is a privilege-escalation hole), which is exactly why this has to run outside the
// sandbox: an app could not do it for itself even if it wanted to.
package waylandctx

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// SandboxEngine is what Zinc calls itself to the compositor. The protocol asks for a
// reverse-DNS name and keeps a list of well-known engines upstream, which Zinc is not on.
//
// It is the repository's own domain rather than a short generic name like "org.zinc" or
// "zinc" on purpose: the engine name is half of the identity - app_id is only required to be
// unique per engine - so it has to be a name this project actually holds, and taking a tidy
// generic one would squat an identifier another project could legitimately register. A name
// derived from where the code lives cannot collide with anyone.
const SandboxEngine = "com.github.crispuscrew.zinc"

// HoldCommand is the hidden subcommand that holds a context open. It is here rather than in
// main so the spawner and the dispatcher cannot disagree about the name.
const HoldCommand = "__wayland"

// statusFD is the descriptor the holder reports readiness on. The launch has to wait for the
// socket to exist (it is about to be bind-mounted) and has to learn whether a context was
// actually created, and a pipe answers both without a file to poll for, a directory to clean
// up, or a timeout that guesses.
//
// The holder's stdio is deliberately NOT wired to the caller's: it is detached and outlives
// it, so inheriting the caller's stderr would keep a launcher's captured pipe open for the
// whole life of the app, and the launcher would block in Wait until the app exited. Anything
// the holder needs to say, it says on this pipe and the caller prints.
const statusFD = 3

// readyTimeout bounds the wait for that first line. The holder binds a socket and does two
// Wayland roundtrips, so this is long only by the standards of what it covers; it exists so a
// compositor that never answers fails the launch with a message instead of hanging it.
const readyTimeout = 10 * time.Second

// Broker implements ports.DisplayBroker. It is stateless: everything it needs comes from the
// address and the host options, and the state that must live for the app's lifetime lives in
// the holder process, not here.
type Broker struct{}

var _ ports.DisplayBroker = Broker{}

// Applies reports whether this app on this host gets a security context at all. Exported
// because the dry run needs the same answer to explain itself, and two copies of the rule
// would eventually disagree about which apps are covered.
func Applies(cfg schema.AppConfig, opt options.HostOptions) bool {
	return !cfg.DisplayMeta.DisableSecurityContext &&
		strings.TrimSpace(opt.RuntimeDir) != "" &&
		strings.TrimSpace(opt.WaylandDisplay) != ""
}

// SocketDir is the per-instance directory holding this instance's own Wayland socket. It
// mirrors the D-Bus layout (dbusproxy.HostSocketDir) one level over: same root, same shape,
// a different middle segment. Per instance rather than per app, because the whole point is
// that the compositor can tell two instances apart - one shared socket would give them one
// identity and make instance_id a lie.
func SocketDir(runtimeDir string, addr paths.Address) string {
	if strings.TrimSpace(runtimeDir) == "" {
		return ""
	}
	return filepath.Join(runtimeDir, "zinc", "wayland", addr.Runtime())
}

// SocketPath is the socket itself. It keeps the compositor socket's basename so that what is
// in the directory reads like what the app connects to; the container-side name comes from
// WAYLAND_DISPLAY either way, so this is for whoever is looking at the directory.
func SocketPath(runtimeDir, display string, addr paths.Address) string {
	dir := SocketDir(runtimeDir, addr)
	if dir == "" || strings.TrimSpace(display) == "" {
		return ""
	}
	return filepath.Join(dir, filepath.Base(display))
}

// Establish spawns the holder for this app and waits until it has a socket. The returned path
// is what the app container mounts; an empty path means "mount the compositor's own socket".
//
// Empty is returned for two very different reasons and neither is an error. The app may have
// opted out (DisplayMeta.DisableSecurityContext). Or the compositor may not implement the
// protocol - GNOME did not for a long time - and refusing to launch there would make Zinc
// unusable on a mainstream desktop in exchange for nothing, since the app would have got the
// raw socket before this existed anyway. That fallback is announced on stderr rather than
// taken quietly, because it is a real reduction in what the compositor knows and the user is
// the only one who can decide it is acceptable.
//
// Everything else does fail the launch: a socket that cannot be bound, a compositor that
// cannot be reached, a rejected request. Those are broken environments or bugs, and silently
// degrading on them would mean the security context is only ever best-effort with no way to
// tell whether it happened.
func (Broker) Establish(addr paths.Address, cfg schema.AppConfig, opt options.HostOptions) (string, error) {
	if !Applies(cfg, opt) {
		return "", nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%s: locate this binary to hold the Wayland security context: %w", addr, err)
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("%s: readiness pipe for the Wayland security context: %w", addr, err)
	}
	defer statusRead.Close()

	proc := exec.Command(self, HoldCommand, addr.String())
	proc.ExtraFiles = []*os.File{statusWrite} // becomes statusFD in the holder
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.Stdout, proc.Stderr = nil, nil // detached: see statusFD
	if err := proc.Start(); err != nil {
		statusWrite.Close()
		return "", fmt.Errorf("%s: start the Wayland security context holder: %w", addr, err)
	}
	go proc.Wait() // reap if the caller (a long-lived TUI) outlives the holder
	// The parent's copy must go, or the read below never sees EOF when the holder dies
	// without answering - which is precisely the case the timeout should not have to cover.
	statusWrite.Close()

	line, err := readStatus(statusRead)
	if err != nil {
		return "", fmt.Errorf("%s: waiting for the Wayland security context: %w", addr, err)
	}
	socket, supported, err := parseStatus(line)
	if err != nil {
		return "", fmt.Errorf("%s: %w", addr, err)
	}
	if !supported {
		// The strict answer is decided here rather than in the holder: the holder knows the
		// compositor and this side knows the config, and the status line already carries the
		// one fact that has to cross between them.
		if cfg.DisplayMeta.RequireSecurityContext {
			return "", fmt.Errorf("%s: RequireSecurityContext is set and this compositor does not implement wp_security_context_v1, so the only way to start this app would be to hand it the compositor socket directly, which is what that setting refuses; clear it to accept the passthrough, or run the app under a compositor that implements the protocol", addr)
		}
		fmt.Fprintf(os.Stderr, "zcr: %s: this compositor does not implement wp_security_context_v1, so the app is given the compositor socket directly and the compositor cannot tell it apart from an unsandboxed client\n", addr)
		return "", nil
	}
	return socket, nil
}

// readStatus reads the holder's one line, under a deadline. os.Pipe's ends are pollable, so
// the deadline is real rather than a goroutine racing a timer.
func readStatus(pipe *os.File) (string, error) {
	if err := pipe.SetReadDeadline(time.Now().Add(readyTimeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		if line == "" {
			return "", fmt.Errorf("the holder exited without reporting: %w", err)
		}
		return "", fmt.Errorf("incomplete report %q: %w", line, err)
	}
	return line, nil
}

// Status verbs. One line, one word, then the detail - small enough that both sides fit on a
// screen and there is no encoding to get wrong between two copies of the same binary.
const (
	statusOK          = "ok"          // followed by the socket path
	statusUnsupported = "unsupported" // followed by why; the caller falls back
	statusFailed      = "error"       // followed by why; the caller fails the launch
)

// parseStatus reads the holder's line. supported is false only for the deliberate fallback;
// an unreadable line is an error, because the alternative is treating a garbled report as
// "no security context" and quietly launching unprotected.
func parseStatus(line string) (socket string, supported bool, err error) {
	verb, detail, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch verb {
	case statusOK:
		if strings.TrimSpace(detail) == "" {
			return "", false, errors.New("the Wayland security context holder reported success without a socket path")
		}
		return detail, true, nil
	case statusUnsupported:
		return "", false, nil
	case statusFailed:
		return "", false, fmt.Errorf("the Wayland security context could not be created: %s", detail)
	default:
		return "", false, fmt.Errorf("unreadable report from the Wayland security context holder: %q", line)
	}
}

// Hold is the body of the hidden `zcr __wayland` subcommand: the process that owns one app's
// security context for as long as the app runs.
//
// It has to be a separate process because `zcr run` detaches and exits, and the context is
// revoked by closing a descriptor - so something has to still be there holding it. This is the
// same answer the multiterminal path reached for the same reason (app/multiterm.go): a hidden
// subcommand re-execing this binary, rather than a daemon.
//
// wait is how the holder learns the app is gone; the caller supplies it so this package does
// not have to know what a container is.
func Hold(addr paths.Address, opt options.HostOptions, wait func(name string) error) error {
	status := os.NewFile(statusFD, "zinc-wayland-status")
	lis, err := create(addr, opt)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			report(status, statusUnsupported+" "+err.Error())
			return nil // the launch continues on the raw socket; this is not a failure
		}
		report(status, statusFailed+" "+err.Error())
		return err
	}
	report(status, statusOK+" "+lis.socket)

	// From here the process exists only to keep close_fd's write end open. Closing it is
	// what tells the compositor to stop accepting, so it must not happen while the app is
	// alive - not on an error path, not on a signal we choose to handle, not early.
	waitErr := wait(addr.Runtime())
	lis.close()
	return waitErr
}

// report writes the holder's one line and closes the pipe, so the caller is released the
// moment there is an answer rather than when this process eventually exits. Write failures
// are ignored: the pipe is absent when a person runs the subcommand by hand, and a holder
// that works is worth more than one that refuses because nobody was listening.
func report(status *os.File, line string) {
	if status == nil {
		return
	}
	fmt.Fprintln(status, line)
	status.Close()
}

// listener is a created-and-registered security context: the socket the app will mount, and
// the one descriptor whose closing revokes it.
type listener struct {
	dir        string
	socket     string
	created    os.FileInfo // the socket as this holder made it - see close
	closeWrite *os.File
}

// create binds the app's socket, hands it to the compositor under a security context, and
// keeps only what has to outlive the exchange.
//
// The identities are chosen so a desktop can cross-check them rather than take them on faith:
//
//   - app_id is the app half of the address. The protocol requires app_id to be the same
//     string across runs and across instances of one application, which is exactly what an
//     app name is and exactly what a per-run identifier is not.
//   - instance_id is the runtime name - the SAME string that names the podman container and
//     that `zcr where` reports. A random uuid would satisfy the protocol and be useless: the
//     point for the consumer is that the identity is discoverable, so a compositor holding an
//     instance_id can find the container, and a person can check that the window claiming to
//     be an app really belongs to the container Zinc started.
//   - sandbox_engine is Zinc's own reverse-DNS name (SandboxEngine), which is what makes the
//     other two unambiguous: the protocol only requires app_id to be unique per engine.
func create(addr paths.Address, opt options.HostOptions) (*listener, error) {
	compositor, err := compositorSocket(opt)
	if err != nil {
		return nil, err
	}
	dir := SocketDir(opt.RuntimeDir, addr)
	socket := SocketPath(opt.RuntimeDir, opt.WaylandDisplay, addr)
	if dir == "" || socket == "" {
		return nil, fmt.Errorf("%s: a Wayland security context needs XDG_RUNTIME_DIR and WAYLAND_DISPLAY set, to place the app's own socket", addr)
	}
	// 0700 rather than the 0755 a plain mkdir gives: XDG_RUNTIME_DIR is already 0700 so this
	// changes nothing today, and it is what keeps the socket unreachable by another user if
	// this ever lands somewhere less strict.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// A socket left behind by a holder that was killed makes bind fail with "address already
	// in use", which would wedge the app permanently for a reason no message would explain.
	// Nothing is listening on it - the holder is the only thing that ever does - so removing
	// it cannot disconnect anyone.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove the stale socket %s: %w", socket, err)
	}
	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	// Go unlinks a unix listener's path when the listener is closed, and this one is closed
	// as soon as the compositor has its own copy of the descriptor - which would delete the
	// very socket the app is about to bind-mount, seconds before it is mounted.
	unixListener.SetUnlinkOnClose(false)

	// Anything that fails from here leaves nothing behind, directory included: the launch is
	// about to fall back to the compositor's own socket, and a leftover of ours would sit in
	// the runtime dir for the rest of the session looking like a live context.
	done := false
	defer func() {
		if !done {
			unixListener.Close()
			os.Remove(socket)
			os.Remove(dir)
		}
	}()

	// A dup of the listening fd, in blocking mode, because it is about to be handed to
	// another process: the compositor gets a descriptor, not Go's runtime poller state.
	listenFile, err := unixListener.File()
	if err != nil {
		return nil, fmt.Errorf("take the listening descriptor for %s: %w", socket, err)
	}
	defer listenFile.Close()

	closeRead, closeWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("revocation pipe for %s: %w", addr, err)
	}
	defer closeRead.Close()

	if err := register(compositor, int(listenFile.Fd()), int(closeRead.Fd()), SandboxEngine, addr.App, addr.Runtime()); err != nil {
		closeWrite.Close()
		return nil, err
	}
	created, err := os.Stat(socket)
	if err != nil {
		closeWrite.Close()
		return nil, fmt.Errorf("stat the socket just created at %s: %w", socket, err)
	}
	done = true
	// The listening fd and the pipe's read end are the compositor's now; the protocol says
	// closing them is the only operation left to us, so that is what happens - here, rather
	// than being carried around as descriptors nothing may use.
	unixListener.Close()
	return &listener{dir: dir, socket: socket, created: created, closeWrite: closeWrite}, nil
}

// close revokes the context and takes the socket away.
//
// The socket is removed only if it is still the one this holder created. An app stopped and
// immediately relaunched gets a new holder that binds the same path, and this one is waking up
// at that exact moment - without the identity check it would delete the new holder's socket
// and leave an app pointing at nothing. The directory removal is left to fail when a new
// socket is in it, which is the same guard by another means.
func (lis *listener) close() {
	lis.closeWrite.Close() // hangup on close_fd: the compositor stops accepting new connections
	if now, err := os.Stat(lis.socket); err == nil && os.SameFile(now, lis.created) {
		os.Remove(lis.socket)
	}
	os.Remove(lis.dir)
}

// compositorSocket resolves the REAL compositor socket - the one Zinc connects to and the app
// never sees. WAYLAND_DISPLAY is allowed to be an absolute path by the spec, and a compositor
// started outside the session's runtime dir does exactly that, so it is honoured rather than
// joined onto XDG_RUNTIME_DIR and turned into a path that exists nowhere.
func compositorSocket(opt options.HostOptions) (string, error) {
	display := strings.TrimSpace(opt.WaylandDisplay)
	if display == "" {
		return "", errors.New("WAYLAND_DISPLAY is not set, so there is no compositor to ask for a security context")
	}
	if filepath.IsAbs(display) {
		return display, nil
	}
	if strings.TrimSpace(opt.RuntimeDir) == "" {
		return "", fmt.Errorf("WAYLAND_DISPLAY is %q but XDG_RUNTIME_DIR is not set, so the compositor socket cannot be located", display)
	}
	return filepath.Join(opt.RuntimeDir, display), nil
}
