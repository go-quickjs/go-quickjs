package quickjs_test

import (
	_ "embed"
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// jsonDifferentialScript holds JSON.parse and JSON.stringify to the
// specification's algorithms, written out in JavaScript. The reference and
// its generators were run on Node 26 too, over a million cases of each kind,
// with no difference.
//
//go:embed testdata/json_differential.js
var jsonDifferentialScript string

// Generated documents, whole and damaged, and generated object graphs,
// whose getters and toJSON methods change what is being written under the
// walk, are given to the built-ins and to the reference, in a runtime that
// keeps what it learns from one case to the next and in a realm of it that
// starts with none of that. The cases are few enough for CI's run under
// checkptr: this is a test of what a fast path loses, not a fuzzer.
func TestJSONDifferential(t *testing.T) {
	count := 600
	if testing.Short() {
		count = 100
	}
	rt := quickjs.New()
	defer rt.Close()
	run := func(t *testing.T, seed int, eval func(string) (quickjs.Value, error)) {
		t.Helper()
		if _, err := eval(jsonDifferentialScript); err != nil {
			t.Fatal(err)
		}
		v, err := eval(fmt.Sprintf("jsonDifferential(%d, %d)", seed, count))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != "" {
			t.Errorf("seed %d:\n%s", seed, got)
		}
	}
	for seed := 1; seed <= 3; seed++ {
		run(t, seed, rt.Eval)
	}
	realm, err := rt.NewRealm()
	if err != nil {
		t.Fatal(err)
	}
	run(t, 4, realm.Eval)
}
