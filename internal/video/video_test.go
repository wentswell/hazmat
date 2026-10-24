package video

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/sniff"
)

var lim = Limits{
	Containers: []string{"mp4"},
	Codecs:     []string{"h264"},
	MaxEdge:    1920,
	MaxFPS:     60,
	MaxSeconds: 16,
}

func wantRefusal(t *testing.T, name string, err error, code contract.RefusalCode) {
	t.Helper()
	var r *contract.Refusal
	if !errors.As(err, &r) {
		t.Errorf("%s: want a refusal %s, got %v", name, code, err)
		return
	}
	if r.Code != code {
		t.Errorf("%s: want %s, got %s (%s)", name, code, r.Code, r.Detail)
	}
}

// probeJSON is the output of ffprobe for an ordinary export of the app.
const probeJSON = `{"streams":[
  {"codec_name":"h264","codec_type":"video","width":1280,"height":720,"duration":"15.000000",
   "side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]},
  {"codec_name":"aac","codec_type":"audio","duration":"15.020000"},
  {"codec_type":"data"}],
  "format":{"duration":"15.020000"}}`

func TestParseProbe(t *testing.T) {
	in, err := parseProbe([]byte(probeJSON))
	if err != nil {
		t.Fatal(err)
	}
	if in.codec != "h264" || in.width != 1280 || in.height != 720 || in.rotation != -90 ||
		in.duration != 15.02 || in.videoDuration != 15 || in.videoStreams != 1 || !in.hasAudio {
		t.Errorf("%+v", in)
	}
	if err := in.check(lim); err != nil {
		t.Errorf("an ordinary file: %v", err)
	}
	_, err = parseProbe([]byte("not json"))
	wantRefusal(t, "no JSON", err, contract.Undecodable)
}

func TestHeaderChecks(t *testing.T) {
	base := info{codec: "h264", width: 1280, height: 720, duration: 15, videoDuration: 15, videoStreams: 1}
	cases := []struct {
		name   string
		change func(*info)
		code   contract.RefusalCode
	}{
		{"no video stream", func(i *info) { i.videoStreams = 0 }, contract.BadStreamLayout},
		{"two video streams", func(i *info) { i.videoStreams = 2 }, contract.BadStreamLayout},
		{"HEVC", func(i *info) { i.codec = "hevc" }, contract.UnsupportedCodec},
		{"no codec", func(i *info) { i.codec = "" }, contract.UnsupportedCodec},
		{"no dimensions", func(i *info) { i.width = 0 }, contract.Undecodable},
		{"4K", func(i *info) { i.width, i.height = 3840, 2160 }, contract.TooManyPixels},
		{"a tall 4K", func(i *info) { i.width, i.height = 1080, 3840 }, contract.TooManyPixels},
		{"a rotation of 45 degrees", func(i *info) { i.rotation = 45 }, contract.BadStreamLayout},
		{"no duration", func(i *info) { i.duration = math.NaN() }, contract.Undecodable},
		{"ten minutes", func(i *info) { i.duration = 600 }, contract.TooLong},
		{"a little above the limit", func(i *info) { i.duration = 16.6 }, contract.TooLong},
	}
	for _, c := range cases {
		in := base
		c.change(&in)
		wantRefusal(t, c.name, in.check(lim), c.code)
	}
	// The app allows a recording to run a little over 15 s.
	in := base
	in.duration = 16.4
	if err := in.check(lim); err != nil {
		t.Errorf("16.4 s with a limit of 16 s: %v", err)
	}
	// A codec name comes from the file. It must not carry text into a message.
	in = base
	in.codec = "h264\"; rm -rf / #" + strings.Repeat("x", 100)
	var r *contract.Refusal
	errors.As(in.check(lim), &r)
	if strings.ContainsAny(r.Detail, "\";/#") || len(r.Detail) > 60 {
		t.Errorf("the detail carries text of the file: %q", r.Detail)
	}
}

// packetLines makes the ffprobe packet output of a video at a constant rate.
func packetLines(seconds float64, fps int, start float64) []byte {
	var b strings.Builder
	n := int(seconds * float64(fps))
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.6f,%.6f\n", start+float64(i)/float64(fps), 1/float64(fps))
	}
	return []byte(b.String())
}

func TestPackets(t *testing.T) {
	p, err := parsePackets(packetLines(15, 30, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.count != 450 || p.first != 0 || math.Abs(p.duration()-15) > 1e-6 || p.fps() != 30 {
		t.Errorf("%+v duration %v fps %v", p, p.duration(), p.fps())
	}
	// Packets are in decode order, not in time order. B-frames give that.
	p, _ = parsePackets([]byte("0.000000,0.033333\n0.133333,0.033333\n0.066667,0.033333\n0.033333,0.033333,\n"))
	if p.count != 4 || p.last != 0.133333 || math.Abs(p.duration()-0.166666) > 1e-6 {
		t.Errorf("out of order: %+v", p)
	}

	_, err = parsePackets(nil)
	wantRefusal(t, "no packets", err, contract.Undecodable)
	_, err = parsePackets([]byte("N/A,0.033333\n"))
	wantRefusal(t, "a packet with no time", err, contract.Undecodable)
}

func TestMeasuredChecks(t *testing.T) {
	header := func(d float64) *info { return &info{duration: d, videoDuration: d} }
	measured := func(seconds float64, fps int) *packets {
		p, err := parsePackets(packetLines(seconds, fps, 0))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := measured(15, 30).check(header(15.02), lim); err != nil {
		t.Errorf("an ordinary file: %v", err)
	}
	// The header says 10 s, and the stream holds 60 s.
	wantRefusal(t, "a false header, too long", measured(60, 30).check(header(10), lim), contract.TooLong)
	// The header says 10 s, and the stream holds 14 s: both are allowed, and
	// the file is refused because they differ.
	wantRefusal(t, "a false header", measured(14, 30).check(header(10), lim), contract.DurationMismatch)
	// The file is 15 s of audio with 5 s of video.
	wantRefusal(t, "a short video stream", measured(5, 30).check(&info{duration: 15, videoDuration: 5}, lim), contract.DurationMismatch)
	wantRefusal(t, "a false stream duration", measured(15, 30).check(&info{duration: 15, videoDuration: 3}, lim), contract.DurationMismatch)
	wantRefusal(t, "240 frames for each second", measured(15, 240).check(header(15), lim), contract.BadStreamLayout)
	if err := measured(15, 60).check(header(15), lim); err != nil {
		t.Errorf("60 frames for each second: %v", err)
	}
	// A stream with no declared duration is judged on the file duration.
	if err := measured(15, 30).check(&info{duration: 15, videoDuration: math.NaN()}, lim); err != nil {
		t.Errorf("no stream duration: %v", err)
	}
}

func TestSampling(t *testing.T) {
	plan := func(seconds float64, fps int, start float64, opts Options) sampling {
		p, _ := parsePackets(packetLines(seconds, fps, start))
		return newSampling(p, opts)
	}
	cases := []struct {
		name            string
		s               sampling
		count           int
		interval, first float64 // first is the time of the first sample
	}{
		{"5 frames of 15 s", plan(15, 30, 0, Options{Count: 5}), 5, 3, 1.5},
		{"1 frame of 15 s", plan(15, 30, 0, Options{Count: 1}), 1, 15, 7.5},
		{"16 frames of 16 s", plan(16, 30, 0, Options{Count: 16}), 16, 1, 0.5},
		{"each second of 15 s", plan(15, 30, 0, Options{EverySeconds: 1}), 15, 1, 0.5},
		{"each second of 15.4 s", plan(15.4, 30, 0, Options{EverySeconds: 1}), 15, 1, 0.5},
		{"each 3 s of 2 s", plan(2, 30, 0, Options{EverySeconds: 3}), 1, 3, 1.5},
		{"each 10 s of 2 s: the first frame", plan(2, 30, 0, Options{EverySeconds: 10}), 1, 10, 0},
		{"a stream that starts at 2 s", plan(15, 30, 2, Options{Count: 5}), 5, 3, 3.5},
		{"more frames asked for than there are", plan(0.1, 30, 0, Options{Count: 5}), 3, 0.02, 0.01},
	}
	for _, c := range cases {
		got := c.s.first + c.s.offset
		if c.s.count != c.count || math.Abs(c.s.interval-c.interval) > 1e-6 || math.Abs(got-c.first) > 1e-6 {
			t.Errorf("%s: count %d interval %v first sample %v; want %d, %v, %v", c.name, c.s.count, c.s.interval, got, c.count, c.interval, c.first)
		}
	}
}

func TestFilter(t *testing.T) {
	s := sampling{first: 0, interval: 3, offset: 1.5, count: 5}
	got := s.filter(1280)
	want := `select=gte((t-1.499999)\,0)*(isnan(prev_selected_t)+gt(floor((t-1.499999)/3.000000)\,floor((prev_selected_t-1.499999)/3.000000))),` +
		`showinfo,` +
		`scale=w=min(1280\,iw):h=min(1280\,ih):force_original_aspect_ratio=decrease:force_divisible_by=2:out_range=full,` +
		`format=yuvj420p`
	if got != want {
		t.Errorf("filter:\n got %s\nwant %s", got, want)
	}
}

// fiftyPackets stands in for ffprobe on a file of fifty frames at ten a second.
var fiftyPackets = `case "$*" in
*packet=*) i=0; while [ $i -lt 50 ]; do printf '%d.%d00000,0.100000\n' $((i/10)) $((i%10)); i=$((i+1)); done ;;
*) echo '{"streams":[{"codec_name":"h264","codec_type":"video","width":64,"height":48,"duration":"5.1"}],"format":{"duration":"5.1"}}' ;;
esac`

// The decode pass keeps the timestamps of the file, so its clock is the one
// the packet listing measured and the plan was made in.
func TestFramesKeepTimestamps(t *testing.T) {
	dir := t.TempDir()
	lim := lim
	lim.Timeout = 5 * time.Second
	tools := Tools{FFprobe: script(t, fiftyPackets), FFmpeg: script(t, `printf '%s\n' "$@" > args.txt`)}
	_, _ = Process(context.Background(), tools, "source", dir, sniff.MP4, lim, Options{Count: 5, FrameEdge: 1280, Quality: 85})
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	list := strings.Split(string(args), "\n")
	if !slices.Contains(list, "-copyts") {
		t.Errorf("ffmpeg ran without -copyts:\n%s", args)
	}
	// The tags of the file are not printed again while the frames are.
	for _, opt := range []string{"-map_metadata", "-map_chapters"} {
		if i := slices.Index(list, opt); i < 0 || i+1 >= len(list) || list[i+1] != "-1" {
			t.Errorf("ffmpeg ran without %s -1:\n%s", opt, args)
		}
	}
}

func TestShowinfoTimes(t *testing.T) {
	stderr := `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'source':
[Parsed_showinfo_1 @ 0xffff8a8] config in time_base: 1/15360, frame_rate: 30/1
[Parsed_showinfo_1 @ 0xffff8a8] n:   0 pts:  23040 pts_time:1.5     duration:    512 duration_time:0.0333333 fmt:yuv420p
[swscaler @ 0xffff92] deprecated pixel format used, make sure you did set range correctly
[Parsed_showinfo_1 @ 0xffff8a8] n:   1 pts:  69120 pts_time:4.5     duration:    512
a line with pts_time:99 that is not of the filter
[Parsed_showinfo_1 @ 0xffff8a8] n:   2 pts: 115200 pts_time:7.533333 duration:    512
  Metadata:
    title           : [Parsed_showinfo_1 @ 0xffff8a8] n:   3 pts: 0 pts_time:8.5 forged by the file
    comment         : [Parsed_showinfo_1 @ 0xffff8a8] n:   4 pts: 0 pts_time:Inf
[Parsed_showinfo_1 @ 0xffff8a8] n:   5 pts: 0 pts_time:
[Parsed_showinfo_1 @ 0xffff8a8] n:   6 pts: 0 pts_time:NaN
[Parsed_showinfo_1 @ 0xffff8a8] n:   7 pts: 0 pts_time:-Inf
[Parsed_showinfo_1 @ 0xffff8a8] n:   8 pts: 0 pts_time`
	got := showinfoTimes([]byte(stderr))
	if len(got) != 3 || got[0] != 1.5 || got[1] != 4.5 || got[2] != 7.533333 {
		t.Errorf("%v", got)
	}
}

// Every frame has the time the filter printed for it, or the file is refused.
func TestFrameTimesAreChecked(t *testing.T) {
	lim := lim
	lim.Timeout = 5 * time.Second
	opts := Options{Count: 5, FrameEdge: 1280, Quality: 85}
	dir := t.TempDir()
	res, err := Process(context.Background(), Tools{FFprobe: script(t, fiftyPackets), FFmpeg: frameTool(t, dir, 16, 5)}, "source", dir, sniff.MP4, lim, opts)
	if err != nil {
		t.Fatalf("five frames with five times: %v", err)
	}
	if len(res.Frames) != 5 || res.Frames[0].TimeS != 0.5 || res.Frames[4].TimeS != 4.5 {
		t.Errorf("times: %+v", res.Frames)
	}
	dir = t.TempDir()
	_, err = Process(context.Background(), Tools{FFprobe: script(t, fiftyPackets), FFmpeg: frameTool(t, dir, 16, 4)}, "source", dir, sniff.MP4, lim, opts)
	wantRefusal(t, "five frames with four times", err, contract.Undecodable)
}

// The size of a frame is read from its header and checked before the decode.
func TestFrameSizeIsChecked(t *testing.T) {
	lim := lim
	lim.Timeout = 5 * time.Second
	for _, c := range []struct {
		size, edge int
		ok         bool
	}{{16, 16, true}, {16, 64, true}, {64, 16, false}, {17, 16, false}} {
		dir := t.TempDir()
		opts := Options{Count: 5, FrameEdge: c.edge, Quality: 85}
		_, err := Process(context.Background(), Tools{FFprobe: script(t, fiftyPackets), FFmpeg: frameTool(t, dir, c.size, 5)}, "source", dir, sniff.MP4, lim, opts)
		name := fmt.Sprintf("a %d px frame, edge %d", c.size, c.edge)
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok {
			wantRefusal(t, name, err, contract.Undecodable)
		}
	}
}

// frameTool is an ffmpeg that writes the five frames of the plan, each a gray
// square of size pixels, and prints the given count of showinfo lines.
func frameTool(t *testing.T, dir string, size, lines int) string {
	t.Helper()
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewGray(image.Rect(0, 0, size, size)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.jpg"), jpg.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return script(t, fmt.Sprintf(`for i in 1 2 3 4 5; do cp ok.jpg frame_0$i.jpg; done
i=0; while [ $i -lt %d ]; do printf '[Parsed_showinfo_1 @ 0x1] n:   %%d pts: 0 pts_time:%%d.5 duration: 1\n' $i $i >&2; i=$((i+1)); done`, lines))
}

func TestQscale(t *testing.T) {
	for quality, want := range map[int]int{100: 2, 95: 3, 85: 5, 50: 12, 1: 22} {
		if got := qscale(quality); got != want {
			t.Errorf("qscale(%d) = %d, want %d", quality, got, want)
		}
	}
}

func TestScore(t *testing.T) {
	flat := func(v uint8) image.Image {
		img := image.NewGray(image.Rect(0, 0, 64, 64))
		for i := range img.Pix {
			img.Pix[i] = v
		}
		return img
	}
	if luma, sharp := score(flat(0)); luma != 0 || sharp != 0 {
		t.Errorf("black: luma %v sharpness %v", luma, sharp)
	}
	if luma, sharp := score(flat(255)); math.Abs(luma-255) > 1 || sharp != 0 {
		t.Errorf("white: luma %v sharpness %v", luma, sharp)
	}

	// A checkerboard is as sharp as a picture gets; a soft slope is not sharp.
	board := image.NewYCbCr(image.Rect(0, 0, 64, 64), image.YCbCrSubsampleRatio420)
	slope := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x/2+y/2)%2 == 0 {
				board.Y[board.YOffset(x, y)] = 255
			}
			slope.Set(x, y, color.Gray{uint8(x * 2)})
		}
	}
	lumaBoard, sharpBoard := score(board)
	_, sharpSlope := score(slope)
	if math.Abs(lumaBoard-127.5) > 2 || sharpBoard < 1000 || sharpSlope > 10 {
		t.Errorf("checkerboard luma %v sharpness %v, slope sharpness %v", lumaBoard, sharpBoard, sharpSlope)
	}
	if luma, sharp := score(image.NewGray(image.Rect(0, 0, 4, 4))); luma != 0 || sharp != 0 {
		t.Errorf("a tiny frame: %v %v", luma, sharp)
	}
}

func FuzzProbeAndPackets(f *testing.F) {
	f.Add([]byte(probeJSON))
	f.Add(packetLines(1, 30, 0))
	f.Fuzz(func(t *testing.T, b []byte) {
		if in, err := parseProbe(b); err == nil {
			_ = in.check(lim)
		}
		p, err := parsePackets(b)
		if err != nil {
			return
		}
		if p.check(&info{duration: p.duration(), videoDuration: math.NaN()}, lim) != nil {
			return
		}
		// A stream that passes the checks gives a plan the ceilings bound.
		s := newSampling(p, Options{EverySeconds: 1})
		if s.count < 1 || s.count > 18 || math.IsNaN(s.interval) {
			t.Fatalf("a plan outside the ceilings: %+v from %+v", s, p)
		}
		_ = s.filter(1280)
	})
}

// script writes a shell script that stands in for ffprobe or ffmpeg.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A child process that does not end by itself gives a refusal, and the sign
// that the worker must not trust the execution environment after it.
func TestAbnormalEnds(t *testing.T) {
	short := lim
	short.Timeout = 300 * time.Millisecond
	cases := []struct {
		name, ffprobe string
		code          contract.RefusalCode
		abnormal      bool
	}{
		{"a probe that never ends", "sleep 30", contract.DecodeTimeout, true},
		{"a probe that dies on a signal", "kill -SEGV $$", contract.Undecodable, true},
		{"a probe that prints without end", "while :; do echo 0.000000,0.033333; done", contract.Undecodable, true},
		{"a probe that fails", "echo 'moov atom not found' >&2; exit 1", contract.Undecodable, false},
	}
	for _, c := range cases {
		tools := Tools{FFprobe: script(t, c.ffprobe), FFmpeg: script(t, "exit 0")}
		_, err := Process(context.Background(), tools, "source", t.TempDir(), sniff.MP4, short, Options{Count: 5, FrameEdge: 1280, Quality: 85})
		wantRefusal(t, c.name, err, c.code)
		var abnormal *Abnormal
		if errors.As(err, &abnormal) != c.abnormal {
			t.Errorf("%s: abnormal is %v, want %v", c.name, !c.abnormal, c.abnormal)
		}
		// What the tool printed comes from the file. It is not in the detail.
		var r *contract.Refusal
		if errors.As(err, &r) && strings.Contains(r.Detail, "moov") {
			t.Errorf("%s: the detail carries text of the tool: %q", c.name, r.Detail)
		}
	}

	// The limit is for the whole video. A probe that uses most of it leaves
	// too little for the packet read that follows.
	slow := Tools{FFmpeg: script(t, "exit 0"), FFprobe: script(t, `sleep 0.2
echo '{"streams":[{"codec_name":"h264","codec_type":"video","width":64,"height":48,"duration":"5"}],"format":{"duration":"5"}}'`)}
	start := time.Now()
	_, err := Process(context.Background(), slow, "source", t.TempDir(), sniff.MP4, short, Options{Count: 5, FrameEdge: 1280, Quality: 85})
	wantRefusal(t, "two slow runs", err, contract.DecodeTimeout)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("two slow runs took %s with a limit of %s", took, short.Timeout)
	}

	// The container is refused before any tool runs.
	_, err = Process(context.Background(), Tools{FFprobe: "/no/such/tool"}, "source", t.TempDir(), sniff.MOV, lim, Options{})
	wantRefusal(t, "a QuickTime file", err, contract.UnsupportedContainer)
	// A tool that is not there is an error of the worker, not a refusal.
	short.Containers = []string{"mp4"}
	_, err = Process(context.Background(), Tools{FFprobe: "/no/such/tool"}, "source", t.TempDir(), sniff.MP4, short, Options{})
	var r *contract.Refusal
	if err == nil || errors.As(err, &r) {
		t.Errorf("a missing tool: %v", err)
	}
}

// The decoder must make every frame of the plan, and no more.
func TestFrameCountIsChecked(t *testing.T) {
	probe := script(t, fiftyPackets)
	opts := Options{Count: 5, FrameEdge: 1280, Quality: 85}
	lim := lim
	lim.Timeout = 5 * time.Second
	// An ffmpeg that exits with success and writes nothing.
	_, err := Process(context.Background(), Tools{FFprobe: probe, FFmpeg: script(t, "exit 0")}, "source", t.TempDir(), sniff.MP4, lim, opts)
	wantRefusal(t, "no frames", err, contract.Undecodable)
	// An ffmpeg that writes a file that is not a JPEG.
	_, err = Process(context.Background(), Tools{FFprobe: probe, FFmpeg: script(t, "echo not-a-jpeg > frame_01.jpg")}, "source", t.TempDir(), sniff.MP4, lim, opts)
	wantRefusal(t, "a frame that is not a JPEG", err, contract.Undecodable)
}

// The log that showinfoTimes reads is partly the text of the file. It must not
// panic, and every time it returns is a finite number from a frame line.
func FuzzShowinfoTimes(f *testing.F) {
	f.Add([]byte("[Parsed_showinfo_1 @ 0x1] n:   0 pts: 0 pts_time:1.5 duration: 1\n"))
	f.Add([]byte("    title : [Parsed_showinfo_1 @ 0x1] n: 0 pts_time:Inf\n[Parsed_showinfo_1 @ 0x1] n: 1 pts_time:"))
	f.Add([]byte("[Parsed_showinfo_1 @ 0x1] n: 2 pts_time:NaN\r\n[Parsed_showinfo_1 @ 0x1] n: 3 pts_time:1e400"))
	f.Fuzz(func(t *testing.T, b []byte) {
		times := showinfoTimes(b)
		lines := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "[Parsed_showinfo_") {
				lines++
			}
		}
		if len(times) > lines {
			t.Fatalf("%d times from %d frame lines", len(times), lines)
		}
		for _, v := range times {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("a time of %v", v)
			}
		}
	})
}
