package training

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// baseJPEG encodes a small solid image to real JPEG bytes (starts with SOI +
// APP0/JFIF from the stdlib encoder).
func baseJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// withExif injects a fake APP1 "Exif" segment right after SOI, carrying a
// recognizable GPS-ish marker so we can assert it is gone after stripping.
func withExif(base []byte) []byte {
	payload := append([]byte("Exif\x00\x00"), []byte("GPSLATITUDE-SECRET")...)
	seg := make([]byte, 0, 4+len(payload))
	segLen := len(payload) + 2
	seg = append(seg, 0xFF, 0xE1, byte(segLen>>8), byte(segLen))
	seg = append(seg, payload...)
	out := make([]byte, 0, len(base)+len(seg))
	out = append(out, base[:2]...) // SOI
	out = append(out, seg...)      // APP1 Exif
	out = append(out, base[2:]...) // rest
	return out
}

func TestStripRemovesExif(t *testing.T) {
	base := baseJPEG(t)
	dirty := withExif(base)
	if !bytes.Contains(dirty, []byte("GPSLATITUDE-SECRET")) {
		t.Fatal("test setup: exif marker not present before strip")
	}
	clean, err := stripJPEGMetadata(dirty)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if bytes.Contains(clean, []byte("GPSLATITUDE-SECRET")) {
		t.Error("exif/GPS payload survived stripping")
	}
	if bytes.Contains(clean, []byte("Exif")) {
		t.Error("APP1 Exif marker survived stripping")
	}
	// Still a decodable image of the same size.
	img, err := jpeg.Decode(bytes.NewReader(clean))
	if err != nil {
		t.Fatalf("stripped bytes no longer decode as jpeg: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Errorf("dimensions changed: got %dx%d", b.Dx(), b.Dy())
	}
	if len(clean) >= len(dirty) {
		t.Errorf("stripped output not smaller: %d >= %d", len(clean), len(dirty))
	}
}

func TestStripKeepsCleanImageDecodable(t *testing.T) {
	base := baseJPEG(t)
	clean, err := stripJPEGMetadata(base)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatalf("clean jpeg failed to decode after strip: %v", err)
	}
}

// A crafted multi-frame JPEG (frame1 EOI, then an appended frame carrying GPS)
// must have the trailing frame — and its GPS — dropped. This is the gap a naive
// "copy from SOS to end of buffer" leaves open for modified/non-iOS clients.
func TestStripDropsTrailingFrame(t *testing.T) {
	frame1 := baseJPEG(t)           // valid single frame, ends in EOI
	frame2 := withExif(baseJPEG(t)) // second frame, carries GPSLATITUDE-SECRET in APP1
	dirty := append(append([]byte{}, frame1...), frame2...)

	clean, err := stripJPEGMetadata(dirty)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if bytes.Contains(clean, []byte("GPSLATITUDE-SECRET")) {
		t.Error("GPS from an appended trailing frame survived stripping")
	}
	// Output must still be a decodable primary image.
	if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatalf("stripped multi-frame output no longer decodes: %v", err)
	}
	// Trailing frame dropped → output no larger than frame1 alone.
	if len(clean) > len(frame1) {
		t.Errorf("trailing frame not dropped: clean=%d frame1=%d", len(clean), len(frame1))
	}
}

func TestStripRejectsNonJPEG(t *testing.T) {
	cases := map[string][]byte{
		"empty":     nil,
		"png-magic": {0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
		"truncated": {0xFF, 0xD8, 0xFF, 0xE1, 0x00}, // SOI + start of APP1 then cut
		"garbage":   []byte("not an image at all"),
	}
	for name, in := range cases {
		if _, err := stripJPEGMetadata(in); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
