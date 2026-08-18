// Package keys defines zc's TUI keybindings as selectable schemes
// (docs/architecture.md section 9.1).
//
// These are zc's OWN terminal-UI keys - how you drive the Bubbletea app
// (move the list, save the form, scroll logs). They are deliberately distinct
// from the desktop hotkeys (section 12), which are a separate, host-level concern
// owned by the Nix module (M8/M10).
//
// The package is split functional-core / imperative-shell like the rest of the
// project: this file plus schemes.go and validate.go are pure (no I/O) - a
// Scheme is just data and Resolve is a lookup - while store.go reads and writes
// the on-disk selection under ~/.config/zinc/zc.
package keys

import "strings"

// Context is a TUI screen. The same key means different things on different
// screens (in the list, "l" shows logs; in the form it cycles an enum), so
// bindings are scoped by context rather than living in one flat table.
type Context int

const (
	CtxList Context = iota
	CtxForm
	CtxLogs
	CtxConfirm
)

// Action is a semantic command within a context. The string value is the name
// used in a custom scheme's TOML override table, so it is part of the on-disk
// format - do not rename without a migration.
type Action string

// List-screen actions.
const (
	Up      Action = "up"
	Down    Action = "down"
	Refresh Action = "refresh"
	New     Action = "new"
	Edit    Action = "edit"
	Run     Action = "run"
	Shell   Action = "shell" // multiterminal app: open another terminal as a shell
	Build   Action = "build" // rebuild the selected app's derived image (app.install)
	Stop    Action = "stop"
	Logs    Action = "logs"
	Rename  Action = "rename" // rename the selected app (delete old + recreate under a new name)
	Delete  Action = "delete"
	Keys    Action = "keys" // open the keybind-scheme picker
	Quit    Action = "quit"
)

// Form-screen actions. EnumNext/EnumPrev/Toggle/Activate are gestures whose
// effect depends on the focused field's kind (enum, bool, action); the form
// still dispatches by kind - the scheme only decides which keys trigger them.
const (
	Save         Action = "save"
	Cancel       Action = "cancel"
	NextField    Action = "next_field"
	PrevField    Action = "prev_field"
	ClearField   Action = "clear_field"
	ResolveImage Action = "resolve_image"
	EnumNext     Action = "enum_next"
	EnumPrev     Action = "enum_prev"
	Toggle       Action = "toggle"
	Activate     Action = "activate"
)

// Logs- and confirm-screen actions.
const (
	Back Action = "back"
	Yes  Action = "yes"
	No   Action = "no"
)

// ContextName is the TOML table name for a context ([bindings.<name>]).
var ContextName = map[Context]string{
	CtxList:    "list",
	CtxForm:    "form",
	CtxLogs:    "logs",
	CtxConfirm: "confirm",
}

// ActionsByContext is every valid action per context, in display order. It is
// the source of truth for validation (unknown action names) and for iterating
// bindings in `zc keys show`.
var ActionsByContext = map[Context][]Action{
	CtxList:    {Up, Down, Refresh, New, Edit, Run, Shell, Build, Stop, Logs, Rename, Delete, Keys, Quit},
	CtxForm:    {NextField, PrevField, EnumPrev, EnumNext, Toggle, Activate, ClearField, ResolveImage, Save, Cancel},
	CtxLogs:    {Back},
	CtxConfirm: {Yes, No},
}

// Contexts lists every context in display order.
var Contexts = []Context{CtxList, CtxForm, CtxLogs, CtxConfirm}

func knownAction(ctx Context, act Action) bool {
	for _, a := range ActionsByContext[ctx] {
		if a == act {
			return true
		}
	}
	return false
}

// contextByName is the inverse of ContextName, for decoding custom schemes.
func contextByName(name string) (Context, bool) {
	for ctx, n := range ContextName {
		if n == name {
			return ctx, true
		}
	}
	return 0, false
}

// Scheme maps each context's actions to the keys that trigger them. A key is a
// Bubbletea key string (e.g. "k", "ctrl+s", "shift+tab", "up"). An action may
// have several keys. The zero value (a nil Scheme) behaves as Default, so a
// caller that never loads a scheme gets today's bindings for free.
type Scheme map[Context]map[Action][]string

// or returns the receiver, or Default when it is nil.
func (s Scheme) or() Scheme {
	if s == nil {
		return Default
	}
	return s
}

// Resolve maps a pressed key to the action it triggers in ctx. Within a context
// every key is bound to at most one action (Validate rejects collisions), so the
// first match is unambiguous.
func (s Scheme) Resolve(ctx Context, key string) (Action, bool) {
	for act, ks := range s.or()[ctx] {
		for _, k := range ks {
			if k == key {
				return act, true
			}
		}
	}
	return "", false
}

// Is reports whether key is bound to act in ctx. The form uses this to check a
// specific gesture (e.g. "is this the toggle key?") on the focused field.
func (s Scheme) Is(ctx Context, act Action, key string) bool {
	for _, k := range s.or()[ctx][act] {
		if k == key {
			return true
		}
	}
	return false
}

// KeysFor returns the keys bound to act in ctx.
func (s Scheme) KeysFor(ctx Context, act Action) []string {
	return s.or()[ctx][act]
}

// Hint renders an action's keys for a footer/help line, e.g. "up/k". The space
// key prints as "space" so it isn't an invisible gap. Returns "" when unbound.
func (s Scheme) Hint(ctx Context, act Action) string {
	ks := s.KeysFor(ctx, act)
	out := make([]string, len(ks))
	for i, k := range ks {
		if k == " " {
			out[i] = "space"
		} else {
			out[i] = k
		}
	}
	return strings.Join(out, "/")
}

// HintPrimary renders only an action's first (canonical) key, for compact footers
// where listing every bound key would be noise - the "not a porridge" rule (section 9.1).
// Returns "" when the action is unbound. The space key prints as "space".
func (s Scheme) HintPrimary(ctx Context, act Action) string {
	bound := s.KeysFor(ctx, act)
	if len(bound) == 0 {
		return ""
	}
	if bound[0] == " " {
		return "space"
	}
	return bound[0]
}

// clone deep-copies a scheme so merges never mutate a built-in.
func (s Scheme) clone() Scheme {
	out := make(Scheme, len(s))
	for ctx, m := range s {
		cm := make(map[Action][]string, len(m))
		for a, ks := range m {
			cm[a] = append([]string(nil), ks...)
		}
		out[ctx] = cm
	}
	return out
}
