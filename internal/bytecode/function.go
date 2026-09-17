package bytecode

import (
	"fmt"
	"strings"
)

// Instr is a single instruction. Op selects the operation; A and B carry
// operands whose meaning depends on the opcode, documented alongside each in
// op.go.
type Instr struct {
	Op Op
	A  uint32
	B  uint32
}

// ConstKind tags the entries of a function's constant pool.
//
// The pool deliberately holds only representations the compiler can produce
// without a runtime: the virtual machine materializes real values from these
// the first time a function runs. Keeping it that way lets the compiler stay
// independent of the value representation.
type ConstKind uint8

const (
	ConstNumber ConstKind = iota
	ConstString
	ConstBigInt
	ConstFunction
	ConstRegExp
)

// Constant is one entry of the constant pool.
type Constant struct {
	Kind ConstKind
	Num  float64
	Str  string
	// Fn is set for ConstFunction: a nested function's compiled template,
	// which OpClosure turns into a closure.
	Fn *Function
	// Flags carries a regular expression's flags for ConstRegExp.
	Flags string
}

// UpvalueDesc says where a closure's captured variable comes from when the
// closure is created.
type UpvalueDesc struct {
	// FromParent is true when the variable is a local of the immediately
	// enclosing function, and false when it is one of that function's own
	// upvalues, which must be forwarded.
	FromParent bool
	// Index is the local slot or upvalue slot in the enclosing function.
	Index uint32
	Name  string
	// Mutable is false for a const binding, so the VM can reject assignment.
	Mutable bool
	// WithDepth is how many `with` bodies enclosed the binding's declaration,
	// which decides how many of them a reference to it has to be probed
	// against: a binding declared inside one is not shadowed by it.
	WithDepth int
	// TDZ marks a let or const binding, which must be checked for use before
	// initialization.
	TDZ bool
	// FuncSelf marks the name a function expression gave itself, which is
	// immutable but whose assignment outside strict mode is discarded rather
	// than refused.
	FuncSelf bool
}

// EvalScope is what a direct `eval` call site can see.
//
// Evaluated code shares its caller's bindings, `this`, `new.target` and
// `super`, which is the whole difference between a direct eval and an indirect
// one. The compiler cannot know what the code will refer to, so it records
// everything in scope and lets the evaluator take what it needs.
type EvalScope struct {
	Bindings []EvalBinding
	// WithDepth is how many `with` bodies the call site is inside, counting
	// the one an enclosing eval's own variables live in. The evaluated code is
	// inside them too, so a name it mentions has to be looked for in their
	// objects before the binding it would otherwise mean.
	WithDepth int
	// Strict records whether the call site is in strict code, which the
	// evaluated code inherits.
	Strict bool
	// The contexts the evaluated code may use, which follow the call site's.
	AllowSuperProp bool
	AllowSuperCall bool
	AllowNewTarget bool
	// VarScopeIsGlobal says whether the call site's own vars are properties of
	// the global object. What the evaluated code declares goes wherever the
	// caller's own vars do, so this is what decides between the global object
	// and the calling function.
	VarScopeIsGlobal bool
	// InFieldInit marks a call site inside a class field initializer, which is
	// a function of its own: `arguments` there is a syntax error, and
	// new.target is undefined.
	InFieldInit bool
	InClassBody bool
	// PrivateNames are the private names of the enclosing classes, which
	// evaluated code may refer to.
	PrivateNames []EvalPrivateName
	// ArgumentNames is set when the call site is a parameter default, and
	// holds what the parameter scope binds: the parameter names, and the
	// arguments object. A var the evaluated code declares may not collide with
	// one of them -- the parameter scope lies between the evaluated code and
	// the function's variable scope, and a var cannot be created in a scope it
	// would be shadowed by.
	ArgumentNames []string
}

// EvalPrivateName is one private name a direct eval's code can refer to,
// together with the hidden binding that holds its key. The binding reaches the
// evaluated code the way any other captured one does.
type EvalPrivateName struct {
	Name   string
	Hidden string
}

// EvalBinding is one name a direct eval's code can reach.
type EvalBinding struct {
	Name string
	// FromLocal is true when Index is a local slot of the calling function, and
	// false when it is one of that function's upvalues.
	FromLocal bool
	Index     uint32
	Mutable   bool
	TDZ       bool
	// FuncSelf marks the name a function expression gave itself.
	FuncSelf bool
	// VarScoped marks a binding of the calling function's own variable scope:
	// a var, a parameter, or a function declared at its top level. A var the
	// evaluated code declares is created only where there is no such binding
	// already.
	VarScoped bool
	// Lexical marks a let, const or class binding, which a var declared by the
	// evaluated code may not hoist over: one of the calling function's own
	// refuses the declaration outright.
	Lexical bool
	// WithDepth is how many `with` bodies enclosed the binding's declaration,
	// which says how many of the ones in scope at the call site can shadow it.
	WithDepth int
}

// TemplateStrings is the text of one tagged template site.
//
// Raw is what was written and Cooked is what the escapes mean, which differ
// exactly where a tag would want to see the difference -- String.raw is the
// whole reason both are kept. Cooked is nil where an escape is malformed, which
// is an error in an ordinary template and merely undefined in a tagged one.
type TemplateStrings struct {
	Cooked []string
	// CookedValid marks the entries of Cooked that mean anything.
	CookedValid []bool
	Raw         []string
}

// LocalDesc describes a local variable slot.
type LocalDesc struct {
	Name    string
	Mutable bool
	TDZ     bool
	// Captured marks a local that some inner closure captures, which forces
	// the VM to box it rather than keep it in the flat frame slice.
	Captured bool
}

// FuncKind distinguishes the callable forms, which differ in how `this`,
// `new` and the prototype are handled.
type FuncKind uint8

const (
	KindNormal FuncKind = iota
	KindArrow
	KindMethod
	KindGetter
	KindSetter
	KindConstructor
	KindDerivedConstructor
	KindClassFieldInit
	KindStaticBlock
)

// SourceLoc maps a byte offset in the instruction stream to a source position,
// for stack traces.
type SourceLoc struct {
	// PC is the index of the first instruction covered by this entry.
	PC   uint32
	Line int32
}

// Function is a compiled function template.
//
// A template is immutable once compiled and is shared by every closure created
// from it; the per-call state lives in the virtual machine's frame.
type Function struct {
	// ParamEnd is the pc just past the parameter prologue, which is where a
	// generator's body proper begins. A generator binds its parameters when it
	// is called and resumes from here, so that a destructuring error in a
	// parameter throws at the call rather than at the first next().
	ParamEnd uint32

	Name string
	// ParamCount is how many arguments the interpreter copies positionally into
	// the frame's slots. It excludes a rest parameter, which is filled from the
	// argument list instead.
	ParamCount int
	// Length is what Function.prototype.length reports: the number of
	// parameters before the first one with a default or a rest element. It says
	// how many arguments the function expects rather than how many it has room
	// for, which is why it is not ParamCount.
	Length int
	// LocalCount is the size of the frame's local slice, covering parameters,
	// declared variables and compiler temporaries.
	LocalCount int
	// MaxStack is the deepest the operand stack gets, so a frame can be
	// allocated once at the right size.
	MaxStack int

	Code      []Instr
	Constants []Constant
	// Names holds the property and variable names referenced by instructions
	// that take a name operand, kept separate from Constants so that the VM can
	// pre-intern them all into atoms at load time.
	Names    []string
	Locals   []LocalDesc
	Upvalues []UpvalueDesc
	// EvalScopes holds one entry per direct `eval` call site in this function,
	// describing what the evaluated code can see.
	EvalScopes []EvalScope

	// Templates holds one entry per tagged template site in this function. The
	// object a site produces is built once and reused, because the tag is
	// entitled to hang state off it and to compare it against a later call's.
	Templates []TemplateStrings

	Kind      FuncKind
	Strict    bool
	Async     bool
	Generator bool
	// HasRest, HasSimpleParams and UsesArguments let the VM skip work that most
	// functions do not need.
	HasRest         bool
	HasSimpleParams bool
	UsesArguments   bool
	// MappedArguments marks a function whose arguments object aliases its
	// parameters, which sloppy mode with a plain parameter list asks for.
	MappedArguments bool
	// ParamsAreLexical marks a parameter list whose bindings are initialized
	// one at a time, in order, so that a default may not read a parameter that
	// comes after it. A missing argument leaves its slot in the dead zone for
	// such a function, rather than undefined, and the prologue clears the
	// marker as it reaches each parameter.
	//
	// Only a list with a default, a pattern or a rest element needs it: with
	// plain parameters there is nothing that could run early enough to notice.
	ParamsAreLexical bool
	// UsesThis records whether the body can observe its `this`, which lets a
	// sloppy-mode call skip substituting the global object when nothing would
	// see the difference.
	UsesThis bool
	// IsExprBody marks a concise arrow body, which affects nothing at runtime
	// but is useful when printing a function's source.
	IsExprBody bool
	// IsModule marks module code, whose top-level `this` is undefined rather
	// than the global object.
	IsModule bool
	// HasDirectEval marks a sloppy function whose body contains a direct eval
	// that could declare a var in it. The frame gets somewhere to put one: a
	// var the evaluated code declares belongs to the function that called it,
	// and there is no slot for a name nobody wrote down.
	HasDirectEval bool

	// Source is the file or origin name used in stack traces.
	Source string
	Lines  []SourceLoc
	// Text is the original source text of the function, which
	// Function.prototype.toString returns.
	Text string
}

// LineAt returns the source line for a program counter, or 0 if unknown.
func (f *Function) LineAt(pc uint32) int32 {
	// Entries are sorted by PC, and functions are small, so a linear scan from
	// the end beats a binary search in practice.
	for i := len(f.Lines) - 1; i >= 0; i-- {
		if f.Lines[i].PC <= pc {
			return f.Lines[i].Line
		}
	}
	return 0
}

// String renders a function's name for diagnostics.
func (f *Function) String() string {
	if f.Name == "" {
		return "<anonymous>"
	}
	return f.Name
}

// Disassemble renders a function's code in a readable form. It exists for
// debugging the compiler and is not used at runtime.
func (f *Function) Disassemble() string {
	var sb strings.Builder
	f.disassembleTo(&sb, "")
	return sb.String()
}

func (f *Function) disassembleTo(sb *strings.Builder, indent string) {
	fmt.Fprintf(sb, "%sfunction %s (params=%d locals=%d stack=%d)\n",
		indent, f, f.ParamCount, f.LocalCount, f.MaxStack)

	for pc, in := range f.Code {
		fmt.Fprintf(sb, "%s  %4d  %-20s", indent, pc, in.Op)
		switch in.Op {
		case OpPushConst, OpClosure, OpNewRegExp:
			fmt.Fprintf(sb, " %d", in.A)
			if int(in.A) < len(f.Constants) {
				fmt.Fprintf(sb, " ; %s", f.Constants[in.A])
			}
		case OpGetProp, OpSetProp, OpGetPropThis, OpDefineField,
			OpGetGlobal, OpGetGlobalOpt, OpSetGlobal, OpDefineGlobalVar,
			OpDefineGlobalFunc, OpSetName, OpGetSuperProp, OpSetSuperProp,
			OpCheckGlobalRef, OpAssertResolved,
			OpGetPrivate, OpSetPrivate, OpDefinePrivate, OpPrivateIn,
			OpDefinePrivateMethod,
			OpGetPrivateMethod, OpDefineGetter, OpDefineSetter:
			fmt.Fprintf(sb, " %d", in.A)
			if int(in.A) < len(f.Names) {
				fmt.Fprintf(sb, " ; %q", f.Names[in.A])
			}
		case OpGetLocal, OpSetLocal, OpPutLocal, OpGetLocalCheck,
			OpSetLocalCheck, OpInitLocal, OpCloseUpvalues:
			fmt.Fprintf(sb, " %d", in.A)
			if int(in.A) < len(f.Locals) {
				fmt.Fprintf(sb, " ; %q", f.Locals[in.A].Name)
			}
		case OpGetUpvalue, OpSetUpvalue, OpGetUpvalueCheck, OpSetUpvalueCheck,
			OpInitUpvalue:
			fmt.Fprintf(sb, " %d", in.A)
			if int(in.A) < len(f.Upvalues) {
				fmt.Fprintf(sb, " ; %q", f.Upvalues[in.A].Name)
			}
		case OpJump, OpJumpIfFalse, OpJumpIfTrue, OpJumpIfFalseKeep,
			OpJumpIfTrueKeep, OpJumpIfNullish, OpJumpIfNotNullish,
			OpPushCatch, OpPushFinally, OpIterNextOrJump:
			fmt.Fprintf(sb, " -> %d", in.A)
		default:
			if in.A != 0 || in.B != 0 {
				fmt.Fprintf(sb, " %d", in.A)
				if in.B != 0 {
					fmt.Fprintf(sb, " %d", in.B)
				}
			}
		}
		sb.WriteByte('\n')
	}

	// Nested functions follow their parent, indented.
	for _, c := range f.Constants {
		if c.Kind == ConstFunction && c.Fn != nil {
			sb.WriteByte('\n')
			c.Fn.disassembleTo(sb, indent+"    ")
		}
	}
}

func (c Constant) String() string {
	switch c.Kind {
	case ConstNumber:
		return fmt.Sprintf("%v", c.Num)
	case ConstString:
		return fmt.Sprintf("%q", c.Str)
	case ConstBigInt:
		return c.Str + "n"
	case ConstFunction:
		return "function " + c.Fn.String()
	case ConstRegExp:
		return "/" + c.Str + "/" + c.Flags
	}
	return "?"
}
