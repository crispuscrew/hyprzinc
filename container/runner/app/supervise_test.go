package app

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/container/runner/domain/options"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// tearingRuntime records the teardown commands it was asked to run, which is how a test sees
// that an app's pod and proxy were actually removed rather than left behind.
type tearingRuntime struct {
	*fakeRuntime
	mu   sync.Mutex
	exec []string
}

func (engine *tearingRuntime) Exec(cmd ports.Command) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.exec = append(engine.exec, cmd.Desc)
	return nil
}

func (engine *tearingRuntime) ran() []string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return append([]string{}, engine.exec...)
}

// tearingNet gives the service a netns to tear down, as a filtered app has.
type tearingNet struct{}

func (tearingNet) RunFlags(schema.AppConfig) []string { return nil }
func (tearingNet) Prepare(schema.AppConfig, options.HostOptions) ([]ports.Command, error) {
	return []ports.Command{{Desc: "create pod"}}, nil
}
func (tearingNet) Teardown(schema.AppConfig) []ports.Command {
	return []ports.Command{{Desc: "remove pod"}, {Desc: "remove bridge"}}
}
func (tearingNet) Counters(schema.AppConfig, options.HostOptions) (ports.Command, bool) {
	return ports.Command{}, false
}

// tearingBroker gives the service something to tear down.
type tearingBroker struct{}

func (tearingBroker) RunFlags(schema.AppConfig) []string { return nil }
func (tearingBroker) Prepare(schema.AppConfig) ([]ports.Command, error) {
	return []ports.Command{{Desc: "start proxy"}}, nil
}
func (tearingBroker) Teardown(schema.AppConfig) []ports.Command {
	return []ports.Command{{Desc: "remove proxy"}, {Desc: "remove socket dir"}}
}

// An app that exits on its own leaves a pod, a proxy and a socket directory behind, and nothing
// removed them: the reaping goroutine that was meant to cannot run, because every front-end
// launches through a zcr that exits moments later. The supervisor is what does it now.
func TestSupervise_TearsDownOnceTheAppIsGone(t *testing.T) {
	engine := &tearingRuntime{fakeRuntime: newFakeRuntime()}
	svc := New(nil, engine, nil, nil, tearingNet{}, tearingBroker{}, nil, nil, nil)
	cfg := schema.AppConfig{AppNameID: "notes", Type: schema.ZincContainer}

	gone := make(chan struct{})
	waited := make(chan string, 1)
	wait := func(name string) error {
		waited <- name
		<-gone // the app is still running until this closes
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- svc.Supervise(cfg, wait) }()

	select {
	case name := <-waited:
		if name != "notes" {
			t.Fatalf("supervised %q, want notes", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the supervisor never waited on the app")
	}

	// Nothing may be torn down while the app is still running.
	if ran := engine.ran(); len(ran) != 0 {
		t.Fatalf("teardown ran while the app was still up: %v", ran)
	}

	close(gone)
	if err := <-done; err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	ran := engine.ran()
	for _, want := range []string{"remove proxy", "remove socket dir", "remove pod", "remove bridge"} {
		if !contains(ran, want) {
			t.Errorf("expected %q to have run after the app exited, got %v", want, ran)
		}
	}
}

// A teardown that fails has to be reported, not swallowed: a leaked pod makes the NEXT launch
// of a filtered app fail, and a supervisor that returned nil would leave nothing to read.
func TestSupervise_ReportsAFailedTeardown(t *testing.T) {
	engine := &failingRuntime{fakeRuntime: newFakeRuntime()}
	svc := New(nil, engine, nil, nil, tearingNet{}, tearingBroker{}, nil, nil, nil)
	cfg := schema.AppConfig{AppNameID: "notes", Type: schema.ZincContainer}

	err := svc.Supervise(cfg, func(string) error { return nil })
	if err == nil {
		t.Fatal("a teardown that failed should be reported")
	}
	if !strings.Contains(err.Error(), "notes") {
		t.Errorf("the error should name the app, got: %v", err)
	}
}

type failingRuntime struct{ *fakeRuntime }

func (failingRuntime) Exec(ports.Command) error { return errors.New("podman said no") }

// The status pipe is what makes `zcr term` able to fail. Until it existed, a refused ruleset or
// a rejected security context was reported as a started app, because the work happens in a
// detached process whose stdio is discarded.
func TestReadTermStatus(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantErr string
	}{
		{"a holder that came up", "ok\n", ""},
		{"a ruleset that would not load", "error start notes (nft): exit 1\n", "nft"},
		{"nothing readable", "surprise\n", "unreadable status"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			go func() {
				write.WriteString(testCase.line)
				write.Close()
			}()
			err = readTermStatus("notes", read)
			switch {
			case testCase.wantErr == "" && err != nil:
				t.Fatalf("want success, got: %v", err)
			case testCase.wantErr != "" && err == nil:
				t.Fatalf("want an error containing %q, got success", testCase.wantErr)
			case testCase.wantErr != "" && !strings.Contains(err.Error(), testCase.wantErr):
				t.Fatalf("want an error containing %q, got: %v", testCase.wantErr, err)
			}
		})
	}
}

// A waiter that dies without answering must not leave the caller believing the app started.
func TestReadTermStatus_SilenceIsAFailure(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	write.Close() // the waiter died with nothing to say

	if err := readTermStatus("notes", read); err == nil {
		t.Fatal("a terminal that reported nothing should not read as a started app")
	}
}

// The lock is per app, so two apps starting together do not queue behind each other - and the
// same app twice does, which is the race it exists to close.
func TestLockLaunch_IsPerApp(t *testing.T) {
	first := lockLaunch("alpha")
	if first == nil {
		t.Skip("no runtime directory available for the lock")
	}
	defer first.close()

	other := make(chan *launchLock, 1)
	go func() { other <- lockLaunch("beta") }()
	select {
	case lock := <-other:
		lock.close()
	case <-time.After(2 * time.Second):
		t.Fatal("a different app had to wait for this one's launch lock")
	}

	same := make(chan *launchLock, 1)
	go func() { same <- lockLaunch("alpha") }()
	select {
	case <-same:
		t.Fatal("the same app's launch was not serialised")
	case <-time.After(300 * time.Millisecond):
	}
	first.close()
	select {
	case lock := <-same:
		lock.close()
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was not released")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// The other direction of the same race: an app can be started again between exiting and the
// supervisor waking up, and the new launch's objects carry the same names. Tearing down then
// would remove what the new launch just built.
func TestSupervise_LeavesARelaunchAlone(t *testing.T) {
	engine := &tearingRuntime{fakeRuntime: newFakeRuntime("notes")} // running again by the time we wake
	svc := New(nil, engine, nil, nil, tearingNet{}, tearingBroker{}, nil, nil, nil)
	cfg := schema.AppConfig{AppNameID: "notes", Type: schema.ZincContainer}

	if err := svc.Supervise(cfg, func(string) error { return nil }); err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	if ran := engine.ran(); len(ran) != 0 {
		t.Fatalf("tore down an app that had been started again: %v", ran)
	}
}
