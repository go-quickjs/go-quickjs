//go:build race

package quickjs

// raceBuild is whether the race detector is on, which would report the
// store watchEngine makes on purpose: such a build notices a stop at the
// engine's counted checks only.
const raceBuild = true
