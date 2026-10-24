package contract

import (
	"errors"
	"strings"
	"testing"

	"github.com/wentswell/hazmat/internal/config"
)

func s3Config() config.Config {
	return config.Config{
		Store:          config.StoreS3,
		AllowedBuckets: []string{"cdn"},
		InputPrefixes:  []string{"uploads/"},
		OutputPrefixes: []string{"uploads/"},
		MaxOutputEdge:  4096,
		MaxRenditions:  2,

		MaxVideoSeconds: 60,
		MaxFrames:       32,
	}
}

func goodVideo() *VideoOptions {
	return &VideoOptions{MaxSeconds: 16, Frames: Frames{Count: 5}, FrameEdge: 1280, Quality: 85}
}

func good() Request {
	return Request{
		V:         1,
		RequestID: "media_3f9c",
		Source:    Source{Bucket: "cdn", Key: "uploads/media_3f9c/source", VersionID: "Xk2"},
		Output:    Output{Bucket: "cdn", Prefix: "uploads/media_3f9c/"},
		Expect:    KindPhoto,
		Photo: &PhotoOptions{Renditions: []Rendition{
			{Name: "image", MaxEdge: 3000, Quality: 85},
			{Name: "scan", MaxEdge: 1280, Quality: 85},
		}},
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(good(), s3Config()); err != nil {
		t.Fatalf("the good request: %v", err)
	}

	cases := map[string]func(*Request){
		"version":                   func(r *Request) { r.V = 2 },
		"request_id":                func(r *Request) { r.RequestID = "a b" },
		"request_id empty":          func(r *Request) { r.RequestID = "" },
		"no version_id":             func(r *Request) { r.Source.VersionID = "" },
		"source bucket":             func(r *Request) { r.Source.Bucket = "other" },
		"output bucket":             func(r *Request) { r.Output.Bucket = "other" },
		"source prefix":             func(r *Request) { r.Source.Key = "events/x/source" },
		"output prefix":             func(r *Request) { r.Output.Prefix = "events/x/" },
		"output prefix no slash":    func(r *Request) { r.Output.Prefix = "uploads/x" },
		"dot segment":               func(r *Request) { r.Source.Key = "uploads/../events/x" },
		"dot segment in prefix":     func(r *Request) { r.Output.Prefix = "uploads/../events/" },
		"empty segment":             func(r *Request) { r.Source.Key = "uploads//x" },
		"absolute key":              func(r *Request) { r.Source.Key = "/uploads/x" },
		"control character":         func(r *Request) { r.Source.Key = "uploads/x\ny" },
		"expect":                    func(r *Request) { r.Expect = "audio" },
		"no photo options":          func(r *Request) { r.Photo = nil },
		"no renditions":             func(r *Request) { r.Photo.Renditions = nil },
		"too many renditions":       func(r *Request) { r.Photo.Renditions = append(r.Photo.Renditions, Rendition{"third", 100, 80}) },
		"rendition name":            func(r *Request) { r.Photo.Renditions[0].Name = "../image" },
		"reserved name":             func(r *Request) { r.Photo.Renditions[0].Name = "frame_01" },
		"name twice":                func(r *Request) { r.Photo.Renditions[1].Name = "image" },
		"edge above the ceiling":    func(r *Request) { r.Photo.Renditions[0].MaxEdge = 4097 },
		"edge too small":            func(r *Request) { r.Photo.Renditions[0].MaxEdge = 0 },
		"quality":                   func(r *Request) { r.Photo.Renditions[0].Quality = 101 },
		"rendition over the source": func(r *Request) { r.Source.Key = "uploads/media_3f9c/image.jpg" },
	}
	for name, change := range cases {
		req := good()
		change(&req)
		err := Validate(req, s3Config())
		var invalid *InvalidRequestError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: want an invalid_request error, got %v", name, err)
		}
	}
}

func TestValidateVideo(t *testing.T) {
	// A video request needs no photo options, and a photo request no video options.
	req := good()
	req.Expect, req.Photo, req.Video = KindVideo, nil, goodVideo()
	if err := Validate(req, s3Config()); err != nil {
		t.Fatal(err)
	}
	req = good()
	req.Expect, req.Video = KindAny, goodVideo()
	if err := Validate(req, s3Config()); err != nil {
		t.Fatal(err)
	}
	every := goodVideo()
	every.Frames = Frames{EverySeconds: 1}
	req.Video = every
	if err := Validate(req, s3Config()); err != nil {
		t.Fatalf("one frame for each second of 16 s: %v", err)
	}

	cases := map[string]func(*Request){
		"video expected, no options": func(r *Request) { r.Video = nil },
		"any expected, no options":   func(r *Request) { r.Expect = KindAny; r.Photo = good().Photo; r.Video = nil },
		"max_seconds zero":           func(r *Request) { r.Video.MaxSeconds = 0 },
		"max_seconds above ceiling":  func(r *Request) { r.Video.MaxSeconds = 61 },
		"no frame rule":              func(r *Request) { r.Video.Frames = Frames{} },
		"two frame rules":            func(r *Request) { r.Video.Frames = Frames{Count: 5, EverySeconds: 1} },
		"count above ceiling":        func(r *Request) { r.Video.Frames.Count = 33 },
		"negative count":             func(r *Request) { r.Video.Frames.Count = -1 },
		"interval gives too many":    func(r *Request) { r.Video.Frames = Frames{EverySeconds: 0.25} },
		"negative interval":          func(r *Request) { r.Video.Frames = Frames{EverySeconds: -1} },
		"frame_edge above ceiling":   func(r *Request) { r.Video.FrameEdge = 5000 },
		"frame_edge too small":       func(r *Request) { r.Video.FrameEdge = 8 },
		"quality":                    func(r *Request) { r.Video.Quality = 0 },
		"remux is not built":         func(r *Request) { r.Video.Remux = true },
	}
	for name, change := range cases {
		req := good()
		req.Expect, req.Photo, req.Video = KindVideo, nil, goodVideo()
		change(&req)
		err := Validate(req, s3Config())
		var invalid *InvalidRequestError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: want an invalid_request error, got %v", name, err)
		}
	}
}

func TestValidateFileStore(t *testing.T) {
	cfg := config.Config{Store: config.StoreFile, FileRoot: "/tmp/x", MaxOutputEdge: 4096, MaxRenditions: 4}
	req := good()
	req.Source.VersionID = ""
	req.Source.Bucket, req.Output.Bucket = "anything", "anything"
	if err := Validate(req, cfg); err != nil {
		t.Fatalf("the file store has no versions and no bucket list: %v", err)
	}
	req.Source.Bucket = ".."
	if err := Validate(req, cfg); err == nil {
		t.Fatal("a bucket named .. passed")
	}
}

func TestDecode(t *testing.T) {
	ok := `{"v":1,"request_id":"a","source":{"bucket":"b","key":"k","version_id":"v"},
		"output":{"bucket":"b","prefix":"p/"},"expect":"photo",
		"photo":{"renditions":[{"name":"image","max_edge":3000,"quality":85}]},
		"video":{"max_seconds":16,"frames":{"count":5},"frame_edge":1280,"quality":85,"remux":false}}`
	req, err := Decode([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if req.Photo.Renditions[0].MaxEdge != 3000 || req.Video.Frames.Count != 5 {
		t.Fatalf("decoded %+v", req)
	}

	for name, raw := range map[string]string{
		"unknown field": strings.Replace(ok, `"expect"`, `"expcet":"x","expect"`, 1),
		"wrong type":    strings.Replace(ok, `"v":1`, `"v":"1"`, 1),
		"not JSON":      "{",
		"empty":         "",
		"two objects":   ok + ok,
	} {
		_, err := Decode([]byte(raw))
		var invalid *InvalidRequestError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: want an invalid_request error, got %v", name, err)
		}
	}
}

func FuzzDecodeAndValidate(f *testing.F) {
	f.Add([]byte(`{"v":1,"request_id":"a","source":{"bucket":"cdn","key":"uploads/a/source","version_id":"v"},"output":{"bucket":"cdn","prefix":"uploads/a/"},"expect":"photo","photo":{"renditions":[{"name":"image","max_edge":3000,"quality":85}]}}`))
	cfg := s3Config()
	f.Fuzz(func(t *testing.T, raw []byte) {
		req, err := Decode(raw)
		if err != nil {
			return
		}
		if Validate(req, cfg) != nil {
			return
		}
		// A request that passes stays inside the allow lists.
		if req.Source.Bucket != "cdn" || req.Output.Bucket != "cdn" ||
			!strings.HasPrefix(req.Source.Key, "uploads/") || !strings.HasPrefix(req.Output.Prefix, "uploads/") ||
			strings.Contains(req.Source.Key, "..") && strings.Contains("/"+req.Source.Key+"/", "/../") {
			t.Fatalf("a request outside the allow lists passed: %+v", req)
		}
		for _, r := range req.Photo.Renditions {
			if strings.ContainsAny(r.Name, "/.") || r.MaxEdge > cfg.MaxOutputEdge {
				t.Fatalf("a bad rendition passed: %+v", r)
			}
		}
	})
}
