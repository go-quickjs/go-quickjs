package vm

// JITStats counts what the optional native tier did in a runtime, for tests
// and the conformance runner: they must tell code that ran natively from code
// that only could have. Compiled counts programs made; Entries native
// entries; Guards guard failures; Hosts exits to Go for an operation;
// Budgets returns to Go when an entry's instruction budget ran out; and
// Interpreted invocations finished in the interpreter after native code;
// SSAEntries counts the entries into code the new pipeline compiled, and
// SSARecords the slots its exits left to Go: references moved, and
// primitives stored over references. Held is the memory the JIT holds, and
// Budget the most it may (Config.JITMemoryLimit); Evicted counts the
// functions whose code or refusal it dropped to make room, Refused the
// compiles it found no room for, and Discarded those of them it found none
// for only once it had compiled them.
type JITStats struct {
	Compiled, Entries, Guards, Hosts, Budgets, Interpreted, SSAEntries, SSARecords uint64
	Held, Budget                                                                   int64
	Evicted, Refused, Discarded                                                    uint64
}
