package compiler

import "github.com/go-quickjs/go-quickjs/internal/ast"

// Private names.
//
// A private name is resolved when the program is compiled, not when the code
// runs: `this.#x` outside any class that declares #x is a syntax error, in the
// same way an unbalanced brace is. That is what makes a private field private
// -- there is no way to ask an object for one you were not written alongside,
// and so no way to discover one by probing.
//
// The names are collected from the whole class body before any of it is
// compiled, because a method may refer to a field declared below it.

// pushPrivateScope makes a class's private names visible to its body.
func (c *compiler) pushPrivateScope(cls *ast.ClassLit) {
	var names []string
	for _, m := range cls.Members {
		if pn, ok := m.Key.(*ast.PrivateName); ok {
			names = append(names, pn.Name)
		}
	}
	for _, f := range cls.Fields {
		if pn, ok := f.Key.(*ast.PrivateName); ok {
			names = append(names, pn.Name)
		}
	}
	c.privateScopes = append(c.privateScopes, names)
}

func (c *compiler) popPrivateScope() {
	c.privateScopes = c.privateScopes[:len(c.privateScopes)-1]
}

// privateVisible reports whether a private name is declared by any enclosing
// class.
//
// A nested function is compiled by a child compiler, so the enclosing classes
// are found by walking up the chain of them.
func (c *compiler) privateVisible(name string) bool {
	for s := c; s != nil; s = s.parent {
		for _, scope := range s.privateScopes {
			for _, n := range scope {
				if n == name {
					return true
				}
			}
		}
	}
	return false
}

// checkPrivateName reports a reference to a private name no enclosing class
// declares.
func (c *compiler) checkPrivateName(pn *ast.PrivateName, pos int) {
	if !c.privateVisible(pn.Name) {
		c.errorf(pos, "private name #%s is not declared in an enclosing class", pn.Name)
	}
}
