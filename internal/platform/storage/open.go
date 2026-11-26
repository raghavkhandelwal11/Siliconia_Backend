package storage

import "context"

// Options chooses where files live: "s3" (default) or "disk".
type Options struct {
	Driver string // STORAGE_DRIVER
	Dir    string // STORAGE_DIR, for disk
	S3     S3Config
}

// Open returns the configured Store.
func Open(ctx context.Context, o Options) (Store, error) {
	if o.Driver == "disk" {
		return NewDisk(o.Dir)
	}
	return NewS3(ctx, o.S3)
}
