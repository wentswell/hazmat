// Package sniff finds the real type of a file from its first bytes. The key
// name and the Content-Type of an upload come from the client and mean nothing.
package sniff

import "bytes"

type Type string

const (
	Unknown Type = ""
	JPEG    Type = "image/jpeg"
	PNG     Type = "image/png"
	WebP    Type = "image/webp"
	GIF     Type = "image/gif"
	HEIF    Type = "image/heif"
	AVIF    Type = "image/avif"
	MP4     Type = "video/mp4"
	MOV     Type = "video/quicktime"
)

// HeadLen is how many bytes Detect needs.
const HeadLen = 16

func Detect(b []byte) Type {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return WebP
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return GIF
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		return brand(string(b[8:12]))
	}
	return Unknown
}

// brand maps the major brand of an ISO base media file. HEIF and AVIF photos
// share the container with MP4 video, so the brand is the only early sign.
func brand(major string) Type {
	switch major {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1":
		return HEIF
	case "avif", "avis":
		return AVIF
	case "qt  ":
		return MOV
	}
	return MP4
}

// Short is the name the operator uses in HAZMAT_PHOTO_TYPES.
func (t Type) Short() string {
	switch t {
	case JPEG:
		return "jpeg"
	case PNG:
		return "png"
	case WebP:
		return "webp"
	case GIF:
		return "gif"
	case HEIF:
		return "heif"
	case AVIF:
		return "avif"
	case MP4:
		return "mp4"
	case MOV:
		return "mov"
	}
	return "unknown"
}

func (t Type) IsPhoto() bool {
	switch t {
	case JPEG, PNG, WebP, GIF, HEIF, AVIF:
		return true
	}
	return false
}

func (t Type) IsVideo() bool { return t == MP4 || t == MOV }
