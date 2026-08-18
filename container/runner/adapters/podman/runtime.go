// Package podman is the container-runtime adapter: it implements the Runtime, ImageBuilder and
// ImageResolver ports against the podman CLI, and is the only place that knows podman's argument
// syntax. The *Args builders are pure so launch plans can be dry-run. It does NOT decide the
// network - AppRunArgs splices in netFlags from a NetEnforcer (docs section 5.3, section 13).
package podman

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/derived"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Container-side fixed paths (refined alongside the real launch path in later
// milestones: theme env wiring, agent sockets).
const (
	ctrXDGRuntime = "/run/zinc"
	ctrThemeDir   = "/etc/zinc/theme"
)

// Runtime implements ports.Runtime against podman. It is stateless.
type Runtime struct{}

// Compile-time checks that this adapter satisfies the ports it claims.
var (
	_ ports.Runtime       = Runtime{}
	_ ports.ImageBuilder  = Builder{}
	_ ports.ImageResolver = Resolver{}
)

// TerminalLaunch wraps a podman argv in the configured terminal emulator. term is the emulator
// argv (e.g. ["foot"]). When hold is set the invocation goes through the host shell so the window
// pauses after it exits - emulator-agnostic, since the emulator is user-configured (section 9.1).
func TerminalLaunch(term, runArgs []string, hold bool) []string {
	out := append([]string{}, term...)
	if !hold {
		out = append(out, "podman")
		return append(out, runArgs...)
	}
	// Each arg is single-quoted so a command argv can never break out of the script
	// (the install/validate layers already reject the worst metacharacters, but the
	// shell wrapper must be safe on its own). printf's \n are interpreted by printf.
	script := "podman " + shellJoin(runArgs) +
		`; status=$?; printf '\n[zinc] exited (status %s) - press Enter to close\n' "$status"; read _`
	return append(out, "sh", "-c", script)
}

// shellQuote wraps str in single quotes for safe interpolation into an `sh -c`
// script, escaping any embedded single quote as the standard '\” sequence. Used only
// by TerminalLaunch's hold wrapper.
func shellQuote(str string) string {
	return "'" + strings.ReplaceAll(str, "'", `'\''`) + "'"
}

// shellJoin single-quotes every arg and joins them with spaces.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// HolderCmd is the main process of a multiterminal app's shared container: a no-op that blocks so
// the container outlives any single terminal. Runs under `--init`, because a bare `sleep` as PID 1
// would ignore `podman stop` until the SIGKILL timeout. Needs `sleep` in the image.
func HolderCmd() []string { return []string{"sleep", "infinity"} }

// ExecArgs builds `podman exec -it <app> <cmd...>` - one interactive session into a
// running container. Each terminal of a multiterminal app is one of these (its cmd is
// the app's own command, or a shell), wrapped by TerminalLaunch.
func ExecArgs(app string, cmd []string) []string {
	out := []string{"exec", "-it", app}
	return append(out, cmd...)
}

// runMode selects the lifecycle flags and trailing command of a `podman run`.
type runMode int

const (
	modeForeground runMode = iota // plain `run --rm`
	modeBackground                // `run -d`
	modeTerminal                  // `run --rm -it` (single interactive terminal)
	modeHolder                    // `run -d --rm --init` + HolderCmd (multiterminal keep-alive)
)

// modeFor derives the run mode from a validated config. Multiterminal takes
// precedence: such an app's container is the holder, and its real command runs in each
// terminal via ExecArgs, not as the container's PID 1.
func modeFor(cfg schema.AppConfig) runMode {
	switch {
	case cfg.StartConditions.Multiterminal:
		return modeHolder
	case cfg.StartConditions.Terminal:
		return modeTerminal
	case cfg.StopConditions.Background:
		return modeBackground
	default:
		return modeForeground
	}
}

// AppRunArgs builds the app's `podman run` argv. netFlags comes from the NetEnforcer and is spliced
// in after the least-privilege baseline. Pure: no I/O.
func (Runtime) AppRunArgs(cfg schema.AppConfig, opt options.HostOptions, netFlags []string) ([]string, error) {
	home := opt.HomeDir
	if home == "" {
		home = "/root"
	}
	// Keys go in the home of whoever runs the app: a non-root app cannot read /root, so a key mounted
	// there would look like a broken key rather than a wrong path.
	if user := cfg.InternalUserMeta; user.UseNonRootUser && user.NonRootUserName != "" {
		home = "/home/" + user.NonRootUserName
	}

	args := []string{"run"}
	mode := modeFor(cfg)
	// KeepAlive keeps the container after its entrypoint exits, so --rm is dropped. Autorestart drops
	// it too, and must: podman refuses the pair at the CLI layer ("the --rm option conflicts with
	// --restart"), and StartApp is detached with nil stdio, so the launch would report success while
	// leaking the pod, ruleset, proxy and holder. `restart: always` in a compose import sets this.
	keepAlive := cfg.StopConditions.KeepAlive || cfg.StartConditions.Autorestart
	switch mode {
	case modeTerminal:
		// CLI/TUI app: needs an interactive TTY and runs in a spawned terminal window
		// (the shell wraps this argv with the emulator; see TerminalLaunch).
		if keepAlive {
			args = append(args, "-it")
		} else {
			args = append(args, "--rm", "-it")
		}
	case modeBackground:
		args = append(args, "-d")
	case modeHolder:
		// Multiterminal keep-alive: detached, no TTY, removed on stop (--rm), with
		// --init so `podman stop` is prompt (see HolderCmd). Its terminals attach via
		// ExecArgs.
		if keepAlive {
			args = append(args, "-d", "--init")
		} else {
			args = append(args, "-d", "--rm", "--init")
		}
	default: // modeForeground
		if !keepAlive {
			args = append(args, "--rm")
		}
	}
	if cfg.StartConditions.Autorestart {
		// Restart only on failure - a clean exit (or a manual stop) is intentional (section 9.1).
		args = append(args, "--restart", "on-failure")
	}
	// Launch is hermetic: never fetch the image at run time (section 5.5). The image must
	// already be in local storage (a derived build, or resolved/pulled at save time); a
	// missing image fails fast instead of a surprise registry pull.
	args = append(args, "--pull", "never")
	args = append(args, "--name", cfg.AppNameID)

	// Least-privilege baseline (section 1, section 5.1): drop every capability and forbid privilege
	// escalation. Anything the app genuinely needs is re-added below from Capabilities.
	args = append(args, "--security-opt", "no-new-privileges", "--cap-drop", "all")

	// The rest of the containment baseline: who the app runs as, and how much of the machine it takes.
	// keep-id is absent here on purpose - a pod owns the user namespace of everything joining it, and
	// podman refuses `--userns` on a container joining one ("cannot set user namespace mode when
	// joining pod"), so a filtered app's keep-id goes on the pod, by the enforcer.
	args = append(args, userArgs(cfg.InternalUserMeta, slices.Contains(netFlags, "--pod"))...)
	args = append(args, resourceArgs(cfg.ResourcesMeta)...)
	args = append(args, healthArgs(cfg.StartConditions)...)

	// Network attachment is the enforcer's decision (section 5.3) - we only splice it in.
	//
	// The app's own environment goes above everything the runner exports, because podman lets a later
	// -e win and the runner's variables describe what it actually built. Sorted, because a Go map has
	// no order and this argv is what --dry-run prints.
	for _, name := range slices.Sorted(maps.Keys(cfg.Env)) {
		args = append(args, "-e", name+"="+cfg.Env[name])
	}

	args = append(args, netFlags...)

	// XDG_RUNTIME_DIR is exported once, and only when we actually mount a socket under
	// it (Wayland and/or Pipewire below). Exporting it unconditionally would point apps
	// at /run/zinc even when it's empty/absent in the container.
	runtimeDirExported := false
	exportRuntimeDir := func() {
		if !runtimeDirExported {
			args = append(args, "-e", "XDG_RUNTIME_DIR="+ctrXDGRuntime)
			runtimeDirExported = true
		}
	}

	// Display / Wayland (section 5.2). The socket is the app's OWN when a security context was
	// established, the compositor's otherwise; the container-side path and WAYLAND_DISPLAY are
	// identical either way.
	if opt.RuntimeDir != "" && opt.WaylandDisplay != "" {
		socket := filepath.Join(opt.RuntimeDir, opt.WaylandDisplay)
		mode := "passthrough"
		if !cfg.DisplayMeta.DisableSecurityContext && opt.WaylandSocket != "" {
			socket, mode = opt.WaylandSocket, "security-context"
		}
		args = append(args,
			"-v", socket+":"+filepath.Join(ctrXDGRuntime, opt.WaylandDisplay)+":ro",
			"-e", "WAYLAND_DISPLAY="+opt.WaylandDisplay,
		)
		exportRuntimeDir()
		// The label records which of the two actually happened. A label claiming a context that does not
		// exist is worse than none: it is what a desktop reads to decide how much to trust the client.
		args = append(args, "--label", "zinc.wayland="+mode)
	}
	if !cfg.DisplayMeta.DisableGpuAccess {
		args = append(args, "--device", "/dev/dri")
	}

	// Theme bundle - one curated read-only directory (section 5.6).
	if cfg.HostTheme && opt.ThemeBundleDir != "" {
		args = append(args, "-v", opt.ThemeBundleDir+":"+ctrThemeDir+":ro")
	}

	// A read-only root filesystem. Podman keeps a writable tmpfs on /dev, /dev/shm, /run,
	// /tmp and /var/tmp (--read-only-tmpfs defaults true), so an app that only needs scratch
	// space still runs; what stops is writing into the image itself.
	if cfg.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}

	// Audio (section 3 AudioMeta): the config states a direction and a strength, this picks the
	// transport. `default` means a PipeWire socket, mounted once however many directions asked for
	// it. The socket is the app's OWN when a security context was established for it
	// (opt.PipeWireSocket), and the session's own otherwise - the container-side path is the same
	// either way, so an app needs no per-mode configuration.
	if audioUsesSession(cfg.AudioMeta) && opt.RuntimeDir != "" {
		pipewireSock := opt.PipeWireSocket
		if pipewireSock == "" {
			pipewireSock = filepath.Join(opt.RuntimeDir, "pipewire-0")
		}
		args = append(args, "-v", pipewireSock+":"+filepath.Join(ctrXDGRuntime, "pipewire-0")+":ro")
		exportRuntimeDir()
	}
	if devices := audioDevices(cfg.AudioMeta); len(devices) > 0 {
		for _, device := range devices {
			args = append(args, "--device", device)
		}
		// /dev/snd is reachable by a logind ACL on the seat or by the `audio` group. --device passes the
		// node but not the group, so without keep-groups the device form silently fails on a group host,
		// and always fails with UseNonRootUser, whose mapped subuid is in neither.
		args = append(args, "--group-add", "keep-groups")
	}

	// Config files (section 3 Configs), read-only unless the config says otherwise. The source is
	// joined here rather than resolved into the config, so BundlePath stays the relative thing
	// validation checks.
	for _, configFile := range cfg.Configs {
		bundle := opt.BundleDir
		if bundle == "" {
			return nil, fmt.Errorf("%s: no bundle directory resolved for this app, so Configs[%q] has no source", cfg.AppNameID, configFile.BundlePath)
		}
		// noexec, as Volumes get. ConfigFile deliberately has no Executable field because a
		// config file is data, and podman's bind default is exec, so without this an authored
		// file lands executable with nothing in the schema able to say otherwise.
		mountOpts := "ro,noexec"
		if configFile.Writable {
			mountOpts = "rw,noexec"
		}
		args = append(args, "-v", filepath.Join(bundle, configFile.BundlePath)+":"+configFile.InnerMount+":"+mountOpts)
	}

	// Volumes (section 3). A volume with a host path is a bind mount; one without is scratch
	// space the app is given rather than a location on the host, and it is a tmpfs, which is
	// what makes SizeLimitMiB a limit the kernel holds rather than a number in a file.
	for _, volume := range cfg.Volumes {
		if volume.HostMounted && strings.TrimSpace(volume.HostMount) != "" {
			mountOpts := "ro"
			if volume.Writable {
				mountOpts = "rw"
			}
			if volume.Executable {
				mountOpts += ",exec"
			} else {
				mountOpts += ",noexec"
			}
			args = append(args, "-v", volume.HostMount+":"+volume.InnerMount+":"+mountOpts)
			continue
		}
		args = append(args, "--mount", tmpfsMount(volume))
	}

	// SSH/GPG keys (section 3 Keys) - RO file mounts into the container home.
	for _, key := range cfg.Keys {
		dir := ".ssh"
		if key.Type == schema.GPG {
			dir = ".gnupg"
		}
		args = append(args, "-v", key.Path+":"+filepath.Join(home, dir, filepath.Base(key.Path))+":ro")
	}

	for _, capability := range cfg.Capabilities {
		args = append(args, "--cap-add", capability)
	}

	// Entrypoint override (exec form): replaces the image ENTRYPOINT. A holder runs
	// HolderCmd as PID 1 instead (the app's real command runs per-terminal via
	// ExecArgs), so it ignores the entrypoint.
	if mode != modeHolder {
		if entry := strings.TrimSpace(cfg.StartConditions.Entrypoint); entry != "" {
			args = append(args, "--entrypoint", entry)
		}
	}

	// Image, then (for a holder) the blocking command. A non-holder relies on
	// --entrypoint / the image default; there are no trailing args.
	args = append(args, derived.RunImage(cfg))
	if mode == modeHolder {
		args = append(args, HolderCmd()...)
	}
	return args, nil
}

// tmpfsMount renders an anonymous volume as a podman --mount value. nosuid and nodev always,
// because scratch space is never a place to gain privilege or reach a device; noexec unless the
// config asked otherwise, matching a bind mount's default.
//
// An unlimited tmpfs is half of host RAM, which is podman's default and the honest reading of
// SizeLimited being off. Validation refuses SizeLimited without a positive size.
func tmpfsMount(volume schema.Volume) string {
	opts := []string{"type=tmpfs", "destination=" + volume.InnerMount, "nosuid", "nodev"}
	if !volume.Writable {
		opts = append(opts, "ro")
	}
	if !volume.Executable {
		opts = append(opts, "noexec")
	}
	if volume.SizeLimited {
		opts = append(opts, fmt.Sprintf("tmpfs-size=%dm", volume.SizeLimitMiB))
	}
	return strings.Join(opts, ",")
}

// userArgs decides who the app runs as inside the container. KeepUserID is a separate question:
// rootless already maps the invoking user to root, and --userns=keep-id instead makes the container
// see the SAME uid as the host, which an app sharing a host directory needs.
func userArgs(user schema.InternalUserMeta, inPod bool) []string {
	var args []string
	if user.KeepUserID && !inPod {
		args = append(args, "--userns=keep-id")
	}
	if user.UseNonRootUser && user.NonRootUserName != "" {
		// By name, not uid: the name has to exist in the image's /etc/passwd, and podman
		// fails loudly when it does not. A numeric uid would always "work" and could land
		// on a user the image does not have, with no home and no shell.
		args = append(args, "--user", user.NonRootUserName)
	}
	return args
}

// healthArgs installs ReadyCheck as the container's healthcheck, so `podman ps` reports health for
// the same command a dependent's readiness wait probes.
//
// CMD-SHELL form with every word single-quoted by shellJoin. The JSON exec form is tidier and is
// NOT used: it works on podman 5 and not on the 4.9 Ubuntu LTS ships, where the check can never
// pass. The interval stays at podman's default for the same reason.
func healthArgs(start schema.StartConditions) []string {
	if len(start.ReadyCheck) == 0 {
		return nil
	}
	return []string{"--health-cmd", "CMD-SHELL " + shellJoin(start.ReadyCheck)}
}

// HealthProbeArgs builds `podman healthcheck run <name>`: run the container's healthcheck
// once, now, and exit 0 only if it passed. This is the readiness probe the app layer polls.
func HealthProbeArgs(name string) []string { return []string{"healthcheck", "run", name} }

// HealthProbe runs the app's healthcheck once and reports whether it passed. A container
// that does not exist yet is a failed probe rather than a special case: a dependency is
// started detached, so "no such container" is the ordinary first answer during the moment
// the caller is waiting through, and it stops being the answer on its own.
func (rt Runtime) HealthProbe(name string) error {
	return rt.Exec(ports.Command{Args: HealthProbeArgs(name), Desc: "readiness probe for " + name})
}

// resourceArgs caps what one app may take from the machine. Zero means unlimited throughout,
// matching the schema and podman's default.
func resourceArgs(res schema.ResourcesMeta) []string {
	var args []string
	if res.MaxCPUCores > 0 {
		// 'f' with -1 precision: 0.5 stays "0.5" and 2 stays "2", never "2.000000" or an
		// exponent, both of which podman rejects.
		args = append(args, "--cpus", strconv.FormatFloat(res.MaxCPUCores, 'f', -1, 64))
	}
	if res.MaxRamMiB > 0 {
		args = append(args, "--memory", strconv.FormatInt(res.MaxRamMiB, 10)+"m")
	}
	if res.MaxSwapMiB > 0 && res.MaxRamMiB > 0 {
		// --memory-swap is the TOTAL of memory and swap, not swap alone: passing the swap figure alone
		// would cap a 2048+512 app at 512. Validation requires the memory limit alongside.
		args = append(args, "--memory-swap", strconv.FormatInt(res.MaxRamMiB+res.MaxSwapMiB, 10)+"m")
	}
	if res.PIDsLimit > 0 {
		args = append(args, "--pids-limit", strconv.FormatInt(res.PIDsLimit, 10))
	}
	return args
}

// Lifecycle argv builders (section 9.1). Pure functions returning the arguments to pass to
// `podman` for the container named after the app.
func StopArgs(name string) []string    { return []string{"stop", name} }
func RestartArgs(name string) []string { return []string{"restart", name} }
func InspectArgs(name string) []string { return []string{"inspect", name} }

// LogsArgs builds `podman logs [-f] <name>`.
func LogsArgs(name string, follow bool) []string {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	return append(args, name)
}

// Exec runs one prepared command (pod create / nft lock / holder start), capturing
// output so a failure is reported with its podman error rather than silently. The
// command's Desc labels the error; the app layer adds the app name.
func (Runtime) Exec(cmd ports.Command) error {
	proc := exec.Command("podman", cmd.Args...)
	if cmd.Stdin != "" {
		proc.Stdin = strings.NewReader(cmd.Stdin)
	}
	if out, err := proc.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s%s", cmd.Desc, strings.TrimSpace(string(out)), helperImageHint(cmd, out))
	}
	return nil
}

// Capture runs one command and returns its stdout, for commands whose output is the answer. Only
// stdout: a podman warning spliced into a JSON document would turn a readable error into a parse
// failure. Stderr goes into the error instead.
func (Runtime) Capture(cmd ports.Command) (string, error) {
	proc := exec.Command("podman", cmd.Args...)
	if cmd.Stdin != "" {
		proc.Stdin = strings.NewReader(cmd.Stdin)
	}
	var stderr bytes.Buffer
	proc.Stderr = &stderr
	out, err := proc.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s%s", cmd.Desc, err, strings.TrimSpace(stderr.String()),
			helperImageHint(cmd, stderr.Bytes()))
	}
	return string(out), nil
}

// helperImageHint turns podman's "image not known" into something actionable when the missing image
// is one we were supposed to have built. Helpers run with --pull never (section 5.5), so a user who
// has not run `make netfilter-image` gets no thread to pull. Scoped to zinc/ images: an app's own
// image being absent is a different problem.
func helperImageHint(cmd ports.Command, out []byte) string {
	if !strings.Contains(string(out), "image not known") {
		return ""
	}
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "zinc/") || strings.HasPrefix(arg, "localhost/zinc/") {
			return "\n  hint: " + arg + " is Zinc's own helper image and is built locally, never pulled." +
				"\n        build it once with: make -C container/runner netfilter-image"
		}
	}
	return ""
}

// StartApp starts the app detached (Setsid) so it outlives a launcher that exits. It returns once
// the process is forked, before `podman run` succeeds; onFail then runs from the reaping goroutine,
// so a post-fork failure tears down the prepared pod instead of leaking it.
func (Runtime) StartApp(cfg schema.AppConfig, opt options.HostOptions, runArgs []string, onFail func()) error {
	proc, err := appCmd(cfg, opt, runArgs)
	if err != nil {
		return err
	}
	if err := proc.Start(); err != nil {
		return fmt.Errorf("launch %s: %w", cfg.AppNameID, err)
	}
	go func() {
		// reap if the caller (long-lived TUI) outlives the app; a non-nil Wait means the
		// app died post-fork, so tear down what Prepare left in place.
		if err := proc.Wait(); err != nil && onFail != nil {
			onFail()
		}
	}()
	return nil
}

// appCmd builds the detached command for the app container: a plain `podman run` for a
// GUI app, or the configured terminal emulator wrapping it for a terminal app. Setsid
// puts it in its own session so closing the launcher doesn't take the app down. Split
// out from StartApp so the argv/Setsid wiring is unit-testable.
func appCmd(cfg schema.AppConfig, opt options.HostOptions, runArgs []string) (*exec.Cmd, error) {
	var proc *exec.Cmd
	if cfg.StartConditions.Terminal {
		if len(opt.Terminal) == 0 {
			return nil, fmt.Errorf("%s: terminal app but no terminal emulator configured (set ZINC_TERMINAL)", cfg.AppNameID)
		}
		wrap := TerminalLaunch(opt.Terminal, runArgs, false)
		proc = exec.Command(wrap[0], wrap[1:]...)
	} else {
		proc = exec.Command("podman", runArgs...) // stdio nil → /dev/null; GUI renders via Wayland
	}
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return proc, nil
}

// OpenSession opens one terminal of a multiterminal app: the configured emulator
// wrapping a `podman exec -it` into the holder, running cmd. It blocks until the
// terminal window closes.
func (Runtime) OpenSession(app string, cmd []string, opt options.HostOptions, hold bool) error {
	argv := TerminalLaunch(opt.Terminal, ExecArgs(app, cmd), hold)
	return exec.Command(argv[0], argv[1:]...).Run()
}

// Exists reports whether a container with this name exists (running or not).
func (Runtime) Exists(name string) bool {
	return exec.Command("podman", "container", "exists", name).Run() == nil
}

// IsRunning reports whether the named container is running now. A failed query answers false:
// the callers use it to decide whether to start something, and starting a second holder that
// then collides by name fails loudly, while attaching to a container that is not there does not.
func (Runtime) IsRunning(name string) bool {
	running, err := isRunning(name)
	return err == nil && running
}

// appearWindow and appearPoll bound the wait for a container that does not exist yet - the launch
// still has a pod create, an nft load and a proxy readiness probe to do. A window at all, so a
// launch that failed after the holder started does not leave it there for the session.
const (
	appearWindow = 60 * time.Second
	appearPoll   = 250 * time.Millisecond
)

// WaitGone blocks until the container has appeared and then stopped. The Wayland holder starts
// BEFORE the container (its socket is a bind-mount source), which is why this cannot just be
// `podman wait`. Polling covers only the appearing half. A container that lives and dies inside one
// poll interval is never seen, and this returns at the end of the window instead.
func WaitGone(name string) error {
	engine := Runtime{}
	deadline := time.Now().Add(appearWindow)
	for !engine.Exists(name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not appear within %s", name, appearWindow)
		}
		time.Sleep(appearPoll)
	}
	// "Gone", not "stopped once". `podman wait` returns on EVERY exit, including one podman is about to
	// undo. The caller closes the Wayland close_fd on return, so returning early revoked the context of
	// an app that came straight back, leaving it with no display. A --rm app disappears, a restarting
	// one does not.
	for {
		if err := exec.Command("podman", "wait", name).Run(); err != nil {
			return err
		}
		if !engine.Exists(name) {
			return nil
		}
		// It still exists: either restarting, or exited for good. Treating "exists" as "alive" loops
		// forever for any container without --rm, so give it a restart window and ask whether it is
		// RUNNING.
		if engine.restartsWithin(name, restartWindow) {
			continue
		}
		return nil
	}
}

// restartWindow bounds how long WaitGone waits for a container to come back after an exit.
// Podman restarts on its own within a moment, so this only has to outlast that.
const restartWindow = 3 * time.Second

// restartsWithin reports whether the container is running again before the window elapses.
// A container that is gone entirely counts as not restarting, which is the same answer.
func (rt Runtime) restartsWithin(name string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		time.Sleep(appearPoll)
		switch state, err := isRunning(name); {
		case err != nil:
			// Unknown, not dead. Running() returns an empty map on failure and Exists reads any failure as "no
			// such container", so a held storage lock looked like "the app exited" - and acting on that revokes
			// a running app's display for good. Keep polling; the window closes on its own.
			continue
		case state:
			return true
		}
		if !rt.Exists(name) {
			return false // genuinely gone, not merely unanswerable
		}
	}
	return false
}

// isRunning asks podman about ONE container and distinguishes the three answers that matter:
// running, not running, and could not tell. `podman ps` filtered by name is used rather than
// `inspect` because a missing container is an empty result rather than an error, so the three
// cases stay apart.
func isRunning(name string) (bool, error) {
	out, err := exec.Command("podman", "ps", "--filter", "name=^"+name+"$", "--format", "{{.Names}}").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == name {
			return true, nil
		}
	}
	return false, nil
}

// Do runs a user-facing podman command (stop/restart/inspect/logs) with the host's
// stdio attached, so output and follow-mode stream straight to the terminal.
func (Runtime) Do(args []string) error {
	pc := exec.Command("podman", args...)
	pc.Stdin, pc.Stdout, pc.Stderr = os.Stdin, os.Stdout, os.Stderr
	return pc.Run()
}

// Running returns the set of container names podman currently reports as running. A
// query failure yields an empty set (not an error) so the list view degrades to
// "nothing running" rather than failing to load.
func (Runtime) Running() (map[string]bool, error) {
	set := map[string]bool{}
	out, err := exec.Command("podman", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return set, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// PIDs returns the host PID of each running container's main process. A query failure is an error
// here, unlike Running: "nothing running" and "I could not look" must not arrive as one answer when
// the caller is about to attribute a bus connection to an app.
func (Runtime) PIDs() (map[string]int, error) {
	out, err := exec.Command("podman", "ps", "--format", "{{.Names}} {{.Pid}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list running container pids: %w", err)
	}
	return parsePIDs(string(out)), nil
}

// parsePIDs reads the "<name> <pid>" lines PIDs asks podman for. Split out so the parsing is
// testable without a runtime: a line podman could not fill in (an empty pid for a container
// that exited between listing and formatting) is skipped rather than recorded as pid 0, which
// would match every other unfilled answer and attribute a bus connection to the wrong app.
func parsePIDs(out string) map[string]int {
	pids := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, pidText, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found || name == "" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid <= 0 {
			continue
		}
		pids[name] = pid
	}
	return pids
}

// Logs returns the last tail lines of a container's logs. podman may exit nonzero
// (e.g. the container never ran) but still print useful output, so both are returned
// for the caller to format.
func (Runtime) Logs(name string, tail int) (string, error) {
	out, err := exec.Command("podman", "logs", "--tail", strconv.Itoa(tail), name).CombinedOutput()
	return string(out), err
}

// audioDevices is every ALSA node the directions named, deduplicated: capture and playback on one
// card share its control node, and the argv is what --dry-run prints.
func audioDevices(audio schema.AudioMeta) []string {
	var devices []string
	for _, list := range [][]string{audio.Playback.Devices, audio.Microphone.Devices} {
		for _, device := range list {
			if !slices.Contains(devices, device) {
				devices = append(devices, device)
			}
		}
	}
	return devices
}

// audioUsesSession reports whether any direction asked for the session's own devices, which
// is what puts the PipeWire socket in the container. Monitor counts: a .monitor source is
// part of PipeWire's graph, so the socket is the only thing that can deliver it.
func audioUsesSession(audio schema.AudioMeta) bool {
	return audio.Playback.Default || audio.Microphone.Default || audio.Monitor.Default
}
