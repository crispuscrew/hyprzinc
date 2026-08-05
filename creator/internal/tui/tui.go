// Package tui is zc's keyboard-first terminal UI (docs/architecture.md section 9.1, M2).
//
// The Model + Update form the functional core: Update is a pure transition over
// (Model, Msg). All I/O - store reads/writes (authoring) and the zcr shell-outs
// (running apps) - happens in tea.Cmd closures (commands.go) that drive the creator
// backend, so the decision logic is testable without a terminal and the TUI is a thin
// driving adapter.
//
// Scope: create / edit / delete / rename / launch / stop / logs, end-to-end by
// keyboard. The form edits the scalar fields; list-valued fields (Capabilities,
// NetworkLists, Volumes, Configs, Keys) stay YAML-editable via the advanced $EDITOR
// action.
package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/creator/internal/backend"
	"github.com/crispuscrew/zinc/creator/internal/keys"
)

type mode int

const (
	modeList mode = iota
	modeForm
	modeLogs
	modeConfirmDelete
	modeRename // prompt for a new name (delete + recreate)
	modeKeys   // keybind-scheme picker
)

// appRow carries an app twice on purpose. cfg is the RESOLVED config - what the app
// actually is - and drives everything the list shows, so an app that inherits its image or
// its network is not listed as though it had neither. raw is the file as written, and is
// what the edit form opens: a form that loaded the resolved config would write the base's
// values back into the child as if the child had stated them.
//
// name is the STORE KEY (the filename without .yaml), and it is what every ACTION uses. The
// config's own AppNameID is only what the file claims to be:
// LoadResolved refuses a mismatch, but a row that failed to resolve is still listed so it
// can be repaired, and such a row carries the raw config with an unchecked AppNameID. Acting
// on that would mean a dropped "notes.yaml" claiming `AppNameID: firefox` sends delete, run
// and stop at the real firefox instead - deleting a reviewed app while reporting success and
// leaving the hostile file in place.
type appRow struct {
	name    string
	cfg     schema.AppConfig
	raw     schema.AppConfig
	running bool
	loadErr error
}

// Model is the whole TUI state.
type Model struct {
	svc  backend.Service // authors via the store, runs via the zcr shell-outs
	keys keys.Active     // active keybind scheme (mdl.keys.Scheme drives key resolution)

	mode   mode
	apps   []appRow
	cursor int

	form *formModel

	logs      viewport.Model
	logsName  string
	logsReady bool

	confirmName string

	rename     textinput.Model // new-name editor (modeRename)
	renameFrom string          // app being renamed

	keysList   []string // scheme names shown in the picker (modeKeys)
	keysCursor int

	width, height int
	status        string
	err           error
	quitting      bool
}

// New builds the initial model. svc is the creator backend and active is the resolved
// keybind scheme - both supplied by the caller (the composition root) so this package
// stays a thin adapter. A zero keys.Active falls back to the default scheme.
func New(svc backend.Service, active keys.Active) Model {
	return Model{svc: svc, keys: active, mode: modeList, logs: viewport.New(80, 20)}
}

func (mdl Model) Init() tea.Cmd { return loadApps(mdl.svc) }

func (mdl Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		mdl.width, mdl.height = msg.Width, msg.Height
		mdl.logs.Width = msg.Width
		mdl.logs.Height = max(msg.Height-4, 3)
		mdl.logsReady = true
		return mdl, nil

	case appsMsg:
		mdl.apps = msg.rows
		if mdl.cursor >= len(mdl.apps) {
			mdl.cursor = max(len(mdl.apps)-1, 0)
		}
		return mdl, nil

	case statusMsg:
		mdl.status, mdl.err = msg.text, nil
		return mdl, loadApps(mdl.svc) // refresh running indicators

	case errMsg:
		mdl.err = msg.err
		return mdl, nil

	case logsMsg:
		mdl.logsName = msg.name
		mdl.logs.SetContent(msg.body)
		mdl.logs.GotoTop()
		mdl.mode = modeLogs
		return mdl, nil

	case editReadyMsg:
		return mdl, openEditor(mdl.svc, msg.path)

	case editedMsg:
		if mdl.form == nil {
			return mdl, nil
		}
		if msg.err != nil {
			mdl.form.err = msg.err // bad edit: stay in the form, show why
			return mdl, nil
		}
		mdl.form.reload(msg.cfg)
		return mdl, nil

	case resolvedMsg:
		if mdl.form != nil {
			if msg.err != nil {
				mdl.form.err = msg.err
			} else {
				mdl.form.image.SetValue(msg.ref)
				mdl.form.err = nil
			}
		}
		return mdl, nil

	case schemesMsg:
		mdl.keysList = msg.names
		mdl.keysCursor = indexOf(msg.names, mdl.keys.Name) // land on the active scheme
		return mdl, nil

	case schemeSetMsg:
		if msg.err != nil {
			mdl.err = msg.err
			return mdl, nil
		}
		mdl.keys = msg.active
		mdl.mode = modeList
		mdl.status, mdl.err = "keybinds: "+msg.active.Name, nil
		return mdl, nil

	case schemeEditMsg:
		return mdl, openSchemeEditor(msg.path)

	case tea.KeyMsg:
		return mdl.handleKey(msg)
	}

	if mdl.mode == modeLogs {
		var cmd tea.Cmd
		mdl.logs, cmd = mdl.logs.Update(msg)
		return mdl, cmd
	}
	return mdl, nil
}

func (mdl Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		mdl.quitting = true
		return mdl, tea.Quit
	}

	switch mdl.mode {
	case modeForm:
		cmd, res := mdl.form.update(msg)
		switch res {
		case formCancel:
			mdl.mode, mdl.form = modeList, nil
			return mdl, nil
		case formSave:
			cfg := mdl.form.toConfig()
			if err := mdl.svc.Save(cfg); err != nil { // validates first
				mdl.form.err = err
				return mdl, nil
			}
			mdl.mode, mdl.form = modeList, nil
			mdl.status, mdl.err = "saved "+cfg.AppNameID, nil
			return mdl, loadApps(mdl.svc)
		case formEdit:
			return mdl, writeDraft(mdl.svc, mdl.form.toConfig())
		case formResolve:
			mdl.status = "resolving image..."
			return mdl, resolveImage(mdl.svc, mdl.form.image.Value())
		}
		return mdl, cmd

	case modeLogs:
		if act, ok := mdl.keys.Scheme.Resolve(keys.CtxLogs, msg.String()); ok && act == keys.Back {
			mdl.mode = modeList
			return mdl, nil
		}
		var cmd tea.Cmd
		mdl.logs, cmd = mdl.logs.Update(msg)
		return mdl, cmd

	case modeConfirmDelete:
		switch act, _ := mdl.keys.Scheme.Resolve(keys.CtxConfirm, msg.String()); act {
		case keys.Yes:
			name := mdl.confirmName
			mdl.mode = modeList
			return mdl, remove(mdl.svc, name)
		case keys.No:
			mdl.mode = modeList
		}
		return mdl, nil

	case modeRename:
		return mdl.handleRenameKey(msg)

	case modeKeys:
		return mdl.handleKeysKey(msg)

	default:
		return mdl.handleListKey(msg)
	}
}

func (mdl Model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	act, ok := mdl.keys.Scheme.Resolve(keys.CtxList, msg.String())
	if !ok {
		return mdl, nil
	}
	switch act {
	case keys.Quit:
		mdl.quitting = true
		return mdl, tea.Quit
	case keys.Up:
		if mdl.cursor > 0 {
			mdl.cursor--
		}
	case keys.Down:
		if mdl.cursor < len(mdl.apps)-1 {
			mdl.cursor++
		}
	case keys.Refresh:
		return mdl, loadApps(mdl.svc)
	case keys.New:
		mdl.form = newForm(schema.AppConfig{}, true)
		mdl.form.scheme = mdl.keys.Scheme
		mdl.mode, mdl.status = modeForm, ""
	case keys.Edit:
		if row, ok := mdl.selected(); ok && row.loadErr == nil {
			mdl.form = newForm(row.raw, false) // edit what was written, not what it resolves to
			mdl.form.scheme = mdl.keys.Scheme
			mdl.mode, mdl.status = modeForm, ""
		}
	case keys.Run:
		if row, ok := mdl.selected(); ok {
			mdl.status = "launching " + row.name + "..."
			return mdl, launch(mdl.svc, row.name)
		}
	case keys.Shell:
		if row, ok := mdl.selected(); ok {
			if !row.cfg.StartConditions.Multiterminal {
				mdl.status = row.name + ": a shell needs a multiterminal app"
				return mdl, nil
			}
			mdl.status = "opening shell for " + row.name + "..."
			return mdl, openShell(mdl.svc, row.name)
		}
	case keys.Build:
		if row, ok := mdl.selected(); ok {
			if len(row.cfg.ImageMeta.Install) == 0 {
				mdl.status = row.name + ": no install lines - nothing to build"
				return mdl, nil
			}
			mdl.status = "building image for " + row.name + "..."
			return mdl, buildImage(mdl.svc, row.name)
		}
	case keys.Stop:
		if row, ok := mdl.selected(); ok {
			return mdl, stop(mdl.svc, row.name)
		}
	case keys.Logs:
		if row, ok := mdl.selected(); ok {
			return mdl, fetchLogs(mdl.svc, row.name)
		}
	case keys.Rename:
		if row, ok := mdl.selected(); ok && row.loadErr == nil {
			inp := newInput(row.name, "")
			cmd := inp.Focus()
			mdl.rename, mdl.renameFrom = inp, row.name
			mdl.mode, mdl.status = modeRename, ""
			return mdl, cmd
		}
	case keys.Delete:
		if row, ok := mdl.selected(); ok {
			mdl.confirmName = row.name
			mdl.mode = modeConfirmDelete
		}
	case keys.Keys:
		mdl.mode, mdl.keysCursor, mdl.status = modeKeys, 0, ""
		return mdl, loadSchemes()
	}
	return mdl, nil
}

// handleKeysKey drives the scheme picker (modeKeys). Navigation reuses the active
// scheme's list movement (so vim users get j/k here too); enter applies the
// highlighted scheme, e edits/creates a custom copy, esc/q backs out.
func (mdl Model) handleKeysKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	keyStr := msg.String()
	switch keyStr {
	case "esc", "q":
		mdl.mode = modeList
		return mdl, nil
	case "enter":
		if len(mdl.keysList) == 0 {
			return mdl, nil
		}
		name := mdl.keysList[mdl.keysCursor]
		mdl.status = "switching to " + name + "..."
		return mdl, setScheme(name)
	case "e":
		if len(mdl.keysList) == 0 {
			return mdl, nil
		}
		return mdl, editScheme(mdl.keysList[mdl.keysCursor])
	}
	switch act, _ := mdl.keys.Scheme.Resolve(keys.CtxList, keyStr); act {
	case keys.Up:
		if mdl.keysCursor > 0 {
			mdl.keysCursor--
		}
	case keys.Down:
		if mdl.keysCursor < len(mdl.keysList)-1 {
			mdl.keysCursor++
		}
	}
	return mdl, nil
}

// handleRenameKey drives the rename prompt (modeRename): a text input prefilled with
// the current name. Enter commits the rename through the service (which loads the old
// definition, rewrites app.name, saves it, and deletes the old - the "delete +
// recreate"); esc cancels; every other key edits the field. A blank or unchanged name
// is treated as a cancel so Enter is never a destructive no-op.
func (mdl Model) handleRenameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		mdl.mode = modeList
		return mdl, nil
	case "enter":
		from, to := mdl.renameFrom, strings.TrimSpace(mdl.rename.Value())
		mdl.mode = modeList
		if to == "" || to == from {
			return mdl, nil
		}
		mdl.status = "renaming " + from + " → " + to + "..."
		return mdl, renameApp(mdl.svc, from, to)
	}
	var cmd tea.Cmd
	mdl.rename, cmd = mdl.rename.Update(msg)
	return mdl, cmd
}

func (mdl Model) selected() (appRow, bool) {
	if mdl.cursor < 0 || mdl.cursor >= len(mdl.apps) {
		return appRow{}, false
	}
	return mdl.apps[mdl.cursor], true
}

// indexOf returns the position of want in names, or 0 if absent.
func indexOf(names []string, want string) int {
	for idx, name := range names {
		if name == want {
			return idx
		}
	}
	return 0
}
