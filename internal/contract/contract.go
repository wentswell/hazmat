// Package contract is the request and the response of the worker, and the
// rules a request must pass before the worker touches storage.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/wentswell/hazmat/internal/config"
)

// Version is the contract version. A change that removes or renames a field
// increases it, and the worker refuses a version it does not know.
const Version = 1

type Kind string

const (
	KindPhoto Kind = "photo"
	KindVideo Kind = "video"
	KindAny   Kind = "any"
)

type Request struct {
	V         int           `json:"v"`
	RequestID string        `json:"request_id"`
	Source    Source        `json:"source"`
	Output    Output        `json:"output"`
	Expect    Kind          `json:"expect"`
	Photo     *PhotoOptions `json:"photo,omitempty"`
	Video     *VideoOptions `json:"video,omitempty"`
}

type Source struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"version_id,omitempty"`
}

type Output struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

type PhotoOptions struct {
	Renditions []Rendition `json:"renditions"`
}

// Rendition is one JPEG the worker writes for a photo, at prefix + name + ".jpg".
type Rendition struct {
	Name    string `json:"name"`
	MaxEdge int    `json:"max_edge"`
	Quality int    `json:"quality"`
}

// VideoOptions says how a video is sampled. The frames are written at
// prefix + "frame_NN.jpg".
type VideoOptions struct {
	// MaxSeconds is the longest video the caller accepts. The operator
	// ceiling is the longest it can ask for.
	MaxSeconds float64 `json:"max_seconds"`
	Frames     Frames  `json:"frames"`
	FrameEdge  int     `json:"frame_edge"`
	Quality    int     `json:"quality"`
	// Remux is reserved. This build refuses a request that sets it.
	Remux bool `json:"remux,omitempty"`
}

// Frames is a count of evenly spaced frames, or one frame for each interval.
// Exactly one of the two is set.
type Frames struct {
	Count        int     `json:"count,omitempty"`
	EverySeconds float64 `json:"every_seconds,omitempty"`
}

type Response struct {
	V       int          `json:"v"`
	Status  Status       `json:"status"`
	Worker  WorkerInfo   `json:"worker"`
	Source  *SourceFacts `json:"source,omitempty"`
	Refusal *Refusal     `json:"refusal,omitempty"`
	// Media is a *PhotoFacts or a *VideoFacts.
	Media   any           `json:"media,omitempty"`
	Outputs []OutputFacts `json:"outputs,omitempty"`
	Timings Timings       `json:"timings_ms"`
}

type Status string

const (
	StatusOK      Status = "ok"
	StatusRefused Status = "refused"
)

type WorkerInfo struct {
	Version string `json:"version"`
	// FFmpeg is the version of the decoder. It is empty in a build with no
	// video path.
	FFmpeg     string `json:"ffmpeg,omitempty"`
	ConfigHash string `json:"config_hash"`
}

type SourceFacts struct {
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256,omitempty"`
	SniffedType string `json:"sniffed_type,omitempty"`
}

type PhotoFacts struct {
	Kind        Kind `json:"kind"`
	Width       int  `json:"width"`
	Height      int  `json:"height"`
	HadMetadata bool `json:"had_metadata"`
	HadGPS      bool `json:"had_gps"`
	Animated    bool `json:"animated"`
}

type VideoFacts struct {
	Kind      Kind   `json:"kind"`
	Container string `json:"container"`
	Codec     string `json:"codec"`
	// Width and Height are of the video as a player shows it.
	Width  int `json:"width"`
	Height int `json:"height"`
	// DurationS is measured from the packets. DeclaredDurationS is what the
	// header says. The worker refuses a file where they differ.
	DurationS         float64 `json:"duration_s"`
	DeclaredDurationS float64 `json:"declared_duration_s"`
	FPS               float64 `json:"fps"`
	Rotation          int     `json:"rotation"`
	// HasAudio is a fact for the caller. The worker does not read the audio.
	HasAudio bool `json:"has_audio"`
}

type OutputFacts struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	VersionID string `json:"version_id,omitempty"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	// Frame is set for a frame of a video.
	*FrameFacts
}

type FrameFacts struct {
	TimeS float64 `json:"time_s"`
	// Luma is the mean brightness, 0 to 255. Sharpness is near 0 for a flat
	// or blurred frame. Both are for the choice of a poster by the caller.
	Luma      float64 `json:"luma"`
	Sharpness float64 `json:"sharpness"`
}

type Timings struct {
	Fetch int64 `json:"fetch"`
	// Probe is the read of the header and the packet times of a video.
	Probe  int64 `json:"probe,omitempty"`
	Decode int64 `json:"decode"`
	Encode int64 `json:"encode"`
	Store  int64 `json:"store"`
}

type RefusalCode string

const (
	UnsupportedType      RefusalCode = "unsupported_type"
	WrongKind            RefusalCode = "wrong_kind"
	TooManyBytes         RefusalCode = "too_many_bytes"
	TooManyPixels        RefusalCode = "too_many_pixels"
	Undecodable          RefusalCode = "undecodable"
	UnsupportedContainer RefusalCode = "unsupported_container"
	UnsupportedCodec     RefusalCode = "unsupported_codec"
	BadStreamLayout      RefusalCode = "bad_stream_layout"
	TooLong              RefusalCode = "too_long"
	DurationMismatch     RefusalCode = "duration_mismatch"
	// DecodeTimeout is a refusal and not an error: the ceilings limit the
	// work of a correct file, so a file that passes them and still does not
	// finish is a hostile file. An error would make the caller try again
	// without end.
	DecodeTimeout RefusalCode = "decode_timeout"
)

// Refusal says the file is the problem. The same file always gets the same
// refusal, so the caller tells the user and does not try again. Code is for
// programs; Detail is for a person.
type Refusal struct {
	Code   RefusalCode `json:"code"`
	Detail string      `json:"detail"`
}

func (r *Refusal) Error() string { return string(r.Code) + ": " + r.Detail }

func Refuse(code RefusalCode, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// InvalidRequestError says the caller is the problem: a bug, not a bad file.
// On Lambda the type name is the errorType of the function error.
type InvalidRequestError struct{ Reason string }

func (e *InvalidRequestError) Error() string { return "invalid_request: " + e.Reason }

// StorageError says the storage is the problem: the source is not there, or
// a read or a write failed. The caller can try again later. On Lambda the type
// name is the errorType of the function error.
type StorageError struct{ Err error }

func (e *StorageError) Error() string { return "storage: " + e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return &InvalidRequestError{Reason: fmt.Sprintf(format, args...)}
}

// Decode reads a request strictly: a field the worker does not know is an
// error, so a misspelled option cannot be ignored without a sign.
func Decode(raw []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, invalid("the request is not valid JSON for contract v%d: %v", Version, err)
	}
	if dec.More() {
		return Request{}, invalid("the request has data after the JSON object")
	}
	return req, nil
}

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	namePattern      = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

// Validate checks a request against the contract and the operator ceilings.
// A value above a ceiling is an error; the worker never lowers it and continues.
func Validate(req Request, cfg config.Config) error {
	if req.V != Version {
		return invalid("contract version %d is not supported; this worker speaks v%d", req.V, Version)
	}
	if !requestIDPattern.MatchString(req.RequestID) {
		return invalid("request_id must match %s", requestIDPattern)
	}

	if err := checkKey("source.key", req.Source.Key); err != nil {
		return err
	}
	if err := checkKey("output.prefix", req.Output.Prefix); err != nil {
		return err
	}
	if !strings.HasSuffix(req.Output.Prefix, "/") {
		return invalid("output.prefix must end with a slash")
	}
	if err := checkBucket("source.bucket", req.Source.Bucket); err != nil {
		return err
	}
	if err := checkBucket("output.bucket", req.Output.Bucket); err != nil {
		return err
	}

	if cfg.Store == config.StoreS3 {
		// The worker never reads "the latest": a second upload to the same key
		// must not change what one request sees.
		if req.Source.VersionID == "" {
			return invalid("source.version_id is mandatory")
		}
		if !contains(cfg.AllowedBuckets, req.Source.Bucket) {
			return invalid("source.bucket %q is not allowed", req.Source.Bucket)
		}
		if !contains(cfg.AllowedBuckets, req.Output.Bucket) {
			return invalid("output.bucket %q is not allowed", req.Output.Bucket)
		}
	}
	if len(cfg.InputPrefixes) > 0 && !hasAnyPrefix(req.Source.Key, cfg.InputPrefixes) {
		return invalid("source.key %q is outside the allowed input prefixes", req.Source.Key)
	}
	if len(cfg.OutputPrefixes) > 0 && !hasAnyPrefix(req.Output.Prefix, cfg.OutputPrefixes) {
		return invalid("output.prefix %q is outside the allowed output prefixes", req.Output.Prefix)
	}

	switch req.Expect {
	case KindPhoto, KindVideo, KindAny:
	default:
		return invalid("expect must be photo, video or any, got %q", req.Expect)
	}
	if req.Photo == nil && req.Expect != KindVideo {
		return invalid("photo options are mandatory when expect is %q", req.Expect)
	}
	if req.Video == nil && req.Expect != KindPhoto {
		return invalid("video options are mandatory when expect is %q", req.Expect)
	}
	if req.Photo != nil {
		if err := checkRenditions(req, cfg); err != nil {
			return err
		}
	}
	if req.Video != nil {
		if err := checkVideo(*req.Video, cfg); err != nil {
			return err
		}
	}
	return nil
}

func checkVideo(v VideoOptions, cfg config.Config) error {
	if !(v.MaxSeconds > 0) || v.MaxSeconds > cfg.MaxVideoSeconds {
		return invalid("video.max_seconds %v is outside 0..%v", v.MaxSeconds, cfg.MaxVideoSeconds)
	}
	f := v.Frames
	switch {
	case f.Count != 0 && f.EverySeconds != 0:
		return invalid("video.frames has a count and an interval; set one")
	case f.Count != 0:
		if f.Count < 1 || f.Count > cfg.MaxFrames {
			return invalid("video.frames.count %d is outside 1..%d", f.Count, cfg.MaxFrames)
		}
	case f.EverySeconds != 0:
		// The longest video the request accepts must not need more frames
		// than the ceiling. The half second is the slack on a duration.
		if !(f.EverySeconds > 0) || math.Ceil((v.MaxSeconds+0.5)/f.EverySeconds) > float64(cfg.MaxFrames) {
			return invalid("video.frames.every_seconds %v gives more than %d frames for %v s", f.EverySeconds, cfg.MaxFrames, v.MaxSeconds)
		}
	default:
		return invalid("video.frames needs a count or an interval")
	}
	if v.FrameEdge < 16 || v.FrameEdge > cfg.MaxOutputEdge {
		return invalid("video.frame_edge %d is outside 16..%d", v.FrameEdge, cfg.MaxOutputEdge)
	}
	if v.Quality < 1 || v.Quality > 100 {
		return invalid("video.quality %d is outside 1..100", v.Quality)
	}
	if v.Remux {
		return invalid("video.remux is not in this build")
	}
	return nil
}

// FrameName is the output name of frame i, counted from 1.
func FrameName(i int) string { return fmt.Sprintf("frame_%02d", i) }

func checkRenditions(req Request, cfg config.Config) error {
	rs := req.Photo.Renditions
	if len(rs) == 0 {
		return invalid("photo.renditions is empty")
	}
	if len(rs) > cfg.MaxRenditions {
		return invalid("photo.renditions has %d entries; the ceiling is %d", len(rs), cfg.MaxRenditions)
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if !namePattern.MatchString(r.Name) {
			return invalid("rendition name %q must match %s", r.Name, namePattern)
		}
		// Reserved for the outputs of the video path.
		if r.Name == "video" || strings.HasPrefix(r.Name, "frame_") {
			return invalid("rendition name %q is reserved", r.Name)
		}
		if seen[r.Name] {
			return invalid("rendition name %q is used twice", r.Name)
		}
		seen[r.Name] = true
		if r.MaxEdge < 16 || r.MaxEdge > cfg.MaxOutputEdge {
			return invalid("rendition %q: max_edge %d is outside 16..%d", r.Name, r.MaxEdge, cfg.MaxOutputEdge)
		}
		if r.Quality < 1 || r.Quality > 100 {
			return invalid("rendition %q: quality %d is outside 1..100", r.Name, r.Quality)
		}
		if req.Output.Bucket == req.Source.Bucket && OutputKey(req.Output.Prefix, r.Name) == req.Source.Key {
			return invalid("rendition %q would overwrite the source", r.Name)
		}
	}
	return nil
}

// OutputKey is where a rendition is written.
func OutputKey(prefix, name string) string { return prefix + name + ".jpg" }

func checkBucket(field, v string) error {
	if v == "" || len(v) > 255 || strings.ContainsAny(v, "/\\\x00") || v == "." || v == ".." {
		return invalid("%s %q is not a bucket name", field, v)
	}
	return nil
}

func checkKey(field, v string) error {
	if v == "" || len(v) > 1024 {
		return invalid("%s must have 1 to 1024 characters", field)
	}
	if strings.HasPrefix(v, "/") || strings.Contains(v, "\\") {
		return invalid("%s %q must be a relative key with forward slashes", field, v)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return invalid("%s has a control character", field)
		}
	}
	for _, part := range strings.Split(strings.TrimSuffix(v, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return invalid("%s %q has an empty or a dot path segment", field, v)
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func hasAnyPrefix(v string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}
