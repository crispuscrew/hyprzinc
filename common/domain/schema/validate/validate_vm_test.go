package validate

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// baseVM is a minimal valid VM app: an absolute, pinned base disk, real sizing, and an
// explicit display mode.
func baseVM() schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincVirtualization,
		AppNameID:     "guest",
		ImageMeta:     schema.ImageMeta{Image: "/var/lib/zinc/images/fedora-42.qcow2"},
		VirtualizationMeta: schema.VirtualizationMeta{
			BaseDigest:  "sha256:" + strings.Repeat("a", 64),
			DiskSizeGiB: 40,
			MemoryMiB:   8192,
			VCPUs:       4,
			Display:     schema.VMDisplayAccelerated,
		},
	}
}

func TestVM_MinimalConfigIsValid(t *testing.T) {
	if err := Validate(baseVM()); err != nil {
		t.Fatalf("a minimal VM app should validate, got: %v", err)
	}
}

// The base disk is pinned by the sha256 of its bytes, the file-level equivalent of the
// rule that a third-party container image must be digest-pinned. Without it, whatever
// happens to be at that path is what boots.
func TestVM_BaseDigestRequiredAndCanonical(t *testing.T) {
	cases := []struct {
		name   string
		digest string
	}{
		{"missing", ""},
		{"no prefix", strings.Repeat("a", 64)},
		{"too short", "sha256:" + strings.Repeat("a", 63)},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64)},
		{"not hex", "sha256:" + strings.Repeat("z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVM()
			cfg.VirtualizationMeta.BaseDigest = tc.digest
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), "BaseDigest") {
				t.Fatalf("digest %q: want a BaseDigest error, got: %v", tc.digest, err)
			}
		})
	}
}

// A relative base path resolves against whatever working directory the launcher happened
// to inherit, so the same config would boot a different disk depending on where it was
// started from.
func TestVM_BasePathMustBeAbsolute(t *testing.T) {
	cfg := baseVM()
	cfg.ImageMeta.Image = "images/fedora-42.qcow2"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative base path: want an absolute-path error, got: %v", err)
	}
}

// A guest has no default RAM or CPU count to fall back on, so both must be stated.
func TestVM_SizingMustBePositive(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.MemoryMiB = 0
	cfg.VirtualizationMeta.VCPUs = 0
	err := Validate(cfg)
	if err == nil {
		t.Fatal("zero memory and vcpus: want an error, got nil")
	}
	for _, want := range []string{"MemoryMiB", "VCPUs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got: %v", want, err)
		}
	}
}

// Whether a guest gets an accelerated window is the difference between a usable game and
// an unusable one, so it is stated rather than guessed.
func TestVM_DisplayMustBeExplicitAndKnown(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.Display = ""
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "Display") {
		t.Fatalf("empty display: want a Display error, got: %v", err)
	}
	cfg.VirtualizationMeta.Display = schema.VMDisplay("Spice")
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "Display") {
		t.Fatalf("unknown display: want a Display error, got: %v", err)
	}
	for _, mode := range []schema.VMDisplay{schema.VMDisplayNone, schema.VMDisplayWindow, schema.VMDisplayAccelerated} {
		cfg.VirtualizationMeta.Display = mode
		if err := Validate(cfg); err != nil {
			t.Errorf("display %q should be accepted, got: %v", mode, err)
		}
	}
}

// A field a VM cannot honour is an ERROR, not something quietly dropped: a config whose
// Capabilities or NetworkLists look configured but do nothing is worse than one that
// refuses to save, because the author believes in a boundary that is not there.
func TestVM_ContainerOnlyFieldsRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*schema.AppConfig)
		want   string
	}{
		{"capabilities", func(cfg *schema.AppConfig) { cfg.Capabilities = []string{"CAP_NET_ADMIN"} }, "Capabilities"},
		{"keys", func(cfg *schema.AppConfig) {
			cfg.Keys = []schema.Key{{Type: schema.SSH, Path: "/home/u/.ssh/id_ed25519"}}
		}, "Keys"},
		{"volumes", func(cfg *schema.AppConfig) {
			cfg.Volumes = []schema.Volume{{HostMounted: true, HostMount: "/data", InnerMount: "/data"}}
		}, "Volumes"},
		{"configs", func(cfg *schema.AppConfig) {
			cfg.Configs = []schema.ConfigFile{{BundlePath: "app.conf", InnerMount: "/etc/app"}}
		}, "Configs"},
		{"host theme", func(cfg *schema.AppConfig) { cfg.HostTheme = true }, "HostTheme"},
		{"multiterminal", func(cfg *schema.AppConfig) {
			cfg.StartConditions.Terminal = true
			cfg.StartConditions.Multiterminal = true
			cfg.StartConditions.Entrypoint = "sh"
		}, "Multiterminal"},
		{"internal user", func(cfg *schema.AppConfig) { cfg.InternalUserMeta.UseNonRootUser = true }, "InternalUserMeta"},
		{"resources", func(cfg *schema.AppConfig) { cfg.ResourcesMeta.MaxRamMiB = 2048 }, "ResourcesMeta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVM()
			tc.mutate(&cfg)
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s on a VM app: want an error naming %s, got: %v", tc.name, tc.want, err)
			}
			if !strings.Contains(err.Error(), "not supported for a VM app") {
				t.Errorf("the error should say the field is unsupported for a VM, got: %v", err)
			}
		})
	}
}

// The mirror of the rule above: VM fields on a container app would be inert, so they are
// rejected rather than ignored.
func TestContainer_VirtualizationFieldsRejected(t *testing.T) {
	cfg := baseCfg()
	cfg.VirtualizationMeta.MemoryMiB = 4096
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "VirtualizationMeta") {
		t.Fatalf("VM fields on a container app: want a VirtualizationMeta error, got: %v", err)
	}
}

// A container app is judged exactly as before: its image still needs a digest pin, and
// the VM rules do not leak into it.
func TestContainer_StillRequiresDigestPin(t *testing.T) {
	cfg := baseCfg()
	cfg.ImageMeta.Image = "docker.io/library/alpine:3.20"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("unpinned container image: want a digest-pin error, got: %v", err)
	}
}

// A VM base is a path, not a registry reference, so it must not be held to the container
// reference rules - an absolute path has no @sha256: tail.
func TestVM_BasePathNotHeldToContainerImageRules(t *testing.T) {
	cfg := baseVM()
	if err := Validate(cfg); err != nil {
		t.Fatalf("an absolute base path with a separate digest should validate, got: %v", err)
	}
}

// zvr runs rootless and cannot bind a privileged port; catching it here beats a bind
// error at launch that says nothing about the config behind it.
func TestVM_PrivilegedHostPortRejected(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 80, GuestPort: 80}}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "privileged port") {
		t.Fatalf("privileged host port: want a privileged-port error, got: %v", err)
	}

	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 2222, GuestPort: 22}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("unprivileged forward should validate, got: %v", err)
	}
}

func TestVM_PortRangeChecked(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.ForwardPorts = []schema.PortForward{{HostPort: 2222, GuestPort: 70000}}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "GuestPort") {
		t.Fatalf("out-of-range guest port: want a GuestPort error, got: %v", err)
	}
}

// The seed ISO is readable by the guest, so a private key placed here would be handed to
// it. Only a .pub path is accepted.
func TestVM_PrivateSSHKeyRejected(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.CloudInit.SSHKeyPath = "/home/user/.ssh/id_ed25519"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "PUBLIC key") {
		t.Fatalf("private key path: want a public-key error, got: %v", err)
	}

	cfg.VirtualizationMeta.CloudInit.SSHKeyPath = "/home/user/.ssh/id_ed25519.pub"
	if err := Validate(cfg); err != nil {
		t.Fatalf("a .pub key path should validate, got: %v", err)
	}
}

// Identity fields alongside a disabled cloud-init would be written nowhere.
func TestVM_DisabledCloudInitWithIdentityRejected(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.CloudInit = schema.CloudInit{Disabled: true, UserName: "player"}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "CloudInit") {
		t.Fatalf("disabled cloud-init with a user name: want a CloudInit error, got: %v", err)
	}
}

func TestVM_CloudInitUserNameCharset(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.CloudInit.UserName = "Player One"
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "UserName") {
		t.Fatalf("invalid user name: want a UserName error, got: %v", err)
	}
}

// Install steps are shared: for a VM they become cloud-init runcmd lines, so the same
// control-character rule applies to both types.
func TestVM_InstallStepsStillScreened(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.CloudInit.UserName = "player"
	cfg.ImageMeta.Install = []string{"dnf install -y steam"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("a clean install step should validate on a VM app, got: %v", err)
	}
	cfg.ImageMeta.Install = []string{"true\nrm -rf /"}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("install step with a newline: want a control-characters error, got: %v", err)
	}
}

// The type itself must still be one of the two known values.
func TestType_UnknownRejected(t *testing.T) {
	cfg := baseVM()
	cfg.Type = schema.Type("ZincJail")
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "ZincJail") {
		t.Fatalf("unknown type: want an error naming it, got: %v", err)
	}
}

// Vulkan needs the accelerated display's virtio-gpu-gl device; asking for it in another
// mode would be a setting that silently does nothing.
func TestVM_VulkanRequiresTheAcceleratedDisplay(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.Vulkan = true
	cfg.VirtualizationMeta.Display = schema.VMDisplayWindow
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "Vulkan") {
		t.Fatalf("Vulkan without the accelerated display: want a Vulkan error, got: %v", err)
	}

	cfg.VirtualizationMeta.Display = schema.VMDisplayAccelerated
	if err := Validate(cfg); err != nil {
		t.Fatalf("Vulkan with the accelerated display should validate, got: %v", err)
	}
}

// Enabling Vulkan removes a boundary rather than adding one, so it must be surfaced as a
// create-time advisory on a security tool.
func TestVM_VulkanWarnsAboutTheSandbox(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.Vulkan = true
	warnings := Warnings(cfg)
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning, "sandbox") {
			found = true
		}
	}
	if !found {
		t.Errorf("enabling Vulkan should warn that it disables qemu's sandbox, got %v", warnings)
	}
	if len(Warnings(baseVM())) != 0 {
		t.Error("an app without Vulkan should not carry that warning")
	}
}

// A virtio display on a guest declared to have no virtio drivers boots to a black screen -
// the firmware paints, the OS takes over, and the device is left with no active scanout.
// Measured on Windows 11. It stays a warning rather than an error because it is the correct
// machine once the guest's driver is installed, which is the only way off a fixed screen size.
func TestVM_VirtioDisplayOnACompatibleGuestWarns(t *testing.T) {
	for _, display := range []schema.VMDisplay{schema.VMDisplayWindow, schema.VMDisplayAccelerated} {
		cfg := baseVM()
		cfg.VirtualizationMeta.Display = display
		cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
		if err := Validate(cfg); err != nil {
			t.Errorf("Display %s on a compatible guest should stay valid, got %v", display, err)
		}
		found := false
		for _, warning := range Warnings(cfg) {
			if strings.Contains(warning, "viogpudo") {
				found = true
			}
		}
		if !found {
			t.Errorf("Display %s on a compatible guest should warn, got %v", display, Warnings(cfg))
		}
	}

	// The driverless pairing is the one being recommended, so it must stay quiet.
	quiet := baseVM()
	quiet.VirtualizationMeta.Display = schema.VMDisplayCompatible
	quiet.VirtualizationMeta.Devices = schema.VMDevicesCompatible
	for _, warning := range Warnings(quiet) {
		if strings.Contains(warning, "viogpudo") {
			t.Errorf("Display Compatible is the driverless choice and must not warn, got %q", warning)
		}
	}
	// A virtio guest drives a virtio display by definition.
	virtio := baseVM()
	virtio.VirtualizationMeta.Display = schema.VMDisplayWindow
	if len(Warnings(virtio)) != 0 {
		t.Errorf("a virtio guest with a virtio display should not warn, got %v", Warnings(virtio))
	}
}

// A fixed guest screen size is only meaningful for the one display mode whose guest has no
// driver to resize itself, and it rides on a device with no BIOS-compatible mode. Accepting
// it anywhere else would write a config that looks configured and changes nothing, or one
// that boots to a blank window.
func TestVM_FixedResolutionNeedsCompatibleDisplayAndUEFI(t *testing.T) {
	compatible := func() schema.AppConfig {
		cfg := baseVM()
		cfg.VirtualizationMeta.Display = schema.VMDisplayCompatible
		cfg.VirtualizationMeta.Devices = schema.VMDevicesCompatible
		cfg.VirtualizationMeta.Firmware = schema.VMFirmwareUEFI
		return cfg
	}

	// Sizes measured against real firmware, not assumed: each of these was confirmed to
	// come up at the size asked for, and 4096x2160 to fall back to 1280x800.
	for _, size := range [][2]int{{1920, 1080}, {2560, 1440}, {3840, 2160}, {3840, 2400}} {
		cfg := compatible()
		cfg.VirtualizationMeta.DisplayWidth, cfg.VirtualizationMeta.DisplayHeight = size[0], size[1]
		if err := Validate(cfg); err != nil {
			t.Fatalf("%dx%d on a Compatible UEFI guest should validate, got: %v", size[0], size[1], err)
		}
	}

	cases := []struct {
		name          string
		width, height int
		mutate        func(*schema.AppConfig)
		want          string
	}{
		{"width with no height", 1920, 0, nil, "both or neither"},
		{"height with no width", 0, 1080, nil, "both or neither"},
		{"below the smallest real mode", 320, 200, nil, "at least"},
		{"a mistyped extra digit", 19200, 10800, nil, "too large"},
		{"wider than the EDID can describe", 4096, 2160, nil, "too large"},
		{"an odd width", 1921, 1080, nil, "even"},
		{"an accelerated guest resizes with its window", 1920, 1080,
			func(cfg *schema.AppConfig) { cfg.VirtualizationMeta.Display = schema.VMDisplayAccelerated },
			"Compatible"},
		{"BIOS cannot drive the device that carries it", 1920, 1080,
			func(cfg *schema.AppConfig) { cfg.VirtualizationMeta.Firmware = schema.VMFirmwareBIOS },
			"UEFI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := compatible()
			cfg.VirtualizationMeta.DisplayWidth = tc.width
			cfg.VirtualizationMeta.DisplayHeight = tc.height
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("%dx%d should be refused", tc.width, tc.height)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}

	// Unset stays unset: a guest that never asked for a size must not acquire one.
	if err := Validate(compatible()); err != nil {
		t.Errorf("no fixed size should still validate, got: %v", err)
	}
}

// The NIC address goes onto a qemu -device argument, where a comma starts a new property and
// a space splits the argument, so the value is screened rather than trusted.
func TestVM_MacAddressScreened(t *testing.T) {
	cfg := baseVM()
	cfg.VirtualizationMeta.MacAddress = "02:1a:2b:3c:4d:5e"
	if err := Validate(cfg); err != nil {
		t.Fatalf("a locally-administered unicast address should validate, got: %v", err)
	}

	cases := []struct{ name, mac, want string }{
		{"too few octets", "02:1a:2b:3c:4d", "six"},
		{"not hex", "02:1a:2b:3c:4d:zz", "hex"},
		{"single-digit octet", "2:1a:2b:3c:4d:5e", "two hex digits"},
		{"multicast", "01:1a:2b:3c:4d:5e", "multicast"},
		{"all zero", "00:00:00:00:00:00", "all-zero"},
		{"property injection", "02:1a:2b:3c:4d:5e,mac=de:ad:be:ef:00:01", "octet"},
		{"argument split", "02 1a 2b 3c 4d 5e", "six"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVM()
			cfg.VirtualizationMeta.MacAddress = tc.mac
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("%q should be refused", tc.mac)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// A readiness probe runs as the container's healthcheck. There is no way to run a command
// inside a guest from outside it, so on a VM app the field would be inert.
func TestVM_ReadyCheckRejected(t *testing.T) {
	cfg := baseVM()
	cfg.StartConditions.ReadyCheck = []string{"true"}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "StartConditions.ReadyCheck") {
		t.Fatalf("ReadyCheck on a VM app: want a not-supported error, got: %v", err)
	}
}

// A guest gets the same fail-closed egress a container does, so a self-scoped list is legal now.
// The rest of the vocabulary is not: a guest has no siblings and publishes through ForwardPorts.
func TestVM_EgressListsAreAcceptedAndTheRestRefused(t *testing.T) {
	base := func() schema.AppConfig {
		cfg := baseVM()
		cfg.NetworkMeta.NetworkLists = nil
		return cfg
	}

	allowed := base()
	allowed.NetworkMeta.NetworkLists = []schema.NetworkList{{
		IPv4CIDR: []string{"10.0.0.0/8"},
		Ports:    []int{443},
	}}
	if err := Validate(allowed); err != nil {
		t.Fatalf("a self-scoped egress list should be legal for a guest now, got: %v", err)
	}

	refused := []struct {
		name string
		list schema.NetworkList
		want string
	}{
		{"ingress", schema.NetworkList{Ingress: true, Ports: []int{8080}}, "ForwardPorts"},
		{"host scoped", schema.NetworkList{Host: true, IPv4CIDR: []string{"0.0.0.0/0"}}, "host-scoped"},
		{"a sibling link", schema.NetworkList{AppName: "vpn", IPv4CIDR: []string{"0.0.0.0/0"}}, "no sibling apps"},
		{"by name", schema.NetworkList{Domains: []string{"example.com"}, Ports: []int{443}}, "name the addresses"},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := base()
			cfg.NetworkMeta.NetworkLists = []schema.NetworkList{testCase.list}
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("%s should be refused for a guest", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("want an error mentioning %q, got: %v", testCase.want, err)
			}
		})
	}
}

// A guest whose lists are allowances has no DNS unless it names a resolver: its queries go to the
// host's, which its own default-drop chain refuses. Measured on a booted guest, and invisible
// until then, so the author is told at authoring time.
func TestWarnings_WhitelistGuestWithNoResolver(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "guest", Type: schema.ZincVirtualization,
		NetworkMeta: schema.NetworkMeta{NetworkLists: []schema.NetworkList{
			{IPv4CIDR: []string{"1.1.1.1/32"}, Ports: []int{443}},
		}},
	}
	if !mentionsDNS(Warnings(cfg)) {
		t.Errorf("a default-drop guest with no DNSServers should be warned, got %v", Warnings(cfg))
	}

	// Named a resolver: the rules let it through and the guest is pointed at it.
	withResolver := cfg
	withResolver.NetworkMeta.DNSServers = []string{"1.1.1.1"}
	if mentionsDNS(Warnings(withResolver)) {
		t.Errorf("a guest that named a resolver should not be warned: %v", Warnings(withResolver))
	}

	// An all-blacklist guest is allow-all-except, so its DNS is not dropped and nothing is wrong.
	blacklist := schema.AppConfig{
		AppNameID: "guest", Type: schema.ZincVirtualization,
		NetworkMeta: schema.NetworkMeta{NetworkLists: []schema.NetworkList{
			{Blacklist: true, IPv4CIDR: []string{"192.0.2.0/24"}},
		}},
	}
	if mentionsDNS(Warnings(blacklist)) {
		t.Errorf("an allow-all-except guest resolves fine, so should not be warned: %v", Warnings(blacklist))
	}

	// A container is not this warning's business: it gets its resolver from the pod, not from qemu.
	container := cfg
	container.Type = schema.ZincContainer
	if mentionsDNS(Warnings(container)) {
		t.Errorf("this is a guest-only warning: %v", Warnings(container))
	}
}

func mentionsDNS(warns []string) bool {
	for _, warn := range warns {
		if strings.Contains(warn, "DNSServers") {
			return true
		}
	}
	return false
}
