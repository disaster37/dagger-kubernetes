// Package main renders the favicon fallback set (multi-size favicon.ico and
// the apple-touch-icon.png) from the same fleet-of-daggers motif as
// ui/public/favicon.svg, using only the Go standard library, so the assets
// stay reproducible without new Go or npm dependencies. Run it from the
// repository root:
//
//	go run ./scripts/gen-favicon
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const (
	// viewBox is the SVG coordinate frame shared by favicon.svg and
	// AppLogo.vue; all blade geometry below is in those units.
	viewBox = 64

	// boundsGrid scans the viewBox at this resolution to measure the drawn
	// motif (finer than any icon pixel grid, so quantization is invisible).
	boundsGrid = 1024

	// samplesPerAxis is the per-pixel supersampling factor used to
	// antialias the motif onto the pixel grid.
	samplesPerAxis = 8

	// faviconMargin leaves a slim border so the motif does not touch the
	// tile edge at 16 px; touchMargin is the generous iOS home-screen
	// padding (iOS masks the corners itself, so none is pre-rounded).
	faviconMargin = 0.06
	touchMargin   = 0.14

	// outDir receives favicon.ico and apple-touch-icon.png (relative to the
	// repository root).
	outDir = "ui/public"
)

// accent is the fixed #58a6ff fill used by favicon.svg; darkBG is the app's
// forced-dark background (#0d1117) from ui/app/assets/css/main.css.
var (
	accent = color.NRGBA{R: 0x58, G: 0xa6, B: 0xff, A: 0xff}
	darkBG = color.NRGBA{R: 0x0d, G: 0x11, B: 0x17, A: 0xff}
)

// placement positions one blade: the SVG `use` transform
// translate(x y) rotate(deg).
type placement struct {
	x, y, deg float64
}

// rrect is a rounded rectangle in blade-local coordinates (SVG <rect> with rx).
type rrect struct {
	x0, y0, x1, y1, r float64
}

// The motif: three blades (the `#d` group of favicon.svg) placed at
// rotate(-18)/rotate(0)/rotate(+18).
var (
	placements = []placement{{21, 38, -18}, {32, 40, 0}, {43, 38, 18}}
	bladePoly  = [][2]float64{{0, -18}, {3.5, -5}, {0, -3}, {-3.5, -5}}
	bladeRects = []rrect{{-7, -5, 7, -2.5, 1}, {-2.5, -2.5, 2.5, 10.5, 1.5}}
)

// bounds is the drawn motif's bounding box in viewBox units.
type bounds struct {
	minX, minY, maxX, maxY float64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-favicon: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	box := motifBounds()

	icoSizes := []uint8{16, 32, 48}
	frames := make([]icoImage, 0, len(icoSizes))
	for _, size := range icoSizes {
		data, err := encodePNG(render(int(size), faviconMargin, color.NRGBA{}, box))
		if err != nil {
			return fmt.Errorf("render %dx%d: %w", size, size, err)
		}
		frames = append(frames, icoImage{size: size, data: data})
	}
	if err := writeAsset("favicon.ico", encodeICO(frames)); err != nil {
		return err
	}

	touch, err := encodePNG(render(180, touchMargin, darkBG, box))
	if err != nil {
		return fmt.Errorf("render 180x180: %w", err)
	}
	return writeAsset("apple-touch-icon.png", touch)
}

func writeAsset(name string, data []byte) error {
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // G306: public build assets; git tracks content, not mode.
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// motifBounds scans a fine grid over the viewBox for pixels covered by any
// blade, so centering/scaling stays correct even if the geometry changes.
func motifBounds() bounds {
	step := viewBox / float64(boundsGrid)
	box := bounds{minX: math.Inf(1), minY: math.Inf(1), maxX: math.Inf(-1), maxY: math.Inf(-1)}
	for gy := 0; gy < boundsGrid; gy++ {
		for gx := 0; gx < boundsGrid; gx++ {
			x := (float64(gx) + 0.5) * step
			y := (float64(gy) + 0.5) * step
			if !inMotif(x, y) {
				continue
			}
			box.minX = math.Min(box.minX, x)
			box.minY = math.Min(box.minY, y)
			box.maxX = math.Max(box.maxX, x)
			box.maxY = math.Max(box.maxY, y)
		}
	}
	return box
}

// inMotif reports whether the viewBox point lands on any of the three blades.
func inMotif(x, y float64) bool {
	for _, pl := range placements {
		if inBlade(x, y, pl) {
			return true
		}
	}
	return false
}

// inBlade maps the viewBox point into blade-local coordinates (the inverse
// of the SVG translate+rotate transform) and tests the blade shapes.
func inBlade(x, y float64, pl placement) bool {
	sin, cos := math.Sincos(-pl.deg * math.Pi / 180)
	dx, dy := x-pl.x, y-pl.y
	lx, ly := dx*cos-dy*sin, dx*sin+dy*cos
	if pointInPolygon(lx, ly, bladePoly) {
		return true
	}
	for _, rr := range bladeRects {
		if pointInRoundedRect(lx, ly, rr) {
			return true
		}
	}
	return false
}

// pointInPolygon is the even-odd ray-casting test for a convex polygon.
func pointInPolygon(x, y float64, poly [][2]float64) bool {
	inside := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
	}
	return inside
}

// pointInRoundedRect tests a rectangle with circular corner radius r
// (equivalent to the SVG <rect rx> shape).
func pointInRoundedRect(x, y float64, rr rrect) bool {
	cx := clamp(x, rr.x0+rr.r, rr.x1-rr.r)
	cy := clamp(y, rr.y0+rr.r, rr.y1-rr.r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= rr.r*rr.r
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// render rasterizes the motif into a size×size tile. The artwork is scaled
// uniformly to fill (1-2·margin) of the tile and centered on its drawn
// bounds. An alpha of 0 in bg yields a transparent background (favicon
// frames); an opaque bg fills the whole tile (apple-touch-icon: iOS ignores
// alpha channels, so the tile is solid).
func render(size int, margin float64, bg color.NRGBA, box bounds) *image.NRGBA {
	span := float64(size) * (1 - 2*margin)
	k := span / math.Max(box.maxX-box.minX, box.maxY-box.minY)
	ctrX, ctrY := (box.minX+box.maxX)/2, (box.minY+box.maxY)/2
	center := float64(size) / 2

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	total := float64(samplesPerAxis * samplesPerAxis)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			covered := 0
			for sy := 0; sy < samplesPerAxis; sy++ {
				for sx := 0; sx < samplesPerAxis; sx++ {
					pxs := float64(px) + (float64(sx)+0.5)/samplesPerAxis
					pys := float64(py) + (float64(sy)+0.5)/samplesPerAxis
					if inMotif((pxs-center)/k+ctrX, (pys-center)/k+ctrY) {
						covered++
					}
				}
			}
			alpha := float64(covered) / total
			img.SetNRGBA(px, py, over(bg, accent, alpha))
		}
	}
	return img
}

// over blends the accent motif over bg at coverage alpha.
func over(bg, fg color.NRGBA, alpha float64) color.NRGBA {
	if bg.A == 0 {
		return color.NRGBA{R: fg.R, G: fg.G, B: fg.B, A: uint8(alpha*255 + 0.5)}
	}
	inv := 1 - alpha
	return color.NRGBA{
		R: uint8(float64(bg.R)*inv + float64(fg.R)*alpha + 0.5),
		G: uint8(float64(bg.G)*inv + float64(fg.G)*alpha + 0.5),
		B: uint8(float64(bg.B)*inv + float64(fg.B)*alpha + 0.5),
		A: 255,
	}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("png encode: %w", err)
	}
	return buf.Bytes(), nil
}

// icoImage is one PNG-compressed frame inside the ICO container (size is the
// square edge in pixels; every size we emit fits in a single byte).
type icoImage struct {
	size uint8
	data []byte
}

// encodeICO packs PNG frames into a Windows ICO container: a 6-byte ICONDIR,
// one 16-byte ICONDIRENTRY per frame, then the raw PNG blobs back to back.
// PNG-in-ICO is the standard modern packaging (browsers and Windows Vista+
// accept it at every size).
func encodeICO(images []icoImage) []byte {
	total := 6 + 16*len(images)
	for _, img := range images {
		total += len(img.data)
	}
	out := make([]byte, 0, total)

	var dir [6]byte
	binary.LittleEndian.PutUint16(dir[2:], 1)                   // type: icon
	binary.LittleEndian.PutUint16(dir[4:], uint16(len(images))) //nolint:gosec // G115: the container holds three frames.
	out = append(out, dir[:]...)

	offset := 6 + 16*len(images)
	for _, img := range images {
		var entry [16]byte
		entry[0] = img.size
		entry[1] = img.size
		binary.LittleEndian.PutUint16(entry[4:], 1)                     // planes
		binary.LittleEndian.PutUint16(entry[6:], 32)                    // bit count
		binary.LittleEndian.PutUint32(entry[8:], uint32(len(img.data))) //nolint:gosec // G115: PNG frames are a few hundred bytes.
		binary.LittleEndian.PutUint32(entry[12:], uint32(offset))       //nolint:gosec // G115: container offsets stay in the KiB range.
		out = append(out, entry[:]...)
		offset += len(img.data)
	}
	for _, img := range images {
		out = append(out, img.data...)
	}
	return out
}
