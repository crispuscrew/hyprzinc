package compose

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// App is one imported service: the app definition it became, and the notes about what the
// compose file said that did not survive the crossing.
type App struct {
	// Service is the compose key this came from, kept because it is what a user has in front
	// of them when they name one with --service; Config.AppNameID may have been coerced.
	Service string
	Config  schema.AppConfig
	Notes   []string
}

// ToApps turns a compose project into one app definition per service, in a stable order. Pure: it
// does not pin an image, since that needs a registry.
//
// compose describes what a service MAY do and has no way to say a service should not have full
// network access, so reading that as "this app wants full network access" would import a posture
// nobody chose. The import is fail-closed: no NetworkLists, which is no network, and the notes say
// so. Published ports are the exception, being stated rather than inferred.
func ToApps(project Project) []App {
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	slices.Sort(names)

	apps := make([]App, 0, len(names))
	for _, name := range names {
		apps = append(apps, serviceToApp(name, project))
	}
	return apps
}

func serviceToApp(name string, project Project) App {
	service := project.Services[name]
	var notes []string
	note := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }

	cfg := schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     appName(name),
		Description:   service.Labels["zinc.description"],
	}
	if cfg.AppNameID != name {
		note("service %q was renamed to %q: an app name is lowercase [a-z0-9._-] and starts alphanumeric", name, cfg.AppNameID)
	}
	cfg.ImageMeta.Image = service.Image

	head, argv := entrypoint(service)
	cfg.StartConditions.Entrypoint = head
	if len(service.Command) > 0 && len(service.Entrypoint) > 0 {
		note("both entrypoint and command were set; Zinc has one Entrypoint, so command %q was dropped", strings.Join(service.Command, " "))
	}
	if len(argv) > 1 {
		note("entrypoint %q was reduced to %q: Zinc's Entrypoint is ONE executable, so its arguments were dropped. The app will not behave as the compose file did until they are baked into the image or a wrapper script.",
			strings.Join(argv, " "), head)
	}
	if len(service.Entrypoint) == 0 && len(service.Command) > 0 {
		note("command %q became the Entrypoint. In compose, `command` replaces the image's CMD and its ENTRYPOINT still runs; here it replaces the ENTRYPOINT. If this image has a setup entrypoint (postgres, redis, mysql and friends all do), it will no longer run.",
			strings.Join(service.Command, " "))
	}
	// Only on-failure restarts come across. compose's "always" and "unless-stopped" restart
	// a container the user stopped on purpose, which is the one thing Autorestart is
	// documented not to do.
	switch service.Restart {
	case "on-failure":
		cfg.StartConditions.Autorestart = true
	case "always", "unless-stopped":
		cfg.StartConditions.Autorestart = true
		note("restart: %s became Autorestart, which restarts only on failure - a clean exit or a manual stop stays stopped", service.Restart)
	}

	for _, dep := range service.DependsOn.Names() {
		cfg.StartConditions.DependsOn = append(cfg.StartConditions.DependsOn, appName(dep))
	}
	if probe, ok := readyCheck(service.Healthcheck); ok {
		cfg.StartConditions.ReadyCheck = probe
	} else if service.Healthcheck != nil && len(service.Healthcheck.Test) > 0 {
		note("healthcheck %q was not imported: Zinc runs a ReadyCheck as argv, and this one is %s",
			strings.Join(service.Healthcheck.Test, " "), healthcheckKind(service.Healthcheck.Test))
	}
	// A dependent that waits on service_healthy needs the DEPENDENCY to carry the probe,
	// and in compose the probe is on the dependency too - so this only has to notice when
	// the file asks for a wait the dependency cannot satisfy.
	for _, dep := range service.DependsOn.Names() {
		if service.DependsOn[dep].Condition != ConditionHealthy {
			continue
		}
		if target, ok := project.Services[dep]; !ok || !hasImportableProbe(target.Healthcheck) {
			note("depends_on %s: service_healthy, but %q defines no healthcheck Zinc can run, so the wait is for it to be running", dep, dep)
		}
	}

	if service.User != "" {
		// The user half is taken before the numeric test, because `1000:1000` is at least as
		// common as a bare `1000` and means the same thing - testing the whole string let the
		// spelling this refuses walk straight through as a "name" of "1000".
		userName, _, hadGroup := strings.Cut(service.User, ":")
		switch {
		case strings.TrimSpace(userName) == "":
			note("user: %s was dropped: it names no user, only a group, and Zinc has no field for a group on its own", service.User)
		case isNumeric(userName):
			note("user: %s was dropped: Zinc passes the user to podman BY NAME, so it must exist in the image; a numeric id would always 'work' and could land on a user the image does not have", service.User)
		default:
			cfg.InternalUserMeta = schema.InternalUserMeta{UseNonRootUser: true, NonRootUserName: userName}
			if hadGroup {
				note("user: %s became NonRootUserName %q: the group half has no Zinc field", service.User, userName)
			}
		}
	}
	cfg.ResourcesMeta = resources(service, note)
	cfg.Capabilities = importCapabilities(service, note)
	cfg.Volumes = importVolumes(service, note)
	cfg.NetworkMeta.NetworkLists = importPorts(service, note)

	if len(cfg.NetworkMeta.NetworkLists) == 0 {
		note("no network: compose cannot say what a service may reach, so nothing was inferred. This app starts with no network at all; add NetworkLists for what it actually needs.")
	} else {
		note("only the published ports came across. What this app may REACH is still nothing - compose does not state egress, so none was invented.")
	}
	for _, dep := range service.DependsOn.Names() {
		note("depends_on %q brings the app up first, but does NOT let it talk to that app. On a compose network they would share one; in Zinc a link is a NetworkList naming %q, and its ports must be published on the other side.", dep, appName(dep))
	}
	return App{Service: name, Config: cfg, Notes: notes}
}

// isNumeric reports whether every character is a digit, which is what makes a compose user
// field an id rather than a name.
func isNumeric(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// appName coerces a compose service key into a legal AppNameID: lowercase, and the
// characters outside [a-z0-9._-] folded to '-'. Compose keys are far more permissive than
// Zinc names, and the name becomes a filename and a container name, so this is a
// narrowing, never a widening.
func appName(service string) string {
	lowered := strings.ToLower(strings.TrimSpace(service))
	var out strings.Builder
	for _, char := range lowered {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '.', char == '_', char == '-':
			out.WriteRune(char)
		default:
			out.WriteRune('-')
		}
	}
	name := strings.TrimLeft(out.String(), "._-")
	if name == "" {
		return "imported"
	}
	return name
}

// entrypoint takes the one executable Zinc runs and reports the argv it came from, so the caller can
// say what was dropped: Zinc's Entrypoint is a single token for podman's --entrypoint, compose's is
// an argv. A one-string compose entrypoint is shell-split, which is compose's own rule - taking the
// whole string would produce an entrypoint that is a filename with spaces, valid and unexecutable.
func entrypoint(service Service) (head string, argv []string) {
	source := service.Entrypoint
	if len(source) == 0 {
		source = service.Command
	}
	if len(source) == 0 {
		return "", nil
	}
	if len(source) == 1 {
		argv = strings.Fields(source[0])
	} else {
		argv = source
	}
	if len(argv) == 0 {
		return "", nil
	}
	return argv[0], argv
}

// readyCheck converts a compose healthcheck into a Zinc ReadyCheck, which is argv. CMD is argv
// already; CMD-SHELL is representable as `sh -c <string>` and is by far the most common form in real
// files. NONE switches an inherited check off, so there is nothing to import.
func readyCheck(check *Healthcheck) ([]string, bool) {
	if check == nil || len(check.Test) == 0 {
		return nil, false
	}
	switch check.Test[0] {
	case "CMD":
		if len(check.Test) == 1 {
			return nil, false
		}
		return append([]string(nil), check.Test[1:]...), true
	case "CMD-SHELL":
		if len(check.Test) != 2 {
			return nil, false
		}
		return []string{"sh", "-c", check.Test[1]}, true
	case "NONE":
		return nil, false
	default:
		// No leading keyword: compose reads a bare string as CMD-SHELL.
		if len(check.Test) == 1 {
			return []string{"sh", "-c", check.Test[0]}, true
		}
		return nil, false
	}
}

func hasImportableProbe(check *Healthcheck) bool {
	_, ok := readyCheck(check)
	return ok
}

// healthcheckKind names why a healthcheck did not import, for the note.
func healthcheckKind(test []string) string {
	switch test[0] {
	case "NONE":
		return "NONE, which disables it"
	case "CMD", "CMD-SHELL":
		return "malformed for its own keyword"
	default:
		return "not a form this importer recognises"
	}
}

// resources reads compose's limits into ResourcesMeta. Memory is a byte quantity with a
// unit suffix; anything unparseable is left at zero (unlimited) rather than guessed at,
// because guessing low would throttle an app and guessing high would not be a limit.
func resources(service Service, note func(string, ...any)) schema.ResourcesMeta {
	res := schema.ResourcesMeta{PIDsLimit: service.PidsLimit}
	if service.Deploy == nil || service.Deploy.Resources.Limits == nil {
		return res
	}
	limits := service.Deploy.Resources.Limits
	if limits.CPUs != "" {
		if cores, err := strconv.ParseFloat(limits.CPUs, 64); err == nil && cores > 0 {
			res.MaxCPUCores = cores
		} else {
			note("deploy.resources.limits.cpus %q could not be read, so this app imports with NO cpu limit. Set ResourcesMeta.MaxCPUCores by hand.", limits.CPUs)
		}
	}
	if limits.Memory != "" {
		if mib, ok := memoryMiB(limits.Memory); ok {
			res.MaxRamMiB = mib
		} else {
			note("deploy.resources.limits.memory %q could not be read as whole MiB, so this app imports with NO memory limit. Set ResourcesMeta.MaxRamMiB by hand.", limits.Memory)
		}
	}
	return res
}

// memoryMiB parses compose's byte quantity ("512M", "2g", "1073741824") into MiB. Compose
// units are binary despite the single-letter spelling, which is what podman uses too.
func memoryMiB(text string) (int64, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(text))
	if trimmed == "" {
		return 0, false
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(trimmed, "gb"), strings.HasSuffix(trimmed, "g"):
		multiplier, trimmed = 1024, strings.TrimSuffix(strings.TrimSuffix(trimmed, "b"), "g")
	case strings.HasSuffix(trimmed, "mb"), strings.HasSuffix(trimmed, "m"):
		multiplier, trimmed = 1, strings.TrimSuffix(strings.TrimSuffix(trimmed, "b"), "m")
	case strings.HasSuffix(trimmed, "kb"), strings.HasSuffix(trimmed, "k"):
		// Sub-MiB limits round down to nothing, and zero means unlimited - the opposite of
		// what the file asked for. Refuse instead.
		return 0, false
	case strings.HasSuffix(trimmed, "b"):
		trimmed = strings.TrimSuffix(trimmed, "b")
		bytes, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || bytes < 1024*1024 {
			return 0, false
		}
		return bytes / (1024 * 1024), true
	default:
		bytes, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || bytes < 1024*1024 {
			return 0, false
		}
		return bytes / (1024 * 1024), true
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value * multiplier, true
}

// importCapabilities carries cap_add across and drops the rest of the compose privilege
// vocabulary on the floor loudly. Zinc's baseline is cap-drop ALL plus what an app names,
// so a file that only drops capabilities is already covered; a file that adds SYS_ADMIN or
// asks for privileged mode is asking for something the sandbox exists to refuse.
func importCapabilities(service Service, note func(string, ...any)) []string {
	var caps []string
	for _, capability := range service.CapAdd {
		upper := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(capability)), "CAP_")
		switch upper {
		case "ALL":
			note("cap_add: ALL was dropped: Zinc's baseline is cap-drop ALL, and re-adding every capability would undo the containment this tool is for. Name the capabilities the app actually needs.")
			continue
		case "NET_ADMIN", "SYS_ADMIN":
			// Refused rather than noted: the validator only refuses these on a FILTERED app, and an
			// imported app has no NetworkLists, so without this they would sail through.
			note("cap_add: %s was dropped: it would let the app remove its own network lock-down, so it is never granted by an import. Add it by hand if this app genuinely needs it and you accept what it means.", upper)
			continue
		}
		note("cap_add: %s was carried over - it is granted on top of Zinc's cap-drop ALL baseline. Drop it if the app does not need it.", upper)
		caps = append(caps, upper)
	}
	for _, opt := range service.SecurityOpt {
		if strings.HasPrefix(opt, "no-new-privileges") {
			continue // already the baseline
		}
		note("security_opt %q was dropped: Zinc sets its own baseline (no-new-privileges, cap-drop ALL) and has no field for arbitrary runtime options", opt)
	}
	return caps
}

// importVolumes reads compose's short-form mounts. A host path becomes a bind mount defaulting -
// unlike compose - to read-only and noexec, the fail-closed reading of an unstated intent. A named
// volume has no Zinc equivalent and is reported rather than invented.
//
// Keys are deliberately not derived from mounts: any guess would come from a path looking key-shaped,
// and being wrong either shares a credential or moves one the app needs.
func importVolumes(service Service, note func(string, ...any)) []schema.Volume {
	var volumes []schema.Volume
	for _, mount := range service.Volumes {
		parts := strings.Split(mount, ":")
		if len(parts) < 2 {
			note("volume %q was dropped: it names a container path with no host path, which is an anonymous volume Zinc does not create", mount)
			continue
		}
		host, inner := parts[0], parts[1]
		if !strings.HasPrefix(host, "/") && !strings.HasPrefix(host, ".") && !strings.HasPrefix(host, "~") {
			note("volume %q was dropped: %q is a named compose volume, and Zinc mounts host paths rather than managing volumes", mount, host)
			continue
		}
		if !strings.HasPrefix(host, "/") {
			note("volume %q was dropped: %q is relative to the compose file (or to a shell's home), and Zinc mounts absolute host paths. Re-add it with the full path.", mount, host)
			continue
		}
		volume := schema.Volume{HostMounted: true, HostMount: host, InnerMount: inner}
		options := ""
		if len(parts) > 2 {
			options = parts[2]
		}
		writable := false
		for _, opt := range strings.Split(options, ",") {
			switch strings.TrimSpace(opt) {
			case "rw":
				volume.Writable, writable = true, true
			case "ro":
				writable = true // stated, and it agrees with the default
			case "exec":
				volume.Executable = true
			}
		}
		// Anything that did not state ro or rw lands read-only, and that has to be said. The
		// case worth naming is `:z` / `:Z`, the SELinux relabel suffix that is everywhere on
		// this project's own platform: it means read-WRITE, and it used to go silent because
		// the note only fired for a bare mount.
		if !writable {
			note("volume %q became read-only and noexec: a compose mount that does not say `ro` is read-write, and importing that silently would hand the app write access nobody asked for. Set Writable if it needs it.", mount)
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

// importPorts turns published ports into the one NetworkList kind compose actually states.
// `ports` reaches the LAN, `expose` reaches only siblings on the same network - which is
// exactly the Host distinction on an ingress list. A host-side port that differs from the
// container's cannot be represented: Zinc publishes a listener under its own number.
func importPorts(service Service, note func(string, ...any)) []schema.NetworkList {
	var lists []schema.NetworkList
	// A compose port may name the address to bind, and `127.0.0.1:8080:80` is how a file says
	// "this is not for the network". Publishing it to the LAN would be the importer widening
	// the sandbox from what the file asked for, which is the one thing it must not do - so a
	// loopback-bound port is imported as a sibling-only listener instead.
	lanSpecs, localSpecs := splitByBindAddress(service.Ports)
	published := portNumbers(lanSpecs, true, note)
	internal := portNumbers(append(append(StringList{}, service.Expose...), localSpecs...), false, note)
	if len(localSpecs) > 0 {
		note("port(s) %v were bound to loopback in the compose file, so they are NOT published to the LAN here - they are reachable only from sibling apps on a link. Add Host if the LAN really should reach them.", []string(localSpecs))
	}
	if len(published) > 0 {
		lists = append(lists, schema.NetworkList{Ingress: true, Host: true, Ports: published})
		note("ports %v are published to the LAN (Ingress + Host). If only sibling apps need them, drop Host.", published)
	}
	if len(internal) > 0 {
		lists = append(lists, schema.NetworkList{Ingress: true, Ports: internal})
	}
	return lists
}

// splitByBindAddress separates port specs that reach the network from those the compose file
// pinned to loopback. A spec is IP:HOST:CONTAINER when it has three fields; anything else
// binds everywhere.
func splitByBindAddress(specs StringList) (lan, local StringList) {
	for _, spec := range specs {
		fields := strings.Split(spec, ":")
		if len(fields) >= 3 {
			bind := strings.Trim(strings.Join(fields[:len(fields)-2], ":"), "[]")
			if address := net.ParseIP(bind); address != nil && address.IsLoopback() {
				local = append(local, spec)
				continue
			}
		}
		lan = append(lan, spec)
	}
	return lan, local
}

// portNumbers reads compose port specs into the container-side numbers. A published spec
// is "HOST:CONTAINER" or "IP:HOST:CONTAINER" or a bare port; the container side is the
// last field, optionally suffixed with /tcp or /udp.
func portNumbers(specs StringList, published bool, note func(string, ...any)) []int {
	var ports []int
	for _, spec := range specs {
		fields := strings.Split(spec, ":")
		last := fields[len(fields)-1]
		last, _, _ = strings.Cut(last, "/")
		if strings.Contains(last, "-") {
			note("port %q was dropped: Zinc's Ports are individual numbers, not a range", spec)
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(last))
		if err != nil || port <= 0 {
			note("port %q was dropped: could not read a container port from it", spec)
			continue
		}
		if published && len(fields) > 1 {
			host := strings.TrimSpace(fields[len(fields)-2])
			if hostPort, herr := strconv.Atoi(host); herr == nil && hostPort != port {
				note("port %q: Zinc publishes a listener under its own number, so it is exposed as %d, not %d", spec, port, hostPort)
			}
		}
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return ports
}
