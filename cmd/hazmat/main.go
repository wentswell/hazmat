// hazmat reads one untrusted upload and writes clean outputs. One binary
// is the Lambda handler (when the Lambda runtime starts it) and a command line
// tool (in every other case).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/wentswell/hazmat/internal/config"
	"github.com/wentswell/hazmat/internal/contract"
	"github.com/wentswell/hazmat/internal/sandbox"
	"github.com/wentswell/hazmat/internal/store"
	"github.com/wentswell/hazmat/internal/video"
	"github.com/wentswell/hazmat/internal/worker"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

const usage = `hazmat reads one untrusted upload and writes clean outputs.

  hazmat run --request FILE [--root DIR]   run one request; the response is JSON on stdout
  hazmat config                            print the operator ceilings (HAZMAT_* variables)
  hazmat version

--root DIR uses the file store: each bucket is a directory below DIR.
Exit status: 0 for a response (ok or refused), 1 for an error.
`

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		serveLambda()
		return
	}
	if err := cli(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hazmat:", err)
		os.Exit(1)
	}
}

func serveLambda() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		slog.Error("the configuration is not valid", "error", err.Error())
		os.Exit(1)
	}
	w, err := newWorker(context.Background(), cfg)
	if err != nil {
		slog.Error("the worker cannot start", "error", err.Error())
		os.Exit(1)
	}
	// On Lambda, "end the execution environment" is: exit a moment after the
	// response is sent. Lambda then starts a new one for the next request. If
	// Lambda freezes this one before the timer fires, the exit happens when
	// the next request arrives; that request fails, and the caller tries again.
	w.Recycle = func() {
		time.AfterFunc(100*time.Millisecond, func() { os.Exit(1) })
	}
	// The whole configuration goes to the log once. It holds no secret.
	w.Log.Info("start", "worker_version", version, "ffmpeg_version", w.FFmpegVersion,
		"video", w.Tools != nil, "config_hash", cfg.Hash(), "config", cfg)

	lambda.Start(func(ctx context.Context, raw json.RawMessage) (contract.Response, error) {
		req, err := contract.Decode(raw)
		if err != nil {
			w.Log.Error("request", "result", "invalid_request", "error", err.Error())
			return contract.Response{}, err
		}
		return w.Process(ctx, req)
	})
}

func newWorker(ctx context.Context, cfg config.Config) (*worker.Worker, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, err
	}
	w := &worker.Worker{
		Config:  cfg,
		Version: version,
		Log:     slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
	}
	var err error
	if w.Tools, w.FFmpegVersion, err = findTools(ctx, cfg); err != nil {
		return nil, err
	}
	switch cfg.Store {
	case config.StoreFile:
		w.Store = store.File{Root: cfg.FileRoot}
	default:
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load the AWS configuration: %w", err)
		}
		w.Store = store.S3{Client: s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.UsePathStyle = cfg.S3PathStyle
		})}
	}
	return w, nil
}

// findTools looks for ffmpeg and ffprobe: in HAZMAT_FFMPEG_DIR, or in "bin" beside
// the worker, which is where the release zip has them. With no directory
// named and no programs there, the worker runs with no video path. A named
// directory that does not have them is a mistake, and stops the start.
func findTools(ctx context.Context, cfg config.Config) (*video.Tools, string, error) {
	dir, named := cfg.FFmpegDir, cfg.FFmpegDir != ""
	if !named {
		exe, err := os.Executable()
		if err != nil {
			return nil, "", nil
		}
		dir = filepath.Join(filepath.Dir(exe), "bin")
	}
	tools := &video.Tools{FFmpeg: filepath.Join(dir, "ffmpeg"), FFprobe: filepath.Join(dir, "ffprobe")}
	for _, p := range []string{tools.FFmpeg, tools.FFprobe} {
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			if named {
				return nil, "", fmt.Errorf("HAZMAT_FFMPEG_DIR: %s is not there", p)
			}
			return nil, "", nil
		}
	}
	// "ffmpeg version 8.1.3 Copyright ...": the third word.
	res, err := sandbox.Run(ctx, sandbox.Limits{Timeout: 10 * time.Second, StdoutBytes: 4 << 10}, os.TempDir(), tools.FFmpeg, "-version")
	if err != nil || res.ExitCode != 0 {
		return nil, "", fmt.Errorf("%s -version does not run (exit status %d): %v", tools.FFmpeg, res.ExitCode, err)
	}
	words := strings.Fields(string(res.Stdout))
	if len(words) < 3 {
		return nil, "", fmt.Errorf("%s -version gave no version", tools.FFmpeg)
	}
	return tools, words[2], nil
}

func cli(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		if _, ffmpeg, _ := findTools(context.Background(), config.Config{FFmpegDir: os.Getenv("HAZMAT_FFMPEG_DIR")}); ffmpeg != "" {
			fmt.Println("ffmpeg", ffmpeg)
		}
		return nil
	case "config":
		cfg, err := config.Load(os.Getenv)
		if err != nil {
			return err
		}
		return printJSON(struct {
			ConfigHash string        `json:"config_hash"`
			Config     config.Config `json:"config"`
		}{cfg.Hash(), cfg})
	case "run":
		return run(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (run, config, version)", args[0])
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	requestPath := fs.String("request", "", "the request, a JSON file")
	root := fs.String("root", "", "use the file store below this directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *requestPath == "" {
		return errors.New("run needs --request FILE")
	}

	getenv := os.Getenv
	if *root != "" {
		getenv = func(name string) string {
			switch name {
			case "HAZMAT_STORE":
				return config.StoreFile
			case "HAZMAT_FILE_ROOT":
				return *root
			}
			return os.Getenv(name)
		}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*requestPath)
	if err != nil {
		return err
	}
	req, err := contract.Decode(raw)
	if err != nil {
		return err
	}
	w, err := newWorker(context.Background(), cfg)
	if err != nil {
		return err
	}
	resp, err := w.Process(context.Background(), req)
	if err != nil {
		return err
	}
	return printJSON(resp)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
