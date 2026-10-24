package video

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/wentswell/hazmat/internal/contract"
)

// info is what the header of the file declares. The worker checks it against
// the ceilings first, because that is cheap, and then measures.
type info struct {
	codec         string
	width, height int // as stored, before rotation
	rotation      int // degrees, a multiple of 90
	duration      float64
	videoDuration float64
	videoStreams  int
	hasAudio      bool
}

func parseProbe(out []byte) (*info, error) {
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
			SideData  []struct {
				Rotation *float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, contract.Refuse(contract.Undecodable, "ffprobe gave no description of the file")
	}
	in := &info{duration: seconds(doc.Format.Duration)}
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			in.videoStreams++
			if in.videoStreams > 1 {
				continue
			}
			in.codec, in.width, in.height = s.CodecName, s.Width, s.Height
			in.videoDuration = seconds(s.Duration)
			for _, sd := range s.SideData {
				if sd.Rotation != nil {
					in.rotation = int(math.Round(*sd.Rotation))
				}
			}
		case "audio":
			in.hasAudio = true
		}
	}
	return in, nil
}

func (in *info) check(lim Limits) error {
	switch {
	case in.videoStreams == 0:
		return contract.Refuse(contract.BadStreamLayout, "the file has no video stream")
	case in.videoStreams > 1:
		// A player shows one stream. The scan must see the same one, and
		// with two the worker cannot know which that is.
		return contract.Refuse(contract.BadStreamLayout, "the file has %d video streams", in.videoStreams)
	case !contains(lim.Codecs, in.codec):
		return contract.Refuse(contract.UnsupportedCodec, "%s video is not accepted", printable(in.codec))
	case in.width <= 0 || in.height <= 0:
		return contract.Refuse(contract.Undecodable, "the video has no dimensions")
	case max(in.width, in.height) > lim.MaxEdge:
		return contract.Refuse(contract.TooManyPixels, "%d x %d, limit %d on the long edge", in.width, in.height, lim.MaxEdge)
	case in.rotation%90 != 0:
		return contract.Refuse(contract.BadStreamLayout, "the rotation is %d degrees", in.rotation)
	case math.IsNaN(in.duration) || in.duration <= 0:
		return contract.Refuse(contract.Undecodable, "the file declares no duration")
	case in.duration > lim.MaxSeconds+durationSlack:
		return contract.Refuse(contract.TooLong, "declared %.1f s, limit %.0f s", in.duration, lim.MaxSeconds)
	}
	return nil
}

// packets is what the worker measured: the time of each video packet, read
// from the sample tables with no decode. A header can declare any duration;
// these are the times that the decode pass and a player use.
type packets struct {
	count int
	first float64 // lowest packet time
	last  float64 // highest packet time
	end   float64 // highest packet time plus its duration
}

func parsePackets(out []byte) (*packets, error) {
	p := &packets{first: math.Inf(1), last: math.Inf(-1), end: math.Inf(-1)}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pts, dur, _ := strings.Cut(line, ",")
		t := seconds(pts)
		if math.IsNaN(t) {
			return nil, contract.Refuse(contract.Undecodable, "a video packet has no time")
		}
		d := seconds(strings.TrimRight(dur, ","))
		if math.IsNaN(d) || d < 0 {
			d = 0
		}
		p.count++
		p.first = math.Min(p.first, t)
		p.last = math.Max(p.last, t)
		p.end = math.Max(p.end, t+d)
	}
	if p.count == 0 {
		return nil, contract.Refuse(contract.Undecodable, "the video stream has no packets")
	}
	return p, nil
}

func (p *packets) duration() float64 { return p.end - p.first }

func (p *packets) fps() float64 {
	if d := p.duration(); d > 0 {
		return math.Round(float64(p.count)/d*100) / 100
	}
	return 0
}

func (p *packets) check(in *info, lim Limits) error {
	d := p.duration()
	switch {
	case d <= 0:
		return contract.Refuse(contract.Undecodable, "the video has no duration")
	case d > lim.MaxSeconds+durationSlack:
		return contract.Refuse(contract.TooLong, "measured %.1f s, limit %.0f s", d, lim.MaxSeconds)
	// The header and the packets must agree. A file that declares 10 s and
	// holds 14 s is refused even when 14 s is allowed: no correct encoder
	// writes it, and a player may believe either number.
	case math.Abs(d-in.duration) > durationSlack:
		return contract.Refuse(contract.DurationMismatch, "the file declares %.1f s and its video has %.1f s", in.duration, d)
	case !math.IsNaN(in.videoDuration) && math.Abs(d-in.videoDuration) > durationSlack:
		return contract.Refuse(contract.DurationMismatch, "the video stream declares %.1f s and has %.1f s", in.videoDuration, d)
	// This limits the work of the decode pass: frames = rate x duration.
	case p.fps() > lim.MaxFPS*1.05:
		return contract.Refuse(contract.BadStreamLayout, "%.0f frames for each second, limit %.0f", p.fps(), lim.MaxFPS)
	}
	return nil
}

// seconds reads a number of ffprobe. It gives NaN for "N/A" and for anything
// that is not a finite number.
func seconds(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsInf(v, 0) {
		return math.NaN()
	}
	return v
}

// printable keeps a name from the file safe for a message: letters, digits,
// and a few marks, 16 characters at most.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 16 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
