package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var s3Env = map[string]string{
	"HAZMAT_ALLOWED_BUCKETS": "cdn.dev.example, cdn.example",
	"HAZMAT_INPUT_PREFIXES":  "uploads/",
	"HAZMAT_OUTPUT_PREFIXES": "uploads/",
}

func with(extra map[string]string) func(string) string {
	m := map[string]string{}
	for k, v := range s3Env {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(s3Env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Store != StoreS3 || len(c.AllowedBuckets) != 2 || c.AllowedBuckets[1] != "cdn.example" {
		t.Errorf("store and buckets: %+v", c)
	}
	if c.MaxPhotoBytes != 15<<20 || c.MaxPhotoPixels != 50_000_000 || c.MaxOutputEdge != 4096 || c.MaxRenditions != 4 {
		t.Errorf("ceilings: %+v", c)
	}
	if c.S3PathStyle {
		t.Error("path style is on by default")
	}
	if on, err := Load(with(map[string]string{"HAZMAT_S3_PATH_STYLE": "true"})); err != nil || !on.S3PathStyle {
		t.Errorf("HAZMAT_S3_PATH_STYLE=true: %+v %v", on, err)
	}
	if strings.Join(c.PhotoTypes, ",") != "jpeg,png,webp" || c.LogLevel != "info" {
		t.Errorf("types and level: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load(with(map[string]string{
		"HAZMAT_MAX_PHOTO_BYTES":  "10MiB",
		"HAZMAT_MAX_PHOTO_PIXELS": "12000000",
		"HAZMAT_MAX_OUTPUT_EDGE":  "3000",
		"HAZMAT_PHOTO_TYPES":      "jpeg",
		"HAZMAT_LOG_LEVEL":        "DEBUG",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPhotoBytes != 10<<20 || c.MaxPhotoPixels != 12_000_000 || c.MaxOutputEdge != 3000 ||
		len(c.PhotoTypes) != 1 || c.LogLevel != "debug" {
		t.Errorf("%+v", c)
	}
	if c, _ := Load(with(map[string]string{"HAZMAT_MAX_PHOTO_BYTES": "4096"})); c.MaxPhotoBytes != 4096 {
		t.Errorf("a plain byte count: %d", c.MaxPhotoBytes)
	}
}

// A value the worker cannot read stops the start. It never becomes a default.
func TestBadValuesStopTheStart(t *testing.T) {
	bad := []map[string]string{
		{"HAZMAT_MAX_PHOTO_BYTES": "ten"},
		{"HAZMAT_MAX_PHOTO_BYTES": "-1"},
		{"HAZMAT_MAX_PHOTO_BYTES": "10MB"},
		{"HAZMAT_MAX_PHOTO_PIXELS": "0"},
		{"HAZMAT_MAX_OUTPUT_EDGE": "4k"},
		{"HAZMAT_MAX_OUTPUT_EDGE": "65536"}, // a JPEG edge is 16 bits
		{"HAZMAT_MAX_RENDITIONS": "1.5"},
		{"HAZMAT_PHOTO_TYPES": "jpeg,heic"},
		{"HAZMAT_LOG_LEVEL": "loud"},
		{"HAZMAT_STORE": "gcs"},
		{"HAZMAT_S3_PATH_STYLE": "yes"},
		{"HAZMAT_STORE": "file"},
	}
	for _, extra := range bad {
		if _, err := Load(with(extra)); err == nil {
			t.Errorf("%v was accepted", extra)
		}
	}
	for _, missing := range []string{"HAZMAT_ALLOWED_BUCKETS", "HAZMAT_INPUT_PREFIXES", "HAZMAT_OUTPUT_PREFIXES"} {
		if _, err := Load(with(map[string]string{missing: ""})); err == nil {
			t.Errorf("the s3 store started with no %s", missing)
		}
	}
}

func TestVideoCeilings(t *testing.T) {
	c, err := Load(env(s3Env))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.VideoContainers, ",") != "mp4,mov" || strings.Join(c.VideoCodecs, ",") != "h264" ||
		c.MaxVideoBytes != 50<<20 || c.MaxVideoEdge != 1920 || c.MaxVideoFPS != 60 || c.MaxVideoSeconds != 60 ||
		c.MaxFrames != 32 || c.DecodeTimeoutSeconds != 20 || c.DecodeMemoryBytes != 1<<30 || c.Recycle != RecycleOnError {
		t.Errorf("defaults: %+v", c)
	}
	c, err = Load(with(map[string]string{
		"HAZMAT_VIDEO_CONTAINERS": "mp4", "HAZMAT_MAX_VIDEO_BYTES": "20MiB", "HAZMAT_MAX_VIDEO_EDGE": "1280",
		"HAZMAT_MAX_VIDEO_FPS": "30", "HAZMAT_MAX_VIDEO_SECONDS": "16", "HAZMAT_MAX_FRAMES": "16",
		"HAZMAT_DECODE_TIMEOUT_SECONDS": "10", "HAZMAT_DECODE_MEMORY": "512MiB", "HAZMAT_RECYCLE": "always", "HAZMAT_FFMPEG_DIR": "/opt/ff",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.VideoContainers) != 1 || c.MaxVideoBytes != 20<<20 || c.MaxVideoEdge != 1280 || c.MaxVideoFPS != 30 ||
		c.MaxVideoSeconds != 16 || c.MaxFrames != 16 || c.DecodeTimeoutSeconds != 10 || c.DecodeMemoryBytes != 512<<20 ||
		c.Recycle != RecycleAlways || c.FFmpegDir != "/opt/ff" {
		t.Errorf("overrides: %+v", c)
	}
	for _, bad := range []map[string]string{
		{"HAZMAT_VIDEO_CODECS": "h264,hevc"}, // the ffmpeg of the release has no HEVC decoder
		{"HAZMAT_VIDEO_CONTAINERS": "mkv"},
		{"HAZMAT_RECYCLE": "sometimes"},
		{"HAZMAT_MAX_VIDEO_SECONDS": "16.5"},
		{"HAZMAT_MAX_FRAMES": "0"},
		{"HAZMAT_DECODE_MEMORY": "big"},
	} {
		if _, err := Load(with(bad)); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// The largest edge a JPEG can store is the largest a request can ask for.
func TestOutputEdgeCeiling(t *testing.T) {
	c, err := Load(with(map[string]string{"HAZMAT_MAX_OUTPUT_EDGE": "65535"}))
	if err != nil || c.MaxOutputEdge != 65535 {
		t.Errorf("65535: %d %v", c.MaxOutputEdge, err)
	}
}

func TestFileStore(t *testing.T) {
	c, err := Load(env(map[string]string{"HAZMAT_STORE": "file", "HAZMAT_FILE_ROOT": "/tmp/x"}))
	if err != nil || c.Store != StoreFile || c.FileRoot != "/tmp/x" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestHash(t *testing.T) {
	a, _ := Load(env(s3Env))
	b, _ := Load(env(s3Env))
	c, _ := Load(with(map[string]string{"HAZMAT_MAX_OUTPUT_EDGE": "3000"}))
	if a.Hash() != b.Hash() || len(a.Hash()) != 16 {
		t.Errorf("the hash is not stable: %s %s", a.Hash(), b.Hash())
	}
	if a.Hash() == c.Hash() {
		t.Error("a different ceiling gave the same hash")
	}
}
