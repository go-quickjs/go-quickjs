package vm

import (
	"strconv"

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
type atomTable struct {
	entries []atomEntry
	byName  map[string]Atom
	// bySymbol maps a symbol to its atom. Symbols are compared by identity, so
	// this is keyed by pointer.
	bySymbol map[*Symbol]Atom
}

func newAtomTable() *atomTable {
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

// intern returns the atom for a property name.
func (t *atomTable) intern(name string) Atom {
	// A canonical array index is encoded rather than stored.
	if idx, ok := arrayIndexOf(name); ok {
		return Atom(atomIndexTag | idx)
	}
	if a, ok := t.byName[name]; ok {
		return a
	}
	t.entries = append(t.entries, atomEntry{name: name})
	a := Atom(len(t.entries) - 1)
	t.byName[name] = a
	return a
}

// internSymbol returns the atom for a symbol key.
func (t *atomTable) internSymbol(s *Symbol) Atom {
	if a, ok := t.bySymbol[s]; ok {
		return a
	}
	t.entries = append(t.entries, atomEntry{sym: s})
	a := Atom(len(t.entries) - 1)
	t.bySymbol[s] = a
	return a
}

// internIndex returns the atom for an array index.
func internIndex(i uint32) Atom { return Atom(atomIndexTag | i) }

// name returns the string form of an atom, which is what property enumeration
// and error messages need.
func (t *atomTable) name(a Atom) string {
	if a.IsIndex() {
		return strconv.FormatUint(uint64(a.Index()), 10)
	}
	e := t.entries[a]
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
	return t.entries[a].sym
}

// IsIndex reports whether the atom encodes an array index.
func (a Atom) IsIndex() bool { return a&atomIndexTag != 0 }

// Index returns the array index an atom encodes.
func (a Atom) Index() uint32 { return uint32(a) &^ atomIndexTag }

// IsSymbol reports whether the atom denotes a symbol, which enumeration must
// skip.
func (t *atomTable) IsSymbol(a Atom) bool {
	return !a.IsIndex() && t.entries[a].sym != nil
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
		return internIndex(i)
	}
	return r.atoms.intern(jsnum.FormatFloat(f))
}
