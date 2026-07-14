package training

import "errors"

// errNotJPEG is returned when the bytes are not a parseable JPEG. Callers map it
// to a 400 (it doubles as image validation).
var errNotJPEG = errors.New("training: not a parseable jpeg")

// stripJPEGMetadata removes metadata segments (Exif/GPS/XMP/thumbnails, comments)
// from a JPEG by DROPPING the APP1..APP15 and COM markers, keeping APP0 (JFIF)
// and the entropy-coded image data. It does NOT re-encode, so there is zero
// added quality loss — important because these are training photos (a re-encode
// would stack JPEG artifacts that hurt L2 downstream).
//
// iOS already strips by re-encoding from a bare bitmap; this is defense-in-depth
// on the server. Any input that is not a well-formed JPEG returns errNotJPEG.
//
// JPEG structure: SOI (FFD8), then a sequence of marker segments. A standalone
// marker (RSTn / SOI / EOI) has no length; every other marker is followed by a
// 2-byte big-endian length that INCLUDES those 2 bytes. SOS (FFDA) is followed
// by entropy-coded data that runs until the next marker; we copy the rest of the
// stream verbatim from SOS onward.
func stripJPEGMetadata(in []byte) ([]byte, error) {
	if len(in) < 2 || in[0] != 0xFF || in[1] != 0xD8 {
		return nil, errNotJPEG
	}
	out := make([]byte, 0, len(in))
	out = append(out, 0xFF, 0xD8) // SOI
	i := 2
	for i+1 < len(in) {
		if in[i] != 0xFF {
			return nil, errNotJPEG
		}
		// Skip fill bytes (0xFF padding) before a marker.
		marker := in[i+1]
		for marker == 0xFF && i+2 < len(in) {
			i++
			marker = in[i+1]
		}
		switch {
		case marker == 0xD9: // EOI
			out = append(out, 0xFF, 0xD9)
			return out, nil
		case marker == 0xDA: // SOS — copy this segment header + all trailing data
			out = append(out, in[i:]...)
			return out, nil
		case marker >= 0xD0 && marker <= 0xD7: // RSTn — standalone, no payload
			out = append(out, 0xFF, marker)
			i += 2
			continue
		case marker == 0x01: // TEM — standalone
			out = append(out, 0xFF, marker)
			i += 2
			continue
		}
		// Length-prefixed segment.
		if i+4 > len(in) {
			return nil, errNotJPEG
		}
		segLen := int(in[i+2])<<8 | int(in[i+3]) // includes the 2 length bytes
		if segLen < 2 || i+2+segLen > len(in) {
			return nil, errNotJPEG
		}
		drop := (marker >= 0xE1 && marker <= 0xEF) || // APP1..APP15 (Exif/XMP/…)
			marker == 0xFE // COM (comment)
		if !drop {
			out = append(out, in[i:i+2+segLen]...)
		}
		i += 2 + segLen
	}
	// Ran off the end without hitting SOS/EOI — not a usable JPEG.
	return nil, errNotJPEG
}
