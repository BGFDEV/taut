package staging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

const (
	// DefaultPartSize is the multipart part size, and so the memory one Put
	// holds while streaming.
	DefaultPartSize = 16 << 20
	// MinPartSize is S3's minimum size for every part but the last.
	MinPartSize = 5 << 20
	// maxPartSize bounds the per-Put buffer.
	maxPartSize = 1 << 30
	// maxParts is S3's limit on parts per upload.
	maxParts = 10000
	// abortTimeout bounds the cleanup of a failed multipart upload, which
	// must run even though the caller's context is already cancelled.
	abortTimeout = 30 * time.Second
)

// S3Config configures the S3 backend.
type S3Config struct {
	// Endpoint is the base URL of an S3-compatible store, for example
	// "http://127.0.0.1:9000". Requests to it use path-style addressing.
	// Empty means AWS S3 itself.
	Endpoint string
	// Region is the signing region; "us-east-1" if empty.
	Region string
	// Bucket holds every object.
	Bucket    string
	AccessKey secret.Value
	SecretKey secret.Value
	// PartSize is the multipart part size in bytes: 0 for DefaultPartSize,
	// otherwise at least MinPartSize. One Put can store at most 10,000
	// parts (160 GiB at the default), and holds one part in memory.
	PartSize int64
	// MaxAttempts is how many times a request is tried; 0 for the SDK
	// default of 3.
	MaxAttempts int
}

// S3 is a Blobs backed by an S3-compatible object store.
type S3 struct {
	client   *s3.Client
	bucket   string
	region   string
	partSize int
	bufs     sync.Pool // *[]byte of partSize, reused across Puts
	// listPageSize overrides the page size in tests; 0 is the server default.
	listPageSize int32
}

// NewS3 builds the S3 backend. It makes no network request; call
// EnsureBucket to verify the configuration.
func NewS3(cfg S3Config) (*S3, error) {
	bad := func(why, next string) (*S3, error) {
		return nil, fail.New("staging.config_invalid", "The S3 staging configuration is not valid", why, next)
	}
	if cfg.Bucket == "" {
		return bad("No bucket is set.", "Set the staging bucket in the config file, then retry.")
	}
	if cfg.AccessKey.IsZero() || cfg.SecretKey.IsZero() {
		return bad("The access key or the secret key is empty.",
			"Set both staging credentials (the config file refers to them by env var or file), then retry.")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad("The endpoint must be a URL such as https://s3.example.com.",
				"Fix the staging endpoint in the config file, then retry.")
		}
	}
	partSize := cfg.PartSize
	if partSize == 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize || partSize > maxPartSize {
		return bad("The part size must be between 5 MiB and 1 GiB.",
			"Fix the staging part size in the config file, then retry.")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey.Reveal(), cfg.SecretKey.Reveal(), ""),
		// Only send and verify the SDK's CRC checksums where the S3 API
		// demands one. The default ("when supported") adds them to every
		// request, which several S3-compatible stores reject or mishandle.
		// Integrity does not depend on them: every object is authenticated
		// end to end by the Encrypted layer.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           cfg.MaxAttempts,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
		opts.UsePathStyle = true
	}
	s := &S3{client: s3.New(opts), bucket: cfg.Bucket, region: region, partSize: int(partSize)}
	s.bufs.New = func() any {
		b := make([]byte, s.partSize)
		return &b
	}
	return s, nil
}

// EnsureBucket creates the bucket if it does not exist. An existing bucket
// that the credentials can use is left alone.
func (s *S3) EnsureBucket(ctx context.Context) error {
	in := &s3.CreateBucketInput{Bucket: &s.bucket}
	if s.region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(s.region),
		}
	}
	_, err := s.client.CreateBucket(ctx, in)
	switch errorCode(err) {
	case "":
		if err == nil {
			return nil
		}
	case "BucketAlreadyOwnedByYou":
		return nil
	case "BucketAlreadyExists":
		// The name is taken; fine if it is ours to use.
		if _, herr := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket}); herr == nil {
			return nil
		}
	}
	return s.mapErr(ctx, "create the staging bucket", err)
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	src := &sourceReader{ctx: ctx, r: r}
	bp := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(bp)
	buf := *bp

	n, eof, err := readFull(src, buf)
	if err != nil {
		return 0, src.failed(key, err)
	}
	if eof {
		// The whole object fits in one part: a single atomic PutObject.
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &s.bucket,
			Key:           &key,
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			return 0, s.mapErr(ctx, "write a staging object", err)
		}
		return int64(n), nil
	}

	// Multipart upload. Parts are invisible until CompleteMultipartUpload,
	// which publishes the object atomically; every failure path aborts so
	// no parts are left behind.
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return 0, s.mapErr(ctx, "write a staging object", err)
	}
	uploadID := created.UploadId
	abort := func() {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: &s.bucket, Key: &key, UploadId: uploadID,
		})
	}

	var parts []types.CompletedPart
	var total int64
	for {
		// buf[:n] is the next part; only the last one may be short.
		num := int32(len(parts) + 1)
		if num > maxParts {
			abort()
			return 0, fail.New("staging.object_too_large",
				"A staging object is too large to upload",
				"It exceeds "+strconv.Itoa(maxParts)+" parts of "+strconv.Itoa(s.partSize>>20)+" MiB.",
				"Raise the staging part size in the config file, then retry.")
		}
		up, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        &s.bucket,
			Key:           &key,
			UploadId:      uploadID,
			PartNumber:    &num,
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			abort()
			return 0, s.mapErr(ctx, "write a staging object", err)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: &num})
		total += int64(n)
		if eof {
			break
		}
		n, eof, err = readFull(src, buf)
		if err != nil {
			abort()
			return 0, src.failed(key, err)
		}
		if n == 0 {
			break // the source ended exactly on a part boundary
		}
	}
	if err := ctx.Err(); err != nil {
		abort()
		return 0, src.failed(key, err)
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &s.bucket,
		Key:             &key,
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()
		return 0, s.mapErr(ctx, "write a staging object", err)
	}
	return total, nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if isS3NotFound(err) {
			return nil, notExist(key)
		}
		return nil, s.mapErr(ctx, "read a staging object", err)
	}
	return out.Body, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	if err := ValidateKey(key); err != nil {
		return Info{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if isS3NotFound(err) {
			return Info{}, notExist(key)
		}
		return Info{}, s.mapErr(ctx, "read a staging object", err)
	}
	return Info{Key: key, Size: aws.ToInt64(out.ContentLength)}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil && !isS3NotFound(err) {
		return s.mapErr(ctx, "delete a staging object", err)
	}
	return nil
}

func (s *S3) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := validatePrefix(prefix); err != nil {
			yield(Info{}, err)
			return
		}
		in := &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix}
		if s.listPageSize > 0 {
			in.MaxKeys = &s.listPageSize
		}
		for {
			out, err := s.client.ListObjectsV2(ctx, in)
			if err != nil {
				yield(Info{}, s.mapErr(ctx, "list staging objects", err))
				return
			}
			for _, o := range out.Contents {
				if !yield(Info{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)}, nil) {
					return
				}
			}
			if !aws.ToBool(out.IsTruncated) || aws.ToString(out.NextContinuationToken) == "" {
				return
			}
			in.ContinuationToken = out.NextContinuationToken
		}
	}
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := validatePrefix(prefix); err != nil {
		return 0, err
	}
	// Objects, one DeleteObjects request per batch of up to 1000.
	removed := 0
	var batch []types.ObjectIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket,
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return s.mapErr(ctx, "delete staging objects", err)
		}
		removed += len(batch) - len(out.Errors)
		batch = batch[:0]
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fail.New("staging.s3_error",
				"Taut could not delete staging objects",
				"The object store refused to delete "+strconv.Itoa(len(out.Errors))+" object(s); the first error was "+
					strconv.Quote(aws.ToString(e.Code))+" for key "+quoteKey(aws.ToString(e.Key))+".",
				"Check that the staging credentials may delete objects in the bucket, then retry.")
		}
		return nil
	}
	for info, err := range s.List(ctx, prefix) {
		if err != nil {
			return removed, err
		}
		batch = append(batch, types.ObjectIdentifier{Key: aws.String(info.Key)})
		if len(batch) == 1000 {
			if err := flush(); err != nil {
				return removed, err
			}
		}
	}
	if err := flush(); err != nil {
		return removed, err
	}

	// Parts of interrupted multipart uploads hold data too, and are not
	// objects, so nothing above removes them.
	uploads, err := s.pendingUploads(ctx, prefix)
	if err != nil {
		return removed, err
	}
	for _, u := range uploads {
		_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: &s.bucket, Key: u.Key, UploadId: u.UploadId,
		})
		if err != nil && errorCode(err) != "NoSuchUpload" {
			return removed, s.mapErr(ctx, "delete staging objects", err)
		}
	}
	return removed, nil
}

// pendingUploads lists the multipart uploads in progress under prefix.
func (s *S3) pendingUploads(ctx context.Context, prefix string) ([]types.MultipartUpload, error) {
	var all []types.MultipartUpload
	in := &s3.ListMultipartUploadsInput{Bucket: &s.bucket, Prefix: &prefix}
	for {
		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			if errorCode(err) == "NotImplemented" {
				return all, nil // the store cannot list them; nothing more we can do
			}
			return all, s.mapErr(ctx, "list unfinished staging uploads", err)
		}
		all = append(all, out.Uploads...)
		if !aws.ToBool(out.IsTruncated) || aws.ToString(out.NextKeyMarker) == "" {
			return all, nil
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// errorCode returns the S3 error code carried by err, or "".
func errorCode(err error) string {
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func httpStatus(err error) int {
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}

func isS3NotFound(err error) bool {
	switch errorCode(err) {
	case "NoSuchKey", "NotFound":
		return true
	}
	return false
}

// mapErr turns an SDK error into an operator-facing one. Cancellation is
// passed through unchanged so callers can tell it from a failure.
func (s *S3) mapErr(ctx context.Context, action string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	what := "Taut could not " + action
	code, status := errorCode(err), httpStatus(err)
	var netErr net.Error
	switch {
	case code == "NoSuchBucket":
		return fail.Wrap(err, "staging.bucket_missing", what,
			"The bucket "+strconv.Quote(s.bucket)+" does not exist on the object store.",
			"Create the bucket, or correct the bucket name in the config file, then retry. `taut auth` creates it for you.")
	case code == "AccessDenied", code == "InvalidAccessKeyId", code == "SignatureDoesNotMatch",
		code == "ExpiredToken", code == "InvalidToken", status == 401, status == 403:
		return fail.Wrap(err, "staging.access_denied", what,
			"The object store rejected the staging credentials (S3 error "+strconv.Quote(code)+").",
			"Check the staging access key and secret key, and that they may read, write, list and delete in bucket "+
				strconv.Quote(s.bucket)+", then retry.").WithExit(fail.ExitPermission)
	case status == 0 && errors.As(err, &netErr):
		return fail.Wrap(err, "staging.unreachable", what,
			"The object store did not answer: "+netErr.Error(),
			"Check that the staging endpoint in the config file is correct and reachable from this machine, then retry.")
	case status >= 500:
		return fail.Wrap(err, "staging.unavailable", what,
			"The object store answered with a server error (HTTP "+strconv.Itoa(status)+") after retries.",
			"Check the health of the object store, then retry; the run resumes where it stopped.")
	default:
		return fail.Wrap(err, "staging.s3_error", what, "",
			"Check the staging endpoint, bucket and credentials in the config file, then retry with --verbose.")
	}
}
