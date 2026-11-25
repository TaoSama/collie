package collie

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// A photo or screenshot in the chat shows at a few hundred pixels, so the
// computer it is on shrinks it before it crosses the network (?as=thumb).
// The lightbox still reads the original.
const (
	thumbW, thumbH = 1320, 780 // a 440×260 preview (app.css .md-thumb) on a 3x screen
	thumbMinBytes  = 96 << 10  // smaller files go as they are
	thumbMaxPixels = 50e6      // bigger ones would take too long to decode
	thumbQuality   = 82
)

// thumbSlots caps how many pictures are decoded at once: a chat full of
// screenshots asks for all of them together.
var thumbSlots = make(chan struct{}, 2)

// makeThumb returns a smaller copy of the PNG or JPEG in b: JPEG when it is
// opaque, PNG otherwise. ok is false when b should go as it is (small, another
// format such as an animated GIF, or the copy would not be smaller).
func makeThumb(b []byte) (out []byte, ok bool) {
	if len(b) < thumbMinBytes {
		return nil, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width*cfg.Height > thumbMaxPixels {
		return nil, false
	}
	scale := min(1, float64(thumbW)/float64(cfg.Width), float64(thumbH)/float64(cfg.Height))
	if scale == 1 && format == "jpeg" {
		return nil, false
	}
	thumbSlots <- struct{}{}
	defer func() { <-thumbSlots }()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, false
	}
	src := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(src, src.Bounds(), img, img.Bounds().Min, draw.Src)
	dst := src
	if scale < 1 {
		dst = shrink(src, max(1, int(float64(cfg.Width)*scale)), max(1, int(float64(cfg.Height)*scale)))
	}
	var buf bytes.Buffer
	if dst.Opaque() {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbQuality})
	} else {
		err = png.Encode(&buf, dst)
	}
	if err != nil || buf.Len() >= len(b) {
		return nil, false
	}
	return buf.Bytes(), true
}

// shrink averages each w×h cell of src into one pixel (premultiplied, so
// transparent edges stay clean).
func shrink(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	var sum [4]int
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, max((y+1)*sh/h, y*sh/h+1)
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, max((x+1)*sw/w, x*sw/w+1)
			sum = [4]int{}
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride+x0*4 : sy*src.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					sum[0] += int(row[i])
					sum[1] += int(row[i+1])
					sum[2] += int(row[i+2])
					sum[3] += int(row[i+3])
				}
			}
			n := (y1 - y0) * (x1 - x0)
			o := y*dst.Stride + x*4
			for c := range sum {
				dst.Pix[o+c] = uint8((sum[c] + n/2) / n)
			}
		}
	}
	return dst
}
