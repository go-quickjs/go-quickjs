// Package realmhook lets the conformance runner make a second realm, which
// test262's $262.createRealm asks for, before the public API has a way to.
//
// The quickjs package fills in NewRealm; the values it hands back are
// quickjs.Values, passed as any so that this package need not import it.
package realmhook

// Realm is a realm of a runtime, made by NewRealm.
type Realm interface {
	// Global returns the realm's global object.
	Global() any
	// Eval runs source text as a script of the realm.
	Eval(src string) (any, error)
	// Set defines a global of the realm; a Go function is made into one of
	// the realm's functions, which throws the realm's errors.
	Set(name string, v any) error
}

// NewRealm makes a realm of the *quickjs.Runtime given.
var NewRealm func(rt any) (Realm, error)
