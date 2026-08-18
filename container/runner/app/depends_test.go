package app

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/adapters/netenforce"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// fakeRuntime records which apps were started, in order, and tracks a running set.
// StartApp marks its app running, so Running() reflects what has come up so far -
// enough to exercise depends_on ordering without a real podman. With detachedStart
// set, StartApp does NOT mark running, modelling production's detached start (the app
// is not yet visible to Running() when a sibling branch is processed) so the
// diamond-dependency dedup can be exercised.
// probeFailures makes the next N readiness probes fail, modelling a dependency whose
// container is up before its service is; probes counts how many were run.
// startOpts records the HostOptions each start was built from, which is how the display
// tests see whether the Wayland socket the broker produced actually reached the argv.
type fakeRuntime struct {
	running       map[string]bool
	started       []string
	startOpts     []options.HostOptions
	detachedStart bool
	probeFailures int
	probes        int
}

func newFakeRuntime(alreadyRunning ...string) *fakeRuntime {
	engine := &fakeRuntime{running: map[string]bool{}}
	for _, name := range alreadyRunning {
		engine.running[name] = true
	}
	return engine
}

func (engine *fakeRuntime) AppRunArgs(cfg schema.AppConfig, opt options.HostOptions, netFlags []string) ([]string, error) {
	return append([]string{"run", "--name", cfg.AppNameID}, netFlags...), nil
}
func (engine *fakeRuntime) Exec(ports.Command) error { return nil }
func (engine *fakeRuntime) Capture(ports.Command) (string, error) {
	return "", nil
}
func (engine *fakeRuntime) StartApp(cfg schema.AppConfig, opt options.HostOptions, runArgs []string, onFail func()) error {
	engine.started = append(engine.started, cfg.AppNameID)
	engine.startOpts = append(engine.startOpts, opt)
	if !engine.detachedStart {
		engine.running[cfg.AppNameID] = true
	}
	return nil
}
func (engine *fakeRuntime) OpenSession(string, []string, options.HostOptions, bool) error { return nil }
func (engine *fakeRuntime) HealthProbe(name string) error {
	engine.probes++
	if engine.probes <= engine.probeFailures {
		return fmt.Errorf("readiness probe for %s: unhealthy", name)
	}
	return nil
}
func (engine *fakeRuntime) Exists(name string) bool           { return engine.running[name] }
func (engine *fakeRuntime) IsRunning(name string) bool        { return engine.running[name] }
func (engine *fakeRuntime) Do([]string) error                 { return nil }
func (engine *fakeRuntime) Running() (map[string]bool, error) { return engine.running, nil }
func (engine *fakeRuntime) Logs(string, int) (string, error)  { return "", nil }

// PIDs numbers the running set so the port is satisfied; the launch path never asks, and
// what a pid MEANS is only decidable against a real runtime, so nothing here pretends
// otherwise.
func (engine *fakeRuntime) PIDs() (map[string]int, error) {
	pids := map[string]int{}
	for name := range engine.running {
		pids[name] = len(pids) + 1
	}
	return pids, nil
}

// fakeStore serves app definitions from an in-memory map.
type fakeStore struct{ apps map[string]schema.AppConfig }

func (store fakeStore) Load(name string) (schema.AppConfig, error) {
	cfg, ok := store.apps[name]
	if !ok {
		return schema.AppConfig{}, fmt.Errorf("app %q not found", name)
	}
	return cfg, nil
}

// The fake serves the same config either way: these tests are about launch ordering, and
// inheritance is resolved by the real store from bytes it does not have.
func (store fakeStore) LoadResolved(name string) (schema.AppConfig, error) { return store.Load(name) }
func (store fakeStore) LoadFileResolved(path string) (schema.AppConfig, error) {
	return store.Load(path)
}
func (store fakeStore) List() ([]string, error)                   { return nil, nil }
func (store fakeStore) Save(schema.AppConfig) error               { return nil }
func (store fakeStore) Delete(string) error                       { return nil }
func (store fakeStore) Exists(name string) bool                   { _, ok := store.apps[name]; return ok }
func (store fakeStore) Path(name string) string                   { return name }
func (store fakeStore) Marshal(schema.AppConfig) ([]byte, error)  { return nil, nil }
func (store fakeStore) LoadFile(string) (schema.AppConfig, error) { return schema.AppConfig{}, nil }

// depApp is a minimal valid (passes validate.Validate), no-network app with the given
// depends_on list. digestPin is defined in plan_test.go (same package).
func depApp(name string, deps ...string) schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion:   schema.SchemaVersion,
		Type:            schema.ZincContainer,
		AppNameID:       name,
		ImageMeta:       schema.ImageMeta{Image: "img" + digestPin},
		StartConditions: schema.StartConditions{DependsOn: deps},
	}
}

// depSvc has no DisplayBroker: the real one re-execs this binary to hold a security context,
// which under `go test` would re-exec the test binary. The display wiring has its own tests
// (display_test.go) with a broker that only records.
func depSvc(store ports.Store, engine ports.Runtime) Service {
	return New(store, engine, nil, nil, netenforce.Enforcer{}, dbusproxy.Broker{}, nil, nil)
}

// web → vpn → base: each dependency (and its own dependencies) must come up before the
// app that needs it, deepest first.
func TestLaunch_StartsDependenciesDepthFirst(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{
		"web":  depApp("web", "vpn"),
		"vpn":  depApp("vpn", "base"),
		"base": depApp("base"),
	}}
	engine := newFakeRuntime()
	if err := depSvc(store, engine).Launch(store.apps["web"], baseOpts()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"base", "vpn", "web"}; !slices.Equal(engine.started, want) {
		t.Fatalf("start order = %v, want %v", engine.started, want)
	}
}

// A dependency that is already running is not started again.
func TestLaunch_SkipsRunningDependency(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{
		"web": depApp("web", "vpn"),
		"vpn": depApp("vpn"),
	}}
	engine := newFakeRuntime("vpn") // vpn already up
	if err := depSvc(store, engine).Launch(store.apps["web"], baseOpts()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(engine.started, []string{"web"}) {
		t.Fatalf("only web should start, got %v", engine.started)
	}
}

// A depends_on name with no definition in the store is a clear error, and nothing launches.
func TestLaunch_MissingDependencyErrors(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{"web": depApp("web", "ghost")}}
	engine := newFakeRuntime()
	err := depSvc(store, engine).Launch(store.apps["web"], baseOpts())
	if err == nil || !strings.Contains(err.Error(), `depends on "ghost"`) {
		t.Fatalf("expected missing-dependency error, got %v", err)
	}
	if len(engine.started) != 0 {
		t.Fatalf("nothing should start, got %v", engine.started)
	}
}

// a → b → a must be reported, not recursed into forever.
func TestLaunch_DependencyCycleRejected(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{
		"a": depApp("a", "b"),
		"b": depApp("b", "a"),
	}}
	engine := newFakeRuntime()
	err := depSvc(store, engine).Launch(store.apps["a"], baseOpts())
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

// Fail-closed: a NetworkList this build can't enforce yet (here host-scoped egress)
// aborts the launch before any dependency or container starts.
func TestLaunch_UnsupportedNetworkFailsClosed(t *testing.T) {
	cfg := depApp("app", "vpn")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{Host: true}}}
	store := fakeStore{apps: map[string]schema.AppConfig{"app": cfg, "vpn": depApp("vpn")}}
	engine := newFakeRuntime()
	err := depSvc(store, engine).Launch(store.apps["app"], baseOpts())
	if err == nil || !strings.Contains(err.Error(), "not supported in this build yet") {
		t.Fatalf("expected fail-closed unsupported-network error, got %v", err)
	}
	if len(engine.started) != 0 {
		t.Fatalf("nothing should start when the network is unsupported, got %v", engine.started)
	}
}

// tier-3 LAN publish (Ingress && Host) is enforceable now, so checkNetwork accepts it.
func TestCheckNetwork_Tier3PublishAllowed(t *testing.T) {
	cfg := depApp("pub")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
		Ingress: true, Host: true, Ports: []int{80},
	}}}
	if err := checkNetwork(cfg); err != nil {
		t.Fatalf("tier-3 publish should be allowed, got: %v", err)
	}
}

// A tier-2 producer (self-scoped ingress) is enforceable now, so checkNetwork accepts it.
func TestCheckNetwork_Tier2ProducerAllowed(t *testing.T) {
	cfg := depApp("db")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
		Ingress: true, Ports: []int{5432},
	}}}
	if err := checkNetwork(cfg); err != nil {
		t.Fatalf("tier-2 producer should be allowed, got: %v", err)
	}
}

// A tier-2 consumer (egress naming a sibling AppName) is enforceable now.
func TestCheckNetwork_Tier2ConsumerAllowed(t *testing.T) {
	cfg := depApp("client")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{AppName: "db"}}}
	if err := checkNetwork(cfg); err != nil {
		t.Fatalf("tier-2 consumer should be allowed, got: %v", err)
	}
}

// A tier-2 app may now also carry other networking. It could not before, because the
// ruleset was one kind or the other and whichever ran ignored the other kind of list
// outright. An app that serves siblings AND reaches the outside is the whole point of a
// gateway, so refusing it refused the feature.
func TestCheckNetwork_Tier2MayAlsoReachOut(t *testing.T) {
	cfg := depApp("db")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{
		{Ingress: true, Ports: []int{5432}},
		{IPv4CIDR: []string{"1.1.1.1/32"}, Ports: []int{443}},
	}}
	if err := checkNetwork(cfg); err != nil {
		t.Fatalf("a link plus egress is enforceable now, got: %v", err)
	}
}

// An ingress list that names an AppName is contradictory and rejected.
func TestCheckNetwork_IngressWithAppNameRejected(t *testing.T) {
	cfg := depApp("db")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
		Ingress: true, AppName: "client", Ports: []int{5432},
	}}}
	err := checkNetwork(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot target an AppName") {
		t.Fatalf("ingress with AppName should be rejected, got: %v", err)
	}
}

// A blacklist on a tier-2 sibling link would open the listed ports instead of gating
// them (the ports are the allowed set), so it is rejected.
func TestCheckNetwork_BlacklistLinkRejected(t *testing.T) {
	cfg := depApp("db")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{
		Ingress: true, Blacklist: true, Ports: []int{5432}, // a producer link, but as a blacklist
	}}}
	err := checkNetwork(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot be a blacklist") {
		t.Fatalf("blacklist on a sibling link should be rejected, got: %v", err)
	}
}

// A dependency shared by two branches (a diamond) is started exactly once, even when a
// detached StartApp has not yet registered it in Running() as the second branch is
// processed. Without the shared started-set the shared filtered dependency's pod would
// be created twice - the second create failing and tearing the first down.
func TestLaunch_DiamondSharedDependencyStartsOnce(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{
		"super": depApp("super", "web", "mail"),
		"web":   depApp("web", "vpn"),
		"mail":  depApp("mail", "vpn"),
		"vpn":   depApp("vpn"),
	}}
	engine := &fakeRuntime{running: map[string]bool{}, detachedStart: true}
	if err := depSvc(store, engine).Launch(store.apps["super"], baseOpts()); err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, name := range engine.started {
		if name == "vpn" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("shared dependency vpn should start exactly once, start sequence = %v", engine.started)
	}
	for _, name := range []string{"vpn", "web", "mail", "super"} {
		if !slices.Contains(engine.started, name) {
			t.Fatalf("%s did not start; sequence = %v", name, engine.started)
		}
	}
}

// readyApp is depApp plus a readiness probe, so dependents wait for it rather than for its
// container merely existing.
func readyApp(name string, deps ...string) schema.AppConfig {
	cfg := depApp(name, deps...)
	cfg.StartConditions.ReadyCheck = []string{"test", "-f", "/run/ready"}
	return cfg
}

// withFastPolling shortens the gap between readiness probes for the duration of a test, so
// a wait that takes several probes does not take several seconds.
func withFastPolling(t *testing.T) {
	t.Helper()
	previous := readyPollInterval
	readyPollInterval = time.Millisecond
	t.Cleanup(func() { readyPollInterval = previous })
}

// A dependency that is running but not yet ready holds its dependent back until it says
// otherwise. This is the whole point of the gate: a client routed through a VPN sibling has
// that sibling as its default route, so starting while the tunnel is still coming up gives
// it no network at all.
func TestLaunch_WaitsForDependencyReadiness(t *testing.T) {
	withFastPolling(t)
	store := fakeStore{apps: map[string]schema.AppConfig{
		"client": depApp("client", "vpn"),
		"vpn":    readyApp("vpn"),
	}}
	engine := newFakeRuntime()
	engine.probeFailures = 3 // up, but not serving yet
	if err := depSvc(store, engine).Launch(store.apps["client"], baseOpts()); err != nil {
		t.Fatal(err)
	}
	if engine.probes != 4 {
		t.Fatalf("expected to keep probing until ready (4 probes), got %d", engine.probes)
	}
	if want := []string{"vpn", "client"}; !slices.Equal(engine.started, want) {
		t.Fatalf("start order = %v, want %v", engine.started, want)
	}
}

// A dependency that never becomes ready fails the launch, naming itself, rather than
// hanging or letting the dependent start with a gateway that cannot forward.
func TestLaunch_UnreadyDependencyFailsLaunch(t *testing.T) {
	withFastPolling(t)
	vpn := readyApp("vpn")
	vpn.StartConditions.ReadyTimeoutSec = 1
	store := fakeStore{apps: map[string]schema.AppConfig{
		"client": depApp("client", "vpn"),
		"vpn":    vpn,
	}}
	engine := newFakeRuntime()
	engine.probeFailures = 1 << 30 // never ready
	err := depSvc(store, engine).Launch(store.apps["client"], baseOpts())
	if err == nil || !strings.Contains(err.Error(), `dependency "vpn" was not ready`) {
		t.Fatalf("expected an unready-dependency error, got %v", err)
	}
	if slices.Contains(engine.started, "client") {
		t.Fatalf("client must not start behind an unready dependency, sequence = %v", engine.started)
	}
}

// An app with no ReadyCheck is ready as soon as it is running - the meaning DependsOn has
// always had - so nothing is probed and no launch pays for the gate.
func TestLaunch_NoReadyCheckIsNotProbed(t *testing.T) {
	store := fakeStore{apps: map[string]schema.AppConfig{
		"web": depApp("web", "db"),
		"db":  depApp("db"),
	}}
	engine := newFakeRuntime()
	if err := depSvc(store, engine).Launch(store.apps["web"], baseOpts()); err != nil {
		t.Fatal(err)
	}
	if engine.probes != 0 {
		t.Fatalf("an app with no ReadyCheck should not be probed, got %d probes", engine.probes)
	}
}

// The multiterminal term path must apply the same fail-closed network gate as launch:
// OpenTerminal rejects a network shape this build cannot enforce (here host-scoped
// egress) rather than proceeding to open a terminal for a mis-enforced app.
func TestOpenTerminal_GatesUnsupportedNetwork(t *testing.T) {
	cfg := depApp("term")
	cfg.StartConditions.Terminal = true
	cfg.StartConditions.Multiterminal = true
	cfg.StartConditions.Entrypoint = "sh"
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{{Host: true}}}
	err := (Service{}).OpenTerminal(cfg, options.HostOptions{}, false)
	if err == nil || !strings.Contains(err.Error(), "not supported in this build yet") {
		t.Fatalf("OpenTerminal must gate an unsupported network shape via checkNetwork, got: %v", err)
	}
}

// Interface scoping rides on pasta, and a linked app is on bridges instead, where podman
// publishes by address rather than by interface name. Accepting the combination published
// the port on EVERY host interface while the config and the authoring warning both named one.
func TestCheckNetwork_InterfaceWithALinkRejected(t *testing.T) {
	cfg := depApp("srv")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{
		{Ingress: true, Ports: []int{5432}},
		{Ingress: true, Host: true, Interface: "eth0", Ports: []int{8080}},
	}}
	err := checkNetwork(cfg)
	if err == nil || !strings.Contains(err.Error(), "every host interface") {
		t.Fatalf("Interface alongside a link must be refused, got: %v", err)
	}
}

// A Via list becomes routes plus a blanket accept on the link; its Ports reach nothing. Left
// accepted, the list would read as "only 443 through the VPN" while tunnelling every port.
func TestCheckNetwork_PortsOnAViaListRejected(t *testing.T) {
	cfg := depApp("client")
	cfg.NetworkMeta = schema.NetworkMeta{NetworkLists: []schema.NetworkList{
		{AppName: "vpn", Via: true, IPv4CIDR: []string{"0.0.0.0/0"}, Ports: []int{443}},
	}}
	err := checkNetwork(cfg)
	if err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("Ports on a routed list must be refused, got: %v", err)
	}
}
