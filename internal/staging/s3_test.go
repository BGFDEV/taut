package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/bgfdev/taut/internal/secret"
)

func TestS3Multipart(t *testing.T) {
	s := newTestS3(t)
	ctx := context.Background()
	cases := map[string]int{
		"one-byte-short-of-a-part": testPartSize - 1, // single PutObject
		"exactly-one-part":         testPartSize,
		"one-part-plus-one":        testPartSize + 1,
		"exactly-two-parts":        2 * testPartSize,
		"three-parts":              2*testPartSize + 123_457,
	}
	for name, size := range cases {
		key := "multipart/" + name
		want := prngBytes(t, uint64(size), size)
		mustPut(t, s, key, want)
		if got := mustGet(t, s, key); !bytes.Equal(got, want) {
			t.Fatalf("%s: content differs", name)
		}
		st, err := s.Stat(ctx, key)
		if err != nil || st.Size != int64(size) {
			t.Fatalf("%s: Stat = %+v, %v", name, st, err)
		}
		// The part count shows in the ETag suffix of a multipart object.
		head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes, ETag %s", name, size, aws.ToString(head.ETag))
		if name == "three-parts" && !strings.Contains(aws.ToString(head.ETag), "-3") {
			t.Errorf("three-part upload has ETag %s, want a -3 suffix", aws.ToString(head.ETag))
		}
	}
	if ups, err := s.pendingUploads(ctx, ""); err != nil || len(ups) != 0 {
		t.Fatalf("pending uploads: %v, %v", ups, err)
	}
}

func TestS3EnsureBucketIsIdempotent(t *testing.T) {
	s := newTestS3(t)
	mustPut(t, s, "keep", []byte("data"))
	for range 2 {
		if err := s.EnsureBucket(context.Background()); err != nil {
			t.Fatalf("EnsureBucket on an existing bucket: %v", err)
		}
	}
	if got := mustGet(t, s, "keep"); string(got) != "data" {
		t.Fatal("EnsureBucket disturbed existing objects")
	}
}

func TestS3ListPaginates(t *testing.T) {
	s := newTestS3(t)
	s.listPageSize = 2
	var want []string
	for i := range 7 {
		k := fmt.Sprintf("page/%02d", i)
		want = append(want, k)
		mustPut(t, s, k, []byte(k))
	}
	if got := listKeys(t, s, "page/"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if n, err := s.DeletePrefix(context.Background(), "page/"); err != nil || n != 7 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
}

func TestS3DeletePrefixAbortsUnfinishedUploads(t *testing.T) {
	s := newTestS3(t)
	ctx := context.Background()
	for _, key := range []string{"runs/a/dangling", "runs/b/dangling"} {
		up, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: &s.bucket, Key: &key, UploadId: up.UploadId, PartNumber: aws.Int32(1),
			Body: bytes.NewReader(prngBytes(t, 1, MinPartSize)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if ups, err := s.pendingUploads(ctx, ""); err != nil || len(ups) != 2 {
		t.Fatalf("setup: %d pending uploads, %v", len(ups), err)
	}
	if n, err := s.DeletePrefix(ctx, "runs/a/"); err != nil || n != 0 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	ups, err := s.pendingUploads(ctx, "")
	if err != nil || len(ups) != 1 || aws.ToString(ups[0].Key) != "runs/b/dangling" {
		t.Fatalf("after purge of runs/a/: %d pending uploads, %v", len(ups), err)
	}
}

func TestS3Errors(t *testing.T) {
	cfg := s3TestConfig(t)
	cfg.MaxAttempts = 1
	ctx := context.Background()

	t.Run("bucket-missing", func(t *testing.T) {
		s, err := NewS3(cfg) // the bucket was never created
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Put(ctx, "k", strings.NewReader("x"))
		wantCode(t, err, "staging.bucket_missing")
		_, err = s.Get(ctx, "k")
		wantCode(t, err, "staging.bucket_missing")
		for _, err := range s.List(ctx, "") {
			wantCode(t, err, "staging.bucket_missing")
		}
		_, err = s.DeletePrefix(ctx, "")
		wantCode(t, err, "staging.bucket_missing")
	})

	t.Run("access-denied", func(t *testing.T) {
		bad := cfg
		bad.SecretKey = secret.New("definitely-the-wrong-secret")
		s, err := NewS3(bad)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Put(ctx, "k", strings.NewReader("x"))
		wantCode(t, err, "staging.access_denied")
		wantCode(t, s.EnsureBucket(ctx), "staging.access_denied")
		if strings.Contains(fmt.Sprintf("%v %+v", err, err), "definitely-the-wrong-secret") {
			t.Fatal("the secret key appears in an error")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		bad := cfg
		bad.Endpoint = "http://127.0.0.1:1"
		s, err := NewS3(bad)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Put(ctx, "k", strings.NewReader("x"))
		wantCode(t, err, "staging.unreachable")
		_, err = s.Stat(ctx, "k")
		wantCode(t, err, "staging.unreachable")
	})

	t.Run("cancelled", func(t *testing.T) {
		s, err := NewS3(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Get(cctx, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Get under a cancelled context: %v", err)
		}
	})
}

func TestNewS3Validation(t *testing.T) {
	ok := S3Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: secret.New("a"), SecretKey: secret.New("s")}
	if _, err := NewS3(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*S3Config){
		"no bucket":       func(c *S3Config) { c.Bucket = "" },
		"no access key":   func(c *S3Config) { c.AccessKey = secret.Value{} },
		"no secret key":   func(c *S3Config) { c.SecretKey = secret.Value{} },
		"bad endpoint":    func(c *S3Config) { c.Endpoint = "127.0.0.1:9000" },
		"tiny part size":  func(c *S3Config) { c.PartSize = MinPartSize - 1 },
		"giant part size": func(c *S3Config) { c.PartSize = maxPartSize + 1 },
	} {
		c := ok
		mutate(&c)
		_, err := NewS3(c)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, "staging.config_invalid")
	}
}

// TestS3ProbeDefaultChecksums records how the server reacts to the SDK's
// default checksum behaviour, which NewS3 turns off. It asserts nothing: it
// documents why the setting is there.
func TestS3ProbeDefaultChecksums(t *testing.T) {
	s := newTestS3(t)
	cfg := s3TestConfig(t)
	ctx := context.Background()
	raw := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey.Reveal(), cfg.SecretKey.Reveal(), ""),
	})
	report := func(op string, err error) {
		if err != nil {
			t.Logf("SDK-default checksums: %-24s FAILED: %v", op, err)
		} else {
			t.Logf("SDK-default checksums: %-24s ok", op)
		}
	}
	key := "probe/object"
	_, err := raw.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: strings.NewReader("hello")})
	report("PutObject", err)
	out, err := raw.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err == nil {
		var b bytes.Buffer
		_, err = b.ReadFrom(out.Body)
		out.Body.Close()
	}
	report("GetObject", err)

	mp := "probe/multipart"
	up, err := raw.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &mp})
	report("CreateMultipartUpload", err)
	if err == nil {
		var parts []types.CompletedPart
		for i := int32(1); i <= 2; i++ {
			p, err := raw.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: &s.bucket, Key: &mp, UploadId: up.UploadId, PartNumber: aws.Int32(i),
				Body: bytes.NewReader(prngBytes(t, uint64(i), MinPartSize)),
			})
			report(fmt.Sprintf("UploadPart %d", i), err)
			if err == nil {
				parts = append(parts, types.CompletedPart{ETag: p.ETag, PartNumber: aws.Int32(i),
					ChecksumCRC32: p.ChecksumCRC32})
			}
		}
		_, err = raw.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: &s.bucket, Key: &mp, UploadId: up.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
		report("CompleteMultipartUpload", err)
		out, err := raw.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &mp})
		if err == nil {
			var b bytes.Buffer
			_, err = b.ReadFrom(out.Body)
			out.Body.Close()
		}
		report("GetObject (multipart)", err)
	}
	_, err = raw.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &s.bucket,
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: &key}}}})
	report("DeleteObjects", err)
}
