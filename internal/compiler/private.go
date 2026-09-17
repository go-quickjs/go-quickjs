package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Private names.
//
// A private name is resolved when the program is compiled, not when the code
// runs: `this.#x` outside any class that declares #x is a syntax error, in the
// same way an unbalanced brace is. That is what makes a private field private
// -- there is no way to ask an object for one you were not written alongside,
// and so no way to discover one by probing.
//
// What the name resolves to is decided when the class is evaluated, not when it
// is compiled. Two classes may spell a private name the same way without
// sharing it, and neither may two evaluations of the same class:
//
//	function make() { return class { #x = 1; static read(o) { return o.#x } } }
//	const A = make(), B = make();
//	A.read(new B())                  // TypeError, not 1
//
// So each class evaluation mints a fresh key per private name and puts it in a
// hidden binding of the enclosing scope. The class body reads that binding the
// way any closure reads a captured variable, which is what makes the two
// evaluations above independent: each has its own binding cell. The names begin
// with a character no identifier may contain, so nothing a script writes can
// collide with one, and nothing a script writes can name one.
//
// The names are collected from the whole class body before any of it is
// compiled, because a method may refer to a field declared below it.

// privateKind says what a private name names, which decides how it is stored.
type privateKind uint8

const (
	privField privateKind = iota
	privMethod
	privGetter
	privSetter
	// privAccessor is a name with both a getter and a setter.
	privAccessor
)

// privateBinding is one private name declared by one class.
type privateBinding struct {
	// name is the spelling without the #.
	name string
	// hidden is the binding that holds the key once the class is evaluated.
	hidden string
	kind   privateKind
	static bool
}

// installed reports whether the name is one each instance carries, which every
// private member except a field and a static is.
func (b privateBinding) installed() bool {
	return !b.static && b.kind != privField
}

// collectPrivateNames lists the private names a class declares, one entry per
// name however many members spell it.
func collectPrivateNames(cls *ast.ClassLit) []privateBinding {
	var out []privateBinding
	add := func(name string, kind privateKind, static bool) {
		for i := range out {
			if out[i].name != name {
				continue
			}
			// `get #g` and `set #g` are two members of one name. Anything else
			// that repeats is an early error, reported elsewhere.
			if (out[i].kind == privGetter && kind == privSetter) ||
				(out[i].kind == privSetter && kind == privGetter) {
				out[i].kind = privAccessor
			}
			return
		}
		out = append(out, privateBinding{name: name, kind: kind, static: static})
	}
	for _, m := range cls.Members {
		pn, ok := m.Key.(*ast.PrivateName)
		if !ok {
			continue
		}
		kind := privMethod
		switch m.Kind {
		case ast.PropGet:
			kind = privGetter
		case ast.PropSet:
			kind = privSetter
		}
		add(pn.Name, kind, m.Static)
	}
	for _, f := range cls.Fields {
		if pn, ok := f.Key.(*ast.PrivateName); ok {
			add(pn.Name, privField, f.Static)
		}
	}
	return out
}

// nameHiddenBindings gives each private name the binding that will hold its key.
func (c *compiler) nameHiddenBindings(bindings []privateBinding) {
	for i := range bindings {
		bindings[i].hidden = fmt.Sprintf("%%p%d", c.hiddenCount)
		c.hiddenCount++
	}
}

// emitPrivateKeys mints a key for each of a class's private names, which is
// what makes this evaluation of the class distinct from every other.
func (c *compiler) emitPrivateKeys(bindings []privateBinding, pos int) {
	for _, b := range bindings {
		slot := c.declare(b.hidden, bindConst, pos)
		c.emit(bytecode.OpPrivateName, c.nameIdx("#"+b.name), 0)
		c.emit(bytecode.OpSetLocal, slot, 0)
		c.markInitialized(b.hidden)
	}
}

// pushPrivateScope makes a class's private names visible to its body.
func (c *compiler) pushPrivateScope(bindings []privateBinding) {
	c.privateScopes = append(c.privateScopes, bindings)
}

func (c *compiler) popPrivateScope() {
	c.privateScopes = c.privateScopes[:len(c.privateScopes)-1]
}

// lookupPrivate finds the innermost declaration of a private name.
//
// A nested function is compiled by a child compiler, so the enclosing classes
// are found by walking up the chain of them.
func (c *compiler) lookupPrivate(name string) (privateBinding, bool) {
	for s := c; s != nil; s = s.parent {
		for i := len(s.privateScopes) - 1; i >= 0; i-- {
			for _, b := range s.privateScopes[i] {
				if b.name == name {
					return b, true
				}
			}
		}
	}
	for _, b := range c.rootOpts().PrivateNames {
		// A direct eval may refer to the private names of the classes its call
		// site is inside, which reach it as ordinary captured bindings.
		if b.Name == name {
			return privateBinding{name: b.Name, hidden: b.Hidden}, true
		}
	}
	return privateBinding{}, false
}

// rootOpts returns the options of the outermost compiler, which is where a
// direct eval's inherited scope is recorded.
func (c *compiler) rootOpts() Options {
	root := c
	for root.parent != nil {
		root = root.parent
	}
	return root.opts
}

// privateVisible reports whether a private name is declared by any enclosing
// class.
func (c *compiler) privateVisible(name string) bool {
	_, ok := c.lookupPrivate(name)
	return ok
}

// checkPrivateName reports a reference to a private name no enclosing class
// declares.
func (c *compiler) checkPrivateName(pn *ast.PrivateName, pos int) {
	if !c.privateVisible(pn.Name) {
		c.errorf(pos, "private name #%s is not declared in an enclosing class", pn.Name)
	}
}

// privateKeyRef encodes where the running code finds a private name's key.
//
// The binding holding it is an ordinary one, so it is reached the way any other
// name is: from a slot of this function, or from a captured one. Packing the
// two cases into a single operand keeps a private access one instruction, as it
// was when the key was a compile-time constant.
func (c *compiler) privateKeyRef(pn *ast.PrivateName, pos int) uint32 {
	b, ok := c.lookupPrivate(pn.Name)
	if !ok {
		c.errorf(pos, "private name #%s is not declared in an enclosing class", pn.Name)
		return 0
	}
	if l, found := c.resolveLocal(b.hidden); found {
		return l.slot << 1
	}
	if idx, found := c.resolveUpvalue(b.hidden); found {
		return idx<<1 | 1
	}
	c.errorf(pos, "private name #%s is not reachable here", pn.Name)
	return 0
}

// privateName resolves a private member reference, checking that it is declared
// and returning the operands every private instruction takes: the name, for the
// error message a failed access produces, and where to find the key.
func (c *compiler) privateName(pn *ast.PrivateName, pos int) (name, key uint32) {
	c.checkPrivateName(pn, pos)
	return c.nameIdx("#" + pn.Name), c.privateKeyRef(pn, pos)
}
