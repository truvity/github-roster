package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Compile-time interface check.
var _ Sink = (*S3)(nil)

// maxRecordBytes caps one record read. Records are a few kilobytes; the
// limit stops a corrupted or hostile object from being read into memory
// unbounded.
const maxRecordBytes = 4 << 20

// S3 stores records as JSON objects in a bucket.
//
// One object per run, keyed <org>/<timestamp>-<job>.json. The timestamp
// leads so keys sort chronologically, and the organization leads the whole
// key so a per-organization bucket policy is possible later without moving
// anything.
type S3 struct {
	client *s3.Client
	bucket string
	// prefix roots every key. Empty for a dedicated bucket; the tenant's
	// "<namespace>/<release>/" in the shared-tier test model, where many
	// installs write into one bucket and MUST not see each other.
	prefix string
	// prefixPerOrg is honored by Key; kept so a deployment that wants a
	// flat layout can have one without changing this type's callers.
	prefixPerOrg bool
}

// NewS3 returns a sink backed by a bucket, rooted at prefix (which may be
// empty, and must end with "/" otherwise).
func NewS3(client *s3.Client, bucket, prefix string, prefixPerOrg bool) (*S3, error) {
	if client == nil {
		return nil, fmt.Errorf("an s3 client is required")
	}

	if bucket == "" {
		return nil, fmt.Errorf("audit.bucket is required")
	}

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("audit prefix %q must end with %q", prefix, "/")
	}

	return &S3{client: client, bucket: bucket, prefix: prefix, prefixPerOrg: prefixPerOrg}, nil
}

func (s *S3) key(record Record) string {
	if !s.prefixPerOrg {
		return s.prefix + record.ID + ".json"
	}

	return s.prefix + Key(record.Org, record.ID)
}

// Write stores one record.
func (s *S3) Write(ctx context.Context, record Record) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.key(record)),
		Body:        bytesReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("write audit record %q: %w", s.key(record), err)
	}

	return nil
}

// List returns records newest first.
//
// S3 lists keys in ascending order, so the newest are last. For the volumes
// this service produces — a handful of runs a day — listing the
// organization's keys and taking the tail is simpler and cheaper than
// maintaining an index, and it stays correct without one.
func (s *S3) List(ctx context.Context, org string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}

	// Listing is always confined to this sink's root: a tenant sharing the
	// bucket must be unable to read a neighbour's records even by asking.
	listPrefix := s.prefix
	if org != "" && s.prefixPerOrg {
		listPrefix += sanitize(org) + "/"
	}

	var prefix *string
	if listPrefix != "" {
		prefix = aws.String(listPrefix)
	}

	var keys []string

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: prefix,
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list audit records: %w", err)
		}

		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}

	// Newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))

	if len(keys) > limit {
		keys = keys[:limit]
	}

	records := make([]Record, 0, len(keys))

	for _, key := range keys {
		record, err := s.get(ctx, key)
		if err != nil {
			// One unreadable record must not hide the rest: the audit log
			// is most needed when something is already wrong.
			continue
		}

		records = append(records, record)
	}

	return records, nil
}

func (s *S3) get(ctx context.Context, key string) (Record, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Record{}, fmt.Errorf("read audit record %q: %w", key, err)
	}

	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(out.Body, maxRecordBytes))
	if err != nil {
		return Record{}, fmt.Errorf("read audit record %q: %w", key, err)
	}

	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, fmt.Errorf("decode audit record %q: %w", key, err)
	}

	return record, nil
}
