package netenforce

import (
	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Counters returns the command that reads back what an app's ruleset has seen, and false when there
// is nothing to ask.
//
// It uses the same helper, pod and capability as the step that applied the ruleset: reading nftables
// is not a lesser privilege than writing it, both being one netlink socket needing CAP_NET_ADMIN.
// The one difference is timing - this runs while the app is alive. It joins the pod's network
// namespace only (a pod shares net, ipc and uts, not pid), so it cannot see, signal or ptrace the
// app, and its argv is fixed.
func (Enforcer) Counters(cfg schema.AppConfig, opt options.HostOptions) (ports.Command, bool) {
	if !filtered(cfg) {
		return ports.Command{}, false
	}
	image := opt.NetfilterImage
	if image == "" {
		image = DefaultNetfilterImage
	}
	pod := PodName(cfg.AppNameID)
	return ports.Command{Args: nftListArgs(pod, image), Desc: "read the nft counters in " + pod}, true
}

// nftListArgs builds the one-shot `podman run` that dumps the pod netns' ruleset as JSON.
// It mirrors nftApplyArgs deliberately, minus the stdin: the two must not drift apart on
// which pod they enter or how much privilege they take.
func nftListArgs(pod, image string) []string {
	return []string{
		"run", "--pod", pod, "--rm", "--pull", "never",
		"--security-opt", "no-new-privileges", "--cap-drop", "all", "--cap-add", "NET_ADMIN",
		// --user 0 is root OF THE POD'S user namespace, which is what owns the netns; see
		// nftApplyArgs. A keep-id pod would otherwise answer "Operation not permitted".
		"--user", "0",
		image, "nft", "-j", "list", "ruleset",
	}
}
