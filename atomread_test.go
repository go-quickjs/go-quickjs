package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestReadingKeysKeepsNothing pins that reading, testing and deleting
// properties by names nothing has used leaves nothing behind: each used to
// intern its name for the runtime's lifetime, so a script that only read
// o["key" + i] grew without bound (KI-07).
func TestReadingKeysKeepsNothing(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`var o = {present: 1}`); err != nil {
		t.Fatal(err)
	}
	before := heapInUse()
	v, err := rt.Eval(`
		let found = 0;
		for (let i = 0; i < 2e6; i++) {
			const k = "key_" + i + "_padding_padding_padding";
			if (o[k] !== undefined || k in o || o.hasOwnProperty(k) || Object.hasOwn(o, k)) found++;
			delete o[k];
			if (o[Symbol()] !== undefined) found++;
		}
		found`)
	if err != nil || v.Int() != 0 {
		t.Fatalf("= %v, %v", v, err)
	}
	if kept := heapInUse() - before; kept > 16<<20 {
		t.Errorf("%d MB kept after reading two million new keys", kept>>20)
	}
}
