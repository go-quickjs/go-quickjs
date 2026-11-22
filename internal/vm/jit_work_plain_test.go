//go:build !quickjs_verify && !race

package vm

// jitWorkPlainTags reports that the test is built without the tags that
// change what the compiler allocates (TestJITCompileWork's bytes).
const jitWorkPlainTags = true
