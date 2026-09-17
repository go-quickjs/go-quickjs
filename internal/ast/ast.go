// Package ast defines the syntax tree produced by the parser and consumed by
// the compiler.
//
// The tree is deliberately close to the ECMAScript grammar rather than
// normalized, because the compiler needs to distinguish forms the grammar
// distinguishes: `a.b` from `a[b]`, `x++` from `++x`, `&&` from `&`. Where the
// grammar is genuinely ambiguous until reduction -- an object literal that
// turns out to be a destructuring pattern -- the parser builds the expression
// form first and converts it with ToPattern.
package ast

// Node is any syntax tree node.
type Node interface {
	// Pos returns the byte offset of the node's first token.
	Pos() int
}

// Expr is an expression node.
type Expr interface {
	Node
	exprNode()
}

// Stmt is a statement node.
type Stmt interface {
	Node
	stmtNode()
}

// Program is the root of a parsed script or module.
type Program struct {
	Body []Stmt
	// Strict reports whether the program body runs in strict mode, either from
	// a "use strict" directive or because it is a module.
	Strict bool
	Module bool
	Start  int
}

func (p *Program) Pos() int { return p.Start }

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

// Ident is an identifier reference or binding.
type Ident struct {
	Name  string
	Start int
}

// PrivateName is a `#field` reference.
type PrivateName struct {
	Name  string
	Start int
}

// NumberLit is a numeric literal.
type NumberLit struct {
	Value float64
	Start int
}

// StringLit is a string literal.
type StringLit struct {
	Value string
	Start int
}

// BigIntLit is a BigInt literal; Raw excludes the trailing 'n'.
type BigIntLit struct {
	Raw   string
	Start int
}

// BoolLit is `true` or `false`.
type BoolLit struct {
	Value bool
	Start int
}

// NullLit is `null`.
type NullLit struct{ Start int }

// RegexpLit is a regular expression literal.
type RegexpLit struct {
	Pattern string
	Flags   string
	Start   int
}

// This is `this`.
type This struct{ Start int }

// Super is `super`, valid only as the base of a call or member expression.
type Super struct{ Start int }

// NewTarget is `new.target`.
type NewTarget struct{ Start int }

// ImportMeta is `import.meta`, valid only inside a module.
//
// It is not a property access, whatever it looks like: there is no binding
// named `import` to read it from, and unlike a property access it may not be
// assigned to.
type ImportMeta struct{ Start int }

// TemplateLit is a template literal. Quasis always has exactly one more
// element than Exprs, so that the parts interleave quasi, expr, quasi, ...
type TemplateLit struct {
	Quasis []TemplateElement
	Exprs  []Expr
	Start  int
}

// TemplateElement is one literal chunk of a template.
type TemplateElement struct {
	// Cooked is the escape-processed text. Invalid escape sequences leave it
	// empty with Valid false, which is legal only in a tagged template.
	Cooked string
	Valid  bool
	Raw    string
}

// TaggedTemplate is `tag`...“.
type TaggedTemplate struct {
	Tag   Expr
	Quasi *TemplateLit
	Start int
}

// ArrayLit is an array literal. A nil element is an elision (a hole).
type ArrayLit struct {
	Elements []Expr
	// TrailingComma records a comma after the last element, which an array
	// literal allows and a binding pattern with a rest element does not.
	TrailingComma bool
	// Paren records that the literal was parenthesized, which stops it from
	// being read as a destructuring pattern: `[a] = b` assigns, `([a]) = b` is
	// a syntax error.
	Paren bool
	Start int
}

// PropKind distinguishes the forms an object literal or class member can take.
type PropKind uint8

const (
	PropInit   PropKind = iota // a: 1, a, method() {}
	PropGet                    // get a() {}
	PropSet                    // set a(v) {}
	PropSpread                 // ...a
)

// Property is a member of an object literal or a class body.
type Property struct {
	Kind PropKind
	// Key is an Ident, StringLit, NumberLit or PrivateName, unless Computed is
	// set, in which case it is an arbitrary expression.
	Key      Expr
	Value    Expr
	Computed bool
	// Shorthand marks `{a}` rather than `{a: a}`; the compiler needs it to
	// report assignment to a non-reference correctly.
	Shorthand bool
	Method    bool
	// Static marks a class member declared with `static`.
	Static bool
	Start  int
}

// ObjectLit is an object literal.
type ObjectLit struct {
	Props []Property
	// TrailingComma records a comma after the last property, which an object
	// literal allows and a binding pattern with a rest element does not.
	TrailingComma bool
	// Paren records that the literal was parenthesized, which stops it from
	// being read as a destructuring pattern.
	Paren bool
	Start int
}

// FuncKind describes the flavour of a function.
type FuncKind uint8

const (
	FuncNormal FuncKind = iota
	FuncArrow
	FuncMethod
	FuncGetter
	FuncSetter
	FuncConstructor
	// FuncDerivedConstructor is the constructor of a class with a heritage
	// clause. It differs in what it may return and in when `this` is bound,
	// both of which the compiler has to know.
	FuncDerivedConstructor
)

// FuncLit is a function or arrow function, as a declaration or an expression.
type FuncLit struct {
	// Name is nil for anonymous functions. The compiler infers a name from the
	// binding context when it can.
	Name      *Ident
	Params    []Expr // Ident, RestElement, AssignPattern, ArrayPattern, ObjectPattern
	Body      []Stmt
	Kind      FuncKind
	Async     bool
	Generator bool
	Strict    bool
	// ExprBody marks a concise arrow body (`x => x + 1`), where Body holds a
	// single synthesized ReturnStmt.
	ExprBody bool
	Start    int
	// End is the offset just past the function's last character, which is what
	// lets Function.prototype.toString return the source as written.
	End int
}

// ClassLit is a class declaration or expression.
type ClassLit struct {
	Name    *Ident
	Extends Expr
	// Members are the class's methods and accessors.
	Members []Property
	// Fields are the instance and static field initializers, in source order.
	Fields []ClassField
	// StaticBlocks are `static { ... }` blocks.
	StaticBlocks [][]Stmt
	Start        int
	// End is the offset just past the class's closing brace.
	End int
}

// FieldInit initializes one instance field, and exists only in the constructor
// the compiler synthesizes for a class.
//
// It is not an assignment: a field is created on the instance rather than
// written through it, so a setter the prototype happens to have for the same
// name is not called, and a private field is added rather than requiring one to
// be there already.
type FieldInit struct {
	Key      Expr
	Value    Expr
	Computed bool
	Start    int
}

// ClassField is a class field definition.
type ClassField struct {
	Key      Expr
	Value    Expr // nil for a field with no initializer
	Computed bool
	Static   bool
	Start    int
}

// Unary is a prefix operator other than ++/--.
type Unary struct {
	Op      string // "-", "+", "!", "~", "typeof", "void", "delete"
	Operand Expr
	Start   int
}

// Update is ++ or --, prefix or postfix.
type Update struct {
	Op      string // "++" or "--"
	Operand Expr
	Prefix  bool
	Start   int
}

// Binary is a binary operator that always evaluates both operands.
type Binary struct {
	Op          string
	Left, Right Expr
	Start       int
}

// Logical is a short-circuiting operator: &&, || or ??.
type Logical struct {
	Op          string
	Left, Right Expr
	// Paren records that the expression was parenthesized in the source. It is
	// kept only because mixing ?? with && or || is a syntax error unless
	// parentheses make the grouping explicit.
	Paren bool
	Start int
}

// Assign is an assignment, possibly compound. For a destructuring assignment
// Op is "=" and Target is a pattern.
type Assign struct {
	Op     string // "=", "+=", "&&=", ...
	Target Expr
	Value  Expr
	// Paren records that the expression was parenthesized in the source, which
	// only matters for deciding what may be assigned to: `(a = b) = c` is a
	// syntax error, while the `a = b` inside `[a = b] = c` is a default value.
	Paren bool
	Start int
}

// Conditional is `test ? cons : alt`.
type Conditional struct {
	Test, Cons, Alt Expr
	Start           int
}

// Call is a function call. Optional marks the `?.()` form.
type Call struct {
	Callee   Expr
	Args     []Expr
	Optional bool
	Start    int
}

// New is `new Callee(Args)`.
type New struct {
	Callee Expr
	Args   []Expr
	Start  int
}

// Member is a property access. Computed distinguishes `a[b]` from `a.b`;
// Optional marks `?.`.
type Member struct {
	Object   Expr
	Property Expr
	Computed bool
	Optional bool
	Start    int
}

// OptionalChain wraps the outermost expression of an optional chain. It marks
// where a short-circuited `?.` unwinds to, so that `a?.b.c` yields undefined
// rather than throwing on `.c`.
type OptionalChain struct {
	Base  Expr
	Start int
}

// Sequence is the comma operator.
type Sequence struct {
	Exprs []Expr
	Start int
}

// Spread is `...arg` in a call or array literal.
type Spread struct {
	Arg   Expr
	Start int
}

// Yield is `yield` or `yield*`.
type Yield struct {
	Arg      Expr // nil for a bare `yield`
	Delegate bool
	Start    int
}

// Await is `await arg`.
type Await struct {
	Arg   Expr
	Start int
}

// ---------------------------------------------------------------------------
// Patterns
// ---------------------------------------------------------------------------

// ArrayPattern is a destructuring array target. A nil element is a hole.
type ArrayPattern struct {
	Elements []Expr
	Rest     Expr // nil if there is no rest element
	Start    int
}

// ObjectPattern is a destructuring object target.
type ObjectPattern struct {
	Props []Property
	Rest  Expr
	Start int
}

// AssignPattern is a destructuring target with a default value.
type AssignPattern struct {
	Target  Expr
	Default Expr
	Start   int
}

// RestElement is `...x` in a parameter list or array pattern.
type RestElement struct {
	Arg   Expr
	Start int
}

// ---------------------------------------------------------------------------
// Statements
// ---------------------------------------------------------------------------

// ExprStmt is an expression evaluated for its side effects. Its value is the
// completion value of the enclosing script, which `eval` returns.
type ExprStmt struct {
	X     Expr
	Start int
}

// BlockStmt is `{ ... }`, which introduces a lexical scope.
type BlockStmt struct {
	Body  []Stmt
	Start int
}

// EmptyStmt is a lone `;`.
type EmptyStmt struct{ Start int }

// DeclKind is the binding form of a variable declaration.
type DeclKind uint8

const (
	DeclVar DeclKind = iota
	DeclLet
	DeclConst
)

func (k DeclKind) String() string {
	switch k {
	case DeclLet:
		return "let"
	case DeclConst:
		return "const"
	}
	return "var"
}

// Declarator is one binding in a variable declaration.
type Declarator struct {
	Target Expr // Ident or a pattern
	Init   Expr // nil if uninitialized
}

// VarDecl is a var, let or const declaration.
type VarDecl struct {
	Kind  DeclKind
	Decls []Declarator
	Start int
}

// FuncDecl is a function declaration statement.
type FuncDecl struct {
	Fn    *FuncLit
	Start int
}

// ClassDecl is a class declaration statement.
type ClassDecl struct {
	Class *ClassLit
	Start int
}

// ReturnStmt is `return`.
type ReturnStmt struct {
	Arg   Expr // nil for a bare return
	Start int
}

// IfStmt is `if`/`else`.
type IfStmt struct {
	Test  Expr
	Cons  Stmt
	Alt   Stmt // nil if there is no else
	Start int
}

// ForStmt is the three-clause `for`.
type ForStmt struct {
	Init   Stmt // VarDecl or ExprStmt, nil if absent
	Test   Expr
	Update Expr
	Body   Stmt
	Start  int
}

// ForInStmt is `for (left in right)`.
type ForInStmt struct {
	// Left is a VarDecl with exactly one declarator, or an assignment target.
	Left  Node
	Right Expr
	Body  Stmt
	Start int
}

// ForOfStmt is `for (left of right)`, optionally `for await`.
type ForOfStmt struct {
	Left  Node
	Right Expr
	Body  Stmt
	Await bool
	Start int
}

// WhileStmt is `while`.
type WhileStmt struct {
	Test  Expr
	Body  Stmt
	Start int
}

// DoWhileStmt is `do ... while`.
type DoWhileStmt struct {
	Body  Stmt
	Test  Expr
	Start int
}

// BreakStmt is `break`, optionally to a label.
type BreakStmt struct {
	Label string
	Start int
}

// ContinueStmt is `continue`, optionally to a label.
type ContinueStmt struct {
	Label string
	Start int
}

// ThrowStmt is `throw`.
type ThrowStmt struct {
	Arg   Expr
	Start int
}

// CatchClause is the `catch` of a try statement.
type CatchClause struct {
	// Param is nil for the optional-binding form, `catch { }`.
	Param Expr
	Body  []Stmt
	Start int
}

// TryStmt is `try`/`catch`/`finally`.
type TryStmt struct {
	Block   []Stmt
	Catch   *CatchClause
	Finally []Stmt // nil if there is no finally block
	Start   int
}

// SwitchCase is one `case` or `default` of a switch.
type SwitchCase struct {
	// Test is nil for the default clause.
	Test  Expr
	Body  []Stmt
	Start int
}

// SwitchStmt is `switch`.
type SwitchStmt struct {
	Disc  Expr
	Cases []SwitchCase
	Start int
}

// LabeledStmt is `label: stmt`.
type LabeledStmt struct {
	Label string
	Body  Stmt
	Start int
}

// DebuggerStmt is `debugger`.
type DebuggerStmt struct{ Start int }

// WithStmt is `with (obj) body`. It is legal only in sloppy mode, and the
// parser rejects it under strict mode before the compiler ever sees it.
type WithStmt struct {
	Object Expr
	Body   Stmt
	Start  int
}

// ---------------------------------------------------------------------------
// Interface conformance
// ---------------------------------------------------------------------------

func (n *Ident) Pos() int          { return n.Start }
func (n *PrivateName) Pos() int    { return n.Start }
func (n *NumberLit) Pos() int      { return n.Start }
func (n *StringLit) Pos() int      { return n.Start }
func (n *BigIntLit) Pos() int      { return n.Start }
func (n *BoolLit) Pos() int        { return n.Start }
func (n *NullLit) Pos() int        { return n.Start }
func (n *RegexpLit) Pos() int      { return n.Start }
func (n *This) Pos() int           { return n.Start }
func (n *ImportMeta) Pos() int     { return n.Start }
func (n *Super) Pos() int          { return n.Start }
func (n *NewTarget) Pos() int      { return n.Start }
func (n *TemplateLit) Pos() int    { return n.Start }
func (n *TaggedTemplate) Pos() int { return n.Start }
func (n *ArrayLit) Pos() int       { return n.Start }
func (n *ObjectLit) Pos() int      { return n.Start }
func (n *FuncLit) Pos() int        { return n.Start }
func (n *ClassLit) Pos() int       { return n.Start }
func (n *Unary) Pos() int          { return n.Start }
func (n *Update) Pos() int         { return n.Start }
func (n *Binary) Pos() int         { return n.Start }
func (n *Logical) Pos() int        { return n.Start }
func (n *Assign) Pos() int         { return n.Start }
func (n *Conditional) Pos() int    { return n.Start }
func (n *Call) Pos() int           { return n.Start }
func (n *New) Pos() int            { return n.Start }
func (n *Member) Pos() int         { return n.Start }
func (n *OptionalChain) Pos() int  { return n.Start }
func (n *Sequence) Pos() int       { return n.Start }
func (n *Spread) Pos() int         { return n.Start }
func (n *Yield) Pos() int          { return n.Start }
func (n *Await) Pos() int          { return n.Start }
func (n *ArrayPattern) Pos() int   { return n.Start }
func (n *ObjectPattern) Pos() int  { return n.Start }
func (n *AssignPattern) Pos() int  { return n.Start }
func (n *RestElement) Pos() int    { return n.Start }

func (*Ident) exprNode()          {}
func (*PrivateName) exprNode()    {}
func (*NumberLit) exprNode()      {}
func (*StringLit) exprNode()      {}
func (*BigIntLit) exprNode()      {}
func (*BoolLit) exprNode()        {}
func (*NullLit) exprNode()        {}
func (*RegexpLit) exprNode()      {}
func (*This) exprNode()           {}
func (*ImportMeta) exprNode()     {}
func (*Super) exprNode()          {}
func (*NewTarget) exprNode()      {}
func (*TemplateLit) exprNode()    {}
func (*TaggedTemplate) exprNode() {}
func (*ArrayLit) exprNode()       {}
func (*ObjectLit) exprNode()      {}
func (*FuncLit) exprNode()        {}
func (*ClassLit) exprNode()       {}
func (*Unary) exprNode()          {}
func (*Update) exprNode()         {}
func (*Binary) exprNode()         {}
func (*Logical) exprNode()        {}
func (*Assign) exprNode()         {}
func (*Conditional) exprNode()    {}
func (*Call) exprNode()           {}
func (*New) exprNode()            {}
func (*Member) exprNode()         {}
func (*OptionalChain) exprNode()  {}
func (*Sequence) exprNode()       {}
func (*Spread) exprNode()         {}
func (*Yield) exprNode()          {}
func (*Await) exprNode()          {}
func (*ArrayPattern) exprNode()   {}
func (*ObjectPattern) exprNode()  {}
func (*AssignPattern) exprNode()  {}
func (*RestElement) exprNode()    {}

func (n *ExprStmt) Pos() int     { return n.Start }
func (n *BlockStmt) Pos() int    { return n.Start }
func (n *EmptyStmt) Pos() int    { return n.Start }
func (n *VarDecl) Pos() int      { return n.Start }
func (n *FuncDecl) Pos() int     { return n.Start }
func (n *ClassDecl) Pos() int    { return n.Start }
func (n *ReturnStmt) Pos() int   { return n.Start }
func (n *IfStmt) Pos() int       { return n.Start }
func (n *ForStmt) Pos() int      { return n.Start }
func (n *ForInStmt) Pos() int    { return n.Start }
func (n *ForOfStmt) Pos() int    { return n.Start }
func (n *WhileStmt) Pos() int    { return n.Start }
func (n *DoWhileStmt) Pos() int  { return n.Start }
func (n *BreakStmt) Pos() int    { return n.Start }
func (n *ContinueStmt) Pos() int { return n.Start }
func (n *ThrowStmt) Pos() int    { return n.Start }
func (n *TryStmt) Pos() int      { return n.Start }
func (n *SwitchStmt) Pos() int   { return n.Start }
func (n *LabeledStmt) Pos() int  { return n.Start }
func (n *DebuggerStmt) Pos() int { return n.Start }
func (n *WithStmt) Pos() int     { return n.Start }
func (n *FieldInit) Pos() int    { return n.Start }

func (*ExprStmt) stmtNode()     {}
func (*BlockStmt) stmtNode()    {}
func (*EmptyStmt) stmtNode()    {}
func (*VarDecl) stmtNode()      {}
func (*FuncDecl) stmtNode()     {}
func (*ClassDecl) stmtNode()    {}
func (*ReturnStmt) stmtNode()   {}
func (*IfStmt) stmtNode()       {}
func (*ForStmt) stmtNode()      {}
func (*ForInStmt) stmtNode()    {}
func (*ForOfStmt) stmtNode()    {}
func (*WhileStmt) stmtNode()    {}
func (*DoWhileStmt) stmtNode()  {}
func (*BreakStmt) stmtNode()    {}
func (*ContinueStmt) stmtNode() {}
func (*ThrowStmt) stmtNode()    {}
func (*TryStmt) stmtNode()      {}
func (*SwitchStmt) stmtNode()   {}
func (*LabeledStmt) stmtNode()  {}
func (*DebuggerStmt) stmtNode() {}
func (*WithStmt) stmtNode()     {}
func (*FieldInit) stmtNode()    {}

// ---------------------------------------------------------------------------
// Modules
// ---------------------------------------------------------------------------

// ImportSpecifier is one binding introduced by an import declaration.
type ImportSpecifier struct {
	// Imported is the name in the source module. It is empty for a default
	// import and for a namespace import.
	Imported string
	// Local is the name bound in this module.
	Local string
	// Kind distinguishes the three forms, which resolve differently.
	Kind  ImportKind
	Start int
}

// ImportKind classifies an import specifier.
type ImportKind uint8

const (
	// ImportNamed is `import {a as b} from "m"`.
	ImportNamed ImportKind = iota
	// ImportDefault is `import a from "m"`.
	ImportDefault
	// ImportNamespace is `import * as a from "m"`.
	ImportNamespace
)

// ImportDecl is an import declaration.
//
// A declaration with no specifiers is a side-effect import: `import "m"`.
type ImportDecl struct {
	Specifiers []ImportSpecifier
	Source     string
	Start      int
}

// ExportSpecifier is one name an export declaration exposes.
type ExportSpecifier struct {
	// Local is the name inside this module, or inside Source when the
	// declaration re-exports.
	Local string
	// Exported is the name other modules see.
	Exported string
	Start    int
}

// ExportDecl is an export declaration.
//
// The three shapes are distinguished by which fields are set: Decl for
// `export const x = 1`, Specifiers for `export {a, b}`, and Source for a
// re-export.
type ExportDecl struct {
	// Decl is the declaration being exported, if the export wraps one.
	Decl Stmt
	// Specifiers lists the names exported by an `export {}` clause.
	Specifiers []ExportSpecifier
	// Source names the module a re-export draws from.
	Source string
	// Default marks `export default`.
	Default bool
	// DefaultExpr holds the expression of `export default expr`.
	DefaultExpr Expr
	// All marks `export * from "m"`, with Alias set for the namespace form.
	All   bool
	Alias string
	Start int
}

func (n *ImportDecl) Pos() int { return n.Start }
func (n *ExportDecl) Pos() int { return n.Start }

func (*ImportDecl) stmtNode() {}
func (*ExportDecl) stmtNode() {}
