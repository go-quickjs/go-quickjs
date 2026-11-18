package vm

import (
	"maps"
	"math"
	"slices"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/sourcemap"
)

// What a realm or a runtime keeps of code that has gone.
//
// A host that runs new code for each request -- Eval, a Program of its own, a
// direct eval or new Function in a loop -- compiles a new bytecode.Function
// each time, which nothing runs once the request is over, and which must not
// stay for what it left behind. So a table of what code made holds the code
// weakly, and is looked through as it doubles. Its functions are generic, so
// that they are laid out after every function and closure of the package.

// templateSites is what a realm keeps of a function with tagged templates: the
// template object of each of its sites, one for each Parse Node and realm, as
// the standard has it (GetTemplateObject). A site's object is the same for as
// long as its code can run: every closure holds its function, whichever run of
// a program made it, so that fn lives for as long as anything can ask. Once it
// has gone nothing can, and its objects go too.
//
// The table is by the function's address, and fn says whose the entry is, an
// address being a new function's once the last is collected: a site in a loop
// pays a lookup and a read of fn, where a weak.Make for the key at each
// evaluation would ask the function's span for its handle, under its lock.
type templateSites struct {
	fn      weak.Pointer[bytecode.Function]
	objects []*Object
}

// templateSitesAt is the fewest entries a realm's table is looked through at.
const templateSitesAt = 32

// newTemplateSites is re's entry in table, its templates', for fn, which has
// none, of room for its sites' objects, made in the place of a collected
// function's at its address if there is one, the table first looked through
// once it has doubled.
//
//go:noinline
func newTemplateSites[_ any](re *Realm, table map[uintptr]*templateSites, fn *bytecode.Function, objects []*Object) *templateSites {
	if len(table) >= max(int(re.templatesAt), templateSitesAt) {
		maps.DeleteFunc(table, func(_ uintptr, s *templateSites) bool { return s.fn.Value() == nil })
		re.templatesAt = int32(min(2*len(table), math.MaxInt32))
	}
	s := &templateSites{fn: weak.Make(fn), objects: objects}
	table[uintptr(unsafe.Pointer(fn))] = s
	return s
}

// protoUser is p's entry for q's list of the prototypes whose chains go
// through it, which is about to take it. The list is cut down when q changes,
// which a realm's Object.prototype does not: each class
// new code made stayed on it as a dead entry for as long as the realm. So it is
// looked through as it fills, and left room for as many again as live, for
// that to be paid for.
func protoUser[_ any](q, p *Object) weak.Pointer[Object] {
	info := q.shape.side.info
	if n := len(info.users); n >= 32 && n == cap(info.users) {
		info.users = slices.DeleteFunc(info.users, func(w weak.Pointer[Object]) bool { return w.Value() == nil })
		info.users = slices.Grow(info.users, len(info.users)+1)
	}
	return weak.Make(p)
}

// sourceMapSeen is what sm has of script s: its map, nil for one that could
// not be read, and whether it has been looked for.
func sourceMapSeen[_ any](sm *sourceMaps, s *bytecode.Script) (*sourcemap.Map, bool) {
	m, seen := sm.maps[weak.Make(s)]
	return m, seen
}

// keepSourceMap keeps m as the map of script s, the table first looked through
// once it has doubled.
func keepSourceMap[_ any](sm *sourceMaps, s *bytecode.Script, m *sourcemap.Map) {
	if len(sm.maps) >= sm.at {
		maps.DeleteFunc(sm.maps, func(k weak.Pointer[bytecode.Script], _ *sourcemap.Map) bool { return k.Value() == nil })
		sm.at = max(32, 2*len(sm.maps))
	}
	sm.maps[weak.Make(s)] = m
}
