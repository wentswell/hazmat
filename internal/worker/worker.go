// Package worker runs one request: read one upload, refuse it or write clean
// outputs, and return the facts. It makes no decision about the content.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/wentswell/hazmat/internal/config"
	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/photo"
	"github.com/wentswell/hazmat/internal/sniff"
	"github.com/wentswell/hazmat/internal/store"
	"github.com/wentswell/hazmat/internal/video"
)

type Worker struct {
	Config  config.Config
	Store   store.Store
	Version string
	Log     *slog.Logger

	// Tools is nil in a build with no ffmpeg. The worker then refuses every
	// video.
	Tools         *video.Tools
	FFmpegVersion string
	// Recycle ends the execution environment after the response is sent. It
	// is called as Config.Recycle says, and is nil where that means nothing
	// (the command line).
	Recycle func()
}

// Process has three results. A response with status ok: the outputs exist. A
// response with status refused: the file is the problem. An error: the request
// is wrong (*contract.InvalidRequestError), the storage failed
// (*contract.StorageError), or the worker itself failed (*contract.WorkerError).
func (w *Worker) Process(ctx context.Context, req contract.Request) (contract.Response, error) {
	began := time.Now()
	resp := contract.Response{
		V:      contract.Version,
		Worker: contract.WorkerInfo{Version: w.Version, FFmpeg: w.FFmpegVersion, ConfigHash: w.Config.Hash()},
	}

	var decoded bool
	err := w.process(ctx, req, &resp, &decoded)
	w.recycle(err, decoded)
	var refusal *contract.Refusal
	switch {
	case err == nil:
		resp.Status = contract.StatusOK
	case errors.As(err, &refusal):
		resp.Status, resp.Refusal, err = contract.StatusRefused, refusal, nil
	}
	w.log(req, resp, err, time.Since(began))
	if err != nil {
		return contract.Response{}, err
	}
	return resp, nil
}

// decoded is set when the file went to the video tools, whatever the result.
func (w *Worker) process(ctx context.Context, req contract.Request, resp *contract.Response, decoded *bool) error {
	if err := contract.Validate(req, w.Config); err != nil {
		return err
	}
	src := req.Source

	// Refuse on the size before the download. The kind is not known yet, so
	// this is the larger of the two ceilings; the ceiling of the kind applies
	// after the type is known.
	limit := w.Config.MaxPhotoBytes
	if w.Tools != nil {
		limit = max(limit, w.Config.MaxVideoBytes)
	}
	start := time.Now()
	size, err := w.Store.Size(ctx, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return &contract.StorageError{Err: err}
	}
	resp.Source = &contract.SourceFacts{Bytes: size}
	if size > limit {
		return contract.Refuse(contract.TooManyBytes, "the file has %d bytes, limit %d", size, limit)
	}

	body, err := w.Store.Get(ctx, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return &contract.StorageError{Err: err}
	}
	defer body.Close()
	// The size above is what storage said. Count the bytes again, and stop
	// one byte past the limit.
	var data bytes.Buffer
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(&data, hash), io.LimitReader(body, limit+1))
	if err != nil {
		return &contract.StorageError{Err: fmt.Errorf("read %s: %w", src.Key, err)}
	}
	resp.Timings.Fetch = time.Since(start).Milliseconds()
	if n > limit {
		return contract.Refuse(contract.TooManyBytes, "the file has more than %d bytes", limit)
	}
	resp.Source.Bytes = n
	resp.Source.SHA256 = hex.EncodeToString(hash.Sum(nil))

	t := sniff.Detect(data.Bytes())
	resp.Source.SniffedType = string(t)
	switch {
	case t == sniff.Unknown:
		return contract.Refuse(contract.UnsupportedType, "the file is not a photo or a video that this worker knows")
	case t.IsVideo() && req.Expect == contract.KindPhoto:
		return contract.Refuse(contract.WrongKind, "the file is a video (%s) and the request expects a photo", t.Short())
	case t.IsPhoto() && req.Expect == contract.KindVideo:
		return contract.Refuse(contract.WrongKind, "the file is a photo (%s) and the request expects a video", t.Short())
	case t.IsVideo() && w.Tools == nil:
		return contract.Refuse(contract.UnsupportedType, "the file is a video (%s) and this build has no video path", t.Short())
	case t.IsVideo() && n > w.Config.MaxVideoBytes:
		return contract.Refuse(contract.TooManyBytes, "the video has %d bytes, limit %d", n, w.Config.MaxVideoBytes)
	case t.IsPhoto() && n > w.Config.MaxPhotoBytes:
		return contract.Refuse(contract.TooManyBytes, "the photo has %d bytes, limit %d", n, w.Config.MaxPhotoBytes)
	case t.IsPhoto() && !allowed(w.Config.PhotoTypes, t.Short()):
		return contract.Refuse(contract.UnsupportedType, "%s photos are not accepted", t.Short())
	}

	var outputs []output
	if t.IsVideo() {
		*decoded = true
		outputs, err = w.video(ctx, req, resp, data.Bytes(), t)
	} else {
		outputs, err = w.photo(req, resp, data.Bytes(), t)
	}
	if err != nil {
		return err
	}

	start = time.Now()
	for _, out := range outputs {
		key := contract.OutputKey(req.Output.Prefix, out.name)
		versionID, err := w.Store.Put(ctx, req.Output.Bucket, key, out.data, "image/jpeg")
		if err != nil {
			return &contract.StorageError{Err: err}
		}
		sum := sha256.Sum256(out.data)
		resp.Outputs = append(resp.Outputs, contract.OutputFacts{
			Name: out.name, Key: key, VersionID: versionID,
			Bytes: int64(len(out.data)), SHA256: hex.EncodeToString(sum[:]),
			Width: out.width, Height: out.height,
			FrameFacts: out.frame,
		})
	}
	resp.Timings.Store = time.Since(start).Milliseconds()
	return nil
}

type output struct {
	name          string
	data          []byte
	width, height int
	frame         *contract.FrameFacts
}

func (w *Worker) photo(req contract.Request, resp *contract.Response, data []byte, t sniff.Type) ([]output, error) {
	res, err := photo.Process(data, t, photo.Options{
		Renditions: req.Photo.Renditions,
		MaxPixels:  w.Config.MaxPhotoPixels,
	})
	if err != nil {
		return nil, err
	}
	resp.Timings.Decode = res.Decode.Milliseconds()
	resp.Timings.Encode = res.Encode.Milliseconds()
	resp.Media = &contract.PhotoFacts{
		Kind:  contract.KindPhoto,
		Width: res.Width, Height: res.Height,
		HadMetadata: res.HadMetadata, HadGPS: res.HadGPS, Animated: res.Animated,
	}
	var outs []output
	for _, o := range res.Outputs {
		outs = append(outs, output{name: o.Name, data: o.Data, width: o.Width, height: o.Height})
	}
	return outs, nil
}

func (w *Worker) video(ctx context.Context, req contract.Request, resp *contract.Response, data []byte, t sniff.Type) ([]output, error) {
	// One new directory for each request, and nothing of it stays. The
	// source is a file, because ffmpeg reads a file; the frames go to a
	// second directory, which is all the child process writes.
	dir, err := os.MkdirTemp("", "hazmat-")
	if err != nil {
		return nil, contract.Fail("make the work directory: %w", err)
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source")
	frames := filepath.Join(dir, "frames")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		return nil, contract.Fail("write the source to the work directory: %w", err)
	}
	if err := os.Mkdir(frames, 0o700); err != nil {
		return nil, contract.Fail("make the frame directory: %w", err)
	}

	opts := req.Video
	res, err := video.Process(ctx, *w.Tools, source, frames, t, video.Limits{
		Containers:  w.Config.VideoContainers,
		Codecs:      w.Config.VideoCodecs,
		MaxEdge:     w.Config.MaxVideoEdge,
		MaxFPS:      float64(w.Config.MaxVideoFPS),
		MaxSeconds:  opts.MaxSeconds,
		Timeout:     time.Duration(w.Config.DecodeTimeoutSeconds * float64(time.Second)),
		MemoryBytes: w.Config.DecodeMemoryBytes,
	}, video.Options{
		Count:        opts.Frames.Count,
		EverySeconds: opts.Frames.EverySeconds,
		FrameEdge:    opts.FrameEdge,
		Quality:      opts.Quality,
	})
	if err != nil {
		return nil, err
	}
	resp.Timings.Probe = res.Probe.Milliseconds()
	resp.Timings.Decode = res.Decode.Milliseconds()
	resp.Media = &contract.VideoFacts{
		Kind:      contract.KindVideo,
		Container: res.Container, Codec: res.Codec,
		Width: res.Width, Height: res.Height,
		DurationS: round3(res.Duration), DeclaredDurationS: round3(res.DeclaredDuration),
		FPS: res.FPS, Rotation: res.Rotation, HasAudio: res.HasAudio,
	}
	var outs []output
	for _, f := range res.Frames {
		outs = append(outs, output{
			name: f.Name, data: f.Data, width: f.Width, height: f.Height,
			frame: &contract.FrameFacts{TimeS: f.TimeS, Luma: f.Luma, Sharpness: f.Sharpness},
		})
	}
	return outs, nil
}

// recycle ends the execution environment when the configuration says so. A
// child process that timed out or died on a signal is the sign of a hostile
// file; the next request must not run beside what that file left behind.
// "always" is after every video that went to the tools, refused or not: the
// refused ones are the likeliest to be hostile.
func (w *Worker) recycle(err error, wasVideo bool) {
	if w.Recycle == nil {
		return
	}
	var abnormal *video.Abnormal
	switch w.Config.Recycle {
	case config.RecycleAlways:
		if wasVideo || errors.As(err, &abnormal) {
			w.Recycle()
		}
	case config.RecycleOnError:
		if errors.As(err, &abnormal) {
			if w.Log != nil {
				w.Log.Warn("recycle", "reason", abnormal.Code)
			}
			w.Recycle()
		}
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// log writes the one line of a request. It holds facts about the file and
// never bytes of the file, EXIF content, or a position.
func (w *Worker) log(req contract.Request, resp contract.Response, err error, took time.Duration) {
	if w.Log == nil {
		return
	}
	attrs := []any{
		"request_id", req.RequestID,
		"source_key", req.Source.Key,
		"worker_version", w.Version,
		"config_hash", resp.Worker.ConfigHash,
		"duration_ms", took.Milliseconds(),
		"timings_ms", resp.Timings,
	}
	if resp.Source != nil {
		attrs = append(attrs, "source_bytes", resp.Source.Bytes, "sniffed_type", resp.Source.SniffedType)
	}
	switch m := resp.Media.(type) {
	case *contract.PhotoFacts:
		attrs = append(attrs, "kind", m.Kind, "width", m.Width, "height", m.Height)
	case *contract.VideoFacts:
		attrs = append(attrs, "kind", m.Kind, "width", m.Width, "height", m.Height,
			"duration_s", m.DurationS, "codec", m.Codec, "fps", m.FPS)
	}
	switch {
	case err != nil:
		var invalid *contract.InvalidRequestError
		var storage *contract.StorageError
		var failed *contract.WorkerError
		result := "error"
		switch {
		case errors.As(err, &invalid):
			result = "invalid_request"
		case errors.As(err, &storage):
			result = "storage_error"
		case errors.As(err, &failed):
			result = "worker_error"
		}
		w.Log.Error("request", append(attrs, "result", result, "error", err.Error())...)
	case resp.Refusal != nil:
		w.Log.Warn("request", append(attrs, "result", "refused", "refusal_code", resp.Refusal.Code, "refusal_detail", resp.Refusal.Detail)...)
	default:
		w.Log.Info("request", append(attrs, "result", "ok", "outputs", len(resp.Outputs))...)
	}
}

func allowed(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
