// Command zcr is Zinc's container runtime: it runs an app file
// (~/.config/zinc/apps/<name>.yaml) via podman, applying the egress lock-down before
// the app starts (docs/architecture.md section 5.3, section 9.1). It is the composition root of the
// runner hexagon: it assembles the app.Service from wire and drives it from the CLI.
//
//	zcr run <app> [--exec] [-v HOST:CONTAINER[:OPTIONS]]...
//	                            print the launch plan, or launch it (--exec); -v/--volume
//	                            adds a runtime-only bind mount (repeatable)
//	zcr build <app>             (re)build the app's derived image (ImageMeta.Install)
//	zcr validate <app>          parse + validate; report problems and warnings
//	zcr stop|restart|inspect <app>
//	zcr logs <app> [-f]
//	zcr term <app> [--shell]    open a terminal for a multiterminal app (section 9.1)
//	zcr ps                      running apps, one per line
//	zcr net [app] [--json]      network posture of every running app, or one app's
//	                            nftables counters (section 5.3)
//	zcr image search <term> | resolve <ref>
//
// <app> is a store name (~/.config/zinc/apps) or a path (contains '/' or ends .yaml).
// zc (the creator) shells out to this binary to run what it authors.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/adapters/host"
	"github.com/crispuscrew/zinc/container/runner/adapters/notifyfilter"
	"github.com/crispuscrew/zinc/container/runner/adapters/pipewirectx"
	"github.com/crispuscrew/zinc/container/runner/adapters/podman"
	"github.com/crispuscrew/zinc/container/runner/adapters/waylandctx"
	"github.com/crispuscrew/zinc/container/runner/app"
	"github.com/crispuscrew/zinc/container/runner/domain/derived"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
	"github.com/crispuscrew/zinc/container/runner/ports"
	"github.com/crispuscrew/zinc/container/runner/wire"
)

const usage = `usage: zcr <command> [args]

  run <app[@instance]> [--instance NAME] [--exec] [-v HOST:CONTAINER[:OPTIONS]]...
                            print the launch plan, or launch it (--exec)
                            -v/--volume adds a runtime-only bind mount (repeatable;
                            OPTIONS default ro,noexec - use rw and/or exec)
  build <app>               (re)build the derived image (ImageMeta.Install)
  validate <app>            parse + validate; report problems and warnings
  stop|restart|inspect <app>
  logs <app> [-f]
  term <app> [--shell]      open a terminal for a multiterminal app
  ps                        running apps, one per line
  net [app[@instance]]      no app: every running app and whether its network is
              [--json]      enforced in a netns of its own. An app: what its nftables
                            ruleset has counted since that netns was created
  recheck <app>             re-resolve ImageMeta.SourceTag and report whether the
                            pinned digest is still what that tag points at
  where <app[@instance]> [--json]
                            print where that instance keeps its state, the name its
                            container takes, and its filtered bus socket and proxy
                            (both "none" when the app asked for no bus); ask rather
                            than assume the layout
  bus [--json]              the bus attribution table: every running D-Bus proxy, its
                            host pid, and the app@instance it serves - what turns a
                            connection seen on the host bus into an app
  image search <term> | resolve <ref>
  version                   print the version

<app> is a store name (~/.config/zinc/apps) or a path (has '/' or ends in .yaml).`

// cmdRecheck re-resolves ImageMeta.SourceTag and compares it with the pinned digest. It changes
// nothing: a moved tag is not automatically a tag you want. Exit 0 current, 1 error, 2 moved.
func cmdRecheck(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr recheck <app>")
	}
	cfg, err := loadApp(svc, argv[0])
	if err != nil {
		return err
	}
	tag := strings.TrimSpace(cfg.ImageMeta.SourceTag)
	if tag == "" {
		return fmt.Errorf("%s: no ImageMeta.SourceTag recorded, so there is no tag to re-resolve; the digest was pinned by hand and nothing knows what it was a pin OF", cfg.AppNameID)
	}
	// The validator refuses a digest here, but validation runs at save and at launch - not on
	// the plain load this command does - so a file edited by hand reaches here unchecked. A
	// digest re-resolves to itself, which would compare one digest against another and report
	// a confident, meaningless answer.
	if strings.Contains(tag, "@sha256:") {
		return fmt.Errorf("%s: ImageMeta.SourceTag is a digest (%s), not a tag - re-resolving it returns itself, so it can never report the pin as stale; record the tag the digest came from", cfg.AppNameID, tag)
	}
	resolved, err := svc.Resolve(tag)
	if err != nil {
		return fmt.Errorf("%s: re-resolving %s: %w", cfg.AppNameID, tag, err)
	}
	fmt.Printf("tag: %s\n", tag)
	fmt.Printf("pinned: %s\n", cfg.ImageMeta.Image)
	fmt.Printf("current: %s\n", resolved)
	if resolved == cfg.ImageMeta.Image {
		fmt.Println("status: current")
		return nil
	}
	fmt.Println("status: moved")
	// Not an error in the "something went wrong" sense - the check worked and the answer is
	// that the tag moved - but a distinct exit code, so a scheduled checker can act on it.
	os.Exit(2)
	return nil
}

// version is stamped at build time via -ldflags "-X main.version=..." (the Makefile
// derives it from `git describe`). It stays "dev" for a plain build.
var version = "dev"

// versionString returns the build-stamped version, falling back to the module version
// embedded by `go install <pkg>@vX` when ldflags did not set one.
func versionString() string {
	if version != "dev" && version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zcr: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) < 1 {
		return fmt.Errorf("%s", usage)
	}
	if argv[0] == "version" || argv[0] == "--version" {
		fmt.Println("zcr " + versionString())
		return nil
	}
	svc, err := wire.DefaultService()
	if err != nil {
		return err
	}
	opt := host.Options()

	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "run":
		return cmdRun(svc, opt, rest)
	case "build":
		return cmdBuild(svc, rest)
	case "validate":
		return cmdValidate(svc, rest)
	case "stop", "restart", "inspect":
		return cmdLifecycle(svc, opt, cmd, rest)
	case "logs":
		return cmdLogs(svc, rest)
	case "term":
		return cmdTerm(svc, opt, rest)
	case "__term":
		// Hidden: the per-terminal waiter spawned by OpenTerminal. It blocks until the
		// terminal closes and does the last-one-out stop.
		return cmdTermWaiter(svc, opt, rest)
	case waylandctx.HoldCommand:
		// Hidden: the per-app Wayland security context holder spawned by the launch path.
		// It creates the app's own compositor socket, reports back, and holds the
		// revocation descriptor open until the app container is gone.
		return cmdWaylandHolder(opt, rest)
	case pipewirectx.HoldCommand:
		// Hidden: the per-app audio holder. It creates the app's own PipeWire socket under a
		// security context, holds the revocation descriptor, and sets the app's permissions
		// so the directions its config did not grant stay unreachable.
		return cmdPipeWireHolder(opt, rest)
	case "__supervise":
		// Hidden: the per-app supervisor. It outlives this process, waits for the app's
		// container to be gone, and tears down what the launch built around it.
		return cmdSupervise(svc, rest)
	case notifyfilter.HoldCommand:
		// Hidden: the per-app notification filter. It serves the socket the app was given and
		// relays to the D-Bus proxy behind it, rewriting the calls NotificationMeta narrows.
		return cmdNotifyFilter(svc, opt, rest)
	case "ps":
		return cmdPs(svc)
	case "net":
		return cmdNet(svc, opt, rest)
	case "where":
		return cmdWhere(svc, opt, rest)
	case "bus":
		return cmdBus(svc, opt, rest)
	case "recheck":
		return cmdRecheck(svc, rest)
	case "image":
		return cmdImage(svc, rest)
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
}

func cmdRun(svc app.Service, opt options.HostOptions, argv []string) error {
	name, execute, runtimeVolumes, err := parseRunArgs(argv)
	if err != nil {
		return err
	}
	cfg, err := loadLaunchable(svc, name)
	if err != nil {
		return err
	}
	// Runtime-only volumes: appended in memory, never written back. Both branches validate the whole
	// config first, so these get the same checkVolume guards as configured Volumes.
	cfg.Volumes = append(cfg.Volumes, runtimeVolumes...)
	if execute {
		return svc.Launch(cfg, opt)
	}
	if verr := validate.Validate(cfg); verr != nil {
		return fmt.Errorf("invalid config %s:\n%w", name, verr)
	}
	if derived.HasInstall(cfg) {
		// The app runs a derived image (FROM ImageMeta.Image + the install layer); a real
		// run builds it first (auto on change, or `zcr build`). Show it so the plan matches.
		fmt.Println("# build derived image first (auto on run when stale, or: zcr build " + cfg.AppNameID + ")")
		fmt.Println("podman " + strings.Join(quoteForDisplay(podman.ImageBuildArgs(cfg)), " "))
		fmt.Print(derived.DerivedContainerfile(cfg))
	}
	for _, warn := range validate.Warnings(cfg) {
		fmt.Println("# WARNING: " + warn)
	}
	// The plan below mounts the compositor's own Wayland socket, because a dry run must not
	// create a security context and a socket that does not exist could not be pasted into a
	// shell. Saying what a real launch does instead keeps the printed commands runnable
	// without the plan quietly understating the sandbox (section 5.2).
	if waylandctx.Applies(cfg, opt) {
		fmt.Println("# a real launch first registers a per-instance Wayland socket with the compositor")
		fmt.Println("# (a detached `zcr " + waylandctx.HoldCommand + "` holder) and mounts THAT in place of " + opt.WaylandDisplay + " below;")
		fmt.Println("# it falls back to exactly what is shown here if the compositor has no wp_security_context_v1")
	}
	plan, err := svc.Plan(cfg, opt)
	if err != nil {
		return err
	}
	printPlan(plan)
	if cfg.StartConditions.Multiterminal {
		// The plan starts the shared holder; each terminal attaches with this exec
		// (run `zcr term <app>` to open one, `--shell` for a shell).
		fmt.Println("# each terminal attaches to the holder")
		fmt.Println("podman " + strings.Join(quoteForDisplay(podman.ExecArgs(cfg.AppNameID, termCmd(cfg))), " "))
	}
	return nil
}

// runUsage is the usage line for `zcr run`, shared by its argument errors.
const runUsage = "usage: zcr run <app> [--exec] [-v HOST:CONTAINER[:OPTIONS]]..."

// parseRunArgs splits `zcr run`'s arguments into the app name, --exec, and repeated -v/--volume
// mounts. Flags may come before or after the name; -v VALUE and -v=VALUE are both accepted.
func parseRunArgs(argv []string) (name string, execute bool, volumes []schema.Volume, err error) {
	// --instance is the flag form of the "app@instance" address. Both exist because they are
	// convenient in different places - a flag reads better in a hand-typed command, an address
	// travels better through a script that already has one string - and they fold into the same
	// address here so nothing downstream has to know which was used.
	var instance string
	for idx := 0; idx < len(argv); idx++ {
		arg := argv[idx]
		switch {
		case arg == "--exec":
			execute = true
		case arg == "-v" || arg == "--volume":
			idx++
			if idx >= len(argv) {
				return "", false, nil, fmt.Errorf("%s: missing value (want HOST:CONTAINER[:OPTIONS])", arg)
			}
			vol, verr := parseVolumeSpec(argv[idx])
			if verr != nil {
				return "", false, nil, verr
			}
			volumes = append(volumes, vol)
		case strings.HasPrefix(arg, "-v="):
			vol, verr := parseVolumeSpec(strings.TrimPrefix(arg, "-v="))
			if verr != nil {
				return "", false, nil, verr
			}
			volumes = append(volumes, vol)
		case strings.HasPrefix(arg, "--volume="):
			vol, verr := parseVolumeSpec(strings.TrimPrefix(arg, "--volume="))
			if verr != nil {
				return "", false, nil, verr
			}
			volumes = append(volumes, vol)
		case arg == "--instance":
			idx++
			if idx >= len(argv) {
				return "", false, nil, fmt.Errorf("--instance: missing value (want a name like 'work')")
			}
			instance = argv[idx]
		case strings.HasPrefix(arg, "--instance="):
			instance = strings.TrimPrefix(arg, "--instance=")
		case strings.HasPrefix(arg, "-"):
			return "", false, nil, fmt.Errorf("unknown flag %q\n%s", arg, runUsage)
		case name == "":
			name = arg
		default:
			return "", false, nil, fmt.Errorf("unexpected argument %q\n%s", arg, runUsage)
		}
	}
	if name == "" {
		return "", false, nil, fmt.Errorf("%s", runUsage)
	}
	if instance != "" {
		if strings.Contains(name, "@") {
			return "", false, nil, fmt.Errorf("%q already names an instance, so --instance would have to override it; give one or the other", name)
		}
		name += "@" + instance
	}
	return name, execute, volumes, nil
}

// parseVolumeSpec parses HOST:CONTAINER[:OPTIONS] into an in-memory Volume. Default is read-only
// and non-executable; "rw" and "exec" widen it. Field-shift characters are caught by the config
// validation cmdRun runs before this reaches podman.
func parseVolumeSpec(spec string) (schema.Volume, error) {
	fields := strings.Split(spec, ":")
	if len(fields) < 2 || len(fields) > 3 {
		return schema.Volume{}, fmt.Errorf("--volume %q: want HOST:CONTAINER[:OPTIONS]", spec)
	}
	host, inner := fields[0], fields[1]
	if strings.TrimSpace(host) == "" {
		return schema.Volume{}, fmt.Errorf("--volume %q: empty HOST path", spec)
	}
	if strings.TrimSpace(inner) == "" {
		return schema.Volume{}, fmt.Errorf("--volume %q: empty CONTAINER path", spec)
	}
	vol := schema.Volume{HostMounted: true, HostMount: host, InnerMount: inner}
	if len(fields) == 3 {
		for _, mountOpt := range strings.Split(fields[2], ",") {
			switch strings.TrimSpace(mountOpt) {
			case "rw":
				vol.Writable = true
			case "ro":
				vol.Writable = false
			case "exec":
				vol.Executable = true
			case "noexec":
				vol.Executable = false
			default:
				return schema.Volume{}, fmt.Errorf("--volume %q: unknown option %q (want rw, ro, exec, noexec)", spec, mountOpt)
			}
		}
	}
	return vol, nil
}

// termCmd is the argv each terminal of a multiterminal app runs: its
// MultiterminalEntrypoint, else its Entrypoint, split into fields.
func termCmd(cfg schema.AppConfig) []string {
	spec := cfg.StartConditions.MultiterminalEntrypoint
	if strings.TrimSpace(spec) == "" {
		spec = cfg.StartConditions.Entrypoint
	}
	return strings.Fields(spec)
}

// cmdBuild (re)builds an app's derived image (FROM ImageMeta.Image + ImageMeta.Install).
// A plain `zcr run` already rebuilds on demand when the install line or base changes;
// this is the explicit build (section 5.5, section 9.1).
func cmdBuild(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr build <app>")
	}
	cfg, err := loadLaunchable(svc, argv[0])
	if err != nil {
		return err
	}
	if verr := validate.Validate(cfg); verr != nil {
		return fmt.Errorf("invalid config %s:\n%w", argv[0], verr)
	}
	if !derived.HasInstall(cfg) {
		return fmt.Errorf("%s: no ImageMeta.Install set - nothing to build; it runs %s directly", cfg.AppNameID, cfg.ImageMeta.Image)
	}
	fmt.Printf("# building %s (FROM %s)\n", derived.DerivedImageRef(cfg.AppNameID), cfg.ImageMeta.Image)
	if err := svc.Build(cfg); err != nil {
		return err
	}
	fmt.Printf("built %s\n", derived.DerivedImageRef(cfg.AppNameID))
	return nil
}

func cmdValidate(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr validate <app>")
	}
	cfg, err := loadApp(svc, argv[0])
	if err != nil {
		return err
	}
	if verr := validate.Validate(cfg); verr != nil {
		return fmt.Errorf("invalid config %s:\n%w", argv[0], verr)
	}
	fmt.Printf("ok: %s - image=%s\n", cfg.AppNameID, cfg.ImageMeta.Image)
	for _, warn := range validate.Warnings(cfg) {
		fmt.Println("warning: " + warn)
	}
	return nil
}

func cmdLifecycle(svc app.Service, opt options.HostOptions, cmd string, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr %s <app>", cmd)
	}
	name := argv[0]
	cfg, err := loadApp(svc, name)
	if err != nil {
		return err
	}
	if cmd == "inspect" {
		return svc.Do(podman.InspectArgs(cfg.AppNameID))
	}
	switch cmd {
	case "stop":
		return svc.Stop(cfg)
	case "restart":
		if len(cfg.NetworkMeta.NetworkLists) > 0 {
			// nft rules live in the pod's netns and are lost on a plain pod restart, so tear
			// the pod down and relaunch through the service (re-applies them).
			_ = svc.Stop(cfg)
			return svc.Launch(cfg, opt)
		}
		// cfg.AppNameID, not name: loadApp has already resolved "app@instance" (and a config
		// given by path) to the runtime name podman actually knows. Passing the raw argument
		// looked for a container literally called "app@instance", which never exists.
		return svc.Do(podman.RestartArgs(cfg.AppNameID))
	}
	return fmt.Errorf("unreachable: %q", cmd)
}

func cmdLogs(svc app.Service, argv []string) error {
	fset := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fset.Bool("f", false, "follow log output")
	if err := fset.Parse(argv); err != nil {
		return err
	}
	if fset.NArg() != 1 {
		return fmt.Errorf("usage: zcr logs <app> [-f]")
	}
	// loadApp rather than refuseVM: it refuses a VM app the same way, and additionally
	// resolves "app@instance" to the runtime name podman knows. Logs asked for the raw
	// argument, so an instance's logs were never reachable.
	cfg, err := loadApp(svc, fset.Arg(0))
	if err != nil {
		return err
	}
	return svc.Do(podman.LogsArgs(cfg.AppNameID, *follow))
}

// parseTermArgs splits `<app> [--shell]`, shared by `term` and the hidden `__term`.
func parseTermArgs(argv []string) (name string, shell bool, err error) {
	for _, arg := range argv {
		switch {
		case arg == "--shell":
			shell = true
		case strings.HasPrefix(arg, "-"):
			return "", false, fmt.Errorf("unknown flag %q\nusage: zcr term <app> [--shell]", arg)
		case name == "":
			name = arg
		default:
			return "", false, fmt.Errorf("unexpected argument %q\nusage: zcr term <app> [--shell]", arg)
		}
	}
	if name == "" {
		return "", false, fmt.Errorf("usage: zcr term <app> [--shell]")
	}
	return name, shell, nil
}

// cmdTerm opens one more terminal for a multiterminal app: it spawns a detached waiter
// and returns. The first terminal starts the shared holder (section 9.1).
func cmdTerm(svc app.Service, opt options.HostOptions, argv []string) error {
	name, shell, err := parseTermArgs(argv)
	if err != nil {
		return err
	}
	cfg, err := loadLaunchable(svc, name)
	if err != nil {
		return err
	}
	return svc.OpenTerminal(cfg, opt, shell)
}

// cmdTermWaiter is the hidden `__term` waiter: it opens one terminal and stops the
// holder if it is the last to close.
func cmdTermWaiter(svc app.Service, opt options.HostOptions, argv []string) error {
	name, shell, err := parseTermArgs(argv)
	if err != nil {
		return err
	}
	cfg, err := loadApp(svc, name)
	if err != nil {
		return err
	}
	return svc.Term(cfg, opt, shell)
}

// cmdWaylandHolder is the hidden holder process owning one app's Wayland security context
// (section 5.2), because `zcr run` detaches and the context dies with its descriptor. It takes the
// ADDRESS, not the runtime name: app_id is stable across instances, instance_id is not.
func cmdWaylandHolder(opt options.HostOptions, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr %s <app[@instance]>", waylandctx.HoldCommand)
	}
	addr, err := paths.ParseAddress(argv[0])
	if err != nil {
		return err
	}
	if strings.TrimSpace(addr.App) == "" {
		return fmt.Errorf("usage: zcr %s <app[@instance]>", waylandctx.HoldCommand)
	}
	return waylandctx.Hold(addr, opt, podman.WaitGone)
}

// cmdSupervise is the hidden supervisor. It takes the address so an instanced app resolves, and
// tears the app down once its container is gone - which is the only thing that does, since every
// front-end launches through a `zcr` that exits moments later.
func cmdSupervise(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr __supervise <app[@instance]>")
	}
	cfg, err := load(svc, argv[0])
	if err != nil {
		return err
	}
	return svc.Supervise(cfg, podman.WaitGone)
}

// cmdNotifyFilter is the hidden notification filter (section 3 NotificationMeta). It takes the
// app name rather than an address: the filter is per app like its bus and its bundle, because
// what it enforces is authored content rather than anything an instance carries.
func cmdNotifyFilter(svc app.Service, opt options.HostOptions, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zcr %s <app[@instance]>", notifyfilter.HoldCommand)
	}
	cfg, err := load(svc, argv[0])
	if err != nil {
		return err
	}
	upstream := dbusproxy.HostSocketPath(opt.RuntimeDir, cfg.AppNameID)
	if upstream == "" {
		return fmt.Errorf("%s: no bus socket to filter", cfg.AppNameID)
	}
	return notifyfilter.Hold(cfg, upstream, opt, podman.WaitGone)
}

// cmdPipeWireHolder is the hidden holder process owning one app's PipeWire security context and
// its permissions (section 3 AudioMeta). Separate from the Wayland holder because it outlives the
// launch for a second reason: the graph changes while an app runs, and a microphone plugged in an
// hour later is a new object the app would otherwise hold the default permission on.
func cmdPipeWireHolder(opt options.HostOptions, argv []string) error {
	usage := fmt.Errorf("usage: zcr %s <app[@instance]> [%s]", pipewirectx.HoldCommand, pipewirectx.MicrophoneFlag)
	if len(argv) < 1 || len(argv) > 2 {
		return usage
	}
	addr, err := paths.ParseAddress(argv[0])
	if err != nil {
		return err
	}
	if strings.TrimSpace(addr.App) == "" {
		return usage
	}
	capture := false
	if len(argv) == 2 {
		if argv[1] != pipewirectx.MicrophoneFlag {
			return usage
		}
		capture = true
	}
	return pipewirectx.Hold(addr, capture, opt, podman.WaitGone)
}

// cmdPs prints the apps podman reports as running, one per line and sorted, so a
// front-end (zc) can read it to show live state.
func cmdPs(svc app.Service) error {
	running, err := svc.Running()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(running))
	for name, up := range running {
		if up {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
	return nil
}

// cmdImage helps choose an image without a browser: search registries, or resolve a tag
// to its digest-pinned form (section 5.5) ready to paste into ImageMeta.Image.
func cmdImage(svc app.Service, argv []string) error {
	if len(argv) != 2 {
		return fmt.Errorf("usage: zcr image search <term> | zcr image resolve <ref>")
	}
	sub, arg := argv[0], argv[1]
	switch sub {
	case "search":
		results, err := svc.Search(arg)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("no images found")
			return nil
		}
		for _, result := range results {
			fmt.Printf("%s\t%s\n", result.Name, result.Description)
		}
		return nil
	case "resolve", "pin":
		pinned, err := svc.Resolve(arg)
		if err != nil {
			return err
		}
		fmt.Println(pinned)
		return nil
	default:
		return fmt.Errorf("unknown image subcommand %q (want search|resolve)", sub)
	}
}

// printPlan shows the launch as the exact podman command(s). For a filtered app this is
// the multi-step pod flow; the nft ruleset piped in is printed too, so what will be
// enforced is fully visible before anything runs.
func printPlan(plan []ports.Command) {
	for _, cmd := range plan {
		fmt.Println("# " + cmd.Desc)
		fmt.Println("podman " + strings.Join(quoteForDisplay(cmd.Args), " "))
		if cmd.Stdin != "" {
			fmt.Print(cmd.Stdin)
		}
	}
}

// loadApp resolves an app by store name or file path. The app-type check lives here because one
// store holds both container and VM apps; the passthroughs that never load a config use refuseVM.
func loadApp(svc app.Service, arg string) (schema.AppConfig, error) {
	cfg, err := load(svc, arg)
	if err != nil {
		return schema.AppConfig{}, err
	}
	if cfg.Type == schema.ZincVirtualization {
		// Refuse rather than try: none of what follows - the image build, the pod, the
		// nftables lock-down - means anything for a guest, and a half-applied container
		// launch is exactly the mis-enforcement the network model refuses elsewhere.
		return schema.AppConfig{}, fmt.Errorf("app %q is a VM app (Type: %s); run it with zvr", cfg.AppNameID, cfg.Type)
	}
	// Screen the name, always: the path form accepts whatever the file claims, and AppNameID becomes a
	// container name, a pod name and a path segment inside an `rm -rf` ("--all", "../..").
	//
	// Only the name. Full validation belongs to the verbs that COMPOSE something; loadApp also serves
	// the verbs acting on a running app, so validating everything here would make any newly tightened
	// rule leave running apps unstoppable. Refusing to START is safety; refusing to STOP is a wedge.
	if err := validate.AppName(cfg.AppNameID); err != nil {
		return schema.AppConfig{}, fmt.Errorf("%s: %w", arg, err)
	}
	return cfg, nil
}

// loadLaunchable is loadApp plus full validation, for the verbs that build an argv, an image
// or a ruleset from a config rather than acting on one that already exists.
func loadLaunchable(svc app.Service, arg string) (schema.AppConfig, error) {
	cfg, err := loadApp(svc, arg)
	if err != nil {
		return schema.AppConfig{}, err
	}
	if err := validate.Validate(cfg); err != nil {
		return schema.AppConfig{}, err
	}
	return cfg, nil
}

// refuseForgedIdentity refuses a file whose AppNameID belongs to an app in the store.
//
// AppNameID is not a label, it is the identity everything downstream reads: the container and pod
// names, the Wayland app_id the compositor is told, the row `zcr bus` attributes a bus connection
// to, and the app `zcr net` reports a posture for. A file anywhere on disk could claim
// `AppNameID: firefox` and be run, and from the outside the result WAS firefox - same container
// name, same app_id, same attribution row. Every surface Zinc offers to prove what an app is
// would have agreed with it.
//
// The store's own file for that app is the exception: running it by path is running it by name.
func refuseForgedIdentity(svc app.Service, arg string, cfg schema.AppConfig) error {
	claimed := strings.TrimSpace(cfg.AppNameID)
	if claimed == "" || !svc.Exists(claimed) {
		return nil
	}
	stored, serr := filepath.Abs(svc.Path(claimed))
	given, gerr := filepath.Abs(arg)
	if serr == nil && gerr == nil && stored == given {
		return nil // this IS that app's file
	}
	return fmt.Errorf("%s claims AppNameID %q, which is a different app in the store: that name is "+
		"the container name, the Wayland app_id and the bus attribution row, so running this file would "+
		"make it indistinguishable from %s. Rename it, or run the store's own app by name",
		arg, claimed, claimed)
}

// refuseVM keeps inspect and logs off VM apps, which would otherwise fail with podman's "no such
// object" - true, but silent about zvr owning the app. An unknown name is left alone: it may be a
// raw container.
func refuseVM(svc app.Service, name string) error {
	cfg, err := load(svc, name)
	if err != nil || cfg.Type != schema.ZincVirtualization {
		// Not a defined app (it may legitimately be a raw container name), or unreadable,
		// or a container: either way this guard has nothing to say and podman answers.
		return nil
	}
	return fmt.Errorf("app %q is a VM app (Type: %s); use zvr", name, cfg.Type)
}

// load reads the app a command names, with its Inherits chain applied: the runner acts on
// what an app IS, not on the part of it that happens to be written in its own file.
func load(svc app.Service, arg string) (schema.AppConfig, error) {
	if strings.Contains(arg, "/") || strings.HasSuffix(arg, ".yaml") {
		// A path names a file, and a file is one definition. Instances address the store,
		// where a name can be run more than once.
		cfg, err := svc.LoadFileResolved(arg)
		if err != nil {
			return schema.AppConfig{}, err
		}
		if err := refuseForgedIdentity(svc, arg, cfg); err != nil {
			return schema.AppConfig{}, err
		}
		return cfg, nil
	}
	addr, err := paths.ParseAddress(arg)
	if err != nil {
		return schema.AppConfig{}, err
	}
	if !svc.Exists(addr.App) {
		return schema.AppConfig{}, fmt.Errorf("no app %q defined (try: zc list)", addr.App)
	}
	cfg, err := svc.LoadResolved(addr.App)
	if err != nil {
		return schema.AppConfig{}, err
	}
	// The instance rides on AppNameID from here down, because every runtime name derives from it: pod,
	// container, healthcheck, proxy, socket directory, teardown. Rewriting it once here beats threading
	// a second identifier through every adapter and missing one. An un-instanced app is unchanged.
	cfg.AppNameID = addr.Runtime()
	if err := expandMounts(&cfg, addr); err != nil {
		return schema.AppConfig{}, err
	}
	return cfg, nil
}

// expandMounts resolves {state}/{app}/{instance} in HOST mount paths, so one definition serves many
// instances. The container side is not templated: the app is meant to be unaware of the others.
func expandMounts(cfg *schema.AppConfig, addr paths.Address) error {
	for index := range cfg.Volumes {
		expanded, err := addr.Expand(cfg.Volumes[index].HostMount)
		if err != nil {
			return fmt.Errorf("Volumes[%d]: %w", index, err)
		}
		cfg.Volumes[index].HostMount = expanded
	}
	// Configs are deliberately absent here. A bundle path names authored content that ships
	// with the app, not a runtime location, so {state} and friends have nothing to say about
	// it - and expanding them produced an absolute path that this config's own validator then
	// rejected for being absolute, which meant a placeholder in a Config could never work.
	return nil
}

// quoteForDisplay lightly quotes args with whitespace, for readable printing only.
func quoteForDisplay(args []string) []string {
	out := make([]string, len(args))
	for idx, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			out[idx] = "'" + arg + "'"
		} else {
			out[idx] = arg
		}
	}
	return out
}
