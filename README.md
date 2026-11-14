# hazmat

**The Lambda that handles your users' uploads so your API doesn't have to.**

You give hazmat one file that a user uploaded. It either refuses the file, or it
writes brand-new JPEGs built from the file's pixels, with all the metadata
stripped out, and tells you some facts about what it saw. That's the whole job.
It doesn't judge the content (no moderation, no "is this safe?") and it talks to
nothing except your storage bucket.

The same Go binary works as the AWS Lambda handler and as a command line tool,
so you can try it on your laptop with no cloud account. It's written in Go, runs
on the `provided.al2023` Lambda runtime on arm64, and ships with a minimal,
statically linked ffmpeg for video.

**Status:** photos and videos both work. For video, it handles H.264 in MP4 or
QuickTime (`.mov`) files. The bundled ffmpeg has no other decoder, so other
codecs (HEVC, for example) are refused.

## Why does this exist?

If your app accepts photos and videos, your servers have to decode bytes that
anyone on the internet can craft. Image and video decoders are a classic place
for security bugs. If that decoding happens inside your main API, a decoder bug
is a bug in the service that holds your database and your secrets.

There's a privacy problem too. A photo stored exactly as it was uploaded keeps
its EXIF data, and EXIF often includes the GPS coordinates of where the photo
was taken, which is often the user's home.

hazmat moves all of that risk into one small function that can only touch the
upload bucket. A few principles guide it:

- **It fails closed.** If it can't fully decode a file, it refuses it. It never
  shrugs and passes the original through.
- **Nothing from the original survives.** Every output is encoded fresh from
  decoded pixels, so no byte of the uploaded file ends up in an output.
- **It checks limits before doing work.** File size is checked before the
  download, image dimensions before the decode, and so on. A hostile file
  can't make it do a lot of work before it notices.
- **Photos are pure Go.** JPEG, PNG, and WebP are decoded with the Go standard
  library and `golang.org/x/image`. No C code, no child processes.
- **Video uses a stripped-down ffmpeg.** It's built from pinned source with
  exactly one container reader, one decoder, no network support, and no
  outside libraries. Less code means less to attack.

## Quick start

You need Go (see [Prerequisites](#prerequisites)). This processes one photo
using local files, with no cloud account.

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

`--root` switches hazmat to its local `file` store, where each bucket is a
directory below the root. The response prints to stdout, and the cleaned photo
lands at `work/cdn/uploads/m1/image.jpg`. Both `work/` and `request.json` are in
`.gitignore`, so you won't accidentally commit a photo that has your location
in it.

To try a video, you also need ffmpeg and ffprobe. Run `make ffmpeg` (needs
Docker) and set `HAZMAT_FFMPEG_DIR=dist/ffmpeg`. Without them, hazmat still
runs, but it refuses every video as `unsupported_type`.

## Layout

| Path | What it is |
|---|---|
| `cmd/hazmat/` | `main`: the Lambda handler when `AWS_LAMBDA_RUNTIME_API` is set, otherwise the CLI (`run`, `config`, `version`) |
| `internal/contract/` | The request and response types, the refusal codes, strict decoding, and validation against the limits |
| `internal/config/` | Reads the `HAZMAT_*` limits and computes `config_hash` |
| `internal/sniff/` | Works out the file type from its first bytes |
| `internal/photo/` | Photo decode, rotation, color profile copy, resize, and JPEG encode. `meta.go` reads the metadata around the pixels. |
| `internal/video/` | The ffprobe header and packet reads, the frame sampling plan, the one ffmpeg pass, and the frame scores |
| `internal/sandbox/` | Runs ffmpeg and ffprobe as boxed-in child processes |
| `internal/store/` | The storage interface, with an S3 and a local file implementation |
| `internal/worker/` | One request from start to finish, and its one log line |
| `ffmpeg/Dockerfile` | Builds the pinned, minimal ffmpeg and ffprobe |
| `test/local/` | Integration tests in the real Lambda runtime image against a local S3 API. `testdata/` holds small video fixtures. |
| `examples/terraform-lambda/` | A Terraform deploy of a release zip |
| `examples/python-caller/` | The API side: presigned upload, invoke, and result handling |
| `scripts/` | `next-version.sh` (picks the release tag) and `collect-licenses.sh` (the Go module licenses for the zip) |
| `VERSION` | The `MAJOR.MINOR` of the next release |

## Prerequisites

- **Go**, at the version in `go.mod` (currently 1.27.3) or newer. That's all you
  need for `make build`, `make test`, `make fuzz`, and `make bench`.
- **Docker**, for `make ffmpeg`, `make local-test`, `make dist`, and
  `test/local/make-fixtures.sh`. `make dist` also needs an arm64 Docker host
  (the release is arm64 only), plus `zip` and `shasum`.
- **No cloud account** for development. The local tests use S3Mock in place of
  AWS S3.
- To try the Terraform example: Terraform 1.5 or newer and AWS credentials. See
  [`examples/terraform-lambda`](examples/terraform-lambda).

## Develop

| Target | Does |
|---|---|
| `make test` | `gofmt` check, `go vet`, and the unit tests |
| `make fuzz` | A short run of each fuzz target (20 s each; set `FUZZTIME` to change it) |
| `make bench` | Benchmarks the photo path: one 12-megapixel photo, two renditions |
| `make build` | Builds `bin/hazmat` for your machine |
| `make ffmpeg` | Builds `dist/ffmpeg/{ffmpeg,ffprobe}` from the pinned source for the Docker host's architecture (needs Docker). It's skipped when `ffmpeg/Dockerfile` hasn't changed. |
| `make local-test` | Runs the binary and ffmpeg inside the Lambda runtime image against S3Mock (needs Docker) |
| `make dist` | Builds `dist/hazmat_<version>_lambda_arm64.zip` and `dist/checksums.txt` (needs an arm64 Docker host) |

```sh
# Process one file locally. The source is at ./work/cdn/uploads/m1/source
bin/hazmat run --request request.json --root ./work

# Print the limits in effect and their config_hash. Without --root, the
# default store is s3, so set its variables or HAZMAT_STORE=file first.
HAZMAT_STORE=file HAZMAT_FILE_ROOT=./work bin/hazmat config

# Print the hazmat version, and the ffmpeg version if it finds ffmpeg
bin/hazmat version
```

The response is JSON on stdout. The exit status is 0 for a response (`ok` or
`refused`) and 1 for an error. Each request also writes one JSON log line to
stderr. Logs contain facts about the file only, never its bytes, EXIF values,
or a location.

A few things worth knowing about the tests:

- `make local-test` is the only test of the Lambda entry point, of the S3 store
  against a real S3 API with object versions, and of the video path with the
  real ffmpeg. The unit tests of `internal/video` use small shell scripts in
  place of ffmpeg and ffprobe, so they run without ffmpeg.
- The local S3 API is S3Mock because the MinIO and LocalStack images are no
  longer free to pull.
- The video fixtures in `test/local/testdata/` are committed. The worker's own
  ffmpeg can't make them (it has no video encoder), so
  `test/local/make-fixtures.sh` makes them again with a full ffmpeg from a
  container image. Run it only to change them.

## Configuration

You set the limits with environment variables. hazmat reads them once at
startup, and if it can't parse a value, it won't start. A request can ask for
less than a limit but never more. Asking for more is an `InvalidRequestError`,
not a silent cap. The `config_hash` in each response tells you which set of
limits was in effect.

Byte sizes are a plain number of bytes or a number with `KiB`, `MiB`, or `GiB`
(for example `15MiB`). Every number must be a positive whole number.

| Variable | Default | Meaning |
|---|---|---|
| `HAZMAT_STORE` | `s3` | `s3`, or `file` for the command line and tests |
| `HAZMAT_FILE_ROOT` | none | The root directory for the `file` store. Each bucket is a subdirectory. Required for `file`. |
| `HAZMAT_S3_PATH_STYLE` | `false` | `true` for S3-compatible stores that don't support `bucket.host` names. Not needed for AWS S3. |
| `HAZMAT_ALLOWED_BUCKETS` | none | Comma-separated buckets a request may name. Required for `s3`. |
| `HAZMAT_INPUT_PREFIXES` | none | Comma-separated key prefixes a request may read from. Required for `s3`. |
| `HAZMAT_OUTPUT_PREFIXES` | none | Comma-separated key prefixes a request may write to. Required for `s3`. |
| `HAZMAT_PHOTO_TYPES` | `jpeg,png,webp` | Photo formats to accept. Only these three can be decoded. |
| `HAZMAT_MAX_PHOTO_BYTES` | `15MiB` | |
| `HAZMAT_MAX_PHOTO_PIXELS` | `50000000` | Read from the header, before decoding |
| `HAZMAT_MAX_OUTPUT_EDGE` | `4096` | The largest `max_edge` or `frame_edge` a request can ask for. At most `65535`, the largest edge a JPEG can have. |
| `HAZMAT_MAX_RENDITIONS` | `4` | |
| `HAZMAT_VIDEO_CONTAINERS` | `mp4,mov` | Detected from the file's first bytes. Only these two can be read. |
| `HAZMAT_VIDEO_CODECS` | `h264` | The only decoder in the bundled ffmpeg |
| `HAZMAT_MAX_VIDEO_BYTES` | `50MiB` | |
| `HAZMAT_MAX_VIDEO_EDGE` | `1920` | The long edge of the source video |
| `HAZMAT_MAX_VIDEO_FPS` | `60` | Measured from the packets |
| `HAZMAT_MAX_VIDEO_SECONDS` | `60` | The largest `max_seconds` a request can ask for |
| `HAZMAT_MAX_FRAMES` | `32` | |
| `HAZMAT_DECODE_TIMEOUT_SECONDS` | `20` | Total time for all child processes of one video. Keep it below the function timeout. |
| `HAZMAT_DECODE_MEMORY` | `1024MiB` | The address-space limit of each child process |
| `HAZMAT_RECYCLE` | `on_error` | `never`, `on_error`, or `always`. See "How the child process is boxed in". |
| `HAZMAT_FFMPEG_DIR` | `bin` next to the binary | Where `ffmpeg` and `ffprobe` live. If unset and they aren't there, every video is refused. If set and they aren't there, hazmat won't start. |
| `HAZMAT_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

On the `file` store, `HAZMAT_INPUT_PREFIXES` and `HAZMAT_OUTPUT_PREFIXES` are
optional, but they're still checked when you set them.

## What happens to a photo

1. **Size check, before downloading.** hazmat asks storage how big the file is.
   It doesn't know yet whether the file is a photo or a video, so if the file is
   over the larger of the two limits (50 MiB by default, or the photo limit of
   15 MiB when there's no ffmpeg), it's refused without being downloaded. The
   download itself stops one byte past that limit, in case storage got the size
   wrong.

2. **Figure out what it really is.** The filename and the `Content-Type` header
   are chosen by the uploader, so hazmat ignores them. It looks at the file's
   first bytes (its "magic number") to tell whether it's really a JPEG, PNG, or
   WebP. Now that the kind is known, the photo limit applies (15 MiB by
   default). HEIC/HEIF, AVIF, and GIF are recognized but can't be decoded, so
   they're refused as `unsupported_type`.

3. **Dimension check, before decoding.** A "decompression bomb" is a tiny file
   that claims to be enormous, say 100,000 x 100,000 pixels, and would eat
   gigabytes of memory when decoded. The width and height are in the file
   header, so hazmat reads them first and refuses anything over the pixel limit
   (50 megapixels by default).

4. **Decode the pixels** with Go's decoders. There's one extra guard here for
   progressive JPEGs, which load in passes ("scans") from blurry to sharp. Each
   scan makes the decoder redraw the whole picture, and a scan can be just a few
   bytes, so an attacker could pack thousands of them into a small file and keep
   the CPU busy for a long time. Real encoders make about 10 scans, so a JPEG
   with more than 100 is refused as `undecodable`.

5. **Fix the rotation.** Phones often save photos sideways plus an EXIF note
   that says "rotate this when you show it". Since hazmat throws away EXIF (step
   7), it rotates the pixels itself first. Otherwise your users' photos would
   show up sideways.

6. **Write the sizes you asked for.** You list one or more "renditions" in the
   request (say, a 2048px version and a 400px thumbnail), and hazmat writes a
   fresh JPEG for each one. It only ever scales down: if the original is smaller
   than a rendition's size, it stays at its original size. For an animated
   PNG, only its default image is used. That's usually the first frame, but an
   animated PNG can also hold a separate still image that isn't part of the
   animation, and then that still image is what you get. Go's WebP decoder
   can't read animated WebP, so an animated WebP is refused as `undecodable`.

7. **Keep the color profile, drop everything else.** The only thing carried
   over from the original is the RGB color profile (ICC). It tells screens how
   to show the colors correctly. Without it, photos from newer phones, which
   use a wider color range, can look washed out. Everything else is dropped:
   EXIF, GPS location, camera details, comments.

   The color profile also came from the uploader, so it's checked before it's
   copied. It must be a well-formed RGB ICC profile:

   - a header with a valid profile class (input, display, output, or color
     space), an RGB color space, and an XYZ or Lab connection space
   - at most 64 tags, each fully inside the profile
   - the tags an RGB profile needs (the three colorants and three curves, or
     `A2B0`)
   - at most 256 KiB, trimmed to the size its header declares

   If the profile fails any check, it's dropped and the photo still goes
   through. Worst case, the colors are a little off.

The response tells you whether the original had metadata and GPS
(`had_metadata`, `had_gps`), so you know what was stripped.

## What happens to a video

hazmat doesn't re-encode videos. It pulls out still frames as JPEGs, which you
can use for thumbnails, a poster image, or to send to a moderation service. The
original video file is left untouched.

ffprobe and ffmpeg do the reading, run as child processes. The key rule:
**hazmat trusts only what it measures, never what the file claims about
itself.**

1. **Check the container.** It reads the container type from the first bytes.
   Only MP4 and QuickTime are recognized as video; anything else is
   `unsupported_type`. A recognized container that `HAZMAT_VIDEO_CONTAINERS`
   doesn't allow is `unsupported_container`. The size check works the same way
   as for photos, with the video limit (50 MiB by default).

2. **Read the header with ffprobe.** It refuses the file if it doesn't have
   exactly one video stream (`bad_stream_layout`), uses a codec that isn't
   allowed (`unsupported_codec`), has frames whose long edge is too big
   (`too_many_pixels`), has a rotation that isn't a multiple of 90 degrees
   (`bad_stream_layout`), or claims a duration over the limit (`too_long`).

3. **Measure the real duration.** A file's header can lie about how long it is.
   So hazmat reads the timestamp of every video packet with ffprobe (without
   decoding anything) and computes the real duration and frame rate from those.
   It refuses the file if it's actually too long (`too_long`), if the header
   disagrees with the packets by more than half a second
   (`duration_mismatch`), or if the frame rate is too high
   (`bad_stream_layout`).

4. **Grab the frames in one ffmpeg pass.** The video is split into equal
   intervals, and hazmat takes the first frame at or after the middle of each
   one. Each frame is rotated the way a player would show it, scaled down to
   `frame_edge`, and saved as a JPEG.

5. **Check the output.** Every planned frame must exist, no more and no fewer,
   and each must be a JPEG no larger than `frame_edge`. If ffmpeg could only
   decode part of the file, it's refused (`undecodable`).

6. **Describe each frame.** Each frame comes back with its timestamp, its
   average brightness (`luma`, 0 to 255), and how sharp it is (`sharpness`,
   near 0 for a flat or blurred frame), so you can skip black frames and pick a
   good poster image. The timestamp is the one ffmpeg measured, read from the
   first 64 KiB of ffmpeg's log. If any frame's timestamp is missing, the file
   is refused (`undecodable`). This can happen, for example, with a file
   stuffed with so many tags that ffmpeg prints more than 64 KiB about them
   first.

hazmat doesn't read the audio. It just tells you whether there is any
(`has_audio`).

### How the child process is boxed in

ffmpeg and ffprobe run with a fixed argument list, an empty environment, their
own process group, and a time limit. On Linux (which includes Lambda) they also
get limits on memory, CPU time, and file size, and no core dumps. They write
into a fresh directory that hazmat deletes afterward.

To be clear, this is **not** a security boundary. The child runs as the same
user as hazmat, so the real limit is the IAM role of the function, which is why
that role should be as small as possible (see the deploy section below).

Because a misbehaving child might leave the environment in a bad state, hazmat
can throw away its Lambda execution environment after a video. This is set with
`HAZMAT_RECYCLE`:

- `on_error` (the default): recycle after a child that didn't exit on its own.
  That means it timed out, was killed by a signal, or produced endless output
  and had to be stopped.
- `always`: recycle after every video, refused or not. Never after a photo.
- `never`: don't recycle.

On Lambda, "recycle" means the process exits 100 ms after it has its result,
and Lambda starts a fresh environment for the next request. There are two rare
ways this can cost a request, and in both the caller just retries:

- Lambda freezes the environment before the exit happens. The exit then happens
  when the next request arrives, and that request fails.
- The runtime takes more than 100 ms to send the response (it usually takes
  about 1 ms). The response is lost and the invocation shows up as a function
  error.

## Request and response

Here's a full request:

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

The rules:

- `v` is the contract version, currently `1`. Unknown fields are rejected with
  `InvalidRequestError`, so typos don't silently do nothing.
- `request_id` must match `^[A-Za-z0-9._:-]{1,128}$`.
- `source.key` and `output.prefix` are 1 to 1024 bytes, with no control
  characters, no backslashes, no leading `/`, and no empty, `.`, or `..` path
  segments. `output.prefix` must end with `/`.
- With the `s3` store, `source.version_id` is required, and both buckets must
  be in `HAZMAT_ALLOWED_BUCKETS`. The source key and the output prefix must
  start with one of the allowed input and output prefixes.
- `expect` is `photo`, `video`, or `any`. If the file turns out to be the other
  kind, it's refused. Whichever kinds `expect` allows, you must include their
  options (`photo`, `video`, or both).
- Each rendition is written to `prefix` + `name` + `.jpg`. `name` must match
  `^[a-z0-9_]{1,32}$` and be unique. It can't be `video` or start with
  `frame_`, because those are reserved for the video path. `max_edge` is 16 up
  to `HAZMAT_MAX_OUTPUT_EDGE`, and `quality` is 1 to 100. A rendition that
  would overwrite the source file is an `InvalidRequestError`.
- `video.max_seconds` is the longest video you'll accept, above 0 and at most
  `HAZMAT_MAX_VIDEO_SECONDS`. `frame_edge` is 16 up to
  `HAZMAT_MAX_OUTPUT_EDGE`, and `quality` is 1 to 100.
- `frames` is either `{"count": N}` for N evenly spaced frames (1 to
  `HAZMAT_MAX_FRAMES`), or `{"every_seconds": S}`, where a video of
  `max_seconds` must not need more than `HAZMAT_MAX_FRAMES` frames. Frames are
  written to `prefix` + `frame_NN.jpg` (`frame_01.jpg`, `frame_02.jpg`, and so
  on). If the source key could be overwritten by one of the frame files (up to
  `HAZMAT_MAX_FRAMES`) in the same bucket, the request is an
  `InvalidRequestError`. hazmat never modifies the source.
- `video.remux` is reserved for later. For now, `false` is accepted and `true`
  is refused.

### What comes back

Every response includes `v`, `status`, `worker` (`version`, `ffmpeg`,
`config_hash`), and `timings_ms` (`fetch`, `probe` for videos, `decode`,
`encode`, `store`).

If hazmat got as far as reading the file, `source` has its `bytes`, `sha256`,
and `sniffed_type` (a MIME type such as `image/jpeg`). Each entry in `outputs`
has `name`, `key`, `version_id`, `bytes`, `sha256`, `width`, and `height`.

For a **photo**, `media` has `kind`, `width` and `height` (after rotation),
`had_metadata`, `had_gps`, and `animated`.

For a **video**, `media` has `kind`, `container`, `codec`, `width` and `height`
(as a player would show it), `duration_s` (measured), `declared_duration_s`
(what the file claimed), `fps`, `rotation`, and `has_audio`. Each frame in
`outputs` also has `time_s`, `luma`, and `sharpness`. `worker.ffmpeg` is the
ffmpeg version used. It's left out when hazmat runs without ffmpeg.

### How to handle each result

| Result | What it means | What your service should do |
|---|---|---|
| `"status": "ok"` | The outputs were written. Each one has its key, version, size, sha256, and dimensions. | Carry on. |
| `"status": "refused"` with `refusal.code` | The file is the problem. The same file always gets the same answer. | Tell the user. Don't retry. |
| Function error `StorageError` | The source is missing, or a read or write failed. | Retry later. |
| Function error `InvalidRequestError` | The request is malformed. That's a bug in your code. | Fix the bug. |
| Function error `WorkerError` | hazmat itself failed: a work directory, a child process that didn't start, an encoder. | Retry later. |

A payload that isn't JSON never reaches hazmat, because the Lambda Invoke API
rejects it. Any other function error (a Lambda timeout, for example) isn't one
of the types above. Treat it as temporary and retry later.

Refusal codes: `unsupported_type`, `wrong_kind`, `too_many_bytes`,
`too_many_pixels`, `undecodable`, and for videos also `unsupported_container`,
`unsupported_codec`, `bad_stream_layout`, `too_long`, `duration_mismatch`, and
`decode_timeout`. A refusal also has a `detail`, a short sentence for a person
to read.

Why is `decode_timeout` a refusal and not an error? The limits are sized so a
legitimate file finishes well in time. A file that passes every limit and still
doesn't finish is a hostile file, and you don't want to keep retrying it.

## Deploying on AWS Lambda

Using Terraform? [`examples/terraform-lambda`](examples/terraform-lambda)
sets up everything below for you: the bucket, the function, the role, and the
log group. [`examples/python-caller`](examples/python-caller) shows the other
half: the code in your API that takes an upload, calls hazmat, and handles each
result.

1. Download the zip and `checksums.txt` from a release, and verify the sha256.

2. Create a function with these settings:

   | Setting | Value |
   |---|---|
   | Runtime | `provided.al2023` |
   | Architecture | `arm64` |
   | Handler | `bootstrap` (the runtime ignores it) |
   | Memory | 3008 MB is a good starting point for 12-megapixel photos and video. Lambda gives CPU in proportion to memory. |
   | Timeout | At least 5 seconds more than `HAZMAT_DECODE_TIMEOUT_SECONDS` (30 s for the default of 20 s) |
   | Reserved concurrency | Set a cap, so a burst of uploads can't use up all of your account's concurrency |

3. Set `HAZMAT_ALLOWED_BUCKETS`, `HAZMAT_INPUT_PREFIXES`, and
   `HAZMAT_OUTPUT_PREFIXES`, and lower the other limits to what your app
   actually needs. If the configuration isn't valid, the function logs why and
   exits at startup, so every invocation fails until you fix it.

4. Turn on versioning for the bucket. Every request must include
   `source.version_id`, so a second upload to the same key can't swap the file
   out from under a request that's already running.

5. Give the function role only what it needs. It never lists or deletes:

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

6. Invoke the function synchronously (`RequestResponse`) with the request JSON,
   and handle the result as described in "How to handle each result". A refusal
   is a normal response, and a function error has the Go type name as its
   `errorType`.

hazmat only reads and writes objects. What to do with the outputs, or with a
refusal, is up to your service.

## About the bundled ffmpeg

`ffmpeg/Dockerfile` builds ffmpeg and ffprobe from a source release pinned by
version and sha256 (currently FFmpeg 8.1.3). It starts from
`--disable-everything` and turns on only what hazmat needs: the `mov` demuxer,
the `h264` decoder and parser, the `mjpeg` encoder, the `image2` muxer, the
`file` protocol, and eight filters. Networking is off, there are no outside
libraries (not even zlib, so a video with a compressed index is refused), and
the build is LGPL. It's statically linked, so both programs run on the Lambda
runtime as is. The Dockerfile says why each component is on.

To move to a new ffmpeg release, update the two pins (`FFMPEG_VERSION` and
`FFMPEG_SHA256`) in the Dockerfile and run `make ffmpeg local-test`. Each ffmpeg
security release gets a new hazmat release.

## CI/CD

| Trigger | Workflow | What it does |
|---|---|---|
| Pull request, push to `main` | `ci.yml` | `test` job: `make test`, `make fuzz FUZZTIME=30s`, `make local-test`, and `make dist`. On a push to `main`, the `release` job then tags and publishes a release (see [Release](#release)). |
| Pull request into `main` or `dev` opened or marked ready, `/ai-review` comment | `ai-review.yml` | A Codex review of the change for correctness and security faults, posted as inline review comments. Comment `/ai-review` to run it again after a push. |

- Both CI jobs run on GitHub's arm64 runners (`ubuntu-24.04-arm`), because the
  zip holds ffmpeg built for the runner's architecture. The ffmpeg build is
  cached on the hash of `ffmpeg/Dockerfile`, so it's only rebuilt when the pins
  or the configure line change.
- The repository holds no AWS credentials. CI only builds and publishes to
  GitHub Releases. Deploying is up to you.
- The AI review reads `CLAUDE.md` and `.github/ai-review-focus.md` for this
  repository's rules. It skips drafts, pull requests opened by bots, and pull
  requests from forks (a `/ai-review` on a fork would hand the API key to a job
  that reads the fork's code). Only people with write access can trigger it
  with `/ai-review`. It needs the `OPENAI_API_KEY` secret and skips when it
  isn't set.
- Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules and
  GitHub Actions, with minor and patch updates grouped.

## Release

Every push to `main` that passes CI becomes a release, tagged
`vMAJOR.MINOR.PATCH`. `MAJOR.MINOR` comes from the `VERSION` file, and `PATCH`
counts up from the last tag in that series (`scripts/next-version.sh`). Bump
`VERSION` for a contract change. It can't go backward: a `VERSION` behind the
newest release fails the release.

Each release contains the Lambda zip for `provided.al2023` on arm64,
`checksums.txt`, and the FFmpeg source the zip was built from. Deployments
should pin both the tag and the sha256.

Releases run one at a time, and GitHub keeps only one waiting. So if three
pushes land close together, the middle one may be skipped, and its change ships
in the next release. A run on a commit older than the newest release fails
without releasing anything, and a run on a commit that's already tagged does
nothing.

The zip contains `bootstrap` (hazmat itself), `bin/ffmpeg`, `bin/ffprobe`,
`bin/ffmpeg-buildconf.txt`, `bin/COPYING.LGPLv2.1`, `LICENSE`, `NOTICE`, and
`licenses/` (the licenses of Go and of the Go modules compiled into
`bootstrap`, gathered by `scripts/collect-licenses.sh`). Together with the
FFmpeg source on each release, that's how the zip meets the LGPL and the Go
module licenses, so keep it all when changing `make dist` or the release step.

## Related

- [`CLAUDE.md`](CLAUDE.md): the rules for contributors and coding agents (what
  the worker must never do, and why).
- [`SECURITY.md`](SECURITY.md): how to report a vulnerability, and what's in
  scope.
- [`examples/terraform-lambda`](examples/terraform-lambda) and
  [`examples/python-caller`](examples/python-caller): a deploy and a caller.
- [FFmpeg](https://ffmpeg.org): the project the bundled decoder comes from.
  Report FFmpeg bugs there.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for how to report it.
Please don't open a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). The ffmpeg and
ffprobe binaries in the release zip are FFmpeg, licensed under the LGPL 2.1 or
later. NOTICE explains where to find that license and the source.
