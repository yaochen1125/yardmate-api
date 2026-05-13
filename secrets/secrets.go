// Package secrets parses an env-style file (KEY=VALUE per line) and exposes
// a Vault for typed lookups. See SPEC.md.
package secrets

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Vault holds the key-value pairs parsed from a secrets.env file.
type Vault struct {
	data map[string]string
}

// Load opens path and parses its content.
func Load(path string) (*Vault, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads env-style content from r. See SPEC.md §2 for the supported format.
func Parse(r io.Reader) (*Vault, error) {
	data := make(map[string]string)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("secrets: line %d: missing '='", lineNum)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("secrets: line %d: empty key", lineNum)
		}
		val := strings.TrimSpace(line[eq+1:])
		if n := len(val); n >= 2 {
			first, last := val[0], val[n-1]
			if (first == '"' || first == '\'') && first == last {
				val = val[1 : n-1]
			}
		}
		data[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read: %w", err)
	}
	return &Vault{data: data}, nil
}

// Get returns the value for key, or "" if unset.
func (v *Vault) Get(key string) string { return v.data[key] }

// GetBool parses Get(key) as a boolean. Recognised true values per
// strconv.ParseBool ("true"/"1"/"t" etc.). Anything else — unset or
// unparseable — returns def.
func (v *Vault) GetBool(key string, def bool) bool {
	raw, ok := v.data[key]
	if !ok || raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return b
}

// Has reports whether key was present in the file (even if its value is empty).
func (v *Vault) Has(key string) bool {
	_, ok := v.data[key]
	return ok
}

// Snapshot returns a fresh map containing the requested keys, each lowercased
// for direct JSON encoding (see SPEC.md §3). Missing keys map to "" so the
// response shape is stable during rotation gaps.
func (v *Vault) Snapshot(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = v.data[k]
	}
	return out
}
