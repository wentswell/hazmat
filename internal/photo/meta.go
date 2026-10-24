package photo

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"

	"github.com/wentswell/hazmat/internal/sniff"
)

// maxICC is the largest color profile the worker copies. A display profile is
// a few KiB; a profile above this is not one the worker wants to carry.
const maxICC = 1 << 20

// meta is what the worker reads from the container around the pixels. Only
// orientation and icc change the output. The rest is reported to the caller.
type meta struct {
	orientation int
	icc         []byte
	hadMetadata bool
	hadGPS      bool
	animated    bool
}

func scan(b []byte, t sniff.Type) meta {
	m := meta{orientation: 1}
	switch t {
	case sniff.JPEG:
		scanJPEG(b, &m)
	case sniff.PNG:
		scanPNG(b, &m)
	case sniff.WebP:
		scanWebP(b, &m)
	}
	if !usableICC(m.icc) {
		m.icc = nil
	}
	return m
}

func (m *meta) exif(tiff []byte) {
	m.hadMetadata = true
	o, gps := parseTIFF(tiff)
	m.orientation = o
	m.hadGPS = m.hadGPS || gps
}

func scanJPEG(b []byte, m *meta) {
	parts := map[byte][]byte{}
	var count byte
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			break
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF: // fill byte
			i++
			continue
		case marker == 0xD8, marker == 0x01, marker >= 0xD0 && marker <= 0xD7: // no length
			i += 2
			continue
		case marker == 0xDA, marker == 0xD9: // the pixels start, or the file ends
			i = len(b)
			continue
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			break
		}
		seg := b[i+4 : i+2+n]
		switch {
		case marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")):
			m.exif(seg[6:])
		case marker == 0xE1, marker == 0xED, marker == 0xFE: // XMP, IPTC, comment
			m.hadMetadata = true
		case marker == 0xE2 && bytes.HasPrefix(seg, []byte("ICC_PROFILE\x00")) && len(seg) > 14:
			parts[seg[12]] = seg[14:]
			count = seg[13]
		}
		i += 2 + n
	}

	// A profile above 64 KiB is split over numbered segments. Take it only
	// when every part is there.
	if count == 0 || len(parts) != int(count) {
		return
	}
	var icc []byte
	for seq := byte(1); seq <= count && seq != 0; seq++ {
		p, ok := parts[seq]
		if !ok || len(icc)+len(p) > maxICC {
			return
		}
		icc = append(icc, p...)
	}
	m.icc = icc
}

func scanPNG(b []byte, m *meta) {
	for i := 8; i+12 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n < 0 || n > len(b)-i-12 {
			break
		}
		data := b[i+8 : i+8+n]
		switch string(b[i+4 : i+8]) {
		case "iCCP":
			// name, a zero byte, the compression method, then a zlib stream.
			if z := bytes.IndexByte(data, 0); z >= 0 && z+2 <= len(data) {
				m.icc = inflate(data[z+2:], maxICC)
			}
		case "eXIf":
			m.exif(data)
		case "tEXt", "zTXt", "iTXt", "tIME":
			m.hadMetadata = true
		case "acTL":
			m.animated = true
		}
		i += 12 + n
	}
}

func scanWebP(b []byte, m *meta) {
	for i := 12; i+8 <= len(b); {
		n := int(binary.LittleEndian.Uint32(b[i+4:]))
		if n < 0 || n > len(b)-i-8 {
			break
		}
		data := b[i+8 : i+8+n]
		switch string(b[i : i+4]) {
		case "VP8X":
			if len(data) > 0 && data[0]&0x02 != 0 {
				m.animated = true
			}
		case "ANIM", "ANMF":
			m.animated = true
		case "ICCP":
			if len(data) <= maxICC {
				m.icc = data
			}
		case "EXIF":
			m.exif(bytes.TrimPrefix(data, []byte("Exif\x00\x00")))
		case "XMP ":
			m.hadMetadata = true
		}
		i += 8 + n + n&1 // chunks are padded to an even length
	}
}

// inflate returns nil when the stream is bad or larger than max.
func inflate(b []byte, max int) []byte {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil || len(out) > max {
		return nil
	}
	return out
}

// usableICC accepts only an RGB profile. The output is always an RGB JPEG, so
// a CMYK or a gray profile on it would be wrong, and wrong is worse than none.
func usableICC(icc []byte) bool {
	return len(icc) >= 128 && len(icc) <= maxICC &&
		string(icc[16:20]) == "RGB " && string(icc[36:40]) == "acsp"
}

// parseTIFF reads the two EXIF facts the worker uses: the orientation, and
// whether a GPS block exists. It reads nothing else and follows no offset
// except the one to the first directory.
func parseTIFF(b []byte) (orientation int, gps bool) {
	orientation = 1
	if len(b) < 8 {
		return
	}
	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}
	if bo.Uint16(b[2:]) != 42 {
		return
	}
	off := int64(bo.Uint32(b[4:]))
	if off+2 > int64(len(b)) {
		return
	}
	entries := int64(bo.Uint16(b[off:]))
	for p := off + 2; entries > 0 && p+12 <= int64(len(b)); p, entries = p+12, entries-1 {
		e := b[p : p+12]
		switch bo.Uint16(e) {
		case 0x0112: // Orientation: one SHORT
			if v := int(bo.Uint16(e[8:])); bo.Uint16(e[2:]) == 3 && bo.Uint32(e[4:]) == 1 && v >= 1 && v <= 8 {
				orientation = v
			}
		case 0x8825: // pointer to the GPS directory
			gps = true
		}
	}
	return
}

// embedICC puts the profile after the start marker of a JPEG, split into
// segments as the ICC specification says.
func embedICC(jpg, icc []byte) []byte {
	if len(icc) == 0 || len(jpg) < 2 {
		return jpg
	}
	const chunk = 65535 - 2 - 14
	count := (len(icc) + chunk - 1) / chunk
	if count > 255 {
		return jpg
	}
	out := make([]byte, 0, len(jpg)+len(icc)+count*18)
	out = append(out, jpg[:2]...)
	for seq := 1; seq <= count; seq++ {
		part := icc[(seq-1)*chunk : min(seq*chunk, len(icc))]
		out = append(out, 0xFF, 0xE2)
		out = binary.BigEndian.AppendUint16(out, uint16(len(part)+16))
		out = append(out, "ICC_PROFILE\x00"...)
		out = append(out, byte(seq), byte(count))
		out = append(out, part...)
	}
	return append(out, jpg[2:]...)
}
