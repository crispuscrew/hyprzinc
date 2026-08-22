// Package firmware prepares the two host-side pieces a Windows-class guest needs before qemu
// starts: a per-app copy of the UEFI variable store, and a running TPM 2.0 emulator. Per-app
// because a variable store holds boot entries and a TPM holds keys the guest believes are sealed
// to its own machine.
package firmware

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/virtualization/runner/domain/qemu"
)

// An OVMF build on the host. Secure Boot needs the matching pair: the code half carries the
// signature database, so a secboot VARS with plain CODE boots unusable rather than secured.
//
// tpm is why this is a table rather than a list of paths. Distributions ship a current 4 MB build
// and a legacy 2 MB one, and only the 4 MB build carries the Tcg2 driver. QEMU publishes the TPM's
// ACPI device regardless, so a legacy-build guest binds a driver to nothing: Windows 11 reads
// version 0 and refuses to install, blaming its requirements. Measured on Fedora 43 with edk2-ovmf
// 20260508.
type ovmfBuild struct {
	code, vars string
	format     string // pflash image format: the 4 MB Fedora build is qcow2, the rest raw
	tpm        bool
}

// Newest first, so a host that has both generations gets the one that works.
var ovmfSearch = []ovmfBuild{
	{"/usr/share/edk2/ovmf/OVMF_CODE_4M.qcow2", "/usr/share/edk2/ovmf/OVMF_VARS_4M.qcow2", "qcow2", true},
	{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd", "raw", true},
	{"/usr/share/edk2/x64/OVMF_CODE.4m.fd", "/usr/share/edk2/x64/OVMF_VARS.4m.fd", "raw", true},
	{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd", "raw", false},
	{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd", "raw", false},
	{"/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin", "raw", false},
}

var ovmfSecbootSearch = []ovmfBuild{
	{"/usr/share/edk2/ovmf/OVMF_CODE_4M.secboot.qcow2", "/usr/share/edk2/ovmf/OVMF_VARS_4M.secboot.qcow2", "qcow2", true},
	{"/usr/share/OVMF/OVMF_CODE_4M.secboot.fd", "/usr/share/OVMF/OVMF_VARS_4M.ms.fd", "raw", true},
	{"/usr/share/edk2/x64/OVMF_CODE.secboot.4m.fd", "/usr/share/edk2/x64/OVMF_VARS.4m.fd", "raw", true},
	{"/usr/share/edk2/ovmf/OVMF_CODE.secboot.fd", "/usr/share/edk2/ovmf/OVMF_VARS.secboot.fd", "raw", false},
	{"/usr/share/OVMF/OVMF_CODE.secboot.fd", "/usr/share/OVMF/OVMF_VARS.secboot.fd", "raw", false},
}

// Prepare resolves the firmware for an app, copying the variable store on first use.
//
// baseImage matters: an OS installed under UEFI records its boot entry in NVRAM, not on disk, so
// the store `zvr install` leaves beside its disk is what seeds the app. Copying the pristine OVMF
// template instead would leave a freshly installed guest at the UEFI shell with no boot device.
func Prepare(virt schema.VirtualizationMeta, varsPath, baseImage string) (qemu.Firmware, error) {
	if virt.Firmware != schema.VMFirmwareUEFI {
		return qemu.Firmware{}, nil
	}

	search := ovmfSearch
	kind := "UEFI"
	if virt.SecureBoot {
		search, kind = ovmfSecbootSearch, "UEFI Secure Boot"
	}
	var build ovmfBuild
	for _, candidate := range search {
		if fileExists(candidate.code) && fileExists(candidate.vars) {
			build = candidate
			break
		}
	}
	if build.code == "" {
		return qemu.Firmware{}, fmt.Errorf(
			"%s firmware (OVMF) not found on this host; install the edk2-ovmf package (Fedora) or ovmf (Debian), "+
				"or set Firmware: BIOS if the guest can boot that way", kind)
	}
	if virt.TPM && !build.tpm {
		// Not fatal: a Linux guest drives the TPM through its own MMIO probe and works on
		// this build regardless. Windows does not, so say which one it is now rather than
		// leave the operator with an installer that only says the PC is unsupported.
		fmt.Fprintf(os.Stderr,
			"warning: the only OVMF build on this host is the legacy %s, which does not hand the TPM to the guest.\n"+
				"         a Linux guest is unaffected; Windows will report that this PC does not meet its requirements.\n"+
				"         install the 4 MB build (edk2-ovmf on Fedora 41+, ovmf on Debian 12+) to fix it.\n",
			filepath.Base(build.code))
	}

	template := build.vars
	if !fileExists(varsPath) {
		if err := os.MkdirAll(filepath.Dir(varsPath), 0o755); err != nil {
			return qemu.Firmware{}, err
		}
		if installed := InstalledVars(baseImage); installed != "" && installed != varsPath {
			// Adopting the store beside the base image is what carries a Windows install's boot entry across.
			// It is also the one input nothing pins - BaseDigest covers the disk, not this file - so check
			// what can be checked before adopting it.
			if err := matchesBuild(installed, build.vars); err != nil {
				return qemu.Firmware{}, fmt.Errorf("the variable store beside %s cannot be used: %w", baseImage, err)
			}
			if virt.SecureBoot {
				// A store carries PK, KEK and db, which together ARE the Secure Boot policy. Adopting an unverified
				// one means the config says Secure Boot while the guest enforces whatever the file says, including
				// nothing at all if it has no PK. No attacker needed: install without --secure-boot, then author
				// the app with SecureBoot: true.
				return qemu.Firmware{}, fmt.Errorf(
					"SecureBoot is set, but the variable store beside %s would be adopted as this app's firmware state.\n"+
						"that file holds PK/KEK/db, so it decides what Secure Boot actually enforces, and nothing pins it "+
						"the way BaseDigest pins the disk.\n"+
						"either clear SecureBoot, or delete %s so the app starts from this host's own secure-boot template "+
						"(a guest installed against the other store has to be reinstalled)",
					baseImage, installed)
			}
			template = installed
		}
		data, err := os.ReadFile(template)
		if err != nil {
			return qemu.Firmware{}, fmt.Errorf("read the UEFI variable template: %w", err)
		}
		if err := os.WriteFile(varsPath, data, 0o600); err != nil {
			return qemu.Firmware{}, fmt.Errorf("create this app's UEFI variable store: %w", err)
		}
	} else if err := matchesBuild(varsPath, template); err != nil {
		return qemu.Firmware{}, err
	}
	return qemu.Firmware{CodePath: build.code, VarsPath: varsPath, Format: build.format}, nil
}

// matchesBuild rejects a store belonging to a different OVMF build. The generations disagree on
// format and size (2 MB code pairs with a 128 KiB store, 4 MB with 512 KiB), and qemu handed a
// mismatched pair warns and boots a guest whose Secure Boot state is quietly wrong.
func matchesBuild(varsPath, template string) error {
	haveFormat, haveSize, err := pflashShape(varsPath)
	if err != nil {
		return fmt.Errorf("inspect this app's UEFI variable store: %w", err)
	}
	wantFormat, wantSize, err := pflashShape(template)
	if err != nil {
		return fmt.Errorf("inspect the UEFI variable template: %w", err)
	}
	if haveFormat != wantFormat || haveSize != wantSize {
		return fmt.Errorf(
			"the UEFI variable store %s is %s/%d bytes but this host's firmware needs %s/%d bytes: "+
				"it was created by a different OVMF build. delete it (or `zvr reset` the app) to have it recreated; "+
				"a guest installed against the old firmware has to be reinstalled",
			varsPath, haveFormat, haveSize, wantFormat, wantSize)
	}
	return nil
}

// pflashShape reports a pflash image's format and the size the firmware sees. For qcow2 that
// is the virtual size from the header, not the file size, because qemu grows the file as the
// guest writes variables into it.
func pflashShape(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	var header [32]byte
	read, err := io.ReadFull(file, header[:])
	if err != nil && read < len(header) {
		// Too short to be qcow2, so it can only be a raw image.
		info, statErr := file.Stat()
		if statErr != nil {
			return "", 0, statErr
		}
		return "raw", info.Size(), nil
	}
	if string(header[:4]) == "QFI\xfb" {
		return "qcow2", int64(binary.BigEndian.Uint64(header[24:32])), nil
	}
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	return "raw", info.Size(), nil
}

// InstalledVars is where `zvr install` leaves the UEFI variables belonging to a disk it
// installed: beside the disk, so they travel with it. Empty when there are none.
func InstalledVars(baseImage string) string {
	if baseImage == "" {
		return ""
	}
	candidate := baseImage + ".uefi-vars.fd"
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

// TPM is a running swtpm process backing one guest's emulated TPM 2.0.
type TPM struct {
	SocketPath string
	pidPath    string
}

// StartTPM launches swtpm for an app and waits for its socket. Windows 11 refuses to
// install without a TPM, so a failure here has to be reported plainly rather than left for
// the installer to express as a vague compatibility complaint.
func StartTPM(stateDir, socketPath, pidPath string) (*TPM, error) {
	if _, err := exec.LookPath("swtpm"); err != nil {
		return nil, fmt.Errorf("swtpm not found on $PATH: an emulated TPM needs it (install the swtpm package), " +
			"or set TPM: false if the guest does not require one")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	// A stale socket from a guest that died blocks the bind, the same way qemu's own
	// sockets do.
	_ = os.Remove(socketPath)

	// The socket qemu is given must be swtpm's CONTROL channel, not its data channel: qemu's tpmdev
	// speaks the control protocol and swtpm hands the data channel back through it. Pointed at
	// --server, qemu blocks forever on a reply that never comes and never opens its window.
	command := exec.Command("swtpm", "socket",
		"--tpmstate", "dir="+stateDir,
		"--ctrl", "type=unixio,path="+socketPath,
		"--tpm2",
		"--flags", "startup-clear",
		"--daemon",
		"--pid", "file="+pidPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start the TPM emulator: %w: %s", err, strings.TrimSpace(string(output)))
	}

	// --daemon returns immediately; the socket appears a moment later, and qemu fails hard
	// if it connects first.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(socketPath) {
			return &TPM{SocketPath: socketPath, pidPath: pidPath}, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("the TPM emulator did not create its socket at %s within 3s", socketPath)
}

// StopTPM terminates the emulator for an app and clears what it leaves behind. Called on
// every stop, including for guests that never had one, so a missing pidfile is not an
// error.
func StopTPM(socketPath, pidPath string) {
	if data, err := os.ReadFile(pidPath); err == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 && isSwtpm(pid) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	_ = os.Remove(pidPath)
	_ = os.Remove(socketPath)
}

// isSwtpm confirms a pid really is a TPM emulator before it is signalled. A stale pidfile
// outlives the process it names, pids are recycled, and the guest supervisor already
// refuses to signal on a pidfile alone - this is the same rule.
func isSwtpm(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc cmdline is NUL-separated, so compare argv ELEMENTS rather than searching the joined blob:
	// a substring search matches any process merely mentioning "swtpm" - an editor, a grep, a build
	// log - which after pid reuse means SIGTERM to something unrelated.
	argv := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(argv) == 0 {
		return false
	}
	return filepath.Base(argv[0]) == "swtpm"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
