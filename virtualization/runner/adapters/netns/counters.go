package netns

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/crispuscrew/zinc/common/domain/nftrules"
)

// Namespaced reports whether a running guest is in a network namespace of its own.
//
// Observed rather than read from the config, because `zvr net` is an attestation surface: editing
// a YAML must not change what is reported about a guest that is already running. It also decides
// whether reading is safe at all - an unwrapped guest is in the host's namespace, where the same
// question would answer with the host's own ruleset.
func Namespaced(pid int) (bool, error) {
	guest, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "net"))
	if err != nil {
		return false, fmt.Errorf("read the guest's network namespace: %w", err)
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return false, fmt.Errorf("read this process's network namespace: %w", err)
	}
	return guest != self, nil
}

// Counters reads back what a guest's ruleset has seen, as nft's JSON.
//
// --preserve-credentials is not optional: the namespace maps this user to its uid 0, and without
// the flag nsenter attempts a setgroups the kernel refuses. --user as well as --net because
// reading nftables needs CAP_NET_ADMIN in the user namespace that owns the netns. Measured - the
// other two forms fail with EPERM, which reads as "no ruleset" if it is not distinguished.
func Counters(pid int) ([]byte, error) {
	out, err := exec.Command("nsenter", countersArgs(pid)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("read the guest's ruleset: %s", exit.Stderr)
		}
		return nil, fmt.Errorf("read the guest's ruleset: %w", err)
	}
	return out, nil
}

func countersArgs(pid int) []string {
	return []string{
		"--target", strconv.Itoa(pid), "--net", "--user", "--preserve-credentials",
		"nft", "-j", "list", "table", "inet", nftrules.TableName,
	}
}
