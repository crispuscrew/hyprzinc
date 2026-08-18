package app

// Multiterminal apps (docs section 9.1). A multiterminal app runs as a detached "holder" container
// so it outlives any single terminal; each terminal is a `podman exec -it` into it. The app lives
// until the LAST terminal closes, unless StopConditions.Background keeps the holder running.
//
// Coordination is by filesystem flock, with no daemon: each terminal is its own detached waiter
// holding an flock on its marker file, auto-released on death so a killed terminal cannot wedge the
// count. The last waiter out stops the container. The waiter's three actions are injected, so the
// ref-count logic is testable without podman or a TTY.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// defaultShell is what a "shell" terminal runs when it isn't re-running the app's own
// command. /bin/sh is present in any real terminal app image (section 9.1 honesty note).
const defaultShell = "/bin/sh"

// OpenTerminal spawns one more terminal: it builds the derived image if needed, launches a detached
// waiter in its own session, and returns. The first terminal also starts the holder. It validates up
// front so the UI reports common errors synchronously rather than in a silent detached process.
func (svc Service) OpenTerminal(cfg schema.AppConfig, opt options.HostOptions, shell bool) error {
	if err := validate.Validate(cfg); err != nil {
		return fmt.Errorf("%s: %w", cfg.AppNameID, err)
	}
	if err := checkNetwork(cfg); err != nil { // fail closed on not-yet-supported network shapes (as launch does)
		return err
	}
	if !cfg.StartConditions.Multiterminal {
		return fmt.Errorf("%s: not a multiterminal app", cfg.AppNameID)
	}
	if len(opt.Terminal) == 0 {
		return fmt.Errorf("%s: terminal app but no terminal emulator configured (set ZINC_TERMINAL)", cfg.AppNameID)
	}
	// Build the derived image (if ImageMeta.Install is set) here, in the foreground, so
	// a build failure is reported to the caller - not lost in the detached waiter.
	if err := svc.ensureImage(cfg); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: locate self: %w", cfg.AppNameID, err)
	}
	argv := []string{"__term", cfg.AppNameID}
	if shell {
		argv = append(argv, "--shell")
	}
	proc := exec.Command(exe, argv...)
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.Stdout, proc.Stderr = nil, nil // detached: don't corrupt the parent's TUI
	if err := proc.Start(); err != nil {
		return fmt.Errorf("%s: open terminal: %w", cfg.AppNameID, err)
	}
	go proc.Wait() // reap if the caller (long-lived TUI) outlives the waiter
	return nil
}

// Term is the blocking waiter that runs inside a `__term` process: it ensures the
// holder is up, opens one terminal, and on close stops the container if it was the
// last one out.
func (svc Service) Term(cfg schema.AppConfig, opt options.HostOptions, shell bool) error {
	if err := validate.Validate(cfg); err != nil {
		return fmt.Errorf("%s: %w", cfg.AppNameID, err)
	}
	if err := checkNetwork(cfg); err != nil { // fail closed on not-yet-supported network shapes (as launch does)
		return err
	}
	if !cfg.StartConditions.Multiterminal {
		return fmt.Errorf("%s: not a multiterminal app", cfg.AppNameID)
	}
	if len(opt.Terminal) == 0 {
		return fmt.Errorf("%s: terminal app but no terminal emulator configured (set ZINC_TERMINAL)", cfg.AppNameID)
	}
	root, err := runRoot()
	if err != nil {
		return err
	}
	wtr := &waiter{
		runRoot:     root,
		background:  cfg.StopConditions.Background,
		ensureUp:    func() error { return svc.ensureHolder(cfg, opt) },
		runTerminal: func() error { return svc.runTerminalSession(cfg, opt, shell) },
		stop:        func() error { return svc.Stop(cfg) },
	}
	return wtr.run(cfg.AppNameID)
}

// ensureHolder starts the holder container if it isn't already running. For a filtered
// app this runs the enforcer's pre-steps (pod create → nft) so there is no
// unfiltered-egress window; the holder's `podman run -d` returns at once.
func (svc Service) ensureHolder(cfg schema.AppConfig, opt options.HostOptions) error {
	// Running, not merely present. A holder runs without --rm whenever the app also sets
	// KeepAlive or Autorestart, so one that died outside the last-one-out path leaves an Exited
	// container behind: Exists stays true, this returns as though the holder were up, and the
	// terminal execs into a corpse. The app is then wedged until someone runs podman by hand.
	if svc.runtime.IsRunning(cfg.AppNameID) {
		return nil
	}
	steps, err := svc.prepareSteps(cfg, opt)
	if err != nil {
		return err // nothing created yet, so there is nothing to tear down
	}
	for _, cmd := range steps {
		if err := svc.runtime.Exec(cmd); err != nil {
			return errors.Join(fmt.Errorf("start %s (%s): %w", cfg.AppNameID, cmd.Desc, err), svc.teardown(cfg, len(steps) > 0))
		}
	}
	// The holder is the app's container, so it needs the same security context an ordinary
	// app gets - the terminals that exec into it inherit whatever socket it was given.
	opt, err = svc.withDisplay(cfg, opt)
	if err != nil {
		return errors.Join(fmt.Errorf("start %s: %w", cfg.AppNameID, err), svc.teardown(cfg, len(steps) > 0))
	}
	opt, err = svc.withAudio(cfg, opt)
	if err != nil {
		return errors.Join(fmt.Errorf("start %s: %w", cfg.AppNameID, err), svc.teardown(cfg, len(steps) > 0))
	}
	opt, err = svc.withNotify(cfg, opt)
	if err != nil {
		return errors.Join(fmt.Errorf("start %s: %w", cfg.AppNameID, err), svc.teardown(cfg, len(steps) > 0))
	}
	appArgs, aerr := svc.runtime.AppRunArgs(cfg, opt, svc.attachFlags(cfg, opt))
	if aerr != nil {
		return errors.Join(aerr, svc.teardown(cfg, len(steps) > 0))
	}
	if err := svc.runtime.Exec(ports.Command{Args: appArgs, Desc: "start holder"}); err != nil {
		return errors.Join(fmt.Errorf("start %s holder: %w", cfg.AppNameID, err), svc.teardown(cfg, len(steps) > 0))
	}
	return nil
}

// runTerminalSession opens one terminal: the configured emulator wrapping a `podman
// exec -it` into the holder, running the app's own command (or a shell). It blocks
// until the terminal window closes.
func (svc Service) runTerminalSession(cfg schema.AppConfig, opt options.HostOptions, shell bool) error {
	cmd := multitermCmd(cfg)
	if shell || len(cmd) == 0 {
		cmd = []string{defaultShell}
	}
	return svc.runtime.OpenSession(cfg.AppNameID, cmd, opt, false)
}

// multitermCmd is the argv each terminal runs: MultiterminalEntrypoint (else the app
// Entrypoint), split on whitespace since `podman exec` takes an argv (unlike the
// single-container run path, which uses --entrypoint). Empty → the caller falls back
// to a shell.
func multitermCmd(cfg schema.AppConfig) []string {
	spec := strings.TrimSpace(cfg.StartConditions.MultiterminalEntrypoint)
	if spec == "" {
		spec = strings.TrimSpace(cfg.StartConditions.Entrypoint)
	}
	return strings.Fields(spec)
}

// waiter owns one terminal's lifecycle. Its three actions are injected so the
// flock/ref-count logic is testable without podman, an emulator, or a real TTY.
type waiter struct {
	runRoot     string
	background  bool
	ensureUp    func() error // start the holder if it isn't running (idempotent)
	runTerminal func() error // open the terminal; block until it closes
	stop        func() error // tear the container down (last one out)
}

// run executes the lifecycle: register under the coordination lock (starting the
// holder if needed), run the terminal, then deregister and - if no other terminal is
// still live and this isn't a background app - stop the container.
func (wtr *waiter) run(app string) error {
	appDir := filepath.Join(wtr.runRoot, app)
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return fmt.Errorf("multiterm: create %s: %w", appDir, err)
	}

	// Phase 1 - under the coordination lock: ensure the holder is up (exactly once
	// across racing waiters) and claim a liveness marker before releasing.
	coord, err := lockFile(filepath.Join(appDir, "lock"), false)
	if err != nil {
		return err
	}
	if err := wtr.ensureUp(); err != nil {
		coord.Close()
		return err
	}
	mark, err := claimMarker(appDir)
	if err != nil {
		coord.Close()
		return err
	}
	coord.Close()

	// Phase 2 - run the terminal; this blocks until the window/session closes.
	runErr := wtr.runTerminal()

	// Phase 3 - under the coordination lock: drop our marker, then stop the container
	// if we were the last live terminal (background opts out). Holding the lock makes
	// the "last one out" decision atomic against a terminal opening at the same moment.
	coord, err = lockFile(filepath.Join(appDir, "lock"), false)
	if err != nil {
		mark.release()
		return err
	}
	defer coord.Close()
	mark.release()
	if !wtr.background && !anyLive(appDir) {
		if stopErr := wtr.stop(); stopErr != nil && runErr == nil {
			return stopErr
		}
	}
	return runErr
}

// lockFile opens path (creating it) and takes an exclusive flock - blocking by
// default, non-blocking when nonblock is set. The returned file holds the lock until
// Close. The fd is O_CLOEXEC (Go default), so a child exec'd while it is held does not
// inherit the lock.
func lockFile(path string, nonblock bool) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("multiterm: open %s: %w", path, err)
	}
	how := syscall.LOCK_EX
	if nonblock {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// marker is a waiter's liveness token: a uniquely named file under the app dir whose
// flock the waiter holds for its whole life. release drops the lock and the file. Call
// it only while holding the coordination lock.
type marker struct {
	file *os.File
	path string
}

func (mrk *marker) release() {
	mrk.file.Close() // releases the flock
	os.Remove(mrk.path)
}

// claimMarker creates a uniquely named "term.*" file (unique even within one process,
// so the in-process test can run many waiters) and flock-holds it.
func claimMarker(appDir string) (*marker, error) {
	file, err := os.CreateTemp(appDir, "term.*")
	if err != nil {
		return nil, fmt.Errorf("multiterm: create marker: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, fmt.Errorf("multiterm: lock marker: %w", err)
	}
	return &marker{file: file, path: file.Name()}, nil
}

// anyLive reports whether any terminal other than the caller is still alive, and reaps
// stale markers as a side effect. A marker whose flock can be taken non-blocking is
// held by nobody (its waiter died) - stale, so it is removed. One that can't be taken
// is held by a live waiter. Call it only while holding the coordination lock.
func anyLive(appDir string) bool {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return false
	}
	live := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "term.") {
			continue
		}
		path := filepath.Join(appDir, entry.Name())
		file, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			file.Close() // releases the lock we just took; the holder was gone
			os.Remove(path)
			continue
		}
		file.Close()
		live = true
	}
	return live
}

// runRoot is the per-app coordination directory root: $XDG_RUNTIME_DIR/zinc/run, else
// the user cache dir. Never a predictable /tmp path (multi-user collisions).
func runRoot() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "zinc", "run"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("multiterm: locate runtime dir: %w", err)
	}
	return filepath.Join(cache, "zinc", "run"), nil
}
