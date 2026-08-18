// Package disk prepares what a guest boots from: a copy-on-write overlay over the pinned
// base image, and the cloud-init seed that gives a fresh guest its identity. Both are
// built by shelling out to the tools that own those formats (qemu-img, xorriso) rather
// than by writing qcow2 and ISO9660 by hand.
package disk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// EnsureOverlay makes sure the app has a writable disk backed by its pinned base, and
// that the base is still the one that was authorised. The overlay is created once and
// then left alone: it holds everything the guest has ever written.
func EnsureOverlay(base, digest, overlay string, sizeGiB int64) error {
	if err := VerifyBase(base, digest); err != nil {
		return err
	}
	if _, err := os.Stat(overlay); err == nil {
		return nil // the guest's disk already exists; never re-create it, that is its data
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", overlay, err)
	}

	// -F qcow2 states the backing file's format explicitly. Without it qemu-img probes,
	// and a probed backing format is a known way to confuse the format detection of
	// whatever opens the overlay next.
	args := []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base, overlay}
	if sizeGiB > 0 {
		args = append(args, strconv.FormatInt(sizeGiB, 10)+"G")
	}
	if output, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("create the app's disk overlay: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// VerifyBase checks the base image still hashes to the digest the config pins. Hashed in full on
// first use, and after that only when the file looks changed - hashing a multi-gigabyte image on
// every launch would add seconds to every start.
//
// "Looks changed" is deliberately broad: device and inode, size, and BOTH timestamps. mtime alone is
// too weak twice over - coarse granularity lets a same-size replacement in one tick through, and it
// can be set to anything with utimes. ctime cannot.
//
// It reliably catches a base that was replaced, rebuilt, moved or restored, which is how a pin goes
// stale. It is not a defence against someone who can write to the image directory, since they can
// rewrite this sidecar too.
func VerifyBase(base, digest string) error {
	info, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("base image %s: %w", base, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("base image %s: not a regular file", base)
	}

	// Before the digest is even consulted: a base that references another file cannot be
	// pinned, because the digest covers the reference and not what it resolves to.
	if err := checkSelfContained(base); err != nil {
		return err
	}

	current := identify(info)
	if cached, ok := readSidecar(base); ok && cached.Digest == digest && cached.Identity == current {
		return nil
	}

	sum, err := fileDigest(base)
	if err != nil {
		return err
	}
	if sum != digest {
		return fmt.Errorf("base image %s does not match the pinned digest\n  authorised: %s\n  on disk:    %s\nthe image was replaced or rebuilt; re-pin it in the app config if that was intended",
			base, digest, sum)
	}
	writeSidecar(base, sidecar{Identity: current, Digest: sum})
	return nil
}

// qcow2 header fields this cares about, by byte offset. The layout is fixed and public:
// magic, then the version, then the offset and length of the backing-file name, and for a
// version 3 image a feature bitmap whose bit 1 means "the data lives in a separate file".
const (
	qcowMagicLen        = 4
	qcowBackingOffsetAt = 8
	qcowIncompatibleAt  = 72
	qcowHeaderProbe     = 80
	// Bit 2 of incompatible_features (qemu's QCOW2_INCOMPAT_DATA_FILE_BITNR). Bit 1 is the corrupt flag
	// and is NOT this; getting it wrong silently passes every external-data-file image. Confirmed
	// against a real image: `qemu-img create -o data_file=x.raw,data_file_raw=on` writes 0x04 at 72.
	qcowExternalDataBit = 1 << 2
)

var qcowMagic = []byte{'Q', 'F', 'I', 0xfb}

// checkSelfContained refuses a base image that names another file in its own header. A qcow2 header
// can carry a backing-file or external-data-file pointer, and those bytes are part of what the
// digest covers - so a hostile image pins perfectly and still serves whatever the pointer resolves
// to: any readable file, or a URL, since the nbd and curl drivers are usually compiled in.
//
// The rule is definitional rather than an attempt to follow the chain: an image that is not
// self-contained cannot be pinned. Anything not qcow2 has no such header and passes.
func checkSelfContained(base string) error {
	file, err := os.Open(base)
	if err != nil {
		return fmt.Errorf("base image %s: %w", base, err)
	}
	defer file.Close()

	header := make([]byte, qcowHeaderProbe)
	read, err := io.ReadFull(file, header)
	if err != nil && read < qcowHeaderProbe {
		// Too short to be a qcow2 header at all, so there is nothing here to reference.
		return nil
	}
	if !bytes.Equal(header[:qcowMagicLen], qcowMagic) {
		return nil // raw, or some other format with no backing concept
	}

	refuse := func(what string) error {
		return fmt.Errorf("base image %s declares %s, so it cannot be pinned\n"+
			"BaseDigest covers this file's bytes, and those bytes only point at the real data; "+
			"the pin would keep matching while the guest booted something else.\n"+
			"flatten it first: qemu-img convert -O qcow2 %s <flattened.qcow2>, then re-pin",
			base, what, base)
	}
	if binary.BigEndian.Uint64(header[qcowBackingOffsetAt:]) != 0 {
		return refuse("a backing file")
	}
	if binary.BigEndian.Uint32(header[qcowMagicLen:]) >= 3 &&
		binary.BigEndian.Uint64(header[qcowIncompatibleAt:])&qcowExternalDataBit != 0 {
		return refuse("an external data file")
	}
	return nil
}

// identify fingerprints a file's identity and both timestamps, at nanosecond resolution
// where the filesystem keeps it. Any ordinary write, replacement or restore moves at least
// one of these fields.
func identify(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// No stat details available: return a value that can never match a stored one, so
		// the image is simply re-hashed rather than trusted on weaker evidence.
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d.%09d:%d.%09d",
		stat.Dev, stat.Ino, info.Size(),
		stat.Mtim.Sec, stat.Mtim.Nsec,
		stat.Ctim.Sec, stat.Ctim.Nsec)
}

// sidecar remembers a verified image so an unchanged one is not re-hashed on every boot.
type sidecar struct {
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
}

func sidecarPath(base string) string {
	return filepath.Join(filepath.Dir(base), "."+filepath.Base(base)+".zinc-digest")
}

func readSidecar(base string) (sidecar, bool) {
	data, err := os.ReadFile(sidecarPath(base))
	if err != nil {
		return sidecar{}, false
	}
	var cached sidecar
	if err := json.Unmarshal(data, &cached); err != nil {
		return sidecar{}, false
	}
	return cached, true
}

// writeSidecar is best-effort: a read-only image directory means re-hashing next time,
// which is slower but never wrong, so a failure here is not worth failing a launch over.
func writeSidecar(base string, entry sidecar) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(sidecarPath(base), data, 0o644)
}

// fileDigest returns the file's content hash as "sha256:<hex>".
func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// Digest is the public form of fileDigest, so `zvr pin` can tell an author what to write
// into a config.
func Digest(path string) (string, error) { return fileDigest(path) }

// WriteSeed builds the app's provisioning disc, rebuilt on every launch so editing the config's
// identity fields takes effect without touching the guest's disk.
func WriteSeed(path string, cfg schema.AppConfig) error {
	files, err := seedFiles(cfg)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "zinc-seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	// Sorted, so the same config always builds the same image: map order is not.
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(stage, name), []byte(files[name]), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	// -volid cidata is the whole contract: cloud-init's NoCloud source looks for a volume
	// with exactly that label. Joliet and Rock Ridge keep the names readable to any guest.
	args := []string{
		"-as", "mkisofs", "-output", path, "-volid", "cidata", "-joliet", "-rock",
	}
	for _, name := range names {
		args = append(args, filepath.Join(stage, name))
	}
	if output, err := exec.Command("xorriso", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("build the cloud-init seed image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// seedFiles decides what goes on the disc by what the guest can read. cloud-init takes user-data and
// meta-data; a compatible-profile guest has never heard of it and gets zinc-setup.cmd instead, which
// stages the virtio drivers. Kept separate from writing them so the choice is testable without an
// ISO tool.
func seedFiles(cfg schema.AppConfig) (map[string]string, error) {
	userData, err := userData(cfg)
	if err != nil {
		return nil, err
	}
	metaData, err := metaData(cfg)
	if err != nil {
		return nil, err
	}
	files := map[string]string{"user-data": userData, "meta-data": metaData}
	if cfg.VirtualizationMeta.Devices == schema.VMDevicesCompatible {
		files["zinc-setup.cmd"] = windowsSetup()
	}
	return files, nil
}

// metaData renders meta-data as JSON. cloud-init documents it as YAML and JSON is valid YAML, which
// satisfies the cut-down implementations too: cirros parses it strictly as JSON and rejects a plain
// YAML mapping, found by booting one.
//
// instance-id is how cloud-init decides whether it already provisioned this guest, so keeping it
// stable per app stops a rebuilt seed re-running first-boot steps. public-keys is the EC2-style
// field, read by implementations that never look at user-data.
func metaData(cfg schema.AppConfig) (string, error) {
	document := map[string]any{
		"instance-id":    "zinc-" + cfg.AppNameID,
		"local-hostname": cfg.AppNameID,
	}
	init := cfg.VirtualizationMeta.CloudInit
	if init.SSHKeyPath != "" {
		key, err := publicKey(init.SSHKeyPath)
		if err != nil {
			return "", err
		}
		document["public-keys"] = []string{key}
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded) + "\n", nil
}

// publicKey reads an authorised key, refusing a private one. The config check screens the
// path; this screens the bytes, because the file behind a .pub name is not guaranteed to
// be what the name says - and the seed is handed to the guest.
func publicKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the public key %s: %w", path, err)
	}
	key := strings.TrimSpace(string(data))
	if strings.Contains(key, "PRIVATE KEY") {
		return "", fmt.Errorf("%s contains a PRIVATE key; VirtualizationMeta.CloudInit.SSHKeyPath must be a public key", path)
	}
	return key, nil
}

// userData renders the cloud-config document. The app's Install steps become runcmd
// lines, which is the VM reading of the same field a container turns into its derived
// image's RUN layer: what to add on top of the pinned base.
func userData(cfg schema.AppConfig) (string, error) {
	init := cfg.VirtualizationMeta.CloudInit
	var doc strings.Builder
	doc.WriteString("#cloud-config\n")
	doc.WriteString("hostname: " + cfg.AppNameID + "\n")

	if init.UserName != "" {
		key := ""
		if init.SSHKeyPath != "" {
			authorised, err := publicKey(init.SSHKeyPath)
			if err != nil {
				return "", err
			}
			key = authorised
		}
		doc.WriteString("users:\n")
		doc.WriteString("  - name: " + init.UserName + "\n")
		doc.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
		doc.WriteString("    shell: /bin/bash\n")
		if key != "" {
			doc.WriteString("    ssh_authorized_keys:\n")
			doc.WriteString("      - " + yamlScalar(key) + "\n")
		}
	}

	if len(cfg.ImageMeta.Install) > 0 {
		doc.WriteString("runcmd:\n")
		for _, step := range cfg.ImageMeta.Install {
			doc.WriteString("  - " + yamlScalar(step) + "\n")
		}
	}
	return doc.String(), nil
}

// yamlScalar quotes a value so it cannot be read as YAML structure. Validation already
// rejects control characters in these fields, so single-quoting (with the doubling YAML
// requires) is enough to keep a value a value.
func yamlScalar(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
