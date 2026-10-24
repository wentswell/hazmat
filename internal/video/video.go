// Package video turns one untrusted video into still JPEG frames. ffprobe and
// ffmpeg read the file, as child processes; this package decides what they are
// asked, and believes nothing in the file that it did not measure.
package video

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/sandbox"
	"github.com/wentswell/hazmat/internal/sniff"
)

// Tools are the two programs of the pinned ffmpeg build.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// Limits are the operator ceilings, and max_seconds of the request.
type Limits struct {
	Containers  []string // "mp4", "mov"
	Codecs      []string // "h264"
	MaxEdge     int      // long edge of the source
	MaxFPS      float64
	MaxSeconds  float64
	Timeout     time.Duration // for all child processes of one video together
	MemoryBytes int64
}

type Options struct {
	// One of the two. Count is changed to an interval with the measured duration.
	Count        int
	EverySeconds float64
	FrameEdge    int
	Quality      int
}

type Result struct {
	Container string
	Codec     string
	// Width and Height are of the video as a player shows it, after rotation.
	Width, Height    int
	Duration         float64 // measured from the packets
	DeclaredDuration float64 // what the header says
	FPS              float64
	Rotation         int
	HasAudio         bool
	Frames           []Frame
	Probe, Decode    time.Duration
}

type Frame struct {
	Name          string
	Data          []byte
	TimeS         float64
	Width, Height int
	// Luma is the mean brightness, 0 to 255. Sharpness is the variance of a
	// Laplacian filter: near 0 for a flat or blurred frame. The caller uses
	// them to drop a black frame and to choose a poster. The worker does not.
	Luma      float64
	Sharpness float64
}

// Abnormal is returned beside a refusal when a child process did not end by
// itself. The worker does not trust the execution environment after that.
type Abnormal struct{ *contract.Refusal }

func (a *Abnormal) Unwrap() error { return a.Refusal }

// durationSlack is how far a measured value may be from a declared or allowed
// one. A header rounds, and the audio of a correct file can be a little longer
// than its video.
const durationSlack = 0.5

// Process reads the video at path and writes frames into dir, which must be
// empty and is the only place a child process writes.
//
// A *contract.Refusal (or an *Abnormal that wraps one) means the file is the
// problem. Any other error means the tools could not run.
func Process(ctx context.Context, tools Tools, path, dir string, t sniff.Type, lim Limits, opts Options) (*Result, error) {
	if !contains(lim.Containers, t.Short()) {
		return nil, contract.Refuse(contract.UnsupportedContainer, "%s files are not accepted", t.Short())
	}
	// One time limit for the whole video, not one for each child process:
	// the three runs together must end before the caller of the worker
	// gives up, so that a slow file is a refusal and not a timeout there.
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	run := runner{ctx: ctx, tools: tools, dir: dir, lim: lim}
	res := &Result{Container: t.Short()}

	start := time.Now()
	info, err := run.probe(path)
	if err != nil {
		return nil, err
	}
	if err := info.check(lim); err != nil {
		return nil, err
	}
	packets, err := run.packets(path)
	if err != nil {
		return nil, err
	}
	if err := packets.check(info, lim); err != nil {
		return nil, err
	}
	res.Probe = time.Since(start)

	res.Codec = info.codec
	res.Width, res.Height = info.width, info.height
	if info.rotation%180 != 0 {
		res.Width, res.Height = res.Height, res.Width
	}
	res.Rotation = info.rotation
	res.HasAudio = info.hasAudio
	res.DeclaredDuration = info.duration
	res.Duration = packets.duration()
	res.FPS = packets.fps()

	start = time.Now()
	plan := newSampling(packets, opts)
	times, err := run.frames(path, plan, lim, opts)
	if err != nil {
		return nil, err
	}
	// Fail closed: every frame the plan names must exist. A file that decodes
	// in part hides the rest from the scan.
	for i := 0; i < plan.count; i++ {
		name := contract.FrameName(i + 1)
		data, err := os.ReadFile(filepath.Join(dir, name+".jpg"))
		if err != nil {
			return nil, contract.Refuse(contract.Undecodable, "the decoder made %d of %d frames", i, plan.count)
		}
		// The header first: the frame is the work of a child process that
		// read a hostile file, so its size is checked before the decode.
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, contract.Refuse(contract.Undecodable, "frame %d of the decoder is not a JPEG", i+1)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || max(cfg.Width, cfg.Height) > opts.FrameEdge {
			return nil, contract.Refuse(contract.Undecodable, "frame %d of the decoder is empty or above the edge %d", i+1, opts.FrameEdge)
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, contract.Refuse(contract.Undecodable, "frame %d of the decoder is not a JPEG", i+1)
		}
		f := Frame{Name: name, Data: data, Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}
		f.Luma, f.Sharpness = score(img)
		if i < len(times) {
			f.TimeS = round3(times[i] - packets.first)
		}
		res.Frames = append(res.Frames, f)
	}
	if extra := contract.FrameName(plan.count+1) + ".jpg"; exists(filepath.Join(dir, extra)) {
		return nil, contract.Refuse(contract.Undecodable, "the decoder made more than %d frames", plan.count)
	}
	// The time of a frame is a measured fact. One frame line of the filter
	// for each frame, or the times are not known: the log was cut, or the
	// file wrote into it.
	if len(times) != plan.count {
		return nil, contract.Refuse(contract.Undecodable, "the decoder reported %d times for %d frames", len(times), plan.count)
	}
	res.Decode = time.Since(start)
	return res, nil
}

type runner struct {
	ctx   context.Context
	tools Tools
	dir   string
	lim   Limits
}

// The input options of both tools: read a local file, as MP4 or QuickTime, and
// nothing else. The format is named, so ffmpeg does not guess it from the bytes.
var inputFlags = []string{"-protocol_whitelist", "file", "-f", "mov"}

func (r runner) run(stdoutBytes int, name string, args ...string) (sandbox.Result, error) {
	res, err := sandbox.Run(r.ctx, sandbox.Limits{
		Timeout:     r.lim.Timeout,
		MemoryBytes: r.lim.MemoryBytes,
		FileBytes:   64 << 20,
		StdoutBytes: stdoutBytes,
	}, r.dir, name, args...)
	if err != nil {
		return res, contract.Fail("start %s: %w", filepath.Base(name), err)
	}
	return res, nil
}

// refusal turns the end of a child process into a refusal, or nil for a clean
// exit. The text of the child is not in the detail: the file controls it.
func refusal(tool string, res sandbox.Result) error {
	switch {
	case res.Overflowed:
		return &Abnormal{contract.Refuse(contract.Undecodable, "%s printed more than its limit", tool)}
	case res.TimedOut:
		return &Abnormal{contract.Refuse(contract.DecodeTimeout, "%s did not finish in time", tool)}
	case res.Signaled:
		return &Abnormal{contract.Refuse(contract.Undecodable, "%s was stopped by a signal", tool)}
	case res.ExitCode != 0:
		return contract.Refuse(contract.Undecodable, "%s cannot read the file (exit status %d)", tool, res.ExitCode)
	}
	return nil
}

func (r runner) probe(path string) (*info, error) {
	args := append([]string{"-v", "error"}, inputFlags...)
	args = append(args, "-i", path,
		"-show_entries", "format=duration:stream=codec_type,codec_name,width,height,duration:stream_side_data=rotation",
		"-of", "json")
	res, err := r.run(1<<20, r.tools.FFprobe, args...)
	if err != nil {
		return nil, err
	}
	if err := refusal("ffprobe", res); err != nil {
		return nil, err
	}
	return parseProbe(res.Stdout)
}

func (r runner) packets(path string) (*packets, error) {
	args := append([]string{"-v", "error"}, inputFlags...)
	args = append(args, "-i", path, "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0")
	// The output is one short line for each frame. The limit is far above
	// what the frame-rate and duration ceilings allow.
	res, err := r.run(8<<20, r.tools.FFprobe, args...)
	if err != nil {
		return nil, err
	}
	if err := refusal("ffprobe", res); err != nil {
		return nil, err
	}
	return parsePackets(res.Stdout)
}

// frames runs the one decode pass and returns the time of each frame taken.
func (r runner) frames(path string, plan sampling, lim Limits, opts Options) ([]float64, error) {
	args := []string{"-nostdin", "-hide_banner", "-nostats", "-loglevel", "info"}
	args = append(args, inputFlags...)
	args = append(args,
		// The plan and the times of the frames are in the clock of the
		// packets that ffprobe listed. Without -copyts, ffmpeg moves the
		// first timestamp of the input to zero, so a file whose packets do
		// not start at zero is sampled at the wrong times and refused. The
		// input -t is still counted from the first packet.
		"-copyts",
		// A second stop behind the measured duration.
		"-t", ftoa(lim.MaxSeconds+1),
		"-i", path,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		// The output gets no tag or chapter of the file. ffmpeg prints the
		// output header from another thread while the filter prints its
		// frame lines, so tags there could break into those lines.
		"-map_metadata", "-1", "-map_chapters", "-1",
		"-vf", plan.filter(opts.FrameEdge),
		"-fps_mode", "passthrough",
		"-frames:v", strconv.Itoa(plan.count+1),
		"-q:v", strconv.Itoa(qscale(opts.Quality)),
		"-f", "image2", "frame_%02d.jpg",
	)
	res, err := r.run(1<<10, r.tools.FFmpeg, args...)
	if err != nil {
		return nil, err
	}
	if err := refusal("ffmpeg", res); err != nil {
		return nil, err
	}
	return showinfoTimes(res.Stderr), nil
}

// sampling says which frames the decode pass takes: the first frame at or
// after the middle of each interval. The middle, because the first frame of a
// video is often black.
type sampling struct {
	first    float64 // time of the first packet
	interval float64
	offset   float64 // from first to the first sample
	count    int
}

func newSampling(p *packets, opts Options) sampling {
	s := sampling{first: p.first, interval: opts.EverySeconds}
	if opts.Count > 0 {
		s.interval = p.duration() / float64(opts.Count)
	}
	s.offset = s.interval / 2
	last := p.last - p.first
	// A file so short that it has no frame at the middle of the first
	// interval still gives its first frame.
	if last < s.offset {
		s.offset = 0
	}
	s.count = int(math.Floor((last-s.offset)/s.interval+1e-9)) + 1
	if opts.Count > 0 && s.count > opts.Count {
		s.count = opts.Count
	}
	return s
}

// filter is the ffmpeg filter chain. Each frame belongs to the interval
// floor((t - first - offset) / interval). A frame is taken when its interval
// is after the interval of the frame taken before it, so one late frame does
// not move the samples that follow.
func (s sampling) filter(edge int) string {
	// The intervals start one microsecond early, the precision of the
	// numbers here. A frame exactly at the start of an interval then belongs
	// to it, as in newSampling, whatever the rounding of t-start: 7.0-5.4 is
	// 1.5999999999999996, and without the margin the frame at 7 s would go
	// to the interval before.
	start := ftoa(s.first + s.offset - 1e-6)
	rel := fmt.Sprintf("(t-%s)", start)
	bucket := func(t string) string {
		return fmt.Sprintf("floor((%s-%s)/%s)", t, start, ftoa(s.interval))
	}
	sel := fmt.Sprintf("gte(%s,0)*(isnan(prev_selected_t)+gt(%s,%s))", rel, bucket("t"), bucket("prev_selected_t"))
	// In a filter chain a comma separates filters, so the commas of the
	// expressions are escaped.
	esc := strings.NewReplacer(",", `\,`)
	e := strconv.Itoa(edge)
	return strings.Join([]string{
		"select=" + esc.Replace(sel),
		"showinfo",
		// Never larger than the source, and JPEG wants even dimensions.
		"scale=w=" + esc.Replace("min("+e+",iw)") + ":h=" + esc.Replace("min("+e+",ih)") +
			":force_original_aspect_ratio=decrease:force_divisible_by=2:out_range=full",
		"format=yuvj420p",
	}, ",")
}

// showinfoTimes reads the pts_time of each frame line the showinfo filter
// printed. The same stream carries the metadata of the file, which ffmpeg
// prints indented, so only a line that starts with the name of the filter is
// one of its own, and only a finite number is a time it measured. A line the
// file forged, or one the stderr cap cut, is left out, and the count of
// times then differs from the count of frames, which refuses the file.
func showinfoTimes(stderr []byte) []float64 {
	var times []float64
	for _, line := range strings.Split(string(stderr), "\n") {
		if !strings.HasPrefix(line, "[Parsed_showinfo_") || !strings.Contains(line, " n:") {
			continue
		}
		_, after, ok := strings.Cut(line, " pts_time:")
		if !ok {
			continue
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		times = append(times, v)
	}
	return times
}

// qscale maps a JPEG quality of 1 to 100 to the scale of the ffmpeg encoder,
// where 2 is the best. The match is approximate.
func qscale(quality int) int {
	q := 2 + int(math.Round(float64(100-quality)*0.2))
	return min(max(q, 2), 31)
}

// score gives the mean brightness and the sharpness of a frame.
func score(img image.Image) (luma, sharpness float64) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// Every second pixel is enough for both numbers, and four times faster.
	const step = 2
	gw, gh := w/step, h/step
	if gw < 3 || gh < 3 {
		return 0, 0
	}
	gray := make([]float64, gw*gh)
	var sum float64
	ycc, _ := img.(*image.YCbCr)
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			var v float64
			if ycc != nil {
				v = float64(ycc.Y[ycc.YOffset(b.Min.X+x*step, b.Min.Y+y*step)])
			} else {
				r, g, bl, _ := img.At(b.Min.X+x*step, b.Min.Y+y*step).RGBA()
				v = (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
			}
			gray[y*gw+x] = v
			sum += v
		}
	}
	luma = sum / float64(gw*gh)

	var lsum, lsq float64
	n := 0
	for y := 1; y < gh-1; y++ {
		for x := 1; x < gw-1; x++ {
			i := y*gw + x
			l := gray[i-gw] + gray[i+gw] + gray[i-1] + gray[i+1] - 4*gray[i]
			lsum += l
			lsq += l * l
			n++
		}
	}
	mean := lsum / float64(n)
	return round3(luma), round3(lsq/float64(n) - mean*mean)
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
