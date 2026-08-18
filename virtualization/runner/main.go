// Command zvr is the Zinc virtualization runner: it boots VM apps as qemu guests, the
// sibling of zcr for containers. The two share one app store and one schema and split by
// Type, so a VM app and a container app are authored the same way and each runner takes
// the ones it owns.
//
// zvr drives qemu directly rather than libvirt. That costs it the lifecycle management a
// daemon would provide - starting, finding and stopping guests is this program's job -
// and buys the thing these VMs are for: qemu runs inside the user's own session, so a
// guest can open a GPU-accelerated window on their compositor with frames that never
// leave the machine.
//
//	zvr run <app>            boot a VM app (detached; the window, if any, is qemu's)
//	zvr run <app> --dry-run  print the exact qemu command line and change nothing
//	zvr stop <app> [--force] shut a guest down (ACPI power button, or signal it)
//	zvr ps                   list running guests
//	zvr status <app>         one guest's state
//	zvr validate <app|path>  check a config without running anything
//	zvr reset <app>          delete the guest's disk, returning it to the pinned base
//	zvr pin <image>          print the sha256 pin for a base image
//	zvr install --disk ...   run an OS installer to produce a base disk (Windows)
//	zvr console <app>        where to attach for the guest's serial console
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
	"github.com/crispuscrew/zinc/virtualization/runner/adapters/disk"
	"github.com/crispuscrew/zinc/virtualization/runner/adapters/fs"
	"github.com/crispuscrew/zinc/virtualization/runner/app"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/paths"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/qemu"
)

// version is the release, stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage:
  zvr run <app> [--dry-run]     boot a VM app; --dry-run prints the qemu command instead
  zvr stop <app> [--force]      shut it down (--force signals the process instead of asking)
  zvr ps                        list running guests
  zvr status <app>              one guest's state
  zvr validate <app|path.yaml>  check a config, run nothing
  zvr reset <app>               delete the guest's disk, back to the pinned base
  zvr pin <image.qcow2>         print the sha256 pin to put in a config
  zvr install --disk PATH --media ISO...
                                run an OS installer to produce a base disk (Windows)
  zvr console <app>             print how to attach to the guest's serial console
  zvr version`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zvr: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command\n%s", usage)
	}
	command, rest := argv[0], argv[1:]

	switch command {
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	case "version", "--version":
		fmt.Println("zvr " + versionString())
		return nil
	case "pin":
		return cmdPin(rest)
	case "install":
		// Standalone: it creates a base disk rather than acting on an existing app.
		return cmdInstall(rest)
	}

	svc, err := service()
	if err != nil {
		return err
	}
	switch command {
	case "run":
		return cmdRun(svc, rest)
	case "stop":
		return cmdStop(svc, rest)
	case "ps":
		return cmdPS(svc, rest)
	case "status":
		return cmdStatus(svc, rest)
	case "validate":
		return cmdValidate(svc, rest)
	case "reset":
		return cmdReset(svc, rest)
	case "console":
		return cmdConsole(svc, rest)
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

func service() (app.Service, error) {
	store, err := fs.Default()
	if err != nil {
		return app.Service{}, err
	}
	layout, err := paths.Default()
	if err != nil {
		return app.Service{}, err
	}
	return app.New(store, layout), nil
}

func cmdRun(svc app.Service, argv []string) error {
	name, flags := splitFlags(argv)
	if name == "" {
		return fmt.Errorf("usage: zvr run <app> [--dry-run]")
	}
	cfg, err := loadApp(svc, name)
	if err != nil {
		return err
	}
	if flags["--dry-run"] {
		args, err := svc.Plan(cfg)
		if err != nil {
			return err
		}
		// Printed as one line so it can be copied and run as-is, which is the point: the
		// operator can see and reproduce exactly what zvr would have started.
		fmt.Println(qemu.Display(args))
		return nil
	}
	if err := svc.Run(cfg); err != nil {
		return err
	}
	fmt.Printf("started %s (%d MiB, %d vCPU, display %s)\n",
		cfg.AppNameID, cfg.VirtualizationMeta.MemoryMiB, cfg.VirtualizationMeta.VCPUs,
		cfg.VirtualizationMeta.Display)
	return nil
}

func cmdStop(svc app.Service, argv []string) error {
	name, flags := splitFlags(argv)
	if name == "" {
		return fmt.Errorf("usage: zvr stop <app> [--force]")
	}
	if err := svc.Stop(name, flags["--force"], app.DefaultStopTimeout); err != nil {
		return err
	}
	fmt.Println("stopped " + name)
	return nil
}

func cmdPS(svc app.Service, argv []string) error {
	if len(argv) != 0 {
		return fmt.Errorf("usage: zvr ps")
	}
	states, err := svc.Running()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("no guests running")
		return nil
	}
	fmt.Printf("%-24s %-8s %s\n", "APP", "PID", "STATE")
	for _, state := range states {
		fmt.Printf("%-24s %-8d %s\n", state.Name, state.PID, describe(state.Guest, state.Detail))
	}
	return nil
}

func cmdStatus(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zvr status <app>")
	}
	state, err := svc.State(argv[0])
	if err != nil {
		return err
	}
	if !state.Alive {
		fmt.Printf("%s: not running\n", argv[0])
		return nil
	}
	fmt.Printf("%s: running (pid %d, guest %s)\n", state.Name, state.PID, describe(state.Guest, state.Detail))
	return nil
}

// describe renders a guest's reported state, falling back to why it could not be asked.
func describe(guest, detail string) string {
	switch {
	case guest != "":
		return guest
	case detail != "":
		return "unknown (" + detail + ")"
	default:
		return "unknown"
	}
}

func cmdValidate(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zvr validate <app|path.yaml>")
	}
	cfg, err := loadApp(svc, argv[0])
	if err != nil {
		return err
	}
	if err := validate.Validate(cfg); err != nil {
		return err
	}
	if cfg.Type != schema.ZincVirtualization {
		return fmt.Errorf("app %q is a container app (Type: %s); validate it with zcr", cfg.AppNameID, cfg.Type)
	}
	fmt.Printf("%s: valid\n", cfg.AppNameID)
	for _, warning := range validate.Warnings(cfg) {
		fmt.Println("warning: " + warning)
	}
	return nil
}

func cmdReset(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zvr reset <app>")
	}
	if err := svc.Reset(argv[0]); err != nil {
		return err
	}
	fmt.Printf("%s: disk deleted; the next run starts from the pinned base image\n", argv[0])
	return nil
}

// cmdConsole reports where the guest's serial console is. Attaching is left to a terminal
// tool rather than reimplemented here: this build does not put the terminal into raw
// mode, and a half-working console that mangles keys would be worse than pointing at one
// that works.
func cmdConsole(svc app.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zvr console <app>")
	}
	state, err := svc.State(argv[0])
	if err != nil {
		return err
	}
	if !state.Alive {
		return fmt.Errorf("%s is not running", argv[0])
	}
	socket := svc.Runtime.ConsolePath(argv[0])
	fmt.Printf("serial console socket: %s\nattach with:  socat -,raw,echo=0 unix-connect:%s\n", socket, socket)
	return nil
}

// cmdPin hashes a base image so an author can paste the pin into a config. It needs no
// store and no runtime directory, which is why it runs before the service is wired.
func cmdPin(argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zvr pin <image.qcow2>")
	}
	digest, err := disk.Digest(argv[0])
	if err != nil {
		return err
	}
	fmt.Println(digest)
	return nil
}

// diskDigest hashes a disk the same way the pin check does.
func diskDigest(path string) (string, error) { return disk.Digest(path) }

// loadApp resolves an app by store name or by file path, the same rule zcr uses: an
// argument with a path separator or a .yaml suffix is read directly.
func loadApp(svc app.Service, arg string) (schema.AppConfig, error) {
	if strings.Contains(arg, "/") || strings.HasSuffix(arg, ".yaml") {
		return svc.Store.LoadFileResolved(arg)
	}
	if !svc.Store.Exists(arg) {
		return schema.AppConfig{}, fmt.Errorf("no app %q defined", arg)
	}
	// Resolved: the runner acts on what an app IS, not on the part of it written in its own
	// file.
	return svc.Store.LoadResolved(arg)
}

// splitFlags separates the single positional argument from the --flags around it.
func splitFlags(argv []string) (string, map[string]bool) {
	flags := map[string]bool{}
	name := ""
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--") {
			flags[arg] = true
			continue
		}
		if name == "" {
			name = arg
		}
	}
	return name, flags
}

// versionString returns the ldflags-stamped version, falling back to the module version
// recorded in the build info.
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return version
	}
	return version
}
