package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// A symbolic link below the root does not lead out of it, for a read or a
// write. A relative link that stays inside the root still works; an absolute
// link is refused even when it points inside.
func TestFileStoreFollowsNoLinkOut(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, outside := filepath.Join(base, "store"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "cdn", "inside"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		filepath.Join(root, "cdn", "uploads"):      outside,                              // a directory out
		filepath.Join(root, "cdn", "file"):         filepath.Join(outside, "secret"),     // a file out
		filepath.Join(root, "cdn", "rel"):          "../../outside",                      // a relative one out
		filepath.Join(root, "cdn", "absin"):        filepath.Join(root, "cdn", "inside"), // absolute: refused, even inside
		filepath.Join(root, "cdn", "inside", "up"): "..",                                 // up, still inside
		filepath.Join(root, "cdn", "alias"):        "inside",                             // inside
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	f := File{Root: root}

	for _, key := range []string{"uploads/m1.jpg", "rel/m1.jpg", "uploads/new/dir/m1.jpg", "absin/m1.jpg"} {
		if _, err := f.Put(ctx, "cdn", key, []byte("x"), ""); err == nil {
			t.Errorf("a write through %s was accepted", key)
		}
	}
	for _, key := range []string{"file", "uploads/secret", "rel/secret"} {
		if _, err := f.Size(ctx, "cdn", key, ""); err == nil {
			t.Errorf("the size of %s was read", key)
		}
		if r, err := f.Get(ctx, "cdn", key, ""); err == nil {
			r.Close()
			t.Errorf("%s was read", key)
		}
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Errorf("the outside directory has %d entries, want 1", len(entries))
	}

	// Links that stay inside the root.
	if _, err := f.Put(ctx, "cdn", "alias/a.jpg", []byte("pixels"), ""); err != nil {
		t.Fatalf("a write through a link inside the root: %v", err)
	}
	if n, err := f.Size(ctx, "cdn", "inside/up/inside/a.jpg", ""); err != nil || n != 6 {
		t.Errorf("a read through a link up and inside the root: %d %v", n, err)
	}
	// A write onto a link replaces the link and leaves its target alone.
	if _, err := f.Put(ctx, "cdn", "file", []byte("new"), ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "SECRET" {
		t.Errorf("the target of a link was written: %q", b)
	}
}

// The root itself can be a link (on macOS /tmp is one), or the top directory.
func TestFileStoreRoots(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	f := File{Root: filepath.Join(base, "link")}
	if _, err := f.Put(ctx, "cdn", "a/b.jpg", []byte("pixels"), ""); err != nil {
		t.Fatalf("a root that is a link: %v", err)
	}
	abs, err := filepath.Abs(filepath.Join(base, "real", "cdn", "a", "b.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	rel := strings.TrimPrefix(filepath.ToSlash(abs), "/")
	bucket, key, _ := strings.Cut(rel, "/")
	if n, err := (File{Root: "/"}).Size(ctx, bucket, key, ""); err != nil || n != 6 {
		t.Errorf("the root /: %d %v", n, err)
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
