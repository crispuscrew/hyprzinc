package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/adapters/netenforce"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/domain/paths"
)

// fakeDisplay records the address it was asked about and answers with a fixed socket (or a
// fixed failure). The real broker re-execs the binary to spawn a holder, which under `go
// test` would re-exec the test binary - so the launch wiring is what is checked here, and the
// protocol itself is checked against a real server in adapters/waylandctx.
type fakeDisplay struct {
	socket string
	err    error
	asked  []paths.Address
}

func (fake *fakeDisplay) Establish(addr paths.Address, cfg schema.AppConfig, opt options.HostOptions) (string, error) {
	fake.asked = append(fake.asked, addr)
	return fake.socket, fake.err
}

func displaySvc(display *fakeDisplay, engine *fakeRuntime) Service {
	return New(nil, engine, nil, nil, netenforce.Enforcer{}, dbusproxy.Broker{}, display, nil)
}

func displayApp() schema.AppConfig {
	return schema.AppConfig{
		SchemaVersion: schema.SchemaVersion,
		Type:          schema.ZincContainer,
		AppNameID:     "browser.work",
		ImageMeta:     schema.ImageMeta{Image: "docker.io/library/firefox" + digestPin},
	}
}

// The socket the broker produced has to reach the argv builder, or the container is created
// with a mount of the compositor's own socket while a security context sits unused beside it.
func TestLaunchMountsTheEstablishedSocket(t *testing.T) {
	display := &fakeDisplay{socket: "/run/user/1000/zinc/wayland/browser.work/wayland-1"}
	engine := newFakeRuntime()
	if err := displaySvc(display, engine).Launch(displayApp(), baseOpts()); err != nil {
		t.Fatal(err)
	}
	if len(engine.startOpts) != 1 {
		t.Fatalf("want one start, got %d", len(engine.startOpts))
	}
	if got := engine.startOpts[0].WaylandSocket; got != display.socket {
		t.Fatalf("the app container was built with WaylandSocket %q, want %q", got, display.socket)
	}
}

// The address, not the runtime name: app_id must be stable across instances, so the two
// halves have to arrive at the broker separated. Without a store nothing can split a dotted
// name, which is the documented fallback - the whole name is the app.
func TestLaunchAsksAboutTheAddress(t *testing.T) {
	display := &fakeDisplay{socket: "/run/user/1000/zinc/wayland/browser.work/wayland-1"}
	if err := displaySvc(display, newFakeRuntime()).Launch(displayApp(), baseOpts()); err != nil {
		t.Fatal(err)
	}
	if len(display.asked) != 1 {
		t.Fatalf("want one Establish, got %d", len(display.asked))
	}
	if got := display.asked[0].Runtime(); got != "browser.work" {
		t.Fatalf("the broker was asked about %q, which is not the container's name", got)
	}
}

// An empty path is the fallback, not a failure: the app launches on the compositor's own
// socket. The argv builder is what turns that into a passthrough mount and an honest label.
func TestLaunchProceedsWithoutASecurityContext(t *testing.T) {
	engine := newFakeRuntime()
	if err := displaySvc(&fakeDisplay{}, engine).Launch(displayApp(), baseOpts()); err != nil {
		t.Fatal(err)
	}
	if len(engine.started) != 1 {
		t.Fatalf("a compositor without the protocol must not stop the launch, started %v", engine.started)
	}
	if got := engine.startOpts[0].WaylandSocket; got != "" {
		t.Fatalf("want no derived socket, got %q", got)
	}
}

// A broker that failed for any other reason fails the launch. Starting the app anyway would
// mean the security context is best-effort with no way to tell whether it happened - and the
// app would be running unconfined while the label claimed otherwise.
func TestLaunchFailsWhenTheContextCannotBeCreated(t *testing.T) {
	display := &fakeDisplay{err: errors.New("listen on /run/user/1000/...: permission denied")}
	engine := newFakeRuntime()
	err := displaySvc(display, engine).Launch(displayApp(), baseOpts())
	if err == nil {
		t.Fatal("want the launch to fail")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the reason was lost: %v", err)
	}
	if len(engine.started) != 0 {
		t.Fatalf("nothing may be started after the display broker failed, got %v", engine.started)
	}
}
