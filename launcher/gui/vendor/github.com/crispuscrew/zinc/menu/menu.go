// Package menu is a reusable, floating, filterable overlay menu for Wayland: a centered
// layer-shell surface, a software-rendered fuzzy-filtered list, keyboard input fed into a picker
// model. It speaks the Wayland wire protocol directly through go-wayland - no cgo - and depends
// on no sibling modules, so any program can import it.
//
// Run is the whole API: items and an ActivateFunc in, chosen index out.
package menu

import (
	"fmt"
	"image"
	"os"
	"strings"
	"syscall"

	"github.com/crispuscrew/zinc/menu/internal/icons"
	"github.com/crispuscrew/zinc/menu/internal/keymap"
	"github.com/crispuscrew/zinc/menu/internal/picker"
	"github.com/crispuscrew/zinc/menu/internal/render"
	"github.com/crispuscrew/zinc/menu/internal/theme"
	"github.com/crispuscrew/zinc/menu/internal/thumbs"

	"github.com/rajveermalviya/go-wayland/wayland/client"
)

// Item is one row in the menu.
type Item struct {
	Label       string // the primary text, and what the fuzzy filter matches against
	Description string // secondary text, shown dimmed after the label
	Group       string // optional section header; keep items of one group adjacent (shown only when idle)
	Icon        string // optional icon: a freedesktop icon name or an absolute image path
	Preview     string // optional image path, drawn as a thumbnail tile in grid layout (Options.Grid)
	Marked      bool   // draws an indicator dot; the caller decides what it means (e.g. running)
}

// ActivateFunc is called when the user picks an item. Returning an error keeps the menu open and
// banners it; nil closes the menu. May be nil, in which case Enter just closes.
//
// It runs on its own goroutine, so a slow activation leaves the overlay responsive rather than
// frozen while it holds an exclusive keyboard grab. Only one call is ever in flight. Esc takes the
// overlay down immediately and Run returns once the call finishes, so none outlives Run.
type ActivateFunc func(item Item) error

// Options tunes one Run. The zero value is usable: a default-size, opaque, animated overlay
// with a "> " prompt.
type Options struct {
	Prompt   string  // drawn before the query (default "> ")
	Footer   string  // hint line at the bottom (default "up/down move   enter select   esc quit")
	BusyVerb string  // verb in the banner shown while ActivateFunc runs, before the item label (default "running")
	AppID    string  // layer-surface namespace / app-id for compositor window rules (default "menu")
	FontPath string  // .ttf/.otf to render with; empty keeps the process default (a system Nerd Font found at startup, else Go Mono)
	Width    int     // overlay width in px (default 720)
	Height   int     // overlay height in px (default 440)
	Opacity  float64 // background opacity 0..1; <= 0 means opaque
	NoAnim   bool    // disable the entrance fade-in
	Debug    bool    // trace the Wayland handshake to stderr

	// Grid lays items out as a thumbnail grid (each Item.Preview drawn as a tile) instead of
	// the default list, for pickers that are visual rather than textual - a wallpaper chooser,
	// an icon picker. Arrow keys navigate in two dimensions; typing still fuzzy-filters.
	Grid       bool
	CellWidth  int // grid cell width in px (default 180); ignored unless Grid
	CellHeight int // grid cell height in px (default 140); ignored unless Grid
}

const (
	defaultCellWidth  = 180
	defaultCellHeight = 140
)

const (
	// The overlay is a fixed-size, centered floating panel (like fuzzel/wofi), not a tiled
	// window, so it keeps this size on any compositor.
	defaultWidth   = 720
	defaultHeight  = 440
	pageStep       = 10
	edgeStep       = 1 << 20 // Home/End: jump far enough to clamp to the first/last row
	fadeDurationMs = 160     // how long the entrance fade-in takes
	busyDotMs      = 400     // how long each step of the busy banner's cycling ellipsis lasts
	busyDotMax     = 3       // how many dots the ellipsis cycles through
)

// debugOn gates Wayland-handshake tracing; Run sets it from Options.Debug.
var debugOn bool

func trace(format string, args ...any) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "menu: "+format+"\n", args...)
	}
}

// Run opens the overlay over items and calls activate on the chosen one. It returns the index
// of the activated item (into items), or -1 if the user cancelled (Esc or the compositor
// closed it).
func Run(items []Item, activate ActivateFunc, opts Options) (int, error) {
	debugOn = opts.Debug
	if opts.FontPath != "" {
		render.UseFont(opts.FontPath)
	}
	trace("font: %s", render.LoadedFont)
	application := &app{
		model:    picker.New(toApps(items)),
		items:    items,
		activate: activate,
		palette:  theme.Detect(),
		prompt:   orString(opts.Prompt, "> "),
		footer:   opts.Footer, // empty falls back to a neutral default in the renderer
		busyVerb: orString(opts.BusyVerb, "running"),
		appID:    orString(opts.AppID, "menu"),
		width:    orInt(opts.Width, defaultWidth),
		height:   orInt(opts.Height, defaultHeight),
		opacity:  1,
		fade:     1,
		selected: -1,
	}
	if opts.Opacity > 0 && opts.Opacity < 1 {
		application.opacity = opts.Opacity
	}
	if opts.Grid {
		application.grid = true
		application.cellW = orInt(opts.CellWidth, defaultCellWidth)
		application.cellH = orInt(opts.CellHeight, defaultCellHeight)
		// Decode thumbnails at exactly the box the renderer draws them into, off the render path.
		boxW, boxH := render.ThumbBox(application.cellW, application.cellH)
		application.thumbs = thumbs.New(boxW, boxH)
	}
	if !opts.NoAnim {
		application.fade = 0
		application.animating = true
	}
	// Deferred before connect, not after: connect fails late on a compositor that lacks
	// zwlr_layer_shell_v1, and returning straight out would leak the open Wayland socket (and
	// any shm mapping already made) once per call - fatal for a long-lived program that pops
	// the menu on a hotkey. cleanup is nil-safe, so a partial connect tears down correctly.
	defer application.cleanup()
	if err := application.connect(); err != nil {
		return -1, err
	}
	loopErr := application.loop()
	// Tear the overlay down before waiting on the activation. The user may have pressed Esc
	// while a slow one was still running, and the window must not sit on screen holding an
	// exclusive keyboard grab until it finishes - that is the whole point of dismissing it.
	// cleanup is idempotent, so the deferred call above is harmless.
	application.cleanup()
	activateErr := application.awaitActivate()
	switch {
	case loopErr != nil:
		return -1, loopErr
	case activateErr != nil:
		return -1, activateErr
	}
	return application.selected, nil
}

// toApps maps the public items to the internal picker rows, resolving each icon spec to a
// decoded raster once (a missing or SVG-only icon resolves to nil and simply draws nothing).
func toApps(items []Item) []picker.App {
	apps := make([]picker.App, len(items))
	for index, item := range items {
		apps[index] = picker.App{
			Name:        item.Label,
			Description: item.Description,
			Group:       item.Group,
			Icon:        icons.Resolve(item.Icon, render.IconSize),
			Preview:     item.Preview,
			Running:     item.Marked,
		}
	}
	return apps
}

func orString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func orInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// app holds the live Wayland objects and the picker state for one run.
type app struct {
	model    *picker.Model
	items    []Item
	activate ActivateFunc
	prompt   string
	footer   string
	busyVerb string
	appID    string

	grid   bool
	cellW  int
	cellH  int
	thumbs *thumbs.Store // async thumbnail decode+cache for the grid layout; nil unless grid

	palette theme.Palette

	display      *client.Display
	ctx          *client.Context
	registry     *client.Registry
	compositor   *client.Compositor
	shm          *client.Shm
	seat         *client.Seat
	keyboard     *client.Keyboard
	layerShell   *layerShell
	surface      *client.Surface
	layerSurface *layerSurface

	current *shmBuffer   // the buffer currently attached to the surface
	retired []*shmBuffer // resized-away buffers, kept alive until the compositor releases them

	width, height int

	opacity      float64 // steady-state background opacity (Options.Opacity), 1 = opaque
	fade         float64 // entrance-animation progress, 0..1
	animating    bool
	animStarted  bool
	animStartMs  uint32
	framePending bool

	shift, ctrl bool

	selected  int    // index into items of the activated row, or -1 if cancelled
	launchErr string // an activate failure shown in the window, dismissed on the next keypress
	runErr    error
	closed    bool

	// One activation at a time, running off the event loop. activateDone is buffered so the
	// goroutine can always deliver its result and exit, even after the user dismissed the
	// overlay and nothing is watching it any more.
	activating   bool
	activateItem string     // the label being activated, for the busy banner
	activateIdx  int        // its index into items, reported as selected once it succeeds
	activateDone chan error // the in-flight result; nil when nothing is activating
	frameMs      uint32     // the last frame callback's timestamp, which paces the busy ellipsis
}

// connect opens the display, binds the globals, and creates the window.
func (application *app) connect() error {
	display, err := client.Connect("")
	if err != nil {
		return fmt.Errorf("connect to the Wayland compositor (is a Wayland session running?): %w", err)
	}
	application.display = display
	application.ctx = display.Context()
	// Surface a compositor-reported protocol error with its message, rather than only the
	// follow-on socket EOF.
	display.SetErrorHandler(func(event client.DisplayErrorEvent) {
		application.runErr = fmt.Errorf("wayland protocol error (code %d): %s", event.Code, event.Message)
		application.closed = true
	})

	registry, err := display.GetRegistry()
	if err != nil {
		return err
	}
	application.registry = registry
	registry.SetGlobalHandler(application.handleGlobal)
	trace("connected; getting globals")

	// First roundtrip binds the advertised globals; the second lets the seat report its
	// capabilities (so the keyboard is set up before the window appears).
	if err := application.roundtrip(); err != nil {
		return application.fail(err)
	}
	if application.compositor == nil || application.shm == nil || application.layerShell == nil {
		return fmt.Errorf("the compositor is missing a required Wayland global "+
			"(have wl_compositor=%t wl_shm=%t zwlr_layer_shell_v1=%t); this menu needs wlr-layer-shell "+
			"(niri, hyprland, sway all have it). Set Options.Debug to list what it advertised",
			application.compositor != nil, application.shm != nil, application.layerShell != nil)
	}
	if err := application.roundtrip(); err != nil {
		return application.fail(err)
	}
	trace("globals bound; creating the overlay")

	surface, err := application.compositor.CreateSurface()
	if err != nil {
		return application.fail(err)
	}
	application.surface = surface

	// A layer surface on the overlay layer, with no anchors (so the compositor centers it)
	// and a fixed size, floats above tiled windows instead of being tiled. Exclusive keyboard
	// interactivity routes typing to the picker.
	layer := application.layerShell.getLayerSurface(surface, nil, layerOverlay, application.appID)
	application.layerSurface = layer
	layer.configureHandler = application.handleLayerConfigure
	layer.closedHandler = func() { application.closed = true }
	if err := layer.setSize(uint32(application.width), uint32(application.height)); err != nil {
		return application.fail(err)
	}
	if err := layer.setAnchor(0); err != nil {
		return application.fail(err)
	}
	if err := layer.setKeyboardInteractivity(keyboardInteractivityExclusive); err != nil {
		return application.fail(err)
	}

	// The first surface commit (with no buffer) asks the compositor for the initial
	// configure; the buffer is attached in the configure handler.
	if err := surface.Commit(); err != nil {
		return application.fail(err)
	}
	trace("overlay committed; draining the first configure")
	// Read one round of events, so a protocol error in the setup above surfaces with its
	// message (via the error handler) rather than as a later broken pipe on the next write.
	if err := application.roundtrip(); err != nil {
		return application.fail(err)
	}
	if application.runErr != nil {
		return application.runErr
	}
	return nil
}

// fail surfaces a compositor protocol error if one is pending. A broken pipe on a write
// usually means the compositor already sent wl_display.error and closed, so read one more
// message to capture that message and prefer it over the raw socket error.
func (application *app) fail(cause error) error {
	if application.runErr == nil {
		_ = application.ctx.Dispatch() // may read a buffered wl_display.error
	}
	if application.runErr != nil {
		return application.runErr
	}
	return cause
}

// roundtrip dispatches events until a sync callback fires, i.e. until the compositor has
// processed everything sent so far.
func (application *app) roundtrip() error {
	callback, err := application.display.Sync()
	if err != nil {
		return err
	}
	done := false
	callback.SetDoneHandler(func(client.CallbackDoneEvent) { done = true })
	for !done {
		if err := application.ctx.Dispatch(); err != nil {
			return fmt.Errorf("wayland roundtrip: %w", err)
		}
	}
	return nil
}

// loop runs the event loop until the window is closed (by launch, cancel, or the
// compositor), returning any error that ended it.
func (application *app) loop() error {
	for !application.closed {
		if err := application.ctx.Dispatch(); err != nil {
			return fmt.Errorf("wayland dispatch: %w", err)
		}
	}
	return application.runErr
}

// bindGlobal binds a registry global with a correctly length-prefixed interface string.
// go-wayland v0.0.0-20230130 writes the length field as the PADDED length, leaving NUL padding
// inside the declared string, which modern libwayland rejects ("string has embedded nul") by
// closing the connection. Same request, true length, padded separately.
func bindGlobal(registry *client.Registry, name uint32, iface string, version uint32, proxy client.Proxy) error {
	const opcode = 0
	padded := client.PaddedLen(len(iface) + 1)
	total := 8 + 4 + (4 + padded) + 4 + 4
	buf := make([]byte, total)
	pos := 0
	client.PutUint32(buf[pos:pos+4], registry.ID())
	pos += 4
	client.PutUint32(buf[pos:pos+4], uint32(total<<16|opcode&0x0000ffff))
	pos += 4
	client.PutUint32(buf[pos:pos+4], name)
	pos += 4
	client.PutString(buf[pos:pos+(4+padded)], iface, len(iface)+1) // true length, not padded
	pos += 4 + padded
	client.PutUint32(buf[pos:pos+4], version)
	pos += 4
	client.PutUint32(buf[pos:pos+4], proxy.ID())
	return registry.Context().WriteMsg(buf, nil)
}

// handleGlobal binds the globals the menu needs, at the version the compositor advertises (only
// v1 requests are used, so any advertised version is safe).
func (application *app) handleGlobal(event client.RegistryGlobalEvent) {
	trace("global advertised: %s v%d (name %d)", event.Interface, event.Version, event.Name)
	switch event.Interface {
	case "wl_compositor":
		compositor := client.NewCompositor(application.ctx)
		if bindGlobal(application.registry, event.Name, event.Interface, event.Version, compositor) == nil {
			application.compositor = compositor
			trace("bound wl_compositor v%d", event.Version)
		}
	case "wl_shm":
		shm := client.NewShm(application.ctx)
		if bindGlobal(application.registry, event.Name, event.Interface, event.Version, shm) == nil {
			application.shm = shm
			trace("bound wl_shm v%d", event.Version)
		}
	case "wl_seat":
		seat := client.NewSeat(application.ctx)
		if bindGlobal(application.registry, event.Name, event.Interface, event.Version, seat) == nil {
			application.seat = seat
			seat.SetCapabilitiesHandler(application.handleSeatCapabilities)
			trace("bound wl_seat v%d", event.Version)
		}
	case "zwlr_layer_shell_v1":
		shell := newLayerShell(application.ctx)
		if bindGlobal(application.registry, event.Name, event.Interface, event.Version, shell) == nil {
			application.layerShell = shell
			trace("bound zwlr_layer_shell_v1 v%d", event.Version)
		}
	}
}

// handleSeatCapabilities wires up the keyboard once the seat reports it has one.
func (application *app) handleSeatCapabilities(event client.SeatCapabilitiesEvent) {
	hasKeyboard := event.Capabilities&uint32(client.SeatCapabilityKeyboard) != 0
	if !hasKeyboard || application.keyboard != nil {
		return
	}
	keyboard, err := application.seat.GetKeyboard()
	if err != nil {
		return
	}
	application.keyboard = keyboard
	keyboard.SetKeyHandler(application.handleKey)
	keyboard.SetModifiersHandler(application.handleModifiers)
	// We use a fixed US layout (internal/keymap), so the compositor's keymap is unused;
	// close its fd to avoid a leak.
	keyboard.SetKeymapHandler(func(event client.KeyboardKeymapEvent) {
		if event.Fd >= 0 {
			syscall.Close(event.Fd)
		}
	})
}

// handleModifiers tracks Shift and Control from the standard xkb modifier indices (Shift is
// index 0, Control index 2 in the default keymap).
func (application *app) handleModifiers(event client.KeyboardModifiersEvent) {
	application.shift = event.ModsDepressed&(1<<0) != 0
	application.ctrl = event.ModsDepressed&(1<<2) != 0
}

// handleKey maps a keypress to a picker action and redraws.
func (application *app) handleKey(event client.KeyboardKeyEvent) {
	if event.State != uint32(client.KeyboardKeyStatePressed) {
		return
	}
	// Any keypress dismisses a launch error and completes the entrance fade, so commit a frame here:
	// a key no branch below handles would otherwise leave the last buffer stuck mid-fade with no
	// further frame callback coming.
	commit := application.launchErr != ""
	application.launchErr = ""
	if application.animating {
		application.fade, application.animating = 1, false
		commit = true
	}
	if commit {
		application.redraw()
	}
	key := keymap.Decode(event.Key, application.shift)

	if application.ctrl {
		switch key.Rune {
		case 'u':
			application.model.ClearQuery()
			application.redraw()
		case 'n':
			application.model.MoveCursor(1)
			application.redraw()
		case 'p':
			application.model.MoveCursor(-1)
			application.redraw()
		case 'c':
			application.closed = true
		}
		return
	}

	if key.Printable() {
		application.model.Type(string(key.Rune))
		application.redraw()
		return
	}

	switch key.Special {
	case keymap.Enter:
		application.activateSelected()
	case keymap.Escape:
		application.closed = true
	case keymap.Backspace:
		application.model.Backspace()
		application.redraw()
	case keymap.Up:
		application.model.MoveCursor(-application.rowStep())
		application.redraw()
	case keymap.Down:
		application.model.MoveCursor(application.rowStep())
		application.redraw()
	case keymap.Left:
		if application.grid {
			application.model.MoveCursor(-1)
			application.redraw()
		}
	case keymap.Right:
		if application.grid {
			application.model.MoveCursor(1)
			application.redraw()
		}
	case keymap.PageUp:
		application.model.MoveCursor(-pageStep)
		application.redraw()
	case keymap.PageDown:
		application.model.MoveCursor(pageStep)
		application.redraw()
	case keymap.Home:
		application.model.MoveCursor(-edgeStep)
		application.redraw()
	case keymap.End:
		application.model.MoveCursor(edgeStep)
		application.redraw()
	}
}

// activateSelected runs ActivateFunc on its own goroutine: inline it would block the event loop
// for seconds or minutes while holding an exclusive keyboard grab, and the desktop would look
// hung. A second Enter while one is in flight is ignored rather than starting a duplicate.
func (application *app) activateSelected() {
	if application.activating {
		return
	}
	index, ok := application.model.SelectedIndex()
	if !ok {
		return
	}
	if application.activate == nil {
		application.selected = index
		application.closed = true
		return
	}
	item := application.items[index]
	activate := application.activate
	done := make(chan error, 1)
	application.activating = true
	application.activateItem = item.Label
	application.activateIdx = index
	application.activateDone = done
	application.launchErr = ""
	go func() { done <- activate(item) }()
	application.redraw() // paint the busy banner and arm the poll that collects the result
}

// finishActivate applies a completed activation: an error goes into the banner and the menu
// stays open (dismissed on the next keypress), success closes it with that item selected.
func (application *app) finishActivate(err error) {
	application.activating = false
	application.activateDone = nil
	if err != nil {
		application.launchErr = err.Error()
		application.redraw()
		return
	}
	application.selected = application.activateIdx
	application.closed = true
}

// awaitActivate blocks until an activation the user dismissed the overlay out of has finished, so
// no ActivateFunc call outlives Run. Called only after the overlay is off screen. The item still
// counts as activated if it succeeded.
func (application *app) awaitActivate() error {
	if !application.activating {
		return nil
	}
	err := <-application.activateDone
	application.activating = false
	application.activateDone = nil
	if err == nil {
		application.selected = application.activateIdx
	}
	return err
}

// busyBanner is the line shown while an activation runs: the caller's verb, the item, and an
// ellipsis that cycles with the compositor's frame clock, so a launch that takes minutes still
// visibly reads as working rather than as a frozen window.
func (application *app) busyBanner() string {
	if !application.activating {
		return ""
	}
	dots := 1 + int(application.frameMs/busyDotMs)%busyDotMax
	return application.busyVerb + " " + application.activateItem + strings.Repeat(".", dots)
}

// handleLayerConfigure acknowledges the layer-surface configure, adopts the size the
// compositor grants (it echoes our requested size), and draws.
func (application *app) handleLayerConfigure(serial, width, height uint32) {
	application.layerSurface.ackConfigure(serial)
	if width > 0 {
		application.width = int(width)
	}
	if height > 0 {
		application.height = int(height)
	}
	if err := application.ensureBuffer(); err != nil {
		application.runErr = err
		application.closed = true
		return
	}
	application.redraw()
}

// shmBuffer is one wl_buffer plus its shared memory. Both must stay alive, and the wl_buffer
// registered with go-wayland, until the compositor releases it - a release landing on an
// unregistered object is fatal to go-wayland's dispatch ("unable find sender"). So a resize
// retires the old buffer rather than destroying it.
type shmBuffer struct {
	buffer        *client.Buffer
	file          *os.File
	mmap          []byte
	width, height int
}

// destroy frees the buffer, mapping, and shm file. Only call it once the compositor is done
// with the buffer (on release, or at cleanup when the connection is closing anyway).
func (buf *shmBuffer) destroy() {
	if buf.buffer != nil {
		buf.buffer.Destroy()
		buf.buffer = nil
	}
	if buf.mmap != nil {
		syscall.Munmap(buf.mmap)
		buf.mmap = nil
	}
	if buf.file != nil {
		buf.file.Close()
		buf.file = nil
	}
}

// ensureBuffer makes application.current a buffer of the wanted size, retiring the old one
// (if the size changed) instead of destroying it while the compositor may still hold it.
func (application *app) ensureBuffer() error {
	width, height := application.width, application.height
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	if application.current != nil && application.current.width == width && application.current.height == height {
		return nil
	}
	if application.current != nil {
		application.retire(application.current)
		application.current = nil
	}
	buf, err := application.newBuffer(width, height)
	if err != nil {
		return err
	}
	application.current = buf
	return nil
}

// newBuffer allocates an unlinked shm file, maps it, and wraps it in a wl_buffer.
func (application *app) newBuffer(width, height int) (*shmBuffer, error) {
	stride := width * 4
	size := stride * height
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, "menu-shm-*")
	if err != nil {
		return nil, fmt.Errorf("create shm file: %w", err)
	}
	// Unlink now; the open fd (and the mapping) keep the memory alive.
	os.Remove(file.Name())
	if err := file.Truncate(int64(size)); err != nil {
		file.Close()
		return nil, err
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("mmap shm: %w", err)
	}
	pool, err := application.shm.CreatePool(int(file.Fd()), int32(size))
	if err != nil {
		syscall.Munmap(data)
		file.Close()
		return nil, err
	}
	buffer, err := pool.CreateBuffer(0, int32(width), int32(height), int32(stride), uint32(client.ShmFormatArgb8888))
	pool.Destroy()
	if err != nil {
		syscall.Munmap(data)
		file.Close()
		return nil, err
	}
	return &shmBuffer{buffer: buffer, file: file, mmap: data, width: width, height: height}, nil
}

// retire holds a resized-away buffer until the compositor releases it, then frees it. A
// buffer the compositor already released while it was current simply never fires this
// handler and is reclaimed at cleanup instead.
func (application *app) retire(buf *shmBuffer) {
	application.retired = append(application.retired, buf)
	buf.buffer.SetReleaseHandler(func(client.BufferReleaseEvent) {
		trace("compositor released a retired buffer (%dx%d)", buf.width, buf.height)
		application.freeRetired(buf)
	})
}

// freeRetired drops buf from the retired list and frees it.
func (application *app) freeRetired(buf *shmBuffer) {
	for index, candidate := range application.retired {
		if candidate == buf {
			application.retired = append(application.retired[:index], application.retired[index+1:]...)
			buf.destroy()
			return
		}
	}
}

// redraw renders the model into the shm buffer as ARGB8888 (byte order B,G,R,A little-endian) and
// commits. It reuses one buffer and does not wait for wl_buffer.release, so a burst of redraws can
// briefly tear: cosmetic, never a crash.
func (application *app) redraw() {
	buf := application.current
	if buf == nil || buf.mmap == nil {
		return
	}
	view := render.View{
		Prompt:  application.prompt,
		Footer:  application.footer,
		Fade:    application.fade,
		Opacity: application.opacity,
		Error:   application.launchErr,
		Status:  application.busyBanner(),
		Grid:    application.grid,
		CellW:   application.cellW,
		CellH:   application.cellH,
		Thumb:   application.thumbFor,
	}
	frame := render.Frame(application.model, application.palette, view, buf.width, buf.height)
	src := frame.Pix
	dst := buf.mmap
	count := len(dst)
	if len(src) < count {
		count = len(src)
	}
	for index := 0; index+3 < count; index += 4 {
		dst[index+0] = src[index+2] // B
		dst[index+1] = src[index+1] // G
		dst[index+2] = src[index+0] // R
		dst[index+3] = src[index+3] // A
	}
	application.surface.Attach(buf.buffer, 0, 0)
	// Damage (surface-local, wl_surface v1) rather than DamageBuffer (v4), so we never
	// depend on binding wl_compositor at v4+. We redraw the whole surface, so full-surface
	// damage is exactly right.
	application.surface.Damage(0, 0, int32(buf.width), int32(buf.height))
	// Ask for a frame callback while anything is left to pick up: animation, an activation, or a
	// thumbnail still decoding or decoded but not yet drawn. The "decoded but not drawn" half matters,
	// or a decode landing between Frame and this check strands its tile on a placeholder.
	//
	// The request must be followed by the commit below: wl_surface.frame is double-buffered state, so
	// asking without committing arms a callback that can never fire.
	if application.animating || application.activating || application.thumbsActive() {
		application.requestFrame()
	}
	application.surface.Commit()
}

// thumbFor returns the decoded thumbnail for a preview path, scheduling its background decode
// on first request; nil means "not ready yet" and the renderer draws a placeholder tile. It is
// the render-time bridge from the picker's Preview paths to the async thumbnail store.
func (application *app) thumbFor(preview string) *image.RGBA {
	if application.thumbs == nil {
		return nil
	}
	thumb, _ := application.thumbs.Get(preview)
	return thumb
}

// thumbsActive reports whether any grid thumbnail is still decoding or has decoded without
// being drawn yet - the condition for keeping the frame-callback poll running.
func (application *app) thumbsActive() bool {
	return application.thumbs != nil && application.thumbs.Active()
}

// rowStep is how far Up/Down move the cursor: one item in the list, a whole row (the current
// column count) in the grid, so vertical arrows move between rows rather than neighbours.
func (application *app) rowStep() int {
	if !application.grid {
		return 1
	}
	return render.GridColumns(application.width, application.cellW)
}

// requestFrame asks the compositor for a frame callback (paced to the display), so the
// entrance animation advances one step per refresh. It no-ops if one is already pending.
func (application *app) requestFrame() {
	if application.framePending || application.surface == nil {
		return
	}
	callback, err := application.surface.Frame()
	if err != nil {
		return
	}
	callback.SetDoneHandler(application.onFrame)
	application.framePending = true
}

// onFrame drives everything paced by frame callbacks: the entrance fade, a finished activation,
// and background-decoded thumbnails. Every branch that wants another frame goes through redraw,
// which commits - see the note above about frame callbacks that never fire. Redrawing an identical
// frame costs one software render; losing the poll costs the feature.
func (application *app) onFrame(event client.CallbackDoneEvent) {
	application.framePending = false
	application.frameMs = event.CallbackData
	if application.animating {
		application.advanceFade(event.CallbackData)
		application.redraw()
		return
	}
	if application.activating {
		select {
		case err := <-application.activateDone:
			application.finishActivate(err)
		default:
			application.redraw() // still working: keep the poll alive and step the ellipsis
		}
		return
	}
	if application.thumbs != nil {
		// One atomic query, not two: a decode landing between a separate TakeDirty and Pending
		// would be seen by neither, and the poll would stop with that thumbnail never drawn.
		dirty, pending := application.thumbs.TakeDirty()
		if dirty || pending {
			application.redraw() // shows whatever landed, and re-arms while work remains
		}
	}
}

// advanceFade sets the current fade from the elapsed time (ease-out cubic) and ends the
// animation once the duration is reached.
func (application *app) advanceFade(nowMs uint32) {
	if !application.animStarted {
		application.animStarted = true
		application.animStartMs = nowMs
	}
	elapsed := nowMs - application.animStartMs
	if elapsed >= fadeDurationMs {
		application.fade = 1
		application.animating = false
		trace("entrance fade done")
		return
	}
	remaining := 1 - float64(elapsed)/fadeDurationMs
	application.fade = 1 - remaining*remaining*remaining
}

// cleanup frees every buffer (current and any awaiting release) and closes the connection. It
// is idempotent: Run calls it explicitly to take the overlay down before waiting on a dismissed
// activation, and again through its defer.
func (application *app) cleanup() {
	if application.current != nil {
		application.current.destroy()
		application.current = nil
	}
	for _, buf := range application.retired {
		buf.destroy()
	}
	application.retired = nil
	if application.layerSurface != nil {
		application.layerSurface.destroy()
		application.layerSurface = nil
	}
	if application.ctx != nil {
		application.ctx.Close() // this is what takes the overlay off screen
		application.ctx = nil   // and closing an already-closed context is not the second call's job
	}
}
