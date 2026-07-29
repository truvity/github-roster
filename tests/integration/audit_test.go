//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/audit/audittest"
)

// TestS3AuditSink runs the sink contract against a real bucket.
//
// The same assertions run against the in-memory sink in the unit suite. If
// the two ever disagree, one of them is lying about what the service does.
func TestS3AuditSink(t *testing.T) {
	requireAWS(t)

	client := newS3Client(t)

	audittest.SinkSuite(t, func(t *testing.T) audit.Sink {
		// A bucket per subtest, so they neither see each other's records
		// nor need cleaning between assertions.
		bucket := bucketName(t)
		createBucket(t, client, bucket)

		sink, err := audit.NewS3(client, bucket, true)
		require.NoError(t, err)

		return sink
	})
}

func newS3Client(t *testing.T) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	require.NoError(t, err)

	// Path style: localstack serves buckets on the endpoint path rather
	// than as virtual hosts.
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
}

func bucketName(t *testing.T) string {
	t.Helper()

	name := "roster-" + runID(t) + "-" + t.Name()

	return sanitizeBucket(name)
}

// sanitizeBucket keeps a bucket name legal: lowercase alphanumerics and
// dashes, 3–63 characters.
func sanitizeBucket(name string) string {
	out := make([]rune, 0, len(name))

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		default:
			out = append(out, '-')
		}
	}

	trimmed := string(out)
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '-' {
		trimmed = trimmed[:len(trimmed)-1]
	}

	if len(trimmed) > 63 {
		trimmed = trimmed[:63]
	}

	return trimmed
}

func createBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()

	ctx := context.Background()

	// Outside us-east-1 the location constraint is mandatory; without it
	// S3 answers IllegalLocationConstraintException.
	region := os.Getenv("AWS_REGION")

	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}

	_, err := client.CreateBucket(ctx, input)
	require.NoError(t, err, "bucket %q", bucket)

	t.Cleanup(func() {
		objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			return
		}

		for _, object := range objects.Contents {
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket), Key: object.Key,
			})
		}

		_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})
}
