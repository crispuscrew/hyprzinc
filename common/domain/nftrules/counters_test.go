package nftrules

import (
	"slices"
	"strings"
	"testing"
)

// liveCapture is real output. It was taken with `nft -j list ruleset` inside the pod netns
// of a running filtered app (one egress list allowing 1.1.1.1:443, one declared resolver)
// after three allowed connections, one query to a resolver it had not declared, and some
// traffic the policy refused. The only edit is the line breaks, which Go concatenates back
// into the single line nft actually emits.
//
// Captured rather than written by hand on purpose: the fields this parser skips - metainfo,
// the table, the chain, the uncounted rules - are the ones a hand-written fixture quietly
// omits, and they are most of what it has to get right.
const liveCapture = `{"nftables": [` +
	`{"metainfo": {"version": "1.1.3", "release_name": "Commodore Bullmoose #4", "json_schema_version": 1}}, ` +
	`{"table": {"family": "inet", "name": "zinc", "handle": 1}}, ` +
	`{"chain": {"family": "inet", "table": "zinc", "name": "output", "handle": 1, "type": "filter", "hook": "output", "prio": 0, "policy": "drop"}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 2, "expr": [{"match": {"op": "==", "left": {"meta": {"key": "oif"}}, "right": "lo"}}, {"accept": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 3, "expr": [{"match": {"op": "in", "left": {"ct": {"key": "state"}}, "right": ["established", "related"]}}, {"accept": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 5, "expr": [{"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "1.1.1.1"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "udp", "field": "dport"}}, "right": {"set": [53, 853]}}}, {"accept": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 9, "comment": "undeclared dns udp", "expr": [{"match": {"op": "==", "left": {"payload": {"protocol": "udp", "field": "dport"}}, "right": {"set": [53, 853]}}}, {"counter": {"packets": 1, "bytes": 57}}, {"drop": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 11, "comment": "undeclared dns tcp", "expr": [{"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": {"set": [53, 853]}}}, {"counter": {"packets": 0, "bytes": 0}}, {"drop": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 12, "comment": "list[0] ip tcp", "expr": [{"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "1.1.1.1"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": 443}}, {"counter": {"packets": 3, "bytes": 180}}, {"accept": null}]}}, ` +
	`{"rule": {"family": "inet", "table": "zinc", "chain": "output", "handle": 14, "comment": "default policy", "expr": [{"counter": {"packets": 11, "bytes": 764}}, {"drop": null}]}}` +
	`]}`

// The whole readout, from a real dump: every counted rule in evaluation order, and nothing
// else. The three rules with no counter (loopback, the conntrack fast path, the accept to
// the declared resolver) must not appear at all - reporting them as zero forever would say
// the traffic they carry is not happening.
func TestParseCounters_LiveCapture(t *testing.T) {
	counters, err := ParseCounters([]byte(liveCapture))
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	want := []RuleCounter{
		{Chain: "output", Verdict: "drop", Label: "undeclared dns udp", Packets: 1, Bytes: 57},
		{Chain: "output", Verdict: "drop", Label: "undeclared dns tcp", Packets: 0, Bytes: 0},
		{Chain: "output", Verdict: "accept", Label: "list[0] ip tcp", Packets: 3, Bytes: 180},
		{Chain: "output", Verdict: "drop", Label: "default policy", Packets: 11, Bytes: 764},
	}
	if !slices.Equal(counters, want) {
		t.Fatalf("counters =\n%+v\nwant\n%+v", counters, want)
	}
}

// Zero is an answer, and it is the answer a freshly launched app gives. It must survive the
// round trip as zero rather than as an absent rule, or a working sandbox that has simply
// refused nothing yet would look like one with no rules at all.
func TestParseCounters_ZeroAndEmpty(t *testing.T) {
	zeroed := `{"nftables": [{"rule": {"chain": "output", "handle": 4, "comment": "default policy", ` +
		`"expr": [{"counter": {"packets": 0, "bytes": 0}}, {"drop": null}]}}]}`
	counters, err := ParseCounters([]byte(zeroed))
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	if len(counters) != 1 || counters[0].Packets != 0 || counters[0].Label != "default policy" {
		t.Fatalf("a zeroed counter should still be reported, got %+v", counters)
	}

	// A netns with no ruleset at all: valid output, nothing counted, not an error.
	empty, err := ParseCounters([]byte(`{"nftables": []}`))
	if err != nil {
		t.Fatalf("an empty ruleset is not an error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("an empty ruleset should yield no counters, got %+v", empty)
	}
}

// A rule Zinc did not write is reported rather than dropped: a counter in an app's netns
// that this ruleset did not put there is exactly the thing worth seeing.
func TestParseCounters_UnlabelledRuleIsStillReported(t *testing.T) {
	raw := `{"nftables": [{"rule": {"chain": "input", "handle": 7, ` +
		`"expr": [{"counter": {"packets": 5, "bytes": 300}}, {"accept": null}]}}]}`
	counters, err := ParseCounters([]byte(raw))
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	if len(counters) != 1 || !strings.Contains(counters[0].Label, "7") {
		t.Fatalf("an unlabelled counted rule should be reported by handle, got %+v", counters)
	}
}

// Malformed input must fail loudly. The middle case is the dangerous one: valid JSON that is
// not a ruleset (an error object, another tool's output, a truncated capture) would
// otherwise parse to "no counters" and read as a sandbox that has refused nothing.
func TestParseCounters_Malformed(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":        `nft: command not found`,
		"not nft output":  `{"error": "no such pod"}`,
		"counter is junk": `{"nftables": [{"rule": {"chain": "output", "handle": 2, "expr": [{"counter": "lots"}, {"drop": null}]}}]}`,
	} {
		if _, err := ParseCounters([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}
