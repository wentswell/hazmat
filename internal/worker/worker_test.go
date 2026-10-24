package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wentswell/hazmat/internal/config"
	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/store"
	"github.com/wentswell/hazmat/internal/video"
)

type fixture struct {
	t    *testing.T
	root string
	logs *bytes.Buffer
	w    *Worker
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	logs := &bytes.Buffer{}
	cfg := config.Config{
		Store:          config.StoreFile,
		FileRoot:       root,
		InputPrefixes:  []string{"uploads/"},
		OutputPrefixes: []string{"uploads/"},
		PhotoTypes:     []string{"jpeg", "png"},
		MaxPhotoBytes:  64 << 10,
		MaxPhotoPixels: 1 << 20,
		MaxOutputEdge:  4096,
		MaxRenditions:  4,

		MaxVideoBytes:   64 << 10,
		MaxVideoSeconds: 60,
		MaxFrames:       32,
	}
	return &fixture{t: t, root: root, logs: logs, w: &Worker{
		Config:  cfg,
		Store:   store.File{Root: root},
		Version: "test",
		Log:     slog.New(slog.NewJSONHandler(logs, nil)),
	}}
}

func (f *fixture) upload(data []byte) contract.Request {
	f.t.Helper()
	p := filepath.Join(f.root, "cdn", "uploads", "m1", "source")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return contract.Request{
		V:         1,
		RequestID: "m1",
		Source:    contract.Source{Bucket: "cdn", Key: "uploads/m1/source"},
		Output:    contract.Output{Bucket: "cdn", Prefix: "uploads/m1/"},
		Expect:    contract.KindPhoto,
		Photo: &contract.PhotoOptions{Renditions: []contract.Rendition{
			{Name: "image", MaxEdge: 3000, Quality: 85},
			{Name: "scan", MaxEdge: 64, Quality: 85},
		}},
		Video: &contract.VideoOptions{MaxSeconds: 16, Frames: contract.Frames{Count: 5}, FrameEdge: 1280, Quality: 85},
	}
}

func (f *fixture) refused(req contract.Request, code contract.RefusalCode) contract.Response {
	f.t.Helper()
	resp, err := f.w.Process(context.Background(), req)
	if err != nil {
		f.t.Fatalf("want a refusal %s, got the error %v", code, err)
	}
	if resp.Status != contract.StatusRefused || resp.Refusal == nil || resp.Refusal.Code != code {
		f.t.Fatalf("want a refusal %s, got %s %+v", code, resp.Status, resp.Refusal)
	}
	if len(resp.Outputs) != 0 {
		f.t.Fatalf("a refusal has outputs: %+v", resp.Outputs)
	}
	if _, err := os.Stat(filepath.Join(f.root, "cdn", "uploads", "m1", "image.jpg")); err == nil {
		f.t.Fatal("a refused file has an output in the store")
	}
	return resp
}

func jpegOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

func ftyp(brand string) []byte {
	return append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, append([]byte(brand), make([]byte, 64)...)...)
}

func TestOK(t *testing.T) {
	f := newFixture(t)
	src := jpegOf(200, 100)
	resp, err := f.w.Process(context.Background(), f.upload(src))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != contract.StatusOK || resp.V != 1 || resp.Refusal != nil {
		t.Fatalf("response: %+v", resp)
	}
	if resp.Worker.Version != "test" || resp.Worker.ConfigHash != f.w.Config.Hash() {
		t.Errorf("worker: %+v", resp.Worker)
	}
	sum := sha256.Sum256(src)
	if resp.Source.Bytes != int64(len(src)) || resp.Source.SHA256 != hex.EncodeToString(sum[:]) || resp.Source.SniffedType != "image/jpeg" {
		t.Errorf("source: %+v", resp.Source)
	}
	if m := resp.Media.(*contract.PhotoFacts); m.Kind != contract.KindPhoto || m.Width != 200 || m.Height != 100 || m.HadGPS {
		t.Errorf("media: %+v", m)
	}
	if len(resp.Outputs) != 2 {
		t.Fatalf("outputs: %+v", resp.Outputs)
	}
	want := []struct {
		name, key string
		w, h      int
	}{{"image", "uploads/m1/image.jpg", 200, 100}, {"scan", "uploads/m1/scan.jpg", 64, 32}}
	for i, o := range resp.Outputs {
		if o.Name != want[i].name || o.Key != want[i].key || o.Width != want[i].w || o.Height != want[i].h {
			t.Errorf("output %d: %+v", i, o)
		}
		// The facts describe the bytes that are in the store.
		stored, err := os.ReadFile(filepath.Join(f.root, "cdn", filepath.FromSlash(o.Key)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(stored)
		if int64(len(stored)) != o.Bytes || hex.EncodeToString(sum[:]) != o.SHA256 {
			t.Errorf("output %s: the facts do not match the stored file", o.Name)
		}
	}

	var line map[string]any
	if err := json.Unmarshal(f.logs.Bytes(), &line); err != nil {
		t.Fatalf("the log is not one JSON line: %v\n%s", err, f.logs)
	}
	if line["result"] != "ok" || line["request_id"] != "m1" || line["level"] != "INFO" {
		t.Errorf("log line: %v", line)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		expect contract.Kind
		code   contract.RefusalCode
	}{
		{"a text file under an image key", []byte("this is not a photo"), contract.KindPhoto, contract.UnsupportedType},
		{"an empty file", nil, contract.KindPhoto, contract.UnsupportedType},
		{"a HEIC file", ftyp("heic"), contract.KindPhoto, contract.UnsupportedType},
		{"a WebP file when the operator allows JPEG and PNG", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), contract.KindPhoto, contract.UnsupportedType},
		{"a GIF file", []byte("GIF89a\x01\x00\x01\x00"), contract.KindAny, contract.UnsupportedType},
		{"a video when a photo is expected", ftyp("mp42"), contract.KindPhoto, contract.WrongKind},
		{"a video, which this build cannot decode", ftyp("mp42"), contract.KindAny, contract.UnsupportedType},
		{"a photo when a video is expected", jpegOf(20, 20), contract.KindVideo, contract.WrongKind},
		{"a file above the byte ceiling", append(jpegOf(20, 20), make([]byte, 64<<10)...), contract.KindPhoto, contract.TooManyBytes},
		{"a photo above the pixel ceiling", jpegOf(1100, 1000), contract.KindPhoto, contract.TooManyPixels},
		{"half a JPEG", jpegOf(200, 100)[:300], contract.KindPhoto, contract.Undecodable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			req := f.upload(c.data)
			req.Expect = c.expect
			resp := f.refused(req, c.code)
			if resp.Refusal.Detail == "" {
				t.Error("a refusal with no detail")
			}
			if !strings.Contains(f.logs.String(), `"result":"refused"`) || !strings.Contains(f.logs.String(), string(c.code)) {
				t.Errorf("log: %s", f.logs)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	req := f.upload(jpegOf(20, 20))

	bad := req
	bad.Source.Key = "events/m1/source"
	_, err := f.w.Process(context.Background(), bad)
	var invalid *contract.InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("a key outside the prefixes: %v", err)
	}
	if !strings.Contains(f.logs.String(), `"result":"invalid_request"`) {
		t.Errorf("log: %s", f.logs)
	}

	// A source that does not exist is an error of the caller or of storage,
	// not a refusal of a file.
	f.logs.Reset()
	missing := req
	missing.Source.Key = "uploads/m2/source"
	var storage *contract.StorageError
	if _, err := f.w.Process(context.Background(), missing); !errors.As(err, &storage) {
		t.Fatalf("a missing source: %v", err)
	}
	if !strings.Contains(f.logs.String(), `"result":"storage_error"`) {
		t.Errorf("log: %s", f.logs)
	}
}

func tool(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// After a decoder that did not end by itself, the worker asks for a new
// execution environment. After an ordinary refusal it does not.
func TestRecycle(t *testing.T) {
	cases := []struct {
		name, mode, ffprobe string
		code                contract.RefusalCode
		recycled            int
	}{
		{"a timeout, on_error", config.RecycleOnError, "sleep 30", contract.DecodeTimeout, 1},
		{"a timeout, always", config.RecycleAlways, "sleep 30", contract.DecodeTimeout, 1},
		{"a timeout, never", config.RecycleNever, "sleep 30", contract.DecodeTimeout, 0},
		{"a signal, on_error", config.RecycleOnError, "kill -SEGV $$", contract.Undecodable, 1},
		{"a file ffprobe cannot read, on_error", config.RecycleOnError, "exit 1", contract.Undecodable, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			recycled := 0
			f.w.Recycle = func() { recycled++ }
			f.w.Tools = &video.Tools{FFprobe: tool(t, c.ffprobe), FFmpeg: tool(t, "exit 0")}
			f.w.Config.Recycle = c.mode
			f.w.Config.VideoContainers = []string{"mp4"}
			f.w.Config.VideoCodecs = []string{"h264"}
			f.w.Config.DecodeTimeoutSeconds = 0.3
			req := f.upload(ftyp("mp42"))
			req.Expect = contract.KindVideo
			f.refused(req, c.code)
			if recycled != c.recycled {
				t.Errorf("recycle was called %d times, want %d", recycled, c.recycled)
			}
			// The work directory of the request is gone.
			left, _ := filepath.Glob(filepath.Join(os.TempDir(), "hazmat-*"))
			if len(left) != 0 {
				t.Errorf("work directories remain: %v", left)
			}
		})
	}
}
