// Package machine supervises guest processes: starting a guest detached from the launching shell,
// finding it again, and stopping it the way its own OS expects. Choosing qemu directly over libvirt
// makes that ours to own, and buys the thing the design is for - qemu runs inside the user's session,
// so it can open an accelerated window on their compositor.
package machine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crispuscrew/zinc/virtualization/runner/adapters/qmp"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/paths"
)

const (
	// startGrace is how long to watch a freshly started guest before declaring it up. A
	// bad command line kills qemu in milliseconds, so this is long enough to catch that
	// without making a good launch feel slow.
	startGrace   = 2 * time.Second
	pollInterval = 50 * time.Millisecond
	// termGrace is how long a guest gets after SIGTERM before SIGKILL. qemu closes its
	// disks on SIGTERM, so this is about letting it finish that, not about the guest.
	termGrace = 5 * time.Second
)

// Runtime starts, inspects and stops guests.
type Runtime struct {
	Paths paths.Paths
}

// State is what zvr knows about one app's guest.
type State struct {
	Name   string
	PID    int
	Alive  bool
	Guest  string // what the guest reports over QMP: running, paused, ...; empty if unreachable
	Detail string // why the guest state is unknown, when it is
}

// Start launches a guest detached from the calling shell and confirms it survived. The
// process is put in its own session so it outlives zvr - a launcher fires and forgets,
// and the guest must not die with the hotkey that started it.
func (runtime Runtime) Start(app string, args []string, extraEnv []string, stdin string) error {
	if state, _ := runtime.State(app); state.Alive {
		return fmt.Errorf("%s is already running (pid %d)", app, state.PID)
	}
	// A previous guest that died without cleaning up leaves a pidfile and sockets behind;
	// qemu refuses to bind a socket that already exists, so clear them first.
	runtime.clean(app)

	logFile, err := os.OpenFile(runtime.Paths.Log(app), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the guest log: %w", err)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== %s starting ===\n", app)

	command := exec.Command(args[0], args[1:]...)
	if len(extraEnv) > 0 {
		// Appended to the inherited environment rather than replacing it: qemu still needs
		// the session's WAYLAND_DISPLAY and XDG_RUNTIME_DIR to open its window at all.
		command.Env = append(os.Environ(), extraEnv...)
	}
	command.Stdout = logFile
	command.Stderr = logFile
	if stdin != "" {
		// The nftables ruleset, when the guest runs inside a filtered namespace. On stdin
		// rather than in a file: it is generated per launch, and a file would be one more
		// thing that could change between being written and being read.
		command.Stdin = strings.NewReader(stdin)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", args[0], err)
	}
	// Reap it here rather than leaving a zombie: zvr exits straight after, but a caller
	// that keeps running (a launcher popping several apps) would otherwise accumulate one
	// per launch.
	go func() { _ = command.Wait() }()

	if err := runtime.confirmStarted(app, command.Process.Pid); err != nil {
		runtime.clean(app)
		return err
	}
	return nil
}

// confirmStarted watches a new guest long enough to tell a successful boot from a command
// line qemu rejected. Without this a bad config would look like a successful launch and
// fail silently in a log nobody reads.
func (runtime Runtime) confirmStarted(app string, pid int) error {
	deadline := time.Now().Add(startGrace)
	sawPIDFile := false
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return fmt.Errorf("the guest exited immediately:\n%s", runtime.logTail(app))
		}
		if _, err := os.Stat(runtime.Paths.PIDFile(app)); err == nil {
			sawPIDFile = true
			break
		}
		time.Sleep(pollInterval)
	}
	if !sawPIDFile {
		if !alive(pid) {
			return fmt.Errorf("the guest exited immediately:\n%s", runtime.logTail(app))
		}
		return fmt.Errorf("the guest did not write its pidfile within %s; see %s", startGrace, runtime.Paths.Log(app))
	}
	return nil
}

// State reports what is known about an app's guest: whether its process is alive, and
// what the guest inside says about itself. A live pid alone does not mean a working
// guest, which is why the QMP answer is reported separately rather than folded in.
func (runtime Runtime) State(app string) (State, error) {
	state := State{Name: app}
	pid, err := runtime.readPID(app)
	if err != nil {
		return state, nil // no pidfile: simply not running
	}
	state.PID = pid
	state.Alive = alive(pid) && isGuestProcess(pid, app)
	if !state.Alive {
		return state, nil
	}

	session, err := qmp.Dial(runtime.Paths.QMP(app))
	if err != nil {
		state.Detail = "control socket unreachable: " + err.Error()
		return state, nil
	}
	defer session.Close()
	status, err := session.QueryStatus()
	if err != nil {
		state.Detail = err.Error()
		return state, nil
	}
	state.Guest = status.Status
	return state, nil
}

// Stop shuts a guest down. By default it presses the ACPI power button and waits, so the
// guest's own OS flushes and unmounts; force skips straight to signalling the process,
// which is what to reach for when a guest has stopped responding.
func (runtime Runtime) Stop(app string, force bool, timeout time.Duration) error {
	pid, err := runtime.readPID(app)
	if err != nil {
		return fmt.Errorf("%s is not running", app)
	}
	// A live pid is not enough: it must still be THIS app's guest. qemu can die without clearing its
	// pidfile, and the kernel eventually reissues the number - at which point signalling on the pidfile
	// alone is "terminate an arbitrary process of this user".
	if !alive(pid) || !isGuestProcess(pid, app) {
		runtime.clean(app)
		return fmt.Errorf("%s is not running (cleaned up a stale pidfile)", app)
	}

	if !force {
		session, err := qmp.Dial(runtime.Paths.QMP(app))
		if err == nil {
			pressErr := session.Powerdown()
			session.Close()
			if pressErr == nil && waitGone(pid, timeout) {
				runtime.clean(app)
				return nil
			}
		}
		// Either the control socket was unreachable or the guest ignored the button. Say
		// so, because the difference matters: the guest may not have flushed its disks.
		fmt.Fprintf(os.Stderr, "zvr: %s did not shut down within %s, terminating the guest process\n", app, timeout)
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !isGone(err) {
		return fmt.Errorf("terminate %s: %w", app, err)
	}
	if !waitGone(pid, termGrace) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isGone(err) {
			return fmt.Errorf("kill %s: %w", app, err)
		}
		waitGone(pid, termGrace)
	}
	runtime.clean(app)
	return nil
}

// Running lists the apps with a live guest, for the ps view. It reads the runtime
// directory rather than a registry, so it reports what is actually running even if it was
// started by a different zvr invocation.
func (runtime Runtime) Running() ([]State, error) {
	entries, err := os.ReadDir(runtime.Paths.RunDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has ever run
		}
		return nil, err
	}
	var states []State
	for _, entry := range entries {
		name, found := strings.CutSuffix(entry.Name(), ".pid")
		if !found {
			continue
		}
		state, err := runtime.State(name)
		if err != nil || !state.Alive {
			continue
		}
		states = append(states, state)
	}
	return states, nil
}

// Console connects the caller's terminal to the guest's serial console.
func (runtime Runtime) ConsolePath(app string) string { return runtime.Paths.Serial(app) }

func (runtime Runtime) readPID(app string) (int, error) {
	data, err := os.ReadFile(runtime.Paths.PIDFile(app))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("unreadable pidfile for %s", app)
	}
	return pid, nil
}

// clean removes the files a dead guest leaves behind. qemu will not bind a socket path
// that already exists, so a stale one blocks the next start.
func (runtime Runtime) clean(app string) {
	for _, path := range []string{
		runtime.Paths.PIDFile(app), runtime.Paths.QMP(app), runtime.Paths.Serial(app),
	} {
		_ = os.Remove(path)
	}
}

// logTail returns the end of a guest's log, for reporting a failed start.
func (runtime Runtime) logTail(app string) string {
	data, err := os.ReadFile(runtime.Paths.Log(app))
	if err != nil {
		return "  (no log at " + runtime.Paths.Log(app) + ")"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

// alive reports whether a process exists. Signal 0 performs the permission and existence
// checks without delivering anything.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// isGuestProcess checks that a pid really is this app's guest and not an unrelated
// process that inherited the number. Pids are recycled, and a supervisor that trusts a
// stale pidfile will eventually signal something it has no business touching.
func isGuestProcess(pid int, app string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc cmdline is NUL-separated, so compare argv ELEMENTS. A substring check matches any command line
	// merely mentioning the name, and every guest's cmdline contains its overlay path, so one app's pid
	// could be read as another's. `-name <app>` is what the launcher writes.
	argv := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	named := false
	for index, arg := range argv {
		if arg == "-name" && index+1 < len(argv) && argv[index+1] == app {
			named = true
			break
		}
	}
	return named && strings.Contains(argv[0], "qemu-system")
}

func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return !alive(pid)
}

func isGone(err error) bool { return err == syscall.ESRCH }
