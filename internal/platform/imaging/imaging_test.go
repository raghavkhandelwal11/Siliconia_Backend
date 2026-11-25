package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.NRGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func TestProcessScalesAndReencodes(t *testing.T) {
	res, err := Process(encodePNG(3200, 1600))
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1600 || res.Height != 800 {
		t.Fatalf("size %dx%d", res.Width, res.Height)
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.Data)); err != nil {
		t.Fatalf("output is not JPEG: %v", err)
	}
}

func TestProcessRejects(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want error
	}{
		"too small":    {encodePNG(300, 900), ErrSmall},
		"not an image": {[]byte("<svg onload=alert(1)>"), ErrType},
		"truncated":    {encodePNG(800, 800)[:200], ErrBroken},
		"too big":      {make([]byte, MaxBytes+1), ErrTooBig},
	}
	for name, c := range cases {
		if _, err := Process(c.data); err != c.want {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}
