package engine

// regRegion is the body of a try_table or a legacy try: the module offsets
// of its instructions, [start, end), and what an exception thrown there
// does (reg_exn.go's catching loop).
type regRegion struct {
	start, end uint32
	// depth is the construct's index in the control stack; caught the
	// frame's caught exceptions while it runs, which a catch drops to.
	depth, caught int
	// clauses are a try_table's catch clauses or a legacy try's catches;
	// delegate says a legacy try delegates to the construct at target.
	clauses  []regClause
	delegate bool
	target   int
}

// regClause is a catch clause: the tag it catches, or any (all); whether
// it also delivers the exception's reference, last (ref); where its values
// go; and the pc it goes on at.
type regClause struct {
	tag      uint32
	all, ref bool
	vals     []regMove
	exn      uint32
	pc       int32
	legacy   bool
}
