package spot

import "testing"

// TestMatchArtistName covers the artist-disambiguation heuristic used by
// resolveTrack. Conservative on purpose: only exact (normalized) matches
// switch into artist mode, so that ambiguous bare queries like "nine"
// don't get auto-resolved to one of many "Nine ..." artists.
func TestMatchArtistName(t *testing.T) {
	cases := []struct {
		query, name string
		want        bool
	}{
		// Exact matches (case + whitespace normalized).
		{"nine inch nails", "Nine Inch Nails", true},
		{"Nine Inch Nails", "nine inch nails", true},
		{"  the smiths  ", "The Smiths", true},
		{"AFI", "afi", true},
		{"radiohead", "Radiohead", true},
		{"Nine Inch  Nails", "Nine Inch Nails", true}, // collapsed whitespace

		// Not matches — should fall through to track search.
		{"nine", "Nine Inch Nails", false},
		{"nin", "Nine Inch Nails", false},
		{"nine inch nails closer", "Nine Inch Nails", false}, // extra word
		{"the smiths how soon is now", "The Smiths", false},
		{"underworld born slippy", "Underworld", false},

		// Empty / pathological.
		{"", "Underworld", false},
		{"underworld", "", false},
		{"   ", "Underworld", false},
	}

	for _, tc := range cases {
		got := matchArtistName(tc.query, tc.name)
		if got != tc.want {
			t.Errorf("matchArtistName(%q, %q) = %v, want %v", tc.query, tc.name, got, tc.want)
		}
	}
}
