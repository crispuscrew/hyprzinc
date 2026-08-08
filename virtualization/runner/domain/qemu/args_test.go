package qemu

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

func testLayout() Layout {
	return Layout{
		Overlay: "/home/u/.local/share/zinc/vms/guest.qcow2",
		Seed:    "/home/u/.local/share/zinc/vms/guest-seed.iso",
		PIDFile: "/run/user/1000/zinc/vm/guest.pid",
		QMP:     "/run/user/1000/zinc/vm/guest.qmp",
		Serial:  "/run/user/1000/zinc/vm/guest.serial",
	}
}

func testCfg() schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincVirtualization,
		AppNameID:     "guest",
		ImageMeta:     schema.ImageMeta{Image: "/home/u/.local/share/zinc/images/fedora.qcow2"},
		VirtualizationMeta: schema.VirtualizationMeta{
			BaseDigest: "sha256:" + strings.Repeat("a", 64),
			MemoryMiB:  8192,
			VCPUs:      4,
			Display:    schema.VMDisplayAccelerated,
		},
	}
}

// pairs finds the value following each occurrence of flag.
func pairs(args []string, flag string) []string {
	var found []string
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			found = append(found, args[index+1])
		}
	}
	return found
}

func has(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// The guest must actually be accelerated and sized as configured; without KVM these VMs
// are far too slow for what they are for.
func TestArgs_MachineSizingAndAcceleration(t *testing.T) {
	args := Args(testCfg(), testLayout())

	if args[0] != Binary {
		t.Errorf("argv[0] = %q, want %q", args[0], Binary)
	}
	if got := pairs(args, "-machine"); len(got) != 1 || !strings.Contains(got[0], "accel=kvm") {
		t.Errorf("-machine = %v, want it to enable kvm", got)
	}
	if got := pairs(args, "-cpu"); len(got) != 1 || got[0] != "host" {
		t.Errorf("-cpu = %v, want host", got)
	}
	if got := pairs(args, "-smp"); len(got) != 1 || got[0] != "4" {
		t.Errorf("-smp = %v, want 4", got)
	}
	if got := pairs(args, "-m"); len(got) != 1 || got[0] != "8192M" {
		t.Errorf("-m = %v, want 8192M", got)
	}
}

// -nodefaults keeps qemu from contributing devices of its own, so a guest's hardware is
// exactly what the config asked for. The seccomp sandbox is the host-side boundary.
func TestArgs_NoDefaultsAndSandbox(t *testing.T) {
	args := Args(testCfg(), testLayout())
	if !has(args, "-nodefaults") {
		t.Error("-nodefaults missing: qemu would add devices the config never asked for")
	}
	sandbox := pairs(args, "-sandbox")
	if len(sandbox) != 1 || !strings.HasPrefix(sandbox[0], "on") {
		t.Fatalf("-sandbox = %v, want it enabled", sandbox)
	}
	for _, deny := range []string{"elevateprivileges=deny", "spawn=deny", "resourcecontrol=deny"} {
		if !strings.Contains(sandbox[0], deny) {
			t.Errorf("-sandbox %q should contain %s", sandbox[0], deny)
		}
	}
}

// The guest writes to its overlay and only its overlay. The pinned base is reached
// through the overlay's backing file, so it must never be handed to qemu as a disk -
// which is what keeps the authored image from drifting away from its digest.
func TestArgs_OnlyTheOverlayIsAttached(t *testing.T) {
	cfg := testCfg()
	args := Args(cfg, testLayout())

	drives := pairs(args, "-drive")
	if len(drives) != 2 {
		t.Fatalf("got %d drives, want the overlay and the seed: %v", len(drives), drives)
	}
	if !strings.Contains(drives[0], testLayout().Overlay) || !strings.Contains(drives[0], "format=qcow2") {
		t.Errorf("first drive = %q, want the qcow2 overlay", drives[0])
	}
	for _, arg := range args {
		if strings.Contains(arg, cfg.ImageMeta.Image) {
			t.Errorf("the pinned base image appears in the command line (%q); only the overlay may be attached", arg)
		}
	}
}

// The seed is the guest's first-boot identity: read-only, because a guest has no business
// rewriting it, and absent entirely when cloud-init is off.
func TestArgs_SeedIsReadOnlyAndOptional(t *testing.T) {
	args := Args(testCfg(), testLayout())
	seed := pairs(args, "-drive")[1]
	if !strings.Contains(seed, "readonly=on") || !strings.Contains(seed, "format=raw") {
		t.Errorf("seed drive = %q, want a read-only raw disk", seed)
	}

	layout := testLayout()
	layout.Seed = ""
	if drives := pairs(Args(testCfg(), layout), "-drive"); len(drives) != 1 {
		t.Errorf("with cloud-init disabled, got %d drives, want only the overlay: %v", len(drives), drives)
	}
}

// A forwarded port must reach the host that started the guest, not the LAN. Binding every
// interface would quietly publish a guest service to the network.
func TestArgs_ForwardsBindLoopbackOnly(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{
		{HostPort: 2222, GuestPort: 22},
		{HostPort: 8080, GuestPort: 80},
	}
	netdev := pairs(Args(cfg, testLayout()), "-netdev")
	if len(netdev) != 1 {
		t.Fatalf("-netdev = %v, want exactly one", netdev)
	}
	for _, want := range []string{"hostfwd=tcp:127.0.0.1:2222-:22", "hostfwd=tcp:127.0.0.1:8080-:80"} {
		if !strings.Contains(netdev[0], want) {
			t.Errorf("-netdev %q should contain %q", netdev[0], want)
		}
	}
	if strings.Contains(netdev[0], "hostfwd=tcp::") {
		t.Error("a forward bound every interface; it must bind 127.0.0.1 so the guest port is not published to the LAN")
	}
}

// An app with no forwards still gets outbound access, and nothing inbound.
func TestArgs_NoForwardsMeansNoInbound(t *testing.T) {
	netdev := pairs(Args(testCfg(), testLayout()), "-netdev")
	if len(netdev) != 1 || netdev[0] != "user,id=net0" {
		t.Fatalf("-netdev = %v, want plain user networking with no forwards", netdev)
	}
}

// The display mode is the difference between a playable game and a slideshow, so each
// mode must produce exactly the device and display pair it promises.
func TestArgs_DisplayModes(t *testing.T) {
	cases := []struct {
		mode        schema.VMDisplay
		wantDevice  string
		wantDisplay string
	}{
		{schema.VMDisplayAccelerated, "virtio-gpu-gl-pci", "gtk,gl=on"},
		{schema.VMDisplayWindow, "virtio-gpu-pci", "gtk"},
		{schema.VMDisplayNone, "", "none"},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := testCfg()
			cfg.VirtualizationMeta.Display = tc.mode
			args := Args(cfg, testLayout())

			if got := pairs(args, "-display"); len(got) != 1 || got[0] != tc.wantDisplay {
				t.Errorf("-display = %v, want %q", got, tc.wantDisplay)
			}
			devices := pairs(args, "-device")
			if tc.wantDevice == "" {
				for _, device := range devices {
					if strings.Contains(device, "gpu") || strings.Contains(device, "tablet") {
						t.Errorf("headless mode should attach no display hardware, got %q", device)
					}
				}
				return
			}
			found := false
			for _, device := range devices {
				if strings.HasPrefix(device, tc.wantDevice) {
					found = true
				}
			}
			if !found {
				t.Errorf("devices = %v, want one starting with %q", devices, tc.wantDevice)
			}
			if !has(devices, "virtio-keyboard-pci") || !has(devices, "virtio-tablet-pci") {
				t.Errorf("a windowed guest needs a keyboard and pointer, got %v", devices)
			}
		})
	}
}

// Audio is an explicit grant, as it is for containers: an app that did not ask for sound
// gets no sound card at all rather than a silent one.
// venus is deliberately absent. Enabling it where the host virglrenderer lacks venus
// support does not degrade to OpenGL - it fails the whole renderer, leaving the guest with
// no display at all, which is exactly what happened when it was tried on Fedora 43.
func TestArgs_AcceleratedDoesNotRequireVenus(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Display = schema.VMDisplayAccelerated
	for _, device := range pairs(Args(cfg, testLayout()), "-device") {
		if strings.Contains(device, "venus") || strings.Contains(device, "blob=on") {
			t.Errorf("device %q enables venus/blob; on a host whose virglrenderer lacks venus this kills the display entirely", device)
		}
	}
}

func TestArgs_AudioOnlyOnGrant(t *testing.T) {
	cfg := testCfg()
	for _, arg := range Args(cfg, testLayout()) {
		if strings.Contains(arg, "audiodev") || strings.Contains(arg, "hda") {
			t.Fatalf("audio appeared without a grant: %q", arg)
		}
	}

	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	args := Args(cfg, testLayout())
	if got := pairs(args, "-audiodev"); len(got) != 1 || !strings.HasPrefix(got[0], "pipewire") {
		t.Errorf("-audiodev = %v, want pipewire", got)
	}
	// Playback only: the codec must have no capture stream, so there is no microphone inside
	// the guest to open. hda-duplex here would hand every audio-enabled guest a microphone it
	// never asked for, which is what this used to do.
	if !has(pairs(args, "-device"), "hda-output,audiodev=snd0") {
		t.Errorf("playback-only audio should attach hda-output, got %v", pairs(args, "-device"))
	}
	if has(pairs(args, "-device"), "hda-duplex,audiodev=snd0") {
		t.Error("a guest with no Microphone grant must not get a capture-capable codec")
	}
}

// A microphone is a device the guest either has or does not. Granting it swaps the codec for
// one that carries a capture stream; without the grant that endpoint does not exist, which is
// enforcement the container side cannot yet match.
func TestArgs_MicrophoneGrantAttachesACaptureCodec(t *testing.T) {
	cfg := testCfg()
	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	cfg.AudioMeta.Microphone = schema.AudioDevice{Default: true}
	args := Args(cfg, testLayout())
	if !has(pairs(args, "-device"), "hda-duplex,audiodev=snd0") {
		t.Errorf("a granted microphone should attach hda-duplex, got %v", pairs(args, "-device"))
	}
}

// A microphone with no playback is still a sound card, and it still has to carry capture.
func TestArgs_MicrophoneAloneStillAttachesAudio(t *testing.T) {
	cfg := testCfg()
	cfg.AudioMeta.Microphone = schema.AudioDevice{Default: true}
	args := Args(cfg, testLayout())
	if got := pairs(args, "-audiodev"); len(got) != 1 {
		t.Fatalf("-audiodev = %v, want one backend", got)
	}
	if !has(pairs(args, "-device"), "hda-duplex,audiodev=snd0") {
		t.Errorf("microphone-only audio should still attach hda-duplex, got %v", pairs(args, "-device"))
	}
}

// The control and console sockets are how zvr finds a running guest again; both must be
// non-blocking servers so a VM boots whether or not anyone is attached.
func TestArgs_ControlSocketsDoNotWaitForAClient(t *testing.T) {
	args := Args(testCfg(), testLayout())
	for _, flag := range []string{"-qmp", "-serial"} {
		got := pairs(args, flag)
		if len(got) != 1 {
			t.Fatalf("%s = %v, want exactly one", flag, got)
		}
		if !strings.Contains(got[0], "server=on") || !strings.Contains(got[0], "wait=off") {
			t.Errorf("%s %q must be a non-blocking server, or the guest waits for a client before booting", flag, got[0])
		}
	}
	if got := pairs(args, "-pidfile"); len(got) != 1 || got[0] != testLayout().PIDFile {
		t.Errorf("-pidfile = %v, want %q", got, testLayout().PIDFile)
	}
}

func TestDisplay_QuotesWhitespace(t *testing.T) {
	got := Display([]string{"qemu-system-x86_64", "-name", "my guest", "-m", "1024M"})
	if !strings.Contains(got, "'my guest'") {
		t.Errorf("Display() = %q, want the whitespace argument quoted", got)
	}
}

// Guest Vulkan is opt-in and costs the qemu process its seccomp jail: venus runs in a
// helper process that virglrenderer forks, and the sandbox both forbids the fork and kills
// the child that inherits its filter - silently, reported only as a generic "virgl could
// not be initialized". Measured on real hardware; this asserts the trade is made exactly
// where it was asked for and nowhere else.
func TestArgs_VulkanIsOptInAndDropsTheSandbox(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Display = schema.VMDisplayAccelerated

	// Default: sandboxed, and no venus.
	plain := Args(cfg, testLayout())
	if len(pairs(plain, "-sandbox")) != 1 {
		t.Error("an app that did not ask for Vulkan must keep qemu's seccomp sandbox")
	}
	for _, device := range pairs(plain, "-device") {
		if strings.Contains(device, "venus") {
			t.Errorf("venus must not appear without being asked for, got %q", device)
		}
	}

	// Opted in: venus present, sandbox gone.
	cfg.VirtualizationMeta.Vulkan = true
	vulkan := Args(cfg, testLayout())
	if len(pairs(vulkan, "-sandbox")) != 0 {
		t.Error("Vulkan requires the sandbox to be dropped, or the venus helper is killed")
	}
	var gpu string
	for _, device := range pairs(vulkan, "-device") {
		if strings.HasPrefix(device, "virtio-gpu-gl-pci") {
			gpu = device
		}
	}
	for _, want := range []string{"venus=on", "blob=on", "hostmem="} {
		if !strings.Contains(gpu, want) {
			t.Errorf("gpu device %q must contain %q", gpu, want)
		}
	}
}

// Vulkan rides on the accelerated display's device, so it must not silently attach itself
// to a mode that has no virtio-gpu-gl to carry it.
func TestArgs_VulkanOnlyOnTheAcceleratedDevice(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Vulkan = true
	for _, mode := range []schema.VMDisplay{schema.VMDisplayWindow, schema.VMDisplayNone} {
		cfg.VirtualizationMeta.Display = mode
		for _, device := range pairs(Args(cfg, testLayout()), "-device") {
			if strings.Contains(device, "venus") {
				t.Errorf("display %q should carry no venus, got %q", mode, device)
			}
		}
	}
}

// A Windows-class guest is a different machine, not a Linux one with a flag flipped: it
// boots UEFI, has a TPM, and can only use hardware whose drivers ship in the box. Windows
// Setup pointed at a virtio disk reports finding no drives at all.
func TestArgs_WindowsClassMachine(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Display = schema.VMDisplayCompatible
	cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	cfg.VirtualizationMeta.Firmware = schema.VMFirmwareUEFI
	cfg.VirtualizationMeta.TPM = true
	layout := testLayout()
	layout.Firmware = Firmware{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE.secboot.fd", VarsPath: "/data/guest-uefi-vars.fd"}
	layout.TPMSocket = "/run/user/1000/zinc/vm/guest.tpm"

	args := Args(cfg, layout)

	// UEFI: two pflash drives, and the code half read-only so the guest cannot rewrite the
	// firmware it booted from.
	flash := pairs(args, "-drive")
	var code, vars string
	for _, drive := range flash {
		if strings.Contains(drive, "if=pflash") && strings.Contains(drive, "unit=0") {
			code = drive
		}
		if strings.Contains(drive, "if=pflash") && strings.Contains(drive, "unit=1") {
			vars = drive
		}
	}
	if code == "" || !strings.Contains(code, "readonly=on") {
		t.Errorf("UEFI code pflash should be attached read-only, got %q", code)
	}
	if vars == "" || !strings.Contains(vars, layout.Firmware.VarsPath) {
		t.Errorf("the guest needs its own writable UEFI variable store, got %q", vars)
	}

	// TPM 2.0 on the interface Windows looks for.
	if !has(pairs(args, "-device"), "tpm-tis,tpmdev=tpm0") {
		t.Error("a TPM guest needs the tpm-tis device Windows expects")
	}
	if got := pairs(args, "-tpmdev"); len(got) != 1 || !strings.Contains(got[0], "emulator") {
		t.Errorf("-tpmdev = %v, want the swtpm emulator backend", got)
	}

	// Devices an installer without virtio drivers can actually see.
	devices := pairs(args, "-device")
	// Prefixes, not exact matches: the NIC also carries this app's own MAC.
	for _, want := range []string{"ahci,id=ahci", "e1000e,netdev=net0", "usb-tablet,bus=usb.0"} {
		if !hasPrefix(devices, want) {
			t.Errorf("a compatible-devices guest needs %q, got %v", want, devices)
		}
	}
	for _, device := range devices {
		if strings.HasPrefix(device, "virtio-") {
			t.Errorf("a compatible-devices guest must carry no virtio hardware, got %q", device)
		}
	}
	// Drives too, not only -device. `if=virtio` creates a virtio-blk controller without
	// ever appearing in a -device argument, which is exactly how a virtio cloud-init seed
	// slipped onto a machine configured for a guest with no virtio drivers.
	for _, drive := range pairs(args, "-drive") {
		if strings.Contains(drive, "if=virtio") {
			t.Errorf("a compatible-devices guest must have no virtio drive, got %q", drive)
		}
	}
	if got := pairs(args, "-display"); len(got) != 1 || got[0] != "gtk" {
		t.Errorf("-display = %v, want plain gtk on VGA", got)
	}

	// Windows reads the hardware clock as local time; UTC leaves it wrong by the offset.
	if got := pairs(args, "-rtc"); len(got) != 1 || got[0] != "base=localtime" {
		t.Errorf("-rtc = %v, want base=localtime for a compatible-devices guest", got)
	}
}

// A Linux guest must be untouched by any of that: still virtio, still BIOS, still UTC.
func TestArgs_LinuxGuestUnchangedByWindowsSupport(t *testing.T) {
	args := Args(testCfg(), testLayout())
	if len(pairs(args, "-tpmdev")) != 0 {
		t.Error("a guest that did not ask for a TPM must not get one")
	}
	for _, drive := range pairs(args, "-drive") {
		if strings.Contains(drive, "pflash") {
			t.Errorf("a BIOS guest should have no pflash, got %q", drive)
		}
	}
	if got := pairs(args, "-rtc"); len(got) != 1 || got[0] != "base=utc" {
		t.Errorf("-rtc = %v, want base=utc for a virtio guest", got)
	}
	if !hasPrefix(pairs(args, "-device"), "virtio-net-pci,netdev=net0") {
		t.Error("a virtio guest should keep its virtio NIC")
	}
}

// The discs are attached on every run, read-only; only the boot order is the install's own.
// A guest needs a CD after its OS is installed as much as during it - that is how the
// drivers the installer had no room for get in, virtio-win's display driver among them - so
// attaching them only while installing would leave no way to hand an installed guest a file.
func TestArgs_InstallMediaAttachedOnEveryRun(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	cfg.VirtualizationMeta.InstallMedia = []string{"/iso/win11.iso", "/iso/virtio-win.iso"}

	normal := Args(cfg, testLayout())
	attached := 0
	for _, drive := range pairs(normal, "-drive") {
		if strings.Contains(drive, "media=cdrom") {
			attached++
			if !strings.Contains(drive, "readonly=on") {
				t.Errorf("a disc must be read-only, got %q", drive)
			}
		}
	}
	if attached != 2 {
		t.Errorf("a normal run attached %d discs, want both", attached)
	}
	if len(pairs(normal, "-boot")) != 0 {
		t.Error("a normal run should not override the boot order: the disk boots, the disc is just there")
	}

	layout := testLayout()
	layout.Installing = true
	installing := Args(cfg, layout)
	cdroms := 0
	for _, drive := range pairs(installing, "-drive") {
		if strings.Contains(drive, "media=cdrom") {
			cdroms++
			if !strings.Contains(drive, "readonly=on") {
				t.Errorf("install media should be read-only, got %q", drive)
			}
		}
	}
	if cdroms != 2 {
		t.Errorf("got %d cdroms, want both install ISOs", cdroms)
	}
	// once=, not order=: an installer reboots itself partway through, and a permanently
	// disc-first order lands that reboot back in the installer.
	got := pairs(installing, "-boot")
	if len(got) != 1 || !strings.Contains(got[0], "once=d") {
		t.Errorf("-boot = %v, want a one-shot disc boot", got)
	}
	if strings.Contains(got[0], "order=d") {
		t.Errorf("-boot = %v, a permanent disc-first order restarts the installer on its own reboot", got)
	}
}

// The cloud-init seed follows the machine's device profile. A virtio seed on a compatible
// guest is doubly wrong: unreadable to a guest with no virtio drivers, and virtio hardware
// on a machine configured precisely because it has none.
func TestArgs_SeedFollowsTheDeviceProfile(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	cfg.VirtualizationMeta.Display = schema.VMDisplayCompatible
	args := Args(cfg, testLayout()) // testLayout carries a seed

	seedAttached := false
	for _, drive := range pairs(args, "-drive") {
		if strings.Contains(drive, testLayout().Seed) {
			seedAttached = true
			if strings.Contains(drive, "if=virtio") {
				t.Errorf("the seed should not be a virtio drive on a compatible guest: %q", drive)
			}
			if !strings.Contains(drive, "readonly=on") {
				t.Errorf("the seed must stay read-only: %q", drive)
			}
		}
	}
	if !seedAttached {
		t.Error("the seed should still be attached, just on a bus the guest can read")
	}
	if !has(pairs(args, "-device"), "ide-cd,drive=seed,bus=ahci.1") {
		t.Error("the compatible seed should ride the same controller as the disk")
	}

	// A virtio guest keeps the virtio seed.
	cfg.VirtualizationMeta.Devices = schema.VMDevicesVirtio
	cfg.VirtualizationMeta.Display = schema.VMDisplayAccelerated
	virtioSeed := false
	for _, drive := range pairs(Args(cfg, testLayout()), "-drive") {
		if strings.Contains(drive, testLayout().Seed) && strings.Contains(drive, "if=virtio") {
			virtioSeed = true
		}
	}
	if !virtioSeed {
		t.Error("a virtio guest should keep its virtio seed drive")
	}
}

// Secure Boot is not enabled by loading the secboot firmware. OVMF keeps its signature
// database in memory only System Management Mode may write, so without SMM the variables
// are unprotected and the firmware does not ENFORCE Secure Boot. Nothing on the host shows
// the difference; the guest decides it. Windows 11 Setup refuses with "This PC doesn't
// currently meet Windows 11 system requirements", because as far as it can tell Secure Boot
// is simply off.
func TestArgs_SecureBootRequiresSMM(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Firmware = schema.VMFirmwareUEFI
	cfg.VirtualizationMeta.SecureBoot = true
	args := Args(cfg, testLayout())

	machine := pairs(args, "-machine")
	if len(machine) != 1 || !strings.Contains(machine[0], "smm=on") {
		t.Errorf("-machine = %v, want smm=on or Secure Boot is not enforced", machine)
	}
	globals := pairs(args, "-global")
	if !has(globals, "driver=cfi.pflash01,property=secure,value=on") {
		t.Errorf("-global = %v, want the pflash secure property that routes variable writes through SMM", globals)
	}
	if !has(globals, "ICH9-LPC.disable_s3=1") {
		t.Errorf("-global = %v, want S3 disabled: resuming from it bypasses the firmware's re-validation", globals)
	}

	// A guest that did not ask for Secure Boot pays none of it.
	cfg.VirtualizationMeta.SecureBoot = false
	plain := Args(cfg, testLayout())
	if got := pairs(plain, "-machine"); strings.Contains(got[0], "smm") {
		t.Errorf("-machine = %v, want no SMM without Secure Boot", got)
	}
	if len(pairs(plain, "-global")) != 0 {
		t.Error("a guest without Secure Boot should carry no secure-boot globals")
	}
}

// Fedora ships its current 4 MB OVMF build as a pair of qcow2 images and keeps only the
// legacy 2 MB build as raw .fd files. Hardcoding format=raw makes qemu read a qcow2 header
// as if it were firmware, so the format has to travel with the paths.
func TestFirmwareArgs_CarriesTheImageFormat(t *testing.T) {
	qcow := firmwareArgs(Firmware{
		CodePath: "/usr/share/edk2/ovmf/OVMF_CODE_4M.secboot.qcow2",
		VarsPath: "/data/guest-uefi-vars.qcow2",
		Format:   "qcow2",
	})
	for _, drive := range pairs(qcow, "-drive") {
		if !strings.Contains(drive, "format=qcow2") {
			t.Errorf("pflash drive %q should declare format=qcow2", drive)
		}
	}

	// An unset format still has to produce a working machine: raw is what every .fd is.
	raw := firmwareArgs(Firmware{
		CodePath: "/usr/share/edk2/ovmf/OVMF_CODE.secboot.fd",
		VarsPath: "/data/guest-uefi-vars.fd",
	})
	for _, drive := range pairs(raw, "-drive") {
		if !strings.Contains(drive, "format=raw") {
			t.Errorf("with no format set, pflash drive %q should default to raw", drive)
		}
	}
}

// Without -uuid every qemu guest reports the SMBIOS UUID 00000000-0000-0000-0000-000000000000
// and the default NIC carries 52:54:00:12:34:56, both shared with every other default qemu VM.
// Windows Autopilot identifies a device by a hash over exactly those fields, so a guest with
// the defaults can match a stranger's corporate enrolment and come up at OOBE demanding a
// sign-in to their tenant. One did: a fresh Windows 11 install landed on an SAP sign-in page.
func TestArgs_GuestsGetTheirOwnMachineIdentity(t *testing.T) {
	cfg := testCfg()
	cfg.AppNameID = "first-vm"
	first := Args(cfg, testLayout())

	cfg.AppNameID = "second-vm"
	second := Args(cfg, testLayout())

	uuidOf := func(args []string) string {
		got := pairs(args, "-uuid")
		if len(got) != 1 {
			t.Fatalf("-uuid = %v, want exactly one", got)
		}
		return got[0]
	}
	firstUUID, secondUUID := uuidOf(first), uuidOf(second)

	if firstUUID == "00000000-0000-0000-0000-000000000000" {
		t.Error("the guest got qemu's default all-zero SMBIOS UUID, which every default VM shares")
	}
	if firstUUID == secondUUID {
		t.Errorf("two apps share the UUID %s; each app is a different machine", firstUUID)
	}
	// Stable across runs: Windows treats a machine whose UUID changed as different hardware
	// and asks to be reactivated, so this must not be randomised.
	cfg.AppNameID = "first-vm"
	if again := uuidOf(Args(cfg, testLayout())); again != firstUUID {
		t.Errorf("the same app got %s then %s; the identity has to survive a restart", firstUUID, again)
	}

	// The NIC feeds the same hardware hash, so it cannot stay on qemu's shared default.
	nic := ""
	for _, device := range pairs(first, "-device") {
		if strings.Contains(device, "netdev=net0") {
			nic = device
		}
	}
	if !strings.Contains(nic, "mac=") {
		t.Errorf("NIC %q should carry a per-app MAC, not qemu's shared default", nic)
	}
	if strings.Contains(nic, "mac=52:54:00:12:34:56") {
		t.Errorf("NIC %q kept qemu's default MAC", nic)
	}
	if macFor("first-vm", "") == macFor("second-vm", "") {
		t.Error("two apps derived the same MAC")
	}
}

// hasPrefix is has() for arguments that carry per-app suffixes, such as the NIC's MAC.
func hasPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// A guest whose NIC sits under QEMU's 52:54:00 prefix announces itself as a virtual machine
// to anything that looks at the address. An app that must not can supply its own, and it has
// to be used verbatim rather than mixed into the derivation.
func TestArgs_MacAddressOverride(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.MacAddress = "02:1a:2b:3c:4d:5e"

	nic := ""
	for _, device := range pairs(Args(cfg, testLayout()), "-device") {
		if strings.Contains(device, "netdev=net0") {
			nic = device
		}
	}
	if !strings.Contains(nic, "mac=02:1a:2b:3c:4d:5e") {
		t.Errorf("NIC %q should carry the address the app asked for", nic)
	}
	if strings.Contains(nic, "52:54:00") {
		t.Errorf("NIC %q still carries QEMU's prefix despite an override", nic)
	}
}

// A guest with no display driver keeps whatever mode the firmware left it in, so its
// resolution is fixed at boot and a window resize only scales those pixels. Plain VGA has no
// xres/yres at all and its built-in EDID is 1280x800, which is why an unconfigured guest is
// always exactly that. bochs-display takes a size and the firmware honours it; virtio-vga and
// qxl-vga accept the properties but OVMF drives their VGA-compatible half and ignores them.
func TestArgs_FixedGuestResolution(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Display = schema.VMDisplayCompatible
	cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	cfg.VirtualizationMeta.Firmware = schema.VMFirmwareUEFI
	cfg.VirtualizationMeta.DisplayWidth = 1920
	cfg.VirtualizationMeta.DisplayHeight = 1080

	devices := pairs(Args(cfg, testLayout()), "-device")
	if !hasPrefix(devices, "bochs-display,xres=1920,yres=1080") {
		t.Errorf("a fixed size needs bochs-display, the only device the firmware takes a resolution from, got %v", devices)
	}
	if hasPrefix(devices, "VGA") {
		t.Errorf("plain VGA cannot be given a resolution and should not be attached, got %v", devices)
	}

	// Asking for nothing must not change the machine: plain VGA is what a guest that never
	// requested a size has always had, and it is the only one a BIOS guest can use.
	cfg.VirtualizationMeta.DisplayWidth, cfg.VirtualizationMeta.DisplayHeight = 0, 0
	if !hasPrefix(pairs(Args(cfg, testLayout()), "-device"), "VGA,vgamem_mb=64") {
		t.Error("a guest with no fixed size should keep plain VGA")
	}
}

// 4K needs more than the resolution: at the display's default 16 MiB it has less video memory
// than its screen, and at QEMU's default 75 Hz its EDID pixel clock overflows a 16-bit field.
// Either one alone drops the guest silently back to 1280x800, so both must be asked for.
func TestArgs_FourKNeedsMemoryAndASlowerEdidClock(t *testing.T) {
	cfg := testCfg()
	cfg.VirtualizationMeta.Display = schema.VMDisplayCompatible
	cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	cfg.VirtualizationMeta.Firmware = schema.VMFirmwareUEFI
	cfg.VirtualizationMeta.DisplayWidth = 3840
	cfg.VirtualizationMeta.DisplayHeight = 2160

	display := ""
	for _, device := range pairs(Args(cfg, testLayout()), "-device") {
		if strings.HasPrefix(device, "bochs-display") {
			display = device
		}
	}
	if display == "" {
		t.Fatal("no bochs-display attached for a 4K guest")
	}
	if !strings.Contains(display, "xres=3840,yres=2160") {
		t.Errorf("display %q should ask for 3840x2160", display)
	}
	// 3840*2160*4 is 31.6 MiB, so the 16 MiB default is not enough.
	if !strings.Contains(display, "vgamem=33554432") {
		t.Errorf("display %q should carry a framebuffer big enough for 4K", display)
	}
	if !strings.Contains(display, "refresh_rate=50000") {
		t.Errorf("display %q should slow the EDID clock enough for 4K to fit its field", display)
	}
}

// An install has no app yet, so it runs under a fixed placeholder name. Deriving the machine
// identity from that name would give every install on every host the same UUID and MAC - the
// collision the identity exists to prevent, at the one moment it matters most, because OOBE
// runs during an install and is what reads them.
func TestArgs_InstallIdentityIsNotSharedAcrossHosts(t *testing.T) {
	cfg := testCfg()
	cfg.AppNameID = "install"

	byName := pairs(Args(cfg, testLayout()), "-uuid")

	layout := testLayout()
	layout.Identity = "/home/someone/.local/share/zinc/images/win11.qcow2"
	seeded := pairs(Args(cfg, layout), "-uuid")

	if len(byName) != 1 || len(seeded) != 1 {
		t.Fatalf("want one -uuid each, got %v and %v", byName, seeded)
	}
	if byName[0] == seeded[0] {
		t.Error("the identity seed was ignored; every install would share one machine identity")
	}

	other := testLayout()
	other.Identity = "/home/nobody/images/win11.qcow2"
	if same := pairs(Args(cfg, other), "-uuid"); same[0] == seeded[0] {
		t.Error("two different disks produced the same identity")
	}
	// Deterministic: resuming a half-finished install must not change the hardware under it.
	if again := pairs(Args(cfg, layout), "-uuid"); again[0] != seeded[0] {
		t.Error("the same disk produced a different identity on a second run")
	}
}
