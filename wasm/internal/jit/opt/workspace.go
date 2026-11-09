package opt

import "sync"

// workspace is what a compile works in -- the IR's values and blocks, the
// passes' tables, the allocator's and the lowering's -- kept for the next
// compile, so that a compile allocates little once the workspace has grown
// to its function's size. A compile takes one from workspaces and puts it
// back when its code is made: nothing the code holds is the workspace's.
type workspace struct {
	gen genScratch
}

var workspaces = sync.Pool{New: func() any { return new(workspace) }}
