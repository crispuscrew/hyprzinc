package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
)

// startDependencies brings up everything cfg needs first (section 6.6), depth-first, waiting on any
// ReadyCheck. An already-running dependency is untouched; a cycle is an error. chain is the stack
// of apps mid-launch, so a name reappearing in it is that cycle.
func (svc Service) startDependencies(cfg schema.AppConfig, opt options.HostOptions, chain []string, started map[string]bool) error {
	if len(cfg.StartConditions.DependsOn) == 0 {
		return nil
	}
	// Three-index slice caps chain so append allocates a fresh backing array rather
	// than aliasing a sibling recursion's storage.
	chain = append(chain[:len(chain):len(chain)], cfg.AppNameID)
	running, err := svc.runtime.Running()
	if err != nil {
		return fmt.Errorf("%s: checking running containers before starting dependencies: %w", cfg.AppNameID, err)
	}
	if running == nil {
		running = map[string]bool{}
	}
	for _, dep := range cfg.StartConditions.DependsOn {
		if running[dep] {
			continue // already up - leave it as-is
		}
		if idx := slices.Index(chain, dep); idx >= 0 {
			return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(chain[idx:], " -> "), dep)
		}
		depCfg, err := svc.store.LoadResolved(dep)
		if err != nil {
			return fmt.Errorf("%s depends on %q: %w", cfg.AppNameID, dep, err)
		}
		if err := svc.launch(depCfg, opt, chain, started); err != nil {
			return fmt.Errorf("starting dependency %q of %s: %w", dep, cfg.AppNameID, err)
		}
		running[dep] = true // so a name listed twice is not started twice
		if err := svc.waitReady(depCfg, cfg.AppNameID); err != nil {
			return err
		}
	}
	return nil
}

// readyPollInterval is the gap between readiness probes. Each probe execs into the
// dependency's container, so this trades a little startup latency against not hammering a
// container that is busy doing the very thing being waited for.
var readyPollInterval = 500 * time.Millisecond

// defaultReadyTimeout bounds a wait whose app did not set StartConditions.ReadyTimeoutSec.
// Long enough for a VPN handshake over a slow link, short enough that a dependency which is
// never going to be ready fails the launch with a message instead of hanging.
const defaultReadyTimeout = 60 * time.Second

// waitReady holds dependent until depCfg is ready, and fails the launch if it never is. Fatal on
// purpose: a client routed through a gateway has it as default route and DNS, so starting early
// means no working network at all.
//
// Only a dependency this launch started is waited on, or every launch behind a momentarily
// unhealthy one would fail. One that never came ready is left running: its logs are the evidence.
func (svc Service) waitReady(depCfg schema.AppConfig, dependent string) error {
	if len(depCfg.StartConditions.ReadyCheck) == 0 {
		return nil
	}
	timeout := defaultReadyTimeout
	if seconds := depCfg.StartConditions.ReadyTimeoutSec; seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err := svc.runtime.HealthProbe(depCfg.AppNameID)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: dependency %q was not ready within %s: %w",
				dependent, depCfg.AppNameID, timeout, err)
		}
		time.Sleep(readyPollInterval)
	}
}

// checkNetwork fails closed on NetworkLists this build cannot enforce. Supported: self-scoped
// egress (section 5.3), tier-3 LAN publishing, tier-2 sibling links. Rejected: routing gateways
// (multi-homing), ingress targeting an AppName, host-scoped egress. Links may coexist with other
// networking, since the renderer now gates by interface and by address at once.
func checkNetwork(cfg schema.AppConfig) error {
	linked := false
	for _, netList := range cfg.NetworkMeta.NetworkLists {
		if isLinkList(netList) {
			linked = true
		}
	}
	for index, netList := range cfg.NetworkMeta.NetworkLists {
		appName := strings.TrimSpace(netList.AppName)
		switch {
		case linked && netList.Ingress && netList.Host && strings.TrimSpace(netList.Interface) != "":
			// Interface scoping rides on pasta (`--network pasta:--interface,<iface>`), and an
			// app with a link is on bridges instead, where podman publishes by address rather
			// than by interface name. Accepting it would publish the port on EVERY host
			// interface while the config, and the authoring warning, both say one.
			return fmt.Errorf("%s: NetworkLists[%d]: Interface %q cannot be honoured on an app that also has a sibling link - the port would be published on every host interface instead of that one; drop Interface, or drop the link",
				cfg.AppNameID, index, netList.Interface)
		case netList.Via && len(netList.Ports) > 0:
			// A Via list becomes routes plus a blanket `oifname <link> accept`; its ports are
			// used nowhere. Left accepted it would read as "only 443 through the VPN" while
			// tunnelling every port.
			return fmt.Errorf("%s: NetworkLists[%d]: Ports cannot be enforced on a routed (Via) list - the sibling link is accepted as a whole, so listing ports would read as a restriction that is not applied; drop Ports, or drop Via",
				cfg.AppNameID, index)
		case netList.GatewayV4 != "" || netList.GatewayV6 != "":
			return fmt.Errorf("%s: NetworkLists[%d]: routing through a gateway (multi-homing) is not supported in this build yet", cfg.AppNameID, index)
		case netList.Ingress && appName != "":
			return fmt.Errorf("%s: NetworkLists[%d]: an ingress list cannot target an AppName - a producer publishes to any sibling that joins its link, and the consumer names the producer", cfg.AppNameID, index)
		case netList.Host && !netList.Ingress:
			return fmt.Errorf("%s: NetworkLists[%d]: host-scoped egress is not supported in this build yet", cfg.AppNameID, index)
		case isLinkList(netList) && netList.Blacklist:
			return fmt.Errorf("%s: NetworkLists[%d]: a sibling link list cannot be a blacklist - its Ports are the allowed set and a blacklist would open them instead of gating them", cfg.AppNameID, index)
		}
	}
	return nil
}

// isLinkList reports whether a NetworkList is a tier-2 sibling link: a producer's
// self-scoped ingress (Ingress, no Host, no AppName) or a consumer's sibling egress
// (egress, no Host, an AppName).
func isLinkList(netList schema.NetworkList) bool {
	appName := strings.TrimSpace(netList.AppName)
	producer := netList.Ingress && !netList.Host && appName == ""
	consumer := !netList.Ingress && !netList.Host && appName != ""
	return producer || consumer
}
