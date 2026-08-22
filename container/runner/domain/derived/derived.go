package derived

// Derived images (docs section 5.5, section 9.1). When an app sets ImageMeta.Install, Zinc builds a
// small derived image - `FROM <ImageMeta.Image>` plus one `RUN` layer - and runs the app from that:
// the quick-setup path, without hand-authoring a Containerfile. The base is digest-pinned by section
// 5.5, so the derived image is built from a known one.
//
// Pure policy: which image runs, what its Containerfile says, and how to tell a fresh build from a
// stale one. `podman build` itself is the podman adapter; the trigger is the app layer.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// BuildLabel is the OCI label that stamps a derived image with the fingerprint of
// the inputs it was built from, so a launch can tell a fresh image from a stale one
// without any extra on-disk state.
const BuildLabel = "zinc.build"

// HasInstall reports whether cfg builds a derived image (ImageMeta.Install has at
// least one non-blank step). When false the app runs straight from ImageMeta.Image
// and no build ever happens.
func HasInstall(cfg schema.AppConfig) bool {
	return len(installSteps(cfg.ImageMeta.Install)) > 0
}

// RunImage is the image a container actually runs from: the locally built derived
// image when ImageMeta.Install is set, otherwise ImageMeta.Image itself.
func RunImage(cfg schema.AppConfig) string {
	if HasInstall(cfg) {
		return DerivedImageRef(cfg.AppNameID)
	}
	return cfg.ImageMeta.Image
}

// DerivedImageRef is the local tag of an app's derived image. It is only ever
// referenced locally (built with `-t`, run with `--pull never`), never pulled.
func DerivedImageRef(name string) string {
	return "zinc/app-" + name + ":local"
}

// DerivedContainerfile renders the pinned base plus a single RUN layer. The install line runs through
// the image's own /bin/sh, so a package-manager invocation works as typed. Fed to `podman build` on
// stdin, so no temp file and no host build context.
func DerivedContainerfile(cfg schema.AppConfig) string {
	return "FROM " + cfg.ImageMeta.Image + "\nRUN " + installScript(cfg.ImageMeta.Install) + "\n"
}

// BuildFingerprint identifies a derived image's inputs and is written as the BuildLabel value. A
// launch rebuilds only when the live image's label differs, so a re-pinned base or edited install
// takes effect on the next run.
func BuildFingerprint(cfg schema.AppConfig) string {
	sum := sha256.Sum256([]byte(cfg.ImageMeta.Image + "\n" + installScript(cfg.ImageMeta.Install)))
	return hex.EncodeToString(sum[:])
}

func installSteps(install []string) []string {
	var steps []string
	for _, line := range install {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			steps = append(steps, trimmed)
		}
	}
	return steps
}

// installScript joins the install steps with " && " into the single shell command
// the derived image's one RUN layer carries, so a multi-step setup fails fast -
// exactly as the same steps on one line would.
func installScript(install []string) string {
	return strings.Join(installSteps(install), " && ")
}

// InstallHint suggests the package-manager invocation for an app's base image, keyed
// off well-known image-name families, so a form can show the right syntax (apt for
// debian/ubuntu, apk for alpine, dnf for fedora/rhel, ...). Best-effort UI sugar over
// the image name only; it never constrains what may go into ImageMeta.Install.
func InstallHint(image string) string {
	img := strings.ToLower(image)
	switch {
	case containsAny(img, "ubuntu", "debian", "/buildpack-deps", "mint"):
		return "apt-get update && apt-get install -y <pkg>"
	case containsAny(img, "alpine"):
		return "apk add --no-cache <pkg>"
	case containsAny(img, "fedora", "rockylinux", "almalinux", "/rhel", "centos", "oraclelinux"):
		return "dnf install -y <pkg>"
	case containsAny(img, "archlinux", "/arch:", "manjaro"):
		return "pacman -Sy --noconfirm <pkg>"
	case containsAny(img, "opensuse", "/suse", "sles"):
		return "zypper install -y <pkg>"
	default:
		return "<your image's package manager> install <pkg>"
	}
}

func containsAny(str string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(str, sub) {
			return true
		}
	}
	return false
}
