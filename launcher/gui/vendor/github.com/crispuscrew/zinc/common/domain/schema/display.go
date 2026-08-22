package schema

// How a fixed-size guest screen is realised, kept here because the authoring side and the runtime need
// the same answer and two copies of these limits would drift.
//
// A guest with no display driver keeps the mode the firmware gave it at boot, and the firmware learns
// it from an EDID the emulated display generates. That EDID has real field widths, so three limits
// apply, all measured against OVMF - and each fails the same silent way, falling back to 1280x800.

// GuestDisplayMaxPixels is the largest value the EDID's active-pixel fields can hold. They
// are 12 bits wide, so 4096 in either direction is not representable: a 4096x2160 guest
// comes up at 1280x800, while 3840x2400 is fine.
const GuestDisplayMaxPixels = 4095

// guestDisplayRefreshRates are tried highest first. The rate is close to cosmetic for a guest, but it
// multiplies the EDID's pixel clock, which is a 16-bit field in units of 10 kHz. QEMU defaults to
// 75 Hz, which is what puts 4K out of reach: 3840x2160 overflows at 75 and at 60, and fits at 50.
var guestDisplayRefreshRates = []int{60000, 50000, 30000}

// GuestDisplayMode is the emulated display's configuration for one screen size.
type GuestDisplayMode struct {
	RefreshMilliHz int   // EDID refresh rate, chosen so the pixel clock fits
	VideoMemBytes  int64 // framebuffer memory the display device needs
}

// GuestDisplay returns how to realise a screen size, and false when no configuration can.
// A caller that gets false must refuse rather than fall back, because the guest's own
// fallback is silent.
func GuestDisplay(width, height int) (GuestDisplayMode, bool) {
	if width <= 0 || height <= 0 ||
		width > GuestDisplayMaxPixels || height > GuestDisplayMaxPixels {
		return GuestDisplayMode{}, false
	}
	for _, refresh := range guestDisplayRefreshRates {
		if edidClockFits(refresh, width, height) {
			return GuestDisplayMode{
				RefreshMilliHz: refresh,
				VideoMemBytes:  videoMemFor(width, height),
			}, true
		}
	}
	return GuestDisplayMode{}, false
}

// edidClockFits mirrors how QEMU derives the EDID pixel clock: blanking is 35% of the width
// and 3.5% of the height, and the result is stored in 16 bits in units of 10 kHz.
func edidClockFits(refreshMilliHz, width, height int) bool {
	horizontalTotal := int64(width) + int64(width)*35/100
	verticalTotal := int64(height) + int64(height)*35/1000
	clock := int64(refreshMilliHz) * horizontalTotal * verticalTotal / 10_000_000
	return clock <= 65535
}

// videoMemFor sizes the framebuffer: 32 bits per pixel, rounded up to a power of two, never
// below the emulated display's own 16 MiB default. Left at that default a 4K guest has less
// memory than its screen needs and quietly comes up at 1280x800 instead.
func videoMemFor(width, height int) int64 {
	const minimum = 16 << 20
	needed := int64(width) * int64(height) * 4
	size := int64(minimum)
	for size < needed {
		size *= 2
	}
	return size
}
