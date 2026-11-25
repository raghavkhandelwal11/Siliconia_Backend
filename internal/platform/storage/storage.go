// Package storage stores files in S3-compatible object storage (LLD 12):
// SeaweedFS locally and on the free VM, or Cloudflare R2's free tier. Tests use
// the in-memory store.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("storage: not found")

// Object is a stored file being read.
type Object struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

// Store keeps files by key.
type Store interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
}

// S3 is a Store on any S3-compatible service.
type S3 struct {
	client *minio.Client
	bucket string
}

// S3Config configures NewS3.
type S3Config struct {
	Endpoint  string // host:port, without scheme
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// NewS3 connects and creates the bucket if it does not exist yet.
func NewS3(ctx context.Context, c S3Config) (*S3, error) {
	client, err := minio.New(c.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       c.UseSSL,
		Region:       c.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: client: %w", err)
	}
	exists, err := client.BucketExists(ctx, c.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: bucket check: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{Region: c.Region}); err != nil {
			return nil, fmt.Errorf("storage: create bucket: %w", err)
		}
	}
	return &S3{client: client, bucket: c.Bucket}, nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key string) (Object, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return Object{}, err
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}
	return Object{Body: obj, ContentType: info.ContentType, Size: info.Size}, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Memory is an in-process Store for tests.
type Memory struct {
	mu    sync.Mutex
	files map[string]memFile
}

type memFile struct {
	ct   string
	data []byte
}

// NewMemory creates an empty in-memory store.
func NewMemory() *Memory { return &Memory{files: map[string]memFile{}} }

// Put implements Store.
func (m *Memory) Put(_ context.Context, key, contentType string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[key] = memFile{contentType, append([]byte(nil), data...)}
	return nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[key]
	if !ok {
		return Object{}, ErrNotFound
	}
	return Object{Body: io.NopCloser(bytes.NewReader(f.data)), ContentType: f.ct, Size: int64(len(f.data))}, nil
}

// Delete implements Store.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, key)
	return nil
}
