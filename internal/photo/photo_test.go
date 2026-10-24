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
	"os"
	"testing"

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

// profile is a block with the header fields of an ICC profile that the worker
// reads, and recognizable bytes after them.
func profile(space string, size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i * 7)
	}
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[16:], space)
	copy(p[36:], "acsp")
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
	jpg = append(jpg, iccSegments(profile("RGB ", 300))[0]...)
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
