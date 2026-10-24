#!/bin/sh
# Copies the license and notice files of Go itself and of every module that is
# compiled into the worker to DIR/<module path>/. The licenses of these modules
# (BSD, Apache 2.0) ask that a binary carries them.
#
#   GOOS=linux GOARCH=arm64 scripts/collect-licenses.sh DIR
set -eu

dir=$1
mkdir -p "$dir/go"
cp "$(go env GOROOT)/LICENSE" "$dir/go/"

go mod download
go list -deps -tags lambda.norpc \
	-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' \
	./cmd/hazmat | sort -u |
while read -r path moddir; do
	found=
	for f in "$moddir"/LICENSE* "$moddir"/LICENCE* "$moddir"/COPYING* "$moddir"/NOTICE*; do
		[ -f "$f" ] || continue
		mkdir -p "$dir/$path"
		cp "$f" "$dir/$path/"
		found=1
	done
	[ -n "$found" ] || { echo "collect-licenses: no license file in $path" >&2; exit 1; }
done
