# hazmat

**The Lambda that handles your users' uploads so your API doesn't have to.**

Reads one untrusted upload and writes clean outputs. It refuses the file, or it
writes JPEG files with no metadata and returns facts about the media. It makes
no decision about the content and calls no service except storage.

One Go binary is the AWS Lambda handler and a command line tool.

**State:** the photo path and the video path work. The video path decodes
H.264 in MP4 or QuickTime; the ffmpeg of the release has no other decoder.

## Why

An app that accepts photos and videos from users must decode bytes that an
attacker controls. A decoder bug in the main service is a bug in the service
that holds the database and the secrets. And a photo that is stored as it was
uploaded keeps its EXIF data, which often holds the GPS position of the user.

hazmat puts the decode in one small function with no access except to
the upload bucket. It fails closed: a file that it cannot fully decode is
refused, never passed through. Every output is encoded again from decoded
pixels, so no byte of the source reaches an output.

- Photos (JPEG, PNG, WebP) use only the Go standard library and
  `golang.org/x/image`. There is no cgo and no child process.
- Videos use an ffmpeg that is built from a pinned source with one demuxer, one
  decoder, no network, and no outside library.
- Every limit (bytes, pixels, duration, frame rate) is checked before the work
  that it limits.

## Quick start

Needs Go. This runs one photo with the file store, with no cloud account.

```sh
make build
mkdir -p work/cdn/uploads/m1
cp photo.jpg work/cdn/uploads/m1/source

cat > request.json <<'JSON'
{
  "v": 1,
  "request_id": "m1",
  "source": {"bucket": "cdn", "key": "uploads/m1/source"},
  "output": {"bucket": "cdn", "prefix": "uploads/m1/"},
  "expect": "photo",
  "photo": {"renditions": [{"name": "image", "max_edge": 2048, "quality": 85}]}
}
JSON

bin/hazmat run --request request.json --root ./work
```

The response goes to stdout, and the output is at
`work/cdn/uploads/m1/image.jpg`. `.gitignore` lists `work/` and
`request.json`, so the photo, which can hold a GPS position, is not committed.
A video needs ffmpeg and ffprobe: run `make ffmpeg` (needs Docker) and set
`HAZMAT_FFMPEG_DIR=dist/ffmpeg`.

## What it does to a photo

1. Reads the size, and refuses a file above the byte ceiling before the download.
2. Finds the real type from the first bytes. The key name and the content type mean nothing.
3. Reads the dimensions from the header, and refuses an image above the pixel ceiling before the decode.
4. Decodes JPEG, PNG, or WebP with the Go decoders. A JPEG with more than 100 scans is refused as `undecodable`: each scan of a progressive JPEG decodes the whole picture again, and one scan can be a few bytes. An encoder makes about 10.
5. Turns the pixels to the EXIF orientation.
6. Writes one JPEG for each rendition of the request. It never makes an image larger.
7. Copies the RGB color profile. It copies nothing else: no EXIF, no GPS position, no comment. The profile must be an ICC profile in structure: a header with an input, display, output, or color space class, an RGB color space, and an XYZ or Lab connection space; a table of at most 64 tags, each inside the profile; and the tags of an RGB profile (the three colorants and the three curves, or `A2B0`). It is at most 256 KiB, and it is cut to the size in its header. A profile that fails is dropped, and the photo still goes through.

## What it does to a video

ffprobe and ffmpeg read the file, as child processes. The worker believes
nothing in the file that it did not measure.

1. Reads the container brand from the first bytes. Refuses a container that is not allowed (`unsupported_container`).
2. Reads the header with ffprobe. Refuses a file that does not have exactly one video stream (`bad_stream_layout`), a codec that is not allowed (`unsupported_codec`), a frame above the edge ceiling (`too_many_pixels`), and a declared duration above the limit (`too_long`).
3. Reads the time of each video packet with ffprobe, with no decode. This is the measured duration. Refuses a video that is too long (`too_long`), a header that does not agree with the packets (`duration_mismatch`), and a frame rate above the ceiling (`bad_stream_layout`).
4. Runs ffmpeg one time, in the clock of the packets. It takes the first frame at or after the middle of each interval, turns it to the display rotation, makes it smaller to `frame_edge`, and writes a JPEG.
5. Checks that every frame of the plan exists, no more and no fewer, and that each is a JPEG no larger than `frame_edge`. A file that decodes in part is refused (`undecodable`).
6. Gives each frame its time, its mean brightness (`luma`), and its `sharpness`, so the caller can drop a black frame and choose a poster. The time is the one ffmpeg measured for that frame, from the start of the packets. The worker reads it from the first 64 KiB of the log of ffmpeg. A file whose frame times are not all there is refused (`undecodable`): for example, a file with so many tags that ffmpeg prints more than 64 KiB about them first.

The worker does not read the audio (`has_audio` is a fact for the caller), does
not encode video, and does not change the source file.

**The child process** gets a fixed argument list, an empty environment, its own
process group, a time limit, and limits on memory, CPU time, and file size. It
writes into one new directory, which the worker deletes. This is not a security
boundary: the child has the user of the worker, so the role of the function is
the real limit. With `HAZMAT_RECYCLE=on_error` (the default) the worker ends its
execution environment after a child that did not end by itself: it timed out,
died on a signal, or printed without end and was stopped. With `always`, it
ends it after every video, refused or not, and never after a photo.

On Lambda, "end the execution environment" is: exit 100 ms after the worker has
its result. Lambda then starts a new environment for the next request. Two
cases can lose a request:

- Lambda freezes the environment before the exit. The exit then happens when the
  next request arrives. That request fails, and its caller tries again.
- The runtime takes more than 100 ms to send the response. The response is
  then lost and the invocation is a function error, so the caller tries the
  same file again. It is usually sent in about 1 ms.

## Request and response

```json
{
  "v": 1,
  "request_id": "media_3f9c",
  "source": {"bucket": "cdn.example", "key": "uploads/media_3f9c/source", "version_id": "Xk2"},
  "output": {"bucket": "cdn.example", "prefix": "uploads/media_3f9c/"},
  "expect": "photo",
  "photo": {"renditions": [
    {"name": "image", "max_edge": 3000, "quality": 85},
    {"name": "scan", "max_edge": 1280, "quality": 85}
  ]},
  "video": {"max_seconds": 16, "frames": {"count": 5}, "frame_edge": 1280, "quality": 85}
}
```

- `v` is the contract version, `1`. A field the worker does not know is an `InvalidRequestError`.
- `request_id` matches `^[A-Za-z0-9._:-]{1,128}$`.
- `source.key` and `output.prefix` have 1 to 1024 bytes, no control character, no backslash, no leading `/`, and no empty, `.` or `..` segment. `output.prefix` ends with `/`.
- `expect` is `photo`, `video`, or `any`. A file of the other kind is refused. The options of each kind that `expect` allows are mandatory.
- A rendition is written at `prefix` + `name` + `.jpg`. `name` matches `^[a-z0-9_]{1,32}$`, is used once, and is not `video` and does not start with `frame_`: those are for the video path. A rendition that would be written at the source key is an `InvalidRequestError`.
- `frames` is `{"count": N}` for N evenly spaced frames, or `{"every_seconds": S}`. Frame N is written at `prefix` + `frame_NN.jpg` (`frame_01.jpg`, and so on). A request whose source key is one of the frame keys that `HAZMAT_MAX_FRAMES` allows, in the same bucket, is an `InvalidRequestError`: the worker does not change the source.
- `video.remux` is reserved. This build accepts `false` and refuses `true`.

Every response has `v`, `status`, `worker` (`version`, `ffmpeg`, `config_hash`),
and `timings_ms` (`fetch`, `probe` for a video, `decode`, `encode`, `store`).
When the worker read the source, `source` holds `bytes`, `sha256`, and
`sniffed_type`. Each entry of `outputs` has `name`, `key`, `version_id`,
`bytes`, `sha256`, `width`, and `height`.

For a photo, `media` holds `kind`, `width` and `height` (after the EXIF
orientation), `had_metadata`, `had_gps`, and `animated`. For a video, `media`
holds `kind`, `container`, `codec`, `width`, `height` (as a player shows it),
`duration_s` (measured), `declared_duration_s`, `fps`, `rotation`, and
`has_audio`. Each frame in `outputs` also has `time_s`, `luma`, and
`sharpness`. `worker.ffmpeg` is the version of the decoder.

The results:

| Result | Meaning | The caller |
|---|---|---|
| `"status": "ok"` | The outputs exist. Each has its key, version, size, sha256, and dimensions. | Continues. |
| `"status": "refused"` with `refusal.code` | The file is the problem. The same file always gets the same answer. | Tells the user. Does not try again. |
| A function error, `StorageError` | The source is not there, or a read or a write failed. | Tries again later. |
| A function error, `InvalidRequestError` | The request is wrong: a bug in the caller. | Corrects the bug. |
| A function error, `WorkerError` | The worker itself failed: a work directory, a child process that did not start, an encoder. | Tries again later. |

A payload that is not JSON does not reach the worker: the Invoke API refuses it.
Any other function error (for example a Lambda timeout) is not one of these
types, and the caller tries again later.

Refusal codes: `unsupported_type`, `wrong_kind`, `too_many_bytes`, `too_many_pixels`, `undecodable`, and for a video `unsupported_container`, `unsupported_codec`, `bad_stream_layout`, `too_long`, `duration_mismatch`, `decode_timeout`.

`decode_timeout` is a refusal and not an error. The ceilings limit the work of a
correct file, so a file that passes them and does not finish is a hostile file.

## Configuration

The operator sets ceilings in environment variables. The worker reads them once
at start, and a value it cannot read stops the start. A request can ask for less
than a ceiling. A request that asks for more is an error; the worker never lowers
a value and continues. `config_hash` in each response identifies the ceilings
that applied.

| Variable | Default | Meaning |
|---|---|---|
| `HAZMAT_STORE` | `s3` | `s3`, or `file` for the command line and tests |
| `HAZMAT_FILE_ROOT` | none | The directory of the `file` store. Each bucket is a directory below it. |
| `HAZMAT_S3_PATH_STYLE` | `false` | `true` for an S3-compatible store that has no `bucket.host` names. AWS S3 does not need it. |
| `HAZMAT_ALLOWED_BUCKETS` | none | Buckets a request can name. Mandatory for `s3`. |
| `HAZMAT_INPUT_PREFIXES` | none | Key prefixes a request can read. Mandatory for `s3`. |
| `HAZMAT_OUTPUT_PREFIXES` | none | Key prefixes a request can write. Mandatory for `s3`. |
| `HAZMAT_PHOTO_TYPES` | `jpeg,png,webp` | Photo types the worker decodes |
| `HAZMAT_MAX_PHOTO_BYTES` | `15MiB` | |
| `HAZMAT_MAX_PHOTO_PIXELS` | `50000000` | Read from the header, before the decode |
| `HAZMAT_MAX_OUTPUT_EDGE` | `4096` | The largest `max_edge` and `frame_edge` a request can ask for. At most `65535`, the largest edge of a JPEG. |
| `HAZMAT_MAX_RENDITIONS` | `4` | |
| `HAZMAT_VIDEO_CONTAINERS` | `mp4,mov` | From the brand of the file |
| `HAZMAT_VIDEO_CODECS` | `h264` | The only decoder in the ffmpeg of the release |
| `HAZMAT_MAX_VIDEO_BYTES` | `50MiB` | |
| `HAZMAT_MAX_VIDEO_EDGE` | `1920` | The long edge of the source video |
| `HAZMAT_MAX_VIDEO_FPS` | `60` | Measured from the packets |
| `HAZMAT_MAX_VIDEO_SECONDS` | `60` | The largest `max_seconds` a request can ask for. A whole number. |
| `HAZMAT_MAX_FRAMES` | `32` | |
| `HAZMAT_DECODE_TIMEOUT_SECONDS` | `20` | For all child processes of one video together. A whole number. Keep it below the timeout of the function. |
| `HAZMAT_DECODE_MEMORY` | `1024MiB` | The address space of a child process |
| `HAZMAT_RECYCLE` | `on_error` | `never`, `on_error`, or `always`: when to end the execution environment after a video. See "The child process". |
| `HAZMAT_FFMPEG_DIR` | `bin` beside the worker | Where `ffmpeg` and `ffprobe` are. With no variable and no programs there, every video is refused. |
| `HAZMAT_LOG_LEVEL` | `info` | |

## Use

```sh
make test     # gofmt + vet + tests
make fuzz     # a short run of each fuzz target
make bench    # one 12-megapixel photo, two renditions
make build    # bin/hazmat
make ffmpeg   # dist/ffmpeg/{ffmpeg,ffprobe} from the pinned source (needs Docker)
make dist     # dist/hazmat_<version>_lambda_arm64.zip + checksums.txt (needs an arm64 Docker host)
make local-test  # the binary and ffmpeg in the Lambda runtime image, against a local S3 API (needs Docker)

# One file, with no cloud account: the source is at ./work/cdn/uploads/m1/source
bin/hazmat run --request request.json --root ./work
```

The response is JSON on stdout. One JSON log line for each request goes to
stderr. It holds facts about the file, and never bytes of the file, EXIF
content, or a position.

## Deploy on AWS Lambda

1. Download the zip and `checksums.txt` of a release, and check the sha256.
2. Make a function with these settings:

   | Setting | Value |
   |---|---|
   | Runtime | `provided.al2023` |
   | Architecture | `arm64` |
   | Handler | `bootstrap` (the runtime does not use it) |
   | Memory | 3008 MB is a good start for 12-megapixel photos and video. Lambda gives CPU in proportion to memory. |
   | Timeout | At least 5 seconds more than `HAZMAT_DECODE_TIMEOUT_SECONDS` (30 s for the default of 20 s) |
   | Reserved concurrency | A limit, so that a burst of uploads cannot use all the concurrency of the account |

3. Set `HAZMAT_ALLOWED_BUCKETS`, `HAZMAT_INPUT_PREFIXES`, and `HAZMAT_OUTPUT_PREFIXES`,
   and lower the other ceilings to what your app needs.
4. Turn on versioning for the bucket. A request must give `source.version_id`,
   so that a second upload to the same key cannot change what one request reads.
5. Give the function role only what it needs. It does not need to list or
   delete:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject"],
         "Resource": "arn:aws:s3:::YOUR-BUCKET/uploads/*"
       },
       {
         "Effect": "Allow",
         "Action": ["logs:CreateLogStream", "logs:PutLogEvents"],
         "Resource": "arn:aws:logs:REGION:ACCOUNT:log-group:/aws/lambda/YOUR-FUNCTION:*"
       }
     ]
   }
   ```

6. Invoke the function synchronously (`RequestResponse`) with the request JSON.
   Read the result as the table in "Request and response" says: a refusal is a
   normal response, and a function error has the Go type name as its
   `errorType`.

The function only reads and writes objects. Your service decides what to do
with the outputs and with a refusal.

## Releases

Every push to `main` that passes CI is a release: `vMAJOR.MINOR.PATCH`, with
`MAJOR.MINOR` from the `VERSION` file. The release holds the Lambda zip for the
`provided.al2023` runtime on arm64, `checksums.txt`, and the FFmpeg source that
the zip was built from. A deployment pins the tag and the sha256.

Releases run one at a time, and GitHub keeps only one waiting: when three
pushes come close together, the middle one can be skipped, and the next release
holds its change. A run on a commit older than the newest release fails and
makes no release.

The zip holds `bootstrap` (the worker), `bin/ffmpeg`, `bin/ffprobe`,
`bin/ffmpeg-buildconf.txt`, `bin/COPYING.LGPLv2.1`, `LICENSE`, `NOTICE`, and
`licenses/` (the licenses of the Go modules in `bootstrap`).

## ffmpeg

`ffmpeg/Dockerfile` builds ffmpeg and ffprobe from a source release that is
pinned by version and sha256, with `--disable-everything` and then only: the
`mov` demuxer, the `h264` decoder, the `mjpeg` encoder, the `image2` muxer, the
`file` protocol, and eight filters. The network is off, there is no outside
library, and the build is LGPL. The link is static, so the two programs run on
the Lambda runtime as they are.

To move to a new ffmpeg release, change the two pins in the Dockerfile and run
`make local-test`. A security release of ffmpeg makes a release of the worker.

## Security

See [SECURITY.md](SECURITY.md) to report a vulnerability. Do not open a public
issue for one.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). The ffmpeg and
ffprobe in the release zip are FFmpeg, under the LGPL 2.1 or later; NOTICE
tells where the license and the source are.
