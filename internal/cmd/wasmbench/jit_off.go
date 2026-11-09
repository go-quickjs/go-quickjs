//go:build !quickjs_jit || !amd64 || !(linux || windows)

package main

// jitBuilt says that NewRuntimeConfigCompiler compiles to native code in
// this build.
const jitBuilt = false
