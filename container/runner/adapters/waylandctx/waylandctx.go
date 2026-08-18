// Package waylandctx is the display adapter: it implements ports.DisplayBroker by giving an app a
// Wayland socket of its own that the compositor has attached a wp_security_context_v1 to, so the
// app's identity is fixed before it exists and it has no request that can change it (section 5.2).
//
// Two protocol facts shape it: listen_fd must be bound and listening before create_listener, so the
// socket is on disk before the container (it is a bind-mount source); and the compositor keeps
// accepting after the creating client disconnects, so only close_fd's write end must outlive the
// one-shot connection. The manager global is hidden from clients that already have a context, so
// an app could not do this for itself even if it wanted to.
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

// SandboxEngine is what Zinc calls itself to the compositor. The repository's own domain rather
// than a generic "org.zinc": app_id is only unique per engine, so the name has to be one this
// project holds.
const SandboxEngine = "com.github.crispuscrew.zinc"

// HoldCommand is the hidden subcommand that holds a context open. It is here rather than in
// main so the spawner and the dispatcher cannot disagree about the name.
const HoldCommand = "__wayland"

// statusFD is the descriptor the holder reports readiness on: the launch has to know the socket
// exists and whether a context was created. The holder's stdio is deliberately not the caller's -
// it is detached and outlives it, and inheriting a launcher's pipe would block it in Wait.
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

// SocketDir is this instance's own Wayland socket directory, mirroring dbusproxy.HostSocketDir. Per
// instance, because the point is that the compositor can tell two instances apart.
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

// Establish spawns the holder and waits until it has a socket. The returned path is what the
// container mounts; empty means "mount the compositor's own socket", either because the app opted
// out or because the compositor lacks the protocol - announced on stderr, since it is a real
// reduction in what the compositor knows. Everything else fails the launch.
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

// Hold is the body of the hidden `zcr __wayland` subcommand: the process owning one app's security
// context for as long as the app runs, because `zcr run` detaches and the context is revoked by
// closing a descriptor. wait is supplied by the caller, so this package need not know what a
// container is.
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

// create binds the app's socket and hands it to the compositor under a security context. The
// identities are chosen so a desktop can cross-check them: app_id is the app name (stable across
// instances, as the protocol requires), instance_id is the runtime name that also names the podman
// container, and sandbox_engine is what makes the other two unambiguous.
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

// close revokes the context and removes the socket, but only if it is still the one this holder
// created: an app relaunched immediately gets a new holder on the same path.
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
