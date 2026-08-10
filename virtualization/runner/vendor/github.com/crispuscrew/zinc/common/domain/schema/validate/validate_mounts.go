package validate

import (
	"path/filepath"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// Validates the ':'-delimited podman mount specs - Volumes, Configs, Keys - plus
// Capabilities. host:container:opts is ':'-split, so ':'/','/whitespace in a path
// shifts podman's fields (e.g. claim "ro" but mount "rw"); every path is screened.

// checkVolume: container path, host source (when HostMounted), size-limit sanity.
func checkVolume(index int, volume schema.Volume, add addFunc) {
	checkInner("Volumes", index, volume.InnerMount, add)
	if volume.HostMounted {
		switch {
		case strings.TrimSpace(volume.HostMount) == "":
			add("Volumes[%d].HostMount: required when HostMounted=true", index)
		case hasUnsafe(volume.HostMount) || strings.ContainsAny(volume.HostMount, ":,"):
			add("Volumes[%d].HostMount %q: must not contain ':', ',', or whitespace (it shifts podman's -v fields)", index, volume.HostMount)
		default:
			checkHostSource("Volumes", index, volume.HostMount, add)
		}
	}
	checkSizeLimit("Volumes", index, volume, add)
}

// brokeredPrefixes are host paths a mount must never name, because Zinc's whole job is to
// hand the app a filtered stand-in for what lives there. $XDG_RUNTIME_DIR holds the session
// bus socket, the compositor socket, and the per-app sockets of every OTHER Zinc app; /proc
// and /sys are host state a sandbox has no business reading wholesale.
//
// The list is literal paths rather than $XDG_RUNTIME_DIR, because this package is pure and
// cannot read the environment. /run/user is where a rootless session puts it on every system
// this targets; a session pointed elsewhere is not covered, which is worth knowing.
//
// This is the one place the "an explicit mount is an explicit grant" principle does not
// apply. A config naming the raw bus reads as an ordinary directory mount, and afterwards
// every surface Zinc offers still reports the app as having no bus: `zcr where` says
// bus: none, `zcr bus` shows no row, and the container is still labelled with a Wayland
// security context it is not using. The grant is invisible in exactly the place a reviewer
// would look, so it is refused instead of trusted to be noticed.
var brokeredPrefixes = []string{"/run/user", "/proc", "/sys", "/dev"}

// checkHostSource applies the host-path policy shared by every host-side mount source.
func checkHostSource(list string, index int, source string, add addFunc) {
	// Absolute only. Podman resolves a relative source against ITS OWN working directory,
	// which is wherever zcr was invoked from (a hotkey, a menu, a TUI), so the same config
	// mounts a different directory depending on the caller. Worse, podman reads a source
	// with no separator at all as the name of a NAMED VOLUME and creates it, so
	// "HostMount: Downloads" silently becomes a fresh empty volume while the config still
	// says HostMounted: true. Neither is something a reviewer can see.
	if !strings.HasPrefix(source, "/") {
		add("%s[%d].HostMount %q: must be an absolute path - podman resolves a relative source against the directory zcr happened to be started in, and a source with no '/' at all becomes a named volume it creates rather than the host path this names", list, index, source)
		return
	}
	if hasDotDot(source) {
		add("%s[%d].HostMount %q: must not contain '..' segments - the path that gets mounted should be the path that was reviewed", list, index, source)
		return
	}
	// Clean first, then compare SEGMENTS. A raw prefix test on the written path is bypassed by
	// any spelling the kernel resolves to the same place: "//proc", "/./proc",
	// "/run//user/1000" and "/run/./user/1000" all reach the directory the rule forbids while
	// failing a HasPrefix against "/proc" or "/run/user". It also refuses things it should not,
	// since "/sys" as a raw prefix matches "/sysroot/home/me", which is the real root on an
	// ostree system.
	cleaned := filepath.Clean(source)
	for _, prefix := range brokeredPrefixes {
		if cleaned != prefix && !strings.HasPrefix(cleaned, prefix+"/") {
			continue
		}
		add("%s[%d].HostMount %q: %s is host state Zinc brokers or grants by other means (the session bus and compositor sockets under /run/user, the devices behind DisplayMeta and AudioMeta, /proc and /sys); mounting it hands the app the capability directly while every Zinc report still says it has none. Ask for the capability instead: DBusMeta for the bus, DisplayMeta for the display and GPU, AudioMeta for sound devices", list, index, source, prefix)
		return
	}
}

// checkConfig screens one ConfigFile. The source is bundle-relative by design, so the rules
// are the mirror image of a Volume's: a Volume must be absolute, a Config must not be.
func checkConfig(index int, configFile schema.ConfigFile, add addFunc) {
	checkInner("Configs", index, configFile.InnerMount, add)
	source := configFile.BundlePath
	switch {
	case strings.TrimSpace(source) == "":
		add("Configs[%d].BundlePath: must name a file under the app's bundle (apps/<app>/configs/)", index)
	case hasUnsafe(source) || strings.ContainsAny(source, ":,"):
		add("Configs[%d].BundlePath %q: must not contain ':', ',', or whitespace (it shifts podman's -v fields)", index, source)
	case strings.HasPrefix(source, "/"):
		add("Configs[%d].BundlePath %q: must be relative to the app's bundle, not an absolute path - an absolute host path is a Volume, and that is where one gets reviewed", index, source)
	case hasDotDot(source):
		add("Configs[%d].BundlePath %q: must not escape the bundle (no '..' segments)", index, source)
	case strings.Contains(source, "{"):
		// The placeholders name runtime state, and a bundle path names authored content. The
		// two were wired together before v3 and could not both hold: expansion produced an
		// absolute state path, which this same function then rejected for being absolute.
		add("Configs[%d].BundlePath %q: placeholders are for runtime paths (Volumes), not for a bundle file, which ships with the app", index, source)
	}
}

// checkKeys: known Type + field-shift-safe Path (mounted path:dest:ro).
func checkKeys(keys []schema.Key, add addFunc) {
	for index, keyEntry := range keys {
		switch keyEntry.Type {
		case schema.SSH, schema.GPG:
		case "":
			add("Keys[%d].Type: must be set (SSH|GPG)", index)
		default:
			add("Keys[%d].Type %q: must be SSH|GPG", index, keyEntry.Type)
		}
		switch {
		case strings.TrimSpace(keyEntry.Path) == "":
			add("Keys[%d].Path: must not be empty", index)
		case hasUnsafe(keyEntry.Path) || strings.ContainsAny(keyEntry.Path, ":,"):
			add("Keys[%d].Path %q: must not contain ':', ',', or whitespace (it shifts podman's -v fields)", index, keyEntry.Path)
		case !strings.HasPrefix(keyEntry.Path, "/"):
			// A Key promises a narrow thing: this one file, read-only, inside the container
			// home. "~" is the shell's, not podman's, so it is taken literally.
			add("Keys[%d].Path %q: must be an absolute path ('~' is not expanded, and a relative path resolves against wherever zcr was started)", index, keyEntry.Path)
		case hasDotDot(keyEntry.Path):
			// The mount destination is filepath.Join(home, dir, filepath.Base(Path)), and
			// Base("/..") is "/", so a trailing ".." collapses the destination onto the
			// container home itself: "Path: /.." mounts the entire host filesystem over it.
			add("Keys[%d].Path %q: must not contain '..' segments - the destination is derived from the path's last element, so '..' mounts the source over the container home instead of into it", index, keyEntry.Path)
		default:
			// A Key is a host bind mount like any other, so it gets the same host-path policy.
			// Without this it was the way around that policy: "Path: /run/user/1000/bus" is
			// absolute, has no '..' and no field-shifting character, and mounts the unfiltered
			// session bus into the container home as a connectable socket, while DBusMeta stays
			// empty and every Zinc report says the app has no bus.
			checkHostSource("Keys", index, keyEntry.Path, add)
		}
	}
}

// checkCapabilities: ALL is forbidden; each entry must match the capability charset.
func checkCapabilities(capabilities []string, add addFunc) {
	for index, capability := range capabilities {
		bare := strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
		switch {
		case bare == "ALL":
			add("Capabilities[%d] %q: granting ALL capabilities is forbidden (add only the specific caps an app needs)", index, capability)
		case !capRE.MatchString(capability):
			add("Capabilities[%d] %q: only an (optional) CAP_ prefix then [A-Z_] allowed (e.g. NET_ADMIN or CAP_NET_ADMIN)", index, capability)
		}
	}
}

// checkInner: container-side mount path - non-empty, no ':'/','/whitespace.
func checkInner(list string, index int, inner string, add addFunc) {
	switch {
	case strings.TrimSpace(inner) == "":
		add("%s[%d].InnerMount: must not be empty", list, index)
	case hasUnsafe(inner) || strings.ContainsAny(inner, ":,"):
		add("%s[%d].InnerMount %q: must not contain ':', ',', or whitespace (it shifts podman's -v fields)", list, index, inner)
	}
}

// checkSizeLimit: reject SizeLimited with non-positive MiB (a no-op that reads as applied).
func checkSizeLimit(list string, index int, volume schema.Volume, add addFunc) {
	if volume.SizeLimited && volume.SizeLimitMiB <= 0 {
		add("%s[%d]: SizeLimited is set but SizeLimitMiB is %d (must be > 0, or clear SizeLimited)", list, index, volume.SizeLimitMiB)
	}
}
