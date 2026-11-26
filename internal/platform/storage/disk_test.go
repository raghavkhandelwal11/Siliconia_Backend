package storage

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestDiskRoundTripAndSafety(t *testing.T) {
	ctx := context.Background()
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "products/2026/11/a.jpg", "image/jpeg", []byte("jpeg")); err != nil {
		t.Fatal(err)
	}
	o, err := d.Get(ctx, "products/2026/11/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(o.Body)
	_ = o.Body.Close()
	if string(b) != "jpeg" || o.ContentType != "image/jpeg" || o.Size != 4 {
		t.Fatalf("got %q %s %d", b, o.ContentType, o.Size)
	}
	for _, bad := range []string{"../etc/passwd", "a/../../x", "/abs", "", "a\\b", "products"} {
		if _, err := d.Get(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want not found", bad, err)
		}
	}
	if err := d.Put(ctx, "../escape.jpg", "image/jpeg", []byte("x")); err == nil {
		t.Error("Put escaped the directory")
	}
	if err := d.Delete(ctx, "products/2026/11/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, "products/2026/11/a.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted file still readable")
	}
}
