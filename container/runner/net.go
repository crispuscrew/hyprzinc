package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/crispuscrew/zinc/container/runner/adapters/netenforce"
	"github.com/crispuscrew/zinc/container/runner/app"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// countersNote is said in every readout, in both forms, because the number invites exactly
// one wrong reading and nothing else in the output prevents it.
const countersNote = "counters live in the pod's netns and are created with it: these are since this launch, not lifetime totals"

// postureFiltered / postureIsolated are the two postures a running app can be in. Not "unfiltered":
// an app with no NetworkLists gets --network none, the MOST restricted posture there is. What it
// lacks is a netns of its own and therefore a ruleset.
const (
	postureFiltered = "filtered"
	postureIsolated = "isolated"
)

// netEntry is one running app's network posture, addressed the way a person types it.
type netEntry struct {
	Address  string `json:"address"`
	App      string `json:"app"`
	Instance string `json:"instance,omitempty"`
	Posture  string `json:"posture"`
	Netns    string `json:"netns,omitempty"`
}

// netReport is one app's counter readout: the same identity fields, plus what its ruleset
// has seen.
type netReport struct {
	netEntry
	Note     string                   `json:"note,omitempty"`
	Counters []netenforce.RuleCounter `json:"counters"`
}

// cmdNet answers both questions a desktop shell asks about an app's network: which running apps have
// a locked netns (no argument), and what one app's ruleset has counted (an argument). One verb
// because the second is a drill-down into a row of the first, and the pair shares what is easy to get
// wrong - resolving a runtime name back to an address, and not describing an isolated app as if it
// had a ruleset.
func cmdNet(svc app.Service, opt options.HostOptions, argv []string) error {
	var name string
	asJSON := false
	for _, arg := range argv {
		switch {
		case arg == "--json":
			asJSON = true
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unknown flag %q\n%s", arg, netUsage)
		case name == "":
			name = arg
		default:
			return fmt.Errorf("unexpected argument %q\n%s", arg, netUsage)
		}
	}
	if name == "" {
		return netList(svc, asJSON)
	}
	return netCounters(svc, opt, name, asJSON)
}

const netUsage = "usage: zcr net [app[@instance]] [--json]"

// netList prints the network posture of every running Zinc app.
func netList(svc app.Service, asJSON bool) error {
	entries, err := netEntries(svc)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(struct {
			Apps []netEntry `json:"apps"`
		}{entries})
	}
	return printEntries(entries)
}

func printEntries(entries []netEntry) error {
	if len(entries) == 0 {
		return nil // same as `zcr ps`: nothing running is not an error and not a message
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ADDRESS\tPOSTURE\tNETNS")
	for _, entry := range entries {
		netns := entry.Netns
		if netns == "" {
			netns = "-"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", entry.Address, entry.Posture, netns)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	// The legend is not decoration. Which of the two words means "locked down" is the whole
	// security content of this table, and it is not guessable from the words alone.
	fmt.Println()
	fmt.Println("filtered: has NetworkLists, so a pod netns of its own with the nft ruleset locked in it")
	fmt.Println("          (zcr net <app> reads its counters)")
	fmt.Println("isolated: no NetworkLists, so --network none - it reaches only its own localhost,")
	fmt.Println("          and has no netns of its own and no ruleset to count")
	return nil
}

func netCounters(svc app.Service, opt options.HostOptions, name string, asJSON bool) error {
	cfg, err := loadApp(svc, name)
	if err != nil {
		return err
	}
	addr, err := paths.ParseAddress(name)
	if err != nil {
		return err
	}
	if strings.Contains(name, "/") || strings.HasSuffix(name, ".yaml") {
		// The argument was a file, not an address (loadApp accepts both). A path is not an
		// identity, so report the one the app carries.
		addr = paths.Address{App: cfg.AppNameID}
	}
	// An empty list, never a null: a consumer iterating the counters of an isolated app
	// should find none, not have to handle the absence of the field as a third case.
	report := netReport{
		netEntry: entryFor(addr, cfg.AppNameID, len(cfg.NetworkMeta.NetworkLists) > 0),
		Counters: []netenforce.RuleCounter{},
	}
	raw, filtered, err := svc.NetCounters(cfg, opt)
	if err != nil {
		return err
	}
	if filtered {
		report.Note = countersNote // said only where there are numbers to misread
		if report.Counters, err = netenforce.ParseCounters([]byte(raw)); err != nil {
			return fmt.Errorf("%s: %w", cfg.AppNameID, err)
		}
	}
	if asJSON {
		return writeJSON(report)
	}
	return printReport(report)
}

func printReport(report netReport) error {
	fmt.Printf("address: %s\n", report.Address)
	fmt.Printf("posture: %s\n", report.Posture)
	if report.Posture != postureFiltered {
		fmt.Println("this app declares no NetworkLists, so it runs with --network none: no netns of")
		fmt.Println("its own, no ruleset, and nothing to count.")
		return nil
	}
	fmt.Printf("netns:   %s\n", report.Netns)
	fmt.Printf("note:    %s\n", countersNote)
	fmt.Println()
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "CHAIN\tVERDICT\tRULE\tPACKETS\tBYTES")
	for _, counter := range report.Counters {
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\n",
			counter.Chain, counter.Verdict, counter.Label, counter.Packets, counter.Bytes)
	}
	return table.Flush()
}

// netEntries lists the posture of every running app, sorted by address. It enumerates what is RUNNING
// rather than what is defined, because a netns exists only while its pod does. Anything running that
// is not a defined app is skipped: `podman ps` also holds Zinc's own proxies and whatever else the
// user runs.
func netEntries(svc app.Service) ([]netEntry, error) {
	defined, err := svc.List()
	if err != nil {
		return nil, err
	}
	running, err := svc.Running()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(running))
	for name, up := range running {
		if up {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	entries := []netEntry{}
	for _, name := range names {
		addr, ok := addressOf(name, defined)
		if !ok {
			continue
		}
		cfg, err := svc.LoadResolved(addr.App)
		if err != nil {
			// Loud rather than skipped. An app missing from this list reads as "not running",
			// and the answer to "is anything unfiltered" must not be shortened by a file that
			// happens to be unreadable.
			return nil, fmt.Errorf("%s is running but its definition could not be read: %w", addr, err)
		}
		entries = append(entries, entryFor(addr, addr.Runtime(), len(cfg.NetworkMeta.NetworkLists) > 0))
	}
	return entries, nil
}

// entryFor builds one row. The netns is named, not probed: a filtered app's container joins
// its pod to get a network at all, so a running filtered app has that pod by construction.
func entryFor(addr paths.Address, runtime string, filtered bool) netEntry {
	entry := netEntry{Address: addr.String(), App: addr.App, Instance: addr.Instance, Posture: postureIsolated}
	if filtered {
		entry.Posture = postureFiltered
		entry.Netns = netenforce.PodName(runtime)
	}
	return entry
}

// addressOf is the inverse of paths.Address.Runtime, and it needs the defined set: the runtime form
// joins app and instance with a dot and an app name may contain one, so "media.server" is either an
// app or an instance of one. An exact match wins; otherwise the part before the LAST dot must be a
// defined app, since an instance name may not contain a dot.
func addressOf(runtime string, defined []string) (paths.Address, bool) {
	known := make(map[string]bool, len(defined))
	for _, name := range defined {
		known[name] = true
	}
	if known[runtime] {
		return paths.Address{App: runtime}, true
	}
	cut := strings.LastIndex(runtime, paths.Separator)
	if cut <= 0 {
		return paths.Address{}, false
	}
	appName, instance := runtime[:cut], runtime[cut+len(paths.Separator):]
	if instance == "" || !known[appName] {
		return paths.Address{}, false
	}
	return paths.Address{App: appName, Instance: instance}, true
}

// writeJSON prints one value as indented JSON - the machine-readable half, for a desktop
// shell scripting against this rather than reading it.
func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
