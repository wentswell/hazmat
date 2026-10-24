//go:build integration

// Package local tests the release binary inside the Lambda runtime image,
// against an S3 API with versions. `make local-test` starts the containers.
package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	bucket    = "cdn"
	invokeURL = "http://127.0.0.1:19001/2015-03-31/functions/function/invocations"
)

var client = s3.New(s3.Options{
	Region:       "us-east-2",
	BaseEndpoint: aws.String("http://127.0.0.1:19000"),
	UsePathStyle: true,
	Credentials:  credentials.NewStaticCredentialsProvider("local-access-key", "local-secret-key", ""),
})

// setup waits for the two containers and makes the bucket with versions on,
// as the CDN bucket has.
func setup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket:                    aws.String(bucket),
			CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "us-east-2"},
		})
		var owned *types.BucketAlreadyOwnedByYou
		if err == nil || asErr(err, &owned) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the S3 container is not ready: %v", err)
		}
		time.Sleep(time.Second)
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
}

func asErr[T error](err error, target *T) bool {
	for err != nil {
		if e, ok := err.(T); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func put(t *testing.T, key string, body []byte) (versionID string) {
	t.Helper()
	out, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		// The client says what it wants. The worker must not believe it.
		ContentType: aws.String("image/jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.VersionId) == "" {
		t.Fatal("the bucket returned no version")
	}
	return aws.ToString(out.VersionId)
}

func get(t *testing.T, key, versionID string) []byte {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
	})
	if err != nil {
		t.Fatalf("get %s version %s: %v", key, versionID, err)
	}
	defer out.Body.Close()
	b, _ := io.ReadAll(out.Body)
	if ct := aws.ToString(out.ContentType); ct != "image/jpeg" {
		t.Errorf("%s has the content type %q", key, ct)
	}
	return b
}

type response struct {
	Status  string `json:"status"`
	Refusal *struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"refusal"`
	Source *struct {
		Bytes       int64  `json:"bytes"`
		SHA256      string `json:"sha256"`
		SniffedType string `json:"sniffed_type"`
	} `json:"source"`
	Worker struct {
		FFmpeg string `json:"ffmpeg"`
	} `json:"worker"`
	Media *struct {
		Kind, Container, Codec string
		Width, Height          int
		HadGPS                 bool    `json:"had_gps"`
		DurationS              float64 `json:"duration_s"`
		DeclaredDurationS      float64 `json:"declared_duration_s"`
		FPS                    float64
		Rotation               int
		HasAudio               bool `json:"has_audio"`
	} `json:"media"`
	Outputs []struct {
		Name, Key, SHA256 string
		VersionID         string `json:"version_id"`
		Bytes             int64
		Width, Height     int
		TimeS             *float64 `json:"time_s"`
		Luma, Sharpness   float64
	} `json:"outputs"`
	// A function error, as Lambda returns it.
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}

// video is the video part of each request. A test can change it.
var video = map[string]any{"max_seconds": 16, "frames": map[string]any{"count": 5}, "frame_edge": 1280, "quality": 85}

func invoke(t *testing.T, key, versionID, expect string) response {
	t.Helper()
	req := map[string]any{
		"v": 1, "request_id": "local-test",
		"source": map[string]string{"bucket": bucket, "key": key, "version_id": versionID},
		"output": map[string]string{"bucket": bucket, "prefix": "uploads/" + t.Name() + "/"},
		"expect": expect,
		"photo": map[string]any{"renditions": []map[string]any{
			{"name": "image", "max_edge": 3000, "quality": 85},
			{"name": "scan", "max_edge": 64, "quality": 85},
		}},
		"video": video,
	}
	body, _ := json.Marshal(req)
	var last error
	// The runtime container can take a moment to listen after it starts.
	for attempt := 0; attempt < 30; attempt++ {
		res, err := http.Post(invokeURL, "application/json", bytes.NewReader(body))
		if err != nil {
			last = err
			time.Sleep(time.Second)
			continue
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var out response
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("the worker returned %q: %v", raw, err)
		}
		return out
	}
	t.Fatalf("the worker container is not ready: %v", last)
	return response{}
}

// photo is a JPEG of w x h with EXIF orientation 6 and a GPS pointer.
func photo(t *testing.T, w, h int, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{shade, uint8(x), uint8(y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 2, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0,
		0x25, 0x88, 4, 0, 1, 0, 0, 0, 64, 0, 0, 0,
		0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, 0xE1}, uint16(len(payload)+2))
	seg = append(seg, payload...)
	j := buf.Bytes()
	return append(append(append([]byte{}, j[:2]...), seg...), j[2:]...)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestPhotoThroughLambdaAndS3(t *testing.T) {
	setup(t)
	src := photo(t, 200, 100, 10)
	key := "uploads/" + t.Name() + "/source"
	version := put(t, key, src)

	resp := invoke(t, key, version, "photo")
	if resp.Status != "ok" {
		t.Fatalf("response: %+v", resp)
	}
	if resp.Source.SHA256 != sum(src) || resp.Source.Bytes != int64(len(src)) || resp.Source.SniffedType != "image/jpeg" {
		t.Errorf("source facts: %+v", resp.Source)
	}
	// Orientation 6 is a quarter turn: 200x100 becomes 100x200.
	if resp.Media.Width != 100 || resp.Media.Height != 200 || !resp.Media.HadGPS {
		t.Errorf("media facts: %+v", resp.Media)
	}
	if len(resp.Outputs) != 2 {
		t.Fatalf("outputs: %+v", resp.Outputs)
	}
	for _, o := range resp.Outputs {
		if o.VersionID == "" {
			t.Fatalf("%s has no version_id", o.Name)
		}
		// Read the object by the version in the response. It is the bytes
		// the response describes, and it holds nothing of the EXIF block.
		stored := get(t, o.Key, o.VersionID)
		if sum(stored) != o.SHA256 || int64(len(stored)) != o.Bytes {
			t.Errorf("%s: the facts do not match the stored object", o.Name)
		}
		if bytes.Contains(stored, []byte("Exif")) {
			t.Errorf("%s holds EXIF data", o.Name)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(stored))
		if err != nil || cfg.Width != o.Width || cfg.Height != o.Height {
			t.Errorf("%s: %v %+v", o.Name, err, cfg)
		}
	}
	if o := resp.Outputs[1]; o.Name != "scan" || o.Width != 32 || o.Height != 64 {
		t.Errorf("scan: %+v", o)
	}
}

// A second upload to the same key must not change what one request reads.
func TestTheWorkerReadsTheExactVersion(t *testing.T) {
	setup(t)
	key := "uploads/" + t.Name() + "/source"
	first := photo(t, 120, 80, 10)
	firstVersion := put(t, key, first)
	second := photo(t, 300, 300, 200)
	put(t, key, second)

	resp := invoke(t, key, firstVersion, "photo")
	if resp.Status != "ok" || resp.Source.SHA256 != sum(first) || resp.Media.Width != 80 {
		t.Fatalf("the worker did not read the first version: %+v %+v", resp.Source, resp.Media)
	}
}

func TestRefusalsThroughLambda(t *testing.T) {
	setup(t)
	cases := []struct {
		name, expect, code string
		body               []byte
	}{
		{"text", "photo", "unsupported_type", []byte("not a photo, whatever the content type says")},
		{"heic", "photo", "unsupported_type", append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}, make([]byte, 64)...)},
		{"video", "photo", "wrong_kind", append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, make([]byte, 64)...)},
		{"large", "photo", "too_many_bytes", append(photo(t, 20, 20, 1), make([]byte, 1<<20)...)},
		{"half", "photo", "undecodable", photo(t, 200, 100, 10)[:200]},
	}
	for _, c := range cases {
		key := fmt.Sprintf("uploads/%s/%s/source", t.Name(), c.name)
		resp := invoke(t, key, put(t, key, c.body), c.expect)
		if resp.Status != "refused" || resp.Refusal == nil || resp.Refusal.Code != c.code {
			t.Errorf("%s: want a refusal %s, got %+v (error %s %s)", c.name, c.code, resp.Refusal, resp.ErrorType, resp.ErrorMessage)
		}
		if len(resp.Outputs) != 0 {
			t.Errorf("%s: a refusal has outputs", c.name)
		}
	}
}

func TestErrorsThroughLambda(t *testing.T) {
	setup(t)
	// A key outside the allowed prefixes is a bug of the caller.
	resp := invoke(t, "events/x/cover.jpg", "v1", "photo")
	if resp.ErrorType != "InvalidRequestError" || resp.Status != "" {
		t.Errorf("a key outside the prefixes: %+v", resp)
	}
	// A version that does not exist is an error, not a refusal of a file.
	key := "uploads/" + t.Name() + "/source"
	put(t, key, photo(t, 20, 20, 1))
	resp = invoke(t, key, "00000000-0000-0000-0000-000000000000", "photo")
	if resp.ErrorType != "StorageError" || resp.Status != "" {
		t.Errorf("a missing version: %+v", resp)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func invokeVideo(t *testing.T, name string, data []byte) response {
	t.Helper()
	key := fmt.Sprintf("uploads/%s/%s/source", t.Name(), name)
	return invoke(t, key, put(t, key, data), "video")
}

func TestVideoThroughLambdaAndS3(t *testing.T) {
	setup(t)
	resp := invokeVideo(t, "good", fixture(t, "good.mp4"))
	if resp.Status != "ok" {
		t.Fatalf("response: %+v", resp)
	}
	if resp.Worker.FFmpeg == "" {
		t.Error("the response does not name the ffmpeg version")
	}
	m := resp.Media
	if m.Kind != "video" || m.Container != "mp4" || m.Codec != "h264" || m.Width != 320 || m.Height != 240 ||
		math.Abs(m.DurationS-4) > 0.05 || math.Abs(m.DeclaredDurationS-4) > 0.1 || m.FPS != 30 || m.Rotation != 0 || !m.HasAudio {
		t.Errorf("media facts: %+v", m)
	}
	// Five frames of 4 s: one at the middle of each 0.8 s.
	if len(resp.Outputs) != 5 {
		t.Fatalf("outputs: %+v", resp.Outputs)
	}
	for i, o := range resp.Outputs {
		want := 0.4 + 0.8*float64(i)
		if o.Name != fmt.Sprintf("frame_%02d", i+1) || o.TimeS == nil || math.Abs(*o.TimeS-want) > 0.04 {
			t.Errorf("frame %d: name %s time %v, want about %.2f", i+1, o.Name, o.TimeS, want)
		}
		if o.Width != 320 || o.Height != 240 || o.Luma < 20 || o.Luma > 235 || o.Sharpness < 10 {
			t.Errorf("frame %d: %dx%d luma %v sharpness %v", i+1, o.Width, o.Height, o.Luma, o.Sharpness)
		}
		stored := get(t, o.Key, o.VersionID)
		if sum(stored) != o.SHA256 {
			t.Errorf("frame %d: the facts do not match the stored object", i+1)
		}
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(stored)); err != nil || cfg.Width != 320 || cfg.Height != 240 {
			t.Errorf("frame %d is not a 320x240 JPEG: %v", i+1, err)
		}
	}
	// The frames are different pictures: the test video moves.
	if resp.Outputs[0].SHA256 == resp.Outputs[4].SHA256 {
		t.Error("the first and the last frame are the same picture")
	}
}

func TestVideoSamplingAndShape(t *testing.T) {
	setup(t)
	defer func(old any) { video["frames"] = old }(video["frames"])

	// One frame for each second of a 4 s video.
	video["frames"] = map[string]any{"every_seconds": 1}
	resp := invokeVideo(t, "every", fixture(t, "good.mp4"))
	if resp.Status != "ok" || len(resp.Outputs) != 4 || math.Abs(*resp.Outputs[3].TimeS-3.5) > 0.04 {
		t.Errorf("each second: %s, %d frames, %+v", resp.Status, len(resp.Outputs), resp.Refusal)
	}
	video["frames"] = map[string]any{"count": 5}

	// A video stored on its side comes out as a player shows it.
	resp = invokeVideo(t, "rotated", fixture(t, "rotated.mp4"))
	if resp.Status != "ok" || resp.Media.Width != 240 || resp.Media.Height != 320 || resp.Media.Rotation == 0 {
		t.Fatalf("rotated: %s %+v %+v", resp.Status, resp.Media, resp.Refusal)
	}
	if o := resp.Outputs[0]; o.Width != 240 || o.Height != 320 {
		t.Errorf("a frame of the rotated video is %dx%d", o.Width, o.Height)
	}

	// A file with one keyframe gives every frame, not only the first.
	resp = invokeVideo(t, "onekey", fixture(t, "onekey.mp4"))
	if resp.Status != "ok" || len(resp.Outputs) != 5 || resp.Outputs[0].SHA256 == resp.Outputs[4].SHA256 {
		t.Errorf("one keyframe: %s, %d frames, %+v", resp.Status, len(resp.Outputs), resp.Refusal)
	}

	// 14 s is allowed.
	resp = invokeVideo(t, "fourteen", fixture(t, "fourteen.mp4"))
	if resp.Status != "ok" || math.Abs(resp.Media.DurationS-14) > 0.1 {
		t.Errorf("14 s: %s %+v %+v", resp.Status, resp.Media, resp.Refusal)
	}
}

func TestVideoRefusals(t *testing.T) {
	setup(t)
	good := fixture(t, "good.mp4")
	cases := []struct {
		name, code string
		data       []byte
	}{
		{"hevc", "unsupported_codec", fixture(t, "hevc.mp4")},
		{"quicktime", "unsupported_container", fixture(t, "quicktime.mov")},
		{"twovideo", "bad_stream_layout", fixture(t, "twovideo.mp4")},
		{"audioonly", "bad_stream_layout", fixture(t, "audioonly.mp4")},
		{"fast", "bad_stream_layout", fixture(t, "fast.mp4")},
		{"big", "too_many_pixels", fixture(t, "big.mp4")},
		{"twenty", "too_long", fixture(t, "twenty.mp4")},
		// The header says 10 s, and the file holds 20 s or 14 s.
		{"false-header-20", "too_long", declare(t, fixture(t, "twenty.mp4"), 10)},
		{"false-header-14", "duration_mismatch", declare(t, fixture(t, "fourteen.mp4"), 10)},
		// Half of a file with its index at the start: the header still says
		// 4 s, and the samples that are there give 2 s.
		{"half", "duration_mismatch", good[:len(good)/2]},
		// Half of a file with its index at the end: there is no index.
		{"half-no-index", "undecodable", fixture(t, "rotated.mp4")[:40000]},
		{"header-only", "undecodable", append(append([]byte{}, good[:32]...), make([]byte, 4096)...)},
		{"large", "too_many_bytes", append(append([]byte{}, good...), make([]byte, 2<<20)...)},
	}
	for _, c := range cases {
		resp := invokeVideo(t, c.name, c.data)
		if resp.Status != "refused" || resp.Refusal == nil || resp.Refusal.Code != c.code {
			t.Errorf("%s: want a refusal %s, got status %q refusal %+v (error %s %s)", c.name, c.code, resp.Status, resp.Refusal, resp.ErrorType, resp.ErrorMessage)
		}
		if len(resp.Outputs) != 0 {
			t.Errorf("%s: a refusal has outputs", c.name)
		}
	}

	// A photo request refuses the same good video.
	key := "uploads/" + t.Name() + "/as-photo/source"
	if resp := invoke(t, key, put(t, key, good), "photo"); resp.Refusal == nil || resp.Refusal.Code != "wrong_kind" {
		t.Errorf("a video for a photo request: %+v", resp)
	}
}

// declare changes each duration in the header of an MP4 file to the given
// seconds, and leaves the samples as they are. It is what a hostile file does
// to look shorter than it is.
func declare(t *testing.T, data []byte, seconds uint32) []byte {
	t.Helper()
	out := append([]byte{}, data...)
	changed := 0
	var walk func(start, end int, movieScale uint32) uint32
	walk = func(start, end int, movieScale uint32) uint32 {
		for p := start; p+8 <= end; {
			size := int(binary.BigEndian.Uint32(out[p:]))
			kind := string(out[p+4 : p+8])
			if size < 8 || p+size > end {
				break
			}
			body := p + 8
			switch kind {
			case "moov", "trak", "mdia":
				movieScale = walk(body, p+size, movieScale)
			case "mvhd", "mdhd": // version 0: flags, two times, the scale, the duration
				scale := binary.BigEndian.Uint32(out[body+12:])
				binary.BigEndian.PutUint32(out[body+16:], seconds*scale)
				if kind == "mvhd" {
					movieScale = scale
				}
				changed++
			case "tkhd": // version 0: flags, two times, the track, 4 reserved, the duration
				binary.BigEndian.PutUint32(out[body+20:], seconds*movieScale)
				changed++
			}
			p += size
		}
		return movieScale
	}
	walk(0, len(out), 0)
	if changed < 3 {
		t.Fatalf("declare changed %d boxes", changed)
	}
	return out
}
