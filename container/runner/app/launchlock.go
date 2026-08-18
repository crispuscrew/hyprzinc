package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// A launch is serialised per app, because the check that refuses a second launch of a running
// app cannot see the first one yet.
//
// The refusal reads what the runtime reports as running, and the app's container does not appear
// there until the end of a launch that creates a pod, loads an nft ruleset and waits for a D-Bus
// proxy to answer. A second launch a second later therefore passes the same check, prepares the
// same objects, fails on the first that already exists, and runs the fail-closed teardown - which
// removes the FIRST launch's pod, proxy and sockets. Two launches racing left nothing running at
// all; a double Enter in a launcher was enough.
//
// The lock is held for the whole launch, so the second one waits and then sees the first as
// running and refuses it properly. It is per app rather than global: two different apps starting
// together is ordinary, and only one app's objects collide with its own.
//
// flock rather than a pid file, for the reason the multiterminal waiter uses it: the kernel drops
// it when the holder dies, so a launch killed halfway cannot wedge every later one.

// launchLock is a held lock, released by close.
type launchLock struct{ file *os.File }

// lockLaunch blocks until this app's launch lock is free and takes it. A runtime directory that
// cannot be made is not a reason to refuse the launch: the lock closes a race, and failing the
// whole launch because a directory is missing would be a worse answer than the race.
func lockLaunch(app string) *launchLock {
	root, err := runRoot()
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil
	}
	file, err := lockFile(filepath.Join(root, "launch-"+app+".lock"), false)
	if err != nil {
		return nil
	}
	return &launchLock{file: file}
}

func (lock *launchLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	lock.file.Close() // releases the flock
}

// superviseCommand is the hidden subcommand that tears an app down after it exits on its own.
const superviseCommand = "__supervise"

// superviseAfter spawns the detached supervisor for this app.
//
// It is given the ADDRESS rather than the runtime name, because the runtime name of an instanced
// app ("notes.work") is not something the store can resolve - only "notes@work" is. A failure to
// spawn is reported and not fatal: the app is already running by this point, and refusing a
// launch that succeeded because its janitor did not start would be the worse answer.
func (svc Service) superviseAfter(cfg schema.AppConfig) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcr: %s: no supervisor, so nothing will tear this app down when it exits: %v\n", cfg.AppNameID, err)
		return
	}
	addr := svc.address(cfg.AppNameID)
	proc := exec.Command(exe, superviseCommand, addr.String())
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.Stdout, proc.Stderr = nil, nil
	if err := proc.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "zcr: %s: no supervisor, so nothing will tear this app down when it exits: %v\n", cfg.AppNameID, err)
		return
	}
	go proc.Wait() // reap if the caller (a long-lived TUI) outlives this
}

// Supervise waits until the app's container is gone and then tears down everything the launch
// created around it: the pod and its netns, the egress bridge, the D-Bus proxy and its socket
// directory, and any published host port.
//
// It exists because none of that happens today when an app exits on its own. The reaping
// goroutine that was meant to cover it cannot run in the shipped product - every front-end
// launches through a short-lived `zcr` that exits moments after forking the app - and a clean
// exit was never covered by it at all. The leak is not only untidy: `podman pod create` has no
// --replace, so the pod left behind makes the NEXT launch of a filtered app fail.
//
// Running the teardown twice is safe and expected: `zcr stop` may get there first, and every
// step is written to succeed on something already gone (`rm -f`, `--ignore`, a network remove
// that returns 0 for a network that is not there).
func (svc Service) Supervise(cfg schema.AppConfig, wait func(name string) error) error {
	if err := wait(cfg.AppNameID); err != nil {
		return fmt.Errorf("supervise %s: %w", cfg.AppNameID, err)
	}
	// Under the launch lock, and only if the app has not come back. Between the app going away
	// and this waking up, a person can start it again - and its objects have the same names, so
	// tearing down now would remove the new launch's pod and proxy. That is the very failure the
	// lock was added for, arriving from the other direction.
	lock := lockLaunch(cfg.AppNameID)
	defer lock.close()
	if svc.runtime.IsRunning(cfg.AppNameID) {
		return nil // it is running again, and that launch owns what is there now
	}
	if err := svc.Stop(cfg); err != nil {
		return fmt.Errorf("supervise %s: tear down after the app exited: %w", cfg.AppNameID, err)
	}
	return nil
}
