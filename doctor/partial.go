package doctor

import (
	"bytes"
	"encoding/json"
)

// CompletedStrings scans a *partial* JSON document for the array named key
// and returns every string element that is already fully closed. It is the
// piece that turns "the model is generating" into visible progress: the
// handler calls it on the accumulated stream and pushes each newly completed
// observation to the client while the rest of the reply is still being
// generated.
//
// Deliberately NOT a general tolerant-JSON parser: the endpoint needs exactly
// one array of strings out of a truncated document, and a targeted scanner is
// both simpler and harder to break than an incremental parser. A standard
// json.Unmarshal can't help — it rejects any incomplete document outright.
//
// Escape handling is delegated to encoding/json by unmarshalling each closed
// string literal on its own; the scanner only finds literal boundaries
// (backslash-aware), it never interprets escapes itself.
func CompletedStrings(key string, partial []byte) []string {
	quotedKey := []byte(`"` + key + `"`)
	anchor := bytes.Index(partial, quotedKey)
	if anchor < 0 {
		return nil
	}
	i := anchor + len(quotedKey)

	// Walk to the array's opening bracket; only whitespace and the colon may
	// sit between the key and the bracket, anything else means this was not
	// the real key position (e.g. the key appearing inside a string value).
	for i < len(partial) && partial[i] != '[' {
		c := partial[i]
		if c != ':' && c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return nil
		}
		i++
	}
	if i >= len(partial) {
		return nil
	}
	i++ // past '['

	var out []string
	for i < len(partial) {
		c := partial[i]
		if c == ']' {
			break
		}
		if c != '"' {
			i++
			continue
		}
		// Find the closing quote, skipping escapes.
		j := i + 1
		closing := -1
		for j < len(partial) {
			if partial[j] == '\\' {
				j += 2
				continue
			}
			if partial[j] == '"' {
				closing = j
				break
			}
			j++
		}
		if closing < 0 {
			break // this element is still streaming; later ones can't be done either
		}
		var s string
		if err := json.Unmarshal(partial[i:closing+1], &s); err == nil {
			out = append(out, s)
		}
		i = closing + 1
	}
	return out
}
