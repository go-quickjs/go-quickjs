// Package nativegen generates the register interpreter's native handlers
// (docs/register-interpreter.md, "Native handlers"): machine code made by
// the JIT's assembler, offline, and written into internal/vm as an
// assembled block of bytes and a native table of the handlers'
// addresses, which the linker resolves. Package vm holds no assembler:
// what its binary holds is the block.
//
// go generate ./internal/vm runs it (cmd), after the package's other
// generators and before layoutpad, which measures what it writes; vm's
// TestRegNativeGenerated fails while a file is not what it makes now.
//
// # vm's layout
//
// The handlers read and write vm's private types: the native context,
// the frame records, regState, frames, the Runtime's fields, objects,
// shapes and caches. The generator never imports vm. It reads the
// layout as plain numbers (Layout): offsets by "Type.field", sizes by
// type and constants by name, which vm's NativeLayout gives in a build
// with the quickjs_nativelayout tag, which nothing else builds; the
// helper cmd/layout, built with that tag and the build's others, prints
// them as JSON (ReadLayout). NativeLayout and the types it reads build on
// every architecture, and Go lays those types out alike on every 64-bit
// one, so the helper is built for the host and any 64-bit host reads
// amd64's layout. Only the JIT's build, which a host's GOOS and GOARCH
// may leave out (linux arm64's), needs a host whose build has it.
//
// The build that reads the layout has none of what the generator writes
// but the handlers' file, which the package needs to link: a variant's
// files are left out of it by their constraint.
//
// # Two axes: architectures and variants
//
// The generator makes several native dispatchers. The first axis is the
// architecture: amd64 now, arm64 next, others later. Each architecture
// has its emitter, in files of its own (amd64*.go), and its files. What
// they share is the layout and the offsets and constants read from it
// (offsets.go, consts.go), the list of the operations the handlers
// answer, in the order every emitter lays them out (handlerOps), the
// variants, and the writing of the blocks and tables (source.go).
//
// The second axis is the variant, within an architecture: a dispatch
// table over the handlers, which a runtime picks through the native
// context's table pointer (ctx.table, which the code keeps in a
// register). plain is the base: the only one in builds without the JIT,
// and the one a runtime that does not run the JIT uses in builds with
// it. feedback is a runtime's that runs the JIT, in builds with it alone:
// its handlers that record the JIT's feedback OR the kinds of value they
// met into the function's feedback slot, every other handler plain's.
// Later, mode tables -- the debugger's -- are variants too.
//
// A handler is generated once. plain's block (zzzyn_native_handlers_<arch>.s)
// holds the entry, the exit stub and every handler, and plain's table.
// Another variant overrides only the handlers that differ (its overrides,
// whose bodies the emitter writes for it, nativeGen.v): they are a block
// of their own, in the variant's file (zzzyn_native_<variant>_<arch>.s)
// with its table, whose entries point at those and at plain's handlers
// for every other operation, so that the binary holds no copy of a
// shared one. The variant's file has its build constraint: a binary
// without the JIT holds no feedback table or handler. Its own handlers
// reach plain's code through its table: the exit stub and the exit to
// nativeRun through two slots after the operations' (slotExit,
// slotExitRet), another of plain's handlers through that operation's
// entry. A jump from one handler to another of its own variant's is
// direct.
//
// vm's TestRegNativeGenerated checks every file, each from the layout of
// its variant's build; TestRegNativePlainSameWithJIT that a build with
// the JIT makes plain's as a build without does; TestRegNativeLinked
// that the blocks and tables a test binary links are the generator's for
// its build; and TestRegNativeFeedbackSelected that a runtime picks its
// table.
package nativegen
