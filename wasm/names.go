package wasm

import (
	"sync"

	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// compiledCode is a decoded, validated module and its engine code, shared
// by every CompiledModule of the same bytes in runtimes with one cache.
type compiledCode struct {
	// m is the module as the engine runs it: its start function is called
	// by the runtime after instantiation, not by the engine.
	m        *module.Module
	code     *engine.Code
	start    uint32
	hasStart bool

	namesOnce sync.Once
	info      *moduleNames
}

// names returns the module's names, read on first use.
func (c *compiledCode) names() *moduleNames {
	c.namesOnce.Do(func() { c.info = readNames(c.m) })
	return c.info
}

// moduleNames is what a module's name section and bytes give stack traces
// and definitions.
type moduleNames struct {
	url     string
	modName string
	hasName bool
	funcs   map[uint32]string
	locals  map[uint32]map[uint32]string
}

// frameName returns function fn's name in a stack trace: "module.function",
// "module", "function" or "".
func (n *moduleNames) frameName(fn uint32) string {
	f, ok := n.funcs[fn]
	switch {
	case n.hasName && ok:
		return n.modName + "." + f
	case n.hasName:
		return n.modName
	case ok:
		return f
	}
	return ""
}

// readNames reads m's name section. A malformed subsection ends the reading
// with what was read before it.
func readNames(m *module.Module) *moduleNames {
	n := &moduleNames{modName: m.Name, hasName: m.HasName}
	n.url = moduleURL(m.Bytes, m.Name, m.HasName)
	r := m.NameSection
	if r.Len == 0 {
		return n
	}
	d := nameReader{b: m.Bytes[r.Offset:r.End()], ok: true}
	for d.ok && d.pos < len(d.b) {
		id := d.byte()
		size := d.u32()
		if !d.ok || uint64(size) > uint64(len(d.b)-d.pos) {
			break
		}
		sub := nameReader{b: d.b[d.pos : d.pos+int(size)], ok: true}
		d.pos += int(size)
		switch id {
		case 1:
			n.funcs = sub.nameMap()
		case 2:
			n.locals = map[uint32]map[uint32]string{}
			count := sub.u32()
			for i := uint32(0); i < count && sub.ok; i++ {
				fn := sub.u32()
				n.locals[fn] = sub.nameMap()
			}
		}
	}
	return n
}

type nameReader struct {
	b   []byte
	pos int
	ok  bool
}

func (d *nameReader) byte() byte {
	if d.pos >= len(d.b) {
		d.ok = false
		return 0
	}
	c := d.b[d.pos]
	d.pos++
	return c
}

func (d *nameReader) u32() uint32 {
	var v uint32
	for shift := 0; shift < 35; shift += 7 {
		c := d.byte()
		if !d.ok {
			return 0
		}
		v |= uint32(c&0x7f) << shift
		if c < 0x80 {
			return v
		}
	}
	d.ok = false
	return 0
}

func (d *nameReader) name() string {
	n := d.u32()
	if !d.ok || uint64(n) > uint64(len(d.b)-d.pos) {
		d.ok = false
		return ""
	}
	s := string(d.b[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s
}

// nameMap reads a vector of index and name pairs.
func (d *nameReader) nameMap() map[uint32]string {
	count := d.u32()
	m := map[uint32]string{}
	for i := uint32(0); i < count && d.ok; i++ {
		idx := d.u32()
		s := d.name()
		if d.ok {
			m[idx] = s
		}
	}
	return m
}
