package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/creator/internal/backend"
	"github.com/crispuscrew/zinc/creator/internal/keys"
)

// Messages produced by the I/O commands below. Update consumes them.
type (
	appsMsg   struct{ rows []appRow }
	statusMsg struct{ text string }
	errMsg    struct{ err error }
	logsMsg   struct {
		name string
		body string
	}
	editReadyMsg struct{ path string } // temp YAML written; ready to open $EDITOR
	editedMsg    struct {              // $EDITOR returned; carries the re-parsed config
		cfg schema.AppConfig
		err error
	}
	resolvedMsg struct { // image field resolved to a pinned @sha256 reference
		ref string
		err error
	}
	schemesMsg   struct{ names []string } // available keybind schemes (picker)
	schemeSetMsg struct {                 // a scheme was activated and reloaded
		active keys.Active
		err    error
	}
	schemeEditMsg struct{ path string } // a scheme file is ready to open in $EDITOR
)

// resolveImage pins the image field's tag to its @sha256 digest (section 5.5) via zcr - which
// pulls the image, so it runs off the UI thread.
func resolveImage(svc backend.Service, ref string) tea.Cmd {
	return func() tea.Msg {
		pinned, err := svc.Resolve(ref)
		return resolvedMsg{ref: pinned, err: err}
	}
}

// writeDraft serializes the form's current config to a temp .yaml so $EDITOR can open
// it; the editor launch is the follow-up (openEditor) once the file exists.
func writeDraft(svc backend.Service, cfg schema.AppConfig) tea.Cmd {
	return func() tea.Msg {
		data, err := svc.Marshal(cfg)
		if err != nil {
			return errMsg{err}
		}
		file, err := os.CreateTemp("", "zc-*.yaml")
		if err != nil {
			return errMsg{err}
		}
		if _, err := file.Write(data); err != nil {
			file.Close()
			os.Remove(file.Name())
			return errMsg{err}
		}
		file.Close()
		return editReadyMsg{path: file.Name()}
	}
}

// openEditor hands the temp file to $EDITOR (default vim), releasing the terminal to it
// and restoring the TUI on exit, then re-parses the (possibly edited) YAML. Parsing here
// keeps Update free of I/O - it just consumes the result.
func openEditor(svc backend.Service, path string) tea.Cmd {
	cmd := exec.Command(editorArgv(path)[0], editorArgv(path)[1:]...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editedMsg{err: fmt.Errorf("editor: %w", err)}
		}
		cfg, lerr := svc.LoadFile(path)
		return editedMsg{cfg: cfg, err: lerr}
	})
}

// editorArgv builds the $EDITOR command (default vim) ending in path.
func editorArgv(path string) []string {
	argv := strings.Fields(os.Getenv("EDITOR"))
	if len(argv) == 0 {
		argv = []string{"vim"}
	}
	return append(argv, path)
}

// loadSchemes lists the selectable keybind schemes (built-ins + custom files).
func loadSchemes() tea.Cmd {
	return func() tea.Msg {
		kst, err := keys.DefaultStore()
		if err != nil {
			return errMsg{err}
		}
		names, err := kst.List()
		if err != nil {
			return errMsg{err}
		}
		return schemesMsg{names}
	}
}

// setScheme persists the chosen scheme as active (validating it first) and returns the
// reloaded effective bindings for the running session.
func setScheme(name string) tea.Cmd {
	return func() tea.Msg {
		kst, err := keys.DefaultStore()
		if err != nil {
			return schemeSetMsg{err: err}
		}
		if err := kst.SetActive(name); err != nil {
			return schemeSetMsg{err: err}
		}
		active, err := kst.Load()
		return schemeSetMsg{active: active, err: err}
	}
}

// editScheme ensures an editable custom scheme file exists (scaffolding a copy when the
// selection is a built-in) and hands its path to the editor step.
func editScheme(name string) tea.Cmd {
	return func() tea.Msg {
		kst, err := keys.DefaultStore()
		if err != nil {
			return errMsg{err}
		}
		_, path, err := kst.EnsureEditable(name)
		if err != nil {
			return errMsg{err}
		}
		return schemeEditMsg{path: path}
	}
}

// openSchemeEditor opens a scheme file in $EDITOR, then refreshes the picker list so a
// newly created custom scheme shows up. Unlike app files the scheme file is kept (it's
// the user's config) and validated only when activated.
func openSchemeEditor(path string) tea.Cmd {
	cmd := exec.Command(editorArgv(path)[0], editorArgv(path)[1:]...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return errMsg{fmt.Errorf("editor: %w", err)}
		}
		kst, derr := keys.DefaultStore()
		if derr != nil {
			return errMsg{derr}
		}
		names, lerr := kst.List()
		if lerr != nil {
			return errMsg{lerr}
		}
		return schemesMsg{names}
	})
}

// loadApps lists the store and tags each app with its running state (one zcr ps query).
// A bad definition is surfaced per-row rather than failing the load; a runtime query
// failure (e.g. zcr not installed) degrades to "nothing running".
func loadApps(svc backend.Service) tea.Cmd {
	return func() tea.Msg {
		names, err := svc.List()
		if err != nil {
			return errMsg{err}
		}
		running, _ := svc.Running() // a query failure degrades to "nothing running"
		rows := make([]appRow, 0, len(names))
		for _, name := range names {
			raw, err := svc.Load(name)
			if err != nil {
				rows = append(rows, appRow{name: name, cfg: schema.AppConfig{AppNameID: name}, loadErr: err})
				continue
			}
			// Resolved for the row, raw for the form. A broken inheritance (a base that was
			// renamed away) still lists, showing the error, so it can be opened and fixed
			// rather than vanishing from the only UI that could repair it.
			cfg, rerr := svc.LoadResolved(name)
			if rerr != nil {
				rows = append(rows, appRow{name: name, cfg: raw, raw: raw, running: running[name], loadErr: rerr})
				continue
			}
			rows = append(rows, appRow{name: name, cfg: cfg, raw: raw, running: running[name]})
		}
		return appsMsg{rows}
	}
}

// launch asks zcr to run the app (validate → build derived image → lock down egress →
// detach). It returns immediately; the TUI carries no launch logic of its own (section 9.1).
func launch(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		if err := svc.Launch(name); err != nil {
			return errMsg{err}
		}
		return statusMsg{"launched " + name}
	}
}

// openShell opens another terminal as a shell for a multiterminal app. Like launch it
// returns immediately - zcr spawns a detached waiter that owns the terminal's lifecycle
// (section 9.1).
func openShell(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		if err := svc.OpenTerminal(name, true); err != nil {
			return errMsg{err}
		}
		return statusMsg{"opened shell for " + name}
	}
}

// buildImage force-rebuilds an app's derived image (FROM ImageMeta.Image + the install
// layer). This is the explicit-rebuild action; a plain run already rebuilds on demand
// when the install lines or base change (section 9.1).
func buildImage(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := svc.Build(name); err != nil {
			return errMsg{err}
		}
		return statusMsg{"built image for " + name}
	}
}

// stop asks zcr to tear a running app down (the pod and its filtered netns, or the
// container).
func stop(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		if err := svc.Stop(name); err != nil {
			return errMsg{err}
		}
		return statusMsg{"stopped " + name}
	}
}

// renameApp renames an app through the store: load the old definition, rewrite
// AppNameID, save it under the new name (validated), and delete the old one. The store
// is the single source of truth, so this is the built-in "delete + recreate" (section 9.1).
func renameApp(svc backend.Service, from, to string) tea.Cmd {
	return func() tea.Msg {
		if err := svc.Rename(from, to); err != nil {
			return errMsg{err}
		}
		return statusMsg{"renamed " + from + " → " + to}
	}
}

func remove(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		if err := svc.Delete(name); err != nil {
			return errMsg{err}
		}
		return statusMsg{"deleted " + name}
	}
}

// fetchLogs grabs a snapshot of the container's logs via zcr. podman may exit nonzero
// (e.g. container never ran) but still print useful output, so we keep both.
func fetchLogs(svc backend.Service, name string) tea.Cmd {
	return func() tea.Msg {
		body, err := svc.Logs(name)
		if err != nil {
			body = strings.TrimRight(body, "\n") + "\n(" + err.Error() + ")"
		}
		if strings.TrimSpace(body) == "" {
			body = "(no output)"
		}
		return logsMsg{name: name, body: body}
	}
}
