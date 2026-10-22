package mir

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Annotate has compiles record what each stretch of their code was emitted
// for (Code's Marks) and the SSA they compiled (Code's Listing): for a
// profiler of native code, which maps a sample's address to the value
// whose instructions it fell in. Set it before any compile; it costs
// nothing while it is false. The marks are amd64's only; arm64's code has
// its listing.
var Annotate bool

// Mark is where a stretch of a code's instructions begins, and what it was
// emitted for: a block ("b3"), a value ("v12 CheckShape"), a block's end
// ("b3 end If"), an edge's moves and poll ("edge b3->b5"), a value's cold
// path ("cold v12 CheckShape"), an exit's stub ("exit"), a routine the code
// shares ("routine saving") or its constants ("constants"). A stretch runs
// to the next mark's offset.
type Mark struct {
	Off int
	Tag string
}

// annotation is a compile's marks while Annotate is set; cold holds each
// cold path's tag, by its index in compiler.cold, and cur the last mark's.
type annotation struct {
	marks []Mark
	cold  []string
	cur   string
}

// mark begins a stretch of tag at the code's end.
func (c *compiler) mark(tag string) {
	if c.ann == nil {
		return
	}
	c.ann.marks = append(c.ann.marks, Mark{Off: c.a.Len(), Tag: tag})
	c.ann.cur = tag
}

// markValue begins v's stretch.
func (c *compiler) markValue(v *ssa.Value) {
	if c.ann != nil {
		c.mark(fmt.Sprintf("v%d %s", v.ID, v.Op))
	}
}

// tagCold gives the cold paths appended since the last call the current
// mark's tag, which their stretches are named for when they are emitted.
func (c *compiler) tagCold() {
	if c.ann == nil {
		return
	}
	for len(c.ann.cold) < len(c.cold) {
		c.ann.cold = append(c.ann.cold, c.ann.cur)
	}
}

// markCold begins the stretch of the i'th cold path.
func (c *compiler) markCold(i int) {
	if c.ann == nil {
		return
	}
	c.tagCold()
	c.mark("cold " + c.ann.cold[i])
}

// finishMarks is the marks at the offsets Finish moved them to, and the
// SSA compiled, for Code; nil while Annotate is false.
func (c *compiler) finishMarks() ([]Mark, string) {
	if c.ann == nil {
		return nil, ""
	}
	marks := make([]Mark, len(c.ann.marks))
	for i, m := range c.ann.marks {
		marks[i] = Mark{Off: c.a.Moved(m.Off), Tag: m.Tag}
	}
	return marks, c.f.String()
}
