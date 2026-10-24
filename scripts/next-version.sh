#!/usr/bin/env bash
# Prints the next release tag: MAJOR.MINOR from the VERSION file, plus the
# patch after the highest existing vMAJOR.MINOR.* tag (.0 when there is none).
# Needs the tags fetched (actions/checkout with fetch-depth: 0).
set -euo pipefail
cd "$(dirname "$0")/.."

series=$(tr -d '[:space:]' < VERSION)
if [[ ! $series =~ ^[0-9]+\.[0-9]+$ ]]; then
	echo "VERSION must be MAJOR.MINOR, got '$series'" >&2
	exit 1
fi

# A VERSION behind the newest release would reuse an old series.
latest=$(git tag --list 'v*.*.*' --sort=-v:refname | head -n1)
if [[ -n $latest ]]; then
	latest_series=${latest#v}
	latest_series=${latest_series%.*}
	if [[ $(printf '%s\n%s\n' "$series" "$latest_series" | sort -V | head -n1) != "$latest_series" ]]; then
		echo "VERSION $series is behind the latest release $latest" >&2
		exit 1
	fi
fi

# A run on an older commit after a newer one was released (a re-run of a
# failed job) would give the older commit the higher tag. Refuse when a
# release of the series is on a commit that HEAD does not contain. A tag of the
# series put on a commit outside the history of main stops every release in
# this way, until that tag is moved or deleted.
newer=$(git tag --list "v$series.*" --no-merged HEAD | head -n1)
if [[ -n $newer ]]; then
	echo "$newer is on a commit that HEAD does not contain: a newer commit is already released" >&2
	exit 1
fi

last=$(git tag --list "v$series.*" --sort=-v:refname | head -n1)
if [[ -z $last ]]; then
	echo "v$series.0"
else
	echo "v$series.$((${last##*.} + 1))"
fi
