// Package photo turns one untrusted photo into clean JPEG files. It is Go
// only: no child process and no C code reads the bytes of the upload.
package photo

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"sort"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/sniff"
)

type Options struct {
	Renditions []contract.Rendition
	MaxPixels  int64
}

type Result struct {
	// Width and Height are of the source as a person sees it, after the
	// EXIF orientation.
	Width, Height int
	HadMetadata   bool
	HadGPS        bool
	Animated      bool
	Outputs       []Output
	Decode        time.Duration
	Encode        time.Duration
}

type Output struct {
	Name          string
	Data          []byte
	Width, Height int
}

// Process decodes src and writes one JPEG for each rendition. The outputs hold
// the pixels and, when there is one, the RGB color profile. They hold nothing
// else from the source: no EXIF, no GPS position, no comment, no second frame.
//
// A *contract.Refusal is returned when the file is the problem.
func Process(src []byte, t sniff.Type, opts Options) (res *Result, err error) {
	// The decoders are memory-safe, so a hostile file cannot do more than
	// panic. A panic is a file the worker cannot decode.
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, contract.Refuse(contract.Undecodable, "the decoder stopped: %v", r)
		}
	}()

	decodeConfig, decode, ok := decoders(t)
	if !ok {
		return nil, contract.Refuse(contract.UnsupportedType, "%s is not a photo type this worker decodes", t.Short())
	}

	// Read the dimensions from the header first. A small file can declare a
	// very large image, and the full decode would allocate all of it.
	start := time.Now()
	cfg, err := decodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, contract.Refuse(contract.Undecodable, "the %s header cannot be read: %v", t.Short(), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, contract.Refuse(contract.Undecodable, "the image has no pixels")
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > opts.MaxPixels {
		return nil, contract.Refuse(contract.TooManyPixels, "%d x %d is %d pixels, limit %d", cfg.Width, cfg.Height, pixels, opts.MaxPixels)
	}
	img, err := decode(bytes.NewReader(src))
	if err != nil {
		return nil, contract.Refuse(contract.Undecodable, "the %s data cannot be decoded: %v", t.Short(), err)
	}
	m := scan(src, t)
	decodeTime := time.Since(start)

	b := img.Bounds()
	res = &Result{
		Width: b.Dx(), Height: b.Dy(),
		HadMetadata: m.hadMetadata, HadGPS: m.hadGPS, Animated: m.animated,
		Decode: decodeTime,
	}
	if m.orientation >= 5 {
		res.Width, res.Height = res.Height, res.Width
	}

	// Render the largest rendition from the source, and each smaller one from
	// the one before it. A second resize of a much smaller picture costs
	// almost nothing; a resize of the full source for each one costs a lot.
	order := make([]int, len(opts.Renditions))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return opts.Renditions[order[a]].MaxEdge > opts.Renditions[order[b]].MaxEdge
	})

	start = time.Now()
	res.Outputs = make([]Output, len(opts.Renditions))
	for _, i := range order {
		r := opts.Renditions[i]
		canvas := render(img, r.MaxEdge)
		img = canvas
		out := orient(canvas, m.orientation)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: r.Quality}); err != nil {
			return nil, fmt.Errorf("encode rendition %q: %w", r.Name, err)
		}
		res.Outputs[i] = Output{
			Name:   r.Name,
			Data:   embedICC(buf.Bytes(), m.icc),
			Width:  out.Bounds().Dx(),
			Height: out.Bounds().Dy(),
		}
	}
	res.Encode = time.Since(start)
	return res, nil
}

func decoders(t sniff.Type) (func(io.Reader) (image.Config, error), func(io.Reader) (image.Image, error), bool) {
	switch t {
	case sniff.JPEG:
		return jpeg.DecodeConfig, jpeg.Decode, true
	case sniff.PNG:
		return png.DecodeConfig, png.Decode, true
	case sniff.WebP:
		return webp.DecodeConfig, webp.Decode, true
	}
	return nil, nil, false
}

// render draws img onto an opaque RGBA canvas whose long edge is maxEdge at
// most. It never makes the image larger. Transparent pixels become white,
// because a JPEG has no transparency.
func render(img image.Image, maxEdge int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxEdge {
		w = max(1, (w*maxEdge+long/2)/long)
		h = max(1, (h*maxEdge+long/2)/long)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	op := draw.Src
	if o, ok := img.(interface{ Opaque() bool }); !ok || !o.Opaque() {
		draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		op = draw.Over
	}
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), img, b.Min, op)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, op, nil)
	}
	return dst
}

// orient turns the pixels so that the image needs no EXIF orientation. The
// values are the eight of the EXIF specification.
func orient(src *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2: // mirrored left to right
				sx, sy = w-1-x, y
			case 3: // turned 180 degrees
				sx, sy = w-1-x, h-1-y
			case 4: // mirrored top to bottom
				sx, sy = x, h-1-y
			case 5: // mirrored over the main diagonal
				sx, sy = y, x
			case 6: // needs a quarter turn clockwise
				sx, sy = y, h-1-x
			case 7: // mirrored over the other diagonal
				sx, sy = w-1-y, h-1-x
			case 8: // needs a quarter turn counterclockwise
				sx, sy = w-1-y, x
			}
			s := src.PixOffset(sx, sy)
			d := dst.PixOffset(x, y)
			copy(dst.Pix[d:d+4], src.Pix[s:s+4])
		}
	}
	return dst
}
