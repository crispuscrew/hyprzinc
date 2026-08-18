package schema

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only app-config schema version this build understands.
const SchemaVersion = 3

type Type string

const (
	ZincContainer      Type = "ZincContainer"
	ZincVirtualization Type = "ZincVirtualization"
)

// AppConfig is one app definition: ~/.config/zinc/apps/<name>.yaml
// Most parameters can be overridden at app start
type AppConfig struct {
	SchemaVersion int  `yaml:"SchemaVersion"`
	Type          Type `yaml:"Type"` // VM vs Container, "" interpreted as error

	AppNameID string `yaml:"AppNameID"` // Also using as container/vm name

	// Inherits names a base app this one starts from, stating only what differs. Stated keys win
	// (false and empty lists included), nested blocks merge, stated lists replace. Resolved on every
	// read, so a base grants its capabilities to every child.
	Inherits string `yaml:"Inherits"`

	Icon        string `yaml:"Icon"`
	Description string `yaml:"Description"`
	Group       string `yaml:"Group"` // optional category, for grouping in a launcher; presentation-only

	StartConditions StartConditions `yaml:"StartConditions"`
	StopConditions  StopConditions  `yaml:"StopConditions"`

	ResourcesMeta    ResourcesMeta    `yaml:"ResourcesMeta"`
	InternalUserMeta InternalUserMeta `yaml:"InternalUserMeta"`
	ImageMeta        ImageMeta        `yaml:"ImageMeta"`
	DisplayMeta      DisplayMeta      `yaml:"DisplayMeta"`
	NetworkMeta      NetworkMeta      `yaml:"NetworkMeta"`
	NotificationMeta NotificationMeta `yaml:"NotificationMeta"`
	DBusMeta         DBusMeta         `yaml:"DBusMeta"`

	// VirtualizationMeta applies only to Type: ZincVirtualization; validation rejects it on a
	// container app rather than ignoring it.
	VirtualizationMeta VirtualizationMeta `yaml:"VirtualizationMeta"`

	// Env is the app's environment. Zinc's own wiring (runtime dir, display, bus address) is refused.
	Env map[string]string `yaml:"Env"`

	// ReadOnlyRootfs makes the root filesystem read-only. /dev, /dev/shm, /run, /tmp and /var/tmp
	// stay writable tmpfs, so what stops is an app writing into its own image.
	ReadOnlyRootfs bool `yaml:"ReadOnlyRootfs"`

	Configs      []ConfigFile `yaml:"Configs"` // files the app ships with, from its own bundle
	Volumes      []Volume     `yaml:"Volumes"` // extra host bind mounts can also be added for one run via `zcr run -v` (not persisted here)
	Keys         []Key        `yaml:"Keys"`
	HostTheme    bool         `yaml:"HostTheme"`
	AudioMeta    AudioMeta    `yaml:"AudioMeta"`
	Capabilities []string     `yaml:"Capabilities"` // if container --cap-add entries
}

type StartConditions struct {
	DependsOn []string `yaml:"DependsOn"` // apps, which must be running while/starting with it

	// ReadyCheck decides whether this app is ready for its dependents. A list of words, not a shell
	// line; installed as the container's healthcheck, so needs a shell. Empty means running is ready,
	// which is wrong for anything a dependent routes through.
	ReadyCheck []string `yaml:"ReadyCheck"`
	// ReadyTimeoutSec bounds how long a dependent waits for ReadyCheck to pass before its
	// launch fails; 0 uses the runner's default. It only means anything with ReadyCheck set.
	ReadyTimeoutSec int `yaml:"ReadyTimeoutSec"`

	Autorestart bool `yaml:"Autorestart"` // Autorestart if falls, not restart if manually closed

	Entrypoint              string `yaml:"Entrypoint"`              // if empty use app default
	Terminal                bool   `yaml:"Terminal"`                // if true, create terminal window for it
	Multiterminal           bool   `yaml:"Multiterminal"`           // createable attached terminals, every terminal hold container to live
	MultiterminalEntrypoint string `yaml:"MultiterminalEntrypoint"` // if empty use Entrypoint
}

type StopConditions struct {
	KeepAlive  bool `yaml:"KeepAlive"`  // Stays freeze/alive after entrypoint finish
	Background bool `yaml:"Background"` // Stays alive after window close
}

type ResourcesMeta struct {
	MaxCPUCores float64 `yaml:"MaxCPUCores"` // Can be 0.5, for example
	MaxRamMiB   int64   `yaml:"MaxRamMiB"`
	MaxSwapMiB  int64   `yaml:"MaxSwapMiB"` // Only if swap accessible
	PIDsLimit   int64   `yaml:"PIDsLimit"`  // For fork-bomb prevented
}

type InternalUserMeta struct {
	UseNonRootUser  bool   `yaml:"UseNonRootUser"` // If true using NonRootUser
	KeepUserID      bool   `yaml:"KeepUserID"`     // Keep the same id and etc as real host user
	NonRootUserName string `yaml:"NonRootUserName"`
}

type ImageMeta struct {
	// Image is a digest-pinned container reference (ZincContainer) or a base disk path
	// (ZincVirtualization, pinned by BaseDigest instead).
	Image string `yaml:"Image"`
	// Install is the setup steps: the derived image's RUN layer, or cloud-init runcmd for a VM.
	Install []string `yaml:"Install"`

	// SourceTag records the tag the digest in Image was resolved FROM. Provenance only: a launch
	// always runs Image. Empty for a hand-pinned image.
	SourceTag string `yaml:"SourceTag"`
}

// VirtualizationMeta configures a VM app. The base disk is never written to (each app gets a
// copy-on-write overlay), so deleting the overlay resets the app.
type VirtualizationMeta struct {
	// BaseDigest is the sha256 the base disk must hash to, as "sha256:<64 hex>".
	BaseDigest string `yaml:"BaseDigest"`

	DiskSizeGiB int64 `yaml:"DiskSizeGiB"` // virtual size of the app's overlay; 0 keeps the base image's size
	MemoryMiB   int64 `yaml:"MemoryMiB"`   // guest RAM, allocated not capped
	VCPUs       int   `yaml:"VCPUs"`       // guest CPUs

	Display VMDisplay `yaml:"Display"`

	// DisplayWidth and DisplayHeight fix the guest's screen. Both zero leaves it to the firmware,
	// which settles on 1280x800 and stays there for a guest with no graphics driver.
	DisplayWidth  int `yaml:"DisplayWidth"`
	DisplayHeight int `yaml:"DisplayHeight"`

	// Vulkan passes guest Vulkan to the host GPU (qemu's venus). Off by default: it costs qemu's
	// seccomp sandbox and needs a host virglrenderer built with venus.
	Vulkan bool `yaml:"Vulkan"`

	// Firmware is how the guest boots. Linux images generally boot either way; Windows 11
	// requires UEFI, and refuses to install without it.
	Firmware VMFirmware `yaml:"Firmware"`
	// SecureBoot enables UEFI Secure Boot, which Windows 11 also expects. Requires UEFI.
	SecureBoot bool `yaml:"SecureBoot"`
	// TPM attaches an emulated TPM 2.0. Windows 11 refuses to install without one.
	TPM bool `yaml:"TPM"`

	// Devices picks the guest's hardware. Windows Setup has no virtio drivers and finds no disk.
	Devices VMDevices `yaml:"Devices"`

	// InstallMedia are ISOs attached read-only on every run, not just at install: guest drivers
	// (virtio-win) arrive the same way. `zvr install` additionally boots from them.
	InstallMedia []string `yaml:"InstallMedia"`

	// ForwardPorts publishes a guest port on the host. VM apps do not use NetworkMeta: nftables in a
	// container netns does not carry over to a guest.
	ForwardPorts []PortForward `yaml:"ForwardPorts"`

	// MacAddress overrides the guest NIC. Empty derives one under QEMU's 52:54:00 prefix, which
	// announces a QEMU guest; a locally-administered address (02, 06, 0a, 0e) belongs to no vendor.
	MacAddress string `yaml:"MacAddress"`

	CloudInit CloudInit `yaml:"CloudInit"`
}

// IsZero reports whether nothing in the VM group was set, so validation can catch VM fields left
// on a container app.
func (virt VirtualizationMeta) IsZero() bool {
	return virt.BaseDigest == "" &&
		virt.DiskSizeGiB == 0 && virt.MemoryMiB == 0 && virt.VCPUs == 0 &&
		virt.Display == "" && virt.DisplayWidth == 0 && virt.DisplayHeight == 0 && !virt.Vulkan &&
		virt.Firmware == "" && !virt.SecureBoot && !virt.TPM &&
		virt.Devices == "" && len(virt.InstallMedia) == 0 &&
		len(virt.ForwardPorts) == 0 && virt.MacAddress == "" &&
		virt.CloudInit == CloudInit{}
}

// VMFirmware is how a guest boots.
type VMFirmware string

const (
	// VMFirmwareBIOS is the traditional path, and what a Linux cloud image expects.
	VMFirmwareBIOS VMFirmware = "BIOS"
	// VMFirmwareUEFI boots through OVMF, with its own writable variable store per app so a
	// guest's boot entries survive a restart without leaking into any other guest.
	VMFirmwareUEFI VMFirmware = "UEFI"
)

// VMDevices is the hardware profile a guest is given.
type VMDevices string

const (
	// VMDevicesVirtio is the default: virtio disk, network and input.
	VMDevicesVirtio VMDevices = "Virtio"
	// VMDevicesCompatible uses AHCI, an Intel NIC and a USB tablet: slower, but installable by an OS
	// that has never heard of virtio.
	VMDevicesCompatible VMDevices = "Compatible"
)

// VMDisplay is how a VM app is seen. Explicit rather than inferred from other fields.
type VMDisplay string

const (
	// VMDisplayNone runs headless over the serial console. The only mode that needs no display.
	VMDisplayNone VMDisplay = "None"
	// VMDisplayWindow opens a local window with no 3D acceleration. The fallback when a
	// guest or host lacks working virtio-gpu.
	VMDisplayWindow VMDisplay = "Window"
	// VMDisplayAccelerated backs the window with virtio-gpu-gl, so guest 3D runs on the host GPU.
	// Needs the virtio-gpu driver in the guest (Linux).
	VMDisplayAccelerated VMDisplay = "Accelerated"
	// VMDisplayCompatible uses plain VGA, which every OS can drive including at install time.
	VMDisplayCompatible VMDisplay = "Compatible"
)

// PortForward publishes GuestPort inside the VM as HostPort on the host.
type PortForward struct {
	HostPort  int `yaml:"HostPort"`
	GuestPort int `yaml:"GuestPort"`
}

// CloudInit is the first-boot identity written to a seed ISO and handed to the guest, so
// a freshly created VM is reachable without an interactive install.
type CloudInit struct {
	// Disabled skips the seed ISO entirely, for a base image that provisions itself or
	// one already configured.
	Disabled bool `yaml:"Disabled"`

	UserName string `yaml:"UserName"` // the account created in the guest; empty = the image's default
	// SSHKeyPath is a host path to a PUBLIC key. The seed ISO is readable by the guest.
	SSHKeyPath string `yaml:"SSHKeyPath"`
}

type DisplayMeta struct {
	DisableSecurityContext bool `yaml:"DisableSecurityContext"` // security-context | passthrough
	// RequireSecurityContext refuses to launch on a compositor without wp_security_context_v1 instead
	// of falling back to the raw socket. Contradicts DisableSecurityContext; both is refused.
	RequireSecurityContext bool `yaml:"RequireSecurityContext"`
	DisableGpuAccess       bool `yaml:"DisableGpuAccess"`
}

// DBusMeta gives an app a session bus of its own: an xdg-dbus-proxy socket carrying only the names
// listed here. Opt-in, because the host socket reaches every service the user runs. The proxy is
// deliberately NOT in the app's pod, so the app cannot signal or ptrace what filters it.
type DBusMeta struct {
	// Talk are the names the app may call. A trailing ".*" also grants services that appear later.
	Talk []string `yaml:"Talk"`
	// Own are the names the app may claim for itself: what a notifier or MPRIS player needs.
	Own []string `yaml:"Own"`
}

// IsZero reports whether no bus access was asked for - the fail-closed default, in which
// the app is given no session bus socket whatsoever.
func (bus DBusMeta) IsZero() bool { return len(bus.Talk) == 0 && len(bus.Own) == 0 }

// TunnelMeta gives an app a WireGuard interface Zinc creates in its netns before it starts. A
// tunnel needs CAP_NET_ADMIN and an app with NetworkLists may never hold it, so the privileged
// helper builds the interface and is gone before the app exists.
type TunnelMeta struct {
	// WireGuardConf is a host path to a wg-quick config, applied inside the netns with the private key
	// on the helper's STDIN. The script directives (PostUp, PreUp, PostDown, PreDown, SaveConfig,
	// Table) are refused as arbitrary shell; DNS is refused too - use NetworkMeta.DNSServers.
	WireGuardConf string `yaml:"WireGuardConf"`
}

// IsZero reports whether no tunnel was asked for.
func (tunnel TunnelMeta) IsZero() bool { return strings.TrimSpace(tunnel.WireGuardConf) == "" }

type NetworkMeta struct {
	// The first entry is priority
	NetworkLists []NetworkList `yaml:"NetworkLists"`

	// Tunnel, when set, is a WireGuard interface Zinc creates in the app's netns before the
	// app starts. See TunnelMeta - the app never holds the capability that builds it.
	Tunnel TunnelMeta `yaml:"Tunnel"`

	// DNSServers are the resolvers the app is given, and the only ones it may reach. A routed app
	// needs them: its --internal bridge resolver answers sibling names and forwards nothing.
	DNSServers []string `yaml:"DNSServers"`
}

type NetworkList struct {
	Host      bool   `yaml:"Host"`      // it list for host or container?
	AppName   string `yaml:"AppName"`   // if host == false, which app net do we use, "" == this app
	Interface string `yaml:"Interface"` // if u want to reach concrete interface of app/host

	Blacklist bool `yaml:"Blacklist"` // or whitelist

	Ingress bool `yaml:"Ingress"` // false = egress rule (default); true = Ports are this app's own listeners, exposed to the scope

	IPv4CIDR []string `yaml:"IPv4CIDR"`
	IPv6CIDR []string `yaml:"IPv6CIDR"`
	Ports    []int    `yaml:"Ports"`

	// Domains allow hosts by name, resolved AT LAUNCH into this list's allowed set. Enforcement is at
	// the IP layer on the addresses held then, so connecting by number is allowed and nothing inspects
	// a hostname. Never refreshed, so a rotating CDN drifts out of the set until restart.
	Domains []string `yaml:"Domains"`

	GatewayV4 string `yaml:"GatewayV4"` // if "" use default
	GatewayV6 string `yaml:"GatewayV6"`

	// Via routes the listed CIDRs THROUGH the named sibling instead of out this app's own egress -
	// how an app sits behind a VPN container without trusting it. If the sibling stops, the traffic
	// blackholes rather than falling back. Per list, so one app can pick a backend per destination.
	Via bool `yaml:"Via"`

	// Forward is the producer's half: siblings may route through me, and I masquerade their traffic
	// out of my egress. Bounded from both ends - WHERE is the client's Via list, WHAT is ForwardPorts.
	// This app's own egress rules do not bound what it forwards.
	Forward bool `yaml:"Forward"`

	// ForwardPorts narrows what a gateway carries to these destination ports; empty forwards any.
	ForwardPorts []int `yaml:"ForwardPorts"`
}

type NotificationMeta struct {
	Disabled bool `yaml:"Disabled"`
	Silenced bool `yaml:"Silenced"` // All notification from app will be silenced

	UseCustomPrefix bool   `yaml:"UseCustomPrefix"`
	CustomPrefix    string `yaml:"CustomPrefix"`

	AllowedActions   bool `yaml:"AllowedActions"`
	AllowedProlonged bool `yaml:"AllowedProlonged"` // Allowed expire_timeout > cfg.notifications.Prolonged
	AllowedLinks     bool `yaml:"AllowedLinks"`
}

// ConfigFile is one file the app ships with, kept in its bundle and mounted in at launch.
type ConfigFile struct {
	// BundlePath is relative to $XDG_CONFIG_HOME/zinc/apps/<app>/configs. Enforced relative: an
	// absolute path here would be a host bind mount, and Volumes is where those are reviewed.
	BundlePath string `yaml:"BundlePath"`
	InnerMount string `yaml:"InnerMount"`
	// Writable lets the app's edits survive. Off by default, so what a reviewer read is what runs.
	Writable bool `yaml:"Writable"`
}

type Volume struct {
	InnerMount string `yaml:"InnerMount"`

	SizeLimited  bool  `yaml:"SizeLimited"`
	SizeLimitMiB int64 `yaml:"SizeLimitMiB"` // Size limit, if possible

	HostMounted bool   `yaml:"HostMounted"`
	HostMount   string `yaml:"HostMount"`

	Writable   bool `yaml:"Writable"`
	Executable bool `yaml:"Executable"`
}

// Keys is a convenience layer for SSH/GPG only (section 3): unlike a plain Volume it mounts the
// key read-only into the container home (.ssh for SSH, .gnupg for GPG).
type KeyType string

const (
	SSH KeyType = "SSH"
	GPG KeyType = "GPG"
)

type Key struct {
	Type KeyType `yaml:"Type"`
	Path string  `yaml:"Path"`
}

// AudioMeta grants sound one direction at a time, saying WHAT an app gets rather than HOW: the
// runner picks the transport from the form each direction takes.
type AudioMeta struct {
	Playback   AudioDevice `yaml:"Playback"`
	Microphone AudioDevice `yaml:"Microphone"`
	// Monitor is the capability to record what OTHER apps are playing, through a PipeWire sink's
	// `.monitor` source. A grant on the RECORDER: there is no way for a player to mark its output
	// private. Not yet enforced for a container, since the socket grants it either way.
	Monitor AudioDevice `yaml:"Monitor"`
}

// AudioDevice is one direction of audio, in three forms:
//
//	none            not granted (also what an absent field means)
//	default         the session's own device, via the PipeWire socket
//	[/dev/snd/...]  exactly these ALSA nodes, passed with --device and enforced by the kernel
//
// On a container `default` is convenience, not enforcement: the PipeWire socket grants capture
// whatever this says. On a VM it IS enforced - the guest gets a playback-only sound device unless
// a microphone was asked for.
type AudioDevice struct {
	Default bool     // the session's own device
	Devices []string // exact /dev/snd nodes; mutually exclusive with Default
}

// IsZero reports the "not granted" state: both the absent field and an explicit `none`.
func (dev AudioDevice) IsZero() bool { return !dev.Default && len(dev.Devices) == 0 }

// audioNone and audioDefault are the two scalar spellings; anything else scalar is refused.
const (
	audioNone    = "none"
	audioDefault = "default"
)

// UnmarshalYAML accepts the scalar forms and the list form. An absent field stays zero.
func (dev *AudioDevice) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		switch node.Value {
		case audioNone, "", "null", "~":
			return nil
		case audioDefault:
			dev.Default = true
			return nil
		}
		return fmt.Errorf("%q: want %s, %s, or a list of /dev/snd device nodes",
			node.Value, audioNone, audioDefault)
	}
	return node.Decode(&dev.Devices)
}

// MarshalYAML writes "not granted" as an explicit `none` rather than omitting the key.
func (dev AudioDevice) MarshalYAML() (any, error) {
	switch {
	// Devices first: validation refuses both being set, but store.Marshal is exported and used
	// unvalidated for the $EDITOR round trip, so the narrow form is the safe tie-break.
	case len(dev.Devices) > 0:
		return dev.Devices, nil
	case dev.Default:
		return audioDefault, nil
	}
	return audioNone, nil
}
