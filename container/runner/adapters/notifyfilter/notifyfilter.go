// Package notifyfilter is the notification adapter: it holds an app to the NotificationMeta its
// config states, by standing between the app and the session bus and rewriting the one call that
// carries a notification (docs/architecture.md section 3 NotificationMeta).
//
// It exists because the D-Bus proxy filters by NAME and nothing else. Whether an app may reach
// org.freedesktop.Notifications at all is a question xdg-dbus-proxy can answer; what an app is
// allowed to PUT in a notification is a question about a message body, and answering it needs
// something that reads one.
//
// The shape is the same as every other privileged step: the filter runs outside the app, the app
// is handed only the filtered socket, and the real bus socket stays where the app cannot reach
// it. The chain becomes app -> filter -> xdg-dbus-proxy -> session bus, so the name filtering
// still happens and this adds only the body rewriting on top.
//
// Everything that is not a notification is forwarded as the bytes it arrived as, descriptors and
// all. An app's bus carries its portal traffic too, and a relay that re-encoded or dropped that
// would break file dialogs and screen sharing for every app that filters notifications.
package notifyfilter

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

// HoldCommand is the hidden subcommand that runs one app's filter.
const HoldCommand = "__notify"

// statusFD is the descriptor the filter reports readiness on, as the other holders do.
const statusFD = 3

const readyTimeout = 10 * time.Second

// Broker implements ports.NotifyBroker.
type Broker struct{}

var _ ports.NotifyBroker = Broker{}

// Applies reports whether this app's config asks for anything the filter has to do.
//
// A zero NotificationMeta is the default and means "whatever the desktop does", so the filter
// stays out of the launch entirely: no extra process, no extra hop, and an app that never
// mentioned notifications keeps the exact bus connection it had before.
//
// It also needs a bus at all. Notifications travel over the session bus, so an app with no
// DBusMeta has no way to send one and nothing to filter.
func Applies(cfg schema.AppConfig) bool {
	return !cfg.DBusMeta.IsZero() && policyOf(cfg.NotificationMeta).applies()
}

// SocketDir is the per-app directory holding the filtered socket.
func SocketDir(runtimeDir, app string) string {
	if runtimeDir == "" || app == "" || strings.ContainsAny(app, "/") || strings.HasPrefix(app, ".") {
		return ""
	}
	return filepath.Join(runtimeDir, "zinc", "notify", app)
}

// SocketPath is the socket the app connects to instead of the proxy's own.
func SocketPath(runtimeDir, app string) string {
	dir := SocketDir(runtimeDir, app)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "bus")
}

// Establish spawns the filter and returns the socket the app should be given. An empty path
// means the app keeps the proxy's socket, which is the answer whenever no policy applies.
func (Broker) Establish(addr paths.Address, cfg schema.AppConfig, opt options.HostOptions) (string, error) {
	if !Applies(cfg) || opt.RuntimeDir == "" {
		return "", nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%s: locate this binary to filter notifications: %w", addr, err)
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("%s: readiness pipe for the notification filter: %w", addr, err)
	}
	defer statusRead.Close()

	// The address, not the runtime name: "notes.work" is not a name the store can resolve, so
	// an instanced app's filter could never load its own config.
	proc := exec.Command(self, HoldCommand, addr.String())
	proc.ExtraFiles = []*os.File{statusWrite}
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.Stdout, proc.Stderr = nil, nil
	if err := proc.Start(); err != nil {
		statusWrite.Close()
		return "", fmt.Errorf("%s: start the notification filter: %w", addr, err)
	}
	go proc.Wait()
	statusWrite.Close()

	line, err := readStatus(statusRead)
	if err != nil {
		return "", fmt.Errorf("%s: waiting for the notification filter: %w", addr, err)
	}
	socket, err := parseStatus(line)
	if err != nil {
		return "", fmt.Errorf("%s: %w", addr, err)
	}
	return socket, nil
}

func readStatus(pipe *os.File) (string, error) {
	if err := pipe.SetReadDeadline(time.Now().Add(readyTimeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("the filter reported nothing: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func parseStatus(line string) (string, error) {
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case "ok":
		if rest == "" {
			return "", errors.New("the filter reported a socket with no path")
		}
		return rest, nil
	case "error":
		return "", errors.New(rest)
	}
	return "", fmt.Errorf("unreadable status from the notification filter: %q", line)
}

// Hold is the body of the hidden `zcr __notify` subcommand: it serves the app's filtered socket
// until the app is gone.
//
// upstream is the proxy's socket, which this process connects to on the app's behalf. The app
// never learns that path: it is handed only the socket this filter listens on.
func Hold(cfg schema.AppConfig, upstream string, opt options.HostOptions, wait func(name string) error) error {
	status := os.NewFile(statusFD, "status")
	defer func() {
		if status != nil {
			status.Close()
		}
	}()
	fail := func(err error) error {
		if status != nil {
			fmt.Fprintln(status, "error "+err.Error())
		}
		return err
	}

	dir := SocketDir(opt.RuntimeDir, cfg.AppNameID)
	if dir == "" {
		return fail(errors.New("no runtime directory for the notification filter"))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(fmt.Errorf("create %s: %w", dir, err))
	}
	path := SocketPath(opt.RuntimeDir, cfg.AppNameID)
	// A socket left by a previous run would make Listen fail on a path nothing serves.
	_ = os.Remove(path)

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %w", path, err))
	}
	defer func() {
		listener.Close()
		_ = os.Remove(path)
		_ = os.Remove(dir)
	}()

	rly := &relay{pol: policyOf(cfg.NotificationMeta)}
	go func() {
		if err := rly.serve(listener, upstream); err != nil {
			fmt.Fprintf(os.Stderr, "zcr: notification filter: %v\n", err)
		}
	}()

	fmt.Fprintln(status, "ok "+path)
	status.Close()
	status = nil

	return wait(cfg.AppNameID)
}
