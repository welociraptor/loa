package herdr

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailTruncates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"short trimmed", "  hello \n", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"one rune short", "hello", 4, "…ello"},
		{"long", "0123456789", 5, "…56789"},
		{"multibyte", strings.Repeat("é", 6), 3, "…ééé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tail(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("tail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if strings.HasPrefix(got, "…") {
				if n := utf8.RuneCountInString(got); n != tc.n+1 {
					t.Errorf("truncated result has %d runes, want %d", n, tc.n+1)
				}
			}
		})
	}
}
