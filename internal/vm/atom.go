package vm

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Atom is an interned property key.
//
// Interning turns every property access into an integer comparison instead of a
// string comparison, and lets a property map be keyed by a machine word.
//
// Array indices are encoded directly rather than interned. A script that writes
// a[i] for a million values of i would otherwise grow the intern table without
// bound, so any key that is a canonical array index is represented as the index
// itself with the high bit set. This costs one bit of index range, which is
// exactly the range the specification allows for array indices anyway
// (0 .. 2^32-2).
type Atom uint32

const (
	// atomIndexTag marks an Atom that carries an array index in its low bits.
	atomIndexTag = 0x80000000
	// maxArrayIndex is the largest legal array index. 2^32-1 is reserved as the
	// length, so it is never an index.
	maxArrayIndex = 0xFFFFFFFE
)

// Well-known atoms, interned first so that their values are compile-time
// predictable and the hottest names never need a lookup.
const (
	atomEmpty Atom = iota
	atomLength
	atomName
	atomPrototype
	atomConstructor
	atomValue
	atomWritable
	atomEnumerable
	atomConfigurable
	atomGet
	atomSet
	atomToString
	atomValueOf
	atomUndefined
	atomNull
	atomTrue
	atomFalse
	atomObject
	atomFunction
	atomNumber
	atomString
	atomBoolean
	atomSymbol
	atomBigint
	atomArguments
	atomCaller
	atomCallee
	atomMessage
	atomStack
	atomProto // "__proto__"
	atomNext
	atomDone
	atomReturn
	atomThrow
	atomRaw
	atomIndex
	atomInput
	atomGroups
	atomLastIndex
	atomSource
	atomFlags
	atomGlobal
	atomToJSON
	atomThen
	atomDefault
	atomExec
	atomStaticAtomCount
)

// staticAtomNames must stay in the same order as the constants above.
var staticAtomNames = [...]string{
	"", "length", "name", "prototype", "constructor", "value", "writable",
	"enumerable", "configurable", "get", "set", "toString", "valueOf",
	"undefined", "null", "true", "false", "object", "function", "number",
	"string", "boolean", "symbol", "bigint", "arguments", "caller", "callee",
	"message", "stack", "__proto__", "next", "done", "return", "throw", "raw",
	"index", "input", "groups", "lastIndex", "source", "flags", "global",
	"toJSON", "then", "default", "exec",
}

// atomEntry is one interned key: either a string or a symbol.
type atomEntry struct {
	name string
	sym  *Symbol
}

// atomTable interns property keys for one runtime.
//
// Every runtime interns the same names as it builds its realm -- some
// thousand of them -- so the first to finish publishes them as a base the
// later ones share: theirs are the atoms from the base's end up.
type atomTable struct {
	// base is the shared names, read and never written, or nil.
	base *atomBase
	// entries are the table's own atoms, from the base's end up.
	entries []atomEntry
	byName  map[string]Atom
	// bySymbol maps a symbol to its atom. Symbols are compared by identity, so
	// this is keyed by pointer. A symbol is a runtime's own, so no base holds
	// one.
	bySymbol map[*Symbol]Atom
}

// atomBase is the names a built realm interns, with the static atoms first
// at their own numbers, shared by the runtimes made after it.
type atomBase struct {
	entries []atomEntry
	byName  map[string]Atom
}

// sharedAtoms is the published base, nil until a realm has been built.
var sharedAtoms atomic.Pointer[atomBase]

func newAtomTable() *atomTable {
	if b := sharedAtoms.Load(); b != nil {
		return &atomTable{
			base:     b,
			byName:   make(map[string]Atom, 64),
			bySymbol: make(map[*Symbol]Atom),
		}
	}
	t := &atomTable{
		entries:  make([]atomEntry, 0, len(staticAtomNames)+64),
		byName:   make(map[string]Atom, len(staticAtomNames)+64),
		bySymbol: make(map[*Symbol]Atom),
	}
	for _, name := range staticAtomNames {
		t.entries = append(t.entries, atomEntry{name: name})
		t.byName[name] = Atom(len(t.entries) - 1)
	}
	return t
}

// publish makes the names a table without a base has interned the base of
// tables made after it, if none has been published. The names keep their
// order, the static atoms first; the symbols, a runtime's own, are left out.
func (t *atomTable) publish() {
	if t.base != nil || sharedAtoms.Load() != nil {
		return
	}
	b := &atomBase{byName: make(map[string]Atom, len(t.entries))}
	for _, e := range t.entries {
		if e.sym == nil {
			b.byName[e.name] = Atom(len(b.entries))
			b.entries = append(b.entries, e)
		}
	}
	sharedAtoms.CompareAndSwap(nil, b)
}

// entry is the atom's entry, in the base or the table's own.
func (t *atomTable) entry(a Atom) atomEntry {
	if b := t.base; b != nil {
		if int(a) < len(b.entries) {
			return b.entries[a]
		}
		return t.entries[int(a)-len(b.entries)]
	}
	return t.entries[a]
}

// next is the atom the table's next entry will be.
func (t *atomTable) next() Atom {
	if b := t.base; b != nil {
		return Atom(len(b.entries) + len(t.entries))
	}
	return Atom(len(t.entries))
}

// find is the atom a name already has, in the base or the table's own.
func (t *atomTable) find(name string) (Atom, bool) {
	if b := t.base; b != nil {
		if a, ok := b.byName[name]; ok {
			return a, true
		}
	}
	a, ok := t.byName[name]
	return a, ok
}

// intern returns the atom for a property name.
func (t *atomTable) intern(name string) Atom {
	// A canonical array index is encoded rather than stored, as long as it
	// fits beside the tag.
	if idx, ok := arrayIndexOf(name); ok && idx < atomIndexTag {
		return Atom(atomIndexTag | idx)
	}
	if a, ok := t.find(name); ok {
		return a
	}
	a := t.next()
	t.entries = append(t.entries, atomEntry{name: name})
	t.byName[name] = a
	return a
}

// lookup returns the atom a name already has, without making one: a name
// never interned is the key of no property.
func (t *atomTable) lookup(name string) (Atom, bool) {
	if idx, ok := arrayIndexOf(name); ok && idx < atomIndexTag {
		return Atom(atomIndexTag | idx), true
	}
	return t.find(name)
}

// internCopy interns a name that may be a slice of something much larger -- a
// key cut out of a JSON document, say.
//
// The table holds a name for as long as the runtime lives, so one that arrives
// this way is copied rather than left pinning what it was cut from. The copy is
// made only when the name is new: a document's keys repeat, and the second
// occurrence of one finds the first.
func (t *atomTable) internCopy(name string) Atom {
	if a, ok := t.find(name); ok {
		return a
	}
	return t.intern(strings.Clone(name))
}

// internSymbol returns the atom for a symbol key.
func (t *atomTable) internSymbol(s *Symbol) Atom {
	if a, ok := t.bySymbol[s]; ok {
		return a
	}
	a := t.next()
	t.entries = append(t.entries, atomEntry{sym: s})
	t.bySymbol[s] = a
	return a
}

// indexAtom returns the atom for an array index.
//
// Only an index below 2**31 fits in an atom's low bits beside the tag. A larger
// one -- an array may have indices up to 2**32-2 -- is an ordinary interned
// name, which still addresses an element: arrayIndex reads back either form.
func (t *atomTable) indexAtom(i uint32) Atom {
	if i >= atomIndexTag {
		return t.intern(strconv.FormatUint(uint64(i), 10))
	}
	return Atom(atomIndexTag | i)
}

// arrayIndex returns the array index an atom names, however it is spelled.
func (t *atomTable) arrayIndex(a Atom) (uint32, bool) {
	if a.IsIndex() {
		return a.Index(), true
	}
	if uint(a) >= uint(t.next()) {
		return 0, false
	}
	e := t.entry(a)
	if e.sym != nil {
		return 0, false
	}
	return arrayIndexOf(e.name)
}

// name returns the string form of an atom, which is what property enumeration
// and error messages need.
func (t *atomTable) name(a Atom) string {
	if a.IsIndex() {
		return strconv.FormatUint(uint64(a.Index()), 10)
	}
	e := t.entry(a)
	if e.sym != nil {
		return e.sym.String()
	}
	return e.name
}

// symbol returns the symbol an atom denotes, or nil for a string key.
func (t *atomTable) symbol(a Atom) *Symbol {
	if a.IsIndex() {
		return nil
	}
	return t.entry(a).sym
}

// IsIndex reports whether the atom encodes an array index.
func (a Atom) IsIndex() bool { return a&atomIndexTag != 0 }

// Index returns the array index an atom encodes.
func (a Atom) Index() uint32 { return uint32(a) &^ atomIndexTag }

// IsSymbol reports whether the atom denotes a symbol, which enumeration must
// skip.
func (t *atomTable) IsSymbol(a Atom) bool {
	return !a.IsIndex() && t.entry(a).sym != nil
}

// arrayIndexOf reports whether name is a canonical array index, meaning the
// decimal spelling of an integer in [0, 2^32-2] with no leading zeros.
//
// The canonical form matters: "01" and "1.0" are ordinary string keys even
// though they parse as numbers, because converting them back to a string does
// not reproduce the original.
func arrayIndexOf(name string) (uint32, bool) {
	n := len(name)
	if n == 0 || n > 10 {
		return 0, false
	}
	// "0" is an index; anything else starting with '0' is not canonical.
	if name[0] == '0' {
		return 0, n == 1
	}
	var v uint64
	for i := 0; i < n; i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
		if v > maxArrayIndex {
			return 0, false
		}
	}
	return uint32(v), true
}

// numberToAtom converts a numeric property key, taking the index encoding when
// the number is a canonical index and interning its string form otherwise.
func (r *Runtime) numberToAtom(f float64) Atom {
	// -0 is included deliberately: ToPropertyKey(-0) is "0", so it addresses
	// the same slot as +0, and uint32(-0.0) is 0.
	if i := uint32(f); float64(i) == f && i <= maxArrayIndex {
		return r.atoms.indexAtom(i)
	}
	return r.atoms.intern(jsnum.FormatFloat(f))
}
