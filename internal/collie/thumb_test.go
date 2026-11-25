package collie

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"
)

// noisy draws an image that compresses badly, like a photo or a busy screenshot.
func noisy(w, h int, alpha bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha && x < w/2 {
				a = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(r.Intn(256)), uint8(x), uint8(y), a})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMakeThumb(t *testing.T) {
	big := encodePNG(t, noisy(3000, 2000, false))
	start := time.Now()
	out, ok := makeThumb(big)
	t.Logf("3000x2000 PNG %d KB -> %d KB in %v", len(big)>>10, len(out)>>10, time.Since(start))
	if !ok {
		t.Fatal("a big PNG should shrink")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" || cfg.Width != 1170 || cfg.Height != thumbH {
		t.Fatalf("opaque thumb = %s %dx%d (%v), want jpeg 1170x%d", format, cfg.Width, cfg.Height, err, thumbH)
	}

	out, ok = makeThumb(encodePNG(t, noisy(2000, 1000, true)))
	if !ok {
		t.Fatal("a big transparent PNG should shrink")
	}
	img, format, err := image.Decode(bytes.NewReader(out))
	if err != nil || format != "png" {
		t.Fatalf("transparent thumb = %s (%v), want png", format, err)
	}
	if _, _, _, a := img.At(10, 10).RGBA(); a != 0 {
		t.Fatalf("transparent half has alpha %d", a)
	}

	if _, ok := makeThumb(encodePNG(t, noisy(100, 100, false))); ok {
		t.Fatal("a small file goes as it is")
	}
	var j bytes.Buffer
	if err := jpeg.Encode(&j, noisy(900, 600, false), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := makeThumb(j.Bytes()); ok {
		t.Fatal("a JPEG already within the edge goes as it is")
	}
	if _, ok := makeThumb(bytes.Repeat([]byte("x"), thumbMinBytes*2)); ok {
		t.Fatal("not an image goes as it is")
	}
}

func TestReadFileAsThumb(t *testing.T) {
	home := t.TempDir()
	big := encodePNG(t, noisy(2400, 1600, false))
	if err := os.WriteFile(filepath.Join(home, "shot.png"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{home: home, node: true, web: fstest.MapFS{}}).NodeRoutes())
	defer srv.Close()

	get := func(as string) (kind, thumb, size string, n int) {
		res, err := srv.Client().Get(srv.URL + "/api/file?path=~/shot.png&as=" + as)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(res.Body)
		return res.Header.Get("X-File-Kind"), res.Header.Get("X-File-Thumb"), res.Header.Get("X-File-Size"), b.Len()
	}
	kind, thumb, size, n := get("thumb")
	if kind != "image" || thumb != "1" || n >= len(big)/4 {
		t.Fatalf("as=thumb: kind=%q thumb=%q body=%d of %d", kind, thumb, n, len(big))
	}
	if size != strconv.Itoa(len(big)) {
		t.Fatalf("X-File-Size = %s, want the original's %d", size, len(big))
	}
	if kind, thumb, _, n := get("image"); kind != "image" || thumb != "" || n != len(big) {
		t.Fatalf("as=image: kind=%q thumb=%q body=%d, want the original", kind, thumb, n)
	}
}
