// Package asmjs validates asm.js modules (asm.js 1.0,
// http://asmjs.org/spec/latest/): a function whose body begins with the
// "use asm" directive and whose code passes asm.js's type rules. What it
// proves -- every value's type, int32, uint32, float32 or float64 -- is
// what the register backend compiles a module's functions to typed code
// from (docs/register-interpreter.md, "The typed table"). A module that
// fails runs as the ordinary code it also is: validation only proves
// facts, and refuses nothing.
package asmjs

// Type is a type of asm.js's type system.
type Type uint8

const (
	Void Type = iota
	Double
	DoubleQ // double?, a Float64Array's load, NaN out of bounds
	Float
	FloatQ   // float?, a Float32Array's load
	Floatish // a float operator's result, which fround coerces
	Signed
	Unsigned
	Int
	Fixnum // an int in [0, 2^31): both signed and unsigned
	Intish // an int operator's result, which | 0 and the bitwise operators coerce
	Extern // what an FFI call takes: signed or double
)

var typeNames = [...]string{
	Void: "void", Double: "double", DoubleQ: "double?", Float: "float", FloatQ: "float?", Floatish: "floatish",
	Signed: "signed", Unsigned: "unsigned", Int: "int", Fixnum: "fixnum", Intish: "intish", Extern: "extern",
}

func (t Type) String() string { return typeNames[t] }

// Sub reports whether t is a subtype of u.
func (t Type) Sub(u Type) bool {
	if t == u {
		return true
	}
	switch t {
	case Fixnum:
		return u == Signed || u == Unsigned || u == Int || u == Intish || u == Extern
	case Signed:
		return u == Int || u == Intish || u == Extern
	case Unsigned:
		return u == Int || u == Intish
	case Int:
		return u == Intish
	case Double:
		return u == DoubleQ || u == Extern
	case Float:
		return u == FloatQ || u == Floatish
	case FloatQ:
		return u == Floatish
	}
	return false
}

// IsInt reports whether t is an int: signed, unsigned or either.
func (t Type) IsInt() bool { return t.Sub(Int) }

// View is a heap view's element type.
type View uint8

const (
	Int8 View = iota
	Uint8
	Int16
	Uint16
	Int32
	Uint32
	Float32
	Float64
)

// viewCtors are the views' constructors by their stdlib names.
var viewCtors = map[string]View{
	"Int8Array": Int8, "Uint8Array": Uint8, "Int16Array": Int16, "Uint16Array": Uint16,
	"Int32Array": Int32, "Uint32Array": Uint32, "Float32Array": Float32, "Float64Array": Float64,
}

// ViewNames are the views' constructors' names, by View.
var ViewNames = [...]string{"Int8Array", "Uint8Array", "Int16Array", "Uint16Array", "Int32Array", "Uint32Array", "Float32Array", "Float64Array"}

// Shift is log2 of the view's element size.
func (v View) Shift() uint8 { return [...]uint8{0, 0, 1, 1, 2, 2, 2, 3}[v] }

// Load is the type of a load from the view.
func (v View) Load() Type {
	switch v {
	case Float32:
		return FloatQ
	case Float64:
		return DoubleQ
	}
	return Intish
}

// GlobalKind is what a module-level binding is.
type GlobalKind uint8

const (
	GlobalVar    GlobalKind = iota // a mutable int, double or float, its initialiser a literal
	GlobalImport                   // foreign.x | 0 or +foreign.x: a mutable int or double
	GlobalFFI                      // foreign.f: a function called through the FFI
	GlobalView                     // new stdlib.XArray(heap)
	GlobalMath                     // stdlib.Math.f, a function
	GlobalConst                    // stdlib.Math.K, stdlib.NaN or stdlib.Infinity: a double
	GlobalFunc                     // a function of the module
	GlobalTable                    // a function table
)

// Global is a module-level binding.
type Global struct {
	Name string
	Kind GlobalKind
	// Type is a variable's or a constant's: Int, Double or Float.
	Type Type
	// View is a view's element type, Math the Math member's name, Field
	// an import's name in foreign or stdlib.
	View  View
	Math  string
	Field string
	// Init is a variable's initial value, Value a constant's.
	Init  float64
	Value float64
	// Func is a function's index in Module.Funcs, Table a table's in
	// Module.Tables.
	Func, Table int
}

// Sig is a function's signature: its parameters' types (Int, Double or
// Float) and its result's (Signed, Double, Float or Void).
type Sig struct {
	Params []Type
	Ret    Type
}

func (s Sig) equal(o Sig) bool {
	if s.Ret != o.Ret || len(s.Params) != len(o.Params) {
		return false
	}
	for i, p := range s.Params {
		if p != o.Params[i] {
			return false
		}
	}
	return true
}

// Table is a function table: its functions, a power of two of them, of
// one signature.
type Table struct {
	Name  string
	Funcs []int
	Sig   Sig
}

// Export is a function the module returns, under a name, "" where the
// module returns the function itself.
type Export struct {
	Name string
	Func int
}
