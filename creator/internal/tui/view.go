package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/creator/internal/keys"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	help       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	dim        = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selected   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	enumSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	runDot     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	offDot     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func (mdl Model) View() string {
	if mdl.quitting {
		return ""
	}
	switch mdl.mode {
	case modeForm:
		return mdl.form.view()
	case modeLogs:
		return mdl.logsView()
	case modeConfirmDelete:
		return mdl.confirmView()
	case modeRename:
		return mdl.renameView()
	case modeKeys:
		return mdl.keysView()
	default:
		return mdl.listView()
	}
}

func (mdl Model) listView() string {
	var bld strings.Builder
	bld.WriteString(titleStyle.Render("Zinc - apps") + "\n\n")

	if len(mdl.apps) == 0 {
		bld.WriteString(dim.Render("no apps yet - press n to create one") + "\n")
	}
	for idx, row := range mdl.apps {
		cursor := "  "
		if idx == mdl.cursor {
			cursor = "▸ "
		}
		dot := offDot.Render("○")
		if row.running {
			dot = runDot.Render("●")
		}
		// The store key, not the config's AppNameID: a row that failed to resolve carries an
		// unchecked name, and this list is what someone reads before running anything.
		name := fmt.Sprintf("%-16s", oneLine(row.name, 16))
		net := fmt.Sprintf("%-10s", netLabel(row.cfg))
		detail := oneLine(row.cfg.ImageMeta.Image, 96)
		if row.loadErr != nil {
			detail = errStyle.Render("(invalid: " + oneLine(row.loadErr.Error(), 96) + ")")
		}
		line := cursor + dot + " " + name + " " + net + " " + detail
		if idx == mdl.cursor {
			line = selected.Render(line)
		}
		bld.WriteString(line + "\n")
	}

	bld.WriteString("\n")
	switch {
	case mdl.err != nil:
		bld.WriteString(errStyle.Render("✗ "+mdl.err.Error()) + "\n")
	case mdl.status != "":
		bld.WriteString(dim.Render(mdl.status) + "\n")
	}
	bld.WriteString(mdl.listFooter())
	return bld.String()
}

// listFooter shows only the actions that apply to the current selection and its state -
// so the help line is never a "porridge" of every key (section 9.1). Global actions
// (new/refresh/keys/quit) always show; edit/delete need a selection; run shows when
// stopped (or for any multiterminal app, where it adds a terminal), stop/logs when
// running, build when install lines are set. Each gesture shows its primary key only.
func (mdl Model) listFooter() string {
	scheme := mdl.keys.Scheme
	var segs []string
	add := func(act keys.Action, label string) {
		if hint := scheme.HintPrimary(keys.CtxList, act); hint != "" {
			segs = append(segs, hint+" "+label)
		}
	}
	add(keys.New, "new")
	if row, ok := mdl.selected(); ok {
		if row.loadErr == nil {
			add(keys.Edit, "edit")
			add(keys.Rename, "rename")
			if !row.running || row.cfg.StartConditions.Multiterminal {
				add(keys.Run, "run")
			}
			if row.cfg.StartConditions.Multiterminal {
				add(keys.Shell, "shell")
			}
			if row.running {
				add(keys.Stop, "stop")
				add(keys.Logs, "logs")
			}
			if len(row.cfg.ImageMeta.Install) > 0 {
				add(keys.Build, "build")
			}
		}
		add(keys.Delete, "delete") // a broken (loadErr) app can still be removed
	}
	add(keys.Refresh, "refresh")
	add(keys.Keys, "keys")
	add(keys.Quit, "quit")
	return help.Render(strings.Join(segs, " · "))
}

func (frm *formModel) view() string {
	var bld strings.Builder
	title := "New app"
	if !frm.creating {
		title = "Edit " + frm.draft.AppNameID
	}
	bld.WriteString(titleStyle.Render(title) + "\n\n")

	for idx, fld := range frm.fields {
		cursor := "  "
		if idx == frm.idx {
			cursor = "▸ "
		}
		label := fmt.Sprintf("%-32s", fld.label)
		if idx == frm.idx {
			label = selected.Render(label)
		}

		// A multi-line field renders its label on the cursor line, then the textarea
		// block beneath it (indented), since its View spans several lines.
		if fld.kind == kindMultiline {
			bld.WriteString(cursor + label + "\n")
			for _, line := range strings.Split(fld.area.View(), "\n") {
				bld.WriteString("    " + line + "\n")
			}
			continue
		}

		var val string
		switch fld.kind {
		case kindText:
			val = fld.input.View()
			if idx != frm.idx && fld.input.Value() == "" {
				val = dim.Render("(empty)")
			}
		case kindBool:
			val = renderBool(fld.bget())
		case kindEnum:
			val = fld.get()
		case kindInfo, kindAction:
			val = dim.Render(fld.info())
		}
		bld.WriteString(cursor + label + val + "\n")
	}

	if frm.err != nil {
		bld.WriteString("\n" + errStyle.Render("✗ "+frm.err.Error()) + "\n")
	}
	bld.WriteString("\n" + frm.footer())
	return bld.String()
}

// footer shows only the gestures for the focused field's kind, plus the always-
// available move/save/cancel - so a bool row doesn't advertise "resolve" and the help
// line stays short (section 9.1). Each gesture shows its primary key only, from the active
// scheme.
func (frm *formModel) footer() string {
	scheme := frm.scheme
	var segs []string
	add := func(hint, label string) {
		if hint != "" {
			segs = append(segs, hint+" "+label)
		}
	}
	add(scheme.HintPrimary(keys.CtxForm, keys.NextField), "move")
	if frm.idx >= 0 && frm.idx < len(frm.fields) {
		switch fld := frm.fields[frm.idx]; fld.kind {
		case kindText:
			add(scheme.HintPrimary(keys.CtxForm, keys.ClearField), "clear")
			if fld.label == "image" {
				add(scheme.HintPrimary(keys.CtxForm, keys.ResolveImage), "resolve")
			}
		case kindMultiline:
			add(scheme.HintPrimary(keys.CtxForm, keys.ClearField), "clear")
			add(scheme.HintPrimary(keys.CtxForm, keys.Activate), "newline")
		case kindBool:
			add(scheme.HintPrimary(keys.CtxForm, keys.Toggle), "toggle")
		case kindEnum:
			add(scheme.HintPrimary(keys.CtxForm, keys.Toggle), "change")
		case kindAction:
			add(scheme.HintPrimary(keys.CtxForm, keys.Activate), "edit")
		}
	}
	add(scheme.HintPrimary(keys.CtxForm, keys.Save), "save")
	add(scheme.HintPrimary(keys.CtxForm, keys.Cancel), "cancel")
	return help.Render(strings.Join(segs, " · "))
}

func (mdl Model) logsView() string {
	header := titleStyle.Render("logs - " + mdl.logsName)
	// Scrolling is the viewport's own built-in (not a scheme action), so it stays
	// literal; only "back" is scheme-driven.
	footer := help.Render(fmt.Sprintf("↑/↓/pgup/pgdn scroll · %s back", mdl.keys.Scheme.HintPrimary(keys.CtxLogs, keys.Back)))
	return header + "\n" + mdl.logs.View() + "\n" + footer
}

func (mdl Model) confirmView() string {
	scheme := mdl.keys.Scheme
	return "\n  " + titleStyle.Render("Delete "+mdl.confirmName+"?") +
		"\n\n  " + help.Render(fmt.Sprintf("%s confirm · %s cancel",
		scheme.HintPrimary(keys.CtxConfirm, keys.Yes), scheme.HintPrimary(keys.CtxConfirm, keys.No))) + "\n"
}

// renameView is the rename prompt (modeRename): a single text input prefilled with the
// current name. enter/esc are intrinsic prompt keys, so they stay literal.
func (mdl Model) renameView() string {
	return "\n  " + titleStyle.Render("Rename "+mdl.renameFrom) +
		"\n\n  " + mdl.rename.View() +
		"\n\n  " + help.Render("enter rename · esc cancel") +
		"\n  " + dim.Render("renames by recreating under the new name; the app must be stopped") + "\n"
}

// keysView is the keybind-scheme picker (modeKeys): every selectable scheme, the active
// one marked, built-in vs custom labelled.
func (mdl Model) keysView() string {
	var bld strings.Builder
	bld.WriteString(titleStyle.Render("Zinc - keybind schemes") + "\n\n")
	if len(mdl.keysList) == 0 {
		bld.WriteString(dim.Render("loading...") + "\n")
	}
	for idx, name := range mdl.keysList {
		cursor := "  "
		if idx == mdl.keysCursor {
			cursor = "▸ "
		}
		kind := "custom"
		if keys.IsBuiltin(name) {
			kind = "built-in"
		}
		mark := " "
		if name == mdl.keys.Name {
			mark = "●"
		}
		label := fmt.Sprintf("%s %-18s", mark, name)
		if idx == mdl.keysCursor {
			label = selected.Render(label)
		}
		bld.WriteString(cursor + label + " " + dim.Render("("+kind+")") + "\n")
	}
	bld.WriteString("\n")
	if mdl.err != nil {
		bld.WriteString(errStyle.Render("✗ "+mdl.err.Error()) + "\n")
	}
	// The picker reuses the list scheme's up/down to move (so vim users get j/k here
	// too); apply/edit/back are intrinsic picker keys, so they stay literal.
	scheme := mdl.keys.Scheme
	bld.WriteString(help.Render(fmt.Sprintf("%s/%s move · enter apply · e edit/new custom · esc back",
		scheme.HintPrimary(keys.CtxList, keys.Up), scheme.HintPrimary(keys.CtxList, keys.Down))))
	return bld.String()
}

func renderBool(val bool) string {
	if val {
		return enumSel.Render("[x] on")
	}
	return dim.Render("[ ] off")
}

// netLabel summarizes an app's network posture for the app list: "isolated" when it has
// no NetworkLists (own localhost only), else the number of lists it carries.
func netLabel(cfg schema.AppConfig) string {
	if n := len(cfg.NetworkMeta.NetworkLists); n > 0 {
		return fmt.Sprintf("net:%d", n)
	}
	return "isolated"
}

// oneLine makes a config-derived string safe to paint into the list.
//
// Everything on this screen comes from a file that may have been shared rather than written
// here, and the renderer splits its output on "\n" - so a newline inside ImageMeta.Image
// adds a whole fabricated row, and a cursor-movement escape such as "\x1b[A\x1b[K" repaints
// the row ABOVE, which is how a genuinely isolated app gets shown next to a hostile one's
// claims. Validation would reject both, but validation runs at save and at launch, never on
// the display path, so a file that is never runnable can still rewrite the list a reviewer
// is reading. Drop anything that can move the cursor, and bound the length.
func oneLine(text string, limit int) string {
	var bld strings.Builder
	kept := 0                   // RUNES, not bytes: fmt pads %-16s by runes, and counting bytes cut a Cyrillic
	for _, char := range text { // or CJK name to roughly a third of the intended width
		if char < 0x20 || char == 0x7f || (char >= 0x80 && char <= 0x9f) {
			continue
		}
		// Bidi overrides and line/paragraph separators reorder or break a row visually without
		// being control characters, which is the same problem this function exists for.
		if char == '\u202e' || char == '\u202d' || char == '\u2028' || char == '\u2029' {
			continue
		}
		if kept >= limit {
			bld.WriteString("...")
			break
		}
		bld.WriteRune(char)
		kept++
	}
	return bld.String()
}
