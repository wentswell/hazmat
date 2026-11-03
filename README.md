# hazmat

**The Lambda that handles your users' uploads so your API doesn't have to.**

You give hazmat one file that a user uploaded. It either refuses the file, or it
writes brand-new JPEGs built from the file's pixels, with all the metadata
stripped out, and tells you some facts about what it saw. That's the whole job.
It doesn't judge the content (no moderation, no "is this safe?") and it talks to
nothing except your storage bucket.

The same Go binary works as the AWS Lambda handler and as a command line tool,
so you can try it on your laptop with no cloud account.

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

You need Go. This processes one photo using local files, with no cloud account.

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

The response prints to stdout, and the cleaned photo lands at
`work/cdn/uploads/m1/image.jpg`. Both `work/` and `request.json` are in
`.gitignore`, so you won't accidentally commit a photo that has your location
in it.

To try a video, you also need ffmpeg and ffprobe. Run `make ffmpeg` (needs
Docker) and set `HAZMAT_FFMPEG_DIR=dist/ffmpeg`.

## What happens to a photo

1. **Size check, before downloading.** hazmat asks storage how big the file is.
   If it's over the limit (15 MiB by default), it's refused without being
   downloaded.

2. **Figure out what it really is.** The filename and the `Content-Type` header
   are chosen by the uploader, so hazmat ignores them. It looks at the file's
   first bytes (its "magic number") to tell whether it's really a JPEG, PNG, or
   WebP.

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
   than a rendition's size, it stays at its original size.

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

1. **Check the container.** It reads the container type from the first bytes
   and refuses anything that isn't allowed (`unsupported_container`).

2. **Read the header with ffprobe.** It refuses the file if it doesn't have
   exactly one video stream (`bad_stream_layout`), uses a codec that isn't
   allowed (`unsupported_codec`), has frames that are too big
   (`too_many_pixels`), or claims a duration over the limit (`too_long`).

3. **Measure the real duration.** A file's header can lie about how long it is.
   So hazmat reads the timestamp of every video packet with ffprobe (without
   decoding anything) and computes the real duration and frame rate from those.
   It refuses the file if it's actually too long (`too_long`), if the header
   disagrees with the packets (`duration_mismatch`), or if the frame rate is
   too high (`bad_stream_layout`).

4. **Grab the frames in one ffmpeg pass.** The video is split into equal
   intervals, and hazmat takes the first frame at or after the middle of each
   one. Each frame is rotated the way a player would show it, scaled down to
   `frame_edge`, and saved as a JPEG.

5. **Check the output.** Every planned frame must exist, no more and no fewer,
   and each must be a JPEG no larger than `frame_edge`. If ffmpeg could only
   decode part of the file, it's refused (`undecodable`).

6. **Describe each frame.** Each frame comes back with its timestamp, its
   average brightness (`luma`), and how sharp it is (`sharpness`), so you can
   skip black frames and pick a good poster image. The timestamp is the one
   ffmpeg measured, read from the first 64 KiB of ffmpeg's log. If any frame's
   timestamp is missing, the file is refused (`undecodable`). This can happen,
   for example, with a file stuffed with so many tags that ffmpeg prints more
   than 64 KiB about them first.

hazmat doesn't read the audio. It just tells you whether there is any
(`has_audio`).

### How the child process is boxed in

ffmpeg and ffprobe run with a fixed argument list, an empty environment, their
own process group, a time limit, and limits on memory, CPU time, and file size.
They write into a fresh directory that hazmat deletes afterward.

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
- `expect` is `photo`, `video`, or `any`. If the file turns out to be the other
  kind, it's refused. Whichever kinds `expect` allows, you must include their
  options (`photo`, `video`, or both).
- Each rendition is written to `prefix` + `name` + `.jpg`. `name` must match
  `^[a-z0-9_]{1,32}$` and be unique. It can't be `video` or start with
  `frame_`, because those are reserved for the video path. A rendition that
  would overwrite the source file is an `InvalidRequestError`.
- `frames` is either `{"count": N}` for N evenly spaced frames, or
  `{"every_seconds": S}`. Frames are written to `prefix` + `frame_NN.jpg`
  (`frame_01.jpg`, `frame_02.jpg`, and so on). If the source key could be
  overwritten by one of the frame files (up to `HAZMAT_MAX_FRAMES`) in the same
  bucket, the request is an `InvalidRequestError`. hazmat never modifies the
  source.
- `video.remux` is reserved for later. For now, `false` is accepted and `true`
  is refused.

### What comes back

Every response includes `v`, `status`, `worker` (`version`, `ffmpeg`,
`config_hash`), and `timings_ms` (`fetch`, `probe` for videos, `decode`,
`encode`, `store`).

If hazmat got as far as reading the file, `source` has its `bytes`, `sha256`,
and `sniffed_type`. Each entry in `outputs` has `name`, `key`, `version_id`,
`bytes`, `sha256`, `width`, and `height`.

For a **photo**, `media` has `kind`, `width` and `height` (after rotation),
`had_metadata`, `had_gps`, and `animated`.

For a **video**, `media` has `kind`, `container`, `codec`, `width` and `height`
(as a player would show it), `duration_s` (measured), `declared_duration_s`
(what the file claimed), `fps`, `rotation`, and `has_audio`. Each frame in
`outputs` also has `time_s`, `luma`, and `sharpness`. `worker.ffmpeg` is the
ffmpeg version used.

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
`decode_timeout`.

Why is `decode_timeout` a refusal and not an error? The limits are sized so a
legitimate file finishes well in time. A file that passes every limit and still
doesn't finish is a hostile file, and you don't want to keep retrying it.

## Configuration

You set the limits with environment variables. hazmat reads them once at
startup, and if it can't parse a value, it won't start. A request can ask for
less than a limit but never more. Asking for more is an `InvalidRequestError`,
not a silent cap. The `config_hash` in each response tells you which set of
limits was in effect.

| Variable | Default | Meaning |
|---|---|---|
| `HAZMAT_STORE` | `s3` | `s3`, or `file` for the command line and tests |
| `HAZMAT_FILE_ROOT` | none | The root directory for the `file` store. Each bucket is a subdirectory. |
| `HAZMAT_S3_PATH_STYLE` | `false` | `true` for S3-compatible stores that don't support `bucket.host` names. Not needed for AWS S3. |
| `HAZMAT_ALLOWED_BUCKETS` | none | Buckets a request may name. Required for `s3`. |
| `HAZMAT_INPUT_PREFIXES` | none | Key prefixes a request may read from. Required for `s3`. |
| `HAZMAT_OUTPUT_PREFIXES` | none | Key prefixes a request may write to. Required for `s3`. |
| `HAZMAT_PHOTO_TYPES` | `jpeg,png,webp` | Photo formats to accept |
| `HAZMAT_MAX_PHOTO_BYTES` | `15MiB` | |
| `HAZMAT_MAX_PHOTO_PIXELS` | `50000000` | Read from the header, before decoding |
| `HAZMAT_MAX_OUTPUT_EDGE` | `4096` | The largest `max_edge` or `frame_edge` a request can ask for. At most `65535`, the largest edge a JPEG can have. |
| `HAZMAT_MAX_RENDITIONS` | `4` | |
| `HAZMAT_VIDEO_CONTAINERS` | `mp4,mov` | Detected from the file's first bytes |
| `HAZMAT_VIDEO_CODECS` | `h264` | The only decoder in the bundled ffmpeg |
| `HAZMAT_MAX_VIDEO_BYTES` | `50MiB` | |
| `HAZMAT_MAX_VIDEO_EDGE` | `1920` | The long edge of the source video |
| `HAZMAT_MAX_VIDEO_FPS` | `60` | Measured from the packets |
| `HAZMAT_MAX_VIDEO_SECONDS` | `60` | The largest `max_seconds` a request can ask for. A whole number. |
| `HAZMAT_MAX_FRAMES` | `32` | |
| `HAZMAT_DECODE_TIMEOUT_SECONDS` | `20` | Total time for all child processes of one video. A whole number. Keep it below the function timeout. |
| `HAZMAT_DECODE_MEMORY` | `1024MiB` | The address-space limit of each child process |
| `HAZMAT_RECYCLE` | `on_error` | `never`, `on_error`, or `always`. See "How the child process is boxed in". |
| `HAZMAT_FFMPEG_DIR` | `bin` next to the binary | Where `ffmpeg` and `ffprobe` live. If unset and they aren't there, every video is refused. |
| `HAZMAT_LOG_LEVEL` | `info` | |

## Development

```sh
make test        # gofmt + vet + tests
make fuzz        # a short run of each fuzz target
make bench       # one 12-megapixel photo, two renditions
make build       # bin/hazmat
make ffmpeg      # dist/ffmpeg/{ffmpeg,ffprobe} from the pinned source (needs Docker)
make dist        # dist/hazmat_<version>_lambda_arm64.zip + checksums.txt (needs an arm64 Docker host)
make local-test  # the binary and ffmpeg in the Lambda runtime image, against a local S3 API (needs Docker)

# Process one file locally. The source is at ./work/cdn/uploads/m1/source
bin/hazmat run --request request.json --root ./work
```

The response is JSON on stdout. Each request also writes one JSON log line to
stderr. Logs contain facts about the file only, never its bytes, EXIF values,
or a location.

## Deploying on AWS Lambda

Using Terraform? [`examples/terraform-lambda`](examples/terraform-lambda)
sets up everything below for you: the bucket, the function, the role, and the
log group.

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
   actually needs.

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

## Releases

Every push to `main` that passes CI becomes a release, tagged
`vMAJOR.MINOR.PATCH`, with `MAJOR.MINOR` taken from the `VERSION` file. Each
release contains the Lambda zip for `provided.al2023` on arm64,
`checksums.txt`, and the FFmpeg source the zip was built from. Deployments
should pin both the tag and the sha256.

Releases run one at a time, and GitHub keeps only one waiting. So if three
pushes land close together, the middle one may be skipped, and its change ships
in the next release. A run on a commit older than the newest release fails
without releasing anything.

The zip contains `bootstrap` (hazmat itself), `bin/ffmpeg`, `bin/ffprobe`,
`bin/ffmpeg-buildconf.txt`, `bin/COPYING.LGPLv2.1`, `LICENSE`, `NOTICE`, and
`licenses/` (the licenses of the Go modules compiled into `bootstrap`).

## About the bundled ffmpeg

`ffmpeg/Dockerfile` builds ffmpeg and ffprobe from a source release pinned by
version and sha256. It starts from `--disable-everything` and turns on only
what hazmat needs: the `mov` demuxer, the `h264` decoder, the `mjpeg` encoder,
the `image2` muxer, the `file` protocol, and eight filters. Networking is off,
there are no outside libraries, and the build is LGPL. It's statically linked,
so both programs run on the Lambda runtime as is.

To move to a new ffmpeg release, update the two pins in the Dockerfile and run
`make local-test`. Each ffmpeg security release gets a new hazmat release.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for how to report it.
Please don't open a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). The ffmpeg and
ffprobe binaries in the release zip are FFmpeg, licensed under the LGPL 2.1 or
later. NOTICE explains where to find that license and the source.
