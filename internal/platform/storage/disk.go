package storage

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Disk is a Store on the local filesystem, for the small "lite" server where
// an object store would cost more memory than the whole API. Keys map to
// files under Dir; the content type comes from the file extension.
type Disk struct{ Dir string }

// NewDisk creates the directory if needed.
func NewDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &Disk{Dir: dir}, nil
}

// file turns a key into a path inside Dir, refusing anything that escapes it.
func (d *Disk) file(key string) (string, error) {
	clean := path.Clean("/" + key)
	if key == "" || clean == "/" || strings.Contains(key, "\\") || clean != "/"+key {
		return "", ErrNotFound
	}
	return filepath.Join(d.Dir, filepath.FromSlash(clean[1:])), nil
}

// Put implements Store. Writes go to a temporary file first, so readers never see half a file.
func (d *Disk) Put(_ context.Context, key, _ string, data []byte) error {
	p, err := d.file(key)
	if err != nil {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Get implements Store.
func (d *Disk) Get(_ context.Context, key string) (Object, error) {
	p, err := d.file(key)
	if err != nil {
		return Object{}, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, err
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		return Object{}, ErrNotFound
	}
	ct := mime.TypeByExtension(path.Ext(key))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return Object{Body: f, ContentType: ct, Size: st.Size()}, nil
}

// Delete implements Store.
func (d *Disk) Delete(_ context.Context, key string) error {
	p, err := d.file(key)
	if err != nil {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
