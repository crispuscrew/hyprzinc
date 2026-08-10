package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/crispuscrew/zinc/common/domain/schema"
	"github.com/crispuscrew/zinc/creator/internal/keys"
)

// formResult is what a form key-press tells the parent to do.
type formResult int

const (
	formStay formResult = iota
	formSave
	formCancel
	formEdit    // open the draft in $EDITOR (the "advanced" action)
	formResolve // resolve the image field's tag to a pinned @sha256 digest
)

type fieldKind int

const (
	kindText      fieldKind = iota // free text via a textinput
	kindMultiline                  // multi-line free text via a textarea (ImageMeta.Install)
	kindBool                       // toggle
	kindInfo                       // read-only (shown, not navigable)
	kindAction                     // navigable; enter triggers an action (e.g. edit YAML)
	kindEnum                       // cycles a fixed set of values (app type, VM display mode)
)

// formField is one editable (or informational) row. Closures read/write the form's
// draft config so the rendered value is always the actual value.
type formField struct {
	label  string
	kind   fieldKind
	input  *textinput.Model
	area   *textarea.Model
	get    func() string
	set    func(string)
	bget   func() bool
	bset   func(bool)
	info   func() string
	values []string // kindEnum: the values cycled through, in order
	// rebuild marks a field whose change alters which other fields exist - the app type,
	// since a guest and a container have almost nothing in common below the name.
	rebuild bool
}

// formModel is the create/edit form. draft holds the bool values directly; the text
// fields live in their own inputs and are folded into draft on save. The v2 schema's
// list-valued fields (ImageMeta.Install is line-oriented here; Capabilities,
// NetworkMeta.NetworkLists, Volumes, Configs, Keys) are edited via the advanced $EDITOR
// action, summarized on the "advanced" row.
type formModel struct {
	creating bool
	draft    schema.AppConfig
	scheme   keys.Scheme // nil → default bindings (keys.Scheme handles the fallback)

	name       textinput.Model
	image      textinput.Model
	entrypoint textinput.Model // StartConditions.Entrypoint (space-separated command line)
	install    textarea.Model  // ImageMeta.Install: derived-image RUN steps, or a guest's cloud-init runcmd
	desc       textinput.Model
	icon       textinput.Model
	dbusTalk   textinput.Model // DBusMeta.Talk, comma-separated
	dbusOwn    textinput.Model // DBusMeta.Own, comma-separated

	// VM-only inputs. They exist whatever the type is, so switching back and forth does
	// not lose what was typed; only the field list rebuilds.
	baseDigest textinput.Model
	memory     textinput.Model
	vcpus      textinput.Model
	diskSize   textinput.Model
	ciUser     textinput.Model
	ciKey      textinput.Model

	fields []formField
	idx    int
	err    error
}

func newForm(base schema.AppConfig, creating bool) *formModel {
	frm := &formModel{creating: creating, draft: base}
	if frm.draft.SchemaVersion == 0 {
		frm.draft.SchemaVersion = schema.SchemaVersion
	}
	if frm.draft.Type == "" {
		frm.draft.Type = schema.ZincContainer
	}

	frm.name = newInput(frm.draft.AppNameID, "firefox")
	frm.image = newInput(frm.draft.ImageMeta.Image, "docker.io/...@sha256:... (trusted images may use a tag)")
	frm.entrypoint = newInput(frm.draft.StartConditions.Entrypoint, "entrypoint, e.g. firefox (blank = image default)")
	frm.install = newArea(strings.Join(frm.draft.ImageMeta.Install, "\n"), "build setup, one shell line per row, e.g. apt-get install -y firefox (blank = none)")
	frm.desc = newInput(frm.draft.Description, "")
	frm.icon = newInput(frm.draft.Icon, "freedesktop name (e.g. firefox) or /path/to/icon.png")
	frm.dbusTalk = newInput(strings.Join(frm.draft.DBusMeta.Talk, ", "), "bus names the app may call, e.g. org.freedesktop.portal.Desktop (blank = no session bus)")
	frm.dbusOwn = newInput(strings.Join(frm.draft.DBusMeta.Own, ", "), "bus names the app may claim, e.g. org.mpris.MediaPlayer2.notes (blank = none)")

	virt := frm.draft.VirtualizationMeta
	frm.baseDigest = newInput(virt.BaseDigest, "sha256:... (zvr pin <image> prints it)")
	frm.memory = newInput(numText(virt.MemoryMiB, 4096), "guest RAM in MiB, e.g. 8192")
	frm.vcpus = newInput(numText(int64(virt.VCPUs), 2), "guest CPUs, e.g. 4")
	frm.diskSize = newInput(numText(virt.DiskSizeGiB, 0), "overlay size in GiB (blank = the base image's size)")
	frm.ciUser = newInput(virt.CloudInit.UserName, "account created in the guest (blank = the image default)")
	frm.ciKey = newInput(virt.CloudInit.SSHKeyPath, "/path/to/key.pub - PUBLIC key, the guest can read the seed")

	frm.buildFields()
	frm.idx = -1
	frm.focusNext() // land on the first editable field
	return frm
}

func newInput(value, placeholder string) textinput.Model {
	inp := textinput.New()
	inp.Prompt = ""
	inp.Placeholder = placeholder
	inp.CharLimit = 256
	inp.SetValue(value)
	return inp
}

// newArea builds the multi-line install editor. Each non-empty line becomes one entry
// of ImageMeta.Install (the build's RUN steps), so the user can lay a multi-step setup
// out across lines.
func newArea(value, placeholder string) textarea.Model {
	area := textarea.New()
	area.Prompt = ""
	area.Placeholder = placeholder
	area.ShowLineNumbers = false
	area.CharLimit = 1024
	area.SetWidth(64)
	area.SetHeight(3)
	area.SetValue(value)
	area.Blur()
	return area
}

func (frm *formModel) buildFields() {
	boolean := func(label string, get func() bool, set func(bool)) formField {
		return formField{label: label, kind: kindBool, bget: get, bset: set}
	}

	var fields []formField
	if frm.creating {
		fields = append(fields, formField{label: "name", kind: kindText, input: &frm.name})
	} else {
		fields = append(fields, formField{label: "name", kind: kindInfo,
			info: func() string { return frm.draft.AppNameID + "   (rename with R in the list)" }})
	}

	// The app type comes first and decides the rest of the form: a guest and a container
	// share a name, an image and a description, and almost nothing else. Showing a
	// container's capabilities or a guest's vCPU count on the wrong kind would offer a
	// setting the runtime would refuse to honour.
	fields = append(fields, formField{
		label: "type", kind: kindEnum, rebuild: true,
		values: []string{string(schema.ZincContainer), string(schema.ZincVirtualization)},
		get:    func() string { return string(frm.draft.Type) },
		set:    func(val string) { frm.draft.Type = schema.Type(val) },
	})

	if frm.draft.Type == schema.ZincVirtualization {
		frm.fields = append(fields, frm.vmFields()...)
		return
	}

	fields = append(fields,
		formField{label: "image", kind: kindText, input: &frm.image},
		// Quick-setup fields, grouped with the image they derive from (section 9.1): the
		// entrypoint to run, and build-time install lines that produce a derived image
		// (FROM image + RUN install).
		formField{label: "entrypoint", kind: kindText, input: &frm.entrypoint},
		formField{label: "install", kind: kindMultiline, area: &frm.install},
		formField{label: "description", kind: kindText, input: &frm.desc},
		formField{label: "icon", kind: kindText, input: &frm.icon},
		boolean("terminal",
			func() bool { return frm.draft.StartConditions.Terminal },
			func(val bool) { frm.draft.StartConditions.Terminal = val }),
		boolean("multiterminal",
			func() bool { return frm.draft.StartConditions.Multiterminal },
			func(val bool) { frm.draft.StartConditions.Multiterminal = val }),
		boolean("autorestart",
			func() bool { return frm.draft.StartConditions.Autorestart },
			func(val bool) { frm.draft.StartConditions.Autorestart = val }),
		boolean("keep_alive",
			func() bool { return frm.draft.StopConditions.KeepAlive },
			func(val bool) { frm.draft.StopConditions.KeepAlive = val }),
		boolean("background",
			func() bool { return frm.draft.StopConditions.Background },
			func(val bool) { frm.draft.StopConditions.Background = val }),
		boolean("display.disable_gpu",
			func() bool { return frm.draft.DisplayMeta.DisableGpuAccess },
			func(val bool) { frm.draft.DisplayMeta.DisableGpuAccess = val }),
		boolean("display.disable_security_context",
			func() bool { return frm.draft.DisplayMeta.DisableSecurityContext },
			func(val bool) { frm.draft.DisplayMeta.DisableSecurityContext = val }),
		boolean("audio.playback",
			func() bool { return !frm.draft.AudioMeta.Playback.IsZero() },
			func(val bool) { setAudio(&frm.draft.AudioMeta.Playback, val) }),
		boolean("audio.microphone",
			func() bool { return !frm.draft.AudioMeta.Microphone.IsZero() },
			func(val bool) { setAudio(&frm.draft.AudioMeta.Microphone, val) }),
		boolean("audio.monitor",
			func() bool { return !frm.draft.AudioMeta.Monitor.IsZero() },
			func(val bool) { setAudio(&frm.draft.AudioMeta.Monitor, val) }),
		boolean("host_theme",
			func() bool { return frm.draft.HostTheme },
			func(val bool) { frm.draft.HostTheme = val }),
		// The session bus is last of the grants because it is the widest: blank means the app
		// gets no bus at all, which is the default worth keeping. Filling either row also sets
		// KeepUserID on save, which the summary row below reports.
		formField{label: "dbus.talk", kind: kindText, input: &frm.dbusTalk},
		formField{label: "dbus.own", kind: kindText, input: &frm.dbusOwn},
		formField{label: "advanced", kind: kindAction, info: frm.advancedSummary},
	)
	frm.fields = fields
}

// vmFields is the guest half of the form. The labels name the schema fields so what is
// edited here is recognisable in the YAML, and the ordering runs hardware first (what the
// guest is), then identity (who it lets in), because the first group is required and the
// second is not.
func (frm *formModel) vmFields() []formField {
	return []formField{
		{label: "base image", kind: kindText, input: &frm.image},
		{label: "base digest", kind: kindText, input: &frm.baseDigest},
		{label: "memory (MiB)", kind: kindText, input: &frm.memory},
		{label: "vcpus", kind: kindText, input: &frm.vcpus},
		{label: "disk (GiB)", kind: kindText, input: &frm.diskSize},
		{
			label: "display", kind: kindEnum,
			// Every mode validation accepts. A missing one is not an omission the user can
			// work around: nextValue treats an unrecognised current value as the first in
			// the list, so opening a guest whose mode is missing here and touching this row
			// silently rewrites it to something else.
			values: []string{
				string(schema.VMDisplayAccelerated),
				string(schema.VMDisplayWindow),
				string(schema.VMDisplayCompatible),
				string(schema.VMDisplayNone),
			},
			get: func() string { return string(frm.draft.VirtualizationMeta.Display) },
			set: func(val string) { frm.draft.VirtualizationMeta.Display = schema.VMDisplay(val) },
		},
		// Install steps mean the same thing to both kinds - what to add on top of the
		// pinned base - and become cloud-init runcmd lines for a guest.
		{label: "install", kind: kindMultiline, area: &frm.install},
		{label: "cloud-init user", kind: kindText, input: &frm.ciUser},
		{label: "cloud-init ssh key", kind: kindText, input: &frm.ciKey},
		{label: "description", kind: kindText, input: &frm.desc},
		{label: "icon", kind: kindText, input: &frm.icon},
		{
			label: "audio.playback", kind: kindBool,
			bget: func() bool { return !frm.draft.AudioMeta.Playback.IsZero() },
			bset: func(val bool) { setAudio(&frm.draft.AudioMeta.Playback, val) },
		},
		{
			label: "audio.microphone", kind: kindBool,
			bget: func() bool { return !frm.draft.AudioMeta.Microphone.IsZero() },
			bset: func(val bool) { setAudio(&frm.draft.AudioMeta.Microphone, val) },
		},
		{label: "advanced", kind: kindAction, info: frm.advancedSummary},
	}
}

// numText renders a stored number for its input, showing an empty field rather than a
// zero the author never typed. A fresh VM app starts from the fallback so the common case
// needs no arithmetic.
func numText(value, fallback int64) string {
	if value > 0 {
		return strconv.FormatInt(value, 10)
	}
	if fallback > 0 {
		return strconv.FormatInt(fallback, 10)
	}
	return ""
}

// reload swaps in a config edited out-of-band (the $EDITOR round-trip) and re-seeds the
// text inputs from it, keeping the same form (and its creating flag) so a half-finished
// new app isn't reset.
func (frm *formModel) reload(cfg schema.AppConfig) {
	frm.draft = cfg
	if frm.draft.SchemaVersion == 0 {
		frm.draft.SchemaVersion = schema.SchemaVersion
	}
	if frm.draft.Type == "" {
		frm.draft.Type = schema.ZincContainer
	}
	frm.name.SetValue(cfg.AppNameID)
	frm.image.SetValue(cfg.ImageMeta.Image)
	frm.entrypoint.SetValue(cfg.StartConditions.Entrypoint)
	frm.install.SetValue(strings.Join(cfg.ImageMeta.Install, "\n"))
	frm.desc.SetValue(cfg.Description)
	frm.icon.SetValue(cfg.Icon)
	frm.buildFields()
	frm.err = nil
	if frm.idx >= len(frm.fields) {
		frm.idx = len(frm.fields) - 1
	}
	frm.focus(frm.idx)
}

// advancedSummary counts the list-valued fields the form doesn't edit inline, pointing
// at the $EDITOR escape hatch for them.
func (frm *formModel) advancedSummary() string {
	return fmt.Sprintf("install=%d  caps=%d  networks=%d  volumes=%d configs=%d  keys=%d  depends_on=%d   (enter: edit YAML in $EDITOR)",
		len(frm.draft.ImageMeta.Install), len(frm.draft.Capabilities), len(frm.draft.NetworkMeta.NetworkLists),
		len(frm.draft.Volumes), len(frm.draft.Configs), len(frm.draft.Keys), len(frm.draft.StartConditions.DependsOn))
}

func (frm *formModel) update(msg tea.Msg) (tea.Cmd, formResult) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil, formStay
	}
	keyStr := keyMsg.String()
	scheme := frm.scheme

	// In the multi-line install field the arrow keys move the cursor within the
	// textarea (intrinsic, like the logs viewport's scrolling); tab / shift+tab still
	// move between fields. Without this, up/down would leave the field - they are the
	// NextField/PrevField bindings.
	if frm.fields[frm.idx].kind == kindMultiline && (keyStr == "up" || keyStr == "down") {
		var cmd tea.Cmd
		frm.install, cmd = frm.install.Update(msg)
		return cmd, formStay
	}

	// Field-kind-independent commands first. These are control keys (esc, ctrl+*, tab),
	// so they never collide with typing into a focused text field.
	switch {
	case scheme.Is(keys.CtxForm, keys.Cancel, keyStr):
		return nil, formCancel
	case scheme.Is(keys.CtxForm, keys.Save, keyStr):
		return nil, formSave
	case scheme.Is(keys.CtxForm, keys.NextField, keyStr):
		frm.focusNext()
		return nil, formStay
	case scheme.Is(keys.CtxForm, keys.PrevField, keyStr):
		frm.focusPrev()
		return nil, formStay
	case scheme.Is(keys.CtxForm, keys.ClearField, keyStr):
		switch cur := frm.fields[frm.idx]; {
		case cur.kind == kindText && cur.input != nil:
			cur.input.SetValue("")
		case cur.kind == kindMultiline && cur.area != nil:
			cur.area.SetValue("")
		}
		return nil, formStay
	case scheme.Is(keys.CtxForm, keys.ResolveImage, keyStr):
		if cur := frm.fields[frm.idx]; cur.kind == kindText && cur.label == "image" {
			return nil, formResolve // pin the typed tag to its @sha256 digest
		}
		return nil, formStay
	}

	// Everything else depends on the focused field's kind. The scheme decides which keys
	// fire each gesture; the dispatch by kind stays here.
	fld := frm.fields[frm.idx]
	switch fld.kind {
	case kindText:
		var cmd tea.Cmd
		*fld.input, cmd = fld.input.Update(msg)
		return cmd, formStay
	case kindMultiline:
		// Enter inserts a newline for a multi-step install script; the lines become the
		// ImageMeta.Install entries on save.
		var cmd tea.Cmd
		*fld.area, cmd = fld.area.Update(msg)
		return cmd, formStay
	case kindAction:
		if scheme.Is(keys.CtxForm, keys.Activate, keyStr) {
			return nil, formEdit
		}
	case kindBool:
		if scheme.Is(keys.CtxForm, keys.Toggle, keyStr) {
			fld.bset(!fld.bget())
		}
	case kindEnum:
		if scheme.Is(keys.CtxForm, keys.Toggle, keyStr) {
			fld.set(nextValue(fld.values, fld.get()))
			if fld.rebuild {
				// The app type decides which fields exist, so rebuild and land somewhere
				// valid rather than leaving the cursor pointing past the new list.
				frm.buildFields()
				frm.idx = -1
				frm.focusNext()
			}
		}
	}
	return nil, formStay
}

// nextValue cycles to the value after current, wrapping. An unrecognised current value
// (a hand-edited config) lands on the first, which is the safe default for both enums.
func nextValue(values []string, current string) string {
	for index, value := range values {
		if value == current {
			return values[(index+1)%len(values)]
		}
	}
	return values[0]
}

func (frm *formModel) focus(idx int) {
	frm.idx = idx
	for _, input := range []*textinput.Model{&frm.name, &frm.image, &frm.entrypoint, &frm.desc, &frm.icon} {
		input.Blur()
	}
	frm.install.Blur()
	switch fld := frm.fields[idx]; {
	case fld.kind == kindText && fld.input != nil:
		fld.input.Focus()
	case fld.kind == kindMultiline:
		frm.install.Focus()
	}
}

func (frm *formModel) focusNext() {
	for step := 0; step < len(frm.fields); step++ {
		idx := (frm.idx + 1 + step) % len(frm.fields)
		if frm.fields[idx].kind != kindInfo {
			frm.focus(idx)
			return
		}
	}
}

func (frm *formModel) focusPrev() {
	for step := 0; step < len(frm.fields); step++ {
		idx := ((frm.idx-1-step)%len(frm.fields) + len(frm.fields)) % len(frm.fields)
		if frm.fields[idx].kind != kindInfo {
			frm.focus(idx)
			return
		}
	}
}

// toConfig folds the text inputs into the draft and returns the config to save.
func (frm *formModel) toConfig() schema.AppConfig {
	cfg := frm.draft
	if frm.creating {
		cfg.AppNameID = strings.TrimSpace(frm.name.Value())
	}
	cfg.ImageMeta.Image = strings.TrimSpace(frm.image.Value())
	cfg.ImageMeta.Install = splitLines(frm.install.Value())
	cfg.Description = frm.desc.Value()
	cfg.Icon = strings.TrimSpace(frm.icon.Value())
	cfg.SchemaVersion = schema.SchemaVersion
	if cfg.Type == "" {
		cfg.Type = schema.ZincContainer
	}

	if cfg.Type == schema.ZincVirtualization {
		virt := cfg.VirtualizationMeta
		virt.BaseDigest = strings.TrimSpace(frm.baseDigest.Value())
		virt.MemoryMiB = parseNum(frm.memory.Value())
		virt.VCPUs = int(parseNum(frm.vcpus.Value()))
		virt.DiskSizeGiB = parseNum(frm.diskSize.Value())
		virt.CloudInit.UserName = strings.TrimSpace(frm.ciUser.Value())
		virt.CloudInit.SSHKeyPath = strings.TrimSpace(frm.ciKey.Value())
		cfg.VirtualizationMeta = virt
		// A guest cannot honour the container-only fields, and validation rejects them
		// rather than ignoring them, so switching an existing container app to a VM clears
		// what would otherwise block the save with settings the author never chose here.
		cfg.StartConditions.Entrypoint = ""
		cfg.StartConditions.Terminal = false
		cfg.StartConditions.Multiterminal = false
		cfg.Capabilities = nil
		cfg.NetworkMeta.NetworkLists = nil
		cfg.Volumes, cfg.Configs, cfg.Keys = nil, nil, nil
		cfg.HostTheme = false
		cfg.DBusMeta = schema.DBusMeta{}
		cfg.InternalUserMeta = schema.InternalUserMeta{}
		cfg.ResourcesMeta = schema.ResourcesMeta{}
		return cfg
	}

	cfg.StartConditions.Entrypoint = strings.TrimSpace(frm.entrypoint.Value())
	cfg.DBusMeta = schema.DBusMeta{
		Talk: splitCommas(frm.dbusTalk.Value()),
		Own:  splitCommas(frm.dbusOwn.Value()),
	}
	// A filtered bus is a uid agreement with the proxy, and validation refuses the pair
	// without it, so a form that took the grants and left KeepUserID alone could only fail to
	// save. Setting it here writes it into the file, where it is visible and reviewable.
	if !cfg.DBusMeta.IsZero() {
		cfg.InternalUserMeta.KeepUserID = true
	}
	// The mirror of the above: VM fields on a container app are inert and rejected.
	cfg.VirtualizationMeta = schema.VirtualizationMeta{}
	return cfg
}

// splitCommas reads a comma-separated form row into its entries, dropping empties so a
// trailing comma is not a bus name. What each entry has to be is validation's business.
func splitCommas(text string) []string {
	var entries []string
	for _, entry := range strings.Split(text, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// parseNum reads a whole number from a form field, treating anything unreadable as unset
// so validation reports it by name ("MemoryMiB must be > 0") instead of the form guessing.
func parseNum(text string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// splitLines turns the install textarea's text into one entry per non-blank line,
// returning nil (not an empty slice) when blank so a cleared install marshals away.
func splitLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// setAudio toggles one direction of audio from the form's boolean row.
//
// The form offers the two simple answers, granted and not; naming exact /dev/snd nodes is a
// list-valued edit and lives in the $EDITOR round trip with Volumes and NetworkLists. That
// makes turning a row ON ambiguous for a config that already names devices, and the wrong
// answer is expensive: overwriting the list with `default` would WIDEN the grant from one
// microphone to the session's, silently, because someone pressed a key on a row that was
// already showing true. So an existing device list is left exactly as it is.
//
// Turning a row OFF is unambiguous and clears whichever form was there: the user asked for
// this direction not to be granted, and none is none.
func setAudio(dev *schema.AudioDevice, granted bool) {
	switch {
	case !granted:
		*dev = schema.AudioDevice{}
	case dev.IsZero():
		*dev = schema.AudioDevice{Default: true}
	}
}
