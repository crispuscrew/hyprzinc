// Package imgutil is the menu's shared image helper: one hardened decode plus two scalers. The decode
// is bounded and panic-safe because the paths come from partly-untrusted config (an app's Icon, a
// wallpaper directory). Square resizes to a fixed square (icons); Fit preserves aspect ratio
// (thumbnails).
package imgutil

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif"  // register the GIF decoder for image.Decode
	_ "image/jpeg" // register the JPEG decoder for image.Decode
	_ "image/png"  // register the PNG decoder for image.Decode
	"io"
	"math"
	"os"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the pure-Go WebP decoder (common for wallpapers)
)

// Decode reads and decodes an image file, bounded because the path may come from partly-untrusted
// config: a REGULAR file only (so a FIFO cannot block the open), capped bytes read, a decoded-size
// ceiling, and a recovered decoder panic. Any failure returns nil, which callers render as no image.
// The budget is in decoded BYTES, since the same pixel count costs one byte per pixel as a paletted
// GIF and eight at 16 bits per channel.
func Decode(path string, maxBytes, maxDecodedBytes int64) (result image.Image) {
	defer func() {
		if recover() != nil {
			result = nil
		}
	}()
	// Stat, not Lstat: Lstat describes the link itself, so IsRegular is false for every
	// symlink and symlinked icons and wallpapers would silently decode to nothing. Stat
	// resolves to the target and still reports its mode, so FIFOs, directories and devices
	// stay rejected - including through a symlink.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil
	}
	if decodedBytes(config) > maxDecodedBytes {
		return nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return decoded
}

// decodedBytes estimates how much memory decoding config will allocate, from the pixel count
// and the colour model the header declares. It never under-estimates for the models the
// registered decoders produce, which is what makes it usable as a guard: a subsampled JPEG is
// charged the 4:4:4 rate, and an unrecognised model is charged the widest rate we know of.
func decodedBytes(config image.Config) int64 {
	return int64(config.Width) * int64(config.Height) * bytesPerPixel(config.ColorModel)
}

func bytesPerPixel(model color.Model) int64 {
	// A paletted image (GIF, and PNG's colour type 3) stores one index per pixel; its model is
	// the palette itself, so it cannot be compared against the named models below.
	if _, paletted := model.(color.Palette); paletted {
		return 1
	}
	switch model {
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	case color.RGBAModel, color.NRGBAModel, color.CMYKModel, color.NYCbCrAModel:
		return 4
	case color.YCbCrModel:
		return 3 // three full planes; a subsampled image allocates less, never more
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.GrayModel, color.AlphaModel:
		return 1
	default:
		return 8
	}
}

// Square returns source resized to size x size as premultiplied RGBA, preserving alpha so
// transparent backgrounds composite cleanly. It is what icons draw, since they sit in a fixed
// square cell. It returns nil for a nil source or a non-positive size.
func Square(source image.Image, size int) *image.RGBA {
	if source == nil || size <= 0 {
		return nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), source, source.Bounds(), xdraw.Over, nil)
	return dst
}

// Fit returns source scaled to fit within boxW x boxH preserving aspect ratio (letterboxed,
// never cropped or stretched), as premultiplied RGBA sized to the fitted dimensions. It is
// what thumbnails draw, since their source aspect ratios vary. A source already smaller than
// the box is kept at native size rather than upscaled (upscaling a small image only blurs it).
// It returns nil for a nil source or a non-positive box.
func Fit(source image.Image, boxW, boxH int) *image.RGBA {
	if source == nil || boxW <= 0 || boxH <= 0 {
		return nil
	}
	srcW := source.Bounds().Dx()
	srcH := source.Bounds().Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil
	}
	scale := math.Min(float64(boxW)/float64(srcW), float64(boxH)/float64(srcH))
	if scale > 1 {
		scale = 1 // never upscale; show a small image at native size, centered by the caller
	}
	dstW := int(float64(srcW)*scale + 0.5)
	dstH := int(float64(srcH)*scale + 0.5)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), source, source.Bounds(), xdraw.Over, nil)
	return dst
}
