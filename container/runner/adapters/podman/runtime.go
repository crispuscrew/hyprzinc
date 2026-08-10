// Package podman is the container-runtime adapter: it implements the Runtime,
// ImageBuilder, and ImageResolver ports against the podman CLI. It is the only place
// that knows podman's argument syntax. AppRunArgs and the *Args builders are pure (no
// I/O) so launch plans can be inspected and dry-run; the rest exec podman.
//
// What it deliberately does NOT decide: the network. AppRunArgs splices in the
// netFlags it is handed by a NetEnforcer (adapters/netenforce), so swapping the egress
// mechanism never touches this file (docs/architecture.md section 5.3, section 13).
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

// TerminalLaunch wraps a `podman ...` argv in the configured terminal emulator so a
// CLI/TUI app (StartConditions.Terminal) appears in its own window. term is the
// emulator argv (e.g. ["foot"] or ["xterm","-e"]); it is run as `term... podman
// <runArgs...>`. It wraps a `run` argv (single-terminal apps) or an `exec` argv
// (multiterminal) alike.
//
// When hold is set the podman invocation is wrapped in the host shell so the window
// pauses after it exits - the user can read final output/errors before the window
// closes. This is emulator-agnostic on purpose: the emulator is user-configured
// (section 9.1), so we don't rely on an emulator-specific --hold flag.
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

// HolderCmd is the main process of a multiterminal app's shared container: a no-op
// that blocks forever so the container outlives any single terminal. It runs under
// `--init` (see modeHolder): a bare `sleep` as PID 1 would ignore `podman stop` until
// the SIGKILL timeout; the injected init (catatonit) owns PID 1, handles SIGTERM, and
// tears down promptly. Needs `sleep` in the image.
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

// AppRunArgs builds the app container's `podman run` argv. netFlags is the network
// attachment supplied by the NetEnforcer (e.g. ["--pod","app-pod"] or
// ["--network","none"]) and is spliced in after the least-privilege baseline; this
// adapter never decides the network itself. The trailing image is derived.RunImage
// (the derived image when ImageMeta.Install is set, else the base). Pure: no I/O.
func (Runtime) AppRunArgs(cfg schema.AppConfig, opt options.HostOptions, netFlags []string) ([]string, error) {
	home := opt.HomeDir
	if home == "" {
		home = "/root"
	}
	// Keys are mounted into the home of whoever runs the app. An app told to run as a
	// non-root user does not get to read /root, so mounting its ssh key there would put the
	// file in the container and still deny it - the failure would look like a broken key
	// rather than a wrong path. /home/<name> is the convention every mainstream image
	// follows for a named user.
	if user := cfg.InternalUserMeta; user.UseNonRootUser && user.NonRootUserName != "" {
		home = "/home/" + user.NonRootUserName
	}

	args := []string{"run"}
	mode := modeFor(cfg)
	// StopConditions.KeepAlive keeps the container after its entrypoint exits, so it is
	// not auto-removed (--rm dropped) for the non-detached modes.
	//
	// Autorestart drops it too, and must: podman REFUSES the pair outright ("the --rm option
	// conflicts with --restart, when the restartPolicy is not \"\" and \"no\""), and it
	// refuses at the CLI layer, so nothing is created and `podman run` exits non-zero. That
	// failure is invisible - StartApp is detached with nil stdio and zcr has already exited -
	// so the launch reported success while leaving the pod, the nft ruleset, the D-Bus proxy
	// and the Wayland holder behind, and the app never started. A restarting container also
	// has to survive its own exit by definition, so keeping it is the correct argv anyway.
	// Reachable from a plain compose import: `restart: always` sets this.
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

	// The rest of the containment baseline: who the app runs as, and how much of the
	// machine it may take. Both are part of the sandbox rather than tuning, so they sit
	// with the capability drop rather than among the optional wiring below.
	// A pod owns the user namespace of everything that joins it, and podman refuses
	// `--userns` on a container joining one ("cannot set user namespace mode when joining pod
	// with infra container"). So a filtered app's keep-id is put on the pod instead, by the
	// enforcer, and left off here - without this the app silently never started.
	args = append(args, userArgs(cfg.InternalUserMeta, slices.Contains(netFlags, "--pod"))...)
	args = append(args, resourceArgs(cfg.ResourcesMeta)...)
	args = append(args, healthArgs(cfg.StartConditions)...)

	// Network attachment is the enforcer's decision (section 5.3) - we only splice it in.
	// The app's own environment. Emitted here, above everything the runner exports, because
	// podman lets a later -e win: the runner's variables describe what it actually built (the
	// runtime dir it mounted, the socket it created, the bus a proxy is serving), so they have
	// to be the ones that survive. validate refuses those three names as well, but that is the
	// second lock, not the only one - which is what this comment used to claim while the code
	// did the opposite.
	//
	// Sorted, because a Go map has no order and this argv is what --dry-run prints, what the
	// reproducible-build check compares, and what a reviewer reads.
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

	// Display / Wayland (section 5.2). The socket mounted is the app's OWN when a security
	// context was established for it (opt.WaylandSocket, filled in by the launch path once
	// the compositor has accepted it), and the compositor's own otherwise. The container-side
	// path and WAYLAND_DISPLAY are identical either way: which socket it is looking at is not
	// the app's business, and an app that had to be configured per mode would be one more
	// thing to keep in sync for no gain.
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
		// The label records which of the two actually happened. It used to be applied whenever
		// the app had not opted out, which claimed a security context on every launch
		// including the ones where none existed - and a label that is wrong is worse than no
		// label, because it is exactly what a desktop would read to decide how much to trust
		// the client.
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

	// Audio (section 3 AudioMeta). The config states a direction and a strength; this picks
	// the transport. `default` in either direction is the session's own device, which means
	// the PipeWire socket, mounted once however many directions asked for it. A device list
	// is passed as exactly those nodes, and nothing else on the sound subsystem is reachable.
	//
	// Worth being precise about what the socket does and does not buy: PipeWire grants a
	// client on it both directions regardless of which one the config asked for, so
	// `Playback: default` with `Microphone: none` is a claim the runtime cannot yet keep.
	// validate.Warnings says so. The device-list form has no such gap, because the kernel is
	// what enforces it.
	if audioUsesSession(cfg.AudioMeta) && opt.RuntimeDir != "" {
		pipewireSock := filepath.Join(opt.RuntimeDir, "pipewire-0")
		args = append(args, "-v", pipewireSock+":"+filepath.Join(ctrXDGRuntime, "pipewire-0")+":ro")
		exportRuntimeDir()
	}
	for _, device := range audioDevices(cfg.AudioMeta) {
		args = append(args, "--device", device)
	}

	// Config files (section 3 Configs): the files the app was authored with, from its own
	// bundle. Read-only unless the config says otherwise, because the point of shipping a
	// file with an app is that what a reviewer read is what the app runs with.
	//
	// The source is joined here rather than being resolved into the config, so BundlePath
	// stays the relative thing validation checks. Rewriting it to an absolute path on load
	// would make the config fail its own validator on the next read.
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

	// Host-mounted volumes (section 3 Volumes). Anonymous/size-limited volumes are deferred;
	// only explicit host bind mounts are wired here.
	for _, volume := range cfg.Volumes {
		if !volume.HostMounted || strings.TrimSpace(volume.HostMount) == "" {
			continue
		}
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
	}

	// SSH/GPG keys (section 3 Keys) - RO file mounts into the container home.
	for _, key := range cfg.Keys {
		dir := ".ssh"
		if key.Type == schema.GPG {
			dir = ".gnupg"
		}
		args = append(args, "-v", key.Path+":"+filepath.Join(home, dir, filepath.Base(key.Path))+":ro")
	}

	// Extra capabilities (section 3 Capabilities).
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

// userArgs decides who the app runs as inside the container. Both fields were in the schema
// and validated from the first release and neither reached podman, so an app that asked to
// run unprivileged ran as root and nothing said otherwise - the worst shape for a setting to
// have on a sandboxing tool.
//
// KeepUserID is the rootless-podman question, not the same one. Rootless maps the invoking
// host user to root inside the container, so a file written into a bind mount comes back
// owned by the host user either way; what --userns=keep-id changes is that the container
// sees the SAME uid as the host, which is what an app sharing a host directory with the
// desktop needs in order to agree about ownership.
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

// healthArgs installs StartConditions.ReadyCheck as the container's healthcheck, which is
// what a dependent's readiness wait probes (HealthProbeArgs). Reusing podman's healthcheck
// rather than exec'ing the probe ourselves means the answer is recorded in container state:
// `podman ps` reports health for the same command the launch sequence waits on, instead of a
// readiness notion only the runner knows about.
//
// Written in the CMD-SHELL form, with every word of the author's command single-quoted by
// shellJoin so an argument containing a space or a quote still means itself. The JSON exec
// form (["CMD", ...]) is tidier and needs no shell in the image, and it is NOT used here: it
// works on podman 5 and does not on the podman 4.9 that Ubuntu LTS ships, where the whole
// bracketed string is handed to a shell instead and the check can never pass. CMD-SHELL is
// the oldest and most portable spelling, and this is a launch-blocking gate - it has to work
// on the podman people actually have.
//
// The interval is left at podman's own default for the same reason. Disabling it (the check
// is driven on demand by the readiness wait, so a timer is not needed) is accepted by both
// versions but is one more thing to differ; keeping the timer also means `podman ps` reports
// live health rather than the last probe's answer, which is worth more than the saved execs.
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

// resourceArgs caps what one app may take from the machine. A container with no limits can
// exhaust the host's memory or fork until nothing else can start, which is a containment
// hole rather than a tuning oversight - these were validated and then dropped on the floor.
//
// Zero means unlimited throughout, matching the schema and podman's own default, so an app
// that sets nothing gets exactly the argv it got before.
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
		// --memory-swap is the TOTAL of memory and swap, not the swap on its own. Passing
		// the swap figure alone would silently shrink the app's memory ceiling instead of
		// adding to it - and on a config asking for 2048 MiB of RAM and 512 of swap, it
		// would cap the whole app at 512. Validation requires the memory limit alongside,
		// so the sum is always the number the author meant.
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

// Capture runs one prepared command and returns its standard output, for the commands whose
// output is the answer rather than a log (reading the netns counters back). Only stdout: a
// podman warning on stderr spliced into the middle of a JSON document would turn a readable
// error into a parse failure with no obvious cause. Stderr goes into the error instead,
// where it is the explanation.
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

// helperImageHint turns podman's "image not known" into something a user can act on when the
// missing image is one WE were supposed to have built.
//
// Every privileged step of a launch - the nft lock-down, the WireGuard setup, the D-Bus proxy -
// runs from the local helper image, deliberately with --pull never, so a launch never fetches
// anything (section 5.5). The cost of that choice is this failure: a user who has not run
// `make netfilter-image` gets "localhost/zinc/netfilter:local: image not known" and no thread to pull,
// because nothing in that sentence says the image was theirs to build. Naming the command costs
// one line and saves the guess.
//
// Scoped to zinc/ images on purpose: an app's own image being absent is a different problem
// with a different fix, and this must not offer the netfilter build as the answer to it.
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

// StartApp starts the app container detached from the caller (Setsid) so it outlives a
// launcher that exits right after it. A terminal app is wrapped in the configured
// emulator; a GUI app renders through the Wayland socket. It returns once the process
// is forked, before `podman run` succeeds; if the app then exits with an error, onFail
// runs from the reaping goroutine so a post-fork failure can tear down the prepared
// (still-filtered) pod/netns instead of leaking it.
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

// appearWindow and appearPoll bound the wait for a container that does not exist yet. The
// launch creates it moments after WaitGone is called, but "moments" covers a pod create, an
// nft load and a D-Bus proxy readiness probe, so the window is generous. It is a window at
// all so that a launch which failed after the holder started does not leave the holder there
// for the rest of the session.
const (
	appearWindow = 60 * time.Second
	appearPoll   = 250 * time.Millisecond
)

// WaitGone blocks until the container named name has appeared and then stopped. It is what
// the Wayland security context holder waits on: the holder is started BEFORE the container
// (its socket is a bind-mount source, so it has to exist first), which is why this cannot
// simply be `podman wait` - that fails immediately on a container nobody has created yet.
//
// Polling only covers the appearing half. Once the container is there, `podman wait` blocks
// in one process for the app's whole life, instead of a poll that would fork a podman per
// interval per running app for hours.
//
// A container that lives and dies inside one poll interval is never seen and this returns at
// the end of the window instead - which costs the holder a minute of idling and nothing else,
// since the app it was holding the context for is already gone.
func WaitGone(name string) error {
	engine := Runtime{}
	deadline := time.Now().Add(appearWindow)
	for !engine.Exists(name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not appear within %s", name, appearWindow)
		}
		time.Sleep(appearPoll)
	}
	// "Gone", not "stopped once". `podman wait` returns on EVERY exit, including one podman
	// is about to undo: a `--restart on-failure` app's first crash, or a plain
	// `podman restart`. The caller closes the Wayland close_fd on return, so returning too
	// early revoked the security context of an app that came straight back - and the
	// restarted container was then bind-mounted onto a socket the compositor no longer
	// accepts on, leaving it with no display at all. Re-checking Exists distinguishes the
	// two: a --rm app disappears, a restarting one does not.
	for {
		if err := exec.Command("podman", "wait", name).Run(); err != nil {
			return err
		}
		if !engine.Exists(name) {
			return nil
		}
		// It still exists, so it is either coming back (restart, or a `podman restart`) or it
		// has exited and is staying that way. Only the first is a reason to keep holding.
		//
		// Existence alone cannot tell them apart, and treating "exists" as "still alive" is an
		// infinite loop for every container that runs without --rm: a KeepAlive or Autorestart
		// app leaves an Exited container behind for good, so `podman wait` returns instantly
		// forever, the security context is never revoked, and its socket is never removed. The
		// architecture's "removed AFTER it dies" then has no time bound at all.
		//
		// So give it a restart window and ask whether it is RUNNING, not whether it is there.
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
		if !rt.Exists(name) {
			return false
		}
		if running, _ := rt.Running(); running[name] {
			return true
		}
	}
	return false
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

// PIDs returns the host PID of each running container's main process, by name.
//
// A query failure is an error here, unlike Running, which degrades to "nothing running" so a
// list view still loads. This answers a question a machine asks - which app owns a given bus
// connection - and "nothing is running" and "I could not look" must not arrive as the same
// answer when the caller's next move is to attribute a capability to an app.
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

// audioDevices is every ALSA node the two directions named, in order and without repeats.
// The two lists overlap in practice: a capture stream needs its card's control node, and so
// does a playback stream on the same card, so a config granting both directions of one
// device names controlC0 twice. Passing --device twice for one node is not an error, but the
// argv is what --dry-run prints and what a reviewer reads, so it should say each node once.
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
