# Security

hazmat reads hostile files on purpose, so a security bug in it matters.
Please report one privately.

## Report a vulnerability

Use GitHub private vulnerability reporting: open the **Security** tab of this
repository and select **Report a vulnerability**. Do not open a public issue.

Include the release tag, the request JSON, and the file (or a script that makes
it). A sample file that shows the bug is the fastest path to a fix.

We answer within 7 days. We fix a confirmed bug in a new release, and credit
the reporter in the advisory unless they ask us not to.

## Supported versions

Only the latest release gets fixes. Every push to `main` is a release, so a fix
ships as a new patch version.

## In scope

- A file that makes an output hold bytes of the source that did not come from
  decoded pixels: EXIF, GPS position, XMP, comments, or other metadata.
- A file that the worker passes but that it did not fully decode.
- A file that gets past a ceiling (bytes, pixels, edge, duration, frame rate,
  frame count) before the worker refuses it.
- A file that crashes the worker, or makes it hang past `HAZMAT_DECODE_TIMEOUT_SECONDS`.
- A request or a file that puts text it controls into an ffmpeg argument, a
  refusal detail, or a log line.
- A request that reads or writes outside `HAZMAT_ALLOWED_BUCKETS`,
  `HAZMAT_INPUT_PREFIXES`, or `HAZMAT_OUTPUT_PREFIXES`.

## Out of scope

- Whether the content of a picture is acceptable. The worker makes no decision
  about content.
- A bug in FFmpeg itself that the minimal build does not reach. Report FFmpeg
  bugs to the FFmpeg project. If the build of this repository reaches the bug,
  report it here too.
- The child process sandbox as a security boundary. The README says it is not
  one: the IAM role of the function is the real limit.
