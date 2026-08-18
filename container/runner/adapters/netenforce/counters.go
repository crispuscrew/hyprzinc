package netenforce

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// RuleCounter is one counted rule's reading: where it sits, what it does, its label (pasta.go,
// "counters") and what it has seen. A counter lives in the pod's netns and is created with it, so it
// reads "since this launch" - `zcr stop`, `zcr restart` and a failed launch all remove the pod.
type RuleCounter struct {
	Chain   string `json:"chain"`
	Verdict string `json:"verdict"`
	Label   string `json:"label"`
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

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

// verdicts are the terminal statements a counted rule can end in. Listed rather than
// inferred, so a statement this ruleset does not emit is reported as an empty verdict
// instead of being mistaken for one it does.
var verdicts = []string{"accept", "drop", "reject", "return", "jump", "goto"}

// ruleJSON is the part of `nft -j list ruleset` this needs. The full schema is large and versioned,
// so decoding only these fields lets a ruleset carrying anything else still parse - the helper
// image's nft is upgraded independently of this code.
//
// expr stays a list of raw one-key objects because that is what it is, and a verdict is spelled
// `{"accept": null}`: a typed pointer would decode that null to nil and lose the fact that the key
// was there.
type ruleJSON struct {
	Chain   string                       `json:"chain"`
	Handle  int                          `json:"handle"`
	Comment string                       `json:"comment"`
	Expr    []map[string]json.RawMessage `json:"expr"`
}

// ParseCounters returns one entry per counted rule in ruleset order, which is evaluation order.
// Rules without a counter are skipped rather than reported as zero: they are the plumbing the ruleset
// left bare, and a permanent zero would suggest their traffic is not happening.
func ParseCounters(raw []byte) ([]RuleCounter, error) {
	var doc struct {
		Nftables []struct {
			Rule *ruleJSON `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading nft JSON: %w", err)
	}
	// A valid JSON document with no "nftables" key is not an empty ruleset, it is something
	// else entirely - an error object, another tool's output, a truncated capture. Returning
	// "no counters" for it would report a silent zero as though the sandbox had been asked.
	if doc.Nftables == nil {
		return nil, fmt.Errorf("not nft JSON output: no \"nftables\" key")
	}
	counters := []RuleCounter{}
	for _, entry := range doc.Nftables {
		if entry.Rule == nil {
			continue // a table, a chain, or the metainfo header
		}
		counter, found, err := counterOf(entry.Rule.Expr)
		if err != nil {
			return nil, fmt.Errorf("chain %s, rule handle %d: %w", entry.Rule.Chain, entry.Rule.Handle, err)
		}
		if !found {
			continue
		}
		counters = append(counters, RuleCounter{
			Chain:   entry.Rule.Chain,
			Verdict: verdictOf(entry.Rule.Expr),
			Label:   labelOf(*entry.Rule),
			Packets: counter.Packets,
			Bytes:   counter.Bytes,
		})
	}
	return counters, nil
}

// counterStats is what a counter statement carries.
type counterStats struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// counterOf pulls the counter statement out of one rule's expression list.
func counterOf(expr []map[string]json.RawMessage) (counterStats, bool, error) {
	var stats counterStats
	for _, statement := range expr {
		raw, ok := statement["counter"]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &stats); err != nil {
			return stats, false, fmt.Errorf("counter: %w", err)
		}
		return stats, true, nil
	}
	return stats, false, nil
}

// verdictOf names what the rule does with a packet it matched, or "" for a rule that only
// counts. First match wins, which is also last: a verdict is terminal.
func verdictOf(expr []map[string]json.RawMessage) string {
	for _, statement := range expr {
		for _, verdict := range verdicts {
			if _, ok := statement[verdict]; ok {
				return verdict
			}
		}
	}
	return ""
}

// labelOf is the rule's comment, which is how the ruleset names its own rules. A counted
// rule with no comment did not come from here; it is reported by handle rather than dropped,
// because a counter in an app's netns that Zinc did not put there is worth seeing.
func labelOf(rule ruleJSON) string {
	if rule.Comment != "" {
		return rule.Comment
	}
	return "unlabelled rule " + strconv.Itoa(rule.Handle)
}
