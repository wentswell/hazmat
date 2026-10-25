package photo

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/sniff"
)

var (
	red    = color.RGBA{220, 30, 30, 255}
	green  = color.RGBA{30, 200, 30, 255}
	blue   = color.RGBA{30, 30, 220, 255}
	yellow = color.RGBA{230, 220, 30, 255}
)

// quadrants is the test picture: red top left, green top right, blue bottom
// left, yellow bottom right. It is not square, so a quarter turn shows.
func quadrants(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := red
			switch {
			case x >= w/2 && y < h/2:
				c = green
			case x < w/2 && y >= h/2:
				c = blue
			case x >= w/2 && y >= h/2:
				c = yellow
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// corners names the color at the middle of each quadrant: TL, TR, BL, BR.
func corners(t *testing.T, img image.Image) string {
	t.Helper()
	b := img.Bounds()
	name := func(x, y int) byte {
		r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
		best, bestDist := byte('?'), int64(1)<<62
		for n, c := range map[byte]color.RGBA{'R': red, 'G': green, 'B': blue, 'Y': yellow} {
			dr, dg, db := int64(r>>8)-int64(c.R), int64(g>>8)-int64(c.G), int64(bl>>8)-int64(c.B)
			if d := dr*dr + dg*dg + db*db; d < bestDist {
				best, bestDist = n, d
			}
		}
		if bestDist > 3*40*40 {
			t.Fatalf("the color at %d,%d is none of the four", x, y)
		}
		return best
	}
	w, h := b.Dx(), b.Dy()
	return string([]byte{name(w/4, h/4), name(3*w/4, h/4), name(w/4, 3*h/4), name(3*w/4, 3*h/4)})
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// segment is one JPEG marker segment.
func segment(marker byte, payload []byte) []byte {
	out := []byte{0xFF, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	return append(out, payload...)
}

// withSegments puts segments after the start marker of a JPEG.
func withSegments(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

// tiff is a little-endian EXIF block with an orientation and, when asked, a
// pointer to a GPS directory.
func tiff(orientation int, gps bool) []byte {
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	entries := 1
	if gps {
		entries = 2
	}
	b = binary.LittleEndian.AppendUint16(b, uint16(entries))
	b = append(b, 0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(orientation), 0, 0, 0)
	if gps {
		b = append(b, 0x25, 0x88, 4, 0, 1, 0, 0, 0, 64, 0, 0, 0)
	}
	return append(b, 0, 0, 0, 0)
}

func exifSegment(orientation int, gps bool) []byte {
	return segment(0xE1, append([]byte("Exif\x00\x00"), tiff(orientation, gps)...))
}

// profile is an ICC profile of size bytes with the structure the worker
// checks: the header, a tag table, and the six tags of an RGB matrix profile.
// The bytes after the tags are recognizable.
func profile(space string, size int) []byte {
	return iccProfile(space, size, "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC")
}

func iccProfile(space string, size int, sigs ...string) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i * 7)
	}
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[12:], "mntr")
	copy(p[16:], space)
	copy(p[20:], "XYZ ")
	copy(p[36:], "acsp")
	binary.BigEndian.PutUint32(p[128:], uint32(len(sigs)))
	data := 132 + 12*len(sigs)
	for i, sig := range sigs {
		e := p[132+12*i:]
		copy(e, sig)
		binary.BigEndian.PutUint32(e[4:], uint32(data+20*i))
		binary.BigEndian.PutUint32(e[8:], 20)
		copy(p[data+20*i:], "XYZ \x00\x00\x00\x00")
	}
	return p
}

func iccSegments(p []byte) [][]byte {
	const chunk = 65519
	count := (len(p) + chunk - 1) / chunk
	var segs [][]byte
	for i := 0; i < count; i++ {
		payload := append([]byte("ICC_PROFILE\x00"), byte(i+1), byte(count))
		segs = append(segs, segment(0xE2, append(payload, p[i*chunk:min((i+1)*chunk, len(p))]...)))
	}
	return segs
}

func pngChunk(kind string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, kind...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

// withPNGChunks puts chunks after the header chunk of a PNG.
func withPNGChunks(p []byte, chunks ...[]byte) []byte {
	const afterIHDR = 8 + 12 + 13
	out := append([]byte{}, p[:afterIHDR]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[afterIHDR:]...)
}

var one = []contract.Rendition{{Name: "image", MaxEdge: 3000, Quality: 90}}

func process(t *testing.T, src []byte, renditions []contract.Rendition) *Result {
	t.Helper()
	res, err := Process(src, sniff.Detect(src), Options{Renditions: renditions, MaxPixels: 50_000_000})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return res
}

func decodeOutput(t *testing.T, out Output) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(out.Data))
	if err != nil {
		t.Fatalf("the output %q is not a JPEG: %v", out.Name, err)
	}
	if b := img.Bounds(); b.Dx() != out.Width || b.Dy() != out.Height {
		t.Fatalf("the output says %dx%d and is %dx%d", out.Width, out.Height, b.Dx(), b.Dy())
	}
	return img
}

func wantRefusal(t *testing.T, err error, code contract.RefusalCode) {
	t.Helper()
	var r *contract.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal %s, got %v", code, err)
	}
	if r.Code != code {
		t.Fatalf("want %s, got %s (%s)", code, r.Code, r.Detail)
	}
}

// The expected pictures come from the EXIF specification, which says for each
// value where row 0 and column 0 of the stored image are for a viewer. They do
// not come from the code under test.
func TestOrientation(t *testing.T) {
	want := map[int]string{
		1: "RGBY", // row 0 top, column 0 left
		2: "GRYB", // row 0 top, column 0 right
		3: "YBGR", // row 0 bottom, column 0 right
		4: "BYRG", // row 0 bottom, column 0 left
		5: "RBGY", // row 0 left, column 0 top
		6: "BRYG", // row 0 right, column 0 top
		7: "YGBR", // row 0 right, column 0 bottom
		8: "GYRB", // row 0 left, column 0 bottom
	}
	base := encodeJPEG(t, quadrants(64, 48))
	for o, corner := range want {
		res := process(t, withSegments(base, exifSegment(o, false)), one)
		img := decodeOutput(t, res.Outputs[0])
		if got := corners(t, img); got != corner {
			t.Errorf("orientation %d: corners %s, want %s", o, got, corner)
		}
		w, h := 64, 48
		if o >= 5 {
			w, h = 48, 64
		}
		if img.Bounds().Dx() != w || img.Bounds().Dy() != h || res.Width != w || res.Height != h {
			t.Errorf("orientation %d: output %v, facts %dx%d, want %dx%d", o, img.Bounds(), res.Width, res.Height, w, h)
		}
	}
}

func TestMetadataIsRemoved(t *testing.T) {
	src := withSegments(encodeJPEG(t, quadrants(64, 48)),
		exifSegment(6, true),
		segment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>secret</x:xmpmeta>")),
		segment(0xED, []byte("Photoshop 3.0\x00secret")),
		segment(0xFE, []byte("secret comment")),
	)
	res := process(t, src, one)
	if !res.HadMetadata || !res.HadGPS {
		t.Fatalf("facts: had_metadata %v, had_gps %v, want both", res.HadMetadata, res.HadGPS)
	}
	out := res.Outputs[0].Data
	if bytes.Contains(out, []byte("secret")) || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("the output holds metadata of the source")
	}
	m := scan(out, sniff.JPEG)
	if m.hadMetadata || m.hadGPS || m.orientation != 1 || m.icc != nil {
		t.Fatalf("the output has metadata: %+v", m)
	}
}

func TestPlainPhotoHasNoMetadataFacts(t *testing.T) {
	res := process(t, encodeJPEG(t, quadrants(64, 48)), one)
	if res.HadMetadata || res.HadGPS || res.Animated {
		t.Fatalf("facts for a plain JPEG: %+v", res)
	}
}

func TestColorProfile(t *testing.T) {
	base := encodeJPEG(t, quadrants(64, 48))
	cases := []struct {
		name string
		icc  []byte
		keep bool
	}{
		{"small RGB profile", profile("RGB ", 548), true},
		{"RGB profile over three segments", profile("RGB ", 150_000), true},
		{"CMYK profile", profile("CMYK", 548), false},
		{"gray profile", profile("GRAY", 548), false},
		{"not a profile", bytes.Repeat([]byte{1}, 548), false},
	}
	for _, c := range cases {
		res := process(t, withSegments(base, iccSegments(c.icc)...), one)
		got := scan(res.Outputs[0].Data, sniff.JPEG).icc
		if c.keep && !bytes.Equal(got, c.icc) {
			t.Errorf("%s: the output profile has %d bytes, want the %d of the source", c.name, len(got), len(c.icc))
		}
		if !c.keep && got != nil {
			t.Errorf("%s: the output has a profile and must not", c.name)
		}
		decodeOutput(t, res.Outputs[0])
		if res.HadMetadata {
			t.Errorf("%s: a color profile is not metadata", c.name)
		}
	}

	// A profile with a missing part is dropped, not half copied.
	segs := iccSegments(profile("RGB ", 150_000))
	res := process(t, withSegments(base, segs[0], segs[2]), one)
	if scan(res.Outputs[0].Data, sniff.JPEG).icc != nil {
		t.Error("a profile with a missing segment was copied")
	}
}

func TestPNGProfileAndChunks(t *testing.T) {
	icc := profile("RGB ", 548)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(icc)
	zw.Close()
	src := withPNGChunks(encodePNG(t, quadrants(64, 48)),
		pngChunk("iCCP", append([]byte("name\x00\x00"), z.Bytes()...)),
		pngChunk("eXIf", tiff(8, true)),
		pngChunk("acTL", make([]byte, 8)),
		pngChunk("tEXt", []byte("Comment\x00secret")),
	)
	res := process(t, src, one)
	if !res.HadMetadata || !res.HadGPS || !res.Animated {
		t.Fatalf("facts: %+v", res)
	}
	if got := corners(t, decodeOutput(t, res.Outputs[0])); got != "GYRB" {
		t.Errorf("corners %s, want GYRB (orientation 8)", got)
	}
	out := res.Outputs[0].Data
	if !bytes.Equal(scan(out, sniff.JPEG).icc, icc) {
		t.Error("the PNG profile is not in the output")
	}
	if bytes.Contains(out, []byte("secret")) {
		t.Error("the output holds a text chunk of the source")
	}
}

func TestResize(t *testing.T) {
	src := encodeJPEG(t, quadrants(400, 300))
	res := process(t, src, []contract.Rendition{
		{Name: "image", MaxEdge: 3000, Quality: 85},
		{Name: "scan", MaxEdge: 128, Quality: 85},
	})
	if len(res.Outputs) != 2 {
		t.Fatalf("got %d outputs", len(res.Outputs))
	}
	// Never larger than the source.
	if o := res.Outputs[0]; o.Name != "image" || o.Width != 400 || o.Height != 300 {
		t.Errorf("image: %dx%d, want 400x300", o.Width, o.Height)
	}
	if o := res.Outputs[1]; o.Name != "scan" || o.Width != 128 || o.Height != 96 {
		t.Errorf("scan: %dx%d, want 128x96", o.Width, o.Height)
	}
	for _, o := range res.Outputs {
		if got := corners(t, decodeOutput(t, o)); got != "RGBY" {
			t.Errorf("%s: corners %s", o.Name, got)
		}
	}

	// The outputs keep the order of the request, whatever order they are made in.
	res = process(t, src, []contract.Rendition{
		{Name: "small", MaxEdge: 40, Quality: 85},
		{Name: "large", MaxEdge: 200, Quality: 85},
		{Name: "middle", MaxEdge: 100, Quality: 85},
	})
	for i, want := range []struct {
		name string
		w, h int
	}{{"small", 40, 30}, {"large", 200, 150}, {"middle", 100, 75}} {
		if o := res.Outputs[i]; o.Name != want.name || o.Width != want.w || o.Height != want.h {
			t.Errorf("output %d: %s %dx%d, want %s %dx%d", i, o.Name, o.Width, o.Height, want.name, want.w, want.h)
		}
		if got := corners(t, decodeOutput(t, res.Outputs[i])); got != "RGBY" {
			t.Errorf("%s: corners %s", want.name, got)
		}
	}

	// A portrait source is limited on its height.
	res = process(t, encodeJPEG(t, quadrants(30, 600)), []contract.Rendition{{Name: "s", MaxEdge: 100, Quality: 85}})
	if o := res.Outputs[0]; o.Width != 5 || o.Height != 100 {
		t.Errorf("portrait: %dx%d, want 5x100", o.Width, o.Height)
	}
}

func TestTransparencyBecomesWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 16; x < 32; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 200, 255})
		}
	}
	out := decodeOutput(t, process(t, encodePNG(t, img), one).Outputs[0])
	if r, g, b, _ := out.At(4, 16).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("a transparent pixel became %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
	if r, _, b, _ := out.At(28, 16).RGBA(); r>>8 > 40 || b>>8 < 160 {
		t.Errorf("an opaque pixel changed: r %d, b %d", r>>8, b>>8)
	}
}

func TestOtherPixelFormats(t *testing.T) {
	gray := image.NewGray16(image.Rect(0, 0, 40, 20))
	for i := range gray.Pix {
		gray.Pix[i] = 0x80
	}
	cmyk := image.NewCMYK(image.Rect(0, 0, 40, 20))
	paletted := image.NewPaletted(image.Rect(0, 0, 40, 20), color.Palette{red, blue})
	for name, src := range map[string][]byte{
		"16-bit gray PNG": encodePNG(t, gray),
		"CMYK JPEG":       encodeJPEG(t, cmyk),
		"paletted PNG":    encodePNG(t, paletted),
	} {
		res := process(t, src, one)
		if o := res.Outputs[0]; o.Width != 40 || o.Height != 20 {
			t.Errorf("%s: %dx%d", name, o.Width, o.Height)
		}
		decodeOutput(t, res.Outputs[0])
	}
}

func TestWebP(t *testing.T) {
	for file, wantCorners := range map[string]string{
		"lossy.webp":    "RGBY",
		"lossless.webp": "RGBY",
		"oriented.webp": "BRYG", // carries EXIF orientation 6 and a GPS pointer
	} {
		src, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if sniff.Detect(src) != sniff.WebP {
			t.Fatalf("%s is not a WebP", file)
		}
		res := process(t, src, one)
		if got := corners(t, decodeOutput(t, res.Outputs[0])); got != wantCorners {
			t.Errorf("%s: corners %s, want %s", file, got, wantCorners)
		}
		if file == "oriented.webp" && (!res.HadMetadata || !res.HadGPS || res.Width != 48 || res.Height != 64) {
			t.Errorf("%s: facts %+v", file, res)
		}
	}
}

func TestRefusals(t *testing.T) {
	jpg := encodeJPEG(t, quadrants(64, 48))
	opts := Options{Renditions: one, MaxPixels: 50_000_000}

	// A PNG header that declares 20,000 x 20,000 pixels and has no pixel data.
	ihdr := binary.BigEndian.AppendUint32(nil, 20000)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 20000)
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	bomb := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	_, err := Process(bomb, sniff.PNG, opts)
	wantRefusal(t, err, contract.TooManyPixels)

	_, err = Process(jpg, sniff.JPEG, Options{Renditions: one, MaxPixels: 64*48 - 1})
	wantRefusal(t, err, contract.TooManyPixels)

	_, err = Process(jpg[:len(jpg)/2], sniff.JPEG, opts)
	wantRefusal(t, err, contract.Undecodable)

	_, err = Process([]byte{0xFF, 0xD8, 0xFF, 0xE0}, sniff.JPEG, opts)
	wantRefusal(t, err, contract.Undecodable)

	// A PNG that says it is a JPEG.
	_, err = Process(encodePNG(t, quadrants(8, 8)), sniff.JPEG, opts)
	wantRefusal(t, err, contract.Undecodable)

	_, err = Process(jpg, sniff.HEIF, opts)
	wantRefusal(t, err, contract.UnsupportedType)
}

func TestParseTIFF(t *testing.T) {
	if o, gps := parseTIFF(tiff(6, true)); o != 6 || !gps {
		t.Errorf("little endian: %d %v", o, gps)
	}
	be := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 3, 0, 0, 0, 0, 0, 0}
	if o, gps := parseTIFF(be); o != 3 || gps {
		t.Errorf("big endian: %d %v", o, gps)
	}
	// An orientation outside 1..8, a short block, and a directory offset
	// past the end all give the default.
	for _, b := range [][]byte{tiff(9, false), tiff(0, false), {'I', 'I', 42}, {'I', 'I', 42, 0, 0xFF, 0xFF, 0xFF, 0xFF}, nil} {
		if o, _ := parseTIFF(b); o != 1 {
			t.Errorf("parseTIFF(%v) = %d, want 1", b, o)
		}
	}
}

func FuzzScan(f *testing.F) {
	jpg := []byte{0xFF, 0xD8}
	jpg = append(jpg, exifSegment(6, true)...)
	jpg = append(jpg, iccSegments(profile("RGB ", 400))[0]...)
	f.Add(jpg)
	f.Add(append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("eXIf", tiff(3, false))...))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, typ := range []sniff.Type{sniff.JPEG, sniff.PNG, sniff.WebP} {
			m := scan(b, typ)
			if m.orientation < 1 || m.orientation > 8 {
				t.Fatalf("orientation %d", m.orientation)
			}
			if len(m.icc) > maxICC {
				t.Fatalf("a profile of %d bytes", len(m.icc))
			}
			// A profile that comes out is one the check accepts as it is,
			// and holds exactly the size its header declares.
			if m.icc != nil && (!bytes.Equal(rgbProfile(m.icc), m.icc) || binary.BigEndian.Uint32(m.icc) != uint32(len(m.icc))) {
				t.Fatalf("a profile that does not pass its own check")
			}
		}
	})
}

// FuzzProcess gives the decoders hostile bytes. Process must return a result
// or a refusal, and must not panic or use memory without a limit.
func FuzzProcess(f *testing.F) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, quadrants(16, 12), nil)
	f.Add(buf.Bytes())
	buf.Reset()
	png.Encode(&buf, quadrants(16, 12))
	f.Add(buf.Bytes())
	f.Add(progressiveJPEG(64, 64, maxJPEGScans+1))
	f.Fuzz(func(t *testing.T, b []byte) {
		typ := sniff.Detect(b)
		res, err := Process(b, typ, Options{
			Renditions: []contract.Rendition{{Name: "s", MaxEdge: 64, Quality: 80}},
			MaxPixels:  1 << 20,
		})
		var refusal *contract.Refusal
		if err != nil && !errors.As(err, &refusal) {
			t.Fatalf("an error that is not a refusal: %v", err)
		}
		if err == nil && (len(res.Outputs) != 1 || res.Outputs[0].Width > 64 || res.Outputs[0].Height > 64) {
			t.Fatalf("a bad result: %+v", res.Outputs)
		}
	})
}

func BenchmarkPhone12MP(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 4032, 3024))
	for i := range img.Pix {
		img.Pix[i] = byte(i*31 + i/4032)
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	src := buf.Bytes()
	opts := Options{
		Renditions: []contract.Rendition{{Name: "image", MaxEdge: 3000, Quality: 85}, {Name: "scan", MaxEdge: 1280, Quality: 85}},
		MaxPixels:  50_000_000,
	}
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Process(src, sniff.JPEG, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// The profile is the one part of the source an output carries, so it must be
// a profile in structure, not a block with a few header bytes right.
func TestRGBProfile(t *testing.T) {
	set := func(p []byte, off int, v string) []byte { copy(p[off:], v); return p }
	u32 := func(p []byte, off int, v uint32) []byte { binary.BigEndian.PutUint32(p[off:], v); return p }
	valid := profile("RGB ", 548)
	cases := []struct {
		name string
		icc  []byte
		want []byte
	}{
		{"matrix profile", valid, valid},
		{"lookup table profile", iccProfile("RGB ", 548, "A2B0", "desc"), iccProfile("RGB ", 548, "A2B0", "desc")},
		{"Lab connection space", set(profile("RGB ", 548), 20, "Lab "), set(profile("RGB ", 548), 20, "Lab ")},
		{"input class", set(profile("RGB ", 548), 12, "scnr"), set(profile("RGB ", 548), 12, "scnr")},
		{"padding after the profile", append(append([]byte{}, valid...), 0, 0, 0, 0), valid},
		{"declared size one more", u32(profile("RGB ", 548), 0, 549), nil},
		{"declared size below the table", u32(profile("RGB ", 548), 0, 131), nil},
		{"device link class", set(profile("RGB ", 548), 12, "link"), nil},
		{"named color class", set(profile("RGB ", 548), 12, "nmcl"), nil},
		{"connection space not XYZ or Lab", set(profile("RGB ", 548), 20, "RGB "), nil},
		{"no signature", set(profile("RGB ", 548), 36, "xxxx"), nil},
		{"CMYK", profile("CMYK", 548), nil},
		{"gray", profile("GRAY", 548), nil},
		{"no tags", u32(profile("RGB ", 548), 128, 0), nil},
		{"more tags than the ceiling", u32(profile("RGB ", 548), 128, maxICCTags+1), nil},
		{"tag table past the end", u32(profile("RGB ", 548), 128, 40), nil},
		{"tag past the end", u32(profile("RGB ", 548), 132+8, 1000), nil},
		{"tag offset past four gigabytes", u32(u32(profile("RGB ", 548), 132+4, 0xFFFFFFF0), 132+8, 0x20), nil},
		{"tag inside the table", u32(profile("RGB ", 548), 132+4, 132), nil},
		{"tag smaller than its type", u32(profile("RGB ", 548), 132+8, 4), nil},
		{"one curve missing", iccProfile("RGB ", 548, "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC"), nil},
		{"only text tags", iccProfile("RGB ", 548, "desc", "cprt"), nil},
		{"above 256 KiB", profile("RGB ", 256<<10+1), nil},
		{"256 KiB", profile("RGB ", 256<<10), profile("RGB ", 256<<10)},
		{"shorter than a header", valid[:131], nil},
		{"the old fake header", func() []byte {
			p := bytes.Repeat([]byte("Exif GPS 51.5N"), 40)
			return set(set(p, 16, "RGB "), 36, "acsp")
		}(), nil},
	}
	for _, c := range cases {
		if got := rgbProfile(c.icc); !bytes.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: got %d bytes, want %d", c.name, len(got), len(c.want))
		}
	}
}

// A block that has only the bytes the old check read reaches no output.
func TestFakeProfileIsDropped(t *testing.T) {
	fake := bytes.Repeat([]byte("Exif GPS 51.5N "), 300)
	binary.BigEndian.PutUint32(fake, uint32(len(fake)))
	copy(fake[16:], "RGB ")
	copy(fake[36:], "acsp")
	res := process(t, withSegments(encodeJPEG(t, quadrants(64, 48)), iccSegments(fake)...), one)
	out := res.Outputs[0].Data
	if bytes.Contains(out, []byte("ICC_PROFILE")) || bytes.Contains(out, []byte("GPS 51.5N")) {
		t.Error("the output carries the fake profile")
	}
	decodeOutput(t, res.Outputs[0])
}

// progressiveJPEG is a gray w x h progressive JPEG with the given count of AC
// refinement scans. One scan is a few bytes: a symbol that ends the bands of
// 16384 blocks. The decoder still walks every coefficient of every block.
func progressiveJPEG(w, h, scans int) []byte {
	b := []byte{0xFF, 0xD8}
	b = append(b, segment(0xDB, append([]byte{0x00}, bytes.Repeat([]byte{1}, 64)...))...) // DQT
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(h))
	sof = binary.BigEndian.AppendUint16(sof, uint16(w))
	sof = append(sof, 1, 1, 0x11, 0)
	b = append(b, segment(0xC2, sof)...) // SOF2, one component
	dht := append([]byte{0x10, 1}, make([]byte, 15)...)
	b = append(b, segment(0xC4, append(dht, 0xE0))...) // AC table: one code, symbol 0xE0
	blocks := ((w + 7) / 8) * ((h + 7) / 8)
	symbols := (blocks + 16383) / 16384
	data := make([]byte, (symbols*15+7)/8)
	sos := segment(0xDA, []byte{1, 1, 0x00, 1, 63, 0x10}) // Ss=1 Se=63 Ah=1 Al=0
	for i := 0; i < scans; i++ {
		b = append(b, sos...)
		b = append(b, data...)
	}
	return append(b, 0xFF, 0xD9)
}

func TestProgressiveScans(t *testing.T) {
	small := []contract.Rendition{{Name: "s", MaxEdge: 64, Quality: 80}}
	run := func(src []byte) (*Result, error, time.Duration) {
		start := time.Now()
		res, err := Process(src, sniff.JPEG, Options{Renditions: small, MaxPixels: 50_000_000})
		return res, err, time.Since(start)
	}
	// A real encoder makes about 10 scans, and libjpeg accepts 100.
	for _, n := range []int{1, 10, maxJPEGScans} {
		if _, err, _ := run(progressiveJPEG(256, 256, n)); err != nil {
			t.Errorf("%d scans: %v", n, err)
		}
	}
	if _, err, _ := run(progressiveJPEG(256, 256, maxJPEGScans+1)); err == nil {
		t.Errorf("%d scans were accepted", maxJPEGScans+1)
	} else {
		wantRefusal(t, err, contract.Undecodable)
	}

	// 400 scans of a 16-megapixel picture took 6 s and were accepted. Now
	// the decoder stops at the limit, so the time of a refusal does not grow
	// with the scans after it.
	_, err, atLimit := run(progressiveJPEG(1024, 1024, maxJPEGScans+1))
	wantRefusal(t, err, contract.Undecodable)
	_, err, far := run(progressiveJPEG(1024, 1024, 20*maxJPEGScans))
	wantRefusal(t, err, contract.Undecodable)
	if far > 3*atLimit+100*time.Millisecond {
		t.Errorf("%d scans took %v to refuse, %d took %v", 20*maxJPEGScans, far, maxJPEGScans+1, atLimit)
	}

	// The decoder skips bytes between segments, so a junk byte before each
	// scan must not hide it.
	src := progressiveJPEG(256, 256, maxJPEGScans+1)
	junk := bytes.ReplaceAll(src, []byte{0xFF, 0xDA}, []byte{'A', 0xFF, 0xDA})
	_, err, _ = run(junk)
	wantRefusal(t, err, contract.Undecodable)
	// Fill bytes before the marker.
	_, err, _ = run(bytes.ReplaceAll(src, []byte{0xFF, 0xDA}, []byte{0xFF, 0xFF, 0xFF, 0xDA}))
	wantRefusal(t, err, contract.Undecodable)

	// An ordinary baseline file is not touched.
	if _, err, _ := run(encodeJPEG(t, quadrants(64, 48))); err != nil {
		t.Errorf("baseline: %v", err)
	}
}

// The count does not depend on how the reads split the bytes.
func TestScanLimitAcrossReads(t *testing.T) {
	for _, n := range []int{maxJPEGScans, maxJPEGScans + 1} {
		src := progressiveJPEG(64, 64, n)
		s := &scanLimit{r: iotest.OneByteReader(bytes.NewReader(src))}
		got, err := io.ReadAll(s)
		if n <= maxJPEGScans && (err != nil || !bytes.Equal(got, src)) {
			t.Errorf("%d scans: %v, read %d of %d bytes", n, err, len(got), len(src))
		}
		if n > maxJPEGScans && !errors.Is(err, errTooManyScans) {
			t.Errorf("%d scans: %v", n, err)
		}
		if s.scans != min(n, maxJPEGScans+1) {
			t.Errorf("%d scans counted as %d", n, s.scans)
		}
	}
}

// A JPEG edge is 16 bits. The configuration stops a larger max_edge at start;
// below it, an image wider than that is made smaller and encodes. Above it,
// the encoder fails, and that is an error of the worker, not of the file.
func TestJPEGEdgeLimit(t *testing.T) {
	src := encodePNG(t, image.NewRGBA(image.Rect(0, 0, 70000, 2)))
	res, err := Process(src, sniff.PNG, Options{Renditions: []contract.Rendition{{Name: "w", MaxEdge: 65535, Quality: 80}}, MaxPixels: 1 << 20})
	if err != nil || res.Outputs[0].Width != 65535 {
		t.Fatalf("an edge of 65535: %v", err)
	}
	_, err = Process(src, sniff.PNG, Options{Renditions: []contract.Rendition{{Name: "w", MaxEdge: 70000, Quality: 80}}, MaxPixels: 1 << 20})
	var failed *contract.WorkerError
	if !errors.As(err, &failed) {
		t.Errorf("an edge of 70000: want a WorkerError, got %T %v", err, err)
	}
}
