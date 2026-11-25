package product

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/imaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// UploadView is a stored image ready to attach to a product.
type UploadView struct {
	ID     uuid.UUID `json:"id"`
	URL    string    `json:"url"`
	Width  int32     `json:"width"`
	Height int32     `json:"height"`
}

// UploadImage validates, normalises and stores a product image (60 an hour).
func (s *Service) UploadImage(ctx context.Context, sellerID uuid.UUID, data []byte) (UploadView, error) {
	if err := s.requireSeller(ctx, s.Q, sellerID); err != nil {
		return UploadView{}, err
	}
	if s.Limiter != nil {
		if r, err := s.Limiter.Allow(ctx, "image-upload", sellerID.String(), 60, time.Hour); err == nil && !r.Allowed {
			return UploadView{}, httpx.ErrRateLimited.WithDetail("Too many uploads. Try again later")
		}
	}
	img, err := imaging.Process(data)
	if err != nil {
		if errors.Is(err, imaging.ErrTooBig) {
			return UploadView{}, httpx.NewError(http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", err.Error())
		}
		return UploadView{}, httpx.NewError(http.StatusUnprocessableEntity, "IMAGE_REJECTED", "This image cannot be used").WithDetail(capitalise(err.Error()))
	}
	id, _ := uuid.NewV7()
	now := time.Now().UTC()
	key := fmt.Sprintf("products/%04d/%02d/%s.jpg", now.Year(), now.Month(), id)
	if err := s.Store.Put(ctx, key, "image/jpeg", img.Data); err != nil {
		s.Log.Error("image store", "err", err)
		return UploadView{}, httpx.ErrUnavailable.WithDetail("Image storage is not responding. Try again in a minute")
	}
	up, err := s.Q.InsertUpload(ctx, store.InsertUploadParams{
		ID: id, OwnerID: sellerID, Purpose: "PRODUCT_IMAGE", StorageKey: key, ContentType: "image/jpeg",
		Bytes: int32(len(img.Data)), Width: int32(img.Width), Height: int32(img.Height),
	})
	if err != nil {
		_ = s.Store.Delete(ctx, key)
		return UploadView{}, err
	}
	return UploadView{ID: up.ID, URL: MediaURL(key), Width: up.Width, Height: up.Height}, nil
}

// ServeMedia streams a public product image. Keys are immutable, so browsers
// and the CDN may cache them for a year.
func (s *Service) ServeMedia(w http.ResponseWriter, r *http.Request, key string) {
	if !strings.HasPrefix(key, "products/") || strings.Contains(key, "..") {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	obj, err := s.Store.Get(r.Context(), key)
	if errors.Is(err, storage.ErrNotFound) {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		s.Log.Error("media read", "key", key, "err", err)
		httpx.WriteError(w, r, httpx.ErrUnavailable)
		return
	}
	defer obj.Body.Close()
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = io.Copy(w, obj.Body)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
