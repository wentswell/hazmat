# hazmat

Go Lambda (and CLI) that reads one untrusted upload and writes clean JPEG outputs. It is meant to be the one place in an app that decodes user bytes. The app's service invokes it; it calls nothing but storage. The repository is public: write nothing here, in code, or in commits that names a deployment, a bucket, an account, or an internal system.

## Rules

- **The worker makes no decision about content.** No moderation call, no policy, no word for "safe". It refuses a file or returns facts.
- **Fail closed.** Anything the worker cannot fully decode is a refusal (`undecodable`), never a pass-through. The source bytes never reach an output: every output is re-encoded from decoded pixels.
- **Check before you allocate.** Size before the download, header dimensions before the decode. Keep that order when adding a path.
- **A refusal is the file's fault; an error is not.** `*contract.Refusal` -> `status: refused` (the caller returns 400, no retry). Any other error -> a function error (the caller returns 503): `*contract.StorageError` for storage, `*contract.InvalidRequestError` for a bad request. The Go type name is the Lambda `errorType`, so do not rename them. Do not turn a storage failure into a refusal or a bad file into an error.
- **Never clamp.** A request above an operator ceiling is `InvalidRequestError`. Do not lower the value and continue.
- **Only the RGB ICC profile is copied to an output.** No EXIF, XMP, IPTC, comment. A new copied field needs a reason and a test.
- **Go only for photos.** No cgo, no child process, no image library beyond the standard library and `golang.org/x/image`. `CGO_ENABLED=0` must keep working.
- **Video: believe what was measured, not what the file declares.** The duration and the frame rate come from the packet times (`probe.go`), the frame count is checked after the decode, and a file where the header and the packets differ is refused.
- **ffmpeg runs only through `internal/sandbox`**, with a fixed argument list. Nothing from the request or the file goes into an argument, except numbers the worker checked and paths the worker made. Nothing a tool printed goes into a refusal detail or a log: the file controls that text.
- **A component that is not in the ffmpeg build cannot be attacked.** `ffmpeg/Dockerfile` lists what is on and why. Adding a decoder, a demuxer, a protocol, or a library needs a reason in that file, and `HAZMAT_VIDEO_CODECS` in `config.go` must name only what the build decodes.
- **A child process that did not end by itself taints the execution environment** (`video.Abnormal`, `HAZMAT_RECYCLE`). `decode_timeout` is a refusal, not an error: do not change that, or a hostile file makes the caller retry without end.
- **Logs hold facts, never content:** no bytes of the file, no EXIF values, no position.
- **No deployment-specific bucket or prefix in the code.** They come from `HAZMAT_*` variables and the request.
- The contract is versioned (`contract.Version`). Removing or renaming a field bumps it; adding an optional one does not. `README.md` documents the contract and the variables: update it in the same change.

## Layout

```
cmd/hazmat/          main: Lambda mode when AWS_LAMBDA_RUNTIME_API is set, else the CLI (run | config | version)
internal/contract/   request, response, refusal codes, Decode (strict), Validate (against the ceilings)
internal/config/     HAZMAT_* ceilings, config_hash
internal/sniff/      type from the first bytes
internal/photo/      decode, orientation, ICC copy, resize, encode; meta.go reads the container around the pixels
internal/video/      ffprobe header + packet times, the sampling plan, the one ffmpeg pass, frame scores
internal/sandbox/    the child process: empty environment, process group, timeout, resource limits (Linux)
internal/store/      Store interface; S3 and File
ffmpeg/              Dockerfile: the pinned, minimal ffmpeg and ffprobe
test/local/          compose.yaml + integration tests (Lambda runtime image + S3Mock); testdata/ videos from make-fixtures.sh
internal/worker/     one request end to end, and its one log line
```

## Build / test

`make test` (gofmt + vet + test), `make fuzz`, `make bench`, `make build`, `make ffmpeg`, `make dist`. `make local-test` runs the binary and ffmpeg inside the Lambda runtime image against S3Mock (`test/local/`, needs Docker): it is the only test of the Lambda entry point, of the S3 store against a real S3 API with versions, and of the video path with the real ffmpeg. The unit tests of `internal/video` use shell scripts in place of the tools, so they need no ffmpeg. The video fixtures are committed; `test/local/make-fixtures.sh` makes them again with a full ffmpeg from a container image. MinIO and LocalStack images are no longer free to pull, which is why it is S3Mock. The orientation table in `photo_test.go` comes from the EXIF specification and was checked against macOS Quick Look; do not "fix" it to match the code. The WebP files in `internal/photo/testdata/` were made with `cwebp` from the quadrant picture of the tests.

## CI / releases

`.github/workflows/ci.yml`: every push to `main` that passes is a release (`vMAJOR.MINOR.PATCH`, `MAJOR.MINOR` from `VERSION`), with the Lambda zip and `checksums.txt`. CI runs on arm64 runners, because the zip holds ffmpeg for arm64; the ffmpeg build is cached on the hash of `ffmpeg/Dockerfile`. Bump `VERSION` for a contract change. The repo holds no AWS credentials; a deployment pins a tag and a sha256. The release also carries the FFmpeg source tarball and the zip carries `LICENSE`, `NOTICE`, `bin/COPYING.LGPLv2.1`, and `licenses/` (from `scripts/collect-licenses.sh`): that is the LGPL and Go-module license compliance, so keep it when changing `make dist` or the release step.
