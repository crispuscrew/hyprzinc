package podman

import (
	"slices"
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/derived"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

func baseOpts() options.HostOptions {
	return options.HostOptions{
		RuntimeDir:     "/run/user/1000",
		WaylandDisplay: "wayland-1",
		ThemeBundleDir: "/home/user/.local/share/zinc/theme-bundle",
		HomeDir:        "/root",
	}
}

// netNone is the network attachment an unfiltered app hands AppRunArgs; the podman
// adapter only splices it in (it no longer decides the network itself).
func netNone() []string { return []string{"--network", "none"} }

func appArgs(t *testing.T, cfg schema.AppConfig, opt options.HostOptions, netFlags []string) []string {
	t.Helper()
	got, err := Runtime{}.AppRunArgs(cfg, opt, netFlags)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A strict, no-network app: exact argv, so the least-privilege baseline, hermetic
// --pull never, the Wayland/theme wiring and the honest display label are all pinned.
//
// No opt.WaylandSocket, so no security context was established for this launch - the app
// gets the compositor's own socket and the label says exactly that.
func TestAppRunArgs_StrictNone(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:   "firefox",
		ImageMeta:   schema.ImageMeta{Image: "docker.io/library/firefox@sha256:abc"},
		DisplayMeta: schema.DisplayMeta{DisableGpuAccess: true}, // security-context wanted, no GPU
		HostTheme:   true,
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	want := []string{
		"run", "--rm", "--pull", "never", "--name", "firefox",
		"--security-opt", "no-new-privileges", "--cap-drop", "all",
		"--network", "none",
		"-v", "/run/user/1000/wayland-1:/run/zinc/wayland-1:ro",
		"-e", "WAYLAND_DISPLAY=wayland-1",
		"-e", "XDG_RUNTIME_DIR=/run/zinc",
		"--label", "zinc.wayland=passthrough",
		"-v", "/home/user/.local/share/zinc/theme-bundle:/etc/zinc/theme:ro",
		"docker.io/library/firefox@sha256:abc",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// With a security context established, the app mounts the DERIVED socket - and only the
// source of the mount changes. The container-side path and WAYLAND_DISPLAY must be identical
// to the passthrough case, or an app would have to be configured differently depending on
// something it cannot see.
func TestAppRunArgs_WaylandSecurityContext(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "browser.work",
		ImageMeta: schema.ImageMeta{Image: "img@sha256:abc"},
	}
	opt := baseOpts()
	opt.WaylandSocket = "/run/user/1000/zinc/wayland/browser.work/wayland-1"
	got := appArgs(t, cfg, opt, netNone())

	assertContainsSeq(t, got, "-v", "/run/user/1000/zinc/wayland/browser.work/wayland-1:/run/zinc/wayland-1:ro")
	assertContainsSeq(t, got, "-e", "WAYLAND_DISPLAY=wayland-1")
	assertContainsSeq(t, got, "--label", "zinc.wayland=security-context")
	mustNotContain(t, got, "/run/user/1000/wayland-1:/run/zinc/wayland-1:ro") // never the compositor's own
}

// The escape hatch keeps its meaning: an app that opted out gets the raw socket even when the
// launch path had a derived one to offer, and the label does not claim otherwise.
func TestAppRunArgs_SecurityContextDisabledMountsRawSocket(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:   "legacy",
		ImageMeta:   schema.ImageMeta{Image: "img@sha256:abc"},
		DisplayMeta: schema.DisplayMeta{DisableSecurityContext: true},
	}
	opt := baseOpts()
	opt.WaylandSocket = "/run/user/1000/zinc/wayland/legacy/wayland-1"
	got := appArgs(t, cfg, opt, netNone())

	assertContainsSeq(t, got, "-v", "/run/user/1000/wayland-1:/run/zinc/wayland-1:ro")
	assertContainsSeq(t, got, "--label", "zinc.wayland=passthrough")
	mustNotContain(t, got, "/run/user/1000/zinc/wayland/legacy/wayland-1:/run/zinc/wayland-1:ro")
}

// A background app with GPU, a host bind mount, pipewire, and an extra cap: the
// enforcer's netFlags are spliced verbatim and every wiring is present.
func TestAppRunArgs_BackgroundGPUMountCap(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:      "work-app",
		ImageMeta:      schema.ImageMeta{Image: "localhost/zinc-go-dev:latest"},
		StopConditions: schema.StopConditions{Background: true},
		DisplayMeta:    schema.DisplayMeta{DisableSecurityContext: true}, // passthrough; GPU on (default)
		Volumes:        []schema.Volume{{InnerMount: "/work", HostMounted: true, HostMount: "/home/user/code", Writable: true}},
		AudioMeta:      schema.AudioMeta{Pipewire: true},
		Capabilities:   []string{"NET_RAW"},
	}
	got := appArgs(t, cfg, baseOpts(), []string{"--network", "container:vpn"})
	assertContainsSeq(t, got, "--network", "container:vpn") // spliced verbatim
	assertContainsSeq(t, got, "--cap-drop", "all")          // least-privilege baseline
	assertContainsSeq(t, got, "--cap-add", "NET_RAW")       // explicit grant on top
	assertContains(t, got, "-d")                            // background
	assertContains(t, got, "/dev/dri")                      // gpu on (opt-out default)
	mustNotContain(t, got, "/dev/snd")                      // legacy_alsa was false
	assertContainsSeq(t, got, "-v", "/home/user/code:/work:rw,noexec")
	mustNotContain(t, got, "/etc/zinc/theme") // HostTheme false → no bundle mount
	assertContainsSeq(t, got, "-v", "/run/user/1000/pipewire-0:/run/zinc/pipewire-0:ro")
	if got[len(got)-1] != "localhost/zinc-go-dev:latest" {
		t.Fatalf("image must be last arg, got %q", got[len(got)-1])
	}
}

func TestAppRunArgs_PipewireWithoutWayland(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "mpd",
		ImageMeta: schema.ImageMeta{Image: "img@sha256:abc"},
		AudioMeta: schema.AudioMeta{Pipewire: true},
	}
	opt := baseOpts()
	opt.WaylandDisplay = "" // headless: no Wayland socket wired
	got := appArgs(t, cfg, opt, netNone())
	assertContainsSeq(t, got, "-v", "/run/user/1000/pipewire-0:/run/zinc/pipewire-0:ro")
	assertContainsSeq(t, got, "-e", "XDG_RUNTIME_DIR=/run/zinc")
}

func TestAppRunArgs_NoRuntimeDirWithoutSockets(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:   "tool",
		ImageMeta:   schema.ImageMeta{Image: "img@sha256:abc"},
		DisplayMeta: schema.DisplayMeta{DisableGpuAccess: true},
	}
	opt := baseOpts()
	opt.WaylandDisplay = "" // no wayland, no audio → runtime dir stays empty
	got := appArgs(t, cfg, opt, netNone())
	mustNotContain(t, got, "XDG_RUNTIME_DIR=/run/zinc")
	assertContainsSeq(t, got, "--security-opt", "no-new-privileges")
	assertContainsSeq(t, got, "--cap-drop", "all")
}

func TestAppRunArgs_Terminal(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:       "shell",
		ImageMeta:       schema.ImageMeta{Image: "docker.io/library/alpine@sha256:abc"},
		StartConditions: schema.StartConditions{Terminal: true},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	if got[0] != "run" {
		t.Fatalf("argv must start with run, got %v", got)
	}
	assertContainsSeq(t, got, "--rm", "-it") // interactive TTY for a CLI/TUI app
	mustNotContain(t, got, "-d")             // terminal apps are never detached/background
}

// The entrypoint overrides the image ENTRYPOINT via --entrypoint (exec form); the
// image is the last arg with no trailing command.
func TestAppRunArgs_Entrypoint(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:       "shell",
		ImageMeta:       schema.ImageMeta{Image: "img@sha256:abc"},
		StartConditions: schema.StartConditions{Entrypoint: "htop"},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	assertContainsSeq(t, got, "--entrypoint", "htop")
	if last := got[len(got)-1]; last != "img@sha256:abc" {
		t.Fatalf("image must be the last arg (no trailing cmd with --entrypoint), got %q", last)
	}
}

// KeepAlive keeps the container after its entrypoint exits, so --rm is dropped.
func TestAppRunArgs_KeepAlive(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:      "job",
		ImageMeta:      schema.ImageMeta{Image: "img@sha256:abc"},
		StopConditions: schema.StopConditions{KeepAlive: true},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	mustNotContain(t, got, "--rm")
}

func TestAppRunArgs_Holder(t *testing.T) {
	// A multiterminal app's container is a detached holder: -d --rm, no -it, and
	// HolderCmd as PID 1 - the app's own command runs per-terminal via ExecArgs.
	cfg := schema.AppConfig{
		AppNameID: "dev",
		ImageMeta: schema.ImageMeta{Image: "docker.io/library/alpine@sha256:abc"},
		StartConditions: schema.StartConditions{
			Terminal: true, Multiterminal: true, Entrypoint: "htop",
		},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	assertContainsSeq(t, got, "-d", "--rm")
	assertContains(t, got, "--init")       // prompt `podman stop` (PID-1 signal semantics)
	mustNotContain(t, got, "-it")          // holder has no TTY
	mustNotContain(t, got, "--entrypoint") // holder ignores the app entrypoint
	wantTail := append([]string{"docker.io/library/alpine@sha256:abc"}, HolderCmd()...)
	if tail := got[len(got)-len(wantTail):]; !slices.Equal(tail, wantTail) {
		t.Fatalf("holder cmd must follow the image, got tail %v want %v", tail, wantTail)
	}
}

// --- derived images (install) ---

func installCfg(install ...string) schema.AppConfig {
	return schema.AppConfig{
		AppNameID: "hollywood",
		ImageMeta: schema.ImageMeta{
			Image:   "docker.io/library/debian@sha256:abc",
			Install: install,
		},
	}
}

// With ImageMeta.Install set, the container must run the locally built derived image,
// not the pinned base - the base is only the FROM of that build.
func TestAppRunArgs_InstallRunsDerivedImage(t *testing.T) {
	got := appArgs(t, installCfg("apt-get install -y hollywood"), baseOpts(), netNone())
	if last := got[len(got)-1]; last != "zinc/app-hollywood:local" {
		t.Fatalf("install app must run the derived image, got last arg %q", last)
	}
	mustNotContain(t, got, "docker.io/library/debian@sha256:abc") // base is only the FROM
}

func TestAppRunArgs_InstallHolder(t *testing.T) {
	cfg := installCfg("apk add --no-cache htop")
	cfg.StartConditions.Terminal, cfg.StartConditions.Multiterminal = true, true
	cfg.StartConditions.Entrypoint = "htop"
	got := appArgs(t, cfg, baseOpts(), netNone())
	wantTail := append([]string{"zinc/app-hollywood:local"}, HolderCmd()...)
	if tail := got[len(got)-len(wantTail):]; !slices.Equal(tail, wantTail) {
		t.Fatalf("holder install app must run the derived image, got tail %v want %v", tail, wantTail)
	}
}

func TestImageBuildArgs(t *testing.T) {
	cfg := installCfg("apk add --no-cache sl")
	got := ImageBuildArgs(cfg)
	if got[0] != "build" || got[len(got)-1] != "-" {
		t.Fatalf("want `build ... -` (Containerfile on stdin), got %v", got)
	}
	assertContainsSeq(t, got, "-t", "zinc/app-hollywood:local")
	assertContainsSeq(t, got, "--label", "zinc.build="+derived.BuildFingerprint(cfg))
}

// --- pure builders + detached command wiring ---

func TestExecArgs(t *testing.T) {
	if got := ExecArgs("dev", []string{"htop", "--tree"}); !slices.Equal(
		got, []string{"exec", "-it", "dev", "htop", "--tree"}) {
		t.Fatalf("exec argv mismatch: %v", got)
	}
	if got := ExecArgs("dev", []string{"/bin/sh"}); !slices.Equal(
		got, []string{"exec", "-it", "dev", "/bin/sh"}) {
		t.Fatalf("shell exec argv mismatch: %v", got)
	}
}

func TestTerminalLaunch(t *testing.T) {
	got := TerminalLaunch([]string{"xterm", "-e"}, []string{"run", "--rm", "-it", "alpine"}, false)
	want := []string{"xterm", "-e", "podman", "run", "--rm", "-it", "alpine"}
	if !slices.Equal(got, want) {
		t.Fatalf("terminal wrap mismatch:\n got %v\nwant %v", got, want)
	}
}

// With hold the podman argv is wrapped in `sh -c` so the window pauses after the
// command exits; the argv must be single-quoted (no break-out) and the script must
// block on input at the end.
func TestTerminalLaunchHold(t *testing.T) {
	got := TerminalLaunch([]string{"foot"}, []string{"run", "--rm", "-it", "alpine"}, true)
	if len(got) != 4 || got[0] != "foot" || got[1] != "sh" || got[2] != "-c" {
		t.Fatalf("hold wrap should be `foot sh -c <script>`, got %v", got)
	}
	script := got[3]
	if !strings.Contains(script, "podman 'run' '--rm' '-it' 'alpine'") {
		t.Fatalf("script missing single-quoted podman argv: %q", script)
	}
	if !strings.Contains(script, "read _") {
		t.Fatalf("script should pause on exit: %q", script)
	}
}

// shellQuote must neutralise an embedded single quote so a crafted command argv cannot
// escape the hold wrapper.
func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	if got, want := shellQuote(`a'b`), `'a'\''b'`; got != want {
		t.Fatalf("shellQuote(%q) = %q, want %q", `a'b`, got, want)
	}
}

func TestLifecycleArgs(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"stop", StopArgs("firefox"), []string{"stop", "firefox"}},
		{"restart", RestartArgs("firefox"), []string{"restart", "firefox"}},
		{"inspect", InspectArgs("firefox"), []string{"inspect", "firefox"}},
		{"logs", LogsArgs("firefox", false), []string{"logs", "firefox"}},
		{"logs follow", LogsArgs("firefox", true), []string{"logs", "-f", "firefox"}},
	}
	for _, tcase := range cases {
		t.Run(tcase.name, func(t *testing.T) {
			if !slices.Equal(tcase.got, tcase.want) {
				t.Fatalf("got %v, want %v", tcase.got, tcase.want)
			}
		})
	}
}

func validCfg() schema.AppConfig {
	return schema.AppConfig{
		AppNameID: "demo",
		ImageMeta: schema.ImageMeta{Image: "docker.io/library/demo@sha256:abc"},
	}
}

func TestAppCmd_GUI(t *testing.T) {
	pc, err := appCmd(validCfg(), options.HostOptions{}, []string{"run", "--rm", "img"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"podman", "run", "--rm", "img"}; !slices.Equal(pc.Args, want) {
		t.Fatalf("gui app argv: got %v want %v", pc.Args, want)
	}
	if pc.SysProcAttr == nil || !pc.SysProcAttr.Setsid {
		t.Fatal("launched app must be detached into its own session (Setsid) so it survives the launcher")
	}
}

func TestAppCmd_Terminal(t *testing.T) {
	cfg := validCfg()
	cfg.StartConditions.Terminal = true
	pc, err := appCmd(cfg, options.HostOptions{Terminal: []string{"foot"}}, []string{"run", "--rm", "-it", "img"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"foot", "podman", "run", "--rm", "-it", "img"}; !slices.Equal(pc.Args, want) {
		t.Fatalf("terminal app argv: got %v want %v", pc.Args, want)
	}
}

func TestAppCmd_TerminalNoEmulator(t *testing.T) {
	cfg := validCfg()
	cfg.StartConditions.Terminal = true
	if _, err := appCmd(cfg, options.HostOptions{}, []string{"run"}); err == nil {
		t.Fatal("a terminal app with no configured emulator must error, not launch blind")
	}
}

// --- test helpers ---

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	if !slices.Contains(args, want) {
		t.Fatalf("expected args to contain %q; got %v", want, args)
	}
}

func mustNotContain(t *testing.T, args []string, bad string) {
	t.Helper()
	if slices.Contains(args, bad) {
		t.Fatalf("did not expect args to contain %q; got %v", bad, args)
	}
}

// assertContainsSeq checks that first and second appear adjacent and in order.
func assertContainsSeq(t *testing.T, args []string, first, second string) {
	t.Helper()
	for index := 0; index+1 < len(args); index++ {
		if args[index] == first && args[index+1] == second {
			return
		}
	}
	t.Fatalf("expected adjacent %q %q in %v", first, second, args)
}

// The containment fields shipped in the schema, were validated, and never reached podman:
// an app that asked for a memory cap or a non-root user got neither, and nothing said so.
// These pin that they now arrive as flags.
func TestAppRunArgs_ResourceCapsReachPodman(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "app",
		ImageMeta: schema.ImageMeta{Image: "localhost/app:local"},
		ResourcesMeta: schema.ResourcesMeta{
			MaxCPUCores: 0.5,
			MaxRamMiB:   2048,
			MaxSwapMiB:  512,
			PIDsLimit:   100,
		},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())

	assertContainsSeq(t, got, "--cpus", "0.5")
	assertContainsSeq(t, got, "--memory", "2048m")
	assertContainsSeq(t, got, "--pids-limit", "100")
	// The one that is not the number in the config: podman's --memory-swap is the total of
	// memory and swap. Passing 512m here would cap the whole app at a quarter of the
	// memory it asked for, which is the opposite of granting it swap.
	assertContainsSeq(t, got, "--memory-swap", "2560m")
}

// A fractional core has to survive formatting. %f would render 0.5 as "0.500000" and a
// large value in exponent form, and podman rejects both.
func TestAppRunArgs_CPUFormatting(t *testing.T) {
	for _, testCase := range []struct {
		cores float64
		want  string
	}{{0.5, "0.5"}, {2, "2"}, {1.25, "1.25"}} {
		cfg := schema.AppConfig{
			AppNameID:     "app",
			ImageMeta:     schema.ImageMeta{Image: "localhost/app:local"},
			ResourcesMeta: schema.ResourcesMeta{MaxCPUCores: testCase.cores},
		}
		assertContainsSeq(t, appArgs(t, cfg, baseOpts(), netNone()), "--cpus", testCase.want)
	}
}

// Swap without a memory limit is refused by validation rather than guessed at, so the
// runtime must not invent a total from a figure that has nothing to add to.
func TestAppRunArgs_SwapWithoutMemoryEmitsNothing(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:     "app",
		ImageMeta:     schema.ImageMeta{Image: "localhost/app:local"},
		ResourcesMeta: schema.ResourcesMeta{MaxSwapMiB: 512},
	}
	mustNotContain(t, appArgs(t, cfg, baseOpts(), netNone()), "--memory-swap")
}

// The sharpest of the inert fields: an app declaring itself unprivileged ran as root.
func TestAppRunArgs_NonRootUser(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:        "app",
		ImageMeta:        schema.ImageMeta{Image: "localhost/app:local"},
		InternalUserMeta: schema.InternalUserMeta{UseNonRootUser: true, NonRootUserName: "app"},
		Keys:             []schema.Key{{Type: schema.SSH, Path: "/home/user/.ssh/id_ed25519"}},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())

	assertContainsSeq(t, got, "--user", "app")
	// The key has to land somewhere that user can read. Mounted into /root it would be
	// present in the container and still unreadable, which reads as a broken key.
	assertContainsSeq(t, got, "-v", "/home/user/.ssh/id_ed25519:/home/app/.ssh/id_ed25519:ro")
}

// KeepUserID is a different question from which user runs the app: it is about the
// container and the host agreeing on the uid, which is what a shared host directory needs.
func TestAppRunArgs_KeepUserID(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:        "app",
		ImageMeta:        schema.ImageMeta{Image: "localhost/app:local"},
		InternalUserMeta: schema.InternalUserMeta{KeepUserID: true},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	if !slices.Contains(got, "--userns=keep-id") {
		t.Errorf("KeepUserID should map the host uid into the container, got %v", got)
	}
	mustNotContain(t, got, "--user")
}

// An app that asks for none of this must get exactly the argv it got before, or wiring the
// fields up would change every existing app's launch. TestAppRunArgs_StrictNone pins the
// full argv; this states the intent.
func TestAppRunArgs_NoCapsNoFlags(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "app",
		ImageMeta: schema.ImageMeta{Image: "localhost/app:local"},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	for _, flag := range []string{"--cpus", "--memory", "--memory-swap", "--pids-limit", "--user", "--userns=keep-id"} {
		mustNotContain(t, got, flag)
	}
}

// A readiness probe becomes the container's healthcheck, in podman's JSON exec form. The
// string form would be run through a shell inside the container, where an argument with a
// space in it stops meaning itself; the exec form passes the author's words as argv.
func TestAppRunArgs_ReadyCheckBecomesHealthcheck(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "vpn",
		ImageMeta: schema.ImageMeta{Image: "localhost/vpn:local"},
		StartConditions: schema.StartConditions{
			ReadyCheck: []string{"sh", "-c", "ip link show wg0 | grep -q UP"},
		},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())

	// CMD-SHELL, with every word single-quoted. The JSON exec form is tidier and needs no
	// shell in the image, but podman 4.9 - what Ubuntu LTS ships, and what CI runs - hands the
	// whole bracketed string to a shell instead, so the check could never pass. This is a
	// launch-blocking gate; it has to work on the podman people actually have.
	assertContainsSeq(t, got, "--health-cmd", `CMD-SHELL 'sh' '-c' 'ip link show wg0 | grep -q UP'`)
	// No --health-interval: podman's own default is one less thing to differ between versions,
	// and keeping the timer means `podman ps` reports live health rather than the last probe.
	mustNotContain(t, got, "--health-interval")
}

// A word with a space or a quote in it must still mean itself once a shell has read it.
func TestAppRunArgs_ReadyCheckQuoting(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:       "app",
		ImageMeta:       schema.ImageMeta{Image: "localhost/app:local"},
		StartConditions: schema.StartConditions{ReadyCheck: []string{"test", "-f", "/run/a b"}},
	}
	assertContainsSeq(t, appArgs(t, cfg, baseOpts(), netNone()),
		"--health-cmd", `CMD-SHELL 'test' '-f' '/run/a b'`)
}

// No ReadyCheck, no healthcheck: an app that never asked for one must get the argv it
// always got.
func TestAppRunArgs_NoReadyCheckNoHealthFlags(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID: "app",
		ImageMeta: schema.ImageMeta{Image: "localhost/app:local"},
	}
	got := appArgs(t, cfg, baseOpts(), netNone())
	mustNotContain(t, got, "--health-cmd")
	mustNotContain(t, got, "--health-interval")
}

// The probe the app layer polls: one run of the container's own healthcheck, now.
func TestHealthProbeArgs(t *testing.T) {
	if got := HealthProbeArgs("vpn"); !slices.Equal(got, []string{"healthcheck", "run", "vpn"}) {
		t.Fatalf("HealthProbeArgs = %v", got)
	}
}

// A pod owns the user namespace of everything that joins it, and podman refuses --userns on
// a container joining one. An app with KeepUserID and any NetworkList used to get the flag on
// the container and silently never start - StartApp is detached, so podman's refusal went
// nowhere and the app just was not there.
func TestAppRunArgs_KeepUserIDIsThePodsWhenFiltered(t *testing.T) {
	cfg := schema.AppConfig{
		AppNameID:        "app",
		ImageMeta:        schema.ImageMeta{Image: "localhost/app:local"},
		InternalUserMeta: schema.InternalUserMeta{KeepUserID: true},
	}
	// Unfiltered: no pod, so the container carries it.
	if got := appArgs(t, cfg, baseOpts(), netNone()); !slices.Contains(got, "--userns=keep-id") {
		t.Errorf("an unfiltered app keeps its own userns flag, got %v", got)
	}
	// Filtered: joining a pod, so it must not.
	mustNotContain(t, appArgs(t, cfg, baseOpts(), []string{"--pod", "app-pod"}), "--userns=keep-id")
}

// A launch never pulls (--pull never, section 5.5), so a helper image that was never built
// fails with podman's bare "image not known" - a sentence that does not say the image was the
// user's to build. The hint has to name the command.
func TestHelperImageHint_NamesTheBuildCommand(t *testing.T) {
	cmd := ports.Command{Args: []string{"run", "--rm", "zinc/netfilter:local", "true"}}
	got := helperImageHint(cmd, []byte("Error: zinc/netfilter:local: image not known"))
	if !strings.Contains(got, "make -C container/runner netfilter-image") {
		t.Errorf("hint does not name the build command: %q", got)
	}
}

// An app's own image being absent is a different problem with a different fix, and offering the
// netfilter build as the answer would send the user to the wrong place.
func TestHelperImageHint_NotForAnAppsOwnImage(t *testing.T) {
	cmd := ports.Command{Args: []string{"run", "--rm", "docker.io/library/alpine@sha256:abc", "true"}}
	if got := helperImageHint(cmd, []byte("Error: docker.io/library/alpine@sha256:abc: image not known")); got != "" {
		t.Errorf("hint offered for an app's own image: %q", got)
	}
}

// Any other failure must not acquire an image hint.
func TestHelperImageHint_OnlyForMissingImages(t *testing.T) {
	cmd := ports.Command{Args: []string{"run", "zinc/netfilter:local", "nft", "-f", "-"}}
	if got := helperImageHint(cmd, []byte("Error: nft: syntax error")); got != "" {
		t.Errorf("hint offered for an unrelated failure: %q", got)
	}
}

// The pid table is what bus attribution resolves a connection against, so a line podman could
// not fill in has to be dropped rather than recorded: a pid of 0 would match every other
// unfilled line and attribute a bus connection to whichever app happened to sort first.
func TestParsePIDs(t *testing.T) {
	got := parsePIDs("notes 4001\nzinc-dbus-notes 4002\nbroken\nempty \nzero 0\n\n")
	want := map[string]int{"notes": 4001, "zinc-dbus-notes": 4002}
	if len(got) != len(want) {
		t.Fatalf("parsePIDs = %v, want %v", got, want)
	}
	for name, pid := range want {
		if got[name] != pid {
			t.Errorf("parsePIDs[%q] = %d, want %d", name, got[name], pid)
		}
	}
}

// No running containers is an empty table, not a table with one empty entry.
func TestParsePIDsEmpty(t *testing.T) {
	if got := parsePIDs("\n"); len(got) != 0 {
		t.Errorf("parsePIDs of empty output = %v, want nothing", got)
	}
}

// podman refuses --rm together with a restart policy, and it refuses at the CLI layer, so
// nothing is created and the run exits non-zero. StartApp is detached with nil stdio and zcr
// has already exited by then, so that failure is silent: the launch reports success, the app
// never starts, and the pod, the nft ruleset, the proxy and the Wayland holder are all left
// behind. `restart: always` in a compose file imports straight to this.
func TestAppRunArgs_AutorestartNeverPairsWithRm(t *testing.T) {
	for _, mode := range []struct {
		name string
		cfg  func(schema.AppConfig) schema.AppConfig
	}{
		{"foreground", func(cfg schema.AppConfig) schema.AppConfig { return cfg }},
		{"terminal", func(cfg schema.AppConfig) schema.AppConfig {
			cfg.StartConditions.Terminal = true
			return cfg
		}},
		{"multiterminal holder", func(cfg schema.AppConfig) schema.AppConfig {
			cfg.StartConditions.Multiterminal = true
			return cfg
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := mode.cfg(autorestartCfg())
			args, err := Runtime{}.AppRunArgs(cfg, options.HostOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--restart on-failure") {
				t.Fatalf("Autorestart did not reach the argv: %v", args)
			}
			if slices.Contains(args, "--rm") {
				t.Fatalf("--rm together with --restart: podman refuses this argv outright: %v", args)
			}
		})
	}
}

func autorestartCfg() schema.AppConfig {
	cfg := validCfg()
	cfg.StartConditions.Autorestart = true
	return cfg
}
