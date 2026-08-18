package main

// Bus attribution: turning a connection on the HOST session bus back into the Zinc app and instance
// it belongs to, plus the reporting half of `zcr where`.
//
// A bus name per app is not available: the proxy is a relay with no bus identity, so `--own` would
// grant THE APP the name and the app would have to claim it - a self-asserted identity, which is
// what attribution exists to stop trusting. Zinc publishes the mapping it holds by construction
// instead, having created and named the proxy itself.
//
// The chain: connection -> pid (the bus reads SO_PEERCRED, solid), pid -> proxy container (live
// runtime list, solid when read, best-effort across pid reuse), proxy -> app@instance (the name
// Zinc gave it).
//
// Measured: xdg-dbus-proxy opens ONE upstream connection PER CLIENT, so an app with no live bus
// client contributes nothing and one with several contributes several names on one pid.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/app"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// busReport is what an app's filtered bus is made of on the host: the socket the app connects
// to, and the container Zinc runs to serve it. Nil (JSON null) for an app with no DBusMeta,
// which has neither - saying so is the point, since printing a path that was never created
// would send a consumer looking for a file that cannot exist.
type busReport struct {
	Socket string `json:"socket"`
	Proxy  string `json:"proxy"`
}

// whereReport is `zcr where`'s answer: the layout of one instance, whether or not it is
// running. Deliberately carries nothing read from the runtime (no pid, no up/down) - every
// field is decided before the app starts, so this keeps answering when podman does not.
type whereReport struct {
	Address   string     `json:"address"`
	App       string     `json:"app"`
	Instance  string     `json:"instance"`
	Container string     `json:"container"`
	State     string     `json:"state"`
	Bus       *busReport `json:"bus"`
}

// busRow is one line of the attribution table: a running proxy, the pid that identifies it on
// the host, and the app@instance it was created for. Flat rather than nesting busReport,
// because this is the shape a consumer indexes BY pid, and a pid two levels down in a
// per-record object is awkward to select on.
type busRow struct {
	Address   string `json:"address"`
	App       string `json:"app"`
	Instance  string `json:"instance"`
	Container string `json:"container"`
	Proxy     string `json:"proxy"`
	PID       int    `json:"pid"`
	Socket    string `json:"socket"`
}

// whereOf builds the report for one address. hasBus is whether the app config asked for a
// filtered bus; the caller has the config, and passing the one bit keeps this pure enough to
// test the output shape without a store.
func whereOf(addr paths.Address, hasBus bool, runtimeDir string) (whereReport, error) {
	stateDir, err := paths.StateDir(addr)
	if err != nil {
		return whereReport{}, err
	}
	report := whereReport{
		Address:   addr.String(),
		App:       addr.App,
		Instance:  addr.Instance,
		Container: addr.Runtime(),
		State:     stateDir,
	}
	if hasBus {
		// The socket is empty when XDG_RUNTIME_DIR is unset, which is the same condition that
		// makes launching this app fail outright ("DBusMeta needs XDG_RUNTIME_DIR set"). The
		// proxy name is still reported, because it is decided by the app name alone.
		report.Bus = &busReport{
			Socket: dbusproxy.HostSocketPath(runtimeDir, addr.Runtime()),
			Proxy:  dbusproxy.ContainerName(addr.Runtime()),
		}
	}
	return report, nil
}

// renderWhere is the human form: one "label: value" per line, the labels being the contract
// for anything cutting on the colon. Every label is always present, including for an app with
// no bus, so a reader never has to tell "no bus" apart from "this zcr is older than the
// field".
func renderWhere(report whereReport) string {
	socket, proxy := "none", "none"
	if report.Bus != nil {
		socket, proxy = report.Bus.Socket, report.Bus.Proxy
		if socket == "" {
			socket = "unknown (XDG_RUNTIME_DIR is unset, so this app could not launch here either)"
		}
	}
	return fmt.Sprintf("state: %s\ncontainer: %s\nbus-socket: %s\nbus-proxy: %s\n",
		report.State, report.Container, socket, proxy)
}

// busRows builds the attribution table, one row per running proxy. Sorted by address, so two calls
// produce the same bytes.
func busRows(pids map[string]int, runtimeDir string, defined func(string) bool) []busRow {
	rows := []busRow{} // never nil: the JSON form of "nothing running" must be [], not null
	for container, pid := range pids {
		runtimeName, isProxy := dbusproxy.AppOfProxy(container)
		if !isProxy {
			continue
		}
		addr := paths.ParseRuntime(runtimeName, defined)
		rows = append(rows, busRow{
			Address:   addr.String(),
			App:       addr.App,
			Instance:  addr.Instance,
			Container: runtimeName,
			Proxy:     container,
			PID:       pid,
			Socket:    dbusproxy.HostSocketPath(runtimeDir, runtimeName),
		})
	}
	sort.Slice(rows, func(one, two int) bool { return rows[one].Address < rows[two].Address })
	return rows
}

// renderBus is the human/awk form: one tab-separated line per proxy, pid second so the column
// a consumer looks a connection up by is easy to cut out. No header, matching `zcr ps`, since
// a header is one more thing a naive reader has to skip.
func renderBus(rows []busRow) string {
	var out strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&out, "%s\t%d\t%s\t%s\n", row.Address, row.PID, row.Proxy, row.Socket)
	}
	return out.String()
}

// printJSON writes the machine form. Indented because a person runs this by hand too, and jq
// does not care either way; the field names and their nesting are the contract, not the
// whitespace.
func printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

// splitJSONFlag pulls --json out of a command's arguments and returns the rest. Hand-rolled
// rather than a flag.FlagSet because the flag reads better AFTER the app name (`zcr where
// notes@work --json`) and a FlagSet stops parsing at the first positional argument.
func splitJSONFlag(argv []string) (rest []string, asJSON bool, err error) {
	for _, arg := range argv {
		switch {
		case arg == "--json":
			asJSON = true
		case strings.HasPrefix(arg, "-"):
			return nil, false, fmt.Errorf("unknown flag %q", arg)
		default:
			rest = append(rest, arg)
		}
	}
	return rest, asJSON, nil
}

// cmdWhere reports an instance's paths, runtime name and bus, so nothing outside Zinc has to
// hardcode the layout. Not folded into `inspect`, which is a passthrough to podman, and this answer
// holds whether or not the app is running. The app must be defined: the bus answer is config.
func cmdWhere(svc app.Service, opt options.HostOptions, argv []string) error {
	rest, asJSON, err := splitJSONFlag(argv)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, whereUsage)
	}
	if len(rest) != 1 {
		return fmt.Errorf("%s", whereUsage)
	}
	addr, err := paths.ParseAddress(rest[0])
	if err != nil {
		return err
	}
	if strings.TrimSpace(addr.App) == "" {
		return fmt.Errorf("%s", whereUsage)
	}
	if strings.Contains(addr.App, "/") {
		// A path names a file, and the container name and state directory are derived from an
		// app ADDRESS. Answering for a path would report a container named after a filename,
		// which is not what anything runs.
		return fmt.Errorf("%q looks like a path; `where` takes an app address (%s)", rest[0], whereUsage)
	}
	// Through the shared load path, so this refuses a VM app for the same reason every other
	// command does: a guest has no container, no proxy and no socket, and reporting the layout
	// one would have had is an answer that never becomes true.
	cfg, err := loadApp(svc, addr.String())
	if err != nil {
		return err
	}
	report, err := whereOf(addr, !cfg.DBusMeta.IsZero(), opt.RuntimeDir)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(report)
	}
	fmt.Print(renderWhere(report))
	return nil
}

const whereUsage = "usage: zcr where <app[@instance]> [--json]"

// cmdBus prints the attribution table - the direction the desktop needs, since it observes a
// connection and holds only a pid. It stops at the pid on purpose: mapping pid to unique names is
// one call the consumer is already positioned to make, and doing it here would put a D-Bus client
// inside the sandbox runtime.
func cmdBus(svc app.Service, opt options.HostOptions, argv []string) error {
	rest, asJSON, err := splitJSONFlag(argv)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, busUsage)
	}
	if len(rest) != 0 {
		return fmt.Errorf("%s", busUsage)
	}
	pids, err := svc.PIDs()
	if err != nil {
		return err
	}
	rows := busRows(pids, opt.RuntimeDir, svc.Exists)
	if asJSON {
		return printJSON(rows)
	}
	fmt.Print(renderBus(rows))
	return nil
}

const busUsage = "usage: zcr bus [--json]"
