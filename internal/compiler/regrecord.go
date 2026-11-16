package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)

// regNoteUpvalueWrite records a write of t that resolves to an upvalue, as
// assignToIdentStatic compiles it.
func (c *compiler) regNoteUpvalueWrite(t *ast.Ident, initializing bool) {
	idx, ok := c.resolveUpvalue(t.Name)
	if !ok {
		if !initializing && t.Name == c.selfName {
			if c.fn.Strict {
				c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
			} else {
				c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResIgnore})
			}
		}
		return
	}
	u := c.fn.Upvalues[idx]
	switch {
	case u.TDZ && !initializing:
		c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResUpvalCheck, Index: idx})
	case u.FuncSelf && !initializing:
		if c.fn.Strict {
			c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
		} else {
			c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResIgnore})
		}
	case !u.Mutable && !initializing:
		c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
	default:
		c.reg.NoteWrite(t, regcomp.Res{Kind: regcomp.ResUpval, Index: idx})
	}
}

// regTop records one of top-level code's declarations, op of the name with
// index name and flag, as the stack form has just emitted it, for the
// register form to make in the same order (regcomp.Record.Top).
func (c *compiler) regTop(op bytecode.Op, name, flag uint32, fn *ast.FuncLit) {
	if c.reg == nil || c.parent != nil {
		return
	}
	c.reg.NoteTop(regcomp.TopOp{Op: op, Name: c.fn.Names[name], Flag: flag, Pos: c.lastPos, Fn: fn})
}

// regTopCode gives fn, top-level code of statements body whose resolution
// rec recorded, its register code, where rec is not nil.
func regTopCode(body []ast.Stmt, fn *bytecode.Function, rec *regcomp.Record) {
	switch {
	case rec == nil:
	case rec.Bail:
		regcomp.Refuse(fn, "a name the record resolves two ways")
		fn.Reg = bytecode.RegNone()
	default:
		if fn.Reg = bytecode.RegReady(regcomp.CompileTop(body, fn, rec)); fn.Reg == nil {
			fn.Reg = bytecode.RegNone()
		}
	}
}

// regWith records the probe at pc, if any, that the stack form compiled
// for name id before its static access (regcomp.Record.Withs).
func (c *compiler) regWith(id *ast.Ident, pc int) {
	if pc < 0 || c.reg == nil {
		return
	}
	in := c.fn.Code[pc]
	c.reg.NoteWith(id, regcomp.WithProbe{Op: in.Op, Limit: uint16(in.A >> bytecode.WithLimitShift)})
}
