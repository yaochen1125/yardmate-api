package referral

import "testing"

func TestNormaliseTakesWhateverWasTyped(t *testing.T) {
	// All of these are the same code: read off a phone and typed in lower
	// case, pasted with the prefix, spaced out, or with the prefix dropped
	// because the four characters are the part that looks like a code.
	for _, typed := range []string{"YM-7KQ2", "ym-7kq2", "YM7KQ2", "7KQ2", " ym - 7kq2 ", "7kq2", "ym 7kq2", "YM-7KQ2\n"} {
		got, ok := Normalise(typed)
		if !ok || got != "YM-7KQ2" {
			t.Errorf("Normalise(%q) = %q, %v; want YM-7KQ2, true", typed, got, ok)
		}
	}
	// Y and M are letters of the alphabet like any others, so YM-YMAB exists
	// and every way of writing it has to reach it. Typed bare, "YMAB" is the
	// body, not a prefix with two characters after it.
	for _, typed := range []string{"YM-YMAB", "YMYMAB", "ymab", "YMAB"} {
		if got, ok := Normalise(typed); !ok || got != "YM-YMAB" {
			t.Errorf("Normalise(%q) = %q, %v; want YM-YMAB, true", typed, got, ok)
		}
	}
}

func TestNormaliseRefusesWhatCannotBeACode(t *testing.T) {
	// 0, O, 1, I and L are not in the alphabet on purpose: they are what the
	// characters that ARE in it get mistyped as, so a code containing one was
	// never issued and saying so early beats a database lookup.
	for _, typed := range []string{"", "7KQ", "7KQ22", "YM-7KQ0", "YM-7KQO", "YM-7KQ1", "YM-7KQI", "YM-7KQL", "7KQ!", "7KQ㋡", "YM-", "YM"} {
		if got, ok := Normalise(typed); ok {
			t.Errorf("Normalise(%q) = %q, true; want false", typed, got)
		}
	}
}
