package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	f := File{Root: t.TempDir()}

	if _, err := f.Put(ctx, "cdn", "uploads/a/image.jpg", []byte("pixels"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Size(ctx, "cdn", "uploads/a/image.jpg", "ignored"); err != nil || n != 6 {
		t.Fatalf("size %d %v", n, err)
	}
	r, err := f.Get(ctx, "cdn", "uploads/a/image.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "pixels" {
		t.Fatalf("read %q", got)
	}

	// A second write replaces the file and leaves no temporary file.
	if _, err := f.Put(ctx, "cdn", "uploads/a/image.jpg", []byte("new"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(f.Root, "cdn", "uploads", "a"))
	if len(entries) != 1 {
		t.Fatalf("the directory has %d entries, want 1", len(entries))
	}

	if _, err := f.Size(ctx, "cdn", "uploads/a", ""); err == nil {
		t.Error("the size of a directory was returned")
	}
	if _, err := f.Size(ctx, "cdn", "uploads/missing", ""); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
}

// The contract refuses these keys first. The store refuses them again.
func TestFileStoreStaysBelowItsRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := File{Root: filepath.Join(root, "store")}
	for _, loc := range [][2]string{{"..", "x"}, {"cdn", "../../x"}, {"../..", "etc/passwd"}} {
		if _, err := f.Put(ctx, loc[0], loc[1], []byte("x"), ""); err == nil {
			t.Errorf("a write to %v was accepted", loc)
		}
		if _, err := f.Get(ctx, loc[0], loc[1], ""); err == nil {
			t.Errorf("a read of %v was accepted", loc)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Error("a file exists outside the root")
	}
}

type fakeS3 struct {
	head *s3.HeadObjectInput
	get  *s3.GetObjectInput
	put  *s3.PutObjectInput
	body []byte
	err  error
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.head = in
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(42)}, f.err
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.get = in
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader([]byte("source")))}, f.err
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.put = in
	f.body, _ = io.ReadAll(in.Body)
	return &s3.PutObjectOutput{VersionId: aws.String("v-new")}, f.err
}

func TestS3Store(t *testing.T) {
	ctx := context.Background()
	fake := &fakeS3{}
	s := S3{Client: fake}

	if n, err := s.Size(ctx, "cdn", "uploads/a/source", "v1"); err != nil || n != 42 {
		t.Fatalf("size %d %v", n, err)
	}
	// Each read names the exact version.
	if aws.ToString(fake.head.VersionId) != "v1" || aws.ToString(fake.head.Key) != "uploads/a/source" {
		t.Errorf("head asked for %v", fake.head)
	}
	r, err := s.Get(ctx, "cdn", "uploads/a/source", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if aws.ToString(fake.get.VersionId) != "v1" || aws.ToString(fake.get.Bucket) != "cdn" {
		t.Errorf("get asked for %v", fake.get)
	}

	version, err := s.Put(ctx, "cdn", "uploads/a/image.jpg", []byte("pixels"), "image/jpeg")
	if err != nil || version != "v-new" {
		t.Fatalf("put %q %v", version, err)
	}
	if string(fake.body) != "pixels" || aws.ToString(fake.put.ContentType) != "image/jpeg" {
		t.Errorf("put sent %q as %q", fake.body, aws.ToString(fake.put.ContentType))
	}

	fake.err = errors.New("denied")
	if _, err := s.Size(ctx, "cdn", "k", "v"); err == nil {
		t.Error("a head error was lost")
	}
	if _, err := s.Get(ctx, "cdn", "k", "v"); err == nil {
		t.Error("a get error was lost")
	}
	if _, err := s.Put(ctx, "cdn", "k", nil, ""); err == nil {
		t.Error("a put error was lost")
	}
}
