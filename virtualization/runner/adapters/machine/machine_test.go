package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the fake guest and the fake wrapper.
//
// Re-executing this binary rather than copying a shell: /bin/sh is busybox on many images, and
// busybox dispatches on argv[0], so a copy named qemu-system-x86_64 is not an applet it knows and
// exits at once. That passed on a host whose /bin/sh is bash and failed in the pinned container.
func TestMain(m *testing.M) {
	switch os.Getenv("ZINC_TEST_HELPER") {
	case "guest":
		// Nothing to do but stay alive and be identified by its argv.
		time.Sleep(10 * time.Minute)
		return
	case "wrapper":
		guest := exec.Command(os.Getenv("ZINC_TEST_GUEST"), "-name", os.Getenv("ZINC_TEST_APP"))
		guest.Env = append(os.Environ(), "ZINC_TEST_HELPER=guest")
		if err := guest.Start(); err != nil {
			os.Exit(1)
		}
		// Reaped, as pasta reaps the qemu it wraps. Without this a killed guest stays a zombie,
		// and a zombie still answers signal 0 - so it would read as still running wherever the
		// orphan is not reaped for us.
		go func() { _ = guest.Wait() }()
		time.Sleep(10 * time.Minute)
		return
	}
	dir, err := os.MkdirTemp("", "zinc-machine")
	if err != nil {
		panic(err)
	}
	if err := copySelf(filepath.Join(dir, "qemu-system-x86_64")); err != nil {
		panic(err)
	}
	fakeGuestBin = filepath.Join(dir, "qemu-system-x86_64")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeGuestBin is this test binary under a name isGuestProcess will accept as qemu.
var fakeGuestBin string

func copySelf(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, body, 0o700)
}

// fakeGuest starts a process that looks like this app's qemu to isGuestProcess: a binary whose
// name carries qemu-system, invoked with `-name <app>`. A real process rather than a fixture,
// because the thing under test reads /proc.
func fakeGuest(t *testing.T, app string, wrapped bool) (started int, guest int) {
	t.Helper()
	var cmd *exec.Cmd
	if wrapped {
		// A wrapper on the host with the guest as its child, which is the shape pasta makes.
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(self)
		cmd.Env = append(os.Environ(),
			"ZINC_TEST_HELPER=wrapper", "ZINC_TEST_GUEST="+fakeGuestBin, "ZINC_TEST_APP="+app)
	} else {
		cmd = exec.Command(fakeGuestBin, "-name", app)
		cmd.Env = append(os.Environ(), "ZINC_TEST_HELPER=guest")
	}
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
		if !running(started) && !running(guest) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("after terminate: wrapper %s, guest %s", procState(started), procState(guest))
}

// running is "not gone and not a zombie". alive() answers signal 0, which a zombie still accepts,
// so whether a killed process reads as gone would otherwise depend on how quickly whoever inherits
// it reaps it - which differs between this host and the pinned container.
func running(pid int) bool {
	state := procState(pid)
	return state != "gone" && state != "Z"
}

func procState(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "gone"
	}
	// The comm field is parenthesised and may itself contain spaces, so read after the last ')'.
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
	if len(fields) == 0 {
		return "unknown"
	}
	return fields[0]
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
