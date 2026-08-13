package doctor

import (
	"reflect"
	"testing"
)

func TestCompletedStrings(t *testing.T) {
	cases := []struct {
		name    string
		partial string
		want    []string
	}{
		{
			name:    "key absent",
			partial: `{"photoProblem":null,"other":[`,
			want:    nil,
		},
		{
			name:    "array opened, first string incomplete",
			partial: `{"photoProblem":null,"observations":["Leaves are dro`,
			want:    nil,
		},
		{
			name:    "one complete one streaming",
			partial: `{"observations":["Leaves drooping","Soil looks dr`,
			want:    []string{"Leaves drooping"},
		},
		{
			name:    "all complete array closed",
			partial: `{"observations":["a","b","c"],"healthLevel":`,
			want:    []string{"a", "b", "c"},
		},
		{
			name:    "escaped quote inside string",
			partial: `{"observations":["he said \"dry\"","next`,
			want:    []string{`he said "dry"`},
		},
		{
			name:    "unicode escape decoded by encoding/json",
			partial: `{"observations":["étiolated growth","x`,
			want:    []string{"étiolated growth"},
		},
		{
			name:    "backslash at chunk boundary keeps element pending",
			partial: `{"observations":["path \`,
			want:    nil,
		},
		{
			name:    "whitespace between key colon and bracket",
			partial: "{\"observations\" :\n [\"a\"",
			want:    []string{"a"},
		},
		{
			name:    "plain mention in a prior value does not shadow the key",
			partial: `{"note":"the observations","observations":["real"`,
			// `"observations"` (with quotes) does not occur inside the value
			// — the value's own quotes don't neighbor the word — so the
			// anchor lands on the real key.
			want: []string{"real"},
		},
		{
			name:    "escaped-quoted mention cannot fake the anchor",
			partial: `{"note":"say \"observations\" plainly","observations":["real"`,
			// Inside a JSON string value every quote is escaped, so the bytes
			// read `\"observations\"` — the backslash breaks the adjacency the
			// needle `"observations"` requires. String values therefore can
			// never shadow the key; the anchor lands on the real key.
			want: []string{"real"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CompletedStrings("observations", []byte(tc.partial))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCompletedStringsGrowsMonotonically(t *testing.T) {
	full := `{"observations":["one","two","three"],"spokenSummary":"done"}`
	seen := 0
	for i := 0; i <= len(full); i++ {
		got := CompletedStrings("observations", []byte(full[:i]))
		if len(got) < seen {
			t.Fatalf("prefix %d: count went backwards (%d -> %d)", i, seen, len(got))
		}
		seen = len(got)
	}
	if seen != 3 {
		t.Fatalf("final count = %d, want 3", seen)
	}
}
