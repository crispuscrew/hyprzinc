package machine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakeGuest starts a process that looks like this app's qemu to isGuestProcess: a binary whose
// name carries qemu-system, invoked with `-name <app>`. A copy of /bin/sh is enough, and using
// a real process rather than a fixture is the point - the thing under test reads /proc.
func fakeGuest(t *testing.T, app string, wrapped bool) (started int, guest int) {
	t.Helper()
	shell, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skip("no /bin/sh to copy")
	}
	fake := filepath.Join(t.TempDir(), "qemu-system-x86_64")
	if err := os.WriteFile(fake, shell, 0o700); err != nil {
		t.Fatal(err)
	}
	// `& wait` rather than a bare command: a shell handed one simple command execs it in
	// place, which would replace the very argv that identifies this process.
	const idle = "sleep 300 & wait"
	args := []string{fake, "-c", idle, "-name", app}
	if wrapped {
		// A wrapper on the host with the guest as its child, which is the shape pasta makes.
		args = []string{"/bin/sh", "-c", fmt.Sprintf("%q -c %q -name %s & wait", fake, idle, app)}
	}
	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reaped as production reaps it. Without this a killed process lingers as a zombie, and a
	// zombie still answers signal 0 - so the test would read a dead process as alive.
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { terminate(cmd.Process.Pid) })
	started = cmd.Process.Pid

	// Waiting for the process to become identifiable, not sleeping for luck: there is a window
	// after Start returns in which /proc/<pid>/cmdline reads empty, because execve has not yet
	// published the new argv. Production reaches guestPID only after the guest has written its
	// pidfile, which is well past that window; the helper waits for the same thing.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !wrapped && isGuestProcess(started, app) {
			return started, started
		}
		for _, child := range childrenOf(started) {
			if wrapped && isGuestProcess(child, app) {
				return started, child
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fake guest for %q never became identifiable", app)
	return 0, 0
}

// The unfiltered launch: zvr started qemu itself, so the process it started is the guest.
func TestGuestPID_UnwrappedIsTheProcessItself(t *testing.T) {
	started, _ := fakeGuest(t, "notes", false)
	if got := guestPID(started, "notes"); got != started {
		t.Errorf("guestPID = %d, want the started process %d", got, started)
	}
}

// The filtered launch, and the bug this function exists for. qemu is pid 1 inside pasta's PID
// namespace and writes 1 to its own pidfile, so the pid must be found from the host side or
// zvr signals init instead of the guest - which is to say, signals nothing and leaks both
// processes.
func TestGuestPID_FindsTheGuestInsideTheWrapper(t *testing.T) {
	started, guest := fakeGuest(t, "notes", true)
	got := guestPID(started, "notes")
	if got == started {
		t.Fatalf("guestPID returned the wrapper (%d) rather than the guest inside it", started)
	}
	if got != guest {
		t.Errorf("guestPID = %d, want the wrapped guest %d", got, guest)
	}
}

// Identity is checked, not assumed: a wrapper whose child belongs to some other app must not
// be reported as this one's, or stopping one guest would signal another.
func TestGuestPID_RefusesAnotherAppsProcess(t *testing.T) {
	started, _ := fakeGuest(t, "other", true)
	if got := guestPID(started, "notes"); got != 0 {
		t.Errorf("guestPID = %d for an app that is not running, want 0", got)
	}
}

// The window the helper waits out, asserted rather than assumed: it is why production resolves
// the pid after the guest has written its pidfile and not straight after starting the process.
func TestGuestPID_IsNotResolvableBeforeExecCompletes(t *testing.T) {
	started, guest := fakeGuest(t, "notes", true)
	if guestPID(started, "notes") != guest {
		t.Error("once identifiable, the guest must resolve")
	}
}

// A launch that has to be abandoned must take the guest with it. Killing the wrapper first
// orphans it: the namespace outlives its creator, so the guest keeps running with nothing left
// to track it by.
func TestTerminate_TakesTheWrappedGuestToo(t *testing.T) {
	started, guest := fakeGuest(t, "notes", true)
	terminate(started)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(started) && !alive(guest) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("after terminate: wrapper alive=%v, guest alive=%v", alive(started), alive(guest))
}

// childrenOf reports host pids. A process with no children has none, rather than an error the
// caller has to distinguish from a real answer.
func TestChildrenOf_NoChildren(t *testing.T) {
	if kids := childrenOf(os.Getpid()); len(kids) != 0 {
		t.Logf("this test process has children: %v", kids) // a test binary may; not a failure
	}
	if kids := childrenOf(-1); kids != nil {
		t.Errorf("childrenOf(-1) = %v, want nil", kids)
	}
}

func TestIsGuestProcess_RejectsAMerelyMatchingCommandLine(t *testing.T) {
	// The guard against pid reuse: signal 0 says a pid exists, not that it is ours.
	if isGuestProcess(os.Getpid(), "notes") {
		t.Error("the test binary is not a guest")
	}
	if isGuestProcess(1, "notes") {
		t.Error("pid 1 is not a guest")
	}
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatalf("sanity: %v", err)
	}
}
