package validate

import (
	"regexp"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// busNameRE is one D-Bus well-known name: two or more dot-separated elements, each starting with a
// letter, underscore or hyphen. Anchored, and deliberately narrow: these are passed to
// xdg-dbus-proxy as --talk/--own arguments, so a name carrying a space or its own "--" would splice
// an option into the filter confining the app (section 5.5, applied to the bus).
var busNameRE = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]*(\.[A-Za-z_-][A-Za-z0-9_-]*)+$`)

// maxBusName is the bus-name length cap from the D-Bus specification.
const maxBusName = 255

// checkDBus screens DBusMeta. An empty block is the fail-closed default and has nothing to check.
//
// The KeepUserID requirement is the non-obvious rule: xdg-dbus-proxy serves the socket as the
// invoking host user, and an app in another user namespace cannot connect - surfacing inside the app
// as a bare "connection refused". Zinc refuses the config and names the key rather than silently
// changing who the app runs as on the strength of an unrelated field.
func checkDBus(cfg schema.AppConfig, add addFunc) {
	bus := cfg.DBusMeta
	if bus.IsZero() {
		return
	}
	if !cfg.InternalUserMeta.KeepUserID {
		add("DBusMeta: a filtered bus needs InternalUserMeta.KeepUserID: true - the proxy serves the socket as the host user and an app in its own user namespace cannot connect to it; set KeepUserID rather than have Zinc change who the app runs as on the strength of a bus grant")
	}
	for index, name := range bus.Talk {
		checkBusName("Talk", index, name, true, add)
	}
	for index, name := range bus.Own {
		checkBusName("Own", index, name, false, add)
	}
}

// checkBusName screens one Talk/Own entry. allowWildcard separates the two fields: a
// trailing ".*" is a meaningful (if broad) thing to be allowed to CALL, and meaningless as
// something to own, since a process claims one concrete name or none.
func checkBusName(field string, index int, name string, allowWildcard bool, add addFunc) {
	trimmed := strings.TrimSpace(name)
	base := trimmed
	wildcard := strings.HasSuffix(trimmed, ".*")
	if wildcard {
		base = strings.TrimSuffix(trimmed, ".*")
	}
	switch {
	case trimmed == "":
		add("DBusMeta.%s[%d]: must not be empty", field, index)
	case trimmed != name:
		add("DBusMeta.%s[%d]: %q has leading or trailing whitespace", field, index, name)
	case len(trimmed) > maxBusName:
		add("DBusMeta.%s[%d]: %q is longer than the %d-character bus-name limit", field, index, name, maxBusName)
	case wildcard && !allowWildcard:
		add("DBusMeta.%s[%d]: %q - a subtree wildcard cannot be owned, since a process claims one concrete name or none", field, index, name)
	case !busNameRE.MatchString(base):
		add("DBusMeta.%s[%d]: %q is not a well-known bus name - two or more dot-separated elements of [A-Za-z0-9_-], no element starting with a digit", field, index, name)
	case wildcard && strings.Count(base, ".") < 2:
		// A wildcard grants the whole subtree, including services that appear there later. With a two-element
		// base that is an entire vendor namespace: "org.freedesktop.*" covers systemd1, whose
		// StartTransientUnit runs an arbitrary command as the user OUTSIDE the container, plus the keyring
		// and every portal. So a wildcard must name something more specific than a vendor prefix.
		add("DBusMeta.%s[%d]: %q grants an entire vendor namespace - a wildcard must name at least three elements before the '*' (org.freedesktop.portal.*, not org.freedesktop.*), because the subtree includes services that appear under it later and org.freedesktop.systemd1 alone is a way out of the sandbox", field, index, name)
	}
}

// escapeNames are bus services that hand a caller code execution outside the container. Naming one is
// legal and occasionally meant, but it is said out loud at authoring time. Short on purpose: only
// names where the escape is the service's advertised purpose. A prompt, not a boundary - the boundary
// is that no wildcard can sweep these up by accident (see checkBusName).
var escapeNames = map[string]string{
	"org.freedesktop.systemd1":                    "StartTransientUnit runs an arbitrary command as the user, outside the container",
	"org.freedesktop.Flatpak":                     "Spawn runs an arbitrary command on the host",
	"org.freedesktop.secrets":                     "the login keyring: every stored secret the user has",
	"org.freedesktop.impl.portal.PermissionStore": "the backing store for portal permissions, so an app can grant itself portal access",
}

// dbusWarnings surfaces grants that are valid, deliberate-looking, and much wider than they
// read. Validation already refuses a vendor-wide wildcard; what is left is worth a word.
func dbusWarnings(bus schema.DBusMeta) []string {
	if bus.IsZero() {
		return nil
	}
	var warns []string
	for _, name := range bus.Talk {
		trimmed := strings.TrimSpace(name)
		if why, ok := escapeNames[trimmed]; ok {
			warns = append(warns, "DBusMeta.Talk: "+trimmed+" is a way out of the sandbox - "+why)
			continue
		}
		if strings.HasSuffix(trimmed, ".*") {
			warns = append(warns, "DBusMeta.Talk: "+trimmed+
				" is a subtree, so it also grants every service that appears under it later, including ones that do not exist yet")
		}
	}
	for _, name := range bus.Own {
		trimmed := strings.TrimSpace(name)
		if _, ok := wellKnownOwners[trimmed]; ok {
			warns = append(warns, "DBusMeta.Own: "+trimmed+
				" is a name the desktop's own service normally claims - if this app wins the race it receives what was meant for that service")
		}
	}
	return warns
}

// wellKnownOwners are names a desktop service is expected to own. An app owning one is
// impersonation if it gets there first, which is a claim worth surfacing.
var wellKnownOwners = map[string]struct{}{
	"org.freedesktop.Notifications":      {},
	"org.freedesktop.secrets":            {},
	"org.freedesktop.ScreenSaver":        {},
	"org.freedesktop.FileManager1":       {},
	"org.mpris.MediaPlayer2":             {},
	"org.freedesktop.impl.portal.Access": {},
}

// checkSourceTag screens SourceTag. It is provenance, but it is handed to a registry client on a
// staleness check, so it gets the same treatment as any reference: no whitespace or control
// characters. A digest is refused because re-resolving one returns itself and reports "never stale".
func checkSourceTag(tag string, add addFunc) {
	trimmed := strings.TrimSpace(tag)
	switch {
	case trimmed == "":
		return // absent is fine: a hand-pinned digest has no known origin
	case trimmed != tag || hasUnsafe(tag):
		add("ImageMeta.SourceTag: %q must not contain whitespace or control characters", tag)
	case strings.Contains(tag, "@sha256:"):
		// Deliberately looser than digestRE, which is anchored so an IMAGE reference cannot
		// begin with something podman would read as a flag. Here the question is only
		// "does this record a digest", so any occurrence counts.
		add("ImageMeta.SourceTag: %q is a digest, not a tag - re-resolving it would return itself and report the pin as never stale; record the tag it came from, or leave this empty", tag)
	}
}
