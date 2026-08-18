package keys

// Built-in schemes. There are two, by design:
//
//   - default - exactly zc's historical bindings, so an install with no
//     keys.toml behaves identically to before this feature existed.
//   - vim     - a small variant for muscle-memory: ctrl+n/ctrl+p also move
//     between form fields, and the list "refresh" drops bare "g" (freeing it
//     for vim-flavoured custom bindings). ctrl+n/ctrl+p are used rather than
//     ctrl+j/ctrl+k because Ctrl+J is the newline byte and clashes with Enter.
//
// Honest note: zc's defaults are already largely keyboard/vim-friendly (j/k
// navigate, h/l cycle), so the gap between the two presets is intentionally
// small. The real point of the feature is letting users define their own
// schemes (see store.go); the two built-ins are the starting points to copy.

// Default mirrors the literals previously hardcoded in the TUI.
var Default = Scheme{
	CtxList: {
		Up:      {"up", "k"},
		Down:    {"down", "j"},
		Refresh: {"g", "ctrl+r"},
		New:     {"n"},
		Edit:    {"e", "enter"},
		Run:     {"r"},
		Shell:   {"S"},
		Build:   {"b"},
		Stop:    {"s"},
		Logs:    {"l"},
		Rename:  {"R"},
		Delete:  {"d"},
		Keys:    {"?"},
		Quit:    {"q"},
	},
	CtxForm: {
		NextField:    {"tab", "down"},
		PrevField:    {"shift+tab", "up"},
		EnumPrev:     {"left", "h"},
		EnumNext:     {"right", "l", " "},
		Toggle:       {" ", "enter", "left", "right", "h", "l"},
		Activate:     {"enter"},
		ClearField:   {"ctrl+d"},
		ResolveImage: {"ctrl+r"},
		Save:         {"ctrl+s"},
		Cancel:       {"esc"},
	},
	CtxLogs: {
		Back: {"esc", "q"},
	},
	CtxConfirm: {
		Yes: {"y"},
		No:  {"n", "esc"},
	},
}

// Vim differs from Default only where it earns its keep (see the note above).
var Vim = Scheme{
	CtxList: {
		Up:      {"k", "up"},
		Down:    {"j", "down"},
		Refresh: {"ctrl+r"},
		New:     {"n"},
		Edit:    {"e", "enter"},
		Run:     {"r"},
		Shell:   {"S"},
		Build:   {"b"},
		Stop:    {"s"},
		Logs:    {"l"},
		Rename:  {"R"},
		Delete:  {"d"},
		Keys:    {"?"},
		Quit:    {"q"},
	},
	CtxForm: {
		NextField:    {"tab", "down", "ctrl+n"},
		PrevField:    {"shift+tab", "up", "ctrl+p"},
		EnumPrev:     {"h", "left"},
		EnumNext:     {"l", "right", " "},
		Toggle:       {" ", "enter", "h", "l", "left", "right"},
		Activate:     {"enter"},
		ClearField:   {"ctrl+d"},
		ResolveImage: {"ctrl+r"},
		Save:         {"ctrl+s"},
		Cancel:       {"esc"},
	},
	CtxLogs: {
		Back: {"q", "esc"},
	},
	CtxConfirm: {
		Yes: {"y"},
		No:  {"n", "esc"},
	},
}

var builtins = map[string]Scheme{
	"default": Default,
	"vim":     Vim,
}

// BuiltinNames lists the built-in scheme names in display order.
func BuiltinNames() []string { return []string{"default", "vim"} }

// IsBuiltin reports whether name is a built-in scheme.
func IsBuiltin(name string) bool {
	_, ok := builtins[name]
	return ok
}

// SchemeFor returns a built-in scheme by name (a copy, safe to mutate). The
// bool is false for an unknown name.
func SchemeFor(name string) (Scheme, bool) {
	s, ok := builtins[name]
	if !ok {
		return nil, false
	}
	return s.clone(), true
}
