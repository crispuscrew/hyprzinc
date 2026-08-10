package validate

import (
	"regexp"
	"sort"
	"strings"
)

// envNameRE is the POSIX environment-name charset. Anything else is not portable and, more to
// the point here, would land in `-e NAME=VALUE` where a '=' or whitespace in the NAME shifts
// what podman reads as the value.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are the variables the runner constructs and exports itself. A config setting one
// would be stating something about the sandbox that the sandbox does not agree with: the
// runtime dir and display are the paths Zinc mounted, and the bus address points at the
// filtered socket a proxy is serving. Overriding them cannot make any of that true, it can
// only make the app look somewhere else and fail in a way that points nowhere near the config.
var reservedEnv = map[string]string{
	"XDG_RUNTIME_DIR":          "the runner sets this to the container's own runtime directory",
	"WAYLAND_DISPLAY":          "the runner sets this to the socket it mounted (DisplayMeta)",
	"DBUS_SESSION_BUS_ADDRESS": "the runner sets this to the filtered bus socket (DBusMeta)",
}

// checkEnv screens the app environment.
func checkEnv(env map[string]string, add addFunc) {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names) // errors in a stable order, whatever the map iteration did
	for _, name := range names {
		switch {
		case !envNameRE.MatchString(name):
			add("Env[%q]: not a usable variable name - letters, digits and underscore, not starting with a digit", name)
		case reservedEnv[name] != "":
			add("Env[%q]: cannot be set here - %s, and overriding it points the app at something that is not there", name, reservedEnv[name])
		}
		if value := env[name]; hasControl(value) {
			add("Env[%q]: the value must not contain control characters", name)
		} else if strings.Contains(value, "\n") {
			add("Env[%q]: the value must be a single line", name)
		}
	}
}
