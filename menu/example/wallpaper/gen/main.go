// Command gen writes a set of sample wallpapers for the wallpaper example's demo target.
//
// The demo needs images to show, but committing photographs would put megabytes of binary
// into the repository and drag licensing along with them. So the samples are generated
// instead: deterministic gradients, produced identically on every machine and every run, into
// a throwaway directory. Deliberately varied aspect ratios (16:9, 4:3, square, ultrawide,
// portrait) so the demo exercises the grid's aspect-preserving letterboxing rather than
// showing eight identically-shaped tiles.
//
// Usage:
//
//	gen -out DIR    # write the sample PNGs into DIR (created if missing)
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// sample is one generated wallpaper: a diagonal two-colour gradient at a given size. The
// names sort into the order the grid shows them.
type sample struct {
	name   string
	width  int
	height int
	from   color.RGBA
	to     color.RGBA
}

var samples = []sample{
	{"01-sunset", 1920, 1080, color.RGBA{0x2b, 0x10, 0x55, 0xff}, color.RGBA{0xff, 0x8a, 0x3d, 0xff}},
	{"02-forest", 1600, 1000, color.RGBA{0x0b, 0x2e, 0x1f, 0xff}, color.RGBA{0x7f, 0xc8, 0x5f, 0xff}},
	{"03-ocean", 1400, 1050, color.RGBA{0x03, 0x1b, 0x3d, 0xff}, color.RGBA{0x3d, 0xc7, 0xd6, 0xff}},
	{"04-ember", 1200, 1200, color.RGBA{0x2b, 0x06, 0x06, 0xff}, color.RGBA{0xf2, 0x53, 0x22, 0xff}},
	{"05-orchid", 1500, 1000, color.RGBA{0x28, 0x0b, 0x35, 0xff}, color.RGBA{0xd6, 0x6e, 0xf2, 0xff}},
	{"06-panorama", 2100, 900, color.RGBA{0x14, 0x14, 0x2b, 0xff}, color.RGBA{0x8a, 0x9b, 0xd6, 0xff}},
	{"07-portrait", 900, 1600, color.RGBA{0x1a, 0x1a, 0x1a, 0xff}, color.RGBA{0xd6, 0xc4, 0x8a, 0xff}},
	{"08-slate", 1280, 720, color.RGBA{0x11, 0x16, 0x1c, 0xff}, color.RGBA{0x6b, 0x7d, 0x8f, 0xff}},
}

func main() {
	out := flag.String("out", "", "directory to write the sample wallpapers into (required)")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gen: -out DIR is required")
		os.Exit(1)
	}
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for _, spec := range samples {
		path := filepath.Join(dir, spec.name+".png")
		if err := write(path, spec); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d sample wallpapers to %s\n", len(samples), dir)
	return nil
}

func write(path string, spec sample) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := png.Encode(file, gradient(spec)); err != nil {
		file.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// gradient renders spec as a diagonal interpolation from its start colour to its end colour,
// with a soft horizontal band so the thumbnails are distinguishable at grid size rather than
// reading as flat colour.
func gradient(spec sample) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, spec.width, spec.height))
	span := float64(spec.width + spec.height)
	for y := 0; y < spec.height; y++ {
		for x := 0; x < spec.width; x++ {
			ratio := float64(x+y) / span
			// A gentle band across the middle third, to break up the flat ramp.
			band := 1.0
			if middle := float64(y) / float64(spec.height); middle > 0.45 && middle < 0.55 {
				band = 1.12
			}
			img.SetRGBA(x, y, color.RGBA{
				R: blend(spec.from.R, spec.to.R, ratio, band),
				G: blend(spec.from.G, spec.to.G, ratio, band),
				B: blend(spec.from.B, spec.to.B, ratio, band),
				A: 0xff,
			})
		}
	}
	return img
}

// blend interpolates one channel from -> to at ratio, scaled by gain and clamped to a byte.
func blend(from, to uint8, ratio, gain float64) uint8 {
	value := (float64(from) + (float64(to)-float64(from))*ratio) * gain
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return uint8(value)
}
