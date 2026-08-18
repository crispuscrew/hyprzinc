// Package runner shells out to `zcr` so the creator can run the apps it authors without importing the
// runner. That is the whole zc/zcr split: they meet only at the on-disk format and this process
// boundary. zcr is expected on $PATH; if it is missing, runtime actions fail with an actionable
// message while authoring keeps working.
package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The runtimes the creator delegates to, resolved from $PATH. zc authors both app types
// and runs neither; which of these a command reaches is decided by the app's Type.
const (
	Binary   = "zcr" // container apps
	VMBinary = "zvr" // VM apps
)

// Result is one image-search hit (name + registry description), mirroring what
// `zcr image search` prints, one tab-separated pair per line.
type Result struct {
	Name        string
	Description string
}

// find locates the zcr binary, returning an actionable error if it is not installed.
func find() (string, error) { return findBinary(Binary) }

// findBinary locates one of the runtimes by name.
func findBinary(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found on $PATH: install the Zinc runtime to run apps (authoring still works without it)", name)
	}
	return path, nil
}

// Passthrough runs `zcr <args...>` wired to the caller's own stdio and returns zcr's
// exit error verbatim. It is the CLI forwarder: `zc run X` becomes `zcr run X`,
// streaming output live (follow logs, inspect JSON, the launch plan) and preserving the
// exit status, so `zc` is a thin front-end over the runtime for those commands.
func Passthrough(args ...string) error { return PassthroughTo(Binary, args...) }

// PassthroughTo is Passthrough against a named runtime, so a VM app's commands reach zvr
// while a container's reach zcr.
func PassthroughTo(binary string, args ...string) error {
	path, err := findBinary(binary)
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// capture runs `zcr <args...>` and returns its stdout. On failure it folds zcr's stderr
// into the error so a caller (the TUI) can surface what went wrong. Used for the
// programmatic actions that need the output as data rather than streamed to a terminal.
func capture(args ...string) (string, error) {
	path, err := find()
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		if msg := strings.TrimSpace(stdout.String()); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("%s %s: %w", Binary, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// safeName screens a name before it becomes an argument to a runner. Two shapes matter: a leading '-'
// lands in the runner's flag slot, and a name ending in ".yaml" is read by zcr as a FILESYSTEM PATH
// resolved against zc's working directory - so the store key "notes.yaml" would run ./notes.yaml, a
// config that never went through the store and that the TUI never displayed.
func safeName(name string) error {
	if name == "" {
		return fmt.Errorf("empty app name")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("app name %q cannot begin with '-': it would reach %s as a flag", name, Binary)
	}
	if !strings.Contains(name, "/") && strings.HasSuffix(name, ".yaml") {
		return fmt.Errorf("app name %q cannot end with '.yaml' (%s would read it as a file path; use ./%s for that)", name, Binary, name)
	}
	return nil
}

// captureApp is capture for the commands whose argument is an app name, so the name is
// screened in one place rather than at each call site.
func captureApp(verb, name string, extra ...string) (string, error) {
	if err := safeName(name); err != nil {
		return "", err
	}
	return capture(append([]string{verb, name}, extra...)...)
}

// Launch starts the app detached: `zcr run <name> --exec` (validate -> build derived
// image -> lock down egress -> detach). zcr returns once the app is spawned.
func Launch(name string) error {
	_, err := captureApp("run", name, "--exec")
	return err
}

// Stop tears the app's pod down: `zcr stop <name>`.
func Stop(name string) error {
	_, err := captureApp("stop", name)
	return err
}

// Plan returns the launch plan without running anything: `zcr run <name>` (no --exec
// prints the exact podman command(s) plus any nft ruleset that would be enforced).
func Plan(name string) (string, error) {
	return captureApp("run", name)
}

// Build (re)builds the app's derived image and returns zcr's build output.
func Build(name string) (string, error) {
	return captureApp("build", name)
}

// OpenTerminal opens one more terminal for a multiterminal app (`zcr term <name>`,
// `--shell` for a shell). zcr spawns a detached waiter and returns.
func OpenTerminal(name string, shell bool) error {
	var extra []string
	if shell {
		extra = append(extra, "--shell")
	}
	_, err := captureApp("term", name, extra...)
	return err
}

// Logs returns a snapshot of the app's logs: `zcr logs <name>` (no follow - it prints
// what podman has and exits).
func Logs(name string) (string, error) {
	return captureApp("logs", name)
}

// Resolve pins an image reference to its digest form: `zcr image resolve <ref>`.
func Resolve(ref string) (string, error) {
	out, err := capture("image", "resolve", ref)
	return strings.TrimSpace(out), err
}

// Search finds images by term: `zcr image search <term>`, parsing the name<TAB>desc
// lines it prints. An empty result is not an error.
func Search(term string) ([]Result, error) {
	out, err := capture("image", "search", term)
	if err != nil {
		return nil, err
	}
	var results []Result
	scan := bufio.NewScanner(strings.NewReader(out))
	for scan.Scan() {
		line := scan.Text()
		if strings.TrimSpace(line) == "" || line == "no images found" {
			continue
		}
		name, desc, _ := strings.Cut(line, "\t")
		results = append(results, Result{Name: name, Description: desc})
	}
	return results, scan.Err()
}

// Running returns the set of apps podman reports as up: `zcr ps`, one name per line.
func Running() (map[string]bool, error) {
	out, err := capture("ps")
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	scan := bufio.NewScanner(strings.NewReader(out))
	for scan.Scan() {
		if name := strings.TrimSpace(scan.Text()); name != "" {
			running[name] = true
		}
	}
	return running, scan.Err()
}
