// Package picker is the menu's toolkit-neutral view-model: the state of the list (the
// filtered rows, the query, the cursor, the marked set) and the transitions a UI drives
// (type, backspace, clear, move, select). It holds no toolkit types and does no I/O, so the
// filter logic is unit-testable without a display; the Wayland layer (package menu) is a thin
// renderer over it.
//
// Fuzzy filter as you type (best match first), the cursor clamps to the visible range and
// resets to the top on a new query, and a per-row indicator dot comes from a marked set the
// caller supplies.
package picker

import (
	"image"

	"github.com/crispuscrew/zinc/menu/internal/match"
)

// App is one entry as the picker shows it.
type App struct {
	Name        string
	Description string
	Group       string
	Icon        *image.RGBA // decoded, pre-scaled icon, or nil for none
	Preview     string      // path to a preview image, drawn as a thumbnail in grid layout
	Running     bool
}

// Model is the picker state. Construct it with New; the UI reads Query / Visible / Cursor
// to render and calls the transition methods on input.
type Model struct {
	apps     []App          // every defined app, in the caller's order (alphabetical)
	names    []string       // apps' names, parallel to apps, for the matcher
	filtered []match.Ranked // the current query's matches, best-first (Index into apps)
	query    string         // the filter text
	cursor   int            // selected row, an index into filtered
}

// New builds a picker over apps (which the caller sorts). It filters once for the empty
// query, so Visible is the full list until the user types.
func New(apps []App) *Model {
	names := make([]string, len(apps))
	for index, app := range apps {
		names[index] = app.Name
	}
	mdl := &Model{apps: apps, names: names}
	mdl.refilter()
	return mdl
}

func (mdl *Model) Query() string { return mdl.query }

// Type appends text (one or more runes) to the query and refilters.
func (mdl *Model) Type(text string) {
	if text == "" {
		return
	}
	mdl.query += text
	mdl.refilter()
}

// Backspace removes the last rune of the query and refilters. It is a no-op on an empty
// query.
func (mdl *Model) Backspace() {
	if mdl.query == "" {
		return
	}
	runes := []rune(mdl.query)
	mdl.query = string(runes[:len(runes)-1])
	mdl.refilter()
}

// ClearQuery empties the query and refilters (the whole list returns).
func (mdl *Model) ClearQuery() {
	mdl.query = ""
	mdl.refilter()
}

// MoveCursor shifts the selection by delta, clamped to the filtered range (and pinned to 0
// when nothing matches).
func (mdl *Model) MoveCursor(delta int) {
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

// Cursor is the selected row, an index into Visible.
func (mdl *Model) Cursor() int { return mdl.cursor }

// Visible is the filtered apps in ranked (best-first) order - the rows the UI draws.
func (mdl *Model) Visible() []App {
	visible := make([]App, len(mdl.filtered))
	for pos, ranked := range mdl.filtered {
		visible[pos] = mdl.apps[ranked.Index]
	}
	return visible
}

// Selected returns the highlighted app, or ok=false when nothing matches (so the UI knows
// there is nothing to launch).
func (mdl *Model) Selected() (App, bool) {
	if len(mdl.filtered) == 0 {
		return App{}, false
	}
	return mdl.apps[mdl.filtered[mdl.cursor].Index], true
}

// SelectedIndex returns the index (into the original apps slice the caller passed to New) of
// the highlighted row, or ok=false when nothing matches.
func (mdl *Model) SelectedIndex() (int, bool) {
	if len(mdl.filtered) == 0 {
		return 0, false
	}
	return mdl.filtered[mdl.cursor].Index, true
}

// refilter recomputes the visible list for the current query and resets the selection to
// the top (the best match).
func (mdl *Model) refilter() {
	mdl.filtered = match.Filter(mdl.query, mdl.names)
	mdl.cursor = 0
}
