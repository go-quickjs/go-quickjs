// Package names reads what stack traces and definitions call a module and
// its functions: the module's URL, from the hash of its bytes, and its name
// section.
package names

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"runtime"
	"sync"
	"weak"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// cache holds each module's names, read on first use, keyed weakly so that
// a module's entry goes with it.
var cache sync.Map // weak.Pointer[module.Module] -> *Names

// Of returns m's names, read the first time they are asked for.
func Of(m *module.Module) *Names {
	k := weak.Make(m)
	if v, ok := cache.Load(k); ok {
		return v.(*Names)
	}
	v, loaded := cache.LoadOrStore(k, read(m))
	if !loaded {
		runtime.AddCleanup(m, func(k weak.Pointer[module.Module]) { cache.Delete(k) }, k)
	}
	return v.(*Names)
}

// Names is what a module's name section and bytes give stack traces and
// definitions.
type Names struct {
	// URL is what stack traces call the module: wasm://wasm/, its name and a
	// dash if it has one, and the hash of its bytes.
	URL string
	// Module is the module's name, if HasModule.
	Module    string
	HasModule bool
	// Funcs names functions, and Locals the locals of each, by index.
	Funcs  map[uint32]string
	Locals map[uint32]map[uint32]string
}

// FrameName returns function fn's name in a stack trace: "module.function",
// "module", "function" or "".
func (n *Names) FrameName(fn uint32) string {
	f, ok := n.Funcs[fn]
	switch {
	case n.HasModule && ok:
		return n.Module + "." + f
	case n.HasModule:
		return n.Module
	case ok:
		return f
	}
	return ""
}

// read reads m's name section. A malformed subsection ends the reading
// with what was read before it.
func read(m *module.Module) *Names {
	n := &Names{Module: m.Name, HasModule: m.HasName}
	n.URL = moduleURL(m.Bytes, m.Name, m.HasName)
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
			n.Funcs = sub.nameMap()
		case 2:
			n.Locals = map[uint32]map[uint32]string{}
			count := sub.u32()
			for i := uint32(0); i < count && sub.ok; i++ {
				fn := sub.u32()
				n.Locals[fn] = sub.nameMap()
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

// moduleURL returns the URL of a module of bytes b, named name if hasName.
func moduleURL(b []byte, name string, hasName bool) string {
	h := fmt.Sprintf("%08x", wireBytesHash(b))
	if hasName {
		return "wasm://wasm/" + name + "-" + h
	}
	return "wasm://wasm/" + h
}

// wireBytesHash is the hash a module's bytes are named by in stack traces:
// the rapidhash of the bytes, with seed zero and the default secret, cut to
// 30 bits, as a string hash field. A module over 16,383 bytes is named by its
// length.
func wireBytesHash(b []byte) uint32 {
	const maxHashCalcLength = 16383
	var h uint32
	if len(b) > maxHashCalcLength {
		h = uint32(len(b))
	} else {
		h = uint32(rapidhash(b, 0)) & (1<<30 - 1)
		if h == 0 {
			h = 27
		}
	}
	return h<<2 | 2
}

var rapidSecret = [3]uint64{0x2d358dccaa6c78a5, 0x8bb84b93962eacc9, 0x4b33a62ed433d4a3}

func rapidMix(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return hi ^ lo
}

// rapidhash hashes p: rapidhash by Wang Yi, public domain.
func rapidhash(p []byte, seed uint64) uint64 {
	r64 := func(p []byte) uint64 { return binary.LittleEndian.Uint64(p) }
	r32 := func(p []byte) uint64 { return uint64(binary.LittleEndian.Uint32(p)) }
	n := uint64(len(p))
	s := &rapidSecret
	seed ^= rapidMix(seed^s[0], s[1]) ^ n
	var a, b uint64
	switch {
	case n >= 4 && n <= 16:
		last := n - 4
		a = r32(p)<<32 | r32(p[last:])
		d := (n & 24) >> (n >> 3)
		b = r32(p[d:])<<32 | r32(p[last-d:])
	case n > 0 && n < 4:
		a = uint64(p[0])<<56 | uint64(p[n>>1])<<32 | uint64(p[n-1])
	case n > 16:
		q, i := p, n
		if i > 48 {
			s1, s2 := seed, seed
			for i >= 48 {
				seed = rapidMix(r64(q)^s[0], r64(q[8:])^seed)
				s1 = rapidMix(r64(q[16:])^s[1], r64(q[24:])^s1)
				s2 = rapidMix(r64(q[32:])^s[2], r64(q[40:])^s2)
				q, i = q[48:], i-48
			}
			seed ^= s1 ^ s2
		}
		if i > 16 {
			seed = rapidMix(r64(q)^s[2], r64(q[8:])^seed^s[1])
			if i > 32 {
				seed = rapidMix(r64(q[16:])^s[2], r64(q[24:])^seed)
			}
		}
		a, b = r64(p[n-16:]), r64(p[n-8:])
	}
	a ^= s[1]
	b ^= seed
	hi, lo := bits.Mul64(a, b)
	return rapidMix(lo^s[0]^n, hi^s[1])
}
