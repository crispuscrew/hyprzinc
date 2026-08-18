package validate

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// The VM rules. Stricter than strictly necessary in one way: a field this build does not implement for
// a VM app is an ERROR rather than quietly ignored, because a config whose Capabilities look
// configured but do nothing is worse than one that refuses to save.

// fileDigestRE is a bare sha256 pin: "sha256:" + 64 hex, anchored at both ends. Unlike
// digestRE (which matches the @sha256:... tail of a container reference) this pins a
// FILE, so it stands alone rather than trailing a name.
const digestPrefix = "sha256:"

// checkVirtualization screens a VM app: its hardware sizing, the pinned base disk, the
// display mode, port forwards and cloud-init identity.
func checkVirtualization(cfg schema.AppConfig, add addFunc) {
	virt := cfg.VirtualizationMeta

	checkBaseImage(cfg.ImageMeta.Image, virt.BaseDigest, add)

	if virt.MemoryMiB <= 0 {
		add("VirtualizationMeta.MemoryMiB %d: must be > 0 (a guest has no default RAM to fall back on)", virt.MemoryMiB)
	}
	if virt.VCPUs <= 0 {
		add("VirtualizationMeta.VCPUs %d: must be > 0", virt.VCPUs)
	}
	if virt.DiskSizeGiB < 0 {
		add("VirtualizationMeta.DiskSizeGiB %d: must be >= 0 (0 keeps the base image's size)", virt.DiskSizeGiB)
	}

	switch virt.Display {
	case schema.VMDisplayNone, schema.VMDisplayWindow, schema.VMDisplayAccelerated, schema.VMDisplayCompatible:
	case "":
		add("VirtualizationMeta.Display: must be set (%s, %s, %s or %s)",
			schema.VMDisplayNone, schema.VMDisplayWindow, schema.VMDisplayAccelerated, schema.VMDisplayCompatible)
	default:
		add("VirtualizationMeta.Display %q: must be one of %s, %s, %s, %s",
			virt.Display, schema.VMDisplayNone, schema.VMDisplayWindow, schema.VMDisplayAccelerated, schema.VMDisplayCompatible)
	}

	checkResolution(virt, add)
	checkMac(virt.MacAddress, add)

	switch virt.Firmware {
	case schema.VMFirmwareBIOS, schema.VMFirmwareUEFI, "":
	default:
		add("VirtualizationMeta.Firmware %q: must be %s or %s (empty means %s)",
			virt.Firmware, schema.VMFirmwareBIOS, schema.VMFirmwareUEFI, schema.VMFirmwareBIOS)
	}
	if virt.SecureBoot && virt.Firmware != schema.VMFirmwareUEFI {
		// Secure Boot is a UEFI mechanism; there is nothing for it to attach to on BIOS.
		add("VirtualizationMeta.SecureBoot: requires Firmware %s", schema.VMFirmwareUEFI)
	}

	switch virt.Devices {
	case schema.VMDevicesVirtio, schema.VMDevicesCompatible, "":
	default:
		add("VirtualizationMeta.Devices %q: must be %s or %s (empty means %s)",
			virt.Devices, schema.VMDevicesVirtio, schema.VMDevicesCompatible, schema.VMDevicesVirtio)
	}

	for index, media := range virt.InstallMedia {
		checkInstallMedia(index, media, add)
	}

	// Vulkan rides on the accelerated display's virtio-gpu-gl device; there is nothing for
	// venus to attach to in the other modes, so asking for it there would be a setting that
	// silently does nothing.
	if virt.Vulkan && virt.Display != schema.VMDisplayAccelerated {
		add("VirtualizationMeta.Vulkan: requires Display %s (venus rides on that device); this app is %q",
			schema.VMDisplayAccelerated, virt.Display)
	}

	for index, forward := range virt.ForwardPorts {
		checkForward(index, forward, add)
	}
	checkCloudInit(virt.CloudInit, add)
}

// checkBaseImage screens the base disk path and its digest pin. The path is used to open
// a file and as the backing store recorded inside the app's overlay, so it must be
// absolute and free of the metacharacters that would shift a qemu-img argument.
func checkBaseImage(image, digest string, add addFunc) {
	switch {
	case strings.TrimSpace(image) == "":
		add("ImageMeta.Image: must not be empty (a VM app needs a base disk image)")
	case hasUnsafe(image):
		add("ImageMeta.Image %q: must be a single-line path (no whitespace or control characters)", image)
	case strings.ContainsRune(image, ','):
		// Same reason as InstallMedia below: a comma is qemu's -drive property separator, so
		// it appends options rather than staying inside the path.
		add("ImageMeta.Image %q: must not contain ',' - it separates qemu's -drive properties, so a comma appends options to the drive rather than staying in the path", image)
	case !filepath.IsAbs(image):
		// Resolved by whichever process happens to run zvr otherwise: a relative base would
		// mean a different disk depending on the working directory a hotkey inherited.
		add("ImageMeta.Image %q: must be an absolute path for a VM app (a relative base resolves differently depending on where the launcher was started)", image)
	case hasDotDot(image):
		add("ImageMeta.Image %q: must not contain a '..' segment", image)
	}

	switch {
	case strings.TrimSpace(digest) == "":
		add("VirtualizationMeta.BaseDigest: must be set - a VM base image is pinned by the sha256 of its bytes (sha256:<64 hex>), the same rule that digest-pins container images (section 5.5)")
	case !validFileDigest(digest):
		add("VirtualizationMeta.BaseDigest %q: must be sha256:<64 lowercase hex>", digest)
	}
}

// validFileDigest reports a canonical bare "sha256:<64 hex>" pin.
func validFileDigest(digest string) bool {
	rest, found := strings.CutPrefix(digest, digestPrefix)
	if !found || len(rest) != 64 {
		return false
	}
	for _, char := range rest {
		isHex := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}

// checkInstallMedia screens an ISO path. It is opened and handed to qemu as a drive, so it
// carries the same rules as the base image.
func checkInstallMedia(index int, media string, add addFunc) {
	switch {
	case strings.TrimSpace(media) == "":
		add("VirtualizationMeta.InstallMedia[%d]: must not be empty", index)
	case hasUnsafe(media):
		add("VirtualizationMeta.InstallMedia[%d] %q: must be a single-line path (no whitespace or control characters)", index, media)
	case strings.ContainsRune(media, ','):
		// A comma separates qemu's -drive properties, so it does not stay inside the path: qemu resolves a
		// duplicate key to the LAST one, so a second file= in the tail replaces the absolute path just
		// approved, and qemu will happily open a URL. `zvr install` boots from this medium.
		add("VirtualizationMeta.InstallMedia[%d] %q: must not contain ',' - it separates qemu's -drive properties, so a comma appends options to the drive rather than staying in the path", index, media)
	case !filepath.IsAbs(media):
		add("VirtualizationMeta.InstallMedia[%d] %q: must be an absolute path", index, media)
	case hasDotDot(media):
		add("VirtualizationMeta.InstallMedia[%d] %q: must not contain a '..' segment", index, media)
	}
}

func checkForward(index int, forward schema.PortForward, add addFunc) {
	if forward.HostPort < 1 || forward.HostPort > 65535 {
		add("VirtualizationMeta.ForwardPorts[%d].HostPort %d: must be 1-65535", index, forward.HostPort)
	}
	if forward.GuestPort < 1 || forward.GuestPort > 65535 {
		add("VirtualizationMeta.ForwardPorts[%d].GuestPort %d: must be 1-65535", index, forward.GuestPort)
	}
	if forward.HostPort > 0 && forward.HostPort < 1024 {
		// Rootless qemu cannot bind a privileged port, so this would fail at launch with a
		// bind error that says nothing about the config that caused it.
		add("VirtualizationMeta.ForwardPorts[%d].HostPort %d: must be >= 1024 (zvr runs rootless and cannot bind a privileged port)", index, forward.HostPort)
	}
}

// checkCloudInit screens the first-boot identity. The user name lands in the guest's
// user-data document and the key path is read from the host, so both are screened before
// anything is written.
func checkCloudInit(cloudInit schema.CloudInit, add addFunc) {
	if cloudInit.Disabled {
		if cloudInit.UserName != "" || cloudInit.SSHKeyPath != "" {
			add("VirtualizationMeta.CloudInit: Disabled is set, so UserName and SSHKeyPath would be written nowhere - clear them or enable cloud-init")
		}
		return
	}
	if name := cloudInit.UserName; name != "" && !nameRE.MatchString(name) {
		add("VirtualizationMeta.CloudInit.UserName %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", name)
	}
	switch path := cloudInit.SSHKeyPath; {
	case path == "":
	case hasUnsafe(path):
		add("VirtualizationMeta.CloudInit.SSHKeyPath %q: must be a single-line path (no whitespace or control characters)", path)
	case !filepath.IsAbs(path):
		add("VirtualizationMeta.CloudInit.SSHKeyPath %q: must be an absolute path", path)
	case strings.HasSuffix(path, ".pub"):
	default:
		// Not fatal-by-content (we cannot read the file here, this is pure), but a path that
		// is not a .pub is overwhelmingly a private key, and the seed ISO is guest-readable.
		add("VirtualizationMeta.CloudInit.SSHKeyPath %q: must be a PUBLIC key (a .pub path) - the seed ISO is readable by the guest, so a private key placed here would be handed to it", path)
	}
}

// checkContainerOnlyFields rejects, on a VM app, the fields that only a container can
// honour. Each names what it would take to support it, so the message reads as a
// boundary rather than a mystery.
func checkContainerOnlyFields(cfg schema.AppConfig, add addFunc) {
	unsupported := []struct {
		set   bool
		field string
		why   string
	}{
		{len(cfg.Capabilities) > 0, "Capabilities",
			"Linux capabilities are a container concept; a guest kernel has its own"},
		{len(cfg.NetworkMeta.NetworkLists) > 0, "NetworkMeta.NetworkLists",
			"the egress lock-down is nftables inside a container netns and does not reach a guest; use VirtualizationMeta.ForwardPorts"},
		{len(cfg.Keys) > 0, "Keys",
			"a VM has no host filesystem to mount keys into; use VirtualizationMeta.CloudInit.SSHKeyPath"},
		{len(cfg.Volumes) > 0, "Volumes",
			"sharing a host directory into a guest needs virtiofs, which this build does not implement"},
		{len(cfg.Configs) > 0, "Configs",
			"sharing a host directory into a guest needs virtiofs, which this build does not implement"},
		{len(cfg.Env) > 0, "Env",
			"a guest gets its environment from its own init, not from the process that started the machine; use VirtualizationMeta.CloudInit or ImageMeta.Install"},
		{cfg.ReadOnlyRootfs, "ReadOnlyRootfs",
			"a guest owns its disk and mounts its own root; the overlay is already discarded on reset, which is the VM answer to the same question"},
		{cfg.DisplayMeta.RequireSecurityContext, "DisplayMeta.RequireSecurityContext",
			"the Wayland security context is established for a container's own socket; a guest draws into a qemu window and never speaks the host's compositor protocol"},
		{!cfg.AudioMeta.Monitor.IsZero(), "AudioMeta.Monitor",
			"a monitor source is a tap on the host's PipeWire mix, and a guest sees an emulated sound card rather than the host graph, so there is nothing there for it to record"},
		{len(cfg.AudioMeta.Playback.Devices) > 0 || len(cfg.AudioMeta.Microphone.Devices) > 0, "AudioMeta device lists",
			"a guest cannot be handed a host character device; guest audio is routed through the session's PipeWire, so use `default` (or `none`)"},
		{cfg.HostTheme, "HostTheme",
			"the theme bundle is a read-only bind mount, which a guest cannot take"},
		{!cfg.DBusMeta.IsZero(), "DBusMeta",
			"the filtered bus is a unix socket bind-mounted from a proxy container, and a guest has no way to take one; reach the host bus over the network or run the app as a container"},
		{cfg.StartConditions.Multiterminal, "StartConditions.Multiterminal",
			"a guest has one console; attach to it with the serial console instead"},
		{len(cfg.StartConditions.ReadyCheck) > 0, "StartConditions.ReadyCheck",
			"the probe runs as the container's healthcheck, and there is no way to run a command inside a guest from outside it"},
		{cfg.InternalUserMeta != (schema.InternalUserMeta{}), "InternalUserMeta",
			"the guest owns its own users; use VirtualizationMeta.CloudInit.UserName"},
		{cfg.ResourcesMeta != (schema.ResourcesMeta{}), "ResourcesMeta",
			"a guest is sized by VirtualizationMeta.MemoryMiB and VCPUs, which allocate rather than cap"},
	}
	for _, rule := range unsupported {
		if rule.set {
			add("%s: not supported for a VM app (%s)", rule.field, rule.why)
		}
	}
}

// checkVirtualizationUnset rejects VM fields on a container app, the mirror of the rule
// above: a field that looks configured must never be silently inert.
func checkVirtualizationUnset(cfg schema.AppConfig, add addFunc) {
	if cfg.VirtualizationMeta.IsZero() {
		return
	}
	add("VirtualizationMeta: only applies to a VM app (Type: %s); this app is %s",
		schema.ZincVirtualization, cfg.Type)
}

// checkResolution screens a fixed guest screen size. Both dimensions or neither: supplying the missing
// half would invent a screen the author did not ask for. It also requires UEFI, because the device
// that carries a fixed mode has no VGA compatibility and a BIOS guest given it shows no picture.
func checkResolution(virt schema.VirtualizationMeta, add addFunc) {
	width, height := virt.DisplayWidth, virt.DisplayHeight
	if width == 0 && height == 0 {
		return
	}
	if width == 0 || height == 0 {
		add("VirtualizationMeta.DisplayWidth/DisplayHeight %dx%d: set both or neither", width, height)
		return
	}
	if width < 640 || height < 480 {
		add("VirtualizationMeta.DisplayWidth/DisplayHeight %dx%d: must be at least 640x480", width, height)
	}
	// Refused rather than clamped: a size the emulated display cannot describe to the
	// firmware does not degrade, it silently comes up at 1280x800 with nothing logged
	// anywhere, which is indistinguishable from the setting being ignored.
	if _, ok := schema.GuestDisplay(width, height); !ok && width >= 640 && height >= 480 {
		add("VirtualizationMeta.DisplayWidth/DisplayHeight %dx%d: too large for the guest's display to describe "+
			"(neither side may exceed %d, and the total is bounded by the EDID pixel clock); 3840x2160 works, 4096x2160 does not",
			width, height, schema.GuestDisplayMaxPixels)
	}
	if width%2 != 0 {
		add("VirtualizationMeta.DisplayWidth %d: must be even", width)
	}
	if virt.Display != schema.VMDisplayCompatible {
		add("VirtualizationMeta.DisplayWidth/DisplayHeight: only Display: %s takes a fixed size; "+
			"an accelerated guest resizes with its window", schema.VMDisplayCompatible)
	}
	if virt.Firmware != schema.VMFirmwareUEFI {
		add("VirtualizationMeta.DisplayWidth/DisplayHeight: needs Firmware: %s "+
			"(the display device that carries a fixed size has no BIOS-compatible mode)", schema.VMFirmwareUEFI)
	}
}

// checkMac screens a NIC address override. It goes onto a qemu -device argument, so the shape
// is pinned rather than trusted: a comma there would start a new property and a space would
// split the argument.
func checkMac(mac string, add addFunc) {
	if mac == "" {
		return
	}
	octets := strings.Split(mac, ":")
	if len(octets) != 6 {
		add("VirtualizationMeta.MacAddress %q: must be six colon-separated hex octets, e.g. 02:1a:2b:3c:4d:5e", mac)
		return
	}
	var first uint64
	for index, octet := range octets {
		if len(octet) != 2 {
			add("VirtualizationMeta.MacAddress %q: octet %d (%q) must be exactly two hex digits", mac, index+1, octet)
			return
		}
		value, err := strconv.ParseUint(octet, 16, 8)
		if err != nil {
			add("VirtualizationMeta.MacAddress %q: octet %d (%q) is not hex", mac, index+1, octet)
			return
		}
		if index == 0 {
			first = value
		}
	}
	// Bit 0 of the first octet marks a multicast address. A NIC given one cannot hold a normal
	// conversation, and the guest's networking would fail looking like anything but a config error.
	if first&0x01 != 0 {
		add("VirtualizationMeta.MacAddress %q: %02x is a multicast address (its low bit is set); "+
			"a NIC needs a unicast one", mac, first)
	}
	if strings.EqualFold(mac, "00:00:00:00:00:00") {
		add("VirtualizationMeta.MacAddress %q: the all-zero address is not usable", mac)
	}
}
