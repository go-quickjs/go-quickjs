package wasm

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// memory is an api.Memory: memory index of a module running code.
type memory struct {
	internalapi.WasmOnlyType
	mem   *engine.Memory
	code  *compiledCode
	index uint32
}

func (m *memory) Definition() api.MemoryDefinition {
	return m.code.memoryDefinition(m.index, exportNamesOf(m.code.m, module.ExternMemory))
}

func (m *memory) Size() uint32   { return uint32(m.mem.Size()) }
func (m *memory) Size64() uint64 { return m.mem.Size() }

func (m *memory) Grow(deltaPages uint32) (uint32, bool) {
	old, ok := m.Grow64(uint64(deltaPages))
	return uint32(old), ok
}

func (m *memory) Grow64(deltaPages uint64) (uint64, bool) {
	old := m.mem.Grow(deltaPages)
	if old < 0 {
		return 0, false
	}
	return uint64(old), true
}

// span returns the n bytes at offset, or nil and false if they are out of
// range.
func (m *memory) span(offset, n uint64) ([]byte, bool) {
	b := m.mem.Bytes()
	if offset > uint64(len(b)) || n > uint64(len(b))-offset {
		return nil, false
	}
	return b[offset : offset+n : offset+n], true
}

func (m *memory) ReadUint8(offset uint32) (byte, bool) {
	b, ok := m.span(uint64(offset), 1)
	if !ok {
		return 0, false
	}
	return b[0], true
}

func (m *memory) ReadUint16Le(offset uint32) (uint16, bool) {
	b, ok := m.span(uint64(offset), 2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b), true
}

func (m *memory) ReadUint32Le(offset uint32) (uint32, bool) {
	b, ok := m.span(uint64(offset), 4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

func (m *memory) ReadFloat32Le(offset uint32) (float32, bool) {
	v, ok := m.ReadUint32Le(offset)
	return math.Float32frombits(v), ok
}

func (m *memory) ReadUint64Le(offset uint32) (uint64, bool) {
	b, ok := m.span(uint64(offset), 8)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

func (m *memory) ReadFloat64Le(offset uint32) (float64, bool) {
	v, ok := m.ReadUint64Le(offset)
	return math.Float64frombits(v), ok
}

func (m *memory) Read(offset, byteCount uint32) ([]byte, bool) {
	return m.span(uint64(offset), uint64(byteCount))
}

func (m *memory) Read64(offset, byteCount uint64) ([]byte, bool) {
	return m.span(offset, byteCount)
}

func (m *memory) WriteUint8(offset uint32, v byte) bool {
	b, ok := m.span(uint64(offset), 1)
	if ok {
		b[0] = v
	}
	return ok
}

func (m *memory) WriteUint16Le(offset uint32, v uint16) bool {
	b, ok := m.span(uint64(offset), 2)
	if ok {
		binary.LittleEndian.PutUint16(b, v)
	}
	return ok
}

func (m *memory) WriteUint32Le(offset, v uint32) bool {
	b, ok := m.span(uint64(offset), 4)
	if ok {
		binary.LittleEndian.PutUint32(b, v)
	}
	return ok
}

func (m *memory) WriteFloat32Le(offset uint32, v float32) bool {
	return m.WriteUint32Le(offset, math.Float32bits(v))
}

func (m *memory) WriteUint64Le(offset uint32, v uint64) bool {
	b, ok := m.span(uint64(offset), 8)
	if ok {
		binary.LittleEndian.PutUint64(b, v)
	}
	return ok
}

func (m *memory) WriteFloat64Le(offset uint32, v float64) bool {
	return m.WriteUint64Le(offset, math.Float64bits(v))
}

func (m *memory) Write(offset uint32, v []byte) bool { return m.Write64(uint64(offset), v) }

func (m *memory) Write64(offset uint64, v []byte) bool {
	b, ok := m.span(offset, uint64(len(v)))
	if ok {
		copy(b, v)
	}
	return ok
}

func (m *memory) WriteString(offset uint32, v string) bool {
	b, ok := m.span(uint64(offset), uint64(len(v)))
	if ok {
		copy(b, v)
	}
	return ok
}

// table is an api.Table.
type table struct {
	internalapi.WasmOnlyType
	r *runtime
	t *engine.Table
}

func (t *table) Type() api.ValueType { return typeCode(t.t.Type, t.t.Module) }
func (t *table) Is64() bool          { return t.t.Is64 }
func (t *table) Size() uint64        { return t.t.Size() }

func (t *table) Max() (uint64, bool) {
	if !t.t.HasMax {
		return 0, false
	}
	return t.t.Max, true
}

func (t *table) Grow(delta uint64, init api.Value) (uint64, error) {
	ref, err := t.r.toRef(init.Ref, t.t.Type, t.t.Module)
	if err != nil {
		return 0, err
	}
	old := t.t.Grow(delta, ref)
	if old < 0 {
		return 0, fmt.Errorf("table of %d elements cannot grow by %d", t.t.Size(), delta)
	}
	return uint64(old), nil
}

func (t *table) Get(index uint64) (api.Value, bool) {
	if index >= t.t.Size() {
		return api.Value{}, false
	}
	return api.Value{Type: t.Type(), Ref: t.r.fromRef(t.t.Elems[index])}, true
}

func (t *table) Set(index uint64, v api.Value) error {
	if index >= t.t.Size() {
		return fmt.Errorf("table index %d is out of bounds (size %d)", index, t.t.Size())
	}
	ref, err := t.r.toRef(v.Ref, t.t.Type, t.t.Module)
	if err != nil {
		return err
	}
	t.t.Elems[index] = ref
	return nil
}
