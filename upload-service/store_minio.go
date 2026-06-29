package main

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
)

// minioObjectStore streams objects out of a single MinIO/S3 bucket.
type minioObjectStore struct {
	client *minio.Client
	bucket string
}

// newMinioObjectStore binds a minio client to a bucket.
func newMinioObjectStore(client *minio.Client, bucket string) *minioObjectStore {
	return &minioObjectStore{client: client, bucket: bucket}
}

// Open returns a streaming reader for the object at key. It Stats the object so
// a missing object or unreachable backend surfaces here — before any response
// body is written — letting the handler map it to 503.
func (s *minioObjectStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %s/%s: %w", s.bucket, key, err)
	}
	// minio-go's GetObject is lazy; the request only fires on Stat/Read, so probe now.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("stat object %s/%s: %w", s.bucket, key, err)
	}
	return obj, nil
}
