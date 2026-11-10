package engine

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// What an embedder binding wasm to a host language needs beyond running it:
// linking one import at a time, so that the host can report errors in the
// order its own lookups make them, exceptions the host throws, and the
// subtype checks of the host's values against canonical types.

// CheckImport reports whether ext may be import i of module m: of the kind
// and type the module declares, with the LinkError Instantiate would return.
func CheckImport(m *module.Module, i int, ext Extern) error {
	return checkImport(m, i, ext)
}

// NewException returns an exception of tag carrying vals; host is what the
// host threw, if the exception stands for a value of its own.
func NewException(tag *Tag, vals []Val, host any) *Exception {
	return &Exception{hdr: header{kind: ObjExn}, Tag: tag, Vals: vals, Host: host}
}

// RefIsCanonicalSub reports whether non-null reference r, to a function, a
// struct or an array, is of canonical type c or a subtype of it.
func RefIsCanonicalSub(r Ref, c module.CanonicalIndex) bool {
	if r.IsNull() || r.IsI31() {
		return false
	}
	got := objHeader(r.p).rtt
	if got == nil {
		return false
	}
	want := rttOf(c)
	return got.isSub(want.canon, len(want.supers)-1)
}

// ArrayNumbers returns the elements of the array r refers to, packed as the
// array holds numbers, its length, and whether r refers to an array of
// numbers.
func ArrayNumbers(r Ref) (b []byte, n int, ok bool) {
	if r.IsNull() || r.IsI31() || r.Kind() != ObjArray {
		return nil, 0, false
	}
	a := asArray(r)
	if a.hdr.rtt.refElems {
		return nil, 0, false
	}
	return a.bytes(), a.n, true
}
