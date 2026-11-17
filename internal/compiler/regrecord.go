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
				c.noteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
			} else {
				c.noteWrite(t, regcomp.Res{Kind: regcomp.ResIgnore})
			}
		}
		return
	}
	u := c.fn.Upvalues[idx]
	switch {
	case u.TDZ && !initializing:
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResUpvalCheck, Index: idx})
	case u.FuncSelf && !initializing:
		if c.fn.Strict {
			c.noteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
		} else {
			c.noteWrite(t, regcomp.Res{Kind: regcomp.ResIgnore})
		}
	case !u.Mutable && !initializing:
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
	default:
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResUpval, Index: idx})
	}
}

// regTop records one of top-level code's declarations, op of the name and
// flag, at pos, -1 for none, for the register form to make in the order
// the declaration instantiation makes them (regcomp.Record.Top).
func (c *compiler) regTop(op bytecode.Op, name string, flag uint32, fn *ast.FuncLit, pos int) {
	if c.reg == nil || c.parent != nil {
		return
	}
	c.reg.NoteTop(regcomp.TopOp{Op: op, Name: name, Flag: flag, Pos: int32(pos), Fn: fn})
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

// regWith records the probe, if any -- where probe, withProbe's, is not
// negative, the last emitProbe noted -- that name id has before its static
// access (regcomp.Record.Withs).
func (c *compiler) regWith(id *ast.Ident, probe int) {
	if probe < 0 || c.reg == nil {
		return
	}
	c.reg.NoteWith(id, c.probe)
}
