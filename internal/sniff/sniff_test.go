package sniff

import "testing"

func ftyp(brand string) []byte {
	return append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, append([]byte(brand), 0, 0, 0, 0)...)
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want Type
	}{
		{"JPEG", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16}, JPEG},
		{"PNG", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), PNG},
		{"WebP", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), WebP},
		{"GIF", []byte("GIF89a\x01\x00\x01\x00"), GIF},
		{"HEIC, as an iPhone writes it", ftyp("heic"), HEIF},
		{"HEIF with the generic brand", ftyp("mif1"), HEIF},
		{"AVIF", ftyp("avif"), AVIF},
		{"MP4", ftyp("isom"), MP4},
		{"MP4 from an iPhone export", ftyp("mp42"), MP4},
		{"QuickTime", ftyp("qt  "), MOV},
		{"a WAV file is RIFF and not WebP", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), Unknown},
		{"text", []byte("hello, this is not media"), Unknown},
		{"HTML", []byte("<!doctype html><html>"), Unknown},
		{"two bytes of a JPEG", []byte{0xFF, 0xD8}, Unknown},
		{"empty", nil, Unknown},
	}
	for _, c := range cases {
		if got := Detect(c.head); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKinds(t *testing.T) {
	for _, typ := range []Type{JPEG, PNG, WebP, GIF, HEIF, AVIF} {
		if !typ.IsPhoto() || typ.IsVideo() {
			t.Errorf("%s must be a photo", typ)
		}
	}
	for _, typ := range []Type{MP4, MOV} {
		if typ.IsPhoto() || !typ.IsVideo() {
			t.Errorf("%s must be a video", typ)
		}
	}
	if Unknown.IsPhoto() || Unknown.IsVideo() || Unknown.Short() != "unknown" {
		t.Error("unknown is no kind")
	}
}

func FuzzDetect(f *testing.F) {
	f.Add([]byte{0xFF, 0xD8, 0xFF})
	f.Add(ftyp("heic"))
	f.Fuzz(func(t *testing.T, b []byte) {
		typ := Detect(b)
		if typ.IsPhoto() && typ.IsVideo() {
			t.Fatalf("%q is two kinds", typ)
		}
	})
}
