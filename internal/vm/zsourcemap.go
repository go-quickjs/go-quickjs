package vm

import (
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/sourcemap"
)

// Stack traces through source maps, as Node's --enable-source-maps writes
// them: a frame in a script that names its source map with
// //# sourceMappingURL= is placed in the source it was compiled from -- the
// TypeScript, say -- and called by the name the map gives it there.
//
// A runtime maps nothing until a host gives it a loader, and then reads a
// script's map the first time a stack in it is written, once. A trace a
// script's Error.prepareStackTrace makes gets the frames as they are, as
// Node's does.

// SourceMapLoader reads the source map a script names: script is the name
// it was run under, and url what its //# sourceMappingURL= comment says. It
// returns the map's JSON and its own URL, against which the sources it
// names are resolved, or false for no map.
type SourceMapLoader func(script, url string) (data []byte, base string, ok bool)

// sourceMaps is a runtime's maps, by the script they are of; nil where a
// script's could not be read.
type sourceMaps struct {
	load SourceMapLoader
	maps map[*bytecode.Script]*sourcemap.Map
}

// SetSourceMapLoader has stack traces mapped through the source maps load
// reads; nil maps none.
func (r *Runtime) SetSourceMapLoader(load SourceMapLoader) {
	if load == nil {
		r.sourceMaps = nil
		return
	}
	r.sourceMaps = &sourceMaps{load: load, maps: map[*bytecode.Script]*sourcemap.Map{}}
}

// sourceMapOf is the map of the script a frame's code is in, or nil.
func (r *Runtime) sourceMapOf(fr *stackFrame) *sourcemap.Map {
	if fr.fn == nil || fr.fn.Script == nil || fr.isEval() {
		return nil
	}
	s := fr.fn.Script
	if s.SourceMapURL() == "" {
		return nil
	}
	sm := r.sourceMaps
	if m, seen := sm.maps[s]; seen {
		return m
	}
	var m *sourcemap.Map
	if data, base, ok := sm.load(s.Name, s.SourceMapURL()); ok {
		m, _ = sourcemap.Parse(data, base)
	}
	sm.maps[s] = m
	return m
}

// writeMappedCallSite writes a frame placed through its script's source
// map, as Node's serializeJSStackFrame does, and reports false where it has
// none or the map places it nowhere. next is the frame that called it, or
// nil.
func (r *Runtime) writeMappedCallSite(b *strings.Builder, fr, next *stackFrame) bool {
	m := r.sourceMapOf(fr)
	if m == nil {
		return false
	}
	line, col := fr.position()
	e, ok := m.Find(int(line)-1, int(col)-1)
	if !ok {
		return false
	}
	name := r.originalName(m, fr, next)
	fnName := fr.functionName()
	typeName := ""
	if !fr.construct() && !r.isToplevel(fr) {
		typeName = r.typeName(fr)
		if fnName == "" {
			fnName = r.methodName(fr)
		}
	}
	if fr.construct() {
		b.WriteString("new ")
	}
	if typeName != "" && typeName != "global" {
		b.WriteString(typeName)
		b.WriteByte('.')
	}
	switch {
	case name != "":
		b.WriteString(name)
	case fnName != "":
		b.WriteString(fnName)
	default:
		b.WriteString("<anonymous>")
	}
	b.WriteString(" (")
	b.WriteString(sourcemap.Path(e.Source))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(e.Line + 1))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(e.Column + 1))
	b.WriteByte(')')
	return true
}

// originalName is the name a frame's function had in the source, as Node's
// getOriginalSymbolName finds it: the name the map gives the function's
// start, or else the one it gives the call of it in the same script.
func (r *Runtime) originalName(m *sourcemap.Map, fr, next *stackFrame) string {
	line, col := fr.enclosing()
	if e, ok := m.Find(int(line)-1, int(col)-1); ok && e.Name != "" {
		return e.Name
	}
	if next != nil && next.fn != nil && next.fileName() == fr.fileName() {
		line, col := next.position()
		if e, ok := m.Find(int(line)-1, int(col)-1); ok {
			return e.Name
		}
	}
	return ""
}

// enclosing is where the frame's function begins, as
// CallSite.getEnclosingLineNumber and getEnclosingColumnNumber say: the
// file's start for code placed above it, a wrapper's.
func (fr *stackFrame) enclosing() (line, col int32) {
	if fr.fn == nil || fr.fn.Script == nil {
		return 0, 0
	}
	line, col = fr.fn.Script.Position(fr.fn.Start)
	if line < 1 {
		return 1, 1
	}
	return line, col
}
