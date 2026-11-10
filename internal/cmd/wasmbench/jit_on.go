//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package main

// jitBuilt says that NewRuntimeConfigCompiler compiles to native code in
// this build.
const jitBuilt = true
