package menu

// Minimal client for wlr-layer-shell-unstable-v1. go-wayland ships only core + xdg-shell, so this is
// hand-written in the style of its generated code: proxy types embedding client.BaseProxy, requests
// encoded onto the wire, and a Dispatch that decodes the events we care about. Only the subset the
// menu needs - enough to float a fixed-size, centered, keyboard-grabbing overlay above tiled windows.

import (
	"github.com/rajveermalviya/go-wayland/wayland/client"
)

// zwlr_layer_shell_v1.layer: which layer the surface lives on. Overlay renders above normal
// windows, which is what a launcher wants.
const (
	layerBackground uint32 = 0
	layerBottom     uint32 = 1
	layerTop        uint32 = 2
	layerOverlay    uint32 = 3
)

// zwlr_layer_surface_v1.keyboard_interactivity: exclusive makes the compositor route all
// keyboard input to this surface while it is up, so typing goes to the picker.
const (
	keyboardInteractivityNone      uint32 = 0
	keyboardInteractivityExclusive uint32 = 1
	keyboardInteractivityOnDemand  uint32 = 2
)

// layerShell is a bound zwlr_layer_shell_v1 global. It has no events, so it needs no Dispatch.
type layerShell struct {
	client.BaseProxy
}

func newLayerShell(ctx *client.Context) *layerShell {
	shell := &layerShell{}
	ctx.Register(shell)
	return shell
}

// getLayerSurface turns a wl_surface into a layer surface (request 0). output is nullable
// (nil lets the compositor pick, normally the focused output); namespace is a free-form tag
// the compositor may use in rules.
func (shell *layerShell) getLayerSurface(surface *client.Surface, output *client.Output, layer uint32, namespace string) *layerSurface {
	surf := newLayerSurface(shell.Context())
	const opcode uint32 = 0
	nsPadded := client.PaddedLen(len(namespace) + 1)
	total := 8 + 4 + 4 + 4 + 4 + (4 + nsPadded)
	buf := make([]byte, total)
	pos := 0
	client.PutUint32(buf[pos:pos+4], shell.ID())
	pos += 4
	client.PutUint32(buf[pos:pos+4], uint32(total)<<16|opcode)
	pos += 4
	client.PutUint32(buf[pos:pos+4], surf.ID())
	pos += 4
	client.PutUint32(buf[pos:pos+4], surface.ID())
	pos += 4
	var outputID uint32 // 0 encodes a null object
	if output != nil {
		outputID = output.ID()
	}
	client.PutUint32(buf[pos:pos+4], outputID)
	pos += 4
	client.PutUint32(buf[pos:pos+4], layer)
	pos += 4
	client.PutString(buf[pos:pos+(4+nsPadded)], namespace, len(namespace)+1) // true length, not padded
	// Errors surface on the next roundtrip as a wl_display.error; the caller drains one.
	_ = shell.Context().WriteMsg(buf, nil)
	return surf
}

// layerSurface is a zwlr_layer_surface_v1. The compositor sends it configure (with the size
// to use) and closed (when it is dismissed).
type layerSurface struct {
	client.BaseProxy
	configureHandler func(serial, width, height uint32)
	closedHandler    func()
}

func newLayerSurface(ctx *client.Context) *layerSurface {
	surf := &layerSurface{}
	ctx.Register(surf)
	return surf
}

func (surf *layerSurface) request0(opcode uint32) error {
	const total = 8
	var buf [total]byte
	client.PutUint32(buf[0:4], surf.ID())
	client.PutUint32(buf[4:8], uint32(total)<<16|opcode)
	return surf.Context().WriteMsg(buf[:], nil)
}

func (surf *layerSurface) request1(opcode, arg uint32) error {
	const total = 8 + 4
	var buf [total]byte
	client.PutUint32(buf[0:4], surf.ID())
	client.PutUint32(buf[4:8], uint32(total)<<16|opcode)
	client.PutUint32(buf[8:12], arg)
	return surf.Context().WriteMsg(buf[:], nil)
}

func (surf *layerSurface) request2(opcode, arg0, arg1 uint32) error {
	const total = 8 + 4 + 4
	var buf [total]byte
	client.PutUint32(buf[0:4], surf.ID())
	client.PutUint32(buf[4:8], uint32(total)<<16|opcode)
	client.PutUint32(buf[8:12], arg0)
	client.PutUint32(buf[12:16], arg1)
	return surf.Context().WriteMsg(buf[:], nil)
}

// setSize asks for a fixed width x height (request 0). With no anchors set, the compositor
// centers a fixed-size surface.
func (surf *layerSurface) setSize(width, height uint32) error { return surf.request2(0, width, height) }

// setAnchor pins the surface to compositor edges (request 1); 0 anchors nothing, i.e. center.
func (surf *layerSurface) setAnchor(anchor uint32) error { return surf.request1(1, anchor) }

// setKeyboardInteractivity controls keyboard focus (request 4).
func (surf *layerSurface) setKeyboardInteractivity(mode uint32) error { return surf.request1(4, mode) }

// ackConfigure acknowledges a configure serial (request 6); required before attaching a buffer.
func (surf *layerSurface) ackConfigure(serial uint32) error { return surf.request1(6, serial) }

// destroy tears the surface down (request 7) and unregisters it.
func (surf *layerSurface) destroy() error {
	defer surf.Context().Unregister(surf)
	return surf.request0(7)
}

// Dispatch decodes the two events the menu handles: configure(serial, width, height) and closed.
func (surf *layerSurface) Dispatch(opcode uint32, fd int, data []byte) {
	switch opcode {
	case 0: // configure(serial, width, height)
		if surf.configureHandler == nil || len(data) < 12 {
			return // ignore a short/malformed event rather than slicing out of bounds
		}
		serial := client.Uint32(data[0:4])
		width := client.Uint32(data[4:8])
		height := client.Uint32(data[8:12])
		surf.configureHandler(serial, width, height)
	case 1: // closed
		if surf.closedHandler != nil {
			surf.closedHandler()
		}
	}
}
