// Package qemu builds the command line for a VM app. Pure - validated config and resolved paths in,
// argv out - which is what lets zvr print the exact command with --dry-run. Started with -nodefaults,
// so a VM's hardware is exactly what the config asked for and never a compiled-in default.
package qemu

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// Binary is the emulator zvr drives. Only x86_64 guests are supported: a foreign
// architecture would run without KVM and be far too slow for the interactive use these
// VMs are for.
const Binary = "qemu-system-x86_64"

// hostMemGiB sizes the host memory window venus shares blob resources through. It is
// address space rather than committed memory, so it is set generously for a guest pushing
// real textures instead of qemu's 256 MiB default.
const hostMemGiB = 8

// Layout is where one app's files live, resolved by the caller so this stays pure.
type Layout struct {
	Overlay string // the app's copy-on-write disk, backed by the pinned base image
	Seed    string // cloud-init seed ISO; empty when cloud-init is disabled
	PIDFile string // qemu writes its pid here, which is how zvr finds it again
	QMP     string // control socket: status queries and a graceful power button
	Serial  string // the guest's serial console, for `zvr console`

	// Firmware is empty for a BIOS guest; a UEFI guest needs OVMF plus its own writable
	// variable store. TPMSocket is empty unless an emulated TPM is attached.
	Firmware  Firmware
	TPMSocket string
	// Installing boots from VirtualizationMeta.InstallMedia rather than the disk, which is
	// how a guest with no cloud image (Windows) gets installed in the first place. The discs
	// themselves are attached on every run, not only this one.
	Installing bool

	// Identity seeds the guest's SMBIOS UUID and MAC instead of the app name. An install has no app yet
	// and runs under a fixed placeholder, so deriving from that name would give every install on every
	// host the same identity - at the one moment it matters most, since OOBE runs here.
	Identity string
}

// Args returns the full argv for cfg. The caller has already validated cfg, so the sizing
// and display mode are known good.
func Args(cfg schema.AppConfig, layout Layout) []string {
	virt := cfg.VirtualizationMeta
	args := []string{
		Binary,
		"-name", cfg.AppNameID,
		// q35 is the modern chipset (PCIe, no legacy baggage); KVM is what makes the guest
		// fast enough to interact with, and -cpu host exposes the real CPU's features so
		// guest code is not held back by a generic model.
		"-machine", machineType(virt),
		"-cpu", "host",
		"-smp", strconv.Itoa(virt.VCPUs),
		"-m", strconv.FormatInt(virt.MemoryMiB, 10) + "M",
		"-nodefaults",
		"-pidfile", layout.PIDFile,
		"-qmp", "unix:" + layout.QMP + ",server=on,wait=off",
		// server=on,wait=off: the socket exists from the start but the guest never waits
		// for anyone to attach, so a VM boots whether or not you are watching its console.
		"-serial", "unix:" + layout.Serial + ",server=on,wait=off",
	}

	identity := cfg.AppNameID
	if layout.Identity != "" {
		identity = layout.Identity
	}
	args = append(args, identityArgs(identity)...)
	args = append(args, secureBootArgs(virt)...)
	args = append(args, rtcArgs(virt.Devices)...)
	args = append(args, sandboxArgs(virt.Vulkan)...)
	args = append(args, firmwareArgs(layout.Firmware)...)
	args = append(args, tpmArgs(layout.TPMSocket)...)
	args = append(args, diskArgs(layout, virt.Devices)...)
	// The discs are attached on every run, not just the install. An OS is only the first
	// thing a guest needs from a CD: the drivers its installer had no room for arrive the
	// same way, and a guest with no network yet has no other way to be handed a file.
	// Read-only, so leaving one in the config costs nothing but a drive letter.
	args = append(args, mediaArgs(virt.InstallMedia)...)
	if layout.Installing {
		// once=d, not order=d: the installer boots from the disc and every reboot after goes to the disk.
		// With the disc permanently first, the installer's own mid-install reboot lands back at "press any
		// key to boot from CD" and starts over.
		args = append(args, "-boot", "once=d,menu=on")
	}
	args = append(args, netArgs(identity, virt)...)
	args = append(args, displayArgs(virt)...)
	args = append(args, audioArgs(cfg.AudioMeta)...)
	return args
}

// sandboxArgs applies qemu's own seccomp jail, since the qemu process is the boundary between a
// guest and this machine.
//
// Guest Vulkan cannot coexist with it: venus runs in a virgl_render_server the sandbox both forbids
// forking (spawn=deny) and kills, silently, with only a generic "virgl could not be initialized" to
// show for it. So a Vulkan app runs qemu unsandboxed and validation warns - the trade is the
// caller's, which is why Vulkan is opt-in.
func sandboxArgs(vulkan bool) []string {
	if vulkan {
		return nil
	}
	return []string{"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"}
}

// diskArgs attaches the app's overlay and, when present, the cloud-init seed. The overlay
// is the only writable disk: the pinned base it is backed by is never opened for writing,
// so the authored image cannot drift from its digest.
func diskArgs(layout Layout, devices schema.VMDevices) []string {
	args := diskArgsFor(devices, layout.Overlay)
	if layout.Seed == "" {
		return args
	}
	// Read-only and raw either way: cloud-init looks for a filesystem labelled cidata, and
	// the guest has no business writing to its own seed.
	if devices == schema.VMDevicesCompatible {
		// On the compatible profile the seed rides the same controller as the disk. A
		// virtio seed would be both unreadable to a guest with no virtio drivers and a
		// piece of virtio hardware on a machine configured precisely because it has none.
		return append(args,
			"-drive", "file="+layout.Seed+",if=none,id=seed,format=raw,readonly=on",
			"-device", "ide-cd,drive=seed,bus=ahci.1")
	}
	return append(args, "-drive", "file="+layout.Seed+",if=virtio,format=raw,readonly=on")
}

// netArgs gives the guest user-mode networking: outbound access through qemu's own NAT,
// with no host interface to attach to and nothing inbound except the forwards asked for.
// Each forward binds 127.0.0.1 rather than every interface, so a forwarded guest port
// reaches the host that started it and not the LAN.
func netArgs(appName string, virt schema.VirtualizationMeta) []string {
	netdev := "user,id=net0"
	for _, forward := range virt.ForwardPorts {
		netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", forward.HostPort, forward.GuestPort)
	}
	return []string{
		"-netdev", netdev,
		"-device", netDeviceFor(virt.Devices) + ",mac=" + macFor(appName, virt.MacAddress),
	}
}

// displayArgs wires up how the VM is seen. Accelerated is the point of the whole design:
// virtio-gpu-gl renders guest 3D on the host GPU and hands the result to the compositor
// as a dmabuf, so frames never leave the machine and never get encoded - the difference
// between a playable game and a slideshow.
func displayArgs(virt schema.VirtualizationMeta) []string {
	mode, vulkan, devices := virt.Display, virt.Vulkan, virt.Devices
	switch mode {
	case schema.VMDisplayCompatible:
		// Plain VGA: no acceleration at all, and the only thing a guest without a virtio-gpu
		// driver can put on screen - which on this hardware means Windows.
		return append(inputArgsFor(devices), "-device", compatibleDisplayDevice(virt), "-display", "gtk")
	case schema.VMDisplayAccelerated:
		// virtio-gpu-gl gives the guest OpenGL through virgl either way. venus adds Vulkan,
		// and needs blob resources plus a host memory window to share buffers through -
		// hostmem reserves address space rather than committing RAM. Measured as the minimal
		// set: neither max_hostmem nor a shared memory-backend was required.
		device := "virtio-gpu-gl-pci"
		if vulkan {
			device += ",venus=on,blob=on,hostmem=" + strconv.Itoa(hostMemGiB) + "G"
		}
		return append(inputArgsFor(devices), "-device", device, "-display", "gtk,gl=on")
	case schema.VMDisplayWindow:
		return append(inputArgsFor(devices), "-device", "virtio-gpu-pci", "-display", "gtk")
	default: // VMDisplayNone
		// No window and no graphics card at all; the serial console is the way in.
		return []string{"-display", "none"}
	}
}

// audioArgs routes guest audio to pipewire when the app asked for it; an app that did not gets no
// sound card at all rather than a silent one.
//
// The microphone is a separate DEVICE, not a setting: hda-duplex has playback and capture streams,
// hda-output only playback. A guest not granted a microphone has no capture endpoint to open, which
// is real enforcement - the promise the container side cannot yet keep. Only the `default` form
// reaches here; validation refuses a device list for a guest.
func audioArgs(audio schema.AudioMeta) []string {
	if audio.Playback.IsZero() && audio.Microphone.IsZero() {
		return nil
	}
	codec := "hda-output"
	if !audio.Microphone.IsZero() {
		codec = "hda-duplex"
	}
	return []string{
		"-audiodev", "pipewire,id=snd0",
		// intel-hda is the device every guest OS already has a driver for, which matters
		// more here than the marginal efficiency of a virtio sound device.
		"-device", "intel-hda",
		"-device", codec + ",audiodev=snd0",
	}
}

// Display renders the argv for a human, lightly quoting anything with whitespace. It is
// what --dry-run prints, so the operator sees the real command rather than a summary.
func Display(args []string) string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			quoted[index] = "'" + arg + "'"
			continue
		}
		quoted[index] = arg
	}
	return strings.Join(quoted, " ")
}
