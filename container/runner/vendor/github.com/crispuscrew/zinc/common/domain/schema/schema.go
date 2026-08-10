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

	// Inherits names another app in the store this one starts from: it states only what
	// differs, and takes everything it does not state from that base. A key it states wins
	// (false and an empty list included), a nested block merges field by field, and a list
	// it states replaces the base's rather than adding to it.
	//
	// Resolution happens on every read, so editing a base changes every app built on it -
	// which is the point, and the thing to be careful with, since a base granting a
	// capability grants it to every child. `zc validate <app> --resolved` prints what an app
	// actually resolves to. What is saved is always what was written, never the merged result.
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

	// VirtualizationMeta applies only to Type: ZincVirtualization. A container app must
	// leave it at its zero value; validation rejects it rather than ignoring it, so a
	// field can never look configured while doing nothing.
	VirtualizationMeta VirtualizationMeta `yaml:"VirtualizationMeta"`

	Configs      []ConfigFile `yaml:"Configs"` // files the app ships with, from its own bundle
	Volumes      []Volume     `yaml:"Volumes"` // extra host bind mounts can also be added for one run via `zcr run -v` (not persisted here)
	Keys         []Key        `yaml:"Keys"`
	HostTheme    bool         `yaml:"HostTheme"`
	AudioMeta    AudioMeta    `yaml:"AudioMeta"`
	Capabilities []string     `yaml:"Capabilities"` // if container --cap-add entries
}

type StartConditions struct {
	DependsOn []string `yaml:"DependsOn"` // apps, which must be running while/starting with it

	// ReadyCheck is the command that decides whether this app is ready for the apps that
	// name it in DependsOn. It is written as a list of words (["test", "-f", "/run/ready"])
	// rather than as one shell line, so an argument containing a space still means itself;
	// the runner quotes each word and installs it as the container's healthcheck, so
	// `podman ps` answers the same question the dependents wait on. The image needs a shell.
	//
	// Empty keeps the old meaning of DependsOn: running is ready. That is fine for a
	// dependency whose service is up the moment its process is, and wrong for anything a
	// dependent routes through - a VPN container is running long before its tunnel is, and
	// a client started in that window has a default route pointing at a gateway that cannot
	// forward yet.
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
	// Image is where the app comes from, read according to Type: a container reference for
	// ZincContainer (digest-pinned unless localhost/), or the path of a base disk image for
	// ZincVirtualization (pinned by VirtualizationMeta.BaseDigest, since a file's hash
	// cannot ride inside its path).
	Image string `yaml:"Image"`
	// Install is the app's setup steps, one per entry. For a container they become the
	// single RUN layer of the derived image; for a VM they become cloud-init runcmd lines
	// on first boot. Same intent either way: what to add on top of the pinned base.
	Install []string `yaml:"Install"`

	// SourceTag records the tag the pinned digest in Image was resolved FROM, e.g.
	// "docker.io/library/alpine:3.20". It is provenance, not a second source of truth: a
	// launch always runs Image, never this, so a tag that moves cannot change what runs.
	//
	// It exists because a digest alone is a dead end. Once pinned, nothing on disk says what
	// the pin was of, so "has the thing I pinned been rebuilt, and does it now carry a fix I
	// need" is a question no tool can ask - a rebuilt tag and an abandoned one look identical.
	// Recording the tag makes staleness answerable: re-resolve it and compare.
	//
	// Empty for an image pinned by hand, which is honest rather than a gap: nothing knows
	// where that digest came from, and guessing a tag would invent provenance.
	SourceTag string `yaml:"SourceTag"`
}

// VirtualizationMeta configures a VM app: the guest's hardware, how you see it, and the
// first-boot identity handed to it. The base disk named by ImageMeta.Image is never
// written to - each app gets its own copy-on-write overlay - so deleting the overlay
// resets the app to the authored base, which is the VM analogue of a fresh container.
type VirtualizationMeta struct {
	// BaseDigest is the sha256 the base disk image must hash to, as "sha256:<64 hex>". A
	// VM base is a file rather than a registry reference, so the pin cannot ride inside
	// the reference the way a container digest does - but the rule is the same one: what
	// runs must be what was authorised.
	BaseDigest string `yaml:"BaseDigest"`

	DiskSizeGiB int64 `yaml:"DiskSizeGiB"` // virtual size of the app's overlay; 0 keeps the base image's size
	MemoryMiB   int64 `yaml:"MemoryMiB"`   // guest RAM, allocated not capped
	VCPUs       int   `yaml:"VCPUs"`       // guest CPUs

	Display VMDisplay `yaml:"Display"`

	// DisplayWidth and DisplayHeight fix the guest's screen size. Both zero leaves it to
	// the firmware, which settles on 1280x800 and stays there: a guest with no graphics
	// driver takes the resolution UEFI hands it at boot and cannot change it afterwards,
	// so resizing the window just scales those pixels. Setting them is what makes a
	// driverless guest, Windows above all, come up at a usable size.
	DisplayWidth  int `yaml:"DisplayWidth"`
	DisplayHeight int `yaml:"DisplayHeight"`

	// Vulkan passes the guest's Vulkan calls through to the host GPU (qemu's venus). It is
	// off by default and must be asked for, because it costs real things: qemu's own seccomp
	// sandbox has to be disabled (the venus renderer runs in a helper process that the
	// sandbox kills), and the host needs a virglrenderer built with venus support, which
	// distributions generally do not ship. Without it a guest still gets accelerated
	// OpenGL, but its Vulkan runs on the CPU - which is what Proton, DXVK and vkd3d use.
	Vulkan bool `yaml:"Vulkan"`

	// Firmware is how the guest boots. Linux images generally boot either way; Windows 11
	// requires UEFI, and refuses to install without it.
	Firmware VMFirmware `yaml:"Firmware"`
	// SecureBoot enables UEFI Secure Boot, which Windows 11 also expects. Requires UEFI.
	SecureBoot bool `yaml:"SecureBoot"`
	// TPM attaches an emulated TPM 2.0. Windows 11 refuses to install without one.
	TPM bool `yaml:"TPM"`

	// Devices picks which hardware the guest is given. It exists because a guest can only
	// use hardware it has drivers for, and Windows Setup has none for virtio: pointed at a
	// virtio disk it reports finding no drives at all.
	Devices VMDevices `yaml:"Devices"`

	// InstallMedia are ISO images attached read-only as CD-ROMs, on every run. A Windows
	// guest is installed from one rather than started from a cloud image, which is where the
	// name comes from, but the discs stay attached afterwards: guest drivers the installer
	// had no room for (virtio-win) are handed over the same way, and a guest whose network
	// is not up yet has no other route in. `zvr install` additionally boots from them.
	InstallMedia []string `yaml:"InstallMedia"`

	// ForwardPorts publishes a guest port on the host over user-mode networking. VM apps
	// do not use NetworkMeta: that model is enforced by nftables inside a container's own
	// network namespace and does not carry over to a guest, so rather than mis-enforce it
	// a VM app is limited to these explicit forwards.
	ForwardPorts []PortForward `yaml:"ForwardPorts"`

	// MacAddress overrides the guest NIC's address. Left empty, Zinc derives one from the
	// app name under QEMU's own 52:54:00 prefix, which is unique per app but says plainly
	// that the machine is a QEMU guest. Set this to present something else - a
	// locally-administered address (first octet 02, 06, 0a or 0e) belongs to no vendor and
	// so identifies nothing.
	MacAddress string `yaml:"MacAddress"`

	CloudInit CloudInit `yaml:"CloudInit"`
}

// IsZero reports whether nothing in the VM group was set. Validation uses it to catch VM
// fields left on a container app, where they would be inert.
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

// VMDevices is the hardware profile a guest is given. Virtio is faster in every dimension
// and is what a Linux guest should use; Compatible exists because a guest cannot use
// hardware it has no driver for, and Windows Setup ships none for virtio - pointed at a
// virtio disk it simply reports that it cannot find a drive.
type VMDevices string

const (
	// VMDevicesVirtio is the default: virtio disk, network and input.
	VMDevicesVirtio VMDevices = "Virtio"
	// VMDevicesCompatible uses hardware every mainstream OS has drivers for out of the box:
	// an AHCI disk, an Intel gigabit NIC and a USB tablet. Slower, and the price of being
	// able to install an OS that has never heard of virtio.
	VMDevicesCompatible VMDevices = "Compatible"
)

// VMDisplay is how a VM app is seen. It is explicit rather than inferred: whether a guest
// gets an accelerated window is exactly the difference between a usable game and an
// unusable one, so it is not something to guess from other fields.
type VMDisplay string

const (
	// VMDisplayNone runs headless - no window, reachable over the serial console. The only
	// mode that works without a display, so it is what a test or a server-ish guest uses.
	VMDisplayNone VMDisplay = "None"
	// VMDisplayWindow opens a local window with no 3D acceleration. The fallback when a
	// guest or host lacks working virtio-gpu.
	VMDisplayWindow VMDisplay = "Window"
	// VMDisplayAccelerated opens a local window backed by virtio-gpu-gl, so guest 3D runs
	// on the host GPU and frames reach the compositor without leaving the machine. Needs a
	// guest with the virtio-gpu driver (Linux).
	VMDisplayAccelerated VMDisplay = "Accelerated"
	// VMDisplayCompatible opens a local window on plain VGA, which every OS can drive
	// including at install time. No acceleration of any kind: it is what a guest without a
	// virtio-gpu driver gets, and on this hardware that means Windows.
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
	// SSHKeyPath is a host path to a PUBLIC key authorised for UserName. Public, never
	// private: the seed ISO is readable by the guest, so a private key put here would be
	// handed to it.
	SSHKeyPath string `yaml:"SSHKeyPath"`
}

type DisplayMeta struct {
	DisableSecurityContext bool `yaml:"DisableSecurityContext"` // security-context | passthrough
	// RequireSecurityContext refuses the launch on a compositor that does not implement
	// wp_security_context_v1, instead of handing the app the compositor socket directly.
	//
	// The fallback exists because most compositors still lack the protocol and a desktop that
	// refused to start anything would be useless. But the fallback is a real downgrade: the
	// app becomes a client the compositor cannot tell apart from an unsandboxed one, and the
	// container is labelled `zinc.wayland=passthrough` to say so. Until now nothing could ask
	// for the strict answer, so an app whose whole reason for being sandboxed is that it is
	// untrusted had no way to say "not like this".
	//
	// Contradicts DisableSecurityContext, and setting both is refused rather than resolved.
	RequireSecurityContext bool `yaml:"RequireSecurityContext"`
	DisableGpuAccess       bool `yaml:"DisableGpuAccess"`
}

// DBusMeta gives an app a session bus of its own: a per-instance socket served by
// xdg-dbus-proxy, carrying only the names listed here. Everything else on the real session
// bus is not merely denied to the app, it is invisible to it.
//
// This is opt-in and empty by default because a session bus is a desktop-wide capability,
// not a detail. Handing an app the host's socket lets it reach every service the user runs:
// read the keyring, drive the portal, talk to the compositor, restart units. A sandbox that
// mounts the real bus has a hole the width of the whole desktop, which is why Zinc's
// default is no bus at all. Naming something here opens exactly that much and nothing more.
//
// The proxy is a container Zinc owns, deliberately NOT a member of the app's pod. The app
// must not be able to signal, ptrace or otherwise reach into the process that is filtering
// it: the proxy holds the real socket, the app holds only the filtered one, and they share
// no namespace but the uid.
type DBusMeta struct {
	// Talk are the well-known names the app may send method calls to, e.g.
	// "org.freedesktop.portal.Desktop". A trailing ".*" matches a subtree, which
	// xdg-dbus-proxy expands - prefer the exact name, because a subtree also grants every
	// service that appears under it later, including ones that did not exist when the
	// config was reviewed.
	Talk []string `yaml:"Talk"`
	// Own are the well-known names the app may claim for itself: what a notifier, a tray
	// icon or an MPRIS media player needs to be addressable at all. Owning a name is how
	// other programs find this app, so it is a grant in its own right rather than something
	// Talk implies.
	Own []string `yaml:"Own"`
}

// IsZero reports whether no bus access was asked for - the fail-closed default, in which
// the app is given no session bus socket whatsoever.
func (bus DBusMeta) IsZero() bool { return len(bus.Talk) == 0 && len(bus.Own) == 0 }

// TunnelMeta gives an app a WireGuard interface that ZINC creates and configures in the
// app's network namespace before the app starts - not the app itself.
//
// That split is the whole point. A tunnel needs CAP_NET_ADMIN to exist, and an app with
// NetworkLists may never hold it: NET_ADMIN in the pod netns would let the app flush the
// egress ruleset that contains it, so validation refuses the combination. Which meant a
// gateway could be a NAT hop and never an actual tunnel endpoint. Zinc builds the interface
// in the same privileged helper that already installs routes and loads the ruleset, and it
// is gone before the app exists - so the app inherits a working tunnel and still holds no
// capability at all.
type TunnelMeta struct {
	// WireGuardConf is a host path to a wg-quick-format config: the [Interface] and [Peer]
	// sections a VPN provider hands out. Zinc reads it at launch and applies it inside the
	// netns; the private key travels on the helper's STDIN, so it never appears in an
	// argument list, an image, or a mount the app could read.
	//
	// The wg-quick script directives are refused rather than run - PostUp, PreUp, PostDown,
	// PreDown, SaveConfig and Table. The first four are arbitrary shell, and the helper that
	// would run them holds NET_ADMIN in the app's namespace; a config file is not a place to
	// accept code from. DNS is refused too, because Zinc already has one place to say that
	// and two would disagree: use NetworkMeta.DNSServers.
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

	// DNSServers are the resolvers the app is given, and the only ones it may reach.
	//
	// An app routed through a sibling needs them. Its own link is an --internal bridge, and
	// the resolver podman puts on one answers sibling names but cannot forward anything
	// else - measured: an external name comes back NXDOMAIN. Naming a resolver here gives
	// the app one it can reach, and because the address is routed through the sibling like
	// any other destination, the queries travel inside the tunnel and stop with it.
	//
	// They are also the only resolvers permitted: DNS to anything else is dropped, so an app
	// that carries a hardcoded resolver cannot step around them when it also has direct
	// egress of its own.
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

	// Domains name hosts this egress list allows by name instead of by address. Each is
	// resolved AT LAUNCH and its addresses join this list's allowed set, under the same
	// Ports as the CIDRs beside it.
	//
	// Read the guarantee precisely, because it is narrower than "the app may only talk to
	// these domains". What is enforced is at the IP layer, on the addresses the domains held
	// when the app started: an app that resolves somewhere else and connects is dropped, and
	// an app that connects to one of these addresses by number is allowed even if it never
	// asked DNS. Nothing here inspects a hostname on the wire.
	//
	// The snapshot is not refreshed while the app runs. A domain whose addresses rotate -
	// anything large and CDN-fronted - will drift out of the set, and the app loses access to
	// it until it is restarted. That direction is deliberate: a stale entry stops working
	// rather than quietly allowing whoever holds the address now.
	Domains []string `yaml:"Domains"`

	GatewayV4 string `yaml:"GatewayV4"` // if "" use default
	GatewayV6 string `yaml:"GatewayV6"`

	// Via turns an egress list naming an AppName from "talk TO that app" into "route
	// THROUGH it": the listed CIDRs are sent to that sibling over their private link
	// instead of out this app's own egress. That is how an app is put behind a VPN
	// container without trusting it to route itself - it has no other path to those
	// destinations, so it cannot leak past the sibling, and if the sibling stops, the
	// traffic blackholes rather than falling back.
	//
	// Per list, so one app can pick a different backend per destination: work subnets
	// through one sibling, everything else direct.
	Via bool `yaml:"Via"`

	// Forward is the other half, set on the producer's own ingress list: siblings on my
	// link may route through me, and I will forward and masquerade their traffic out of my
	// own egress. Explicit because forwarding for other apps is a privilege - it makes this
	// app a router - and must never be implied by another app naming it.
	//
	// What a gateway forwards is bounded from BOTH ends, and the two ends answer different
	// questions. WHERE is the client's: only the CIDRs its own Via list names are routed
	// here at all, and it cannot change that - the runner installs those routes and the app
	// has no capability to alter them. WHAT is the gateway's, and that is ForwardPorts.
	//
	// A gateway's own egress rules deliberately do NOT bound what it forwards. They say
	// where this app may go; forwarded traffic is somebody else's, already bounded by
	// whoever sent it.
	Forward bool `yaml:"Forward"`

	// ForwardPorts narrows what a forwarding app will carry to these destination ports.
	// Empty forwards any port, which is what a general-purpose gateway is for; naming them
	// is how a gateway becomes single-purpose - a DNS hop that passes 53 and nothing else,
	// so a client routed through it cannot use it to reach anything but a resolver.
	//
	// Only the ports. The addresses are not repeated here because the client already fixed
	// them by choosing what to route, and stating them twice would let the two disagree.
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

// Readable drop, because u cannot mount something u cannot read
// ConfigFile is one file the app ships with: authored alongside the app, kept in the app's
// own bundle directory, and mounted into the container at launch.
//
// It has its own type rather than borrowing Volume, which is what it did until schema v3.
// Sharing that struct meant three of its six fields were meaningless here - HostMounted was
// documented as ignored, SizeLimited and SizeLimitMiB were validated and could never apply to
// a single file - and, worse, the one field they did share carried opposite rules: a Volume's
// HostMount must be an absolute host path, while a Config's had to be relative and was
// rejected if absolute. One name, one type, two contradictory meanings depending on which
// list the entry happened to sit in.
type ConfigFile struct {
	// BundlePath is relative to this app's bundle: $XDG_CONFIG_HOME/zinc/apps/<app>/configs.
	// Relative on purpose, and enforced: a config is content the app was authored WITH, so it
	// travels with the app. An absolute path here would be a host bind mount wearing a
	// different field's name, and Volumes is where those belong and where they are reviewed.
	BundlePath string `yaml:"BundlePath"`
	InnerMount string `yaml:"InnerMount"`
	// Writable lets the app change its own config and have that survive, which some apps
	// expect. It is opt-in because the default should be that what a reviewer read is what
	// the app keeps running with.
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

// Keys is a convenience layer for SSH/GPG only (section 3): unlike a plain Volume it
// mounts the key read-only into the container home (.ssh for SSH, .gnupg for GPG).
type KeyType string

const (
	SSH KeyType = "SSH"
	GPG KeyType = "GPG"
)

type Key struct {
	Type KeyType `yaml:"Type"`
	Path string  `yaml:"Path"`
}

// AudioMeta grants sound, one direction at a time. It says WHAT an app gets rather than
// HOW it is delivered: the runner picks the transport from the form each direction takes.
//
// Splitting the two directions is the point. A microphone is a different capability from a
// speaker, and until schema v3 one flag granted both: `Pipewire: true` mounted the session
// socket, and PipeWire's default access hands that client capture as well as playback, plus
// the monitor sources that record whatever ANOTHER app is playing. An app that only ever
// makes noise was granted the ability to listen to the room, and the config had no way to
// say otherwise.
type AudioMeta struct {
	Playback   AudioDevice `yaml:"Playback"`
	Microphone AudioDevice `yaml:"Microphone"`
	// Monitor is the capability to record what OTHER apps are playing. A PipeWire sink
	// carries a `.monitor` source, which is a readable tap on everything mixed into it, and a
	// client on the session socket can open one. That is what a screen recorder uses, and it
	// crosses the boundary between two sandboxed apps rather than between an app and a host
	// device: a music player and a video call share a sink.
	//
	// It is a capability on the RECORDER, not a protection on the app being recorded. Zinc can
	// only describe what an app may do, so there is no "my output is private" to write in the
	// player's config: whether anything taps its sink is decided by the other app's grant.
	//
	// `none` is not yet enforced for a container, because mounting the socket grants this
	// whatever the field says; validation says so. `default` is honest and costs nothing, so
	// an app that really does record the desktop can declare it today.
	Monitor AudioDevice `yaml:"Monitor"`
}

// AudioDevice is one direction of audio. Three forms, and they differ in how strongly they
// are enforced, which is why they are spelled differently rather than hidden behind one bool:
//
//	none            not granted (also what an absent field means)
//	default         the session's own device, reached through the PipeWire socket
//	[/dev/snd/...]  exactly these ALSA nodes and nothing else
//
// The list form is the strong one: those device nodes are passed with `--device` and the
// kernel enforces it, so an app given one microphone cannot open a second card. The `default`
// form is convenience, and on a CONTAINER it is not yet enforcement: mounting the PipeWire
// socket grants capture whatever this field says, until Zinc speaks PipeWire's security
// context the way it already speaks Wayland's. Validation says so rather than letting the
// field read like a control it is not. On a VM `default` IS enforced, because the guest is
// given a playback-only sound device unless a microphone was asked for.
type AudioDevice struct {
	Default bool     // the session's own device
	Devices []string // exact /dev/snd nodes; mutually exclusive with Default
}

// IsZero reports the "not granted" state, which is both the absent field and an explicit
// `none`.
func (dev AudioDevice) IsZero() bool { return !dev.Default && len(dev.Devices) == 0 }

// audioNone and audioDefault are the two scalar spellings. Anything else scalar is refused
// by name, so a typo is an error rather than a silent denial.
const (
	audioNone    = "none"
	audioDefault = "default"
)

// UnmarshalYAML accepts the scalar forms and the list form. An absent field never reaches
// here and stays zero, which is the same as `none`.
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

// MarshalYAML writes the state back in the form it was meant to be read in. "not granted" is
// written as an explicit `none` rather than omitted: a config that says `Microphone: none`
// records that the grant was considered and refused, which is the same reason the schema
// writes an explicit `false` for every other denial instead of leaving the key out.
func (dev AudioDevice) MarshalYAML() (any, error) {
	switch {
	case dev.Default:
		return audioDefault, nil
	case len(dev.Devices) > 0:
		return dev.Devices, nil
	}
	return audioNone, nil
}
