//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package jit

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// testValue is a VM value's layout: a number word and a pointer word.
type testValue struct {
	num uint64
	ref unsafe.Pointer
}

const testTagBase = 0xFFF8000000000000

// testUpvalue is a captured binding's cell, as far as native code reads it.
type testUpvalue struct{ slot *testValue }

// testPoison fills a frame's entries where captured bindings are numbered,
// which native code must never read or write: their values are in cells.
var testPoison = testValue{num: 0x0123456789abcdef}

// testString is a string's layout as far as native code reads it.
type testString struct {
	s           string
	left, right *testString
	length      int
	ascii       bool
	u16         *uint16
}

// testText is a string's spec: its code units, and how it is held -- flat
// ASCII, flat with its UTF-16 units cached, flat without them (which
// native code leaves to Go), or a rope (likewise).
type testText struct {
	units []uint16
	form  int
}

const (
	textASCII = iota
	textCached
	textUncached
	textRope
)

func randomText(r *rand.Rand) testText {
	t := testText{form: r.IntN(4)}
	for range r.IntN(5) {
		u := uint16('a' + r.IntN(26))
		if t.form == textCached || t.form == textUncached || r.IntN(2) == 0 {
			// An ASCII string's units are ASCII, as its flag says; the
			// number is drawn all the same, so the programs drawn after
			// it are the same.
			if n := r.IntN(0x2000); t.form != textASCII {
				u = uint16(0x100 + n)
			}
		}
		t.units = append(t.units, u)
	}
	return t
}

// testWTF8 is units' WTF-8, the VM's UTF-8 form of a flat string, which it
// compares by its bytes: a pair is one code point, a lone surrogate three
// bytes of its own.
func testWTF8(units []uint16) string {
	var b []byte
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		if u >= 0xD800 && u < 0xDC00 && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] < 0xE000 {
			b = utf8.AppendRune(b, utf16.DecodeRune(u, rune(units[i+1])))
			i++
			continue
		}
		if u >= 0xD800 && u < 0xE000 {
			b = append(b, 0xE0|byte(u>>12), 0x80|byte(u>>6&0x3F), 0x80|byte(u&0x3F))
			continue
		}
		b = utf8.AppendRune(b, u)
	}
	return string(b)
}

// testCharCodeAt is the handle of the object that is charCodeAt, the
// intrinsic, in a heap whose context has it; testStringShape the shape of
// the String.prototype that holds it.
const testCharCodeAt = 6

var testStringShape = &testShape{keys: []testKey{{key: 15}}}

// testObject is an object's layout as far as native code reads it: its
// side fields are its own, which ext points to when it has them, as an
// array always does.
type testObject struct {
	class    uint8
	flags    uint8
	arrayLen uint32
	ext      *testObjectExt
	shape    *testShape
	vals     []testValue
	proto    *testObject
	testObjectExt
}

// shapeID is the object's shape's address, which native code compares with
// a site's.
func (o *testObject) shapeID() uintptr { return uintptr(unsafe.Pointer(o.shape)) }

// testShape is a shape as far as native code reads one: its properties'
// keys and flags, at their values' positions in an object of it. An object
// with properties has one: one of testShapes, which sites name, or one of
// its own, which none do.
type testShape struct {
	keys []testKey
}

// testKey is a property's key and flags.
type testKey struct {
	key   uint32
	flags uint8
}

// newTestShape is the shape of the given keys and flags.
func newTestShape(keys []uint32, flags []uint8) *testShape {
	s := &testShape{}
	for i, k := range keys {
		s.keys = append(s.keys, testKey{k, flags[i]})
	}
	return s
}

// testObjectExt is an object's side fields (abi.Encoding's ObjectExt).
type testObjectExt struct {
	elems []testValue
}

// testHolders are the prototypes test objects may have, which property
// sites may name (ssa.Holder): the first's prototype is the second. Their
// addresses do not change, as the VM's are kept alive; native code only
// reads them, and their properties hold numbers and booleans, the same in
// every heap.
var testHolders = func() *[2]testObject {
	h := new([2]testObject)
	h[0] = testObject{shape: newTestShape([]uint32{12, 13}, []uint8{testWritable, 0}), proto: &h[1],
		vals: []testValue{{num: math.Float64bits(7)}, {num: testEncoding.True}}}
	h[1] = testObject{shape: newTestShape([]uint32{11, testAccessorKey}, []uint8{testWritable, testAccessor}),
		vals: []testValue{{num: math.Float64bits(-2)}, {num: testTagBase | 9, ref: unsafe.Pointer(&testPair)}}}
	return h
}()

// testHolderValues are testHolders' properties' values, as the SSA
// evaluator has them: the second's second is its accessor, whose pair the
// evaluator knows by its cell (ssa.Heap's Accessors).
var testHolderValues = [2][]ir.Value{{ir.Float(7), ir.Bool(true)}, {ir.Float(-2), {Kind: ir.Opaque, Bits: 98}}}

// testAccessorKey is the key of testHolders[1]'s accessor, testPair, whose
// getter and setter are testGetter and testSetter: objects of no kind,
// which native code only compares (ssa.OpAccessorIs).
const testAccessorKey = 16

type testAccessorPair struct{ getter, setter unsafe.Pointer }

var (
	testGetter, testSetter, testOther byte
	testPair                          = testAccessorPair{unsafe.Pointer(&testGetter), unsafe.Pointer(&testSetter)}
)

// testAccessorSite is an accessor's site (ssa.AccessorSite): a read of
// testHolders[1]'s accessor, or a write, from a receiver of one of
// testShapes whose prototype is testHolders[0], sometimes compiled for
// another function than the pair has; inlined, the getter returning a
// number or its receiver, the setter nothing -- callees with nothing to
// leave for, whose frames an exit never makes.
func testAccessorSite(g *rand.Rand, write bool) ssa.AccessorSite {
	h0 := ssa.Holder{Object: uintptr(unsafe.Pointer(&testHolders[0])), Shape: testHolders[0].shapeID()}
	h1 := ssa.Holder{Object: uintptr(unsafe.Pointer(&testHolders[1])), Shape: testHolders[1].shapeID()}
	site := ssa.PropertySite{Key: testAccessorKey, Shape: testShapes[g.IntN(len(testShapes))].shape, Index: 1,
		Holders: [2]ssa.Holder{h0, h1}}
	if g.IntN(3) == 0 {
		o := testShapes[g.IntN(len(testShapes))].shape
		if o != site.Shape {
			site.Cases = []ssa.PropertyCase{{Shape: o, Index: 1, Holders: site.Holders}}
		}
	}
	fn := &testGetter
	if write {
		fn = &testSetter
	}
	if g.IntN(4) == 0 {
		fn = &testOther
	}
	ret := ir.Literal(ir.Float(5))
	callee := &ir.Program{Locals: 1}
	this := -1
	switch {
	case write:
		// The value, its one parameter, is not returned: its result is
		// dropped anyway.
		ret = ir.Literal(ir.Value{Kind: ir.Undefined})
	case g.IntN(2) == 0:
		ret, this = ir.Slot(0), 0
	}
	callee.Code = []ir.Instruction{{Op: ir.Return, Left: ret}}
	callee.Maps = []ir.StateMap{{PC: 0}}
	in := &ssa.InlineSite{Program: callee, Feedback: layout{}, Callee: uintptr(unsafe.Pointer(fn)), Argc: b2i(write), Method: true,
		Params: b2i(write), ThisSlot: this}
	return ssa.AccessorSite{Property: site, Function: uintptr(unsafe.Pointer(fn)), Setter: write, Inline: in}
}

// b2i is 1 for true, 0 for false.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// testHolderSite is a site found on a prototype: the receiver of one of
// testShapes, then one or two of testHolders, the last sometimes of a
// shape it does not have. Sometimes it met receivers of other shapes too
// (ssa.PropertyCase): with the same prototypes, or the property their own.
func testHolderSite(r *rand.Rand) site {
	sh := testShapes[r.IntN(len(testShapes))]
	s := site{Shape: sh.shape}
	h0 := ssa.Holder{Object: uintptr(unsafe.Pointer(&testHolders[0])), Shape: testHolders[0].shapeID()}
	h1 := ssa.Holder{Object: uintptr(unsafe.Pointer(&testHolders[1])), Shape: testHolders[1].shapeID()}
	if r.IntN(2) == 0 {
		i := r.IntN(len(testHolders[0].vals))
		s.Key, s.Index, s.Holders = testHolders[0].shape.keys[i].key, int32(i), [2]ssa.Holder{h0}
	} else {
		s.Key, s.Index, s.Holders = testHolders[1].shape.keys[0].key, 0, [2]ssa.Holder{h0, h1}
	}
	if r.IntN(5) == 0 {
		last := 0
		if s.Holders[1].Object != 0 {
			last = 1
		}
		s.Holders[last].Shape = 0x9000
	}
	if r.IntN(2) == 0 {
		for _, o := range testShapes {
			if o.shape == sh.shape || r.IntN(3) == 0 {
				continue
			}
			k := ssa.PropertyCase{Shape: o.shape, Index: s.Index, Holders: s.Holders}
			for i, key := range o.keys {
				if key == s.Key && o.flags[i]&testEncoding.PropNotData == 0 {
					k = ssa.PropertyCase{Shape: o.shape, Index: int32(i)}
				}
			}
			s.Cases = append(s.Cases, k)
		}
	}
	return s
}

// testShapes are the shapes test objects have, each with its table's keys
// and flags, and its address, which a site's feedback names (or one no
// object has).
var testShapes = func() []testShapeSpec {
	specs := []testShapeSpec{
		{keys: []uint32{10, 11, 12}, flags: []uint8{testWritable, testWritable, 0}},
		{keys: []uint32{11}, flags: []uint8{testWritable}},
		{keys: []uint32{12, 10}, flags: []uint8{testWritable, testAccessor}},
	}
	for i := range specs {
		specs[i].s = newTestShape(specs[i].keys, specs[i].flags)
		specs[i].shape = uintptr(unsafe.Pointer(specs[i].s))
	}
	return specs
}()

type testShapeSpec struct {
	s     *testShape
	shape uintptr
	keys  []uint32
	flags []uint8
}

// testBarrier is the harness's write-barrier flag (abi.Encoding's
// WriteBarrier), which a heap sets as its global object's spec says
// (testArray.barrier) when it is made for a run.
var testBarrier uint8

// testFlagHTMLDDA is the flags byte's [[IsHTMLDDA]] bit.
const testFlagHTMLDDA = 0x40

const (
	testClassObject = 0
	testClassArray  = 2
	testFlagSparse  = 4

	// A property's flags.
	testWritable = 1 << iota
	testAccessor
	testDeleted
	testPrivate
	testUninit
)

// testFlags are the flags a shapeless object's properties have.
var testFlags = []uint8{testWritable, testWritable, 0, testAccessor, testDeleted, testPrivate | testWritable, testUninit | testWritable}

// testEncoding mirrors the VM's tags (internal/vm/value.go), and its
// objects as testObject lays them out.
var testEncoding = abi.Encoding{
	ValueSize: 16, NumOffset: 0, RefOffset: 8,
	Undefined: testTagBase | 1, Null: testTagBase | 2,
	True: testTagBase | 1<<8 | 3, False: testTagBase | 3,
	Uninitialized: testTagBase | 8, CanonicalNaN: 0x7FF8000000000000,
	Object:      testTagBase | 7,
	ObjectClass: int32(unsafe.Offsetof(testObject{}.class)), ObjectFlags: int32(unsafe.Offsetof(testObject{}.flags)),
	ObjectExt: int32(unsafe.Offsetof(testObject{}.ext)), ExtElems: int32(unsafe.Offsetof(testObjectExt{}.elems)),
	ObjectArrayLen: int32(unsafe.Offsetof(testObject{}.arrayLen)),
	ObjectElems:    int32(unsafe.Offsetof(testObject{}.testObjectExt) + unsafe.Offsetof(testObjectExt{}.elems)),
	ClassArray:     testClassArray, FlagSparse: testFlagSparse, FlagHTMLDDA: testFlagHTMLDDA,
	UpvalueSlot: int32(unsafe.Offsetof(testUpvalue{}.slot)),
	String:      testTagBase | 4, StringData: int32(unsafe.Offsetof(testString{}.s)),
	StringLeft: int32(unsafe.Offsetof(testString{}.left)), StringLength: int32(unsafe.Offsetof(testString{}.length)),
	StringASCII: int32(unsafe.Offsetof(testString{}.ascii)), StringU16: int32(unsafe.Offsetof(testString{}.u16)),
	ObjectShape: int32(unsafe.Offsetof(testObject{}.shape)), ObjectProps: int32(unsafe.Offsetof(testObject{}.vals)),
	ObjectProto:  int32(unsafe.Offsetof(testObject{}.proto)),
	WriteBarrier: uint64(uintptr(unsafe.Pointer(&testBarrier))),
	StoreRef:     StoreRef(),
	ShapeKeys:    int32(unsafe.Offsetof(testShape{}.keys)), KeySize: int32(unsafe.Sizeof(testKey{})),
	PropertyKey: int32(unsafe.Offsetof(testKey{}.key)), PropertyFlags: int32(unsafe.Offsetof(testKey{}.flags)),
	ClassObject: testClassObject, PropNotData: testAccessor | testDeleted | testPrivate,
	PropNotWritable: testAccessor | testDeleted | testPrivate | testUninit | testWritable, PropWritable: testWritable,
	PropUninit: testUninit,
	Mega:       true,
	// The cell a read of a property no object has finds (abi.MegaAbsent).
	UndefinedCell: uint64(uintptr(unsafe.Pointer(&testUndefinedCell))),
	Accessor:      testTagBase | 9, AccessorGetter: int32(unsafe.Offsetof(testAccessorPair{}.getter)),
	AccessorSetter: int32(unsafe.Offsetof(testAccessorPair{}.setter)),
}

// testUndefinedCell holds undefined, which nothing writes.
var testUndefinedCell = testValue{num: testTagBase | 1}

// testMegaOverride, if not nil, is the megamorphic cache every heap's code
// probes, a test's own.
var testMegaOverride *abi.MegaCache

// testStaleShape is a shape no object has, which a stale cache entry
// names for a prototype.
var testStaleShape testShape

// testMegas are megamorphic caches (abi.MegaCache) the harness's code
// probes, as the VM's Go fills its: an empty one; and, for receivers of
// testShapes, entries of their own data properties, writable or not, and
// of testHolders' along the chains to them, some of them stale -- a
// prototype's shape not the one it has now -- each placed in its set as
// the VM places it (megaPut). A heap's code runs with the one its spec
// says (nativeHeap.mega), and the SSA evaluator reads the same.
var testMegas = func() [3]*abi.MegaCache {
	var out [3]*abi.MegaCache
	g := rand.New(rand.NewPCG(7, 8))
	put := func(m *abi.MegaCache, n abi.MegaEntry) {
		set := m[abi.MegaSet(uintptr(n.Shape), n.Key):][:abi.MegaWays]
		at := len(set) - 1
		for w := range set {
			if set[w].Shape == n.Shape && set[w].Key == n.Key {
				at = w
				break
			}
		}
		copy(set[1:at+1], set[:at])
		set[0] = n
	}
	link := func(i int, stale bool) abi.MegaLink {
		l := abi.MegaLink{Object: unsafe.Pointer(&testHolders[i]), Shape: unsafe.Pointer(testHolders[i].shape)}
		if stale {
			l.Shape = unsafe.Pointer(&testStaleShape)
		}
		return l
	}
	for v := range out {
		m := new(abi.MegaCache)
		out[v] = m
		if v == 0 {
			continue
		}
		for _, spec := range testShapes {
			for i, key := range spec.keys {
				if g.IntN(2) == 0 && spec.flags[i]&testEncoding.PropNotData == 0 {
					idx := uint32(i)
					if spec.flags[i]&testEncoding.PropNotWritable == testWritable {
						idx |= abi.MegaWritable
					}
					put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: key, Index: idx})
				}
			}
			for j, k := range testHolders[0].shape.keys {
				if g.IntN(2) == 0 {
					put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: k.key, Index: uint32(j),
						Chain: &abi.MegaChain{link(0, g.IntN(4) == 0)}})
				}
			}
			if g.IntN(2) == 0 {
				put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: testHolders[1].shape.keys[0].key, Index: 0,
					Chain: &abi.MegaChain{link(0, false), link(1, g.IntN(4) == 0)}})
			}
			// Key 14, which nothing has: absent along no prototype, along
			// the second holder, or along both.
			switch g.IntN(4) {
			case 0:
				put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: 14, Index: abi.MegaAbsent, Chain: &abi.MegaChain{}})
			case 1:
				put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: 14, Index: abi.MegaAbsent, Chain: &abi.MegaChain{link(1, g.IntN(4) == 0)}})
			case 2:
				put(m, abi.MegaEntry{Shape: unsafe.Pointer(spec.s), Key: 14, Index: abi.MegaAbsent,
					Chain: &abi.MegaChain{link(0, g.IntN(4) == 0), link(1, false)}})
			}
		}
	}
	return out
}()

// testHeap is what handles 0 to 4 name, as in package ssa's tests: an
// array of numbers, an array with holes and other tags among them, an
// object that is not an array, an array whose length runs past its dense
// elements, and the global object.
type testHeap []testArray

type testArray struct {
	cells  []uint64 // number words
	length uint64
	array  bool
	shape  int // into testShapes, or -1 for none
	// texts are the strings handles 5 and 6 name, and intrinsic says
	// whether the context has charCodeAt: the global object's spec has them.
	texts     []testText
	intrinsic bool
	keys      []uint32 // the table's keys, flags and values' number words
	flags     []uint8
	props     []ir.Value
	// htmldda gives the object Annex B's [[IsHTMLDDA]].
	htmldda bool
	// proto is the object's prototype, of testHolders, or -1 for none.
	proto int
	// barrier, in the global object's spec, sets the write-barrier flag.
	barrier bool
}

func randomTestHeap(r *rand.Rand) testHeap {
	h := make(testHeap, 4)
	for i := range h {
		a := testArray{cells: make([]uint64, r.IntN(6)), array: i != 2, htmldda: r.IntN(4) == 0, proto: r.IntN(4) - 2}
		for j := range a.cells {
			switch k := r.IntN(6); {
			case i == 1 && k == 0:
				a.cells[j] = testEncoding.Uninitialized
			case i == 1 && k == 1:
				a.cells[j] = testEncoding.True
			default:
				a.cells[j] = math.Float64bits(float64(r.IntN(9) - 3))
			}
		}
		a.length = uint64(len(a.cells))
		if i == 3 {
			a.length += uint64(r.IntN(4))
		}
		a.shape = r.IntN(len(testShapes)+1) - 1
		if i == 2 && r.IntN(2) == 0 {
			// The ordinary object: often shapeless, so that it is searched.
			a.shape = -1
		}
		if a.shape >= 0 {
			a.keys, a.flags = testShapes[a.shape].keys, testShapes[a.shape].flags
		} else {
			for range r.IntN(4) {
				a.keys = append(a.keys, 10+uint32(r.IntN(4)))
				a.flags = append(a.flags, testFlags[r.IntN(len(testFlags))])
			}
		}
		a.props = make([]ir.Value, len(a.keys))
		for j := range a.props {
			switch r.IntN(8) {
			case 0:
				a.props[j] = ir.Bool(true)
			case 1, 2:
				// A reference: to one of the heap's objects, another, or text.
				a.props[j] = ir.Value{Kind: ir.Opaque, Bits: uint64(r.IntN(6))}
			case 3:
				a.props[j] = ir.Value{Kind: ir.String, Bits: 5}
			default:
				a.props[j] = ir.Float(float64(r.IntN(9) - 3))
			}
		}
		h[i] = a
	}
	// The global object, handle 4: an ordinary object whose bindings may
	// be in their temporal dead zone; its length's bits are which of the
	// keys 10 to 13 script-level lexical bindings shadow.
	g := testArray{shape: -1, proto: -1, length: uint64(r.IntN(16)) & uint64(r.IntN(16)),
		texts: []testText{randomText(r), randomText(r)}, intrinsic: r.IntN(4) != 0, barrier: r.IntN(4) == 0}
	if t := g.texts; (len(t[0].units)+len(t[1].units))%3 == 0 {
		// Often two strings of one content, held as two: flat, or one a
		// rope. Decided by what was drawn, drawing nothing more.
		t[1].units = slices.Clone(t[0].units)
		if t[1].form != textRope {
			t[1].form = t[0].form
		}
	}
	for range 4 {
		g.keys = append(g.keys, 10+uint32(r.IntN(4)))
		g.flags = append(g.flags, testFlags[r.IntN(len(testFlags))])
		g.props = append(g.props, ir.Float(float64(r.IntN(9)-3)))
		if r.IntN(4) == 0 {
			g.props[len(g.props)-1] = ir.Value{Kind: ir.Opaque, Bits: uint64(r.IntN(4))}
		}
	}
	h = append(h, g)
	return h
}

// nativeHeap is a heap's objects. Native code reads them; the evaluators
// read and write the same cells through views. Every other handle refers to
// an object that is not an array, and every string to text, each its own,
// so that a reference copied from the wrong slot shows.
type nativeHeap struct {
	objects []testObject
	others  [8]testObject
	texts   [8]testString
	specs   []testText // handles 5 and 6's
	// intrinsic is the context's charCodeAt cell, and strProto the
	// String.prototype native code reads the method in (abi.Context's
	// StringProto): its one property holds the intrinsic.
	intrinsic testValue
	strProto  testObject
	// evaluated is the objects' property values for the SSA evaluator.
	evaluated [][]ir.Value
	// lexNames is the names script-level lexical bindings have, a bit for
	// each key: those of the global object's spec's length's bits.
	lexNames []uint64
	// mega is the megamorphic cache its code probes, of testMegas.
	mega *abi.MegaCache
}

func (h testHeap) native() *nativeHeap {
	// The objects are made first, at the addresses they keep, so that
	// properties can refer to them.
	n := &nativeHeap{objects: make([]testObject, len(h))}
	if len(h) > 0 {
		// Chosen by the spec, which draws nothing more.
		n.mega = testMegas[len(h[0].cells)%len(testMegas)]
	}
	if testMegaOverride != nil {
		n.mega = testMegaOverride
	}
	for i, a := range h {
		o := &n.objects[i]
		o.elems = make([]testValue, len(a.cells))
		for j, c := range a.cells {
			o.elems[j].num = c
		}
		if a.array {
			o.class = testClassArray
		}
		if a.length > uint64(len(a.cells)) {
			o.flags, o.arrayLen = testFlagSparse, uint32(a.length)
		}
		if a.array || len(a.cells) > 0 || o.flags&testFlagSparse != 0 {
			o.ext = &o.testObjectExt
		}
		if a.htmldda {
			o.flags |= testFlagHTMLDDA
		}
		if a.shape >= 0 && slices.Equal(testShapes[a.shape].keys, a.keys) && slices.Equal(testShapes[a.shape].flags, a.flags) {
			o.shape = testShapes[a.shape].s
		} else if len(a.props) > 0 {
			// A layout of its own, which no site names: a shapeless
			// object's, or one a test says is of another shape than its
			// keys are.
			o.shape = newTestShape(a.keys, a.flags)
		}
		if a.proto >= 0 {
			o.proto = &testHolders[a.proto]
		}
		o.vals = make([]testValue, len(a.props))
		for j, v := range a.props {
			o.vals[j] = n.word(v)
		}
		n.evaluated = append(n.evaluated, slices.Clone(a.props))
	}
	testBarrier = 0
	if len(h) > 4 && h[4].barrier {
		testBarrier = 1
	}
	if len(h) > 4 {
		n.lexNames = []uint64{h[4].length << 10}
		n.specs = h[4].texts
		for i, t := range h[4].texts {
			s := &n.texts[5+i]
			s.length = len(t.units)
			switch t.form {
			case textASCII:
				b := make([]byte, len(t.units))
				for j, u := range t.units {
					b[j] = byte(u)
				}
				s.s, s.ascii = string(b), true
			case textCached:
				s.s = testWTF8(t.units)
				if len(t.units) > 0 {
					s.u16 = &slices.Clone(t.units)[0]
				}
			case textUncached:
				s.s = testWTF8(t.units)
			case textRope:
				// A rope's flags are valid, as the VM's are; its UTF-8 is not.
				s.left, s.right = &testString{}, &testString{}
				s.ascii = !slices.ContainsFunc(t.units, func(u uint16) bool { return u >= 0x80 })
			}
		}
		if h[4].intrinsic {
			n.intrinsic = n.word(ir.Value{Kind: ir.Opaque, Bits: testCharCodeAt})
			n.strProto = testObject{shape: testStringShape, vals: []testValue{n.intrinsic}}
		}
	}
	return n
}

// heap is what the SSA evaluator reads: views of the arrays' cells, and the
// objects' shapes with its own copy of their property words.
func (n *nativeHeap) heap() ssa.Heap {
	h := ssa.Heap{Arrays: n.views(), Word: testWordValue, Holders: map[uintptr]*ssa.Object{}, Mega: n.mega}
	h.Accessors = map[*ir.Value]ssa.Accessor{}
	for i := range testHolders {
		o := &testHolders[i]
		m := &ssa.Object{Shape: o.shapeID(), Proto: uintptr(unsafe.Pointer(o.proto)), Props: slices.Clone(testHolderValues[i])}
		h.Holders[uintptr(unsafe.Pointer(o))] = m
		for j, k := range o.shape.keys {
			if k.flags&testAccessor != 0 {
				h.Accessors[&m.Props[j]] = ssa.Accessor{Getter: uintptr(unsafe.Pointer(&testGetter)), Setter: uintptr(unsafe.Pointer(&testSetter))}
			}
		}
	}
	for i := range n.objects {
		o := &n.objects[i]
		e := ssa.Object{Shape: o.shapeID(), Ordinary: o.class == testClassObject, Props: n.evaluated[i], HTMLDDA: o.flags&testFlagHTMLDDA != 0,
			Proto: uintptr(unsafe.Pointer(o.proto))}
		var keys []testKey
		if o.shape != nil {
			keys = o.shape.keys[:len(o.vals)]
		}
		for _, p := range keys {
			e.Keys = append(e.Keys, p.key)
			e.Data = append(e.Data, p.flags&testEncoding.PropNotData == 0)
			e.Writable = append(e.Writable, p.flags&testEncoding.PropNotWritable == testWritable)
			e.Uninit = append(e.Uninit, p.flags&testUninit != 0)
		}
		h.Objects = append(h.Objects, e)
	}
	if len(h.Objects) > 4 {
		h.Strings = map[uint64]ssa.String{}
		for i, t := range n.specs {
			// Read natively when flat and at hand: ASCII, or cached.
			flat := t.form == textASCII || t.form == textCached || len(t.units) == 0 && t.form != textRope
			h.Strings[uint64(5+i)] = ssa.String{Units: t.units, Flat: flat, Rope: t.form == textRope}
		}
		if n.intrinsic.ref != nil {
			h.CharCodeAt = &ir.Value{Kind: ir.Opaque, Bits: testCharCodeAt}
		}
		h.Global, h.Lexical = &h.Objects[4], map[uint32]bool{}
		for k := uint32(10); k < 14; k++ {
			if n.lexNames[0]&(1<<k) != 0 {
				h.Lexical[k] = true
			}
		}
	}
	return h
}

// views are the slot IR's views of the objects, as the VM grants them.
func (n *nativeHeap) views() []ir.ArrayView {
	vs := make([]ir.ArrayView, ir.MaxSlots)
	for i := range n.objects {
		o := &n.objects[i]
		v := ir.ArrayView{DenseLength: uint64(len(o.elems)), Length: uint64(len(o.elems))}
		if o.flags&testFlagSparse != 0 && uint64(o.arrayLen) > v.Length {
			v.Length = uint64(o.arrayLen)
		}
		if len(o.elems) > 0 {
			v.Data = unsafe.Pointer(&o.elems[0])
		}
		if o.class != testClassArray {
			// The VM grants no view of an object that is not an array.
			continue
		}
		v.NumberLimit = testTagBase
		vs[i] = v
	}
	return vs
}

// same reports whether native code's instance holds the same words as the
// evaluator's: elements, and properties.
func (n *nativeHeap) same(m *nativeHeap) bool {
	for i := range n.objects {
		if !slices.Equal(n.objects[i].elems, m.objects[i].elems) {
			return false
		}
		for j, v := range n.objects[i].vals {
			if v != n.word(m.evaluated[i][j]) {
				return false
			}
		}
	}
	return true
}

// word encodes a slot IR value as the VM would hold it.
// testWordValue decodes an element's word: a number, or one of the
// encoding's primitives. Elements here hold no reference.
func testWordValue(w uint64) ir.Value {
	switch w {
	case testEncoding.Undefined:
		return ir.Value{Kind: ir.Undefined}
	case testEncoding.Null:
		return ir.Value{Kind: ir.Null}
	case testEncoding.True:
		return ir.Bool(true)
	case testEncoding.False:
		return ir.Bool(false)
	case testEncoding.Uninitialized:
		return ir.Value{Kind: ir.Uninitialized}
	}
	return ir.Value{Kind: ir.Number, Bits: w}
}

func (n *nativeHeap) word(v ir.Value) testValue {
	switch v.Kind {
	case ir.Number:
		if math.IsNaN(math.Float64frombits(v.Bits)) {
			return testValue{num: testEncoding.CanonicalNaN}
		}
		return testValue{num: v.Bits}
	case ir.Undefined:
		return testValue{num: testEncoding.Undefined}
	case ir.Null:
		return testValue{num: testEncoding.Null}
	case ir.Boolean:
		if v.Bits != 0 {
			return testValue{num: testEncoding.True}
		}
		return testValue{num: testEncoding.False}
	case ir.Uninitialized:
		return testValue{num: testEncoding.Uninitialized}
	}
	// A reference: every object has one word, as in the VM, and so here does
	// every string; the pointer tells them apart.
	if v.Kind == ir.String {
		return testValue{num: testEncoding.String, ref: unsafe.Pointer(&n.texts[v.Bits%8])}
	}
	if v.Bits < uint64(len(n.objects)) {
		return testValue{num: testEncoding.Object, ref: unsafe.Pointer(&n.objects[v.Bits])}
	}
	return testValue{num: testEncoding.Object, ref: unsafe.Pointer(&n.others[v.Bits%8])}
}

// referenceExits counts the harness's exit records -- references copied,
// primitives stored over references, sources known only at run time, and
// among them heap cells -- returned references, and exits that left their
// frames to Go (abi.ExitTable), so that the test can tell it reaches them.
var referenceExits struct{ copies, scalars, maybes, cells, returns, tables int }

// source is the value a run-time source names (origin.go): the value at
// its address, one of the state's slots or a heap cell, which it counts; nil
// for a primitive's 0. word holds the source, as native code wrote it.
func source(word *uint64, at func(int) *testValue, slots int) *testValue {
	if *word == 0 {
		return nil
	}
	v := *(**testValue)(unsafe.Pointer(word))
	for i := range slots {
		if at(i) == v {
			return v
		}
	}
	referenceExits.cells++
	return v
}

// applyRecords does what Go does with an exit's records (abi.Record),
// checking that each names a slot of the state once.
func applyRecords(ctx *abi.Context, at func(int) *testValue, slots int) error {
	n := int(ctx.Records)
	if n > slots {
		return fmt.Errorf("%d records for %d slots", n, slots)
	}
	seen := map[uint64]bool{}
	src := make([]testValue, n)
	for i, r := range ctx.Record[:n] {
		slot := r.Slot &^ (abi.RecordScalar | abi.RecordMaybe)
		if slot >= uint64(slots) || seen[slot] {
			return fmt.Errorf("record %d: slot %d of %d, or twice", i, slot, slots)
		}
		seen[slot] = true
		switch {
		case r.Slot&abi.RecordScalar != 0:
			if at(int(slot)).ref == nil {
				return fmt.Errorf("record %d stores into slot %d, which holds no reference", i, slot)
			}
			src[i] = testValue{num: r.Word}
			referenceExits.scalars++
		case r.Slot&abi.RecordMaybe != 0:
			src[i] = testValue{num: r.Word}
			if v := source(&ctx.Record[i].Arg, at, slots); v != nil && v.ref != nil {
				src[i] = *v
			}
			referenceExits.maybes++
		default:
			if r.Arg >= uint64(slots) || at(int(r.Arg)).ref == nil {
				return fmt.Errorf("record %d copies slot %d, which holds no reference", i, r.Arg)
			}
			src[i] = *at(int(r.Arg))
			referenceExits.copies++
		}
	}
	for i, r := range ctx.Record[:n] {
		*at(int(r.Slot &^ (abi.RecordScalar | abi.RecordMaybe))) = src[i]
	}
	return nil
}

var nativeTestValues = []ir.Value{
	ir.Float(0), ir.Float(math.Copysign(0, -1)), ir.Float(1), ir.Float(-1), ir.Float(0.5), ir.Float(3),
	ir.Float(1 << 31), ir.Float(1<<32 + 1), ir.Float(-(1 << 31) - 1), ir.Float(math.NaN()),
	ir.Float(math.Inf(1)), ir.Float(math.Inf(-1)), ir.Float(5e-324), ir.Float(1e300), ir.Float(-1e300),
	ir.Float(9007199254740993), ir.Float(1 << 63), ir.Float(-(1 << 63)), ir.Float(1 << 62), ir.Float(4294967295.5),
	ir.Bool(true), ir.Bool(false), {Kind: ir.Undefined}, {Kind: ir.Null}, {Kind: ir.Uninitialized},
	{Kind: ir.Opaque, Bits: 3}, {Kind: ir.Opaque, Bits: 4}, {Kind: ir.String, Bits: 5},
	{Kind: ir.Opaque, Bits: 0}, {Kind: ir.Opaque, Bits: 1}, {Kind: ir.Opaque, Bits: 2}, {Kind: ir.String, Bits: 6},
	{Kind: ir.Opaque, Bits: testCharCodeAt}, {Kind: ir.Opaque, Bits: testCharCodeAt},
}

func nativeTestValue(r *rand.Rand) ir.Value {
	if r.IntN(3) == 0 {
		return ir.Float(float64(r.IntN(20) - 5))
	}
	v := nativeTestValues[r.IntN(len(nativeTestValues))]
	if v.Kind == ir.Number && math.IsNaN(math.Float64frombits(v.Bits)) {
		v.Bits = testEncoding.CanonicalNaN
	}
	return v
}

// layout is where a program's locals are: the first frame of them are the
// frame's, and the rest captured bindings, or the receiver at slot this
// (-1 if none), which the program reads and never writes. It also holds
// what the VM knows of the program's property sites, by PC.
type layout struct {
	frame, this int
	sites       map[int]site
	globals     map[int]ssa.GlobalSite
	// generic and genericEntries are the sites and entries whose
	// speculation failed before, by PC (ssa.Feedback's Generic).
	generic, genericEntries map[int]bool
	// accessors are the property sites that found an accessor, by PC
	// (ssa.AccessorFeedback).
	accessors map[int]ssa.AccessorSite
}

// Accessor is ssa.AccessorFeedback's.
func (l layout) Accessor(pc int) (ssa.AccessorSite, bool) {
	s, ok := l.accessors[pc]
	return s, ok
}

// Inline is ssa.Feedback's: the harness inlines nothing.
func (l layout) Inline(int) (ssa.InlineSite, bool) { return ssa.InlineSite{}, false }

// NativeCalls is ssa.Feedback's: the harness calls nothing natively.
func (l layout) NativeCalls(int) []ssa.CallSite { return nil }

// Generic and EntryGeneric are ssa.Feedback's.
func (l layout) Generic(pc int) bool      { return l.generic[pc] }
func (l layout) EntryGeneric(pc int) bool { return l.genericEntries[pc] }

// Global is ssa.Feedback's.
func (l layout) Global(pc int) (ssa.GlobalSite, bool) {
	s, ok := l.globals[pc]
	return s, ok
}

// site is a property site's feedback.
type site = ssa.PropertySite

// Property is ssa.Feedback's.
func (l layout) Property(pc int) (ssa.PropertySite, bool) {
	s, ok := l.sites[pc]
	return s, ok
}

// testProgramSize bounds the programs ssaTestProgram makes: frame locals
// and instructions. A test may raise it, as TestSSANativeHighPressure does.
var testProgramSize = struct{ locals, code int }{6, 24}

// ssaTestProgram makes a program and says where its locals are.
func ssaTestProgram(r *rand.Rand) (*ir.Program, layout) {
	frameLocals := 1 + r.IntN(testProgramSize.locals)
	locals := frameLocals + r.IntN(3)
	n := 2 + r.IntN(testProgramSize.code)
	operand := func() ir.Operand {
		if r.IntN(4) == 0 {
			v := nativeTestValue(r)
			if v.Kind == ir.Opaque || v.Kind == ir.String {
				v = ir.Float(2)
			}
			return ir.Literal(v)
		}
		return ir.Slot(r.IntN(locals))
	}
	ops := []ir.Op{ir.Copy, ir.Binary, ir.Binary, ir.Binary, ir.Unary, ir.Update, ir.Update, ir.Branch, ir.Branch,
		ir.Jump, ir.Swap, ir.CopyPair, ir.StoreLoad, ir.Host, ir.Nop,
		ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayKey, ir.ArrayUpdate, ir.PropertyRead, ir.PropertyWrite,
		ir.ReferenceRead, ir.ReferenceRead, ir.BindingRead, ir.BindingRead, ir.StringMethod, ir.StringCode, ir.StringCode}
	operators := []ir.Operator{ir.Add, ir.Sub, ir.Mul, ir.Div, ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq, ir.Ne,
		ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr, ir.UShr, ir.Mod, ir.Mod}
	p := &ir.Program{Locals: locals}
	sites, globals := map[int]site{}, map[int]ssa.GlobalSite{}
	for pc := 0; pc < n; pc++ {
		in := ir.Instruction{Op: ops[r.IntN(len(ops))], Left: operand(), Right: operand(), Third: operand(),
			Dest: r.IntN(frameLocals), Extra: r.IntN(frameLocals), Target: r.IntN(n + 1), Postfix: r.IntN(2) == 0, When: r.IntN(2) == 0}
		switch in.Op {
		case ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayKey:
			in.Left = ir.Slot(r.IntN(locals))
			// Some reads carry the element by its cell, whatever it holds.
			in.Reference = in.Op == ir.ArrayRead && r.IntN(3) == 0
		case ir.StringMethod:
			in.Left = ir.Slot(r.IntN(locals))
		case ir.StringCode:
			in.Left, in.Right = ir.Slot(r.IntN(locals)), ir.Slot(r.IntN(locals))
		case ir.BindingRead:
			in.Left = ir.Literal(ir.Value{Kind: ir.Undefined})
			if r.IntN(4) != 0 {
				globals[pc] = ssa.GlobalSite{Key: 10 + uint32(r.IntN(4)), Index: int32(r.IntN(5)) - 1}
			}
		case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
			in.Left = ir.Slot(r.IntN(locals))
			// Mostly a site the VM knows: a key, and sometimes a shape with the
			// property's index -- plain data, and writable for a write, as the
			// VM's caches remember -- or a shape no object has.
			if r.IntN(4) != 0 {
				s := site{Key: 10 + uint32(r.IntN(4))}
				if s.Key == 13 && pc%3 == 0 {
					// A key no object has, which the megamorphic cache may
					// say none of a receiver's prototypes has (abi.MegaAbsent).
					s.Key = 14
				}
				switch r.IntN(3) {
				case 0:
					sh := testShapes[r.IntN(len(testShapes))]
					i := r.IntN(len(sh.keys))
					f := sh.flags[i]
					if f&testEncoding.PropNotData == 0 && (in.Op == ir.PropertyRead || f&testEncoding.PropNotWritable == testWritable) {
						s.Key, s.Shape, s.Index = sh.keys[i], sh.shape, int32(i)
					}
				case 1:
					s.Shape = 0x9000
				}
				// A read sometimes found its property on a prototype.
				if in.Op != ir.PropertyWrite && r.IntN(3) == 0 {
					s = testHolderSite(r)
				}
				sites[pc] = s
			}
		case ir.ArrayUpdate:
			in.Left = ir.Slot(r.IntN(locals))
			in.Right = ir.Slot(in.Extra)
			in.Operator = []ir.Operator{ir.Add, ir.Sub}[r.IntN(2)]
		case ir.Binary:
			in.Operator = operators[r.IntN(len(operators))]
		case ir.Unary:
			in.Operator = []ir.Operator{ir.Neg, ir.Pos, ir.Not, ir.Int32, ir.BitNot}[r.IntN(5)]
		case ir.Update:
			in.Operator = []ir.Operator{ir.Add, ir.Sub}[r.IntN(2)]
			if r.IntN(2) == 0 {
				in.Extra = -1
			}
		case ir.Branch:
			in.Operator = []ir.Operator{ir.Truth, ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq, ir.Ne}[r.IntN(7)]
		}
		if (in.Op == ir.Binary || in.Op == ir.Branch) && (in.Operator == ir.Eq || in.Operator == ir.Ne) {
			// Strict or loose, often against null or undefined, which the new
			// pipeline compares natively whatever the other operand is.
			in.Strict = r.IntN(2) == 0
			if r.IntN(2) == 0 {
				nullish := ir.Literal(ir.Value{Kind: []ir.Kind{ir.Null, ir.Undefined}[r.IntN(2)]})
				if r.IntN(2) == 0 {
					in.Left, in.Right = in.Right, nullish
				} else {
					in.Right = nullish
				}
			}
		}
		if r.IntN(8) == 0 {
			in.Check, in.CheckSlot = true, r.IntN(locals)
		}
		p.Code = append(p.Code, in)
	}
	p.Code = append(p.Code, ir.Instruction{Op: ir.Return, Left: operand()})
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	this := -1
	if locals > frameLocals && r.IntN(2) == 0 {
		this = locals - 1
	}
	// A few operations and entries are built as after their speculation
	// failed (ssa.Feedback's Generic).
	// They are drawn from a generator of their own, so the programs drawn
	// after this one are what they were before there were any.
	g := rand.New(rand.NewPCG(uint64(len(p.Code)), uint64(frameLocals)))
	generic, genericEntries := map[int]bool{}, map[int]bool{}
	for pc := range p.Code {
		if g.IntN(10) == 0 {
			generic[pc] = true
		}
		if g.IntN(8) == 0 {
			genericEntries[pc] = true
		}
	}
	// Drawn as when it chose whether every exit came back at an entry,
	// which no code is compiled for now, so that what is drawn after is as
	// it was.
	_ = g.IntN(2)
	// Some property sites found an accessor, drawn last, so that what was
	// drawn before is as it was.
	accessors := map[int]ssa.AccessorSite{}
	for pc := range p.Code {
		// Some reads have met more shapes than their cases hold, and look
		// in the megamorphic cache past their own tables.
		if s, ok := sites[pc]; ok && g.IntN(2) == 0 {
			s.Megamorphic = true
			sites[pc] = s
		}
	}
	for pc, in := range p.Code {
		if (in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite || in.Op == ir.ReferenceRead) && g.IntN(4) == 0 {
			accessors[pc] = testAccessorSite(g, in.Op == ir.PropertyWrite)
		}
	}
	return p, layout{frameLocals, this, sites, globals, generic, genericEntries, accessors}
}

var exitNames = map[uint64]ir.ExitKind{abi.ExitReturn: ir.Returned, abi.ExitDeopt: ir.GuardExit,
	abi.ExitHost: ir.HostExit, abi.ExitPoll: ir.BudgetExit}

// compiled is a program compiled by the new pipeline, for the harness.
// testArena holds every program the harness compiles, packed next to each
// other, as a runtime's are.
var testArena = NewArena()

type compiled struct {
	f    *ssa.Func
	mc   *mir.Code
	code *SSACode
}

// compileNative builds, optimizes and compiles p; it returns nil when the
// SSA builder does not support p.
func compileNative(p *ir.Program, l layout) (*compiled, error) {
	f, err := ssa.BuildWith(p, l)
	if errors.Is(err, ssa.ErrUnsupported) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.FrameLocals, f.ThisSlot = l.frame, l.this
	ssa.Optimize(f)
	mc, err := mir.Compile(f, testEncoding)
	if errors.Is(err, mir.ErrUnsupported) && !strings.Contains(err.Error(), "runtime error") {
		// A refusal, such as more values live at once than there are spill
		// slots: not compiled, as the builder's refusals are not. A
		// backend's own panic -- an index out of range -- is a bug, which a
		// refusal would hide.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("CompileAMD64: %w\n%s", err, f)
	}
	code, err := NewSSACode(testArena, mc)
	if err != nil {
		return nil, err
	}
	return &compiled{f, mc, code}, nil
}

// nativeMismatch enters native code and the SSA evaluator at pc on the same
// slots, with polls every poll back-edges (0: none), and describes any
// difference in exit, return word or frame; "" if there is none.
func nativeMismatch(c *compiled, pc int, slots []ir.Value, poll int, heap testHeap) string {
	f := c.f
	nh, eh := heap.native(), heap.native()
	// One more value past the slots, the address of the stack's top within
	// the frame's allocation where there are no operands.
	frame := make([]testValue, len(slots), len(slots)+1)
	// Captured bindings live in cells, and the receiver in the context;
	// their numbers' entries in the frame are poison.
	ctx := &abi.Context{}
	ctx.SetConsts(&testEncoding)
	this := (*testValue)(unsafe.Pointer(&ctx.This))
	captured := make([]testValue, f.Locals-f.FrameLocals)
	cells := make([]*testUpvalue, len(captured))
	for i, v := range slots {
		frame[i] = nh.word(v)
		if k := i - f.FrameLocals; k >= 0 && i < f.Locals {
			captured[k], frame[i] = frame[i], testPoison
			cells[k] = &testUpvalue{slot: &captured[k]}
			if i == f.ThisSlot {
				*this = captured[k]
				captured[k] = testPoison
			}
		}
	}
	at := func(i int) *testValue {
		if i == f.ThisSlot {
			return this
		}
		if k := i - f.FrameLocals; k >= 0 && i < f.Locals {
			return &captured[k]
		}
		return &frame[i]
	}
	want := append([]ir.Value(nil), slots...)
	wantExit, err := ssa.EvaluateHeap(f, pc, want, eh.heap(), poll)
	if errors.Is(err, ssa.ErrUnsupported) {
		// A run the model does not make -- a reference stored in an
		// element -- is not compared, as a program the builder refuses is
		// not compiled; the VM's tests run such code.
		return ""
	}
	if err != nil {
		return err.Error()
	}
	// Without polls, a program finishes within 2,000 back-edges (finishes);
	// native code that runs on far past that has gone wrong, and its poll
	// says so.
	counter := 1 << 20
	if poll > 0 {
		counter = poll
	}
	ctx.Locals, ctx.BackEdges, ctx.Mega = unsafe.Pointer(&frame[0]), &counter, nh.mega
	if f.StackSize > 0 {
		ctx.Stack = unsafe.Pointer(&frame[f.Locals])
	}
	// Room past the frame for an inlined callee's (ssa.OpFrameRoom), and
	// contexts past this one: the VM's stack, as the harness's, is ample.
	ctx.Stack = unsafe.Add(unsafe.Pointer(unsafe.SliceData(frame)), f.Locals*16)
	ctx.StackBase, ctx.StackEnd, ctx.Bounds = unsafe.Pointer(unsafe.SliceData(frame)), uint64(len(frame)+1024), &abi.Bounds{LevelLimit: 64}
	if len(cells) > 0 {
		ctx.Upvalues = unsafe.Pointer(&cells[0])
	}
	if len(nh.objects) > 4 {
		ctx.Global, ctx.LexNames = unsafe.Pointer(&nh.objects[4]), unsafe.Pointer(&nh.lexNames)
		*(*testValue)(unsafe.Pointer(&ctx.CharCodeAt)) = nh.intrinsic
		if nh.intrinsic.ref != nil {
			ctx.StringProto, ctx.StringShape = unsafe.Pointer(&nh.strProto), uint64(uintptr(unsafe.Pointer(testStringShape)))
			ctx.CharCodeAtOffset = 0
		}
	}
	if err := c.code.Run(pc, ctx); err != nil {
		return err.Error()
	}
	if ctx.ExitKind == abi.ExitTable {
		referenceExits.tables++
	}
	ApplyExit(ctx)
	got := ir.Exit{Kind: exitNames[ctx.ExitKind], State: ir.StateMap{PC: uint32(ctx.ExitPC), Depth: int(ctx.ExitDepth)}}
	report := func(why string) string {
		return fmt.Sprintf("%s: from pc %d, poll %d, slots %v, heap %v, %d frame locals, receiver %d\nssa    %+v slots %v\nnative %+v ret %#x frame %v captured %v receiver %v",
			why, pc, poll, slots, heap, f.FrameLocals, f.ThisSlot, wantExit, want, got, ctx.Ret, frame, captured, *this)
	}
	if got.Kind != wantExit.Kind {
		return report("exit kind")
	}
	if got.Kind == ir.Returned {
		ret := testValue{num: ctx.Ret}
		if v := source(&ctx.RetFrom, at, f.Locals); v != nil && v.ref != nil {
			ret = *v
			referenceExits.returns++
		}
		if ret != nh.word(wantExit.Value) {
			return report("return value")
		}
		if !nh.same(eh) {
			return report("elements")
		}
		return ""
	}
	if err := applyRecords(ctx, at, f.Locals+int(ctx.ExitDepth)); err != nil {
		return report(err.Error())
	}
	if got.State != wantExit.State {
		return report("exit state")
	}
	for i := 0; i < f.Locals+wantExit.State.Depth; i++ {
		if w := nh.word(want[i]); *at(i) != w {
			return report(fmt.Sprintf("slot %d", i))
		}
	}
	for i := f.FrameLocals; i < f.Locals; i++ {
		if frame[i] != testPoison {
			return report(fmt.Sprintf("frame entry %d, a captured binding's number", i))
		}
	}
	if !nh.same(eh) {
		return report("elements")
	}
	return ""
}

// finishes reports whether f, entered at pc, exits within 2,000 back-edges.
// Feedback lets native code go further than the slot IR, which leaves every
// property operation to Go, so the SSA evaluator itself is asked.
func finishes(f *ssa.Func, pc int, slots []ir.Value, heap testHeap) bool {
	exit, err := ssa.EvaluateHeap(f, pc, append([]ir.Value(nil), slots...), heap.native().heap(), 2000)
	return err == nil && exit.Kind != ir.BudgetExit
}

// minimize shrinks a failing program, turning instructions into Nop while
// it still validates, compiles and fails the same way from the same entry.
func minimize(p *ir.Program, l layout, pc int, slots []ir.Value, poll int, heap testHeap) (*ir.Program, string) {
	fails := func(q *ir.Program) string {
		if q.Validate() != nil {
			return ""
		}
		c, err := compileNative(q, l)
		if err != nil || c == nil {
			return ""
		}
		defer c.code.Close()
		// Removing an instruction can make a loop endless; without polls
		// neither side would come back.
		if !c.code.HasEntry(pc) || poll == 0 && !finishes(c.f, pc, slots, heap) {
			return ""
		}
		return nativeMismatch(c, pc, append([]ir.Value(nil), slots...), poll, heap)
	}
	best := fails(p)
	for changed := true; changed; {
		changed = false
		for i := range p.Code {
			if p.Code[i].Op == ir.Nop || p.Code[i].Op == ir.Return {
				continue
			}
			q := *p
			q.Code = append([]ir.Instruction(nil), p.Code...)
			q.Code[i] = ir.Instruction{Op: ir.Nop}
			if why := fails(&q); why != "" {
				p, best, changed = &q, why, true
			}
		}
	}
	return p, best
}

// checkNative compiles p and compares native code with the SSA evaluator
// from every entry on random slots. A failure is minimized and reported with
// the function, its allocation and the program.
func checkNative(t *testing.T, r *rand.Rand, p *ir.Program, l layout) (ok bool) {
	t.Helper()
	c, err := compileNative(p, l)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		return false
	}
	defer c.code.Close()
	for _, e := range c.f.Entries {
		for trial := 0; trial < 6; trial++ {
			slots := make([]ir.Value, p.Locals+p.StackSize)
			for i := range slots {
				slots[i] = nativeTestValue(r)
			}
			heap := randomTestHeap(r)
			// Mostly primitives, so that code runs past the guards.
			if trial < 2 {
				for i, v := range slots {
					if v.Kind == ir.Opaque || v.Kind == ir.String {
						slots[i] = ir.Float(7)
					}
				}
			}
			for _, poll := range []int{0, 1, 3} {
				// Without polls, a program that loops forever would never
				// come back from either side.
				if poll == 0 && !finishes(c.f, e.PC, slots, heap) {
					continue
				}
				if why := nativeMismatch(c, e.PC, slots, poll, heap); why != "" {
					small, smallWhy := minimize(p, l, e.PC, slots, poll, heap)
					mc, _ := compileNative(small, l)
					detail := ""
					if mc != nil {
						detail = mc.f.String() + "\n" + mc.mc.Locations()
						mc.code.Close()
					}
					t.Fatalf("%s\n\nminimized: %s\n%s\nprogram: %+v", why, smallWhy, detail, small.Code)
				}
			}
		}
	}
	return true
}

func TestSSANativeMatchesEvaluator(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	compiled := 0
	defer func(table bool) { mir.TableExits = table }(mir.TableExits)
	for attempt := 0; attempt < 100000 && compiled < 2000; attempt++ {
		p, l := ssaTestProgram(r)
		if p.Validate() != nil {
			continue
		}
		// Every other program leaves its frames to Go to write (ApplyExit),
		// the rest write them themselves, records and all, which the
		// counters count.
		mir.TableExits = compiled%2 == 1
		if checkNative(t, r, p, l) {
			compiled++
		}
	}
	if compiled < 1000 {
		t.Fatalf("only %d programs compiled", compiled)
	}
	t.Logf("%d programs; exits with references: %+v", compiled, referenceExits)
	if referenceExits.copies == 0 || referenceExits.scalars == 0 || referenceExits.maybes == 0 || referenceExits.cells == 0 || referenceExits.returns == 0 || referenceExits.tables == 0 {
		t.Fatalf("some kind of record or reference return never happened: %+v", referenceExits)
	}
}

// A compile in warm workspaces allocates little beyond what the code keeps
// (its exits' descriptions): a runtime compiles at run time, and what a
// compile leaves the collector is paid by the program. The budget is a
// little over what random programs take now, so that a pass allocating per
// value again shows here: 49 a compile (51.5 with Go 1.24) without accessor
// sites; 56.2 (59.0) since a quarter of their property sites are
// accessors' and megamorphic -- each inlined getter or setter a frame of
// its own, each site's out-of-line paths a closure (mir's cold), which
// are per site, not per value. Their sites' feedback and their guards'
// operands come from the workspace (accessorSite, guardOf): 58.6 (61.3)
// before.
func TestWorkspaceCompileAllocations(t *testing.T) {
	r := rand.New(rand.NewPCG(41, 42))
	var ws ssa.Workspace
	var mws mir.Workspace
	type job struct {
		p *ir.Program
		l layout
	}
	compile := func(j job) bool {
		defer ws.Rewind()
		defer mws.Rewind()
		f, err := ssa.BuildIn(&ws, j.p, j.l)
		if err != nil {
			return false
		}
		f.FrameLocals, f.ThisSlot = j.l.frame, j.l.this
		ssa.Optimize(f)
		_, err = mir.CompileIn(&mws, f, testEncoding)
		return err == nil
	}
	var jobs []job
	for attempt := 0; attempt < 20000 && len(jobs) < 200; attempt++ {
		p, l := ssaTestProgram(r)
		if p.Validate() == nil && compile(job{p, l}) {
			jobs = append(jobs, job{p, l})
		}
	}
	if len(jobs) < 200 {
		t.Fatalf("only %d programs compiled", len(jobs))
	}
	allocs := testing.AllocsPerRun(3, func() {
		for _, j := range jobs {
			compile(j)
		}
	}) / float64(len(jobs))
	t.Logf("%.1f allocations a compile", allocs)
	if allocs > 62 {
		t.Fatalf("%.1f allocations a compile", allocs)
	}
}

// What code will hold once placed (CodeFootprint) is what it holds then
// (SSACode's Footprint): its instructions, as placed, and what describes
// its entries and every exit, each exit's inlined frames and the slots
// they write, each described once for the code. A
// workspace counts what it keeps for the next compile (Bytes), and the
// arena what it has mapped.
func TestCodeFootprint(t *testing.T) {
	r := rand.New(rand.NewPCG(51, 52))
	var ws ssa.Workspace
	var mws mir.Workspace
	arena := NewArena()
	placed, listed, distinct := 0, 0, 0
	for attempt := 0; attempt < 2000 && placed < 50; attempt++ {
		p, l := ssaTestProgram(r)
		if p.Validate() != nil {
			continue
		}
		f, err := ssa.BuildIn(&ws, p, l)
		if err != nil {
			ws.Rewind()
			continue
		}
		f.FrameLocals, f.ThisSlot = l.frame, l.this
		ssa.Optimize(f)
		mc, err := mir.CompileIn(&mws, f, testEncoding)
		if err != nil {
			ws.Rewind()
			mws.Rewind()
			continue
		}
		if ws.Bytes() == 0 || mws.Bytes() == 0 {
			t.Fatal("a workspace in use counts nothing")
		}
		want := len(mc.Bytes)
		var frame *abi.ExitFrame
		for _, x := range mc.Exits {
			want += int(unsafe.Sizeof(*x)) + len(x.Inline)*int(unsafe.Sizeof(abi.ExitInline{}))
			listed += int(x.Count)
			if x.Frame != nil && x.Frame != mc.Exits[0].Frame {
				t.Fatal("a code's exit descriptions do not share one frame")
			}
			if x.Frame != frame {
				distinct += len(x.Frame.Slots)
				frame = x.Frame
				want += int(unsafe.Sizeof(*frame)) + len(frame.Slots)*int(unsafe.Sizeof(abi.ExitSlot{})) + len(frame.Index)*2 + len(frame.Words)*8
			}
		}
		before := CodeFootprint(mc)
		code, err := NewSSACode(arena, mc)
		ws.Rewind()
		mws.Rewind()
		if err != nil {
			t.Fatal(err)
		}
		if got := code.Footprint(); got != before || got < want+len(mc.Entries)*4 {
			t.Fatalf("footprint %d placed, %d before, want at least %d", got, before, want+len(mc.Entries)*4)
		}
		if mapped, live := arena.Mapped(); live < code.Size() || mapped < live {
			t.Fatalf("arena: %d mapped, %d live, code %d", mapped, live, code.Size())
		}
		if err := code.Close(); err != nil || code.Footprint() != 0 {
			t.Fatalf("closed code holds %d: %v", code.Footprint(), err)
		}
		placed++
	}
	if placed < 50 {
		t.Fatalf("only %d programs placed", placed)
	}
	if mapped, live := arena.Mapped(); mapped != 0 || live != 0 {
		t.Fatalf("empty arena maps %d, %d live", mapped, live)
	}
	// Each slot an exit writes is described once in the code's frame
	// (abi.ExitFrame): exits name the same slot at the same register over
	// and over.
	t.Logf("%d slots listed, %d described", listed, distinct)
	if listed == 0 || 4*distinct > 3*listed {
		t.Errorf("%d slots described for %d listed by the exits", distinct, listed)
	}
}

// A workspace changes where a compile's memory comes from, never what it
// makes: random programs built in one workspace, rewound between them as a
// runtime rewinds it, compile to the same code as programs built on their
// own; so a workspace whose rewinding leaves something behind, or hands out
// memory still in use, shows here.
func TestWorkspaceCompilesTheSame(t *testing.T) {
	r := rand.New(rand.NewPCG(31, 32))
	var ws ssa.Workspace
	var mws mir.Workspace
	n := 0
	defer func(table bool) { mir.TableExits = table }(mir.TableExits)
	for attempt := 0; attempt < 20000 && n < 1500; attempt++ {
		p, l := ssaTestProgram(r)
		if p.Validate() != nil {
			continue
		}
		mir.TableExits = n%2 == 1
		alone, err := ssa.BuildWith(p, l)
		if err != nil {
			continue
		}
		alone.FrameLocals, alone.ThisSlot = l.frame, l.this
		ssa.Optimize(alone)
		want, err := mir.Compile(alone, testEncoding)
		if err != nil {
			continue
		}
		shared, err := ssa.BuildIn(&ws, p, l)
		if err != nil {
			t.Fatalf("built alone, not in a workspace: %v", err)
		}
		shared.FrameLocals, shared.ThisSlot = l.frame, l.this
		ssa.Optimize(shared)
		got, err := mir.CompileIn(&mws, shared, testEncoding)
		if err != nil {
			t.Fatalf("compiled alone, not in a workspace: %v", err)
		}
		// An exit's code holds its description's address, which differs
		// between the two; the descriptions themselves must not.
		if len(got.Bytes) != len(want.Bytes) || !reflect.DeepEqual(got.Exits, want.Exits) ||
			len(got.Exits) == 0 && !bytes.Equal(got.Bytes, want.Bytes) || !maps.Equal(got.Entries, want.Entries) {
			t.Fatalf("program %d compiles differently in a workspace:\n%s", attempt, alone)
		}
		ws.Rewind()
		mws.Rewind()
		n++
	}
	if n < 1000 {
		t.Fatalf("only %d programs compiled", n)
	}
}

// TestSSANativeHighPressure is TestSSANativeMatchesEvaluator's comparison
// for programs with many locals and long bodies, which keep more values
// live than there are registers: what spills, and the spill slots reused
// once their values die, which small programs never reach. A slot reused
// while its value was still live once miscompiled NavierStokes-sized
// functions.
func TestSSANativeHighPressure(t *testing.T) {
	saved := testProgramSize
	testProgramSize.locals, testProgramSize.code = 64, 120
	defer func() { testProgramSize = saved }()
	r := rand.New(rand.NewPCG(41, 42))
	compiled := 0
	for attempt := 0; attempt < 20000 && compiled < 120; attempt++ {
		p, l := ssaTestProgram(r)
		if p.Validate() != nil {
			continue
		}
		if checkNative(t, r, p, l) {
			compiled++
		}
	}
	if compiled < 60 {
		t.Fatalf("only %d programs compiled", compiled)
	}
}

// TestSSANativeCapturedShadow reads an array through a shadow that names a
// captured binding: x=c; loop { x.length; x=y } merges the captured c, x as
// entered at the loop, and y, so ArrayOf finds x's array at run time, in a
// binding's cell on the first iteration. Random programs seldom do.
func TestSSANativeCapturedShadow(t *testing.T) {
	// Slots: x, y, the length, then the captured c.
	p := &ir.Program{Locals: 4, Code: []ir.Instruction{
		{Op: ir.Copy, Dest: 0, Left: ir.Slot(3)},
		{Op: ir.ArrayLength, Dest: 2, Left: ir.Slot(0)},
		{Op: ir.Copy, Dest: 0, Left: ir.Slot(1)},
		{Op: ir.Jump, Target: 1},
	}}
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	c, err := compileNative(p, layout{3, -1, nil, nil, nil, nil, nil})
	if err != nil || c == nil {
		t.Fatalf("compile: %v", err)
	}
	defer c.code.Close()
	if !strings.Contains(c.f.String(), "consts") {
		t.Fatalf("x has no shadow:\n%s", c.f)
	}
	r := rand.New(rand.NewPCG(5, 6))
	for trial := 0; trial < 20; trial++ {
		heap := randomTestHeap(r)
		for _, slots := range [][]ir.Value{
			{ir.Float(0), {Kind: ir.Opaque, Bits: 1}, ir.Float(0), {Kind: ir.Opaque, Bits: 0}},
			{{Kind: ir.Opaque, Bits: 3}, {Kind: ir.Opaque, Bits: 0}, ir.Float(0), {Kind: ir.Opaque, Bits: 1}},
			{ir.Float(0), {Kind: ir.Opaque, Bits: 2}, ir.Float(0), {Kind: ir.Opaque, Bits: 0}},
		} {
			for _, e := range c.f.Entries {
				for _, poll := range []int{1, 2, 3} {
					if why := nativeMismatch(c, e.PC, slots, poll, heap); why != "" {
						t.Fatal(why)
					}
				}
			}
		}
	}
}

// TestSSANativeStrings drives charCodeAt through every form a string has,
// indexes in range and out, with the intrinsic in the context and without,
// and with another callee: random programs seldom line a string, its method
// and an index up. Each run must match the evaluator, and the code units
// must be read natively for ASCII and cached strings alike.
func TestSSANativeStrings(t *testing.T) {
	// Slots: the string, the index, the method, the code. The method read,
	// then the call; and the call alone, of whatever method slot 2 holds:
	// code is entered at its start, not after the read.
	var codes [2]*compiled
	for i, from := range []int{0, 1} {
		p := &ir.Program{Locals: 4, Code: []ir.Instruction{
			{Op: ir.StringMethod, Left: ir.Slot(0), Dest: 2},
			{Op: ir.StringCode, Left: ir.Slot(2), Right: ir.Slot(0), Third: ir.Slot(1), Dest: 3},
			{Op: ir.Return, Left: ir.Slot(3)},
		}[from:]}
		p.Maps = make([]ir.StateMap, len(p.Code))
		for pc := range p.Maps {
			p.Maps[pc].PC = uint32(pc)
		}
		c, err := compileNative(p, layout{4, -1, nil, nil, nil, nil, nil})
		if err != nil || c == nil {
			t.Fatalf("compile: %v", err)
		}
		defer c.code.Close()
		codes[i] = c
	}
	r := rand.New(rand.NewPCG(7, 8))
	read := map[int]int{}
	for form := textASCII; form <= textRope; form++ {
		for _, intrinsic := range []bool{true, false} {
			heap := randomTestHeap(r)
			heap[4].texts = []testText{{units: []uint16{'h', 'i', '!'}, form: form}, {units: []uint16{0x3b1, 0x3b2}, form: form}}
			if form == textASCII {
				heap[4].texts[1].form = textCached
			}
			heap[4].intrinsic = intrinsic
			for _, s := range []uint64{5, 6} {
				for _, i := range []float64{0, 1, 2, 3, -1, 0.5, math.Copysign(0, -1)} {
					for _, callee := range []ir.Value{{Kind: ir.Opaque, Bits: testCharCodeAt}, {Kind: ir.Opaque, Bits: 0}, ir.Float(1)} {
						for from, c := range codes {
							slots := []ir.Value{{Kind: ir.String, Bits: s}, ir.Float(i), callee, ir.Float(0)}
							if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
								t.Fatalf("form %d, intrinsic %v, string %d, index %v, callee %v, from %d: %s", form, intrinsic, s, i, callee, from, why)
							}
							want := slices.Clone(slots)
							if exit, _ := ssa.EvaluateHeap(c.f, 0, want, heap.native().heap(), 0); exit.Kind == ir.Returned {
								read[heap[4].texts[s-5].form]++
							}
						}
					}
				}
			}
		}
	}
	if read[textASCII] == 0 || read[textCached] == 0 || read[textUncached]+read[textRope] != 0 {
		t.Fatalf("code units read by form: %v", read)
	}
}

// TestSSANativeRemainder pins %'s fast path: integers divide natively, with
// a zero remainder carrying the dividend's sign and -2**63 % -1 not
// faulting; everything else exits to Go, where math.Mod answers.
// TestSSANativeEquality compares pairs of values with == and === natively
// (ssa.OpEqTagged): numbers, +0 and -0 included, and NaN; one object and
// two; null, undefined and booleans; one string; values of different
// kinds, which loosely go to Go -- but null and undefined, which equal
// each other and an object with [[IsHTMLDDA]] alone, unconverted. Each
// outcome -- the answer, or an exit -- is the evaluator's and is pinned.
// Random programs seldom meet -0 this way.
func TestSSANativeEquality(t *testing.T) {
	negZero := ir.Value{Kind: ir.Number, Bits: math.Float64bits(math.Copysign(0, -1))}
	nan := ir.Float(math.NaN())
	obj0, obj1 := ir.Value{Kind: ir.Opaque, Bits: 0}, ir.Value{Kind: ir.Opaque, Bits: 1}
	str := ir.Value{Kind: ir.String, Bits: 5}
	undef, null := ir.Value{Kind: ir.Undefined}, ir.Value{Kind: ir.Null}
	for _, strict := range []bool{true, false} {
		p := &ir.Program{Locals: 3, Code: []ir.Instruction{
			{Op: ir.Binary, Operator: ir.Eq, Strict: strict, Dest: 2, Left: ir.Slot(0), Right: ir.Slot(1)},
			{Op: ir.Return, Left: ir.Slot(2)},
		}}
		p.Maps = make([]ir.StateMap, len(p.Code))
		for pc := range p.Maps {
			p.Maps[pc].PC = uint32(pc)
		}
		c, err := compileNative(p, layout{3, -1, nil, nil, nil, nil, nil})
		if err != nil || c == nil {
			t.Fatalf("compile: %v", err)
		}
		for _, tc := range []struct {
			x, y   ir.Value
			equal  bool
			native bool // strictly; loosely, different words go to Go, but two booleans'
		}{
			{ir.Float(0), negZero, true, true}, {ir.Float(1), ir.Float(1), true, true}, {ir.Float(1), ir.Float(2), false, true},
			{nan, nan, false, true}, {nan, ir.Float(1), false, true},
			{obj0, obj0, true, true}, {obj0, obj1, false, true},
			{null, null, true, true}, {undef, undef, true, true}, {ir.Bool(true), ir.Bool(true), true, true},
			{ir.Bool(true), ir.Bool(false), false, true}, {null, undef, false, true}, {ir.Float(1), obj0, false, true},
			{str, str, true, true}, {ir.Float(1), str, false, true},
			{null, obj0, false, true}, {ir.Float(1), undef, false, true}, {undef, str, false, true}, {ir.Bool(false), null, false, true},
		} {
			heap := randomTestHeap(rand.New(rand.NewPCG(1, 2)))
			slots := []ir.Value{tc.x, tc.y, ir.Float(0)}
			if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
				t.Fatalf("strict %v, %v == %v: %s", strict, tc.x, tc.y, why)
			}
			exit, _ := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
			sameWord := tc.x.Kind == tc.y.Kind && (tc.x.Kind == ir.Opaque || tc.x.Kind == ir.String || tc.x.Bits == tc.y.Bits)
			bothNumbers := tc.x.Kind == ir.Number && tc.y.Kind == ir.Number
			nullish := func(v ir.Value) bool { return v.Kind == ir.Null || v.Kind == ir.Undefined }
			// Loosely: null or undefined against anything, and two
			// booleans, unequal as strictly.
			booleans := tc.x.Kind == ir.Boolean && tc.y.Kind == ir.Boolean
			loosely := !strict && (nullish(tc.x) || nullish(tc.y) || booleans)
			native := tc.native && (strict || sameWord || bothNumbers || loosely)
			if got := exit.Kind == ir.Returned; got != native {
				t.Fatalf("strict %v, %v == %v: native %v, want %v", strict, tc.x, tc.y, got, native)
			}
			equal := tc.equal
			if loosely && !sameWord {
				// The other null or undefined, or an object with
				// [[IsHTMLDDA]], as the heap's may have.
				equal = nullish(tc.x) && nullish(tc.y)
				for _, v := range []ir.Value{tc.x, tc.y} {
					equal = equal || v.Kind == ir.Opaque && heap[v.Bits].htmldda
				}
			}
			if native && exit.Value != ir.Bool(equal) {
				t.Fatalf("strict %v, %v == %v = %v, want %v", strict, tc.x, tc.y, exit.Value, equal)
			}
		}
		// Two strings, handles 5 and 6, of one length: the same content
		// in two strings, flat in any form, is equal, compared by bytes a
		// word at a time and then one at a time; a byte that differs in
		// either part, or byte lengths that differ, make them unequal; a
		// rope, and strings longer than abi.MaxEqualUnits, go to Go.
		units := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
		wide := func(n int, last uint16) []uint16 {
			u := make([]uint16, n)
			for i := range u {
				u[i] = uint16(0x100 + i)
			}
			u[n-1] = last
			return u
		}
		long := units(strings.Repeat("x", abi.MaxEqualUnits+1))
		for _, tc := range []struct {
			a, b          testText
			equal, native bool
		}{
			{testText{units("abcdefghij"), textASCII}, testText{units("abcdefghij"), textASCII}, true, true},
			{testText{units("abcdefghij"), textASCII}, testText{units("abcdefghiX"), textASCII}, false, true},
			{testText{units("abcXefghij"), textASCII}, testText{units("abcdefghij"), textASCII}, false, true},
			{testText{wide(10, 0x200), textCached}, testText{wide(10, 0x200), textUncached}, true, true},
			{testText{wide(10, 0x200), textCached}, testText{wide(10, 0x201), textUncached}, false, true},
			{testText{units("ab"), textASCII}, testText{wide(2, 0x101), textCached}, false, true},
			{testText{nil, textASCII}, testText{nil, textCached}, true, true},
			{testText{units("abcdefghij"), textASCII}, testText{units("abcdefghij"), textRope}, false, false},
			{testText{long, textASCII}, testText{slices.Clone(long), textASCII}, false, false},
		} {
			heap := randomTestHeap(rand.New(rand.NewPCG(1, 2)))
			heap[4].texts = []testText{tc.a, tc.b}
			slots := []ir.Value{{Kind: ir.String, Bits: 5}, {Kind: ir.String, Bits: 6}, ir.Float(0)}
			if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
				t.Fatalf("strict %v, %v == %v: %s", strict, tc.a.units, tc.b.units, why)
			}
			exit, _ := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
			if got := exit.Kind == ir.Returned; got != tc.native {
				t.Fatalf("strict %v, %v == %v: native %v, want %v", strict, tc.a.units, tc.b.units, got, tc.native)
			}
			if tc.native && exit.Value != ir.Bool(tc.equal) {
				t.Fatalf("strict %v, %v == %v = %v, want %v", strict, tc.a.units, tc.b.units, exit.Value, tc.equal)
			}
		}
		c.code.Close()
	}
}

// TestSSANativeElementCells reads elements by their cells (ir.Instruction's
// Reference): a number, true, a hole, past the end, at indexes that are not
// integers, and from an object that is not an array. A present element is
// read natively, whatever it holds; the rest exit to Go. Random programs
// seldom meet a hole this way.
func TestSSANativeElementCells(t *testing.T) {
	p := &ir.Program{Locals: 3, Code: []ir.Instruction{
		{Op: ir.ArrayRead, Reference: true, Dest: 2, Left: ir.Slot(0), Right: ir.Slot(1)},
		{Op: ir.Return, Left: ir.Slot(2)},
	}}
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	c, err := compileNative(p, layout{3, -1, nil, nil, nil, nil, nil})
	if err != nil || c == nil {
		t.Fatalf("compile: %v", err)
	}
	defer c.code.Close()
	heap := testHeap{
		{cells: []uint64{math.Float64bits(1), testEncoding.Uninitialized, testEncoding.True, math.Float64bits(4.5)}, array: true, shape: -1},
		{cells: []uint64{math.Float64bits(2)}, shape: -1},
	}
	for _, tc := range []struct {
		array  uint64
		index  float64
		native bool
	}{
		{0, 0, true}, {0, 2, true}, {0, 3, true},
		{0, 1, false}, {0, 4, false}, {0, -1, false}, {0, 1.5, false}, {0, math.NaN(), false}, {0, 1 << 33, false},
		{1, 0, false},
	} {
		slots := []ir.Value{{Kind: ir.Opaque, Bits: tc.array}, ir.Float(tc.index), ir.Float(0)}
		if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
			t.Fatalf("array %d [%v]: %s", tc.array, tc.index, why)
		}
		exit, _ := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
		if native := exit.Kind == ir.Returned; native != tc.native {
			t.Fatalf("array %d [%v]: native %v, want %v", tc.array, tc.index, native, tc.native)
		}
	}
}

func TestSSANativeRemainder(t *testing.T) {
	p := &ir.Program{Locals: 3, Code: []ir.Instruction{
		{Op: ir.Binary, Operator: ir.Mod, Dest: 2, Left: ir.Slot(0), Right: ir.Slot(1)},
		{Op: ir.Return, Left: ir.Slot(2)},
	}}
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	c, err := compileNative(p, layout{3, -1, nil, nil, nil, nil, nil})
	if err != nil || c == nil {
		t.Fatalf("compile: %v", err)
	}
	defer c.code.Close()
	negZero := math.Copysign(0, -1)
	for _, tc := range []struct {
		x, y   float64
		native bool
	}{
		{7, 3, true}, {-7, 3, true}, {7, -3, true}, {-7, -3, true},
		{-4, 2, true}, {4, -2, true}, {negZero, 5, true}, {0, -5, true},
		{-1 << 63, -1, true}, {-1 << 63, 7, true}, {1 << 62, 3, true}, {9007199254740992, 10, true},
		{5, 0, false}, {5.5, 2, false}, {5, 2.5, false}, {math.NaN(), 1, false}, {1, math.NaN(), false},
		{math.Inf(1), 1, false}, {1, math.Inf(-1), false}, {1 << 63, 3, false},
	} {
		heap := randomTestHeap(rand.New(rand.NewPCG(1, 2)))
		slots := []ir.Value{ir.Float(tc.x), ir.Float(tc.y), ir.Float(0)}
		if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
			t.Fatalf("%v %% %v: %s", tc.x, tc.y, why)
		}
		exit, _ := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
		if native := exit.Kind == ir.Returned; native != tc.native {
			t.Fatalf("%v %% %v: native %v, want %v", tc.x, tc.y, native, tc.native)
		}
		if tc.native {
			want := math.Mod(tc.x, tc.y)
			if math.Float64bits(math.Float64frombits(exit.Value.Bits)) != math.Float64bits(want) {
				t.Fatalf("%v %% %v = %v, want %v", tc.x, tc.y, math.Float64frombits(exit.Value.Bits), want)
			}
		}
	}
	// The division takes RDX, the operands' base: a remainder on the
	// operand stack, stored there by a host exit's stub, must land in it.
	q := &ir.Program{Locals: 2, StackSize: 1, Code: []ir.Instruction{
		{Op: ir.Binary, Operator: ir.Mod, Dest: 2, Left: ir.Slot(0), Right: ir.Slot(1)},
		{Op: ir.Host},
		{Op: ir.Return, Left: ir.Slot(2)},
	}, Maps: []ir.StateMap{{PC: 0}, {PC: 1, Depth: 1}, {PC: 2, Depth: 1}}}
	d, err := compileNative(q, layout{2, -1, nil, nil, nil, nil, nil})
	if err != nil || d == nil {
		t.Fatalf("compile: %v", err)
	}
	defer d.code.Close()
	heap := randomTestHeap(rand.New(rand.NewPCG(1, 2)))
	for _, x := range []float64{17, -17, 18} {
		if why := nativeMismatch(d, 0, []ir.Value{ir.Float(x), ir.Float(6), ir.Float(0)}, 0, heap); why != "" {
			t.Fatalf("%v %% 6 on the stack: %s", x, why)
		}
	}
}

var ssaNativeCorpus = []string{
	`function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`,
	`function f(n){let s=0;for(let i=0;i<n;i++)s+=i*0.5-i/3;return s}`,
	`function f(n){var x=0.1;for(var i=0;i<n;i++)x=3.7*x*(1-x);return x}`,
	`function f(n){let h=0;for(let i=0;i<n;i++){h=(h<<5)-h+i|0;h^=h>>>13;h&=0x7fffffff;h|=1;h=~h;h>>=1}return h}`,
	`function f(n){let s=0;for(let i=0;i<n;i++){if(s>1e6)break;s+=i<10?i:-i}return s}`,
	`function f(a,b){let c=0;while(a>0){c+=a&b;a=a>>1;b=b<<1|0}return c}`,
	`function f(n){let a=1,b=1;for(let i=2;i<n;i++){let t=a+b;a=b;b=t}return b}`,
	`function f(n,m){let s=0;for(let i=0;i<n;i++)for(let j=0;j<m;j++)s+=i^j;return s}`,
	`function f(x){return !x?-x:+x}`,
	`function f(n){let s=0;for(let i=n;i--;)s+=i;return s}`,
	`function f(x){let n=0;do{x=x%2?3*x+1:x/2;n++}while(x!==1&&n<1000);return n}`,
}

func TestSSANativeFromJavaScript(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 14))
	compiled := 0
	for _, src := range ssaNativeCorpus {
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := compiler.Compile(prog, compiler.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range top.Constants {
			if c.Kind != bytecode.ConstFunction {
				continue
			}
			p, err := jitcompile.Lower(c.Fn)
			if err != nil {
				continue
			}
			if checkNative(t, r, p, layout{c.Fn.LocalCount, -1, nil, nil, nil, nil, nil}) {
				compiled++
			}
		}
	}
	if compiled < len(ssaNativeCorpus)-2 {
		t.Fatalf("only %d of %d functions compiled", compiled, len(ssaNativeCorpus))
	}
}

// BenchmarkSSARoundTrip is the new pipeline's floor: Run, the bridge, an
// entry's checks, a host exit's record, and the return to Go: 15 ns or
// less is what it was built to.
func BenchmarkSSARoundTrip(b *testing.B) {
	for _, locals := range []int{1, 8} {
		b.Run(fmt.Sprintf("%d-locals", locals), func(b *testing.B) {
			p := &ir.Program{Locals: locals, Code: []ir.Instruction{{Op: ir.Host}, {Op: ir.Return, Left: ir.Slot(0)}},
				Maps: []ir.StateMap{{PC: 0}, {PC: 1}}}
			c, err := compileNative(p, layout{locals, -1, nil, nil, nil, nil, nil})
			if err != nil || c == nil {
				b.Fatal(err)
			}
			defer c.code.Close()
			frame := make([]testValue, locals)
			counter := 1 << 62
			ctx := &abi.Context{Locals: unsafe.Pointer(&frame[0]), BackEdges: &counter}
			ctx.SetConsts(&testEncoding)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := c.code.Run(0, ctx)
				ApplyExit(ctx)
				if err != nil || ctx.ExitKind != abi.ExitHost {
					b.Fatal(err, ctx.ExitKind)
				}
			}
		})
	}
}

// TestSSANativeStores stores values of every kind in a property and reads
// them back, with the collector's write-barrier flag clear and set: a store
// that changes a pointer word -- a reference stored, or one replaced -- is
// made with Go's write barrier while it is set (storeRef), or, where the
// code may not, is Go's. A reference loaded from the cell being stored to
// and used after the store keeps the store from happening natively
// (ssa's storeChecks); one loaded from another object's cell does not.
// Random programs seldom read and then write one property of one object.
func TestSSANativeStores(t *testing.T) {
	cached := ssa.PropertySite{Key: 10, Shape: testShapes[0].shape, Index: 0}
	program := func(code ...ir.Instruction) *ir.Program {
		p := &ir.Program{Locals: 4, Code: append(code, ir.Instruction{Op: ir.Return, Left: ir.Slot(3)})}
		p.Maps = make([]ir.StateMap, len(p.Code))
		for pc := range p.Maps {
			p.Maps[pc].PC = uint32(pc)
		}
		return p
	}
	read := func(object int) ir.Instruction {
		return ir.Instruction{Op: ir.ReferenceRead, Dest: 3, Left: ir.Slot(object), Key: 10}
	}
	write := ir.Instruction{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(1), Key: 10}
	for _, tc := range []struct {
		name string
		p    *ir.Program
		// returns is what is returned: what was read before the store, what
		// was stored, or what object 2 held.
		returns string
	}{
		{"read, store, use", program(read(0), write), "old"},
		{"store, read", program(write, read(0)), "stored"},
		{"read another, store, use", program(read(2), write), "other"},
	} {
		c, err := compileNative(tc.p, layout{4, -1, map[int]site{0: cached, 1: cached}, nil, nil, nil, nil})
		if err != nil || c == nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		for _, value := range []ir.Value{ir.Float(5), {Kind: ir.Opaque, Bits: 1}, ir.Bool(true), {Kind: ir.Undefined}} {
			for _, old := range []ir.Value{ir.Float(3), {Kind: ir.Opaque, Bits: 3}} {
				for _, barrier := range []bool{false, true} {
					heap := randomTestHeap(rand.New(rand.NewPCG(3, 4)))
					for _, i := range []int{0, 2} {
						sh := testShapes[0]
						heap[i].shape, heap[i].proto = 0, -1
						heap[i].keys, heap[i].flags = sh.keys, sh.flags
						heap[i].props = []ir.Value{old, ir.Float(1), ir.Float(2)}
					}
					heap[2].props[0] = ir.Value{Kind: ir.Opaque, Bits: 2}
					heap[4].barrier = barrier
					slots := []ir.Value{{Kind: ir.Opaque, Bits: 0}, value, {Kind: ir.Opaque, Bits: 2}, ir.Float(0)}
					name := fmt.Sprintf("%s: %v over %v, barrier %v", tc.name, value, old, barrier)
					if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
						t.Fatalf("%s: %s", name, why)
					}
					exit, err := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					// While the collector marks, a store of a pointer word, or
					// over one, leaves unless the code stores it through
					// storeRef; otherwise the value read before it was kept
					// (OpKeep).
					pointers := isTestReference(value) || isTestReference(old)
					native := !(barrier && pointers) || testEncoding.StoreRef != 0
					if got := exit.Kind == ir.Returned; got != native {
						t.Fatalf("%s: native %v, want %v", name, got, native)
					}
					want := map[string]ir.Value{"old": old, "stored": value, "other": {Kind: ir.Opaque, Bits: 2}}[tc.returns]
					if native && exit.Value != want {
						t.Fatalf("%s: returned %v, want %v", name, exit.Value, want)
					}
				}
			}
		}
		c.code.Close()
	}
}

// A store that more references read from cells are live across than keep
// cells are left for (abi.MaxKeeps) keeps none, and leaves itself to Go
// where it would write a cell one was read from (ssa's storeChecks): here a
// guard after them names 50 reads of the cell six stores write, the reads
// merged after each store into values of their own (300 cells' worth).
func TestSSANativeStoresPastTheKeeps(t *testing.T) {
	cached := ssa.PropertySite{Key: 10, Shape: testShapes[0].shape, Index: 0}
	const reads, stores = 50, 6
	p := &ir.Program{Locals: reads + 4}
	sites := map[int]site{}
	for i := range reads {
		sites[len(p.Code)] = cached
		p.Code = append(p.Code, ir.Instruction{Op: ir.ReferenceRead, Dest: 3 + i, Left: ir.Slot(0), Key: 10})
	}
	for range stores {
		sites[len(p.Code)] = cached
		p.Code = append(p.Code, ir.Instruction{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(1), Key: 10})
	}
	// A read of another object's, whose guard's frame state holds every
	// read before the store.
	sites[len(p.Code)] = cached
	p.Code = append(p.Code, ir.Instruction{Op: ir.ReferenceRead, Dest: reads + 3, Left: ir.Slot(2), Key: 10},
		ir.Instruction{Op: ir.Return, Left: ir.Slot(3)})
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	c, err := compileNative(p, layout{p.Locals, -1, sites, nil, nil, nil, nil})
	if err != nil || c == nil {
		t.Fatalf("compile: %v", err)
	}
	defer c.code.Close()
	if c.f.Keeps > abi.MaxKeeps {
		t.Fatalf("%d keeps, past abi.MaxKeeps's %d", c.f.Keeps, abi.MaxKeeps)
	}
	checks := 0
	for _, b := range c.f.Blocks {
		for _, v := range b.Values {
			if v.Op == ssa.OpPropWrite {
				checks += len(v.Args) - 2
			}
		}
	}
	if checks == 0 {
		t.Fatalf("the store checks no cell:\n%s", c.f)
	}
	for _, value := range []ir.Value{ir.Float(5), {Kind: ir.Opaque, Bits: 1}} {
		for _, old := range []ir.Value{ir.Float(3), {Kind: ir.Opaque, Bits: 3}} {
			heap := randomTestHeap(rand.New(rand.NewPCG(5, 6)))
			for _, i := range []int{0, 2} {
				sh := testShapes[0]
				heap[i].shape, heap[i].proto = 0, -1
				heap[i].keys, heap[i].flags = sh.keys, sh.flags
				heap[i].props = []ir.Value{old, ir.Float(1), ir.Float(2)}
			}
			// Object 2 is of another shape: its read leaves, with the
			// frame state the reads are in.
			heap[2].shape = 1
			slots := make([]ir.Value, p.Locals)
			slots[0], slots[1], slots[2] = ir.Value{Kind: ir.Opaque, Bits: 0}, value, ir.Value{Kind: ir.Opaque, Bits: 2}
			for i := 3; i < len(slots); i++ {
				slots[i] = ir.Float(0)
			}
			if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
				t.Fatalf("%v over %v: %s", value, old, why)
			}
		}
	}
}

// A function keeps up to abi.MaxKeeps values read from cells across the
// stores that may write them: here 80 reads across one store of the cell
// are all kept, past the 64 there were. Crypto's bnModPow, whose calls are
// stores of any cell, left every call to Go for want of them.
func TestSSAKeepsManyValues(t *testing.T) {
	cached := ssa.PropertySite{Key: 10, Shape: testShapes[0].shape, Index: 0}
	for _, tc := range []struct{ reads, stores int }{{80, 1}} {
		p := &ir.Program{Locals: tc.reads + 4}
		sites := map[int]site{}
		for i := range tc.reads {
			sites[len(p.Code)] = cached
			p.Code = append(p.Code, ir.Instruction{Op: ir.ReferenceRead, Dest: 3 + i, Left: ir.Slot(0), Key: 10})
		}
		for range tc.stores {
			sites[len(p.Code)] = cached
			p.Code = append(p.Code, ir.Instruction{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(1), Key: 10})
		}
		// A read of another object's, whose guard's frame state holds every
		// read before the stores.
		sites[len(p.Code)] = cached
		p.Code = append(p.Code, ir.Instruction{Op: ir.ReferenceRead, Dest: tc.reads + 3, Left: ir.Slot(2), Key: 10},
			ir.Instruction{Op: ir.Return, Left: ir.Slot(3)})
		p.Maps = make([]ir.StateMap, len(p.Code))
		for pc := range p.Maps {
			p.Maps[pc].PC = uint32(pc)
		}
		c, err := compileNative(p, layout{p.Locals, -1, sites, nil, nil, nil, nil})
		if err != nil || c == nil {
			t.Fatalf("%d reads: compile: %v", tc.reads, err)
		}
		if c.f.Keeps != tc.reads {
			t.Fatalf("%d reads across %d stores: %d keep cells", tc.reads, tc.stores, c.f.Keeps)
		}
		for _, value := range []ir.Value{ir.Float(5), {Kind: ir.Opaque, Bits: 1}} {
			heap := randomTestHeap(rand.New(rand.NewPCG(7, 8)))
			for _, i := range []int{0, 2} {
				sh := testShapes[0]
				heap[i].shape, heap[i].proto = 0, -1
				heap[i].keys, heap[i].flags = sh.keys, sh.flags
				heap[i].props = []ir.Value{{Kind: ir.Opaque, Bits: 3}, ir.Float(1), ir.Float(2)}
			}
			heap[2].shape = 1
			slots := make([]ir.Value, p.Locals)
			slots[0], slots[1], slots[2] = ir.Value{Kind: ir.Opaque, Bits: 0}, value, ir.Value{Kind: ir.Opaque, Bits: 2}
			for i := 3; i < len(slots); i++ {
				slots[i] = ir.Float(0)
			}
			if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
				t.Fatalf("%d reads, %v stored: %s", tc.reads, value, why)
			}
		}
		c.code.Close()
	}
}

// isTestReference reports a value with a pointer word.
func isTestReference(v ir.Value) bool { return v.Kind == ir.Opaque || v.Kind == ir.String }

// A read whose receiver is of a shape its site was not compiled for looks
// further only where its cache is megamorphic (ssa.PropertySite's
// Megamorphic): in the receiver's own table, then in the megamorphic
// cache -- a prototype's property, found along the entry's chain, each
// object and shape checked; one nothing has, undefined; a chain the
// receiver's prototypes are not, or a stale shape in it, Go's, a host
// exit. Anywhere else it deoptimizes (ir.GuardExit), as V8's map checks
// do, even where its own table has the property: the interpreter makes
// the read, and the site's cache learns the shape -- but where the cache is
// empty, which may only say the VM's interpreter, which fills none, ran
// it (ssa.PropertySite's Scan): its own table is searched, and it
// deoptimizes only where that has not the property. Each as the SSA
// evaluator says.
func TestSSANativeMegamorphic(t *testing.T) {
	s0, s1 := testShapes[0], testShapes[1]
	link := func(i int, shape unsafe.Pointer) abi.MegaLink {
		if shape == nil {
			shape = unsafe.Pointer(testHolders[i].shape)
		}
		return abi.MegaLink{Object: unsafe.Pointer(&testHolders[i]), Shape: shape}
	}
	m := new(abi.MegaCache)
	for _, e := range []abi.MegaEntry{
		{Shape: unsafe.Pointer(s1.s), Key: 12, Index: 0, Chain: &abi.MegaChain{link(0, nil)}},
		{Shape: unsafe.Pointer(s1.s), Key: 13, Index: 1, Chain: &abi.MegaChain{link(0, unsafe.Pointer(&testStaleShape))}},
		{Shape: unsafe.Pointer(s1.s), Key: 14, Index: abi.MegaAbsent, Chain: &abi.MegaChain{link(0, nil), link(1, nil)}},
	} {
		set := m[abi.MegaSet(uintptr(e.Shape), e.Key):][:abi.MegaWays]
		copy(set[1:], set)
		set[0] = e
	}
	testMegaOverride = m
	defer func() { testMegaOverride = nil }()
	for _, tc := range []struct {
		key   uint32
		proto int
		mega  bool
		want  ir.Value // its value, or Undefined's Kind 0 for an exit
		exits bool
		// scan marks a site whose cache is empty (ssa.PropertySite's Scan):
		// compiled for no shape, it searches the receiver's own table.
		scan bool
	}{
		{12, 0, true, ir.Float(7), false, false},
		{12, 0, false, ir.Value{}, true, false},
		{14, 0, true, ir.Value{Kind: ir.Undefined}, false, false},
		{14, 1, true, ir.Value{}, true, false},
		{13, 0, true, ir.Value{}, true, false},
		{12, -1, true, ir.Value{}, true, false},
		{11, 0, true, ir.Float(4), false, false},
		{11, 0, false, ir.Value{}, true, false},
		{11, 0, false, ir.Float(4), false, true},
		{12, 0, false, ir.Value{}, true, true},
	} {
		p := &ir.Program{Locals: 4, Code: []ir.Instruction{{Op: ir.ReferenceRead, Dest: 3, Left: ir.Slot(0), Key: tc.key},
			{Op: ir.Return, Left: ir.Slot(3)}}}
		p.Maps = make([]ir.StateMap, len(p.Code))
		for pc := range p.Maps {
			p.Maps[pc].PC = uint32(pc)
		}
		ps := ssa.PropertySite{Key: tc.key, Shape: s0.shape, Index: 0, Megamorphic: tc.mega}
		if tc.scan {
			ps = ssa.PropertySite{Key: tc.key, Scan: true}
		}
		c, err := compileNative(p, layout{4, -1, map[int]site{0: ps}, nil, nil, nil, nil})
		if err != nil || c == nil {
			t.Fatalf("%+v: compile: %v", tc, err)
		}
		heap := randomTestHeap(rand.New(rand.NewPCG(3, 4)))
		heap[2].shape, heap[2].proto = 1, tc.proto
		heap[2].keys, heap[2].flags = s1.keys, s1.flags
		heap[2].props = []ir.Value{ir.Float(4)}
		slots := []ir.Value{{Kind: ir.Opaque, Bits: 2}, ir.Float(1), ir.Float(2), ir.Float(0)}
		if why := nativeMismatch(c, 0, slots, 0, heap); why != "" {
			t.Fatalf("%+v: %s", tc, why)
		}
		exit, err := ssa.EvaluateHeap(c.f, 0, slices.Clone(slots), heap.native().heap(), 0)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got := exit.Kind != ir.Returned; got != tc.exits || !tc.exits && exit.Value != tc.want {
			t.Fatalf("%+v: %+v", tc, exit)
		}
		if want := map[bool]ir.ExitKind{true: ir.HostExit, false: ir.GuardExit}[tc.mega]; tc.exits && exit.Kind != want {
			t.Fatalf("%+v: exit %v, want %v", tc, exit.Kind, want)
		}
		c.code.Close()
	}
}
