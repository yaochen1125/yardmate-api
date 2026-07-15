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
// by entropy-coded data that runs until the next real marker.
//
// We parse THROUGH the scan data rather than copying "from SOS to end of buffer"
// so that we stop at the primary image's EOI and DROP anything appended after it.
// A crafted multi-picture (MPO/MPF) JPEG can carry a second frame — with its own
// APP1/GPS — after the first frame's EOI; a naive "copy to end" would pass that
// GPS through. Since this strip is defense-in-depth against arbitrary/modified
// clients (the endpoint accepts any caller), it must hold for that input too.
// Progressive JPEGs (multiple SOS + tables between scans) are handled by the same
// loop: each scan's entropy run is copied, then we resume marker processing.
func stripJPEGMetadata(in []byte) ([]byte, error) {
	if len(in) < 2 || in[0] != 0xFF || in[1] != 0xD8 {
		return nil, errNotJPEG
	}
	out := make([]byte, 0, len(in))
	out = append(out, 0xFF, 0xD8) // SOI
	i := 2
	sawSOS := false // require actual scan data before we accept an EOI
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
		case marker == 0xD9: // EOI — end of the primary image. Drop trailing frames.
			if !sawSOS {
				// SOI…EOI with no scan (e.g. FF D8 FF D9) sniffs as image/jpeg but
				// has no image data — reject rather than store an undecodable file.
				return nil, errNotJPEG
			}
			out = append(out, 0xFF, 0xD9)
			return out, nil
		case marker >= 0xD0 && marker <= 0xD7: // RSTn — standalone, no payload
			out = append(out, 0xFF, marker)
			i += 2
			continue
		case marker == 0x01: // TEM — standalone
			out = append(out, 0xFF, marker)
			i += 2
			continue
		case marker == 0xDA: // SOS — copy the header segment, then the scan data.
			if i+4 > len(in) {
				return nil, errNotJPEG
			}
			segLen := int(in[i+2])<<8 | int(in[i+3])
			if segLen < 2 || i+2+segLen > len(in) {
				return nil, errNotJPEG
			}
			sawSOS = true
			out = append(out, in[i:i+2+segLen]...) // SOS header
			i += 2 + segLen
			// Copy entropy-coded data up to (not including) the next real marker.
			// Inside the scan, 0xFF is either a stuffed byte (FF00) or a restart
			// marker (FFD0-D7); both are part of the scan. Any other 0xFF<marker>
			// (EOI, or the next SOS/table in a progressive image) ends this run.
			j := i
			for j+1 < len(in) {
				if in[j] != 0xFF {
					j++
					continue
				}
				nb := in[j+1]
				if nb == 0x00 || (nb >= 0xD0 && nb <= 0xD7) {
					j += 2 // stuffed byte / restart marker → part of the scan
					continue
				}
				if nb == 0xFF {
					j++ // fill byte before a marker
					continue
				}
				break // real marker
			}
			out = append(out, in[i:j]...)
			i = j
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
	// Ran off the end without hitting the primary EOI — not a usable JPEG.
	return nil, errNotJPEG
}
