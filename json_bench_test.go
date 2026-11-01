package quickjs_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"unicode/utf8"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func BenchmarkJSONParse(b *testing.B) {
	benchJSONDocument(b, false)
}

func BenchmarkJSONStringify(b *testing.B) {
	benchJSONDocument(b, true)
}

// Keep input construction and compilation outside the timed loop. The document
// mixes repeated object shapes, arrays, fractions, escaped text and Unicode.
func benchJSONDocument(b *testing.B, stringify bool) {
	b.Helper()
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{
			"id": i, "name": fmt.Sprintf("Item %d \u00e9", i), "amount": float64(i) * 1.25,
			"active": i%3 == 0, "tags": []string{"a", "b", "c"},
			"dimensions": map[string]any{"width": i % 100, "height": 20.5, "depth": nil},
			"note":       "line\n\"quoted\"",
		}
	}
	doc, err := json.Marshal(map[string]any{"version": 1, "items": items})
	if err != nil {
		b.Fatal(err)
	}
	rt := quickjs.New()
	defer rt.Close()
	setup := `function f() { return JSON.parse(text).items.length; }`
	want := len(items)
	if stringify {
		// The literal sets up the graph without initializing the JSON builtin.
		setup = `var data = ` + string(doc) + `; function f() { return JSON.stringify(data).length; }`
		want = utf8.RuneCount(doc) // the fixture's Unicode characters are all BMP
	} else if err := rt.Set("text", string(doc)); err != nil {
		b.Fatal(err)
	}
	if _, err := rt.Eval(setup); err != nil {
		b.Fatal(err)
	}
	fn, err := rt.Get("f")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	b.ResetTimer()
	var last quickjs.Value
	for b.Loop() {
		last, err = fn.Call()
		if err != nil {
			b.Fatal(err)
		}
	}
	if got := last.String(); got != strconv.Itoa(want) {
		b.Fatalf("got %s, want %d", got, want)
	}
}
