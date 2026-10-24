# The ffmpeg and ffprobe that the worker runs: built from a pinned source, with
# only what the video path needs. A component that is not in the binary cannot
# be attacked, so add one only with a reason.
#
#   make ffmpeg        writes dist/ffmpeg/{ffmpeg,ffprobe} for the Docker host
#
# To move to a new ffmpeg release: change FFMPEG_VERSION and FFMPEG_SHA256
# (sha256 of the .tar.xz from https://ffmpeg.org/releases/, checked against its
# .asc signature), then run `make ffmpeg local-test`.

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS build

ARG FFMPEG_VERSION=8.1.3
ARG FFMPEG_SHA256=7138d28c96d9d3e3af4ee3d8cad72741f8ffb40da90c1112235dea3ecd3178a3

RUN apk add --no-cache build-base curl linux-headers nasm xz

WORKDIR /src
RUN curl -fsSLO "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" \
 && echo "${FFMPEG_SHA256}  ffmpeg-${FFMPEG_VERSION}.tar.xz" | sha256sum -c - \
 && tar -xf "ffmpeg-${FFMPEG_VERSION}.tar.xz" --strip-components=1

# What is on, and why:
#   mov demuxer, h264 decoder + parser   the one container and codec the worker accepts
#   mjpeg encoder, image2 muxer          the JPEG frames
#   file protocol                        local files only; the network is off
#   select                               takes one frame for each interval
#   scale, format                        frame size, and the pixel format of JPEG
#   transpose, hflip, vflip, rotate      ffmpeg adds these for a rotated video
#   showinfo                             prints the time of each frame taken
# There is no video encoder except mjpeg, no audio decoder, no hardware
# decoder, and no outside library (zlib included: a compressed index is refused).
# The build is LGPL: no --enable-gpl and no --enable-nonfree.
RUN ./configure \
      --disable-everything \
      --disable-autodetect \
      --disable-network \
      --disable-doc \
      --disable-debug \
      --disable-ffplay \
      --disable-shared \
      --enable-static \
      --extra-ldflags=-static \
      --enable-demuxer=mov \
      --enable-decoder=h264 \
      --enable-parser=h264 \
      --enable-encoder=mjpeg \
      --enable-muxer=image2 \
      --enable-protocol=file \
      --enable-filter=select,scale,format,transpose,hflip,vflip,rotate,showinfo \
 && make -j"$(nproc)" ffmpeg ffprobe \
 && strip ffmpeg ffprobe \
 && ./ffmpeg -hide_banner -buildconf | tail -n +2 > buildconf.txt

# The LGPL needs the license beside the programs and the source to be
# available: the zip gets COPYING.LGPLv2.1, and each release gets the tarball.
FROM scratch
COPY --from=build /src/ffmpeg /src/ffprobe /src/buildconf.txt /src/COPYING.LGPLv2.1 /src/ffmpeg-*.tar.xz /
