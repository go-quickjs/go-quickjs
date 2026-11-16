package regdiff

import (
	"os"
	"testing"
)

// TestRegDiffCursors compares the scripts of the root package's
// TestCursorReuseMatchesNode and TestAsyncFirstRunOrder in register code:
// for-in loops left by a break, a continue or a return, which mark their
// cursors done for the next loop; destructurings and for-of loops over
// arrays, whose cursors their closes release for the next; a dictionary's
// keys kept by its layout; async functions whose first run is a call. Each
// runs as register code, with no fallback to the other tiers.
func TestRegDiffCursors(t *testing.T) {
	regDiffParallel(t)
	for _, c := range []struct{ file, call string }{
		{"cursor_forin.js", `JSON.stringify(R)`},
		{"cursor_destructure.js", `JSON.stringify(R)`},
		{"async_first_run.js", `JSON.stringify(log)`},
	} {
		src, err := os.ReadFile("../../testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		regDiff(t, string(src), []string{c.call})
	}
}
