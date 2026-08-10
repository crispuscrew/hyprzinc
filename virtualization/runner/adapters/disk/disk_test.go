package disk

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

func vmCfg() schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincVirtualization,
		AppNameID:     "guest",
		VirtualizationMeta: schema.VirtualizationMeta{
			CloudInit: schema.CloudInit{UserName: "player"},
		},
	}
}

func TestUserData_IdentityAndHostname(t *testing.T) {
	doc, err := userData(vmCfg())
	if err != nil {
		t.Fatalf("userData: %v", err)
	}
	for _, want := range []string{"#cloud-config", "hostname: guest", "- name: player", "sudo: ALL=(ALL) NOPASSWD:ALL"} {
		if !strings.Contains(doc, want) {
			t.Errorf("user-data missing %q:\n%s", want, doc)
		}
	}
}

// Install steps are the VM reading of the same field a container turns into its derived
// image's RUN layer, so they must reach the guest as runcmd lines.
func TestUserData_InstallBecomesRuncmd(t *testing.T) {
	cfg := vmCfg()
	cfg.ImageMeta.Install = []string{"dnf install -y steam", "systemctl enable sshd"}
	doc, err := userData(cfg)
	if err != nil {
		t.Fatalf("userData: %v", err)
	}
	if !strings.Contains(doc, "runcmd:") {
		t.Fatalf("user-data has no runcmd section:\n%s", doc)
	}
	for _, step := range cfg.ImageMeta.Install {
		if !strings.Contains(doc, "'"+step+"'") {
			t.Errorf("install step %q should appear quoted in runcmd:\n%s", step, doc)
		}
	}
}

// A value carrying YAML punctuation must stay a value. The document is assembled by
// string building, so anything that could be read as structure has to be quoted.
func TestUserData_ValuesCannotBecomeStructure(t *testing.T) {
	cfg := vmCfg()
	cfg.ImageMeta.Install = []string{"echo it's fine", "true\nnot-a-key: value"}
	doc, err := userData(cfg)
	if err != nil {
		t.Fatalf("userData: %v", err)
	}
	if !strings.Contains(doc, "'echo it''s fine'") {
		t.Errorf("a single quote should be doubled, not left to close the scalar:\n%s", doc)
	}
	// Validation rejects control characters upstream; this checks the renderer does not
	// additionally hand a newline through as though it were structure.
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "not-a-key: value" {
			t.Errorf("an install step escaped its scalar and became a cloud-config key:\n%s", doc)
		}
	}
}

// The seed is handed to the guest, so a private key here would be giving it away. The
// config check screens the path; this screens the bytes actually read, because the file
// behind a .pub path is not guaranteed to be what its name says.
func TestUserData_PrivateKeyContentRefused(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	private := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(keyPath, []byte(private), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := vmCfg()
	cfg.VirtualizationMeta.CloudInit.SSHKeyPath = keyPath

	_, err := userData(cfg)
	if err == nil || !strings.Contains(err.Error(), "PRIVATE key") {
		t.Fatalf("a private key behind a .pub path should be refused, got: %v", err)
	}
}

func TestUserData_PublicKeyAuthorised(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample user@host"
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := vmCfg()
	cfg.VirtualizationMeta.CloudInit.SSHKeyPath = keyPath

	doc, err := userData(cfg)
	if err != nil {
		t.Fatalf("userData: %v", err)
	}
	if !strings.Contains(doc, "ssh_authorized_keys:") || !strings.Contains(doc, key) {
		t.Errorf("the public key should be authorised for the user:\n%s", doc)
	}
}

// A base image that no longer hashes to its pin must stop the launch. This is the file
// equivalent of refusing an unpinned container image.
func TestVerifyBase_RejectsChangedImage(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("original image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatalf("an unchanged image should verify, got: %v", err)
	}

	if err := os.WriteFile(base, []byte("substituted image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = VerifyBase(base, digest)
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned digest") {
		t.Fatalf("a replaced image should be refused, got: %v", err)
	}
}

// qcowHeader builds the first 80 bytes of a qcow2 image: magic, version, the backing-file
// pointer, and the version 3 incompatible-feature bitmap. Written by hand rather than with
// qemu-img so the test says what it means and needs no tool on the box.
func qcowHeader(version uint32, backingOffset uint64, incompatible uint64) []byte {
	header := make([]byte, 80)
	copy(header, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(header[4:], version)
	binary.BigEndian.PutUint64(header[8:], backingOffset)
	binary.BigEndian.PutUint64(header[72:], incompatible)
	return header
}

// A digest covers the bytes of the file it names. A qcow2 header can point at another file,
// and those bytes are inside the digest, so a hostile base pins perfectly and still serves
// whatever the pointer resolves to - any readable host file, or a URL. Refuse an image that
// is not self-contained rather than trying to follow the chain.
func TestVerifyBase_RefusesAnImageThatReferencesAnotherFile(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		header []byte
		want   string
	}{
		{"backing file", qcowHeader(3, 0x200, 0), "a backing file"},
		{"backing file, v2", qcowHeader(2, 0x200, 0), "a backing file"},
		// The literal is deliberate. Deriving it from the production constant would make this
		// test agree with the code by construction, which is exactly how the wrong bit (1<<1,
		// the corrupt flag) shipped green: 0x04 is what qemu-img actually writes at offset 72
		// for `-o data_file=x.raw,data_file_raw=on`.
		{"external data file", qcowHeader(3, 0, 0x04), "an external data file"},
		// The corrupt flag is a different bit and must not be mistaken for this one.
		{"corrupt flag is not a data file", qcowHeader(3, 0, 0x02), ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base.qcow2")
			if err := os.WriteFile(base, testCase.header, 0o644); err != nil {
				t.Fatal(err)
			}
			digest, err := Digest(base)
			if err != nil {
				t.Fatal(err)
			}
			// The digest is correct, which is the point: the pin matches and the image is
			// still refused.
			err = VerifyBase(base, digest)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("this flag is not an external data file and must not be refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("want a refusal naming %q, got: %v", testCase.want, err)
			}
		})
	}
}

// A self-contained qcow2 is the normal case and must still verify.
func TestVerifyBase_SelfContainedImageVerifies(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, qcowHeader(3, 0, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatalf("a self-contained image should verify, got: %v", err)
	}
}

// The sidecar exists so an unchanged multi-gigabyte image is not re-hashed on every
// launch. It must never become a way to skip the check. The hard case is a replacement of
// exactly the same length: size alone cannot see it, and a second-granularity mtime would
// not either if both writes land in the same tick, which is why the recorded identity
// includes the inode and both timestamps at nanosecond resolution.
func TestVerifyBase_SidecarCannotAuthoriseWrongBytes(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecarPath(base)); err != nil {
		t.Fatalf("a verified image should leave a sidecar: %v", err)
	}

	if err := os.WriteFile(base, []byte("replaced"), 0o644); err != nil { // same length
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err == nil {
		t.Fatal("a same-size replacement should still be caught")
	}
}

// Rewinding mtime to the recorded value must not resurrect a stale sidecar: ctime moves
// when the inode does, including when someone sets mtime back.
func TestVerifyBase_ForgedMtimeStillCaught(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(base, []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Put the timestamp back exactly where the sidecar recorded it.
	if err := os.Chtimes(base, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err == nil {
		t.Fatal("restoring mtime must not make a replaced image look verified")
	}
}

// An unchanged image verifies repeatedly, and a metadata-only change forces a re-hash.
//
// The second half is the interesting one, and it is the documented cost of putting ctime in
// the sidecar's identity: chmod, chown, an xattr or a new hardlink all move ctime without
// touching a byte of content, so the cache misses and the whole file is read again. That is
// deliberate - ctime is what makes a swapped-then-restored base detectable - but it means a
// test cannot make the file unreadable to prove the cache was used, because the chmod that
// removes permission is itself what invalidates the entry.
//
// This test previously did exactly that, and passed only because the pinned build container
// ran as root: CAP_DAC_OVERRIDE reopened the 0o000 file, the re-hash succeeded, and the sole
// assertion (no error) held without ever observing whether the cache was consulted. Run as
// any ordinary user it failed. Asserting the re-hash is both true and testable.
func TestVerifyBase_UnchangedImageVerifiesAndMetadataChangeReHashes(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("stable image"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := VerifyBase(base, digest); err != nil {
			t.Fatalf("verify %d of an unchanged image: %v", attempt, err)
		}
	}

	// Metadata-only change: the content is untouched, so a re-hash still matches the pin and
	// the call must succeed. Making it observable is what needs root, which is why the
	// assertion here is that the pin still holds rather than that the file went unread.
	if err := os.Chmod(base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatalf("a chmod moves ctime and forces a re-hash, but the bytes are unchanged so the pin must still hold: %v", err)
	}
}

// Verifying against a digest the config did not pin must fail even when the sidecar
// records a valid hash for the file.
func TestVerifyBase_RejectsDifferentPin(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBase(base, digest); err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("b", 64)
	if err := VerifyBase(base, other); err == nil {
		t.Fatal("a config pinning a different digest must not be satisfied by the sidecar")
	}
}
