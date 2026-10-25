// Package config holds the operator ceilings. They come from HAZMAT_* environment
// variables, are read once at start, and never change while the worker runs.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	StoreS3   = "s3"
	StoreFile = "file"

	RecycleNever   = "never"
	RecycleOnError = "on_error"
	RecycleAlways  = "always"

	maxJPEGEdge = 65535
)

// Config is what the operator allows. A request can ask for less than a
// ceiling; a request that asks for more is refused as invalid_request.
type Config struct {
	Store    string `json:"store"`
	FileRoot string `json:"file_root,omitempty"`
	// S3PathStyle is for an S3-compatible store that does not answer on
	// bucket.host names. AWS S3 does not need it.
	S3PathStyle bool `json:"s3_path_style,omitempty"`

	AllowedBuckets []string `json:"allowed_buckets"`
	InputPrefixes  []string `json:"input_prefixes"`
	OutputPrefixes []string `json:"output_prefixes"`

	PhotoTypes     []string `json:"photo_types"`
	MaxPhotoBytes  int64    `json:"max_photo_bytes"`
	MaxPhotoPixels int64    `json:"max_photo_pixels"`
	MaxOutputEdge  int      `json:"max_output_edge"`
	MaxRenditions  int      `json:"max_renditions"`

	// FFmpegDir holds ffmpeg and ffprobe. Empty means the directory "bin"
	// beside the worker; a build with no ffmpeg there refuses every video.
	FFmpegDir       string   `json:"ffmpeg_dir,omitempty"`
	VideoContainers []string `json:"video_containers"`
	VideoCodecs     []string `json:"video_codecs"`
	MaxVideoBytes   int64    `json:"max_video_bytes"`
	MaxVideoEdge    int      `json:"max_video_edge"`
	MaxVideoFPS     int      `json:"max_video_fps"`
	MaxVideoSeconds float64  `json:"max_video_seconds"`
	MaxFrames       int      `json:"max_frames"`
	// DecodeTimeoutSeconds is the time limit of all child processes of one
	// video together. Keep it below the timeout of the function.
	// DecodeMemoryBytes is the address space of each child process.
	DecodeTimeoutSeconds float64 `json:"decode_timeout_seconds"`
	DecodeMemoryBytes    int64   `json:"decode_memory_bytes"`
	// Recycle says when the worker ends its execution environment after a
	// video: never, on_error (a child process did not end by itself), always.
	Recycle string `json:"recycle"`

	LogLevel string `json:"log_level"`
}

// Load reads the configuration through getenv (os.Getenv in production). A
// value that cannot be read is an error, so a typo stops the start instead of
// silently falling back to a default.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Store:          StoreS3,
		PhotoTypes:     []string{"jpeg", "png", "webp"},
		MaxPhotoBytes:  15 << 20,
		MaxPhotoPixels: 50_000_000,
		MaxOutputEdge:  4096,
		MaxRenditions:  4,
		LogLevel:       "info",

		VideoContainers:      []string{"mp4", "mov"},
		VideoCodecs:          []string{"h264"},
		MaxVideoBytes:        50 << 20,
		MaxVideoEdge:         1920,
		MaxVideoFPS:          60,
		MaxVideoSeconds:      60,
		MaxFrames:            32,
		DecodeTimeoutSeconds: 20,
		DecodeMemoryBytes:    1 << 30,
		Recycle:              RecycleOnError,
	}

	if v := getenv("HAZMAT_STORE"); v != "" {
		c.Store = v
	}
	c.FileRoot = getenv("HAZMAT_FILE_ROOT")
	switch v := getenv("HAZMAT_S3_PATH_STYLE"); v {
	case "", "false":
	case "true":
		c.S3PathStyle = true
	default:
		return Config{}, fmt.Errorf("HAZMAT_S3_PATH_STYLE must be true or false, got %q", v)
	}
	c.AllowedBuckets = list(getenv("HAZMAT_ALLOWED_BUCKETS"))
	c.InputPrefixes = list(getenv("HAZMAT_INPUT_PREFIXES"))
	c.OutputPrefixes = list(getenv("HAZMAT_OUTPUT_PREFIXES"))
	if v := list(getenv("HAZMAT_PHOTO_TYPES")); v != nil {
		c.PhotoTypes = v
	}
	if v := getenv("HAZMAT_LOG_LEVEL"); v != "" {
		c.LogLevel = strings.ToLower(v)
	}

	var err error
	if c.MaxPhotoBytes, err = size(getenv, "HAZMAT_MAX_PHOTO_BYTES", c.MaxPhotoBytes); err != nil {
		return Config{}, err
	}
	if c.MaxPhotoPixels, err = integer(getenv, "HAZMAT_MAX_PHOTO_PIXELS", c.MaxPhotoPixels); err != nil {
		return Config{}, err
	}
	edge, err := integer(getenv, "HAZMAT_MAX_OUTPUT_EDGE", int64(c.MaxOutputEdge))
	if err != nil {
		return Config{}, err
	}
	// A JPEG stores each edge in 16 bits, so a larger output cannot be
	// encoded, and the request would end in an error instead of a file.
	if edge > maxJPEGEdge {
		return Config{}, fmt.Errorf("HAZMAT_MAX_OUTPUT_EDGE must be at most %d, the largest JPEG edge, got %d", maxJPEGEdge, edge)
	}
	c.MaxOutputEdge = int(edge)
	renditions, err := integer(getenv, "HAZMAT_MAX_RENDITIONS", int64(c.MaxRenditions))
	if err != nil {
		return Config{}, err
	}
	c.MaxRenditions = int(renditions)

	c.FFmpegDir = getenv("HAZMAT_FFMPEG_DIR")
	if v := list(getenv("HAZMAT_VIDEO_CONTAINERS")); v != nil {
		c.VideoContainers = v
	}
	if v := list(getenv("HAZMAT_VIDEO_CODECS")); v != nil {
		c.VideoCodecs = v
	}
	if v := getenv("HAZMAT_RECYCLE"); v != "" {
		c.Recycle = v
	}
	if c.MaxVideoBytes, err = size(getenv, "HAZMAT_MAX_VIDEO_BYTES", c.MaxVideoBytes); err != nil {
		return Config{}, err
	}
	if c.DecodeMemoryBytes, err = size(getenv, "HAZMAT_DECODE_MEMORY", c.DecodeMemoryBytes); err != nil {
		return Config{}, err
	}
	for _, f := range []struct {
		name string
		dst  *int
	}{{"HAZMAT_MAX_VIDEO_EDGE", &c.MaxVideoEdge}, {"HAZMAT_MAX_VIDEO_FPS", &c.MaxVideoFPS}, {"HAZMAT_MAX_FRAMES", &c.MaxFrames}} {
		v, err := integer(getenv, f.name, int64(*f.dst))
		if err != nil {
			return Config{}, err
		}
		*f.dst = int(v)
	}
	for _, f := range []struct {
		name string
		dst  *float64
	}{{"HAZMAT_MAX_VIDEO_SECONDS", &c.MaxVideoSeconds}, {"HAZMAT_DECODE_TIMEOUT_SECONDS", &c.DecodeTimeoutSeconds}} {
		v, err := integer(getenv, f.name, int64(*f.dst))
		if err != nil {
			return Config{}, err
		}
		*f.dst = float64(v)
	}

	return c, c.check()
}

func (c Config) check() error {
	switch c.Store {
	case StoreS3:
		// The role is the real limit, but a worker with no allow list would
		// read and write wherever a caller points it.
		if len(c.AllowedBuckets) == 0 || len(c.InputPrefixes) == 0 || len(c.OutputPrefixes) == 0 {
			return fmt.Errorf("the s3 store needs HAZMAT_ALLOWED_BUCKETS, HAZMAT_INPUT_PREFIXES and HAZMAT_OUTPUT_PREFIXES")
		}
	case StoreFile:
		if c.FileRoot == "" {
			return fmt.Errorf("the file store needs HAZMAT_FILE_ROOT")
		}
	default:
		return fmt.Errorf("HAZMAT_STORE must be %q or %q, got %q", StoreS3, StoreFile, c.Store)
	}
	for _, t := range c.PhotoTypes {
		if t != "jpeg" && t != "png" && t != "webp" {
			return fmt.Errorf("HAZMAT_PHOTO_TYPES: %q is not a type this worker decodes (jpeg, png, webp)", t)
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("HAZMAT_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
	for _, t := range c.VideoContainers {
		if t != "mp4" && t != "mov" {
			return fmt.Errorf("HAZMAT_VIDEO_CONTAINERS: %q is not a container this worker reads (mp4, mov)", t)
		}
	}
	for _, t := range c.VideoCodecs {
		// The ffmpeg of the release has one video decoder.
		if t != "h264" {
			return fmt.Errorf("HAZMAT_VIDEO_CODECS: %q is not a codec this worker decodes (h264)", t)
		}
	}
	switch c.Recycle {
	case RecycleNever, RecycleOnError, RecycleAlways:
	default:
		return fmt.Errorf("HAZMAT_RECYCLE must be never, on_error or always, got %q", c.Recycle)
	}
	return nil
}

// Hash identifies this configuration in each response, so a stored result
// shows which limits applied to it.
func (c Config) Hash() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func list(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func integer(getenv func(string) string, name string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, v)
	}
	return n, nil
}

// size reads a byte count: a plain integer, or an integer with KiB, MiB or GiB.
func size(getenv func(string) string, name string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	mult := int64(1)
	for suffix, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if rest, ok := strings.CutSuffix(v, suffix); ok {
			v, mult = strings.TrimSpace(rest), m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("%s must be a positive byte count such as 15MiB, got %q", name, getenv(name))
	}
	return n * mult, nil
}
