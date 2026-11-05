//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

// jitTemplateCell is a cell the code keeps (jitEntry's
// ssaCells) holding *c, the template of a closure the code does not make, which
// a frame state names where the closure would be (ssa.MadeCallback). The
// constant it was read from is its closure's, the one the code was compiled
// from, of a run of the program: the code outlives that closure, as the next
// run of the program takes it up, and an exit had read the template from
// memory the collector had since given to something else.
//
//go:noinline
func jitTemplateCell[_ any](fb *jitFeedback, c *Value) *Value {
	cell := new(Value)
	*cell = *c
	k := fb.keep()
	k.cells = append(k.cells, cell)
	return cell
}
