// Package imaging checks and normalises uploaded product images. Every image is
// decoded and re-encoded as JPEG, which drops EXIF data (including GPS) and
// anything that only pretends to be an image.
package imaging

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // register decoders
	"net/http"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Limits for product images.
const (
	MaxBytes  = 8 << 20 // upload size
	MinSide   = 400     // shortest side, so zoom stays sharp
	MaxPixels = 40e6    // refuse decompression bombs before decoding
	MaxSide   = 1600    // stored images are scaled down to this
	Quality   = 85
)

var (
	ErrType   = errors.New("upload a JPEG, PNG or WebP image")
	ErrTooBig = errors.New("the image is larger than 8 MB")
	ErrSmall  = errors.New("the image must be at least 400 × 400 pixels")
	ErrHuge   = errors.New("the image has too many pixels")
	ErrBroken = errors.New("the image could not be read")
)

// Result is a normalised image.
type Result struct {
	Data          []byte
	Width, Height int
}

// Process validates and re-encodes an uploaded image.
func Process(data []byte) (Result, error) {
	if len(data) > MaxBytes {
		return Result{}, ErrTooBig
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return Result{}, ErrType
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, ErrBroken
	}
	if float64(cfg.Width)*float64(cfg.Height) > MaxPixels {
		return Result{}, ErrHuge
	}
	if min(cfg.Width, cfg.Height) < MinSide {
		return Result{}, ErrSmall
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Result{}, ErrBroken
	}
	w, h := fit(src.Bounds().Dx(), src.Bounds().Dy(), MaxSide)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// White behind transparent PNG/WebP areas, as JPEG has no alpha.
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: Quality}); err != nil {
		return Result{}, err
	}
	return Result{Data: out.Bytes(), Width: w, Height: h}, nil
}

// fit scales (w, h) down so the longer side is at most max.
func fit(w, h, max int) (int, int) {
	if w <= max && h <= max {
		return w, h
	}
	if w >= h {
		return max, h * max / w
	}
	return w * max / h, max
}
