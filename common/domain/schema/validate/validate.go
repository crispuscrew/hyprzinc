package validate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// Validate checks an AppConfig against the hard rules. Pure (no I/O), so zc (save)
// and zcr (launch) judge identically; all problems are joined, not just the first.
func Validate(cfg schema.AppConfig) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	checkIdentity(cfg, add)
	checkLifecycle(cfg, add)
	checkInstall(cfg.ImageMeta.Install, add)
	checkDepends(cfg.StartConditions.DependsOn, add)
	checkReadiness(cfg.StartConditions, add)

	// The two app types share their identity, lifecycle and setup rules above and diverge
	// below: each rejects what the other's runtime cannot honour, so a field is never
	// accepted into a config where it would do nothing.
	if cfg.Type == schema.ZincVirtualization {
		checkVirtualization(cfg, add)
		checkContainerOnlyFields(cfg, add)
		return errors.Join(errs...)
	}

	checkVirtualizationUnset(cfg, add)
	checkContainerImage(cfg.ImageMeta.Image, add)
	checkResources(cfg.ResourcesMeta, add)
	checkInternalUser(cfg.InternalUserMeta, add)
	checkNotifications(cfg.NotificationMeta, add)
	checkDBus(cfg, add)
	checkSourceTag(cfg.ImageMeta.SourceTag, add)

	checkDNS(cfg.NetworkMeta, add)
	checkTunnel(cfg, add)
	for index, netList := range cfg.NetworkMeta.NetworkLists {
		checkNetworkList(index, netList, add)
	}
	for index, volume := range cfg.Volumes {
		checkVolume(index, volume, add)
	}
	for index, configMount := range cfg.Configs {
		checkConfig(index, configMount, add)
	}
	checkKeys(cfg.Keys, add)
	checkCapabilities(cfg.Capabilities, add)
	checkNetworkCapabilities(cfg, add)

	return errors.Join(errs...)
}

// checkInstall screens each ImageMeta.Install step. The steps are joined into the one
// RUN layer of the derived-image Containerfile (FROM ImageMeta.Image + RUN ...), so a
// control character - above all a newline - would break out of that single RUN line
// and let a crafted config inject its own Containerfile directives (e.g. a second FROM
// that swaps the base to an unpinned image, defeating the digest pin while the YAML's
// Image still looks pinned). A legitimate multi-step setup uses one list entry per
// step; a single step never needs an embedded newline (section 5.5).
func checkInstall(install []string, add addFunc) {
	for index, step := range install {
		if hasControl(step) {
			add("ImageMeta.Install[%d]: must not contain control characters (a newline would inject extra Containerfile directives); put each setup step in its own list entry (section 5.5)", index)
		}
	}
}

// checkDepends screens each StartConditions.DependsOn name. A dependency name is used
// verbatim to locate its app file on disk (the store joins it into a path), so it must
// be a safe object name - the same charset as AppNameID - or a "../.." value could
// read and launch a definition from outside the apps directory (section 6.6).
func checkDepends(dependsOn []string, add addFunc) {
	for index, dep := range dependsOn {
		if strings.TrimSpace(dep) == "" {
			add("StartConditions.DependsOn[%d]: must not be empty", index)
			continue
		}
		if !nameRE.MatchString(dep) {
			add("StartConditions.DependsOn[%d] %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", index, dep)
		}
	}
}

// maxReadyTimeoutSec is a day. Anything beyond it is a typo rather than a wait, and the
// nanosecond duration the runner builds from it must not overflow.
const maxReadyTimeoutSec = 86400

// checkReadiness screens the readiness gate a dependent waits on. ReadyCheck is exec form
// and reaches podman as one JSON argument, so it needs no quoting rules - only that every
// word is a word. A timeout on its own is the inert-field case: nothing waits without a
// probe to wait for, so the number would read as a bound that is not in force.
func checkReadiness(start schema.StartConditions, add addFunc) {
	for index, arg := range start.ReadyCheck {
		if strings.TrimSpace(arg) == "" {
			add("StartConditions.ReadyCheck[%d]: must not be empty", index)
		}
	}
	// Bounded at both ends. The upper bound is not tidiness: the runner turns this into a
	// time.Duration in nanoseconds, and a large enough value overflows to a NEGATIVE duration,
	// so the deadline is already past and the wait would end on the first failed probe - a
	// very long timeout silently becoming no timeout at all.
	if start.ReadyTimeoutSec < 0 || start.ReadyTimeoutSec > maxReadyTimeoutSec {
		add("StartConditions.ReadyTimeoutSec %d: must be between 0 and %d (0 = the runner's default)", start.ReadyTimeoutSec, maxReadyTimeoutSec)
	}
	if start.ReadyTimeoutSec > 0 && len(start.ReadyCheck) == 0 {
		add("StartConditions.ReadyTimeoutSec %d: has no effect without ReadyCheck - with no probe, a dependency counts as ready once it is running and nothing waits", start.ReadyTimeoutSec)
	}
}

// checkNetworkCapabilities forbids network-administration capabilities on a filtered
// app. A filtered app (one with NetworkLists) runs inside the pod whose network
// namespace carries the nftables egress lock-down; granting CAP_NET_ADMIN (or the
// superset CAP_SYS_ADMIN) would let the app flush that ruleset at runtime and reach an
// unfiltered network. Enforcement and app-granted netns control are mutually exclusive
// by design (section 5.3).
func checkNetworkCapabilities(cfg schema.AppConfig, add addFunc) {
	if len(cfg.NetworkMeta.NetworkLists) == 0 {
		return // unfiltered app runs with --network none; NET_ADMIN reaches nothing
	}
	for index, capability := range cfg.Capabilities {
		switch strings.TrimPrefix(strings.ToUpper(capability), "CAP_") {
		case "NET_ADMIN", "SYS_ADMIN":
			add("Capabilities[%d] %q: cannot be granted to an app with NetworkLists - it could flush the egress lock-down in the pod netns and escape the network filter (section 5.3)", index, capability)
		}
	}
}

// checkIdentity: schema version, Type, AppNameID, the digest-pinned image, user name.
func checkIdentity(cfg schema.AppConfig, add addFunc) {
	if cfg.SchemaVersion != schema.SchemaVersion {
		add("SchemaVersion: got %d, want %d", cfg.SchemaVersion, schema.SchemaVersion)
	}

	switch cfg.Type {
	case schema.ZincContainer, schema.ZincVirtualization:
	case "":
		add("Type: must be set (%s or %s)", schema.ZincContainer, schema.ZincVirtualization)
	default:
		add("Type %q: must be %s or %s", cfg.Type, schema.ZincContainer, schema.ZincVirtualization)
	}

	switch {
	case strings.TrimSpace(cfg.AppNameID) == "":
		add("AppNameID: must not be empty")
	case !nameRE.MatchString(cfg.AppNameID):
		add("AppNameID %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", cfg.AppNameID)
	}

	// Inherits is joined into a store path to find the base, so it is held to the same
	// charset as the names above - a "../.." value would read a config from outside the apps
	// directory. The resolver enforces this too, since it is the one doing the join; this is
	// the copy that tells an author at save time rather than at launch.
	if base := strings.TrimSpace(cfg.Inherits); base != "" {
		switch {
		case !nameRE.MatchString(base):
			add("Inherits %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", base)
		case base == strings.TrimSpace(cfg.AppNameID):
			add("Inherits %q: an app cannot inherit from itself", base)
		}
	}

	// NonRootUserName becomes `podman --user`; keep it a safe charset.
	if name := cfg.InternalUserMeta.NonRootUserName; name != "" && !nameRE.MatchString(name) {
		add("InternalUserMeta.NonRootUserName %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", name)
	}
}

// checkContainerImage screens a container app's image reference. A VM app's Image is a
// path to a base disk instead, pinned by its own file digest, so it is screened by
// checkBaseImage rather than here.
func checkContainerImage(image string, add addFunc) {
	switch {
	case strings.TrimSpace(image) == "":
		add("ImageMeta.Image: must not be empty")
	case hasUnsafe(image):
		// Interpolated into a FROM line - must be a single-line ref (section 5.5).
		add("ImageMeta.Image %q: must be a single-line reference (no whitespace or control characters)", image)
	case !LocalImage(image) && !digestRE.MatchString(image):
		// section 5.5: third-party images pinned by canonical digest; only localhost/ may use a mutable tag.
		add("ImageMeta.Image %q: third-party images must be digest-pinned (...@sha256:<64 hex>); only localhost/ images may use a mutable tag (section 5.5)", image)
	}
}

// checkLifecycle: terminal / multiterminal / background interplay (section 9.1).
func checkLifecycle(cfg schema.AppConfig, add addFunc) {
	start := cfg.StartConditions
	switch {
	case start.Multiterminal && !start.Terminal:
		add("StartConditions.Multiterminal: requires Terminal (it spawns terminals into a shared container)")
	case start.Terminal && cfg.StopConditions.Background && !start.Multiterminal:
		// A foreground terminal app can't also be Background; Multiterminal lifts this.
		add("StartConditions.Terminal: a terminal app runs in a foreground window; it cannot also be StopConditions.Background (use Multiterminal to keep the shared container alive after the last terminal closes)")
	}
	if start.Multiterminal && strings.TrimSpace(start.Entrypoint) == "" && strings.TrimSpace(start.MultiterminalEntrypoint) == "" {
		// Each terminal re-execs the app, so it needs an explicit command (PID 1 is a holder).
		add("StartConditions: Multiterminal needs an explicit Entrypoint or MultiterminalEntrypoint (the image default cannot be replayed into each terminal)")
	}
}

// checkResources: caps are >= 0 (0 = unlimited for CPU/RAM/PIDs).
func checkResources(res schema.ResourcesMeta, add addFunc) {
	if res.MaxCPUCores < 0 {
		add("ResourcesMeta.MaxCPUCores %v: must be >= 0 (0 = unlimited)", res.MaxCPUCores)
	}
	if res.MaxRamMiB < 0 {
		add("ResourcesMeta.MaxRamMiB %d: must be >= 0 (0 = unlimited)", res.MaxRamMiB)
	}
	if res.MaxSwapMiB < 0 {
		add("ResourcesMeta.MaxSwapMiB %d: must be >= 0", res.MaxSwapMiB)
	}
	if res.PIDsLimit < 0 {
		add("ResourcesMeta.PIDsLimit %d: must be >= 0 (0 = unlimited)", res.PIDsLimit)
	}
	// A swap allowance on its own has no meaning to enforce. The runtime tells podman the
	// TOTAL of memory and swap, because that is the only figure podman takes, and with no
	// memory limit there is no total to state - podman would either refuse the flag or read
	// the swap figure as the app's whole memory ceiling, which is the opposite of what
	// asking for swap means. Naming the missing field beats either.
	if res.MaxSwapMiB > 0 && res.MaxRamMiB <= 0 {
		add("ResourcesMeta.MaxSwapMiB %d: needs MaxRamMiB set too - swap is allowed on top of the memory limit, so without one there is nothing to add it to",
			res.MaxSwapMiB)
	}
}

// checkInternalUser screens who the app runs as. Both halves have to agree: the runtime
// passes the name to podman, so asking for a non-root user without naming one leaves
// nothing to pass, and naming one without asking leaves a field that reads as if it were in
// force. Either way the config would say something the launch does not do, which is the
// whole reason these fields were worth wiring up.
func checkInternalUser(user schema.InternalUserMeta, add addFunc) {
	if user.UseNonRootUser && strings.TrimSpace(user.NonRootUserName) == "" {
		add("InternalUserMeta.UseNonRootUser: set NonRootUserName too - the user is passed to podman by name, and it must exist in the image")
	}
	if !user.UseNonRootUser && strings.TrimSpace(user.NonRootUserName) != "" {
		add("InternalUserMeta.NonRootUserName %q: has no effect without UseNonRootUser", user.NonRootUserName)
	}
}

// checkNotifications fails closed on a block that is defined and does nothing. Zinc has no
// notification path yet - nothing proxies, silences or prefixes an app's notifications - so
// every field here is inert. Silently accepting Silenced would tell an author their app is
// muted while it notifies freely, and the honest answer for an unimplemented mechanism is to
// refuse the config rather than mis-enforce it. The zero value stays legal, so an app that
// never touched the block is unaffected.
func checkNotifications(notify schema.NotificationMeta, add addFunc) {
	if notify == (schema.NotificationMeta{}) {
		return
	}
	add("NotificationMeta: not implemented - Zinc does not proxy or filter app notifications yet, so none of these fields would be enforced; leave the block at its defaults")
}

// Warnings returns non-fatal create-time advisories (zc); nothing here blocks save or
// launch - it flags valid-but-risky or probably-unintended configs. Exposing inbound
// (an Ingress list) is always surfaced, loudest when it reaches the LAN.
func Warnings(cfg schema.AppConfig) []string {
	var warns []string
	if cfg.Type == schema.ZincVirtualization && cfg.VirtualizationMeta.Vulkan {
		// Worth saying out loud on a security tool: this is the one VM setting that removes
		// a boundary rather than adding one.
		warns = append(warns,
			"VirtualizationMeta.Vulkan: guest Vulkan requires disabling qemu's seccomp sandbox for this app, "+
				"because the venus renderer runs in a helper process the sandbox would kill. The guest gains "+
				"GPU-accelerated Vulkan; the qemu process loses its syscall filter.")
	}
	if cfg.Type == schema.ZincVirtualization && virtioGpuOnACompatibleGuest(cfg.VirtualizationMeta) {
		// Measured on a Windows 11 guest: the firmware paints on a virtio-gpu, and then the
		// screen goes black the moment the OS takes over, because a guest with no virtio-gpu
		// driver leaves the device with no active scanout. Nothing errors and nothing is
		// logged - the window just says the display output is not active. Devices:
		// Compatible is the app saying this guest has no virtio drivers, so pairing it with
		// a virtio display is worth a word at authoring time rather than a black screen at
		// the first boot. Not an error: once the driver IS installed this is the right
		// machine, and it is how a guest stops being stuck at a fixed screen size.
		warns = append(warns,
			"VirtualizationMeta: Display "+string(cfg.VirtualizationMeta.Display)+" gives the guest a virtio-gpu, "+
				"but Devices: Compatible says the guest has no virtio drivers. Unless one is installed inside it "+
				"(viogpudo, from the virtio-win disc), the screen goes black as soon as the OS starts. "+
				"Display: Compatible is the driverless choice.")
	}
	if cfg.Type == schema.ZincContainer && !cfg.DisplayMeta.DisableGpuAccess {
		// The only grant in the schema whose zero value is the permissive one: absent means
		// /dev/dri is bind-mounted. Every other grant defaults closed (no NetworkLists means
		// --network none, empty DBusMeta means no bus at all, no Capabilities means
		// --cap-drop all), so a reviewer scanning a config for what it was given will not
		// see this one. Say it out loud until the field can be made opt-in, which changes
		// the meaning of existing configs and so needs a SchemaVersion bump.
		warns = append(warns,
			"DisplayMeta: GPU access is on because DisableGpuAccess is not set. Unlike every other grant, "+
				"this one defaults to allowed: the app gets /dev/dri even if it never renders. "+
				"Set DisableGpuAccess: true unless this app needs the GPU.")
	}
	warns = append(warns, dbusWarnings(cfg.DBusMeta)...)
	for index, netList := range cfg.NetworkMeta.NetworkLists {
		if netList.Ingress {
			warns = append(warns, ingressWarnings(index, netList)...)
			continue
		}
		// Egress: an empty blacklist blocks nothing (allow-all) - worth surfacing on a
		// security tool.
		if netList.Blacklist &&
			len(netList.IPv4CIDR) == 0 && len(netList.IPv6CIDR) == 0 && len(netList.Ports) == 0 {
			warns = append(warns, fmt.Sprintf(
				"NetworkLists[%d]: egress blacklist with no CIDRs/ports blocks nothing (allow-all)", index))
		}
	}
	return warns
}

// virtioGpuOnACompatibleGuest reports the one display/device pairing that produces a picture
// from the firmware and nothing from the OS. Window and Accelerated are both virtio-gpu; None
// has no display at all, and Compatible is the driverless choice this warning points at.
func virtioGpuOnACompatibleGuest(virt schema.VirtualizationMeta) bool {
	if virt.Devices != schema.VMDevicesCompatible {
		return false
	}
	return virt.Display == schema.VMDisplayWindow || virt.Display == schema.VMDisplayAccelerated
}

// ingressWarnings surfaces one published-port (Ingress) list: inbound exposure always
// gets a notice, and the loud form when it reaches the LAN (Host) or opens every port
// (an ingress blacklist = default-accept inbound). A ports-less ingress list exposes
// nothing and most likely means the author forgot Ports.
func ingressWarnings(index int, netList schema.NetworkList) []string {
	scope := "apps that join this app's network"
	if netList.Host {
		iface := strings.TrimSpace(netList.Interface)
		if iface == "" {
			iface = "all host interfaces"
		}
		scope = fmt.Sprintf("the LAN via %s", iface)
	}
	switch {
	case netList.Blacklist:
		return []string{fmt.Sprintf(
			"NetworkLists[%d]: ingress blacklist exposes ALL inbound ports (default-accept) to %s", index, scope)}
	case len(netList.Ports) > 0:
		return []string{fmt.Sprintf(
			"NetworkLists[%d]: ingress exposes port(s) %s to %s", index, joinPorts(netList.Ports), scope)}
	default:
		return []string{fmt.Sprintf(
			"NetworkLists[%d]: ingress list exposes no ports (Ports is empty) - did you forget Ports? (%s)", index, scope)}
	}
}
