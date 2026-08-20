// Package paths decides where a VM app's files live. It is pure string work over a
// resolved home and runtime directory, so the layout is unit-testable and every command
// agrees on where to look: `zvr run` creates the overlay the same place `zvr reset`
// deletes it and `zvr ps` probes for a pid.
package paths

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/crispuscrew/zinc/virtualization/runner/domain/qemu"
)

// Paths is the three directories a VM app touches. State and images persist; run holds
// only what is meaningful while a guest is alive, which is why it sits in the runtime
// directory the session cleans up rather than in the user's data.
type Paths struct {
	StateDir string // per-app overlays and seed ISOs
	ImageDir string // base disk images, shared by every app that pins one
	RunDir   string // pidfiles and control sockets for running guests
}

// Default resolves the layout from the XDG environment.
func Default() (Paths, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve home directory: %w", err)
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	runDir := os.Getenv("XDG_RUNTIME_DIR")
	if runDir == "" {
		// Falling back to a world-writable /tmp path would put a guest's control socket
		// somewhere another user could reach it, and that socket is a power button.
		return Paths{}, fmt.Errorf("XDG_RUNTIME_DIR is not set; zvr needs a private runtime directory for guest control sockets")
	}
	return Paths{
		StateDir: filepath.Join(dataHome, "zinc", "vms"),
		ImageDir: filepath.Join(dataHome, "zinc", "images"),
		RunDir:   filepath.Join(runDir, "zinc", "vm"),
	}, nil
}

// Overlay is the app's own copy-on-write disk: everything the guest writes lands here,
// never in the pinned base, so deleting this file resets the app to its authored image.
func (paths Paths) Overlay(app string) string {
	return filepath.Join(paths.StateDir, app+".qcow2")
}

// Seed is the app's cloud-init seed ISO.
func (paths Paths) Seed(app string) string {
	return filepath.Join(paths.StateDir, app+"-seed.iso")
}

// Log is where a guest's qemu process writes its own diagnostics (not the guest's console,
// which goes to the serial socket).
func (paths Paths) Log(app string) string {
	return filepath.Join(paths.StateDir, app+".log")
}

func (paths Paths) PIDFile(app string) string { return filepath.Join(paths.RunDir, app+".pid") }
func (paths Paths) QMP(app string) string     { return filepath.Join(paths.RunDir, app+".qmp") }
func (paths Paths) Serial(app string) string  { return filepath.Join(paths.RunDir, app+".serial") }

// Resolv is the resolver a filtered guest's qemu reads. See netns.Command.
func (paths Paths) Resolv(app string) string { return filepath.Join(paths.RunDir, app+".resolv.conf") }

// Layout gathers the paths qemu itself needs. seeded is false for an app whose cloud-init
// is disabled, which leaves the seed drive off the command line entirely.
func (paths Paths) Layout(app string, seeded bool) qemu.Layout {
	layout := qemu.Layout{
		Overlay: paths.Overlay(app),
		PIDFile: paths.PIDFile(app),
		QMP:     paths.QMP(app),
		Serial:  paths.Serial(app),
	}
	if seeded {
		layout.Seed = paths.Seed(app)
	}
	return layout
}

// EnsureDirs creates the directories a launch writes into. The runtime directory is
// created 0700: it holds the QMP sockets, and reaching one of those is reaching the
// guest's power button and its memory.
func (paths Paths) EnsureDirs() error {
	for _, dir := range []string{paths.StateDir, paths.ImageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(paths.RunDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", paths.RunDir, err)
	}
	return nil
}

// VirglPrefix is where a venus-capable virglrenderer is expected to live. Distributions
// ship virglrenderer built WITHOUT venus (Fedora 43's has no venus symbols at all), so a
// guest that wants Vulkan needs one built with -Dvenus=true, and zvr has to point qemu at
// it rather than at the system copy. ZVR_VIRGL_PREFIX overrides the default.
func (paths Paths) VirglPrefix() string {
	if override := os.Getenv("ZVR_VIRGL_PREFIX"); override != "" {
		return override
	}
	return filepath.Join(filepath.Dir(paths.StateDir), "virgl-venus")
}

// VenusEnv returns the environment qemu needs to use that virglrenderer, and reports
// whether it is actually installed. Two variables, because venus needs both halves: the
// library qemu loads, and the helper binary it forks - whose path is compiled into the
// library, so a venus-capable library would otherwise still exec the distro's venus-less
// render server.
func (paths Paths) VenusEnv() ([]string, error) {
	prefix := paths.VirglPrefix()
	library := filepath.Join(prefix, "lib64", "libvirglrenderer.so.1")
	server := filepath.Join(prefix, "libexec", "virgl_render_server")
	for _, required := range []string{library, server} {
		if _, err := os.Stat(required); err != nil {
			return nil, fmt.Errorf(
				"guest Vulkan needs a virglrenderer built with venus support, which was not found at %s\n"+
					"  missing: %s\n"+
					"Distributions ship virglrenderer without venus. Build one with:\n"+
					"  make -C virtualization/runner virgl-venus\n"+
					"which is the same build against a PINNED commit. Doing it by hand from the tag\n"+
					"alone runs whatever that mutable ref points at today, and the result is a library\n"+
					"qemu loads with its seccomp sandbox already disabled for Vulkan.\n"+
					"Or set ZVR_VIRGL_PREFIX to an existing one, or turn VirtualizationMeta.Vulkan off.",
				prefix, required)
		}
	}
	return []string{
		"LD_LIBRARY_PATH=" + filepath.Join(prefix, "lib64"),
		"RENDER_SERVER_EXEC_PATH=" + server,
	}, nil
}

// UEFIVars is this app's writable UEFI variable store. Per app because UEFI keeps boot
// entries there: a shared store would let one guest's boot configuration overwrite
// another's.
func (paths Paths) UEFIVars(app string) string {
	return filepath.Join(paths.StateDir, app+"-uefi-vars.fd")
}

// TPMState is where this app's emulated TPM keeps its persistent state - the keys a guest
// believes are sealed to its own machine, which is exactly why it is not shared.
func (paths Paths) TPMState(app string) string {
	return filepath.Join(paths.StateDir, app+"-tpm")
}

func (paths Paths) TPMSocket(app string) string { return filepath.Join(paths.RunDir, app+".tpm") }
func (paths Paths) TPMPID(app string) string    { return filepath.Join(paths.RunDir, app+".tpm.pid") }
