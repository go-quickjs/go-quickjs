package engine

import "unsafe"

// What wasm objects hold, for a meter that measures a heap by walking it --
// a JavaScript runtime's memory limit, whose walk reaches wasm objects
// through the JS objects that wrap them. Each MemoryFootprint returns the
// bytes its object holds and what it keeps alive; has reports whether the
// walk has counted a pointer already, marking it counted, so that what
// several objects share is counted once.

// MemoryFootprint returns what the memory's committed bytes take. They are
// keyed by their first byte, which is what a walk keys the bytes of a buffer
// over them by.
func (m *Memory) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if m == nil || has(unsafe.Pointer(m)) {
		return 0
	}
	size := int64(unsafe.Sizeof(Memory{}))
	if p := m.Base(); p != nil && !has(p) {
		if m.reservation != nil {
			size += int64(m.Size())
		} else {
			size += int64(cap(m.buf))
		}
	}
	return size
}

// MemoryFootprint returns what the table's elements take, and the GC
// objects and instances they keep alive.
func (t *Table) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if t == nil || has(unsafe.Pointer(t)) {
		return 0
	}
	return int64(unsafe.Sizeof(Table{})) + int64(cap(t.Elems))*refBytes + refsFootprint(has, func(visit func(Ref)) {
		for _, r := range t.Elems {
			visit(r)
		}
	})
}

// MemoryFootprint returns what the global takes and what its reference
// keeps alive.
func (g *Global) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if g == nil || has(unsafe.Pointer(g)) {
		return 0
	}
	return int64(unsafe.Sizeof(Global{})) + RefFootprint(g.Ref, has)
}

// MemoryFootprint returns what the function keeps alive: its instance.
func (f *Function) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if f == nil || has(unsafe.Pointer(f)) {
		return 0
	}
	return int64(unsafe.Sizeof(Function{})) + f.Inst.MemoryFootprint(has)
}

// MemoryFootprint returns what the instance holds: its memories, tables,
// globals and segments, the GC objects they reach, and the instances of the
// functions it imported.
func (inst *Instance) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if inst == nil || has(unsafe.Pointer(inst)) {
		return 0
	}
	size := int64(unsafe.Sizeof(Instance{}))
	for _, m := range inst.Memories {
		size += m.MemoryFootprint(has)
	}
	for _, t := range inst.Tables {
		size += t.MemoryFootprint(has)
	}
	for _, g := range inst.Globals {
		size += g.MemoryFootprint(has)
	}
	for _, f := range inst.Funcs {
		if f != nil && f.Inst != inst {
			size += f.MemoryFootprint(has)
		}
	}
	for _, d := range inst.datas {
		size += int64(cap(d))
	}
	for _, seg := range inst.elems {
		size += int64(cap(seg)) * refBytes
	}
	return size + refsFootprint(has, func(visit func(Ref)) {
		for _, seg := range inst.elems {
			for _, r := range seg {
				visit(r)
			}
		}
	})
}

// MemoryFootprint returns what the GC objects the frames of the calls the
// budget's instances are running hold, for a budget charging an outer
// meter: what nothing else the meter walks may reach.
func (b *Budget) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	if b == nil || has(unsafe.Pointer(b)) {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return refsFootprint(has, func(visit func(Ref)) {
		for _, x := range b.active {
			x.visitFrames(visit)
		}
	})
}

// RefFootprint returns what the object r refers to holds and keeps alive: a
// GC object's fields or elements and what they refer to, a function's
// instance. An i31, a host's value and null hold nothing of wasm's.
func RefFootprint(r Ref, has func(unsafe.Pointer) bool) int64 {
	return refsFootprint(has, func(visit func(Ref)) { visit(r) })
}

// refsFootprint returns what the objects roots visits hold and keep alive.
func refsFootprint(has func(unsafe.Pointer) bool, roots func(visit func(Ref))) int64 {
	var size int64
	var work []Ref
	visit := func(r Ref) {
		if r.p == nil || r.IsI31() {
			return
		}
		switch r.Kind() {
		case ObjStruct, ObjArray, ObjExn:
			if !has(r.p) {
				work = append(work, r)
			}
		case ObjFunc:
			size += r.Func().MemoryFootprint(has)
		}
	}
	roots(visit)
	for len(work) > 0 {
		r := work[len(work)-1]
		work = work[:len(work)-1]
		size += objectSize(r, visit)
	}
	return size
}
