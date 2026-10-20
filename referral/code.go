package referral

import (
	"strings"
	"unicode/utf8"
)

// Prefix is what every YardMate invite code starts with. The server strips it
// before looking anything up; it is there so that a code pasted into a chat
// reads as belonging to this app rather than as four stray characters.
const Prefix = "YM-"

// bodyLen is how many characters carry the entropy. Four over the 31-letter
// alphabet below is ~920k codes — far more users than this app will have, and
// short enough to be read off one phone and typed into another.
const bodyLen = 4

// alphabet leaves out 0/O and 1/I/L: every pair this drops is a pair that
// would have been mistyped when the code is spoken aloud.
const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// Normalise turns whatever was typed into the one form the database stores
// ("YM-7KQ2"), or returns false when it cannot be a code.
//
// What arrives is not what was printed: people paste "ym-7kq2", type
// "YM 7KQ2", or drop the prefix because the four characters are the part that
// looks like the code. All of those are the same code. Case is folded, spaces
// and dashes go, and the prefix is optional on the way in and always present
// on the way out.
func Normalise(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r == ' ' || r == '-' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r > utf8.RuneSelf:
			return "", false
		default:
			b.WriteRune(r)
		}
	}
	// The prefix is only taken off when what is left is a whole code: "YM" are
	// two letters of the alphabet, so a bare "YMAB" is the body of YM-YMAB, not
	// a prefix with two characters after it.
	s := b.String()
	head := strings.TrimSuffix(Prefix, "-")
	if len(s) == len(head)+bodyLen && strings.HasPrefix(s, head) {
		s = s[len(head):]
	}
	if len(s) != bodyLen {
		return "", false
	}
	for _, r := range s {
		if !strings.ContainsRune(alphabet, r) {
			return "", false
		}
	}
	return Prefix + s, true
}
