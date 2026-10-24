#!/usr/bin/env bash
# Makes the video files of test/local/testdata with a full ffmpeg from a
# container image. The files are committed; run this only to change them.
# The worker's own ffmpeg cannot make them: it has no video encoder.
set -euo pipefail
cd "$(dirname "$0")/testdata"

ff() {
	docker run --rm -v "$PWD:/w" -w /w mwader/static-ffmpeg:latest \
		-hide_banner -loglevel error -y "$@"
}
# A moving test picture, small and cheap to decode. $1 seconds, $2 size, $3 rate.
src() { echo "-f lavfi -i testsrc2=duration=$1:size=$2:rate=$3"; }
tone() { echo "-f lavfi -i sine=frequency=440:duration=$1"; }
h264="-c:v libx264 -preset veryfast -pix_fmt yuv420p -b:v 150k"
aac="-c:a aac -b:a 32k"

# What the app sends, in small: H.264 and AAC in MP4.
ff $(src 4 320x240 30) $(tone 4) $h264 $aac -movflags +faststart good.mp4
# The same picture stored on its side, with the rotation in the header.
ff $(src 4 320x240 30) $h264 -metadata:s:v:0 rotate=90 rotated.mp4
ff -display_rotation:v:0 -90 -i rotated.mp4 -c copy rotated.tmp.mp4 && mv rotated.tmp.mp4 rotated.mp4
# One keyframe, at the start. Every other frame needs the frames before it.
ff $(src 4 320x240 30) $h264 -g 9999 -keyint_min 9999 -sc_threshold 0 onekey.mp4
# 14 s and 20 s. The tests also change their headers to say 10 s.
ff $(src 14 160x120 15) $h264 -b:v 30k fourteen.mp4
ff $(src 20 160x120 15) $h264 -b:v 30k twenty.mp4
# What the worker refuses.
ff $(src 2 320x240 30) -c:v libx265 -preset veryfast -tag:v hvc1 -pix_fmt yuv420p hevc.mp4
ff $(src 2 320x240 30) $h264 -f mov quicktime.mov
ff $(src 2 320x240 30) $h264 -map 0:v -map 0:v twovideo.mp4
ff $(tone 2) $aac -f mp4 audioonly.mp4
ff $(src 1 320x240 120) $h264 fast.mp4
ff -f lavfi -i color=c=gray:duration=1:size=2560x1440:rate=5 $h264 big.mp4

ls -l
