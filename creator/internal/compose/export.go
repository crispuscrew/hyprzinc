package compose

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// FromApp renders one app definition as a compose project and returns the notes naming what could not
// come with it. Pure. The notes are the honest half: what compose cannot carry - the egress lock-down
// above all - would otherwise be absent without being mentioned, and a reader comparing the two files
// would conclude Zinc does nothing the compose file does not show. A VM app is refused rather than
// half-rendered.
func FromApp(cfg schema.AppConfig) (Project, []string, error) {
	if cfg.Type == schema.ZincVirtualization {
		return Project{}, nil, fmt.Errorf("%s is a VM app: compose describes containers, and a guest is not one", cfg.AppNameID)
	}

	var notes []string
	note := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }

	service := Service{
		Image:         cfg.ImageMeta.Image,
		ContainerName: cfg.AppNameID,
		// The baseline the runner applies to every app. Stated outright rather than left
		// implicit, because in compose it is not the default: a service with no cap_drop
		// runs with podman's default capability set.
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges:true"},
	}
	if entry := strings.TrimSpace(cfg.StartConditions.Entrypoint); entry != "" {
		service.Entrypoint = StringList{entry}
	}
	if cfg.StartConditions.Autorestart {
		service.Restart = "on-failure"
	}
	if user := cfg.InternalUserMeta; user.UseNonRootUser && user.NonRootUserName != "" {
		service.User = user.NonRootUserName
	}
	service.CapAdd = append(service.CapAdd, cfg.Capabilities...)
	service.PidsLimit = cfg.ResourcesMeta.PIDsLimit
	if limits := resourceLimits(cfg.ResourcesMeta); limits != nil {
		service.Deploy = &Deploy{Resources: Resources{Limits: limits}}
	}
	if probe := cfg.StartConditions.ReadyCheck; len(probe) > 0 {
		service.Healthcheck = &Healthcheck{Test: append(StringList{"CMD"}, probe...)}
		if seconds := cfg.StartConditions.ReadyTimeoutSec; seconds > 0 {
			// Deliberately NOT healthcheck.timeout, which bounds how long ONE probe may run;
			// ReadyTimeoutSec bounds how long a DEPENDENT waits.
			note("StartConditions.ReadyTimeoutSec (%ds) is not represented: it bounds how long a dependent waits for this app, and compose's healthcheck.timeout bounds a single probe's run - different things, so it was not written as one.", seconds)
		}
	}
	for _, dep := range cfg.StartConditions.DependsOn {
		if service.DependsOn == nil {
			service.DependsOn = Dependencies{}
		}
		// The condition cannot be decided from this app alone: whether the wait is for "started" or "healthy"
		// is a property of the DEPENDENCY's ReadyCheck. service_started is the safe reading, and claiming
		// service_healthy for a dependency with no healthcheck would make the compose file hang.
		service.DependsOn[dep] = Depend{Condition: ConditionStarted}
	}
	if desc := strings.TrimSpace(cfg.Description); desc != "" {
		service.Labels = Labels{"zinc.description": desc}
	}

	service.DNS = append(service.DNS, cfg.NetworkMeta.DNSServers...)
	service.Volumes = append(service.Volumes, volumeMounts(cfg, note)...)
	ports, expose, netNotes := networkFields(cfg)
	service.Ports, service.Expose = ports, expose
	notes = append(notes, netNotes...)

	if len(cfg.ImageMeta.Install) > 0 {
		note("ImageMeta.Install is not represented: Zinc builds a derived image from the pinned base plus those steps, and compose can only point `build:` at a Containerfile on disk. The image named here is the BASE, not what Zinc runs.")
	}
	if cfg.HostTheme {
		note("HostTheme is not represented: the theme bundle is assembled by the runner at launch and mounted read-only from a path only it knows.")
	}
	note("Desktop wiring (the Wayland socket, PipeWire, /dev/dri) is not represented: the runner resolves it at launch from the session it is started in, so it is not knowable when this file is written.")
	if cfg.StartConditions.Multiterminal {
		note("StartConditions.Multiterminal is not represented: the shared holder container and its per-terminal `exec` are a runner behaviour, not a compose one.")
	}
	if len(cfg.Keys) > 0 {
		note("Keys are mounted read-only into the home of the user the app runs as; the paths below are the host's and are not portable to another machine.")
	}
	if len(cfg.Configs) > 0 {
		note("Configs are not represented: they are mounted from the app's own bundle directory (apps/<app>/configs), which compose has no equivalent for. The described container starts without them.")
	}
	if cfg.ResourcesMeta.MaxSwapMiB > 0 {
		note("ResourcesMeta.MaxSwapMiB (%d MiB) is not represented: compose states one memory limit, and the swap allowance sits on top of it rather than inside it.", cfg.ResourcesMeta.MaxSwapMiB)
	}
	if cfg.InternalUserMeta.KeepUserID {
		note("InternalUserMeta.KeepUserID is not represented: mapping the host uid into the container is `podman --userns=keep-id`, which compose has no field for.")
	}
	if !cfg.DBusMeta.IsZero() {
		// The direction of the loss is what makes this safe: an importer gets no bus grants, which is
		// the fail-closed default.
		note("DBusMeta (%d Talk, %d Own) is not represented: the filtered bus is a socket served by a proxy container the runner starts, and compose has no way to state either the proxy or the grants. An app imported from this file gets NO session bus, not these names.",
			len(cfg.DBusMeta.Talk), len(cfg.DBusMeta.Own))
	}
	if cfg.StartConditions.Terminal || cfg.StopConditions.KeepAlive || cfg.StopConditions.Background {
		note("The terminal and stop-condition settings (Terminal, KeepAlive, Background) are not represented: they decide how the runner launches and reaps the app, which is not something a compose file states.")
	}
	if len(cfg.NetworkMeta.DNSServers) > 0 {
		note("NetworkMeta.DNSServers is written as `dns:`, but only as a setting: in Zinc it is also a RESTRICTION - the ruleset drops DNS to anything else - and compose cannot say that half.")
	}

	project := Project{
		Name:     cfg.AppNameID,
		Services: map[string]Service{cfg.AppNameID: service},
	}
	if len(cfg.NetworkMeta.NetworkLists) > 0 {
		notes = append(notes,
			"THE EGRESS LOCK-DOWN IS NOT REPRESENTED. Zinc applies an nftables ruleset to this app's network namespace, and locks it, BEFORE the app starts; running this compose file gives the container ordinary, unfiltered networking. This file describes the app - it does not sandbox it.")
	}
	return project, notes, nil
}

// resourceLimits renders ResourcesMeta into compose's own spellings, or nil when the app
// set no CPU or memory cap. PIDsLimit is not here: compose keeps it at the service's top
// level rather than under deploy.
func resourceLimits(res schema.ResourcesMeta) *Limits {
	var limits Limits
	if res.MaxCPUCores > 0 {
		limits.CPUs = strconv.FormatFloat(res.MaxCPUCores, 'f', -1, 64)
	}
	if res.MaxRamMiB > 0 {
		limits.Memory = strconv.FormatInt(res.MaxRamMiB, 10) + "M"
	}
	if limits == (Limits{}) {
		return nil
	}
	return &limits
}

// volumeMounts renders the app's host bind mounts and key mounts in compose's short
// string form. The options are the ones the runner passes podman, so a reader sees the
// same posture: read-only and noexec unless the app asked otherwise.
func volumeMounts(cfg schema.AppConfig, note func(string, ...any)) StringList {
	var mounts StringList
	for index, volume := range cfg.Volumes {
		if !volume.HostMounted || strings.TrimSpace(volume.HostMount) == "" {
			note("Volumes[%d] (%s) is not represented: it is not a host bind mount, and compose's short mount syntax has nothing to point at.", index, volume.InnerMount)
			continue
		}
		options := "ro"
		if volume.Writable {
			options = "rw"
		}
		if volume.Executable {
			options += ",exec"
		} else {
			options += ",noexec"
		}
		mounts = append(mounts, volume.HostMount+":"+volume.InnerMount+":"+options)
	}
	home := "/root"
	if user := cfg.InternalUserMeta; user.UseNonRootUser && user.NonRootUserName != "" {
		home = "/home/" + user.NonRootUserName
	}
	for _, key := range cfg.Keys {
		dir := ".ssh"
		if key.Type == schema.GPG {
			dir = ".gnupg"
		}
		base := key.Path
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}
		mounts = append(mounts, key.Path+":"+home+"/"+dir+"/"+base+":ro")
	}
	return mounts
}

// networkFields renders the NetworkLists compose has a word for, and reports the rest.
// Published ports (Ingress && Host) become `ports`, a sibling link's published ports
// become `expose`, and every other kind of list - the allow/deny egress rules, the
// routing - has no compose spelling at all, which is exactly the part that matters most.
func networkFields(cfg schema.AppConfig) (ports, expose StringList, notes []string) {
	for index, netList := range cfg.NetworkMeta.NetworkLists {
		switch {
		case netList.Ingress && netList.Host:
			for _, port := range netList.Ports {
				text := strconv.Itoa(port)
				ports = append(ports, text+":"+text)
			}
		case netList.Ingress:
			for _, port := range netList.Ports {
				expose = append(expose, strconv.Itoa(port))
			}
		case strings.TrimSpace(netList.AppName) != "" && netList.Via:
			notes = append(notes, fmt.Sprintf(
				"NetworkLists[%d] routes this app's traffic through %q. Compose has no routing: joining both services to one network lets them talk, but it does NOT send this app's egress through the other, and nothing here stops it reaching the internet directly.",
				index, netList.AppName))
		case strings.TrimSpace(netList.AppName) != "":
			notes = append(notes, fmt.Sprintf(
				"NetworkLists[%d] links this app to %q over a private bridge, restricted to that app's published ports. Put both services on one `internal` compose network to approximate it; the port restriction does not come along.",
				index, netList.AppName))
		}
	}
	// Sorted by number, not as text: sort.Strings would order 443, 8080, 80.
	sortPortSpecs(ports)
	sortPortSpecs(expose)
	return ports, expose, notes
}

// sortPortSpecs orders published/exposed entries by their container port. Sorting them as
// text would put 443 before 8080 before 80.
func sortPortSpecs(specs StringList) {
	sort.Slice(specs, func(one, two int) bool { return specPort(specs[one]) < specPort(specs[two]) })
}

// specPort reads the container-side port out of a "N" or "HOST:N" spec.
func specPort(spec string) int {
	last := spec
	if _, after, found := strings.Cut(spec, ":"); found {
		last = after
	}
	port, _ := strconv.Atoi(last)
	return port
}
