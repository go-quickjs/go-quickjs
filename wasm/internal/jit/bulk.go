//go:build amd64 || arm64

package jit

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// The bulk instructions the code does itself: memory.size, memory.copy and
// memory.fill of any memory, table.size and table.fill. Each backend's
// bulk_GOARCH.go emits them. What allocates (memory.grow, table.grow),
// reads a segment (memory.init, table.init) or copies a table's
// references (table.copy) is Go's (goOp).
//
// memory.copy, memory.fill and table.fill store every entry first, so
// that the cache's registers are free for the copy's own and no entry is
// in a register their loops overwrite; their operands are read from their
// slots. Both ends of a copy are checked before a byte is written, as the
// interpreter checks them, and a copy to a higher address that overlaps
// its source is made from its end down. They make no exit and no poll:
// the count is bounded by the memory or the table.

// memorySize compiles memory.size of memory mem: its size in bytes, read
// where an access reads it, over the page size.
func (c *compiler) memorySize(mem uint32) {
	r := c.allocG(0)
	c.memSizeInto(r, mem)
	c.shiftRight(r, 16)
	t := module.KindI32
	if c.m.Memories[mem].Limits.Is64 {
		t = module.KindI64
	}
	c.pushReg(t, uint8(r))
}

// tableSize compiles table.size of table: its number of elements.
func (c *compiler) tableSize(table uint32) {
	r := c.allocG(0)
	c.tableEntry(r, offTables, table)
	c.a.Load(r, r, c.req.Layout.TableElems+8)
	t := module.KindI32
	if c.m.Tables[table].Limits.Is64 {
		t = module.KindI64
	}
	c.pushReg(t, uint8(r))
}

// bulkOperands stores every entry into its slot and pops the instruction's
// three operands, the first last.
func (c *compiler) bulkOperands() (x, y, z value) {
	c.spillAll()
	z, y, x = c.pop(), c.pop(), c.pop()
	return x, y, z
}
