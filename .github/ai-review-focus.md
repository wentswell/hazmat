# AI review focus: hazmat

The AI review (`.github/workflows/ai-review.yml`) reads this file. It lists where this repository breaks. Keep it short: one line per fault class.

This worker decodes files that an attacker controls. Look hardest
at these, which are the rules in CLAUDE.md that a change can break:
- A path that passes a file it did not fully decode, or lets a
  byte of the source reach an output.
- Work, a download, or an allocation before the limit that
  bounds it is checked.
- A bad file that becomes a function error, or a storage or
  worker failure that becomes a refusal.
- A request above a ceiling that is lowered instead of refused.
- Metadata other than the RGB ICC profile copied to an output.
- Request or file data in an ffmpeg argument, or tool output in
  a refusal detail or a log line.
- A log line with file bytes, EXIF values, or a position.
- A contract or HAZMAT_* change that README.md does not match.

This repository is public. Report any name of a deployment, a
bucket, a cloud account, or an internal system in code, docs, or
examples. The repository's own GitHub path is public and is not
a finding.
