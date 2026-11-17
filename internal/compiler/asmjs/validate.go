package asmjs

import (
	"fmt"
	"math"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/ast"
)

// Module is a validated asm.js module: its parameters' names, its
// bindings, functions and tables, and what it returns.
type Module struct {
	Lit                   *ast.FuncLit
	Stdlib, Foreign, Heap string
	Globals               []*Global
	Funcs                 []*Func
	Tables                []*Table
	Exports               []Export
	byName                map[string]*Global
	text                  string
}

// Global is the module-level binding of name, or nil.
func (m *Module) Global(name string) *Global { return m.byName[name] }

// Func is a function of a module: its literal, its signature, its locals
// (its parameters first), and the type of each of its expressions.
type Func struct {
	Name   string
	Lit    *ast.FuncLit
	Sig    Sig
	Locals []Local
	// Header is how many of the body's statements are the parameters'
	// annotations and the locals' declarations.
	Header int
	// Types is each expression's type, as the rules gave it.
	Types map[ast.Expr]Type
	// Calls is what each call calls, and with what result.
	Calls map[*ast.Call]*Call

	byName map[string]int
	retSet bool
}

// Local is the index of the local name, or -1.
func (f *Func) Local(name string) int {
	if i, ok := f.byName[name]; ok {
		return i
	}
	return -1
}

// Local is a function's parameter or local variable: its type (Int,
// Double or Float) and a local's initial value.
type Local struct {
	Name string
	Type Type
	Init float64
}

// CallKind is what a call calls.
type CallKind uint8

const (
	CallFunc  CallKind = iota // a function of the module
	CallTable                 // a function of a table, by its index
	CallFFI                   // an import, through the FFI
	CallMath                  // a stdlib Math function
)

// Call is a call of a function: what it calls, and the type its result
// is coerced to (Signed, Double, Float or Void; a Math function's own).
type Call struct {
	Kind   CallKind
	Global *Global
	Ret    Type
	// Index is a table call's index expression, before its mask.
	Index Expr
}

// Expr is an expression, as Call.Index holds one.
type Expr = ast.Expr

// Error is why a module is not asm.js, at a position of its source.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("asm.js: %s (at %d)", e.Msg, e.Pos) }

type failure struct{ err *Error }

// IsModule reports whether lit's body begins with a directive prologue
// that holds "use asm", whose text is text.
func IsModule(lit *ast.FuncLit, text string) bool {
	for _, s := range lit.Body {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return false
		}
		str, ok := es.X.(*ast.StringLit)
		if !ok {
			return false
		}
		if raw := rawString(text, str.Start); raw == `"use asm"` || raw == `'use asm'` {
			return true
		}
	}
	return false
}

// rawString is the string literal's source at start, quotes and all.
func rawString(text string, start int) string {
	if start < 0 || start >= len(text) {
		return ""
	}
	q := text[start]
	if q != '"' && q != '\'' {
		return ""
	}
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case q:
			return text[start : i+1]
		case '\n':
			return ""
		}
	}
	return ""
}

// Validate validates lit, a function whose source text is text, as an
// asm.js module.
func Validate(lit *ast.FuncLit, text string) (m *Module, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(failure)
			if !ok {
				panic(r)
			}
			m, err = nil, f.err
		}
	}()
	v := &validator{m: &Module{Lit: lit, byName: map[string]*Global{}, text: text}}
	v.module()
	return v.m, nil
}

type validator struct {
	m *Module
	f *Func
	// sigs are the signatures the calls of each function and table want,
	// checked against theirs at the end.
	callSigs  map[int][]sigAt
	tableSigs map[int][]sigAt
	// tableMasks are the masks of the table calls, each its table's
	// length less one.
	tableMasks []tableMask
}

type sigAt struct {
	sig Sig
	pos int
}

func (v *validator) fail(pos int, format string, args ...any) {
	panic(failure{&Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}})
}

func (v *validator) module() {
	lit, m := v.m.Lit, v.m
	if lit.Kind != ast.FuncNormal || lit.Async || lit.Generator || lit.ExprBody {
		v.fail(lit.Start, "not a plain function")
	}
	if len(lit.Params) > 3 {
		v.fail(lit.Start, "more than three parameters")
	}
	names := map[string]bool{}
	for i, p := range lit.Params {
		id, ok := p.(*ast.Ident)
		if !ok {
			v.fail(p.Pos(), "a parameter that is not a name")
		}
		if names[id.Name] {
			v.fail(id.Pos(), "%s is two parameters", id.Name)
		}
		names[id.Name] = true
		*[]*string{&m.Stdlib, &m.Foreign, &m.Heap}[i] = id.Name
	}
	body := lit.Body
	i := 0
	for i < len(body) {
		es, ok := body[i].(*ast.ExprStmt)
		if !ok {
			break
		}
		if _, ok := es.X.(*ast.StringLit); !ok {
			break
		}
		i++
	}
	v.callSigs, v.tableSigs = map[int][]sigAt{}, map[int][]sigAt{}
	// The bindings, the functions, the tables, the return: in that order.
	const (
		inVars = iota
		inFuncs
		inTables
		done
	)
	state := inVars
	var tableDecls []*ast.VarDecl
	for ; i < len(body); i++ {
		switch s := body[i].(type) {
		case *ast.VarDecl:
			if s.Kind != ast.DeclVar {
				v.fail(s.Start, "a declaration that is not var")
			}
			if state == inVars {
				if isTableDecl(s) {
					v.fail(s.Start, "a function table before the functions")
				}
				for _, d := range s.Decls {
					v.global(d, names)
				}
				continue
			}
			if state > inTables {
				v.fail(s.Start, "a declaration after the return")
			}
			state = inTables
			tableDecls = append(tableDecls, s)
			for _, d := range s.Decls {
				id, ok := d.Target.(*ast.Ident)
				if !ok || d.Init == nil {
					v.fail(s.Start, "a function table that is not a name and an array")
				}
				arr, ok := d.Init.(*ast.ArrayLit)
				if !ok {
					v.fail(s.Start, "a declaration after the functions that is no function table")
				}
				v.declare(id, names)
				n := len(arr.Elements)
				if n == 0 || n&(n-1) != 0 {
					v.fail(int(arr.Start), "a function table of %d functions, not a power of two", n)
				}
				g := &Global{Name: id.Name, Kind: GlobalTable, Table: len(m.Tables)}
				m.Tables = append(m.Tables, &Table{Name: id.Name})
				m.byName[id.Name] = g
				m.Globals = append(m.Globals, g)
			}
		case *ast.FuncDecl:
			if state > inFuncs {
				v.fail(s.Start, "a function after the function tables")
			}
			state = inFuncs
			v.declare(s.Fn.Name, names)
			g := &Global{Name: s.Fn.Name.Name, Kind: GlobalFunc, Func: len(m.Funcs)}
			m.byName[g.Name] = g
			m.Globals = append(m.Globals, g)
			m.Funcs = append(m.Funcs, &Func{Name: g.Name, Lit: s.Fn})
		case *ast.ReturnStmt:
			if state == done {
				v.fail(s.Start, "a second return")
			}
			state = done
			v.exports(s)
		default:
			v.fail(s.Pos(), "a statement an asm.js module has none of")
		}
	}
	if state != done {
		v.fail(lit.Start, "no return of the exports")
	}
	// The functions' headers, then their bodies, then the tables, whose
	// functions' signatures are known by then.
	for _, f := range m.Funcs {
		v.header(f)
	}
	for _, f := range m.Funcs {
		v.f = f
		v.body(f)
		v.f = nil
	}
	ti := 0
	for _, d := range tableDecls {
		for _, decl := range d.Decls {
			t := m.Tables[ti]
			ti++
			for k, e := range decl.Init.(*ast.ArrayLit).Elements {
				id, ok := e.(*ast.Ident)
				g := m.Global(nameOf(id, ok))
				if !ok || g == nil || g.Kind != GlobalFunc {
					v.fail(d.Start, "a function table's element that is not a function of the module")
				}
				sig := m.Funcs[g.Func].Sig
				if k == 0 {
					t.Sig = sig
				} else if !t.Sig.equal(sig) {
					v.fail(id.Pos(), "%s's signature is not its table's", id.Name)
				}
				t.Funcs = append(t.Funcs, g.Func)
			}
		}
	}
	for _, tm := range v.tableMasks {
		if tm.mask != float64(len(m.Tables[tm.table].Funcs)-1) {
			v.fail(tm.pos, "a call through %s masked by %v, not its length less one", m.Tables[tm.table].Name, tm.mask)
		}
	}
	for fi, uses := range v.callSigs {
		for _, u := range uses {
			if !m.Funcs[fi].Sig.equal(u.sig) {
				v.fail(u.pos, "a call of %s with another signature than its own", m.Funcs[fi].Name)
			}
		}
	}
	for ti, uses := range v.tableSigs {
		for _, u := range uses {
			if !m.Tables[ti].Sig.equal(u.sig) {
				v.fail(u.pos, "a call through %s with another signature than its functions'", m.Tables[ti].Name)
			}
		}
	}
}

func nameOf(id *ast.Ident, ok bool) string {
	if !ok {
		return ""
	}
	return id.Name
}

func isTableDecl(s *ast.VarDecl) bool {
	for _, d := range s.Decls {
		if _, ok := d.Init.(*ast.ArrayLit); ok {
			return true
		}
	}
	return false
}

// declare declares the module-level name id.
func (v *validator) declare(id *ast.Ident, names map[string]bool) {
	if id == nil {
		v.fail(v.m.Lit.Start, "a function with no name")
	}
	if names[id.Name] || id.Name == "arguments" || id.Name == "eval" {
		v.fail(id.Pos(), "%s is declared twice", id.Name)
	}
	names[id.Name] = true
}

// global validates the module's var declarator d.
func (v *validator) global(d ast.Declarator, names map[string]bool) {
	m := v.m
	id, ok := d.Target.(*ast.Ident)
	if !ok || d.Init == nil {
		v.fail(v.m.Lit.Start, "a module variable that is not a name with an initialiser")
	}
	v.declare(id, names)
	g := &Global{Name: id.Name}
	switch e := d.Init.(type) {
	case *ast.NumberLit, *ast.Unary:
		t, val, ok := v.literal(e)
		if !ok {
			if u, isU := e.(*ast.Unary); isU && u.Op == "+" {
				// +foreign.x, a double import.
				if field, ok := v.member(u.Operand, m.Foreign); ok {
					g.Kind, g.Type, g.Field = GlobalImport, Double, field
					break
				}
			}
			v.fail(e.Pos(), "%s's initialiser is not a literal or an import", id.Name)
		}
		g.Kind, g.Init = GlobalVar, val
		g.Type = Double
		if t.IsInt() {
			g.Type = Int
		}
	case *ast.Binary:
		// foreign.x | 0, an int import.
		field, ok := v.member(e.Left, m.Foreign)
		if !ok || e.Op != "|" || !v.isZero(e.Right) {
			v.fail(e.Pos(), "%s's initialiser is not an import", id.Name)
		}
		g.Kind, g.Type, g.Field = GlobalImport, Int, field
	case *ast.Member:
		if field, ok := v.member(e, m.Foreign); ok {
			g.Kind, g.Field = GlobalFFI, field
			break
		}
		if field, ok := v.member(e, m.Stdlib); ok && (field == "NaN" || field == "Infinity") {
			g.Kind, g.Type, g.Field = GlobalConst, Double, field
			g.Value = math.NaN()
			if field == "Infinity" {
				g.Value = math.Inf(1)
			}
			break
		}
		if math, ok := v.member(e.Object, m.Stdlib); ok && math == "Math" {
			if field, ok := v.member(e, ""); ok {
				if k, isConst := mathConsts[field]; isConst {
					g.Kind, g.Type, g.Field, g.Value = GlobalConst, Double, field, k
					break
				}
				if _, isFunc := mathFuncs[field]; isFunc {
					g.Kind, g.Math = GlobalMath, field
					break
				}
			}
		}
		v.fail(e.Pos(), "%s's initialiser is no import of foreign or stdlib", id.Name)
	case *ast.New:
		ctor, ok := v.member(e.Callee, m.Stdlib)
		view, isView := viewCtors[ctor]
		if !ok || !isView || len(e.Args) != 1 {
			v.fail(e.Pos(), "%s is no view of the heap", id.Name)
		}
		if h, ok := e.Args[0].(*ast.Ident); !ok || h.Name != m.Heap || m.Heap == "" {
			v.fail(e.Pos(), "%s is a view of something other than the heap", id.Name)
		}
		g.Kind, g.View = GlobalView, view
	case *ast.Call:
		// fround(literal): a float variable.
		if len(e.Args) == 1 && v.isFround(e.Callee) {
			if t, val, ok := v.literal(e.Args[0]); ok {
				_ = t
				g.Kind, g.Type, g.Init = GlobalVar, Float, float64(float32(val))
				break
			}
		}
		v.fail(e.Pos(), "%s's initialiser is not a literal or an import", id.Name)
	default:
		v.fail(d.Init.Pos(), "%s's initialiser is not a literal or an import", id.Name)
	}
	m.byName[id.Name] = g
	m.Globals = append(m.Globals, g)
}

// member is the field name of e, a member base.name, where base is the
// name want ("" for any base).
func (v *validator) member(e ast.Expr, want string) (string, bool) {
	mb, ok := e.(*ast.Member)
	if !ok || mb.Computed || mb.Optional {
		return "", false
	}
	if want != "" {
		b, ok := mb.Object.(*ast.Ident)
		if !ok || b.Name != want {
			return "", false
		}
	}
	p, ok := mb.Property.(*ast.Ident)
	if !ok {
		return "", false
	}
	return p.Name, true
}

// isFround reports whether e names the module's import of Math.fround.
func (v *validator) isFround(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok || v.f != nil && v.f.Local(id.Name) >= 0 {
		return false
	}
	g := v.m.Global(id.Name)
	return g != nil && g.Kind == GlobalMath && g.Math == "fround"
}

func (v *validator) isZero(e ast.Expr) bool {
	t, val, ok := v.literal(e)
	return ok && t.IsInt() && val == 0 && !math.Signbit(val)
}

// exports validates the module's return.
func (v *validator) exports(s *ast.ReturnStmt) {
	fn := func(e ast.Expr) int {
		id, ok := e.(*ast.Ident)
		g := v.m.Global(nameOf(id, ok))
		if g == nil || g.Kind != GlobalFunc {
			v.fail(e.Pos(), "an export that is not a function of the module")
		}
		return g.Func
	}
	switch e := s.Arg.(type) {
	case *ast.Ident:
		v.m.Exports = append(v.m.Exports, Export{Func: fn(e)})
	case *ast.ObjectLit:
		for _, p := range e.Props {
			if p.Kind != ast.PropInit || p.Computed || p.Method {
				v.fail(p.Start, "an export that is not a name: a function")
			}
			var name string
			switch k := p.Key.(type) {
			case *ast.Ident:
				name = k.Name
			case *ast.StringLit:
				name = k.Value
			default:
				v.fail(p.Start, "an export's name that is not a name")
			}
			v.m.Exports = append(v.m.Exports, Export{Name: name, Func: fn(p.Value)})
		}
	default:
		v.fail(s.Start, "a return of something other than a function or an object of them")
	}
}

// literal is e's type and value where e is a numeric literal, or a
// negated one: a double where its text has a decimal point, an int
// otherwise. The parser folds an operator of literals into the literal
// of its value (parser's unaryOf and binaryOf), which keeps the first
// operand's position: a folded negation is typed as the literal it
// negates, a folded ~ as a signed, a folded + as a double, and a folded
// binary operator by its first operand's text and its value -- the value
// JavaScript gives the expression, whose type the unfolded rules would
// give at least as narrowly. A folded ! of a literal is an int, 0 or 1.
func (v *validator) literal(e ast.Expr) (Type, float64, bool) {
	neg := false
	if u, ok := e.(*ast.Unary); ok && u.Op == "-" {
		neg, e = true, u.Operand
	}
	text := v.m.text
	if b, ok := e.(*ast.BoolLit); ok && !neg && b.Start < len(text) && text[b.Start] == '!' {
		if b.Value {
			return Fixnum, 1, true
		}
		return Fixnum, 0, true
	}
	n, ok := e.(*ast.NumberLit)
	if !ok || n.Start < 0 || n.Start >= len(text) {
		return 0, 0, false
	}
	val := n.Value
	raw := ""
	switch c := text[n.Start]; {
	case c >= '0' && c <= '9' || c == '.':
		raw = numberText(text, n.Start)
	case c == '-':
		// A negation the parser folded: the literal it negates.
		if neg {
			return 0, 0, false
		}
		j := n.Start + 1
		for j < len(text) && strings.IndexByte(" \t\r\n", text[j]) >= 0 {
			j++
		}
		raw = numberText(text, j)
		neg, val = true, -val
	case c == '~':
		if neg || val != math.Trunc(val) || val < -(1<<31) || val >= 1<<31 {
			return 0, 0, false
		}
		if val >= 0 {
			return Fixnum, val, true
		}
		return Signed, val, true
	case c == '+':
		if neg {
			val = -val
		}
		return Double, val, true
	}
	if raw == "" {
		return 0, 0, false
	}
	if neg {
		val = -val
	}
	if strings.Contains(raw, ".") && !strings.HasPrefix(raw, "0x") && !strings.HasPrefix(raw, "0X") {
		return Double, val, true
	}
	if val != math.Trunc(val) || math.IsInf(val, 0) {
		return 0, 0, false
	}
	switch {
	case val == 0 && math.Signbit(val):
		// -0 is a double.
		return Double, val, true
	case val >= 0 && val < 1<<31:
		return Fixnum, val, true
	case val < 0 && val >= -(1<<31):
		return Signed, val, true
	case val >= 1<<31 && val < 1<<32:
		return Unsigned, val, true
	}
	return 0, 0, false
}

// numberText is the numeric literal's source at start.
func numberText(text string, start int) string {
	if start < 0 || start >= len(text) {
		return ""
	}
	i := start
	for i < len(text) {
		c := text[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.':
			i++
		case (c == '+' || c == '-') && i > start && (text[i-1] == 'e' || text[i-1] == 'E') && !strings.HasPrefix(text[start:], "0x") && !strings.HasPrefix(text[start:], "0X"):
			i++
		default:
			return text[start:i]
		}
	}
	return text[start:i]
}

// header reads f's parameters' annotations and its locals' declarations.
func (v *validator) header(f *Func) {
	lit := f.Lit
	if lit.Kind != ast.FuncNormal || lit.Async || lit.Generator {
		v.fail(lit.Start, "%s is not a plain function", f.Name)
	}
	f.byName = map[string]int{}
	f.Types = map[ast.Expr]Type{}
	f.Calls = map[*ast.Call]*Call{}
	addLocal := func(id *ast.Ident, t Type, init float64) {
		if _, dup := f.byName[id.Name]; dup || id.Name == "arguments" || id.Name == "eval" {
			v.fail(id.Pos(), "%s is declared twice in %s", id.Name, f.Name)
		}
		f.byName[id.Name] = len(f.Locals)
		f.Locals = append(f.Locals, Local{Name: id.Name, Type: t, Init: init})
	}
	body := lit.Body
	for i, p := range lit.Params {
		id, ok := p.(*ast.Ident)
		if !ok {
			v.fail(p.Pos(), "a parameter of %s that is not a name", f.Name)
		}
		if i >= len(body) {
			v.fail(id.Pos(), "%s's parameter %s has no annotation", f.Name, id.Name)
		}
		t, ok := v.annotation(body[i], id.Name)
		if !ok {
			v.fail(body[i].Pos(), "%s's parameter %s has no annotation", f.Name, id.Name)
		}
		addLocal(id, t, 0)
		f.Sig.Params = append(f.Sig.Params, t)
	}
	i := len(lit.Params)
	for ; i < len(body); i++ {
		d, ok := body[i].(*ast.VarDecl)
		if !ok {
			break
		}
		if d.Kind != ast.DeclVar {
			v.fail(d.Start, "a declaration that is not var")
		}
		for _, decl := range d.Decls {
			id, ok := decl.Target.(*ast.Ident)
			if !ok || decl.Init == nil {
				v.fail(d.Start, "a local that is not a name with an initialiser")
			}
			if t, val, ok := v.literal(decl.Init); ok {
				lt := Double
				if t.IsInt() {
					lt = Int
				}
				addLocal(id, lt, val)
				continue
			}
			if c, ok := decl.Init.(*ast.Call); ok && len(c.Args) == 1 && v.isFround(c.Callee) {
				if _, val, ok := v.literal(c.Args[0]); ok {
					addLocal(id, Float, float64(float32(val)))
					continue
				}
			}
			v.fail(decl.Init.Pos(), "%s's initialiser is not a literal", id.Name)
		}
	}
	f.Header = i
}

// annotation is the type the statement s annotates parameter p with:
// p = p | 0, p = +p or p = fround(p).
func (v *validator) annotation(s ast.Stmt, p string) (Type, bool) {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return 0, false
	}
	a, ok := es.X.(*ast.Assign)
	if !ok || a.Op != "=" {
		return 0, false
	}
	if id, ok := a.Target.(*ast.Ident); !ok || id.Name != p {
		return 0, false
	}
	isP := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == p }
	switch e := a.Value.(type) {
	case *ast.Binary:
		if e.Op == "|" && isP(e.Left) && v.isZero(e.Right) {
			return Int, true
		}
	case *ast.Unary:
		if e.Op == "+" && isP(e.Operand) {
			return Double, true
		}
	case *ast.Call:
		if len(e.Args) == 1 && isP(e.Args[0]) && v.isFround(e.Callee) {
			return Float, true
		}
	}
	return 0, false
}

// body validates f's statements past its header.
func (v *validator) body(f *Func) {
	for _, s := range f.Lit.Body[f.Header:] {
		v.stmt(s)
	}
	if !f.retSet {
		f.Sig.Ret = Void
	}
}

func (v *validator) stmt(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.ExprStmt:
		if c, ok := s.X.(*ast.Call); ok && !v.isMathCall(c) {
			v.call(c, Void)
			return
		}
		v.expr(s.X)
	case *ast.BlockStmt:
		for _, b := range s.Body {
			v.stmt(b)
		}
	case *ast.EmptyStmt:
	case *ast.IfStmt:
		v.want(s.Test, Int, "an if's condition")
		v.stmt(s.Cons)
		if s.Alt != nil {
			v.stmt(s.Alt)
		}
	case *ast.WhileStmt:
		v.want(s.Test, Int, "a loop's condition")
		v.stmt(s.Body)
	case *ast.DoWhileStmt:
		v.stmt(s.Body)
		v.want(s.Test, Int, "a loop's condition")
	case *ast.ForStmt:
		if s.Init != nil {
			es, ok := s.Init.(*ast.ExprStmt)
			if !ok {
				v.fail(s.Start, "a for's declaration")
			}
			v.expr(es.X)
		}
		if s.Test != nil {
			v.want(s.Test, Int, "a loop's condition")
		}
		if s.Update != nil {
			v.expr(s.Update)
		}
		v.stmt(s.Body)
	case *ast.BreakStmt, *ast.ContinueStmt:
	case *ast.LabeledStmt:
		v.stmt(s.Body)
	case *ast.ReturnStmt:
		v.ret(s)
	case *ast.SwitchStmt:
		if t := v.expr(s.Disc); !t.Sub(Signed) {
			v.fail(s.Start, "a switch over a %s, not a signed", t)
		}
		seen := map[float64]bool{}
		for _, c := range s.Cases {
			if c.Test != nil {
				t, val, ok := v.literal(c.Test)
				if !ok || !t.Sub(Signed) {
					v.fail(c.Start, "a case that is not a signed literal")
				}
				if seen[val] {
					v.fail(c.Start, "a case twice")
				}
				seen[val] = true
				v.f.Types[c.Test] = Signed
			}
			for _, b := range c.Body {
				v.stmt(b)
			}
		}
	default:
		v.fail(s.Pos(), "a statement asm.js has none of")
	}
}

// ret validates a return of f's: its form gives f's result type, which
// every return of f must have.
func (v *validator) ret(s *ast.ReturnStmt) {
	t := Void
	if s.Arg != nil {
		switch et := v.expr(s.Arg); {
		case et.Sub(Signed):
			t = Signed
		case et == Double:
			t = Double
		case et == Float:
			t = Float
		default:
			v.fail(s.Start, "a return of a %s, not a signed, a double or a float", et)
		}
	}
	f := v.f
	if !f.retSet {
		f.retSet, f.Sig.Ret = true, t
	} else if f.Sig.Ret != t {
		v.fail(s.Start, "a return of a %s where %s returns a %s", t, f.Name, f.Sig.Ret)
	}
}

// want validates e and checks it is of a subtype of t.
func (v *validator) want(e ast.Expr, t Type, what string) Type {
	et := v.expr(e)
	if !et.Sub(t) {
		v.fail(e.Pos(), "%s is a %s, not a %s", what, et, t)
	}
	return et
}

// expr validates e and gives its type, which Types keeps.
func (v *validator) expr(e ast.Expr) Type {
	t := v.exprType(e)
	v.f.Types[e] = t
	return t
}

func (v *validator) exprType(e ast.Expr) Type {
	if t, _, ok := v.literal(e); ok {
		return t
	}
	switch e := e.(type) {
	case *ast.Ident:
		return v.ident(e)
	case *ast.Member:
		view, _ := v.heapAccess(e)
		return view.Load()
	case *ast.Assign:
		return v.assign(e)
	case *ast.Unary:
		return v.unary(e)
	case *ast.Binary:
		return v.binary(e)
	case *ast.Conditional:
		v.want(e.Test, Int, "a condition")
		a, b := v.expr(e.Cons), v.expr(e.Alt)
		switch {
		case a.IsInt() && b.IsInt():
			return Int
		case a == Double && b == Double:
			return Double
		case a == Float && b == Float:
			return Float
		}
		v.fail(e.Start, "a conditional of a %s and a %s", a, b)
	case *ast.Sequence:
		var t Type
		for i, x := range e.Exprs {
			if c, ok := x.(*ast.Call); ok && i < len(e.Exprs)-1 && !v.isMathCall(c) {
				v.call(c, Void)
				v.f.Types[x] = Void
				continue
			}
			t = v.expr(x)
		}
		return t
	case *ast.Call:
		if v.isMathCall(e) {
			return v.mathCall(e)
		}
		v.fail(int(e.Start), "a call whose result is not coerced")
	}
	v.fail(e.Pos(), "an expression asm.js has none of")
	return 0
}

// ident is the type of the variable named id.
func (v *validator) ident(id *ast.Ident) Type {
	if i := v.f.Local(id.Name); i >= 0 {
		return v.f.Locals[i].Type
	}
	g := v.m.Global(id.Name)
	if g == nil {
		v.fail(id.Pos(), "%s is not declared", id.Name)
	}
	switch g.Kind {
	case GlobalVar, GlobalImport, GlobalConst:
		return g.Type
	}
	v.fail(id.Pos(), "%s is not a variable", id.Name)
	return 0
}

// heapAccess validates e, a view's element H[i >> shift], and gives the
// view.
func (v *validator) heapAccess(e *ast.Member) (View, *Global) {
	id, ok := e.Object.(*ast.Ident)
	if !ok || !e.Computed || e.Optional || v.f.Local(id.Name) >= 0 {
		v.fail(int(e.Start), "a member that is not a view's element")
	}
	g := v.m.Global(id.Name)
	if g == nil || g.Kind != GlobalView {
		v.fail(int(e.Start), "%s is not a view of the heap", id.Name)
	}
	if t, val, ok := v.literal(e.Property); ok {
		if !t.IsInt() || val < 0 || val*float64(int(1)<<g.View.Shift()) >= 1<<31 {
			v.fail(int(e.Start), "an index out of the heap's range")
		}
		v.f.Types[e.Property] = t
		return g.View, g
	}
	shift := g.View.Shift()
	if b, ok := e.Property.(*ast.Binary); ok && b.Op == ">>" {
		if t, val, ok := v.literal(b.Right); ok && t.IsInt() && val == float64(shift) {
			v.f.Types[b.Right] = t
			v.want(b.Left, Intish, "an index")
			v.f.Types[e.Property] = Signed
			return g.View, g
		}
	}
	if shift == 0 {
		v.want(e.Property, Int, "a byte view's index")
		return g.View, g
	}
	v.fail(int(e.Start), "an index of %s that is not shifted by %d", id.Name, shift)
	return 0, nil
}

func (v *validator) assign(e *ast.Assign) Type {
	if e.Op != "=" {
		v.fail(int(e.Start), "an assignment by %s", e.Op)
	}
	switch t := e.Target.(type) {
	case *ast.Ident:
		var vt Type
		if i := v.f.Local(t.Name); i >= 0 {
			vt = v.f.Locals[i].Type
		} else if g := v.m.Global(t.Name); g != nil && (g.Kind == GlobalVar || g.Kind == GlobalImport) {
			vt = g.Type
		} else {
			v.fail(t.Pos(), "an assignment to %s, which is no variable", t.Name)
		}
		v.f.Types[t] = vt
		val := v.expr(e.Value)
		switch {
		case vt == Int && val.IsInt(), vt == Double && val == Double, vt == Float && val == Float:
			return val
		}
		v.fail(int(e.Start), "a %s assigned to a %s", val, vt)
	case *ast.Member:
		view, _ := v.heapAccess(t)
		v.f.Types[t] = view.Load()
		val := v.expr(e.Value)
		switch view {
		case Float32:
			if val.Sub(Floatish) || val.Sub(DoubleQ) {
				return val
			}
		case Float64:
			if val.Sub(DoubleQ) || val.Sub(FloatQ) {
				return val
			}
		default:
			if val.Sub(Intish) {
				return val
			}
		}
		v.fail(int(e.Start), "a %s stored to a %s", val, ViewNames[view])
	}
	v.fail(int(e.Start), "an assignment to what is no variable or element")
	return 0
}

func (v *validator) unary(e *ast.Unary) Type {
	switch e.Op {
	case "+":
		if c, ok := e.Operand.(*ast.Call); ok && !v.isMathCall(c) {
			v.call(c, Double)
			v.f.Types[c] = Double
			return Double
		}
		t := v.expr(e.Operand)
		if t.Sub(Signed) || t.Sub(Unsigned) || t.Sub(DoubleQ) || t.Sub(FloatQ) {
			return Double
		}
	case "-":
		t := v.expr(e.Operand)
		switch {
		case t.IsInt():
			return Intish
		case t.Sub(DoubleQ):
			return Double
		case t.Sub(FloatQ):
			return Floatish
		}
	case "~":
		if in, ok := e.Operand.(*ast.Unary); ok && in.Op == "~" {
			// ~~x of a double or a float: its ToInt32.
			if t := v.expr(in.Operand); t.Sub(DoubleQ) || t.Sub(FloatQ) {
				v.f.Types[in] = t
				return Signed
			} else if t.Sub(Intish) {
				v.f.Types[in] = Signed
				return Signed
			}
			break
		}
		if t := v.expr(e.Operand); t.Sub(Intish) {
			return Signed
		}
	case "!":
		if t := v.expr(e.Operand); t.IsInt() {
			return Int
		}
	}
	v.fail(e.Start, "a %s of what it does not take", e.Op)
	return 0
}

func (v *validator) binary(e *ast.Binary) Type {
	switch e.Op {
	case "|":
		if c, ok := e.Left.(*ast.Call); ok && !v.isMathCall(c) && v.isZero(e.Right) {
			v.call(c, Signed)
			v.f.Types[c] = Signed
			v.f.Types[e.Right] = Fixnum
			return Signed
		}
		fallthrough
	case "&", "^", "<<", ">>", ">>>":
		a, b := v.expr(e.Left), v.expr(e.Right)
		if !a.Sub(Intish) || !b.Sub(Intish) {
			v.fail(e.Start, "a %s of a %s and a %s", e.Op, a, b)
		}
		if e.Op == ">>>" {
			return Unsigned
		}
		return Signed
	case "+", "-":
		var leaves []ast.Expr
		var flat func(x ast.Expr)
		flat = func(x ast.Expr) {
			if b, ok := x.(*ast.Binary); ok && (b.Op == "+" || b.Op == "-") {
				flat(b.Left)
				flat(b.Right)
				return
			}
			leaves = append(leaves, x)
		}
		flat(e.Left)
		flat(e.Right)
		allInt := len(leaves) <= 1<<20
		for _, l := range leaves {
			if _, seen := v.f.Types[l]; !seen {
				v.expr(l)
			}
			if !v.f.Types[l].IsInt() {
				allInt = false
			}
		}
		if allInt {
			v.markChain(e)
			return Intish
		}
		a, b := v.expr(e.Left), v.expr(e.Right)
		switch {
		case a.Sub(DoubleQ) && b.Sub(DoubleQ):
			return Double
		case a.Sub(FloatQ) && b.Sub(FloatQ):
			return Floatish
		}
		v.fail(e.Start, "a %s of a %s and a %s", e.Op, a, b)
	case "*":
		a, b := v.expr(e.Left), v.expr(e.Right)
		small := func(x ast.Expr, _ Type) bool {
			lt, val, ok := v.literal(x)
			return ok && lt.IsInt() && math.Abs(val) < 1<<20
		}
		switch {
		case a.IsInt() && small(e.Right, b), b.IsInt() && small(e.Left, a):
			return Intish
		case a.Sub(DoubleQ) && b.Sub(DoubleQ):
			return Double
		case a.Sub(FloatQ) && b.Sub(FloatQ):
			return Floatish
		}
		v.fail(e.Start, "a * of a %s and a %s (Math.imul multiplies ints)", a, b)
	case "/", "%":
		a, b := v.expr(e.Left), v.expr(e.Right)
		switch {
		case a.Sub(Signed) && b.Sub(Signed), a.Sub(Unsigned) && b.Sub(Unsigned):
			return Intish
		case a.Sub(DoubleQ) && b.Sub(DoubleQ):
			return Double
		case e.Op == "/" && a.Sub(FloatQ) && b.Sub(FloatQ):
			return Floatish
		}
		v.fail(e.Start, "a %s of a %s and a %s", e.Op, a, b)
	case "<", "<=", ">", ">=", "==", "!=":
		a, b := v.expr(e.Left), v.expr(e.Right)
		switch {
		case a.Sub(Signed) && b.Sub(Signed), a.Sub(Unsigned) && b.Sub(Unsigned),
			a == Double && b == Double, a == Float && b == Float:
			return Int
		}
		v.fail(e.Start, "a %s of a %s and a %s", e.Op, a, b)
	}
	v.fail(e.Start, "the operator %s, which asm.js has none of", e.Op)
	return 0
}

// markChain gives the inner sums of an additive chain of ints their type,
// intish.
func (v *validator) markChain(e *ast.Binary) {
	v.f.Types[e] = Intish
	for _, x := range []ast.Expr{e.Left, e.Right} {
		if b, ok := x.(*ast.Binary); ok && (b.Op == "+" || b.Op == "-") {
			v.markChain(b)
		}
	}
}

// isMathCall reports whether c calls a stdlib Math function the module
// imported.
func (v *validator) isMathCall(c *ast.Call) bool {
	id, ok := c.Callee.(*ast.Ident)
	if !ok || v.f != nil && v.f.Local(id.Name) >= 0 {
		return false
	}
	g := v.m.Global(id.Name)
	return g != nil && g.Kind == GlobalMath
}

// mathFuncs are the stdlib Math functions asm.js calls.
var mathFuncs = map[string]bool{
	"acos": true, "asin": true, "atan": true, "cos": true, "sin": true, "tan": true, "ceil": true, "floor": true,
	"exp": true, "log": true, "sqrt": true, "abs": true, "atan2": true, "pow": true, "imul": true, "fround": true,
	"min": true, "max": true, "clz32": true,
}

// mathConsts are the stdlib Math constants.
var mathConsts = map[string]float64{
	"E": math.E, "LN10": math.Ln10, "LN2": math.Ln2, "LOG2E": math.Log2E, "LOG10E": math.Log10E,
	"PI": math.Pi, "SQRT1_2": math.Sqrt2 / 2, "SQRT2": math.Sqrt2,
}

// mathCall validates a call of a Math function and gives its result's
// type.
func (v *validator) mathCall(c *ast.Call) Type {
	g := v.m.Global(c.Callee.(*ast.Ident).Name)
	v.f.Calls[c] = &Call{Kind: CallMath, Global: g}
	args := make([]Type, len(c.Args))
	for i, a := range c.Args {
		if g.Math == "fround" {
			if ac, ok := a.(*ast.Call); ok && !v.isMathCall(ac) {
				v.call(ac, Float)
				v.f.Types[ac] = Float
				args[i] = Float
				continue
			}
		}
		args[i] = v.expr(a)
	}
	ret := Void
	arity := func(n int) {
		if len(args) != n {
			v.fail(int(c.Start), "Math.%s of %d arguments", g.Math, len(args))
		}
	}
	switch g.Math {
	case "abs":
		arity(1)
		switch {
		case args[0].Sub(Signed):
			ret = Unsigned
		case args[0].Sub(DoubleQ):
			ret = Double
		case args[0].Sub(FloatQ):
			ret = Floatish
		}
	case "ceil", "floor", "sqrt":
		arity(1)
		switch {
		case args[0].Sub(DoubleQ):
			ret = Double
		case args[0].Sub(FloatQ):
			ret = Floatish
		}
	case "sin", "cos", "tan", "asin", "acos", "atan", "exp", "log":
		arity(1)
		if args[0].Sub(DoubleQ) {
			ret = Double
		}
	case "atan2", "pow":
		arity(2)
		if args[0].Sub(DoubleQ) && args[1].Sub(DoubleQ) {
			ret = Double
		}
	case "imul":
		arity(2)
		if args[0].Sub(Intish) && args[1].Sub(Intish) {
			ret = Signed
		}
	case "clz32":
		arity(1)
		if args[0].Sub(Intish) {
			ret = Fixnum
		}
	case "fround":
		arity(1)
		if a := args[0]; a.Sub(DoubleQ) || a.Sub(Floatish) || a.Sub(Signed) || a.Sub(Unsigned) {
			ret = Float
		}
	case "min", "max":
		if len(args) < 2 {
			v.fail(int(c.Start), "Math.%s of fewer than two arguments", g.Math)
		}
		ret = Signed
		if args[0] == Double {
			ret = Double
		}
		for _, a := range args {
			if ret == Signed && !a.Sub(Signed) || ret == Double && a != Double {
				ret = Void
			}
		}
	}
	if ret == Void {
		v.fail(int(c.Start), "Math.%s of what it does not take", g.Math)
	}
	v.f.Calls[c].Ret = ret
	return ret
}

// call validates c, a call of a function of the module, of a table's or
// of an import, whose result is coerced to ret.
func (v *validator) call(c *ast.Call, ret Type) {
	if c.Optional {
		v.fail(int(c.Start), "an optional call")
	}
	args := func(ok func(Type) Type) []Type {
		var sig []Type
		for _, a := range c.Args {
			t := v.expr(a)
			p := ok(t)
			if p == Void {
				v.fail(a.Pos(), "an argument of a %s", t)
			}
			sig = append(sig, p)
		}
		return sig
	}
	internal := func(t Type) Type {
		switch {
		case t.IsInt():
			return Int
		case t == Double:
			return Double
		case t == Float:
			return Float
		}
		return Void
	}
	switch callee := c.Callee.(type) {
	case *ast.Ident:
		if v.f.Local(callee.Name) >= 0 {
			break
		}
		g := v.m.Global(callee.Name)
		if g == nil {
			break
		}
		switch g.Kind {
		case GlobalFunc:
			v.f.Calls[c] = &Call{Kind: CallFunc, Global: g, Ret: ret}
			v.callSigs[g.Func] = append(v.callSigs[g.Func], sigAt{Sig{args(internal), ret}, int(c.Start)})
			return
		case GlobalFFI:
			if ret == Float {
				v.fail(int(c.Start), "an import's result coerced to a float")
			}
			v.f.Calls[c] = &Call{Kind: CallFFI, Global: g, Ret: ret}
			args(func(t Type) Type {
				if t.Sub(Extern) {
					return Extern
				}
				return Void
			})
			return
		}
	case *ast.Member:
		id, ok := callee.Object.(*ast.Ident)
		if !ok || !callee.Computed || v.f.Local(id.Name) >= 0 {
			break
		}
		g := v.m.Global(id.Name)
		if g == nil || g.Kind != GlobalTable {
			break
		}
		b, ok := callee.Property.(*ast.Binary)
		if !ok || b.Op != "&" {
			v.fail(int(c.Start), "a table call whose index is not masked")
		}
		mt, mask, ok := v.literal(b.Right)
		if !ok || !mt.IsInt() {
			v.fail(int(c.Start), "a table call whose mask is not a literal")
		}
		v.want(b.Left, Intish, "a table call's index")
		v.f.Types[b.Right] = mt
		v.f.Types[b] = Signed
		v.f.Calls[c] = &Call{Kind: CallTable, Global: g, Ret: ret, Index: b.Left}
		v.tableSigs[g.Table] = append(v.tableSigs[g.Table], sigAt{Sig{args(internal), ret}, int(c.Start)})
		v.tableMasks = append(v.tableMasks, tableMask{g.Table, mask, int(c.Start)})
		return
	}
	v.fail(int(c.Start), "a call of what is no function of the module, table or import")
}

type tableMask struct {
	table int
	mask  float64
	pos   int
}
