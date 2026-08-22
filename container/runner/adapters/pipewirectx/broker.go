package pipewirectx

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Broker implements ports.AudioBroker.
type Broker struct{}

var _ ports.AudioBroker = Broker{}

// The holder's one status line. Same shape as the Wayland holder's, for the same reason: the
// launch has to learn both that the socket exists and whether a context was actually created,
// and a pipe answers both without a file to poll for.
const (
	statusOK          = "ok"
	statusUnsupported = "unsupported"
	statusError       = "error"
)

// MicrophoneFlag is how the holder is told the app may capture. The holder is a separate
// process, so the grant has to cross on the command line; only the capture half needs to,
// because it is the only one the permissions depend on.
const MicrophoneFlag = "--microphone"

// Establish spawns the holder and returns the socket the container should mount. An empty path
// means "mount the session's own socket": either the app asked for no session audio, or this
// daemon has no security context, which is announced rather than taken quietly.
func (Broker) Establish(addr paths.Address, cfg schema.AppConfig, opt options.HostOptions) (string, error) {
	if !Applies(cfg) || opt.RuntimeDir == "" {
		return "", nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%s: locate this binary to hold the audio context: %w", addr, err)
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("%s: readiness pipe for the audio context: %w", addr, err)
	}
	defer statusRead.Close()

	argv := []string{HoldCommand, addr.String()}
	if grantOf(cfg).capture {
		argv = append(argv, MicrophoneFlag)
	}
	proc := exec.Command(self, argv...)
	proc.ExtraFiles = []*os.File{statusWrite} // becomes statusFD in the holder
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.Stdout, proc.Stderr = nil, nil
	if err := proc.Start(); err != nil {
		statusWrite.Close()
		return "", fmt.Errorf("%s: start the audio context holder: %w", addr, err)
	}
	go proc.Wait()
	// The parent's copy must go, or the read below never sees EOF when the holder dies without
	// answering.
	statusWrite.Close()

	line, err := readStatus(statusRead)
	if err != nil {
		return "", fmt.Errorf("%s: waiting for the audio context: %w", addr, err)
	}
	socket, supported, err := parseStatus(line)
	if err != nil {
		return "", fmt.Errorf("%s: %w", addr, err)
	}
	if !supported {
		fmt.Fprintf(os.Stderr, "zcr: %s: this PipeWire daemon implements no security context, so the app is given the session socket directly and the directions its config names are not enforced\n", addr)
		return "", nil
	}
	return socket, nil
}

func readStatus(pipe *os.File) (string, error) {
	if err := pipe.SetReadDeadline(time.Now().Add(readyTimeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("the holder reported nothing: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func parseStatus(line string) (socket string, supported bool, err error) {
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case statusOK:
		if rest == "" {
			return "", false, errors.New("the holder reported a socket with no path")
		}
		return rest, true, nil
	case statusUnsupported:
		return "", false, nil
	case statusError:
		return "", false, errors.New(rest)
	}
	return "", false, fmt.Errorf("unreadable status from the audio holder: %q", line)
}

// Hold is the body of the hidden `zcr __pipewire` subcommand: the process that owns one app's
// audio context for as long as the app runs.
//
// It has to be a separate process for the reason the Wayland holder does - `zcr run` detaches
// and the context is revoked by closing a descriptor - and for one this package adds: the
// permissions have to be re-applied when the graph changes, so something must be watching.
func Hold(addr paths.Address, capture bool, opt options.HostOptions, wait func(name string) error) error {
	status := os.NewFile(statusFD, "status")
	defer func() {
		if status != nil {
			status.Close()
		}
	}()

	lis, err := create(addr, opt)
	switch {
	case errors.Is(err, ErrUnsupported):
		report(status, statusUnsupported)
		return nil
	case err != nil:
		report(status, statusError+" "+err.Error())
		return err
	}
	defer lis.close()
	report(status, statusOK+" "+lis.socketPath)
	status.Close()
	status = nil

	// The permission half runs until the app is gone. A failure here is not a reason to drop
	// the context: the socket is already the app's, and revoking it mid-run would take away
	// audio the config did grant. It is reported and the holder keeps holding.
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- enforce(addr, grant{capture: capture}, opt.RuntimeDir, stop) }()

	waitErr := wait(addr.Runtime())
	close(stop)
	if err := <-done; err != nil && !errors.Is(err, os.ErrClosed) {
		fmt.Fprintf(os.Stderr, "zcr: %s: audio permissions: %v\n", addr, err)
	}
	return waitErr
}

func report(status *os.File, line string) {
	if status == nil {
		return
	}
	fmt.Fprintln(status, line)
}

// listenUnix binds and listens on path, returning the descriptor to hand the daemon.
//
// Made with syscall rather than net.ListenUnix because what the security context needs is the
// listening descriptor itself: net's File() hands back a dup, leaving two descriptors for one
// socket and a listener this process would have to keep alive for no reason.
func listenUnix(path string) (*os.File, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create the app's audio socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", path, err)
	}
	if err := syscall.Listen(fd, 16); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
