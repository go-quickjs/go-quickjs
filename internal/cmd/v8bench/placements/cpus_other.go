//go:build !windows && !linux

package main

import "os/exec"

// canPin says whether startPinned gives a process its CPUs. macOS gives a
// process no CPUs of its own -- Apple silicon's threads take no affinity,
// and its clusters share their caches unlike a Ryzen's complexes -- nor
// does any other OS here: -parallel runs both labels at once, unpinned,
// each with GOMAXPROCS half the CPUs.
const canPin = false

// cacheGroups knows no caches here: -cpus auto splits the CPUs in halves,
// which then size each run's GOMAXPROCS.
func cacheGroups() []cpuSet { return nil }

// numaNodes knows no NUMA nodes here: Apple silicon is one.
func numaNodes() []cpuSet { return nil }

// coreClasses knows no classes of cores here.
func coreClasses() map[int]int { return nil }

// startPinned starts cmd, on whatever CPUs the OS gives it.
func startPinned(cmd *exec.Cmd, set cpuSet) error { return cmd.Start() }
