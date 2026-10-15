//go:build !windows && !linux

package cpuset

import "os/exec"

// CanPin says whether Start gives a process its CPUs. macOS gives a
// process no CPUs of its own -- Apple silicon's threads take no affinity,
// and its clusters share their caches unlike a Ryzen's complexes -- nor
// does any other OS here: -parallel runs both labels at once, unpinned,
// each with GOMAXPROCS half the CPUs.
const CanPin = false

// cacheGroups knows no caches here: -cpus auto splits the CPUs in halves,
// which then size each run's GOMAXPROCS.
func cacheGroups() []Set { return nil }

// Allowed is every CPU: a process here is given none of its own.
func Allowed() (Set, error) { return allCPUs(), nil }

// numaNodes knows no NUMA nodes here: Apple silicon is one.
func numaNodes() []Set { return nil }

// coreClasses knows no classes of cores here.
func coreClasses() map[int]int { return nil }

// Start starts cmd, on whatever CPUs the OS gives it.
func Start(cmd *exec.Cmd, set Set) error { return cmd.Start() }
