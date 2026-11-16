package compiler

import (
	"os"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)

// registerForm turns the register backend on: each function the stack
// compiler compiles is also compiled into register code, from its AST and
// the record of its resolution (regcomp), where the backend covers it.
// QJS_REG in the environment turns it on.
var registerForm atomic.Bool

func init() { registerForm.Store(os.Getenv("QJS_REG") != "") }

// SetRegisterForm turns the register backend on or off, for tests.
func SetRegisterForm(on bool) { registerForm.Store(on) }

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
		c.reg.NoteWrite(t, regcomp.Res{})
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
