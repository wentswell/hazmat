// Package store is where the worker reads the upload and writes its outputs.
// The worker calls no other service.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Store interface {
	// Size is the byte count of one version, read before the download.
	Size(ctx context.Context, bucket, key, versionID string) (int64, error)
	Get(ctx context.Context, bucket, key, versionID string) (io.ReadCloser, error)
	// Put returns the version of the object it wrote. The caller reads the
	// output by that version, so a later write to the same key changes nothing.
	Put(ctx context.Context, bucket, key string, body []byte, contentType string) (versionID string, err error)
}

// S3API is the part of the S3 client the store uses.
type S3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type S3 struct{ Client S3API }

func (s S3) Size(ctx context.Context, bucket, key, versionID string) (int64, error) {
	out, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key, VersionId: &versionID})
	if err != nil {
		return 0, fmt.Errorf("head s3://%s/%s: %w", bucket, key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

func (s S3) Get(ctx context.Context, bucket, key, versionID string) (io.ReadCloser, error) {
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key, VersionId: &versionID})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
	}
	return out.Body, nil
}

func (s S3) Put(ctx context.Context, bucket, key string, body []byte, contentType string) (string, error) {
	out, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &bucket,
		Key:         &key,
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
	})
	if err != nil {
		return "", fmt.Errorf("put s3://%s/%s: %w", bucket, key, err)
	}
	return aws.ToString(out.VersionId), nil
}

// File keeps each bucket as a directory below Root. It is for the command
// line and the tests. It has no versions: version IDs are ignored and empty.
type File struct{ Root string }

func (f File) path(bucket, key string) (string, error) {
	root, err := filepath.Abs(f.Root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, bucket, filepath.FromSlash(key))
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", errors.New("the path leaves the root of the file store")
	}
	return p, nil
}

func (f File) Size(_ context.Context, bucket, key, _ string) (int64, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", p)
	}
	return info.Size(), nil
}

func (f File) Get(_ context.Context, bucket, key, _ string) (io.ReadCloser, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (f File) Put(_ context.Context, bucket, key string, body []byte, _ string) (string, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	// Write beside the target and rename, so a reader never sees half a file.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return "", os.Rename(tmp.Name(), p)
}
