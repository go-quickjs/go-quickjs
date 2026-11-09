package wast

import (
	"strconv"

	"github.com/go-quickjs/go-quickjs/wasm/internal/text"
)

// ValueKind is the kind of a constant.
type ValueKind uint8

const (
	I32 ValueKind = iota
	I64
	F32
	F64
	V128
	// RefNull is (ref.null heaptype).
	RefNull
	// RefExtern is (ref.extern n): host value n, given to WebAssembly as an
	// external reference.
	RefExtern
	// RefHost is (ref.host n): host value n.
	RefHost
)

// Shape is a v128 constant's lane interpretation.
type Shape uint8

const (
	I8x16 Shape = iota
	I16x8
	I32x4
	I64x2
	F32x4
	F64x2
)

var shapes = map[string]Shape{
	"i8x16": I8x16, "i16x8": I16x8, "i32x4": I32x4, "i64x2": I64x2, "f32x4": F32x4, "f64x2": F64x2,
}

// Lanes returns the number of lanes.
func (s Shape) Lanes() int { return [...]int{16, 8, 4, 2, 4, 2}[s] }

// LaneBits returns the width of a lane.
func (s Shape) LaneBits() int { return 128 / s.Lanes() }

// IsFloat reports whether the lanes are floats.
func (s Shape) IsFloat() bool { return s == F32x4 || s == F64x2 }

func (s Shape) String() string {
	return [...]string{"i8x16", "i16x8", "i32x4", "i64x2", "f32x4", "f64x2"}[s]
}

// Value is a constant: an argument, or a result an assertion expects.
type Value struct {
	Kind ValueKind
	// Bits holds an integer's two's complement bits or a float's bits.
	Bits uint64
	// V128 is a vector's bytes, little-endian, and Shape the lanes it was
	// written as.
	V128  [16]byte
	Shape Shape
	// Heap is a null reference's heap type: an abstract heap type's
	// keyword ("func", "extern", "any", "none", ...), or a type index.
	Heap string
	// Ref is a host value's number, for RefExtern and RefHost.
	Ref uint32
}

// NaNKind is a NaN pattern.
type NaNKind uint8

const (
	// NotNaN is an exact value.
	NotNaN NaNKind = iota
	// CanonicalNaN matches a NaN whose payload is the canonical one.
	CanonicalNaN
	// ArithmeticNaN matches a NaN whose payload's top bit is set.
	ArithmeticNaN
)

// ResultKind is the kind of a result pattern.
type ResultKind uint8

const (
	// ResultValue matches a number exactly, by its bits, or a host
	// reference (ref.extern n, ref.host n) by its number.
	ResultValue ResultKind = iota
	// ResultNaN matches a float of type Value.Kind (F32 or F64) that is a
	// NaN of kind NaN.
	ResultNaN
	// ResultV128 matches a vector lane by lane: Lanes, of Value.Shape.
	ResultV128
	// ResultRefNull matches a null reference. Heap is its heap type, or ""
	// for (ref.null) without one, which matches any null.
	ResultRefNull
	// ResultRef matches any non-null reference of the abstract heap type
	// Heap: "any", "eq", "i31", "struct", "array", "func", "exn" or
	// "extern".
	ResultRef
	// ResultEither matches if any of Either does.
	ResultEither
)

// Lane is one lane of a vector pattern: its bits, as wide as the lane, or a
// NaN pattern.
type Lane struct {
	NaN  NaNKind
	Bits uint64
}

// Result is a result pattern of assert_return.
type Result struct {
	Kind   ResultKind
	Value  Value
	NaN    NaNKind
	Lanes  []Lane
	Heap   string
	Either []Result
}

var constKinds = map[string]ValueKind{"i32.const": I32, "i64.const": I64, "f32.const": F32, "f64.const": F64}

func (p *parser) numberToken() text.Token {
	t := p.next()
	switch t.Kind {
	case text.Nat, text.Int, text.Float:
		return t
	}
	p.unexpected(t)
	return t
}

// number parses a number literal of the given kind.
func (p *parser) number(k ValueKind, t text.Token) uint64 {
	var v uint64
	ok := false
	switch k {
	case I32:
		v, ok = text.ParseInt(t.Text, 32)
	case I64:
		v, ok = text.ParseInt(t.Text, 64)
	case F32:
		var f uint32
		f, ok = text.ParseF32(t.Text)
		v = uint64(f)
	case F64:
		v, ok = text.ParseF64(t.Text)
	}
	if !ok {
		p.fail(t, "constant out of range")
	}
	return v
}

// laneValue parses one lane's literal.
func (p *parser) laneValue(s Shape, t text.Token) uint64 {
	var v uint64
	ok := false
	switch s {
	case I8x16:
		v, ok = text.ParseInt(t.Text, 8)
	case I16x8:
		v, ok = text.ParseInt(t.Text, 16)
	case I32x4:
		v, ok = text.ParseInt(t.Text, 32)
	case I64x2:
		v, ok = text.ParseInt(t.Text, 64)
	case F32x4:
		var f uint32
		f, ok = text.ParseF32(t.Text)
		v = uint64(f)
	case F64x2:
		v, ok = text.ParseF64(t.Text)
	}
	if !ok {
		p.fail(t, "constant out of range")
	}
	return v
}

func (p *parser) shape() Shape {
	t := p.next()
	s, ok := shapes[t.Text]
	if t.Kind != text.Keyword || !ok {
		p.unexpected(t)
	}
	return s
}

func putLane(v *[16]byte, s Shape, i int, bits uint64) {
	w := s.LaneBits() / 8
	for j := 0; j < w; j++ {
		v[i*w+j] = byte(bits >> (8 * j))
	}
}

// heapType parses a null reference's heap type.
func (p *parser) heapType() string {
	t := p.next()
	switch t.Kind {
	case text.Keyword:
		switch t.Text {
		case "any", "none", "eq", "i31", "struct", "array", "func", "nofunc",
			"exn", "noexn", "extern", "noextern":
			return t.Text
		}
	case text.Nat:
		if v, ok := text.ParseNat32(t.Text); ok {
			return strconv.FormatUint(uint64(v), 10)
		}
		p.fail(t, "i32 constant out of range")
	}
	p.unexpected(t)
	return ""
}

func (p *parser) refNumber() uint32 {
	t := p.next()
	if t.Kind != text.Nat {
		p.unexpected(t)
	}
	v, ok := text.ParseNat32(t.Text)
	if !ok {
		p.fail(t, "i32 constant out of range")
	}
	return v
}

// literal parses an argument.
func (p *parser) literal() Value {
	p.lparen()
	kw := p.next()
	if kw.Kind != text.Keyword {
		p.unexpected(kw)
	}
	var v Value
	switch kw.Text {
	case "i32.const", "i64.const", "f32.const", "f64.const":
		v.Kind = constKinds[kw.Text]
		v.Bits = p.number(v.Kind, p.numberToken())
	case "v128.const":
		v.Kind = V128
		v.Shape = p.shape()
		for i := 0; i < v.Shape.Lanes(); i++ {
			t := p.peek()
			if k := t.Kind; k != text.Nat && k != text.Int && k != text.Float {
				p.fail(t, "wrong number of lane literals")
			}
			p.p++
			putLane(&v.V128, v.Shape, i, p.laneValue(v.Shape, t))
		}
	case "ref.null":
		v.Kind = RefNull
		v.Heap = p.heapType()
	case "ref.extern":
		v.Kind = RefExtern
		v.Ref = p.refNumber()
	case "ref.host":
		v.Kind = RefHost
		v.Ref = p.refNumber()
	default:
		p.unexpected(kw)
	}
	p.rparen()
	return v
}

// nanKind returns the NaN pattern a token is, if it is one.
func nanKind(t text.Token) NaNKind {
	if t.Kind == text.Keyword {
		switch t.Text {
		case "nan:canonical":
			return CanonicalNaN
		case "nan:arithmetic":
			return ArithmeticNaN
		}
	}
	return NotNaN
}

// result parses a result pattern.
func (p *parser) result() Result {
	p.lparen()
	kw := p.next()
	if kw.Kind != text.Keyword {
		p.unexpected(kw)
	}
	var r Result
	switch kw.Text {
	case "i32.const", "i64.const", "f32.const", "f64.const":
		r.Value.Kind = constKinds[kw.Text]
		if n := nanKind(p.peek()); n != NotNaN {
			if r.Value.Kind != F32 && r.Value.Kind != F64 {
				p.fail(p.peek(), "NaN pattern with non-float type")
			}
			p.p++
			r.Kind, r.NaN = ResultNaN, n
			break
		}
		r.Value.Bits = p.number(r.Value.Kind, p.numberToken())
	case "v128.const":
		r.Kind = ResultV128
		r.Value.Kind = V128
		s := p.shape()
		r.Value.Shape = s
		for p.peek().Kind != text.RParen {
			t := p.next()
			var l Lane
			if n := nanKind(t); n != NotNaN {
				if !s.IsFloat() {
					p.fail(t, "invalid vector constant")
				}
				l.NaN = n
			} else {
				if k := t.Kind; k != text.Nat && k != text.Int && k != text.Float {
					p.unexpected(t)
				}
				l.Bits = p.laneValue(s, t)
			}
			r.Lanes = append(r.Lanes, l)
		}
		if len(r.Lanes) != s.Lanes() {
			p.fail(p.peek(), "wrong number of lane literals")
		}
		for i, l := range r.Lanes {
			putLane(&r.Value.V128, s, i, l.Bits)
		}
	case "ref.null":
		r.Kind = ResultRefNull
		if p.peek().Kind != text.RParen {
			r.Heap = p.heapType()
		}
	case "ref.extern", "ref.host":
		if p.peek().Kind == text.RParen && kw.Text == "ref.extern" {
			r.Kind, r.Heap = ResultRef, "extern"
			break
		}
		r.Kind = ResultValue
		r.Value.Kind = RefExtern
		if kw.Text == "ref.host" {
			r.Value.Kind = RefHost
		}
		r.Value.Ref = p.refNumber()
	case "ref", "ref.any":
		r.Kind, r.Heap = ResultRef, "any"
	case "ref.eq", "ref.i31", "ref.struct", "ref.array", "ref.func", "ref.exn":
		r.Kind, r.Heap = ResultRef, kw.Text[len("ref."):]
	case "either":
		r.Kind = ResultEither
		for p.peek().Kind == text.LParen {
			r.Either = append(r.Either, p.result())
		}
		if len(r.Either) == 0 {
			p.unexpected(p.peek())
		}
	default:
		p.unexpected(kw)
	}
	p.rparen()
	return r
}
