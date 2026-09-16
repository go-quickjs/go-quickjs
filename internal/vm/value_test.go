package vm

import (
	"math"
	"testing"
	"unsafe"
)

func TestValueKinds(t *testing.T) {
	tests := []struct {
		v    Value
		want Kind
	}{
		{Float(1.5), KindNumber},
		{Float(0), KindNumber},
		{Float(math.Inf(1)), KindNumber},
		{Float(math.Inf(-1)), KindNumber},
		{Float(math.NaN()), KindNumber},
		{Int(42), KindNumber},
		{Undefined, KindUndefined},
		{Null, KindNull},
		{True, KindBool},
		{False, KindBool},
		{Str(NewString("x")), KindString},
		{Sym(NewSymbol("s", true)), KindSymbol},
		{Big(NewBigInt(1)), KindBigInt},
		{Obj(newObject(nil, ClassObject)), KindObject},
		{uninitialized, KindUninitialized},
	}
	for _, tt := range tests {
		if got := tt.v.Kind(); got != tt.want {
			t.Errorf("Kind() = %v, want %v", got, tt.want)
		}
	}
}

// TestNaNNeverCollidesWithATag guards the invariant the whole NaN-boxed
// representation rests on. A negative quiet NaN has the same bit pattern as the
// tag space, so if Float failed to normalize it, a number would be
// misidentified as some other kind.
func TestNaNNeverCollidesWithATag(t *testing.T) {
	candidates := []float64{
		math.NaN(),
		-math.NaN(),
		math.Float64frombits(0xFFF8000000000000), // exactly tagBase
		math.Float64frombits(0xFFF8000000000001), // tagBase + KindUndefined
		math.Float64frombits(0xFFF8000000000007), // tagBase + KindObject
		math.Float64frombits(0xFFFFFFFFFFFFFFFF), // all ones
		math.Float64frombits(0x7FF8000000000000),
		0 / math.Inf(1) * math.Inf(1), // an arithmetic NaN
	}
	for _, f := range candidates {
		v := Float(f)
		if v.Kind() != KindNumber {
			t.Errorf("Float(%#x) has kind %v, want number",
				math.Float64bits(f), v.Kind())
		}
		if !v.IsNumber() {
			t.Errorf("Float(%#x).IsNumber() = false", math.Float64bits(f))
		}
		if f != f && !v.IsNaN() {
			t.Errorf("Float(NaN %#x).IsNaN() = false", math.Float64bits(f))
		}
	}
}

func TestEveryNonNumberTagIsDistinct(t *testing.T) {
	// Each tagged kind must decode to itself, or values of different types
	// would be confused for one another.
	seen := map[Kind]bool{}
	for k := KindUndefined; k <= KindUninitialized; k++ {
		v := Value{num: mkTag(k, 0)}
		got := v.Kind()
		if got != k {
			t.Errorf("tag for %v decoded as %v", k, got)
		}
		if seen[got] {
			t.Errorf("duplicate tag for %v", got)
		}
		seen[got] = true
	}
}

func TestValueSizeIsTwoWordsPlusPayload(t *testing.T) {
	// The representation is chosen for size; if it grows, the operand stack and
	// every property slot grow with it.
	const want = 24
	if got := unsafe.Sizeof(Value{}); got != want {
		t.Errorf("sizeof(Value) = %d, want %d", got, want)
	}
}

func TestBoolPayload(t *testing.T) {
	if !True.BoolValue() {
		t.Error("True.BoolValue() = false")
	}
	if False.BoolValue() {
		t.Error("False.BoolValue() = true")
	}
	if !Bool(true).StrictEquals(True) || !Bool(false).StrictEquals(False) {
		t.Error("Bool did not produce the shared constants")
	}
}

func TestTruthy(t *testing.T) {
	tests := []struct {
		v    Value
		want bool
	}{
		{Float(1), true},
		{Float(0), false},
		{Float(math.Copysign(0, -1)), false}, // -0 is falsy
		{Float(math.NaN()), false},
		{Undefined, false},
		{Null, false},
		{True, true},
		{False, false},
		{Str(NewString("")), false},
		{Str(NewString("0")), true}, // a non-empty string is truthy
		{Big(NewBigInt(0)), false},
		{Big(NewBigInt(1)), true},
		{Obj(newObject(nil, ClassObject)), true},
	}
	for _, tt := range tests {
		if got := tt.v.Truthy(); got != tt.want {
			t.Errorf("Truthy(%v) = %v, want %v", tt.v.Kind(), got, tt.want)
		}
	}
}

func TestTypeOf(t *testing.T) {
	fn := newObject(nil, ClassFunction)
	fn.data = &funcData{}

	tests := []struct {
		v    Value
		want string
	}{
		{Float(1), "number"},
		{Undefined, "undefined"},
		{Null, "object"}, // the famous mistake, preserved
		{True, "boolean"},
		{Str(NewString("x")), "string"},
		{Sym(NewSymbol("s", true)), "symbol"},
		{Big(NewBigInt(1)), "bigint"},
		{Obj(newObject(nil, ClassObject)), "object"},
		{Obj(fn), "function"},
	}
	for _, tt := range tests {
		if got := tt.v.TypeOf(); got != tt.want {
			t.Errorf("TypeOf() = %q, want %q", got, tt.want)
		}
	}
}

func TestStrictEquals(t *testing.T) {
	o := newObject(nil, ClassObject)
	s := NewSymbol("s", true)

	// NaN is not equal to itself, and +0 === -0.
	if Float(math.NaN()).StrictEquals(Float(math.NaN())) {
		t.Error("NaN === NaN should be false")
	}
	if !Float(0).StrictEquals(Float(math.Copysign(0, -1))) {
		t.Error("+0 === -0 should be true")
	}
	if !Str(NewString("ab")).StrictEquals(Str(NewString("ab"))) {
		t.Error("equal strings should be ===")
	}
	if Str(NewString("a")).StrictEquals(Str(NewString("b"))) {
		t.Error("different strings should not be ===")
	}
	if !Obj(o).StrictEquals(Obj(o)) {
		t.Error("an object should be === itself")
	}
	if Obj(o).StrictEquals(Obj(newObject(nil, ClassObject))) {
		t.Error("distinct objects should not be ===")
	}
	if !Sym(s).StrictEquals(Sym(s)) {
		t.Error("a symbol should be === itself")
	}
	if Sym(s).StrictEquals(Sym(NewSymbol("s", true))) {
		t.Error("symbols with the same description are still distinct")
	}
	// Different types are never strictly equal.
	if Float(0).StrictEquals(Str(NewString("0"))) {
		t.Error("0 === \"0\" should be false")
	}
	if Null.StrictEquals(Undefined) {
		t.Error("null === undefined should be false")
	}
}

func TestSameValueAndSameValueZero(t *testing.T) {
	nan, zero, negZero := Float(math.NaN()), Float(0), Float(math.Copysign(0, -1))

	// SameValueZero: NaN equals itself, +0 equals -0.
	if !nan.SameValueZero(nan) {
		t.Error("SameValueZero(NaN, NaN) should be true")
	}
	if !zero.SameValueZero(negZero) {
		t.Error("SameValueZero(+0, -0) should be true")
	}
	// SameValue (Object.is): NaN equals itself, +0 differs from -0.
	if !nan.SameValue(nan) {
		t.Error("SameValue(NaN, NaN) should be true")
	}
	if zero.SameValue(negZero) {
		t.Error("SameValue(+0, -0) should be false")
	}
}

// BenchmarkKindDispatch measures the cost of the kind check that every
// instruction in the interpreter performs.
func BenchmarkKindDispatch(b *testing.B) {
	vals := []Value{
		Float(1.5), Undefined, Null, True,
		Str(NewString("x")), Obj(newObject(nil, ClassObject)),
	}
	b.ReportAllocs()
	var n int
	for b.Loop() {
		for _, v := range vals {
			n += int(v.Kind())
		}
	}
	if n == 0 {
		b.Fatal("optimized away")
	}
}

func BenchmarkFloatConstruction(b *testing.B) {
	b.ReportAllocs()
	var v Value
	for b.Loop() {
		v = Float(3.14159)
	}
	if v.Kind() != KindNumber {
		b.Fatal("unexpected kind")
	}
}
