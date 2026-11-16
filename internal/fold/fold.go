// Package fold is what the parser and the compiler's two backends know of
// a program before it runs, decided where each already visits the code,
// in the pass that visits it, never by walking a tree again:
//
//   - The parser folds an operator whose operands are literals as it
//     builds it, as V8's parser does (Parser::BuildUnaryExpression,
//     ShortcutLiteralBinaryExpression): ! of a literal, - + and ~ of a
//     number, - of a BigInt, and an arithmetic, shift or bitwise operator
//     on two numbers are the literal of their value (Not, Negate, BitNot,
//     Arith), so that 1 + 2 * 3 is 7 from the bottom up. + of two string
//     literals is their concatenation (Strings), a chain's run of them
//     joined once, up to the longest string there may be
//     (MaxStringLength); + of a string and a number is left, as V8
//     leaves it.
//   - Each backend compiles typeof and void of a literal as the value
//     (Value), a test that is a literal as a jump or none (Test), leaves
//     out the branch a literal test never takes (Dead), and the
//     statements of a list after one that always leaves it, which it
//     notes as it compiles that one: as V8's ToBooleanIsTrue,
//     ToBooleanIsFalse and RemainderOfBlockIsDead.
//
// Nothing is folded that could run code or be rebound: an identifier --
// undefined, NaN and Infinity among them, which a binding may shadow -- a
// call, a property read. A literal runs nothing.
package fold

import (
	"math"
	"math/big"
	"os"
	"strings"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/fdlibm"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Kind is a kind of fold, which a tool turns off to tell what each one
// changes of a compile's output (QJS_FOLD): compilediff's classification.
type Kind uint32

const (
	// Unary is ! of a literal, - + ~ of a number, - of a BigInt (the
	// parser's), and typeof, void and ! of a value that folds (the
	// backends').
	Unary Kind = 1 << iota
	// Numeric is + - * / % ** << >> >>> & | ^ of two numbers (the
	// parser's).
	Numeric
	// Condition is a test whose value folds: a jump, or none.
	Condition
	// DeadBranch is the branch of an if, or the loop, that a folded test
	// never takes: compiled for its errors and declarations, no code kept.
	DeadBranch
	// Unreachable is the statements of a list after one that always
	// leaves it: compiled so too.
	Unreachable
	// Strings is + of two string literals (the parser's).
	Strings

	// All is every kind, what a compile does.
	All = Unary | Numeric | Condition | DeadBranch | Unreachable | Strings
)

var kinds atomic.Uint32

// kindNames are the kinds' names in QJS_FOLD.
var kindNames = map[string]Kind{
	"unary": Unary, "numeric": Numeric, "condition": Condition,
	"deadbranch": DeadBranch, "unreachable": Unreachable, "strings": Strings,
}

// QJS_FOLD, where set, names the kinds the process folds, separated by
// commas, or "none": what internal/cmd/compilediff's fold command runs a
// compile with, at each kind alone, to tell what each changes.
func init() {
	k := All
	if v, ok := os.LookupEnv("QJS_FOLD"); ok {
		k = 0
		for _, name := range strings.Split(v, ",") {
			k |= kindNames[strings.TrimSpace(name)]
		}
	}
	kinds.Store(uint32(k))
}

// On reports whether kind k is on (QJS_FOLD).
func On(k Kind) bool { return Kind(kinds.Load())&k != 0 }

// Const is a value the compile knows: undefined, null, a boolean, a
// number, a string, or a BigInt, its digits in Str as a literal has them,
// and where its source starts.
type Const struct {
	Kind  ConstKind
	Bool  bool
	Num   float64
	Str   string
	Start int
}

// ConstKind is the type of a Const.
type ConstKind uint8

// The kinds of a Const.
const (
	Undefined ConstKind = iota
	Null
	Bool
	Number
	String
	BigInt
)

// Literal is e's value where e is a literal.
func Literal(e ast.Expr) (Const, bool) {
	switch n := e.(type) {
	case *ast.NumberLit:
		return Const{Kind: Number, Num: n.Value, Start: n.Start}, true
	case *ast.StringLit:
		return Const{Kind: String, Str: n.Value, Start: n.Start}, true
	case *ast.BoolLit:
		return Const{Kind: Bool, Bool: n.Value, Start: n.Start}, true
	case *ast.NullLit:
		return Const{Kind: Null, Start: n.Start}, true
	case *ast.BigIntLit:
		return Const{Kind: BigInt, Str: n.Raw, Start: n.Start}, true
	}
	return Const{}, false
}

// Value is e's value where the compile knows it: e's where it is a
// literal, and (Unary) where it is typeof, void or ! of a value that
// folds -- which the parser has left only over typeof and void, as
// !typeof x or void !0, so that what is looked into is a chain of these
// three operators and no more.
func Value(e ast.Expr) (Const, bool) {
	if v, ok := Literal(e); ok {
		return v, true
	}
	n, ok := e.(*ast.Unary)
	if !ok || !On(Unary) {
		return Const{}, false
	}
	switch n.Op {
	case "!", "typeof", "void":
	default:
		return Const{}, false
	}
	v, ok := Value(n.Operand)
	if !ok {
		return Const{}, false
	}
	switch n.Op {
	case "!":
		t, ok := Truthy(v)
		if !ok {
			return Const{}, false
		}
		return Const{Kind: Bool, Bool: !t, Start: n.Start}, true
	case "typeof":
		return Const{Kind: String, Str: typeOf(v), Start: n.Start}, true
	}
	return Const{Kind: Undefined, Start: n.Start}, true
}

// Not is !lit for a literal, and whether it folds (Unary): any literal,
// as V8's parser folds it.
func Not(lit ast.Expr) (truthy, ok bool) {
	if !On(Unary) {
		return false, false
	}
	v, ok := Literal(lit)
	if !ok {
		return false, false
	}
	t, ok := Truthy(v)
	return !t, ok
}

// Negate is -raw of a BigInt literal's digits, as decimal digits, a
// constant as the compilers have always made one of -1n.
func Negate(raw string) (string, bool) {
	b, ok := bigValue(raw)
	if !ok {
		return "", false
	}
	return b.Neg(b).String(), true
}

// BitNot is ~v, as the runtime computes it.
func BitNot(v float64) float64 { return float64(^jsnum.ToInt32(v)) }

// Arith is op of two numbers, and whether op is one that folds: as
// V8's parser folds it, and as the runtime computes it (vm's numericOp
// and int32Op): the same Go operations, jsMod's and jsPow's cases first,
// which TestNumericFolds holds to the runtime's answers.
func Arith(op string, a, b float64) (float64, bool) {
	switch op {
	case "+":
		return a + b, true
	case "-":
		return a - b, true
	case "*":
		return a * b, true
	case "/":
		return a / b, true
	case "%":
		return mod(a, b), true
	case "**":
		return pow(a, b), true
	case "<<":
		return float64(jsnum.ToInt32(a) << (jsnum.ToUint32(b) & 31)), true
	case ">>":
		return float64(jsnum.ToInt32(a) >> (jsnum.ToUint32(b) & 31)), true
	case ">>>":
		return float64(jsnum.ToUint32(a) >> (jsnum.ToUint32(b) & 31)), true
	case "&":
		return float64(jsnum.ToInt32(a) & jsnum.ToInt32(b)), true
	case "|":
		return float64(jsnum.ToInt32(a) | jsnum.ToInt32(b)), true
	case "^":
		return float64(jsnum.ToInt32(a) ^ jsnum.ToInt32(b)), true
	}
	return 0, false
}

// MaxStringLength is the most UTF-16 code units a string may have: the
// runtime's (vm's maxStringLength), which a concatenation past it throws a
// RangeError for, and V8's String::kMaxLength. Strings concatenates no
// literals past it, so that the program throws as it runs, as it would
// have.
const MaxStringLength = is64Bit*(1<<29-24) + (1-is64Bit)*(1<<28-16)

// is64Bit is 1 where an int has 64 bits, and 0 where it has 32, as vm's.
const is64Bit = math.MaxInt >> 62 & 1

// mod is the % operator on numbers, as the runtime's jsMod computes it.
func mod(a, b float64) float64 {
	if ai, bi := int32(a), int32(b); float64(ai) == a && float64(bi) == b && ai > 0 && bi != 0 {
		return float64(ai % bi)
	}
	return math.Mod(a, b)
}

// pow is the ** operator on numbers, as the runtime's jsPow computes it.
func pow(a, b float64) float64 {
	switch {
	case math.IsNaN(b):
		return math.NaN()
	case (a == 1 || a == -1) && math.IsInf(b, 0):
		return math.NaN()
	case b == 2:
		return a * a
	case b == 0.5:
		if math.IsInf(a, 0) {
			return math.Inf(1)
		}
		return math.Sqrt(a + 0)
	}
	return fdlibm.PowC(a, b)
}

// bigValue is the value of a BigInt literal's digits.
func bigValue(raw string) (*big.Int, bool) {
	return new(big.Int).SetString(raw, 0)
}

// Truthy is ToBoolean of v, and whether it is known: a BigInt literal's
// digits are read for whether they are zero.
func Truthy(v Const) (truthy, ok bool) {
	switch v.Kind {
	case Undefined, Null:
		return false, true
	case Bool:
		return v.Bool, true
	case Number:
		return v.Num != 0 && !math.IsNaN(v.Num), true
	case String:
		return v.Str != "", true
	case BigInt:
		if b, ok := bigValue(v.Str); ok {
			return b.Sign() != 0, true
		}
	}
	return false, false
}

// typeOf is typeof of v.
func typeOf(v Const) string {
	return [...]string{"undefined", "object", "boolean", "number", "string", "bigint"}[v.Kind]
}

// Test is the truth of test where the compile knows it (Condition).
func Test(test ast.Expr) (truthy, ok bool) {
	if !On(Condition) {
		return false, false
	}
	v, ok := Value(test)
	if !ok {
		return false, false
	}
	return Truthy(v)
}

// Dead reports whether test, an if's or a loop's, folds and the branch it
// never takes is left out (DeadBranch), and which way it goes.
func Dead(test ast.Expr) (truthy, ok bool) {
	if !On(DeadBranch) {
		return false, false
	}
	v, ok := Value(test)
	if !ok {
		return false, false
	}
	return Truthy(v)
}

// Leaves reports whether s, compiled already, is a statement that always
// leaves the list it is in -- a return, a throw, a break or a continue --
// or one whose compile found so of what it holds (a block, an if), which
// the compile passes as held: so that nothing after it in the list is
// reached (Unreachable), V8's RemainderOfBlockIsDead. A labelled
// statement may be left to the statement after it, and a try, a switch
// or a loop are not looked into.
func Leaves(s ast.Stmt, held bool) bool {
	switch s.(type) {
	case *ast.ReturnStmt, *ast.ThrowStmt, *ast.BreakStmt, *ast.ContinueStmt:
		return true
	case *ast.BlockStmt, *ast.IfStmt:
		return held
	}
	return false
}
