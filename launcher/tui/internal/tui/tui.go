// Package tui is zlt's keyboard-first picker: a fuzzy-filtered list of the defined apps.
// Type to filter, move with the arrows (or ctrl+n/p), enter launches the selected app
// through zcr and quits (dmenu-style), esc/ctrl+c cancels. All of that is pure Model /
// Update; the only I/O is the zcr shell-out, which happens in tea.Cmd closures
// (commands.go) so Update stays testable.
package tui

import (
	"github.com/crispuscrew/zinc/launcher/common/match"

	tea "github.com/charmbracelet/bubbletea"
)

// App is one launchable entry as the picker shows it.
type App struct {
	Name        string
	Description string
	Running     bool
}

// Model is the picker state.
type Model struct {
	apps      []App          // every defined app, in the caller's order (alphabetical)
	names     []string       // apps' names, parallel to apps, for the matcher
	filtered  []match.Ranked // the current query's matches, best-first (Index into apps)
	query     string         // the filter text
	cursor    int            // selected row, an index into filtered
	runner    Runner         // the zcr delegate
	status    string         // a transient message (a launch error); empty shows key hints
	launching bool           // a launch is in flight, so Enter is ignored until it resolves
	launched  string         // set once an app has launched, so the program can report it
	width     int
	height    int
}

// New builds a picker over apps (which the caller sorts), driving runner for launch and
// running-state.
func New(apps []App, runner Runner) Model {
	names := make([]string, len(apps))
	for index, app := range apps {
		names[index] = app.Name
	}
	mdl := Model{apps: apps, names: names, runner: runner}
	mdl.refilter()
	return mdl
}

// Launched reports the app the picker launched, or "" if it was cancelled. The program
// reads it after Run returns.
func (mdl Model) Launched() string { return mdl.launched }

// Init kicks off the best-effort running-state lookup.
func (mdl Model) Init() tea.Cmd {
	return loadRunning(mdl.runner)
}

func (mdl Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		mdl.width, mdl.height = msg.Width, msg.Height
		return mdl, nil

	case runningMsg:
		if msg.err == nil {
			for name := range msg.running {
				mdl.setRunning(name, true)
			}
		}
		return mdl, nil

	case launchedMsg:
		if msg.err != nil {
			mdl.status = msg.name + ": " + msg.err.Error()
			mdl.launching = false // failed, so let the user try again
			return mdl, nil
		}
		mdl.launched = msg.name
		return mdl, tea.Quit

	case tea.KeyMsg:
		return mdl.handleKey(msg)
	}
	return mdl, nil
}

func (mdl Model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		return mdl, tea.Quit
	case tea.KeyEnter:
		return mdl.launchSelected()
	case tea.KeyUp, tea.KeyCtrlP:
		mdl.moveCursor(-1)
		return mdl, nil
	case tea.KeyDown, tea.KeyCtrlN:
		mdl.moveCursor(1)
		return mdl, nil
	case tea.KeyBackspace:
		if mdl.query != "" {
			runes := []rune(mdl.query)
			mdl.query = string(runes[:len(runes)-1])
			mdl.refilter()
		}
		return mdl, nil
	case tea.KeyCtrlU:
		mdl.query = ""
		mdl.refilter()
		return mdl, nil
	case tea.KeyRunes, tea.KeySpace:
		mdl.query += string(key.Runes)
		mdl.refilter()
		return mdl, nil
	}
	return mdl, nil
}

// launchSelected launches the highlighted app (if any) via zcr. A second Enter while a launch
// is still in flight is ignored: the first only prints a static "launching ..." line while zcr
// takes a second or two to bring the app up, so it is easy to press twice. Two concurrent
// `zcr run` of one app race on the same pod, and the loser's fail-closed teardown tears down
// the pod the winner just started - killing the app the user asked for.
func (mdl Model) launchSelected() (tea.Model, tea.Cmd) {
	if len(mdl.filtered) == 0 || mdl.launching {
		return mdl, nil
	}
	name := mdl.apps[mdl.filtered[mdl.cursor].Index].Name
	mdl.status = "launching " + name + "..."
	mdl.launching = true
	return mdl, launch(mdl.runner, name)
}

// moveCursor shifts the selection by delta, clamped to the filtered range.
func (mdl *Model) moveCursor(delta int) {
	if len(mdl.filtered) == 0 {
		mdl.cursor = 0
		return
	}
	mdl.cursor += delta
	if mdl.cursor < 0 {
		mdl.cursor = 0
	}
	if mdl.cursor > len(mdl.filtered)-1 {
		mdl.cursor = len(mdl.filtered) - 1
	}
}

// refilter recomputes the visible list for the current query and resets the selection to
// the top (the best match), clearing any stale status.
func (mdl *Model) refilter() {
	mdl.filtered = match.Filter(mdl.query, mdl.names)
	mdl.cursor = 0
	mdl.status = ""
}

func (mdl *Model) setRunning(name string, running bool) {
	for index := range mdl.apps {
		if mdl.apps[index].Name == name {
			mdl.apps[index].Running = running
			return
		}
	}
}
