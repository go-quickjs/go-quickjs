package vm

import (
	"errors"
	"reflect"
	"runtime/metrics"
	"unsafe"
)

// Memory limits.
//
// A Go program has one heap for all of its runtimes, and the collector cannot
// say which of them the memory it holds belongs to. So a runtime with a limit
// measures its own, as QuickJS's JS_ComputeMemoryUsage does: it walks what is
// reachable from its roots -- its realms, its stack and frames, its job queue
// -- and adds up what each object, string and buffer takes. The walk costs as
// much as the heap is large, so it is made when the process has allocated a
// quarter of the limit since the last one, which the interrupt check reads
// from the Go runtime: the cost is proportional to what the script allocates,
// as a collector's is. An allocation large enough to cross the limit at once
// -- a buffer, a repeated string -- is checked before it is made.
//
// The measurement is an estimate. What a Go closure captured is invisible to
// it, and so is anything a host holds; the sizes are the engine's own layout.
// It is not what the process uses, but it grows as the script's heap grows,
// which is what a limit has to stop.

// ErrMemoryLimit is a script having used more memory than its runtime's
// limit. Like a cancelled context, it stops the script: a try/catch that
// swallowed it would leave the memory held and the limit exceeded.
var ErrMemoryLimit = errors.New("out of memory: the runtime's memory limit was exceeded")

// memoryMeter measures a runtime's heap against its limit.
type memoryMeter struct {
	limit int64
	// baseline is what the runtime's built-ins take, measured when it was
	// made, which is not the script's to pay for.
	baseline int64
	// live is the script's heap at the last measurement, and what has been
	// reserved since.
	live int64
	// allocsAt is the process's allocations at the last measurement.
	allocsAt uint64
	sample   []metrics.Sample

	// epoch marks what the walk now running has visited, so that an object
	// or a string is counted once however many references it has.
	epoch uint16
	total int64
	// The walk's work lists, kept to be reused.
	objects []*Object
	strings []*String
	values  []reflect.Value
	seen    map[unsafe.Pointer]struct{}
}

// setMemoryLimit gives the runtime a limit, measuring what its built-ins take
// so that the limit is on what the script adds.
func (r *Runtime) setMemoryLimit(limit int64) {
	if limit <= 0 {
		return
	}
	m := &memoryMeter{
		limit:  limit,
		sample: []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}},
		seen:   map[unsafe.Pointer]struct{}{},
	}
	r.meter = m
	m.baseline = m.walk(r)
	m.allocsAt = m.allocated()
}

// allocated is how much the process has allocated since it began.
func (m *memoryMeter) allocated() uint64 {
	metrics.Read(m.sample)
	if m.sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return m.sample[0].Value.Uint64()
}

// checkMemory measures the heap once enough has been allocated since it was
// last measured.
func (r *Runtime) checkMemory() error {
	m := r.meter
	if int64(m.allocated()-m.allocsAt) < max(m.limit/4, 1<<20) {
		return nil
	}
	return r.measureMemory(0)
}

// measureMemory measures the heap, reporting ErrMemoryLimit if it and need
// more bytes would be over the limit.
func (r *Runtime) measureMemory(need int64) error {
	m := r.meter
	m.allocsAt = m.allocated()
	m.live = max(0, m.walk(r)-m.baseline)
	if m.live+need > m.limit {
		return r.stop(ErrMemoryLimit)
	}
	m.live += need
	return nil
}

// reserveMemory is called before allocating n bytes at once, the size of
// which the script chose: a buffer, a repeated string. One too large for
// what is left of the limit is refused before it is allocated, rather than
// noticed after.
func (r *Runtime) reserveMemory(n int) error {
	m := r.meter
	if m == nil || n <= 0 {
		return nil
	}
	if m.live+int64(n) <= m.limit {
		m.live += int64(n)
		return nil
	}
	// What was reserved may since have been let go of.
	return r.measureMemory(int64(n))
}

// concat joins two strings as the + operator does. The join itself is a rope
// node, but writing the rope out costs its whole length -- and a string
// joined to itself, again and again, is a rope far longer than the memory it
// holds. So the shorter side is reserved: over a string doubled n times,
// what is reserved adds up to what writing it out will take.
func (r *Runtime) concat(a, b *String) (*String, error) {
	if a.length+b.length > maxStringLength {
		return nil, r.throwStringLength()
	}
	if r.meter != nil {
		short := a
		if b.length < a.length {
			short = b
		}
		if err := r.reserveMemory(stringBytes(short)); err != nil {
			return nil, err
		}
	}
	return a.Concat(b), nil
}

// stringBytes is what a string takes written out: a byte per code unit of
// ASCII, and up to three of anything else.
func stringBytes(s *String) int {
	if s.ascii {
		return s.length
	}
	return 3 * s.length
}

// Sizes of the engine's own structures, as the walk counts them.
var (
	objectSize   = int64(unsafe.Sizeof(Object{}))
	propertySize = int64(unsafe.Sizeof(Property{}))
	valueSize    = int64(unsafe.Sizeof(Value{}))
	stringSize   = int64(unsafe.Sizeof(String{}))
)

// Types the walk treats specially.
var (
	objectPtrType       = reflect.TypeOf((*Object)(nil))
	stringPtrType       = reflect.TypeOf((*String)(nil))
	runtimePtrType      = reflect.TypeOf((*Runtime)(nil))
	meterPtrType        = reflect.TypeOf((*memoryMeter)(nil))
	bigIntPtrType       = reflect.TypeOf((*BigInt)(nil))
	sharedMemoryPtrType = reflect.TypeOf((*SharedMemory)(nil))
	valueSliceType      = reflect.TypeOf([]Value(nil))
	vmPackage           = reflect.TypeOf(Object{}).PkgPath()
)

// walk measures the heap reachable from the runtime's roots.
func (m *memoryMeter) walk(r *Runtime) int64 {
	m.epoch++
	if m.epoch == 0 {
		m.epoch = 1
	}
	m.total = 0
	clear(m.seen)

	// The live part of the stack and of the call stack. What lies past them
	// is stale, and is not the script's.
	m.valueSlice(r.stack[:r.stackTop], false)
	for d := 0; d < r.frameDepth; d++ {
		m.values = append(m.values, reflect.ValueOf(r.frameAt(d)).Elem())
	}
	// Everything else the runtime holds: its realms, jobs, modules, atoms.
	// The host's job queue and the queue of WeakMaps that have gone are
	// written by other goroutines; the one holds only Go functions, and what
	// the other's maps hold is on their keys, which are counted there.
	rv := reflect.ValueOf(r).Elem()
	for i := 0; i < rv.NumField(); i++ {
		switch rv.Type().Field(i).Name {
		case "stack", "frames", "cur", "meter", "hostJobs", "weakMaps":
			continue
		}
		m.values = append(m.values, rv.Field(i))
	}

	for len(m.values) > 0 || len(m.objects) > 0 || len(m.strings) > 0 {
		switch {
		case len(m.objects) > 0:
			o := m.objects[len(m.objects)-1]
			m.objects = m.objects[:len(m.objects)-1]
			m.object(o)
		case len(m.strings) > 0:
			s := m.strings[len(m.strings)-1]
			m.strings = m.strings[:len(m.strings)-1]
			m.string(s)
		default:
			v := m.values[len(m.values)-1]
			m.values = m.values[:len(m.values)-1]
			m.generic(v)
		}
	}
	return m.total
}

// value visits one value.
func (m *memoryMeter) value(v Value) {
	switch {
	case v.ref == nil:
	case v.IsObject():
		m.addObject(v.Object())
	case v.isTag(KindString):
		m.addString(v.String())
	default:
		if ref := v.refAny(); ref != nil {
			m.values = append(m.values, reflect.ValueOf(ref))
		}
	}
}

// valueSlice visits values in a slice, counting its capacity when it owns
// its array: a frame's locals are a window on the stack, whose capacity runs
// to the stack's end.
func (m *memoryMeter) valueSlice(vs []Value, owned bool) {
	n := len(vs)
	if owned {
		n = cap(vs)
	}
	m.total += int64(n) * valueSize
	for _, v := range vs {
		if v.ref != nil {
			m.value(v)
		}
	}
}

// weakMapRefs counts a key's WeakMap values, which the key holds rather than
// the maps (see weakmap.go). A symbol's are found by reflection.
func (m *memoryMeter) weakMapRefs(refs *weakMapRefs) {
	m.total += int64(unsafe.Sizeof(*refs)) + int64(len(refs.more))*(8+valueSize)*3/2
	if refs.first.value.ref != nil {
		m.value(refs.first.value)
	}
	for _, v := range refs.more {
		if v.ref != nil {
			m.value(v)
		}
	}
}

func (m *memoryMeter) addObject(o *Object) {
	if o != nil && o.mark != m.epoch {
		o.mark = m.epoch
		m.objects = append(m.objects, o)
	}
}

func (m *memoryMeter) addString(s *String) {
	// The empty string is shared by every runtime, and is nothing.
	if s != nil && s != emptyString && s.mark != m.epoch {
		s.mark = m.epoch
		m.strings = append(m.strings, s)
	}
}

// object counts an object and what it refers to.
func (m *memoryMeter) object(o *Object) {
	m.total += objectSize + int64(cap(o.props))*propertySize
	// A shared layout's index is the layout's, not the object's.
	if s := o.shape; s != nil && s.unique && s.index != nil {
		m.total += int64(len(s.index.slots)) * 16
	}
	m.addObject(o.proto)
	for i := range o.props {
		if o.props[i].value.ref != nil {
			m.value(o.props[i].value)
		}
	}
	m.valueSlice(o.elems, true)
	if refs := o.weakMapRefs; refs != nil {
		m.weakMapRefs(refs)
	}
	if o.data != nil {
		m.values = append(m.values, reflect.ValueOf(o.data))
	}
}

// string counts a string: a flat one's bytes, or a rope's nodes.
func (m *memoryMeter) string(s *String) {
	m.total += stringSize + int64(len(s.s))
	if s.u16 != nil {
		m.total += int64(s.length) * 2
	}
	m.addString(s.left)
	m.addString(s.right)
}

// generic visits anything else the engine holds, by reflection: its own
// types, and the slices, maps and pointers that hold them. A type of another
// package -- Intl's formatters, the compiled bytecode -- is shared or the
// program's rather than the script's heap, and is not counted.
func (m *memoryMeter) generic(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		switch v.Type() {
		case objectPtrType:
			m.addObject((*Object)(v.UnsafePointer()))
			return
		case stringPtrType:
			m.addString((*String)(v.UnsafePointer()))
			return
		case runtimePtrType, meterPtrType:
			return
		case bigIntPtrType:
			b := (*BigInt)(v.UnsafePointer())
			m.total += int64(unsafe.Sizeof(BigInt{})) + int64(cap(b.V.Bits()))*8
			return
		case sharedMemoryPtrType:
			// Other agents change it as it is read, so only its size is
			// counted, which does not move.
			m.total += int64(cap((*SharedMemory)(v.UnsafePointer()).mem))
			return
		}
		elem := v.Type().Elem()
		if elem.PkgPath() != vmPackage {
			return
		}
		p := v.UnsafePointer()
		if _, ok := m.seen[p]; ok {
			return
		}
		m.seen[p] = struct{}{}
		m.total += int64(elem.Size())
		m.values = append(m.values, v.Elem())
	case reflect.Interface:
		if !v.IsNil() {
			m.generic(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(Value{}) {
			if v.CanAddr() {
				m.value(*(*Value)(unsafe.Pointer(v.UnsafeAddr())))
			} else {
				m.value(Value{num: v.Field(0).Float(), ref: v.Field(1).UnsafePointer()})
			}
			return
		}
		if v.Type().PkgPath() != vmPackage {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); mayHoldHeap(f.Kind()) {
				m.values = append(m.values, f)
			}
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		if v.Type() == valueSliceType && v.CanAddr() {
			m.valueSlice(*(*[]Value)(unsafe.Pointer(v.UnsafeAddr())), false)
			return
		}
		elem := v.Type().Elem()
		m.total += int64(v.Cap()) * int64(elem.Size())
		if mayHoldHeap(elem.Kind()) {
			for i := 0; i < v.Len(); i++ {
				m.values = append(m.values, v.Index(i))
			}
		}
	case reflect.Array:
		if mayHoldHeap(v.Type().Elem().Kind()) {
			for i := 0; i < v.Len(); i++ {
				m.values = append(m.values, v.Index(i))
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		t := v.Type()
		m.total += int64(v.Len()) * int64(t.Key().Size()+t.Elem().Size()) * 3 / 2
		keys, elems := mayHoldHeap(t.Key().Kind()), mayHoldHeap(t.Elem().Kind())
		if !keys && !elems {
			return
		}
		for it := v.MapRange(); it.Next(); {
			if keys {
				m.values = append(m.values, it.Key())
			}
			if elems {
				m.values = append(m.values, it.Value())
			}
		}
	case reflect.String:
		m.total += int64(v.Len())
	}
}

// mayHoldHeap reports whether a value of a kind can refer to more of the
// heap, and so is worth visiting.
func mayHoldHeap(k reflect.Kind) bool {
	switch k {
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice,
		reflect.Array, reflect.Map, reflect.String:
		return true
	}
	return false
}
