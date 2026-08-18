package main

import (
	"fmt"
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/creator/internal/backend"
	"github.com/crispuscrew/zinc/creator/internal/runner"
)

// zc authors both app types and runs neither. Which runtime a command goes to is decided by the app's
// Type, not by the command, and hiding that split is zc's job. The two runtimes do not share a
// vocabulary, so the mapping is explicit rather than a verbatim argv forward, and a command with no
// counterpart is refused by name.

// delegate routes one runtime command at an app to the runtime that owns it.
func delegate(svc backend.Service, cmd string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: zc %s <name>", cmd)
	}
	name := firstPositional(argv)
	if name == "" {
		return fmt.Errorf("usage: zc %s <name>", cmd)
	}

	cfg, err := loadForDelegate(svc, name)
	if err != nil {
		// An app zc cannot read is still worth forwarding: it may be a raw container name
		// or a path that the container runtime understands and the store does not.
		return runner.Passthrough(append([]string{cmd}, argv...)...)
	}
	if cfg.Type != schema.ZincVirtualization {
		return runner.Passthrough(append([]string{cmd}, argv...)...)
	}
	return delegateVM(cmd, name, argv)
}

// delegateVM translates a command into zvr's vocabulary.
func delegateVM(cmd, name string, argv []string) error {
	switch cmd {
	case "run":
		// zcr prints the plan unless --exec; zvr does the opposite, running unless
		// --dry-run. Keep zc's own contract - no --exec means show me, do not do it.
		if hasFlag(argv, "--exec") {
			return runner.PassthroughTo(runner.VMBinary, "run", name)
		}
		return runner.PassthroughTo(runner.VMBinary, "run", name, "--dry-run")
	case "stop":
		return runner.PassthroughTo(runner.VMBinary, append([]string{"stop", name}, flagsOnly(argv)...)...)
	case "inspect":
		return runner.PassthroughTo(runner.VMBinary, "status", name)
	case "term":
		return runner.PassthroughTo(runner.VMBinary, "console", name)
	case "build":
		return fmt.Errorf("%q is a VM app: there is no image to build (a guest installs itself on first boot from ImageMeta.Install)", name)
	case "logs":
		return fmt.Errorf("%q is a VM app: it has no container log; attach to its console with `zc term %s`", name, name)
	case "restart":
		return fmt.Errorf("%q is a VM app: restart it with `zc stop %s` then `zc run %s --exec`", name, name, name)
	default:
		return fmt.Errorf("%q is not supported for VM apps", cmd)
	}
}

// loadForDelegate reads an app by store name or path, the same rule the runtimes use - and
// RESOLVED, like them. Which runtime a command goes to is decided by Type, and Type is
// exactly the kind of field a child inherits rather than restates: read raw, a VM app that
// takes its Type from a base looks like a container app and every delegated command goes to
// zcr, which then has to refuse it.
func loadForDelegate(svc backend.Service, name string) (schema.AppConfig, error) {
	if strings.Contains(name, "/") || strings.HasSuffix(name, ".yaml") {
		return svc.LoadFileResolved(name)
	}
	return svc.LoadResolved(name)
}

// firstPositional returns the first non-flag argument.
func firstPositional(argv []string) string {
	for _, arg := range argv {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

// flagsOnly keeps the flags and drops the positional, for rebuilding a command line.
func flagsOnly(argv []string) []string {
	var flags []string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
		}
	}
	return flags
}

func hasFlag(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}
