BINARY   := hazmat
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X 'main.version=$(VERSION)'
FUZZTIME ?= 20s

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/hazmat

.PHONY: test
test:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed: run gofmt -w ."; exit 1; }
	go vet ./...
	go test ./...

# go test runs one fuzz target of one package at a time.
.PHONY: fuzz
fuzz:
	go test ./internal/sniff -run '^$$' -fuzz '^FuzzDetect$$' -fuzztime $(FUZZTIME)
	go test ./internal/contract -run '^$$' -fuzz '^FuzzDecodeAndValidate$$' -fuzztime $(FUZZTIME)
	go test ./internal/photo -run '^$$' -fuzz '^FuzzScan$$' -fuzztime $(FUZZTIME)
	go test ./internal/photo -run '^$$' -fuzz '^FuzzProcess$$' -fuzztime $(FUZZTIME)
	go test ./internal/video -run '^$$' -fuzz '^FuzzProbeAndPackets$$' -fuzztime $(FUZZTIME)
	go test ./internal/video -run '^$$' -fuzz '^FuzzShowinfoTimes$$' -fuzztime $(FUZZTIME)

.PHONY: bench
bench:
	go test ./internal/photo -run '^$$' -bench . -benchtime 5x

# The Lambda zip for the provided.al2023 runtime, plus its checksum, in dist/.
# CI runs it with VERSION=<the new tag>. The runtime starts the file named
# bootstrap. lambda.norpc leaves out the RPC mode that only old runtimes use.
# The zip also has bin/ffmpeg and bin/ffprobe, so it must be made on an arm64
# Docker host: the release is arm64 only.
.PHONY: dist
dist: ffmpeg
	@test "$$(docker version --format '{{.Server.Arch}}')" = arm64 || { echo "make dist needs an arm64 Docker host"; exit 1; }
	rm -rf dist/zip dist/*.zip dist/checksums.txt && mkdir -p dist/zip/bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -tags lambda.norpc \
		-ldflags "$(LDFLAGS)" -o dist/zip/bootstrap ./cmd/hazmat
	cp dist/ffmpeg/ffmpeg dist/ffmpeg/ffprobe dist/zip/bin/
	cp dist/ffmpeg/buildconf.txt dist/zip/bin/ffmpeg-buildconf.txt
	cp dist/ffmpeg/COPYING.LGPLv2.1 dist/zip/bin/
	cp LICENSE NOTICE dist/zip/
	GOOS=linux GOARCH=arm64 scripts/collect-licenses.sh dist/zip/licenses
	cd dist/zip && zip -q -X -r ../$(BINARY)_$(VERSION)_lambda_arm64.zip .
	rm -rf dist/zip
	cd dist && shasum -a 256 *.zip > checksums.txt

# The release binary inside the Lambda runtime image, against S3Mock as the
# S3 bucket. Needs Docker. The binary is built for the architecture of the Docker
# host, so the same target works on a laptop and in CI.
COMPOSE := docker compose -f test/local/compose.yaml

# ffmpeg and ffprobe for the architecture of the Docker host, from the pinned
# source in ffmpeg/Dockerfile. A few minutes the first time; Docker keeps the
# layers after that.
# The stamp is the hash of the Dockerfile and the architecture, so a build
# that is already in dist/ffmpeg (or that the CI cache put there) is kept.
FFMPEG_STAMP = $(shell shasum -a 256 ffmpeg/Dockerfile | cut -d' ' -f1)-$(shell docker version --format '{{.Server.Arch}}')

.PHONY: ffmpeg
ffmpeg:
	@if [ "$$(cat dist/ffmpeg/stamp 2>/dev/null)" = "$(FFMPEG_STAMP)" ]; then \
		echo "ffmpeg: dist/ffmpeg is up to date"; \
	else \
		rm -rf dist/ffmpeg && \
		docker build --output type=local,dest=dist/ffmpeg ffmpeg/ && \
		echo "$(FFMPEG_STAMP)" > dist/ffmpeg/stamp; \
	fi

.PHONY: local-test
local-test: ffmpeg
	mkdir -p dist/local
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') \
		go build -trimpath -tags lambda.norpc -ldflags "$(LDFLAGS)" -o dist/local/bootstrap ./cmd/hazmat
	$(COMPOSE) up -d
	go test -tags integration -count=1 ./test/local/ || { $(COMPOSE) logs worker | tail -40; $(COMPOSE) down -v; exit 1; }
	$(COMPOSE) down -v
