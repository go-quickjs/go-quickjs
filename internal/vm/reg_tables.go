//go:build amd64 || arm64

package vm

// The native handlers' exit table, made from the Go handler tables once
// they are whole. Go runs a package's inits in the order of its files'
// names, and regTable and regSlowTable are written by the inits of
// reg_table.go and of each reg_table_<name>.go, which replace entries.
// This init must follow all of them, or it copies tables that later
// inits still change: the file is named so that it sorts after every
// reg_table*.go whatever follows reg_table_ ('_' sorts before 's'). The
// typed tables' copy, reg_typed.go's init, sorts after it in turn.
// TestHandlerTables, which pins every table's entries, fails if a
// renamed file breaks that order.
func init() {
	nativeExitTable = regTable
	for _, op := range nativeSlowOnMiss {
		nativeExitTable[op] = regSlowTable[op]
	}
}
