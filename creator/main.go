// Command zc is Zinc's app-definition tool (docs/architecture.md section 9.1).
//
// It authors app files (~/.config/zinc/apps/<name>.yaml) and manages them: create, edit,
// list, validate, delete, and a keyboard-first TUI. Both app kinds are authored here - a
// container app and a VM app are the same file with a different Type - and neither is run
// here: to RUN what it authors zc shells out to the runtime that owns the app, `zcr` for
// containers and `zvr` for VMs. So zc imports neither runner and knows nothing about podman
// or qemu; they meet only at the on-disk format and at that process boundary. The relevant
// runtime must be on $PATH for the run/manage commands; authoring works without it.
//
//	zc tui                             keyboard-first manager (create/edit/run/stop/logs)
//	zc new <name> --image <img> [--desc d] [--icon i] [--entrypoint cmd] [--tunnel wg.conf]
//	                              [--dbus-talk a.b.C,...] [--dbus-own a.b.C,...]
//	zc list
//	zc validate <name|app.yaml>
//	zc delete <name>
//	zc keys list|show|set <s>|edit|validate|path   TUI keybind schemes
//	zc compose export <name> [-o f]    describe an app as a Compose-specification file
//	zc compose import <compose.yaml>   author app definitions from one
//	zc run <name|app.yaml> [--exec]    ⟶ zcr run
//	zc build <name|app.yaml>           ⟶ zcr build
//	zc stop|restart|inspect <name>     ⟶ zcr
//	zc logs <name> [-f]                ⟶ zcr logs
//	zc term <name> [--shell]           ⟶ zcr term
//	zc image search <term>|resolve <ref>   ⟶ zcr image
//
// A bare <name> resolves against the store (~/.config/zinc/apps); an argument that looks
// like a path (contains "/" or ends in ".yaml") is read directly.
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/common/domain/schema/validate"
	"github.com/crispuscrew/zinc/common/domain/schema/wgconf"
	"github.com/crispuscrew/zinc/creator/internal/backend"
	"github.com/crispuscrew/zinc/creator/internal/keys"
	"github.com/crispuscrew/zinc/creator/internal/runner"
	"github.com/crispuscrew/zinc/creator/internal/store"
	"github.com/crispuscrew/zinc/creator/internal/tui"
)

const usage = `usage: zc <command> [args]

  tui                               keyboard-first manager (create/edit/run/stop/logs)
  new <name> --image <img> [--desc d] [--icon i] [--entrypoint cmd] [--tunnel wg.conf]
             [--dbus-talk a.b.C,...] [--dbus-own a.b.C,...]
  new <name> --vm --image <base.qcow2> --base-digest sha256:... [--memory MiB]
             [--vcpus N] [--disk GiB] [--display None|Window|Accelerated|Compatible]
             [--ci-user u] [--ci-ssh-key k.pub] [--forward HOST:GUEST] [--install 'a; b']
             [--firmware UEFI] [--secure-boot] [--tpm] [--devices Compatible] [--vulkan]
             [--resolution WxH] [--mac ADDR] [--media disc.iso]
  init [--force]                    seed the store with example apps to start from
  list
  validate <name|app.yaml> [--resolved]   --resolved prints what an inheriting app merges to
  delete <name>
  keys list|show|set <s>|edit|validate|path   TUI keybind schemes (default|vim|custom)
  compose export <name> [-o f]      describe an app as a compose file (lossy; prints what)
  compose import <compose.yaml>     author apps from a compose file (fail-closed; lossy)
  run <name|app.yaml> [--exec]      build the launch plan; print it, or launch    (⟶ zcr)
  build <name|app.yaml>             (re)build the derived image (ImageMeta.Install) (⟶ zcr)
  stop|restart|inspect <name>       (⟶ zcr)
  logs <name> [-f]                  (⟶ zcr)
  term <name> [--shell]             open a terminal for a multiterminal app        (⟶ zcr)
  image search <term>|resolve <ref> find/pin an image                             (⟶ zcr)
  version                           print the version

Runtime commands go to whichever runtime owns the app: zcr for container apps, zvr for
VM apps. zc authors both and runs neither.`

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
		fmt.Fprintln(os.Stderr, "zc: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) < 1 {
		return fmt.Errorf("%s", usage)
	}
	cmd, rest := argv[0], argv[1:]

	if cmd == "version" || cmd == "--version" {
		fmt.Println("zc " + versionString())
		return nil
	}

	// keys is self-contained (zc's own config dir); dispatch it before building the
	// store - it needs neither the store nor the runtime.
	if cmd == "keys" {
		return cmdKeys(rest)
	}

	// `image` is container-only and takes a reference rather than an app name, so it
	// forwards without a store lookup.
	if cmd == "image" {
		return runner.Passthrough(argv...)
	}

	// Authoring commands work on the store locally; no runtime needed.
	sto, err := store.Default()
	if err != nil {
		return err
	}
	svc := backend.New(sto)

	// Runtime commands are delegated to whichever runtime owns the app: zcr for
	// containers, zvr for VMs. zc authors both kinds and runs neither, so it reads the
	// app's Type to decide where the command goes.
	switch cmd {
	case "run", "build", "stop", "restart", "inspect", "logs", "term":
		return delegate(svc, cmd, rest)
	}

	switch cmd {
	case "tui":
		return cmdTUI(svc)
	case "new":
		return cmdNew(svc, rest)
	case "init":
		return cmdInit(svc, rest)
	case "list":
		return cmdList(svc)
	case "validate":
		return cmdValidate(svc, rest)
	case "delete":
		return cmdDelete(svc, rest)
	case "compose":
		return cmdCompose(svc, rest)
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
}

// seedApps are the examples `zc init` writes: deliberately few and boring, each demonstrating one
// thing the schema can do. The images are alpine, pinned, because an example that pulls a browser
// on first run is a several-hundred-megabyte surprise.
var seedApps = []struct {
	name string
	yaml string
}{
	{"example-shell", `SchemaVersion: 3
Type: ZincContainer
AppNameID: example-shell
Description: A terminal in a container - the smallest thing that runs
Group: examples
ImageMeta:
  Image: docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
StartConditions:
  Entrypoint: /bin/sh
  Terminal: true
`},
	{"example-egress", `SchemaVersion: 3
Type: ZincContainer
AppNameID: example-egress
Description: Network locked down to one destination - everything else is dropped
Group: examples
ImageMeta:
  Image: docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
StartConditions:
  Entrypoint: /bin/sh
  Terminal: true
# Without NetworkLists an app reaches only its own localhost. With one, it reaches
# exactly what is listed and nothing else - the rules are applied before the app starts.
NetworkMeta:
  NetworkLists:
    - Domains: ["example.com"]
      Ports: [443]
`},
	{"example-instanced", `SchemaVersion: 3
Type: ZincContainer
AppNameID: example-instanced
Description: One definition, many instances - run it as example-instanced@work
Group: examples
ImageMeta:
  Image: docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
StartConditions:
  Entrypoint: /bin/sh
  Terminal: true
# {state} expands to this instance's own directory. Ask zcr where it is:
#   zcr where example-instanced@work
Volumes:
  - HostMounted: true
    HostMount: "{state}/data"
    InnerMount: /data
    Writable: true
`},
}

// cmdInit seeds the store, so a fresh machine answers "and now what" with files rather than
// documentation. It refuses to overwrite by default and reports what it skipped: this is a command
// someone runs twice without thinking.
func cmdInit(svc backend.Service, argv []string) error {
	force := false
	for _, arg := range argv {
		switch arg {
		case "--force":
			force = true
		default:
			return fmt.Errorf("unknown flag %q (usage: zc init [--force])", arg)
		}
	}

	var written, skipped []string
	for _, seed := range seedApps {
		if svc.Exists(seed.name) && !force {
			skipped = append(skipped, seed.name)
			continue
		}
		// Parsed through the same path a hand-written file takes, rather than constructed as
		// a struct: a seed that only ever existed as Go could drift from what the YAML codec
		// actually accepts, and then the examples would be the one set of files guaranteed
		// not to demonstrate the real format.
		draft, err := os.CreateTemp("", "zinc-seed-*.yaml")
		if err != nil {
			return fmt.Errorf("seed %s: %w", seed.name, err)
		}
		tmpPath := draft.Name()
		_, werr := draft.WriteString(seed.yaml)
		cerr := draft.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("seed %s: %w", seed.name, errors.Join(werr, cerr))
		}
		cfg, lerr := svc.LoadFile(tmpPath)
		os.Remove(tmpPath)
		if lerr != nil {
			return fmt.Errorf("seed %s: %w", seed.name, lerr)
		}
		if err := svc.Save(cfg); err != nil { // validates before anything reaches the store
			return fmt.Errorf("seed %s: %w", seed.name, err)
		}
		written = append(written, seed.name)
	}

	for _, name := range written {
		fmt.Printf("created %s → %s\n", name, svc.Path(name))
	}
	if len(skipped) > 0 {
		fmt.Printf("kept existing: %s (use --force to replace)\n", strings.Join(skipped, ", "))
	}
	if len(written) > 0 {
		fmt.Println("\ntry:  zc validate example-shell   then   zc run example-shell --exec")
	}
	return nil
}

func cmdTUI(svc backend.Service) error {
	_, err := tea.NewProgram(tui.New(svc, loadKeys()), tea.WithAltScreen()).Run()
	return err
}

// loadKeys resolves the active TUI keybind scheme. A missing or broken scheme must never
// stop the TUI from starting, so any error falls back to the default (today's bindings)
// with a warning on stderr.
func loadKeys() keys.Active {
	if kst, err := keys.DefaultStore(); err == nil {
		if active, lerr := kst.Load(); lerr == nil {
			return active
		} else {
			fmt.Fprintln(os.Stderr, "zc: keybinds: "+lerr.Error()+" - using default")
		}
	} else {
		fmt.Fprintln(os.Stderr, "zc: keybinds: "+err.Error()+" - using default")
	}
	return keys.Active{Name: "default", Scheme: keys.Default}
}

func cmdNew(svc backend.Service, argv []string) error {
	// The name is the first argument; flags follow it (Go's flag parser stops at the
	// first positional, so "new <name> --image ..." must split this way).
	if len(argv) < 1 || strings.HasPrefix(argv[0], "-") {
		return fmt.Errorf("%s", newUsage)
	}
	name, flags := argv[0], argv[1:]

	fset := flag.NewFlagSet("new", flag.ContinueOnError)
	image := fset.String("image", "", "container image (digest-pinned for third-party; section 5.5), or a VM base disk path with --vm")
	desc := fset.String("desc", "", "human-readable description")
	icon := fset.String("icon", "", "icon name")
	isVM := fset.Bool("vm", false, "author a VM app (Type: ZincVirtualization) instead of a container")
	digest := fset.String("base-digest", "", "VM only: sha256:<64 hex> the base disk must hash to (zvr pin <image> prints it)")
	memory := fset.Int64("memory", 4096, "VM only: guest RAM in MiB")
	vcpus := fset.Int("vcpus", 2, "VM only: guest CPU count")
	diskSize := fset.Int64("disk", 0, "VM only: overlay size in GiB (0 keeps the base image's size)")
	display := fset.String("display", string(schema.VMDisplayAccelerated),
		"VM only: None (headless), Window (no 3D), Accelerated (virtio-gpu-gl), or Compatible (plain VGA, for a guest with no virtio-gpu driver)")
	ciUser := fset.String("ci-user", "", "VM only: account cloud-init creates in the guest")
	ciKey := fset.String("ci-ssh-key", "", "VM only: path to a PUBLIC key authorised for that account")
	forward := fset.String("forward", "", "VM only: publish a guest port as HOST:GUEST (e.g. 2222:22), repeatable with commas")
	firmware := fset.String("firmware", "", "VM only: BIOS or UEFI (Windows 11 requires UEFI)")
	secureBoot := fset.Bool("secure-boot", false, "VM only: enable UEFI Secure Boot")
	tpmFlag := fset.Bool("tpm", false, "VM only: attach an emulated TPM 2.0 (Windows 11 requires one)")
	devices := fset.String("devices", "", "VM only: Virtio (default) or Compatible (for guests without virtio drivers, e.g. Windows)")
	vulkan := fset.Bool("vulkan", false, "VM only: pass guest Vulkan through to the host GPU (needs a venus-capable virglrenderer; disables qemu's sandbox for this app)")
	resolution := fset.String("resolution", "", "VM only: fixed guest screen size as WxH (e.g. 1920x1080); for --display Compatible, whose guest has no driver to resize itself")
	mac := fset.String("mac", "", "VM only: guest NIC address, or \"random\"; the default is derived per-app under QEMU's 52:54:00 prefix, so set this to present something that names no vendor")
	media := fset.String("media", "", "VM only: ISO attached read-only as a CD-ROM on every run (e.g. a virtio-win driver disc), repeatable with commas")
	entrypoint := fset.String("entrypoint", "", "the process to run; empty uses the image's default command (which for many images exits immediately)")
	tunnelConf := fset.String("tunnel", "", "container only: path to a wg-quick config; Zinc builds the WireGuard interface for the app (the app itself never gets NET_ADMIN)")
	install := fset.String("install", "", "setup steps, ';'-separated: a container's derived-image RUN layer, or a guest's cloud-init runcmd")
	dbusTalk := fset.String("dbus-talk", "", "container only: D-Bus names the app may call, comma-separated (e.g. org.freedesktop.portal.Desktop); without this the app gets no session bus at all")
	dbusOwn := fset.String("dbus-own", "", "container only: D-Bus names the app may claim, comma-separated (e.g. org.mpris.MediaPlayer2.notes)")
	if err := fset.Parse(flags); err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q (flags must follow the name)", fset.Arg(0))
	}

	// Seed a minimal definition; validate.Validate (via Save) enforces the rest - the image
	// policy for a container, the pin and sizing for a VM. The user fleshes it out with
	// `zc tui` or a hand edit.
	cfg := schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     name,
		Description:   *desc,
		Icon:          *icon,
	}
	cfg.ImageMeta.Image = *image
	cfg.StartConditions.Entrypoint = strings.TrimSpace(*entrypoint)
	// ';' rather than ',' because these are shell lines and commas are ordinary in them.
	for _, step := range strings.Split(*install, ";") {
		if trimmed := strings.TrimSpace(step); trimmed != "" {
			cfg.ImageMeta.Install = append(cfg.ImageMeta.Install, trimmed)
		}
	}
	if *tunnelConf != "" {
		if *isVM {
			return fmt.Errorf("--tunnel is container-only: a guest brings up its own interfaces")
		}
		if err := seedTunnel(&cfg, *tunnelConf); err != nil {
			return err
		}
	}
	keepIDImplied := false
	if *dbusTalk != "" || *dbusOwn != "" {
		if *isVM {
			return fmt.Errorf("--dbus-talk/--dbus-own are container-only: a guest cannot take a bind-mounted bus socket")
		}
		cfg.DBusMeta = schema.DBusMeta{Talk: splitList(*dbusTalk), Own: splitList(*dbusOwn)}
		// A filtered bus is a uid agreement with the proxy and validation refuses the pair without it, so
		// authoring one without KeepUserID would only ever produce a config that will not save. Set it here
		// and write it into the file, where it is reviewable.
		cfg.InternalUserMeta.KeepUserID = true
		keepIDImplied = true
	}
	if *isVM {
		cfg.Type = schema.ZincVirtualization
		forwards, ferr := parseForwards(*forward)
		if ferr != nil {
			return ferr
		}
		width, height, rerr := parseResolution(*resolution)
		if rerr != nil {
			return rerr
		}
		macAddress, merr := resolveMac(*mac)
		if merr != nil {
			return merr
		}
		cfg.VirtualizationMeta = schema.VirtualizationMeta{
			BaseDigest:    *digest,
			MemoryMiB:     *memory,
			VCPUs:         *vcpus,
			DiskSizeGiB:   *diskSize,
			Display:       schema.VMDisplay(*display),
			DisplayWidth:  width,
			DisplayHeight: height,
			MacAddress:    macAddress,
			Vulkan:        *vulkan,
			Firmware:      schema.VMFirmware(*firmware),
			SecureBoot:    *secureBoot,
			TPM:           *tpmFlag,
			Devices:       schema.VMDevices(*devices),
			InstallMedia:  splitList(*media),
			ForwardPorts:  forwards,
			CloudInit:     schema.CloudInit{UserName: *ciUser, SSHKeyPath: *ciKey},
		}
	} else if vmFlagUsed(fset) {
		// Silently ignoring these would write a container app that looks configured for a
		// guest, which is the same trap the cross-type validation exists to close.
		return fmt.Errorf("VM flags were given without --vm; add --vm to author a VM app")
	}

	if svc.Exists(cfg.AppNameID) {
		return fmt.Errorf("app %q already exists at %s", cfg.AppNameID, svc.Path(cfg.AppNameID))
	}
	if err := svc.Save(cfg); err != nil { // validates first (image policy, schema, ...)
		return err
	}
	fmt.Printf("created %s → %s\n", cfg.AppNameID, svc.Path(cfg.AppNameID))
	if keepIDImplied {
		fmt.Println("note: set InternalUserMeta.KeepUserID - a filtered bus needs the app's uid to match the proxy's")
	}
	// Same advisories `zc validate` prints. Authoring is when a valid-but-surprising choice
	// is cheapest to change: the alternative is meeting it as a black screen or an open port
	// at the first launch, with nothing on screen connecting the two.
	for _, warn := range validate.Warnings(cfg) {
		fmt.Println("warning: " + warn)
	}
	return nil
}

// seedTunnel points the app at a wg-quick config AND authors the egress rule the handshake needs.
// The tunnel is built inside a default-drop namespace, so without a rule permitting UDP to the
// peer's endpoint the handshake never leaves. The endpoint is in the file already.
func seedTunnel(cfg *schema.AppConfig, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("--tunnel: %w", err)
	}
	conf, err := wgconf.Parse(string(data))
	if err != nil {
		return fmt.Errorf("--tunnel %s: %w", path, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("--tunnel: %w", err)
	}
	cfg.NetworkMeta.Tunnel = schema.TunnelMeta{WireGuardConf: absolute}
	for _, endpoint := range conf.Endpoints {
		list := schema.NetworkList{Ports: []int{endpoint.Port}}
		if strings.Contains(endpoint.Host, ":") {
			list.IPv6CIDR = []string{endpoint.Host + "/128"}
		} else {
			list.IPv4CIDR = []string{endpoint.Host + "/32"}
		}
		cfg.NetworkMeta.NetworkLists = append(cfg.NetworkMeta.NetworkLists, list)
	}
	return nil
}

const newUsage = `usage:
  zc new <name> --image <img> [--desc d] [--icon i] [--dbus-talk a.b.C] [--dbus-own a.b.C]
  zc new <name> --vm --image <base.qcow2> --base-digest sha256:... \
                [--memory MiB] [--vcpus N] [--disk GiB] [--display None|Window|Accelerated]`

// splitList reads a comma-separated flag into the entries it names, dropping the empty ones
// so a trailing comma is not a path. What each entry has to be is validation's business.
func splitList(spec string) []string {
	var entries []string
	for _, entry := range strings.Split(spec, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// parseForwards reads "HOST:GUEST[,HOST:GUEST...]" into port forwards. Validation screens
// the numbers themselves (range, and the privileged ports a rootless qemu cannot bind);
// this only has to turn the text into fields.
func parseForwards(spec string) ([]schema.PortForward, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var forwards []schema.PortForward
	for _, pair := range strings.Split(spec, ",") {
		hostText, guestText, found := strings.Cut(strings.TrimSpace(pair), ":")
		if !found {
			return nil, fmt.Errorf("--forward %q: want HOST:GUEST, e.g. 2222:22", pair)
		}
		host, herr := strconv.Atoi(hostText)
		guest, gerr := strconv.Atoi(guestText)
		if herr != nil || gerr != nil {
			return nil, fmt.Errorf("--forward %q: both ports must be numbers", pair)
		}
		forwards = append(forwards, schema.PortForward{HostPort: host, GuestPort: guest})
	}
	return forwards, nil
}

// vmFlagUsed reports whether any VM-only flag was set. Go's flag package records which
// flags were actually passed, which is what distinguishes "left at its default" from
// "explicitly set to the default".
func vmFlagUsed(fset *flag.FlagSet) bool {
	used := false
	fset.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "base-digest", "memory", "vcpus", "disk", "display", "ci-user", "ci-ssh-key", "forward", "vulkan", "firmware", "secure-boot", "tpm", "devices", "resolution", "mac", "media":
			used = true
		}
	})
	return used
}

func cmdList(svc backend.Service) error {
	names, err := svc.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("no apps defined yet - create one with: zc new <name> --image <img>")
		return nil
	}
	for _, name := range names {
		cfg, err := svc.LoadResolved(name) // an inheriting app lists what it resolves to
		if err != nil {
			fmt.Printf("%-20s (error: %v)\n", name, err)
			continue
		}
		fmt.Printf("%-20s %-4s %-10s %s\n", name, kindLabel(cfg), traitLabel(cfg), cfg.ImageMeta.Image)
	}
	return nil
}

func cmdValidate(svc backend.Service, argv []string) error {
	if len(argv) < 1 || len(argv) > 2 {
		return fmt.Errorf("usage: zc validate <name|app.yaml> [--resolved]")
	}
	showResolved := len(argv) == 2 && argv[1] == "--resolved"
	if len(argv) == 2 && !showResolved {
		return fmt.Errorf("unknown flag %q\nusage: zc validate <name|app.yaml> [--resolved]", argv[1])
	}
	cfg, err := loadApp(svc, argv[0]) // resolved: an app is judged as what it actually is
	if err != nil {
		return err
	}
	// An app that inherits cannot be audited by reading its own file, which is the cost of
	// resolving live. Printing what it merges to is how that cost is paid back.
	if showResolved {
		data, merr := svc.Marshal(cfg)
		if merr != nil {
			return merr
		}
		fmt.Printf("# %s as it resolves\n%s\n", cfg.AppNameID, data)
	}
	if verr := validate.Validate(cfg); verr != nil {
		return fmt.Errorf("invalid config %s:\n%w", argv[0], verr)
	}
	if cfg.Type == schema.ZincVirtualization {
		virt := cfg.VirtualizationMeta
		fmt.Printf("ok: %s - VM, base=%s %d MiB %d vCPU display=%s\n",
			cfg.AppNameID, cfg.ImageMeta.Image, virt.MemoryMiB, virt.VCPUs, virt.Display)
	} else {
		fmt.Printf("ok: %s - image=%s network=%s\n", cfg.AppNameID, cfg.ImageMeta.Image, netLabel(cfg))
	}
	if base := strings.TrimSpace(cfg.Inherits); base != "" && !showResolved {
		fmt.Printf("      inherits from %s - `zc validate %s --resolved` prints what it merges to\n", base, cfg.AppNameID)
	}
	for _, warn := range validate.Warnings(cfg) {
		fmt.Println("warning: " + warn)
	}
	return nil
}

func cmdDelete(svc backend.Service, argv []string) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: zc delete <name>")
	}
	if !svc.Exists(argv[0]) {
		return fmt.Errorf("no app %q defined", argv[0])
	}
	if err := svc.Delete(argv[0]); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", argv[0])
	return nil
}

// cmdKeys manages zc's TUI keybind schemes (section 9.1): list the available schemes, show a
// scheme's effective bindings, set the active one, edit/scaffold a custom scheme,
// validate, or print the config dir. These are zc's own UI keys - not the desktop
// hotkeys (section 12).
func cmdKeys(argv []string) error {
	kst, err := keys.DefaultStore()
	if err != nil {
		return err
	}
	sub := "list"
	if len(argv) > 0 {
		sub = argv[0]
	}
	switch sub {
	case "list":
		active, _ := kst.Load()
		names, err := kst.List()
		if err != nil {
			return err
		}
		for _, name := range names {
			mark := "  "
			if name == active.Name {
				mark = "* "
			}
			kind := "custom"
			if keys.IsBuiltin(name) {
				kind = "built-in"
			}
			fmt.Printf("%s%-20s %s\n", mark, name, kind)
		}
		return nil
	case "show":
		if len(argv) > 1 {
			scheme, err := kst.Resolve(argv[1])
			if err != nil {
				return err
			}
			return printScheme(argv[1], scheme)
		}
		active, err := kst.Load()
		if err != nil {
			return err
		}
		return printScheme(active.Name, active.Scheme)
	case "set":
		if len(argv) != 2 {
			return fmt.Errorf("usage: zc keys set <scheme>")
		}
		if err := kst.SetActive(argv[1]); err != nil {
			return err
		}
		fmt.Printf("active keybind scheme: %s\n", argv[1])
		return nil
	case "edit":
		name := "default"
		if len(argv) > 1 {
			name = argv[1]
		}
		scheme, path, err := kst.EnsureEditable(name)
		if err != nil {
			return err
		}
		if err := openInEditor(path); err != nil {
			return err
		}
		if verr := kst.Validate(scheme); verr != nil {
			return fmt.Errorf("scheme %q has problems:\n%w", scheme, verr)
		}
		fmt.Printf("saved scheme %q (%s)\n  activate it with: zc keys set %s\n", scheme, path, scheme)
		return nil
	case "validate":
		if len(argv) > 1 {
			if err := kst.Validate(argv[1]); err != nil {
				return err
			}
			fmt.Printf("ok: scheme %q is valid\n", argv[1])
			return nil
		}
		active, err := kst.Load() // Load resolves + validates the active scheme
		if err != nil {
			return err
		}
		fmt.Printf("ok: active scheme %q is valid\n", active.Name)
		return nil
	case "path":
		fmt.Println(kst.Dir)
		return nil
	default:
		return fmt.Errorf("unknown keys subcommand %q (want list|show|set|edit|validate|path)", sub)
	}
}

// printScheme prints a scheme's bindings grouped by screen, in action order.
func printScheme(name string, scheme keys.Scheme) error {
	fmt.Printf("scheme %q\n", name)
	for _, ctx := range keys.Contexts {
		fmt.Printf("  [%s]\n", keys.ContextName[ctx])
		for _, act := range keys.ActionsByContext[ctx] {
			if hint := scheme.Hint(ctx, act); hint != "" {
				fmt.Printf("    %-14s %s\n", act, hint)
			}
		}
	}
	return nil
}

// openInEditor opens path in $EDITOR (default vim) with the host's stdio.
func openInEditor(path string) error {
	argv := strings.Fields(os.Getenv("EDITOR"))
	if len(argv) == 0 {
		argv = []string{"vim"}
	}
	argv = append(argv, path)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// loadApp resolves an app by store name or file path, and returns the RESOLVED app: every caller
// here asks what an app IS. The editing paths use svc.Load, which returns the file as written.
func loadApp(svc backend.Service, arg string) (schema.AppConfig, error) {
	if strings.Contains(arg, "/") || strings.HasSuffix(arg, ".yaml") {
		return svc.LoadFileResolved(arg)
	}
	if !svc.Exists(arg) {
		return schema.AppConfig{}, fmt.Errorf("no app %q defined (try: zc list)", arg)
	}
	return svc.LoadResolved(arg)
}

// netLabel summarizes an app's network posture for the list/validate output: "isolated"
// when it has no NetworkLists (own localhost only), else the number of lists it carries.
func netLabel(cfg schema.AppConfig) string {
	if n := len(cfg.NetworkMeta.NetworkLists); n > 0 {
		return fmt.Sprintf("net:%d", n)
	}
	return "isolated"
}

// kindLabel marks which runtime owns an app. One store holds both kinds, so a listing
// that did not say which is which would leave the reader guessing why some apps answer
// `zc build` and others do not.
func kindLabel(cfg schema.AppConfig) string {
	if cfg.Type == schema.ZincVirtualization {
		return "vm"
	}
	return "ctr"
}

// traitLabel is the one-word summary that matters for each kind: a container's network
// posture, a guest's sizing. Describing a VM as "isolated" would be borrowing container
// vocabulary for a boundary that is enforced somewhere else entirely.
func traitLabel(cfg schema.AppConfig) string {
	if cfg.Type == schema.ZincVirtualization {
		return fmt.Sprintf("%dM/%dcpu", cfg.VirtualizationMeta.MemoryMiB, cfg.VirtualizationMeta.VCPUs)
	}
	return netLabel(cfg)
}

// parseResolution reads a "WxH" screen size. It is one flag rather than two because a width
// without a height is not a screen, and a single value cannot be given by accident.
func parseResolution(spec string) (int, int, error) {
	if strings.TrimSpace(spec) == "" {
		return 0, 0, nil
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(spec)), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--resolution %q: want WxH, e.g. 1920x1080", spec)
	}
	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("--resolution %q: width %q is not a number", spec, parts[0])
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("--resolution %q: height %q is not a number", spec, parts[1])
	}
	return width, height, nil
}

// resolveMac turns --mac into the address to store. "random" is drawn once, here, and the literal
// result written into the config: a config that said "random" would draw a new address every run,
// and a guest whose NIC changes loses its DHCP lease and looks like swapped hardware to Windows.
// The address is locally administered and unicast, so it belongs to no vendor.
func resolveMac(flag string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(flag), "random") {
		return flag, nil
	}
	var octets [6]byte
	if _, err := rand.Read(octets[:]); err != nil {
		return "", fmt.Errorf("draw a random MAC address: %w", err)
	}
	octets[0] = (octets[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		octets[0], octets[1], octets[2], octets[3], octets[4], octets[5]), nil
}
