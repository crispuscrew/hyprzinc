package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/virtualization/runner/adapters/firmware"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/paths"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/qemu"
)

// `zvr install` produces a base disk by running an OS installer, for guests that have no
// cloud image to start from - Windows above all.
//
// It deliberately takes flags rather than an app name. An app config pins its base image by
// digest, and a disk that does not exist yet has no digest, so requiring one here would be
// a chicken-and-egg: you could not author the app until the disk existed, and could not
// create the disk without the app. Install first, pin the result, then author the app
// against it. That also keeps the rule that a pinned base is never written to intact -
// installation is how the base comes into being, not something done to a base that already
// has one.
const installUsage = `usage: zvr install --disk PATH --media ISO [--media ISO]... [options]

  --disk PATH        the disk to install onto; created if missing
  --size GiB         size of a newly created disk (default 64)
  --media ISO        installer image, repeatable - Windows also needs the virtio-win ISO
                     if you intend to switch it to virtio devices afterwards
  --memory MiB       guest RAM during the install (default 4096)
  --vcpus N          guest CPUs during the install (default 4)
  --firmware F       BIOS or UEFI (default UEFI; Windows 11 requires UEFI)
  --secure-boot      enable UEFI Secure Boot (Windows 11 expects it)
  --tpm              attach an emulated TPM 2.0 (Windows 11 requires one)
  --devices D        Virtio or Compatible (default Compatible, which is what an
                     installer without virtio drivers can actually see)
  --resolution WxH   fixed guest screen size, e.g. 1920x1080. A guest with no display
                     driver keeps whatever the firmware gave it, which is 1280x800
                     unless this says otherwise. Needs UEFI.`

type mediaList []string

func (list *mediaList) String() string { return strings.Join(*list, ",") }
func (list *mediaList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

// checkQemuPath screens a path that becomes a qemu -drive property.
//
// `zvr install` builds its config by hand and never calls validate.Validate, so the rules
// the validator applies to VirtualizationMeta.Image and InstallMedia do not reach these
// flags. That gap matters more here than in a config, not less: install is the one path that
// BOOTS from the medium, and it writes to --disk directly.
//
// A comma is the whole problem. qemu separates -drive properties with commas and resolves a
// duplicate key to the LAST one, so a path containing ",file=/elsewhere" does not stay a
// path: it appends a second file= that replaces the one just approved. A directory named
// "Win11.iso,file=/home/u/.ssh" inside an unpacked "install kit" is enough, since the Stat
// above succeeds on the literal string. The basename also lands in the QMP and serial socket
// paths, where the tail after a comma is parsed as chardev options.
func checkQemuPath(flagName, path string) error {
	switch {
	case strings.ContainsAny(path, ",:"):
		return fmt.Errorf("%s %q: must not contain ',' or ':' - qemu separates -drive properties with commas and resolves a duplicate key to the last one, so those characters append options to the drive rather than staying in the path", flagName, path)
	case strings.ContainsAny(path, " \t\n\r"), strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return fmt.Errorf("%s %q: must be a single-line path with no whitespace or control characters", flagName, path)
	}
	return nil
}

func cmdInstall(argv []string) error {
	fset := flag.NewFlagSet("install", flag.ContinueOnError)
	disk := fset.String("disk", "", "the disk to install onto; created if missing")
	size := fset.Int64("size", 64, "size in GiB of a newly created disk")
	memory := fset.Int64("memory", 4096, "guest RAM in MiB during the install")
	vcpus := fset.Int("vcpus", 4, "guest CPUs during the install")
	firmwareKind := fset.String("firmware", string(schema.VMFirmwareUEFI), "BIOS or UEFI")
	secureBoot := fset.Bool("secure-boot", false, "enable UEFI Secure Boot")
	tpm := fset.Bool("tpm", false, "attach an emulated TPM 2.0")
	devices := fset.String("devices", string(schema.VMDevicesCompatible), "Virtio or Compatible")
	resolution := fset.String("resolution", "", "fixed guest screen size as WxH, e.g. 1920x1080 (UEFI only)")
	var media mediaList
	fset.Var(&media, "media", "installer ISO (repeatable)")
	if err := fset.Parse(argv); err != nil {
		return err
	}
	if *disk == "" || len(media) == 0 {
		return fmt.Errorf("%s", installUsage)
	}
	width, height, err := parseResolution(*resolution)
	if err != nil {
		return err
	}
	// `zvr install` builds its config by hand rather than loading a saved app, so nothing
	// has validated this pairing. The device that carries a fixed size has no
	// BIOS-compatible mode, so under BIOS the flag would be quietly dropped - and a flag
	// that looks accepted and changes nothing is the trap this project refuses elsewhere.
	if width > 0 {
		if schema.VMFirmware(*firmwareKind) != schema.VMFirmwareUEFI {
			return fmt.Errorf("--resolution needs --firmware %s: the display device that carries a fixed size has no BIOS-compatible mode",
				schema.VMFirmwareUEFI)
		}
		if _, ok := schema.GuestDisplay(width, height); !ok {
			return fmt.Errorf("--resolution %dx%d: too large for the guest's display to describe "+
				"(neither side may exceed %d, and the total is bounded by the EDID pixel clock); "+
				"3840x2160 works, 4096x2160 does not", width, height, schema.GuestDisplayMaxPixels)
		}
	}

	// The enums are read straight into the config below, and an unrecognised value is not an
	// error anywhere downstream: firmware.Prepare treats anything that is not UEFI as "no
	// firmware", and the machine builder treats anything that is not Compatible as virtio.
	// So `--firmware uefi --secure-boot` (lowercase, the natural typing) would produce a
	// SeaBIOS machine with no Secure Boot, while still emitting the smm and pflash options
	// that make the command line look like Secure Boot. In a config these are hard errors;
	// they have to be hard errors here too, for the same reason --resolution is checked
	// above: a flag that looks accepted and changes nothing is the trap this project
	// refuses elsewhere.
	switch schema.VMFirmware(*firmwareKind) {
	case schema.VMFirmwareBIOS, schema.VMFirmwareUEFI:
	default:
		return fmt.Errorf("--firmware %q: must be %s or %s (the value is case-sensitive)",
			*firmwareKind, schema.VMFirmwareBIOS, schema.VMFirmwareUEFI)
	}
	switch schema.VMDevices(*devices) {
	case schema.VMDevicesVirtio, schema.VMDevicesCompatible:
	default:
		return fmt.Errorf("--devices %q: must be %s or %s (the value is case-sensitive)",
			*devices, schema.VMDevicesVirtio, schema.VMDevicesCompatible)
	}
	if *secureBoot && schema.VMFirmware(*firmwareKind) != schema.VMFirmwareUEFI {
		return fmt.Errorf("--secure-boot needs --firmware %s: Secure Boot is a property of the UEFI variable store, and a BIOS machine has none",
			schema.VMFirmwareUEFI)
	}

	diskPath, err := filepath.Abs(*disk)
	if err != nil {
		return err
	}
	if err := checkQemuPath("--disk", diskPath); err != nil {
		return err
	}
	for index, path := range media {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := checkQemuPath("--media", absolute); err != nil {
			return err
		}
		if _, err := os.Stat(absolute); err != nil {
			return fmt.Errorf("install media %s: %w", absolute, err)
		}
		media[index] = absolute
	}

	if _, err := os.Stat(diskPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
			return err
		}
		create := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, strconv.FormatInt(*size, 10)+"G")
		if output, err := create.CombinedOutput(); err != nil {
			return fmt.Errorf("create %s: %w: %s", diskPath, err, strings.TrimSpace(string(output)))
		}
		fmt.Printf("created %s (%d GiB, grows as it is used)\n", diskPath, *size)
	} else {
		// Refuse to silently install over something: this writes to the disk directly.
		fmt.Printf("installing onto the existing disk %s\n", diskPath)
	}

	cfg := schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincVirtualization,
		AppNameID:     "install",
		VirtualizationMeta: schema.VirtualizationMeta{
			MemoryMiB:     *memory,
			VCPUs:         *vcpus,
			Display:       schema.VMDisplayCompatible,
			DisplayWidth:  width,
			DisplayHeight: height,
			Firmware:      schema.VMFirmware(*firmwareKind),
			SecureBoot:    *secureBoot,
			TPM:           *tpm,
			Devices:       schema.VMDevices(*devices),
			InstallMedia:  media,
		},
	}

	layout, cleanup, err := installLayout(cfg, diskPath)
	if err != nil {
		return err
	}
	defer cleanup()

	args := qemu.Args(cfg, layout)
	fmt.Println("starting the installer; the guest's window is qemu's own.")
	fmt.Println("when the install finishes, shut the guest down from inside and this returns.")

	// Foreground, unlike `run`: an install is one interactive sitting, and the command
	// ending when the guest powers off is exactly the signal the next step needs.
	command := exec.Command(args[0], args[1:]...)
	command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := command.Run(); err != nil {
		return fmt.Errorf("the installer exited with an error: %w", err)
	}

	digest, err := installedDigest(diskPath)
	if err != nil {
		return err
	}
	fmt.Printf("\ninstall finished. %s is now a base image.\n\n", diskPath)
	fmt.Println("Author an app against it:")
	fmt.Printf("  zc new <name> --vm --image %s \\\n", diskPath)
	fmt.Printf("      --base-digest %s \\\n", digest)
	fmt.Printf("      --memory %d --vcpus %d --devices %s --display Compatible", *memory, *vcpus, *devices)
	if width > 0 {
		fmt.Printf(" \\\n      --resolution %dx%d", width, height)
	}
	if *firmwareKind == string(schema.VMFirmwareUEFI) {
		fmt.Print(" --firmware UEFI")
	}
	if *secureBoot {
		fmt.Print(" --secure-boot")
	}
	if *tpm {
		fmt.Print(" --tpm")
	}
	fmt.Println()
	fmt.Println("\nEvery run then writes to its own overlay, so `zvr reset` returns the app")
	fmt.Println("to the state it is in right now.")
	return nil
}

// installLayout resolves the host-side pieces for an install: the disk itself stands in for
// the overlay (there is no base to layer over yet), with firmware and TPM state kept beside
// it so a half-finished install can be resumed.
func installLayout(cfg schema.AppConfig, diskPath string) (qemu.Layout, func(), error) {
	layout := paths.Paths{}
	resolved, err := paths.Default()
	if err != nil {
		return qemu.Layout{}, nil, err
	}
	layout = resolved
	if err := layout.EnsureDirs(); err != nil {
		return qemu.Layout{}, nil, err
	}

	// Named after the disk so two installs at once do not collide, and so resuming one
	// finds its own firmware state.
	name := "install-" + strings.TrimSuffix(filepath.Base(diskPath), filepath.Ext(diskPath))
	machine := qemu.Layout{
		Overlay:    diskPath,
		PIDFile:    layout.PIDFile(name),
		QMP:        layout.QMP(name),
		Serial:     layout.Serial(name),
		Installing: true,
		// The disk's own path, not the placeholder app name: it is unique to this host, so
		// the guest gets a machine identity no other install shares. Deterministic, so
		// resuming a half-finished install does not change the hardware under it.
		Identity: diskPath,
	}

	// The install's own variables live beside the disk it is creating, which is where a
	// later app run looks for them.
	prepared, err := firmware.Prepare(cfg.VirtualizationMeta, diskPath+".uefi-vars.fd", "")
	if err != nil {
		return qemu.Layout{}, nil, err
	}
	machine.Firmware = prepared

	cleanup := func() {}
	if cfg.VirtualizationMeta.TPM {
		firmware.StopTPM(layout.TPMSocket(name), layout.TPMPID(name))
		if _, err := firmware.StartTPM(diskPath+".tpm-state", layout.TPMSocket(name), layout.TPMPID(name)); err != nil {
			return qemu.Layout{}, nil, err
		}
		machine.TPMSocket = layout.TPMSocket(name)
		cleanup = func() { firmware.StopTPM(layout.TPMSocket(name), layout.TPMPID(name)) }
	}
	return machine, cleanup, nil
}

// installedDigest hashes the finished disk so the operator can paste the pin straight into
// an app config.
func installedDigest(diskPath string) (string, error) {
	digest, err := diskDigest(diskPath)
	if err != nil {
		return "", fmt.Errorf("hash the installed disk: %w", err)
	}
	return digest, nil
}

// parseResolution reads a "WxH" screen size. One flag rather than two: a width without a
// height is not a screen.
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
