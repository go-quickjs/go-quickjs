package cpuset

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"syscall"
	"unsafe"
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	getLogicalProcessorInfoEx  = kernel32.NewProc("GetLogicalProcessorInformationEx")
	setProcessAffinityMaskProc = kernel32.NewProc("SetProcessAffinityMask")
	getProcessAffinityMaskProc = kernel32.NewProc("GetProcessAffinityMask")
)

// Allowed is the CPUs this process may run on, as
// GetProcessAffinityMask reports them.
func Allowed() (Set, error) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, err
	}
	var mask, system uintptr
	if r, _, err := getProcessAffinityMaskProc.Call(uintptr(h), uintptr(unsafe.Pointer(&mask)), uintptr(unsafe.Pointer(&system))); r == 0 {
		return nil, err
	}
	var set Set
	for c := range 64 {
		if uint64(mask)&(1<<c) != 0 {
			set = append(set, c)
		}
	}
	return set, nil
}

// CanPin says whether Start gives a process its CPUs.
const CanPin = true

// cacheGroups is the logical CPUs of each level 3 cache, as
// GetLogicalProcessorInformationEx reports the caches, or nil. A cache's
// record is a SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX of RelationCache: its
// CACHE_RELATIONSHIP's level at 8 and its first GROUP_AFFINITY at 40, the
// mask then the group, in every Windows' layout of it.
func cacheGroups() []Set {
	const relationCache = 2
	var n uint32
	getLogicalProcessorInfoEx.Call(relationCache, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	if r, _, _ := getLogicalProcessorInfoEx.Call(relationCache, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return nil
	}
	var groups []Set
	for off := 0; off+50 <= int(n); {
		size := int(*(*uint32)(unsafe.Pointer(&buf[off+4])))
		if size <= 0 {
			break
		}
		rel := *(*uint32)(unsafe.Pointer(&buf[off]))
		level := buf[off+8]
		mask := *(*uint64)(unsafe.Pointer(&buf[off+40]))
		group := *(*uint16)(unsafe.Pointer(&buf[off+48]))
		if rel == relationCache && level == 3 && group == 0 {
			var set Set
			for c := range 64 {
				if mask&(1<<c) != 0 {
					set = append(set, c)
				}
			}
			if len(set) > 0 && !slices.ContainsFunc(groups, func(g Set) bool { return slices.Equal(g, set) }) {
				groups = append(groups, set)
			}
		}
		off += size
	}
	slices.SortFunc(groups, func(a, b Set) int { return a[0] - b[0] })
	return groups
}

// numaNodes is the logical CPUs of each NUMA node, as
// GetLogicalProcessorInformationEx reports the nodes, or nil. A node's
// record is a SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX of RelationNumaNode:
// its NUMA_NODE_RELATIONSHIP's first GROUP_AFFINITY at 32. A process's
// memory comes from the node its threads run on, so a group of CPUs inside
// one has its memory there.
func numaNodes() []Set {
	const relationNumaNode = 1
	var n uint32
	getLogicalProcessorInfoEx.Call(relationNumaNode, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	if r, _, _ := getLogicalProcessorInfoEx.Call(relationNumaNode, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return nil
	}
	var nodes []Set
	for off := 0; off+42 <= int(n); {
		size := int(*(*uint32)(unsafe.Pointer(&buf[off+4])))
		if size <= 0 {
			break
		}
		if rel := *(*uint32)(unsafe.Pointer(&buf[off])); rel == relationNumaNode {
			mask := *(*uint64)(unsafe.Pointer(&buf[off+32]))
			if group := *(*uint16)(unsafe.Pointer(&buf[off+40])); group == 0 {
				var set Set
				for c := range 64 {
					if mask&(1<<c) != 0 {
						set = append(set, c)
					}
				}
				if len(set) > 0 {
					nodes = append(nodes, set)
				}
			}
		}
		off += size
	}
	slices.SortFunc(nodes, func(a, b Set) int { return a[0] - b[0] })
	return nodes
}

// coreClasses is each logical CPU's efficiency class, as
// GetLogicalProcessorInformationEx reports its core's -- higher for a
// faster core: a hybrid Intel's P-cores 1, its E-cores 0 -- or nil. A
// core's record is a SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX of
// RelationProcessorCore: its PROCESSOR_RELATIONSHIP's class at 9 and its
// first GROUP_AFFINITY at 32.
func coreClasses() map[int]int {
	const relationProcessorCore = 0
	var n uint32
	getLogicalProcessorInfoEx.Call(relationProcessorCore, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	if r, _, _ := getLogicalProcessorInfoEx.Call(relationProcessorCore, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return nil
	}
	classes := map[int]int{}
	for off := 0; off+42 <= int(n); {
		size := int(*(*uint32)(unsafe.Pointer(&buf[off+4])))
		if size <= 0 {
			break
		}
		if rel := *(*uint32)(unsafe.Pointer(&buf[off])); rel == relationProcessorCore {
			class := int(buf[off+9])
			mask := *(*uint64)(unsafe.Pointer(&buf[off+32]))
			if group := *(*uint16)(unsafe.Pointer(&buf[off+40])); group == 0 {
				for c := range 64 {
					if mask&(1<<c) != 0 {
						classes[c] = class
					}
				}
			}
		}
		off += size
	}
	return classes
}

// Start starts cmd on set: Windows gives a process its CPUs once it
// is running, which it has been for the moment that takes.
func Start(cmd *exec.Cmd, set Set) error {
	var mask uint64
	for _, c := range set {
		if c >= 64 {
			return fmt.Errorf("CPU %d: only the first 64 CPUs can be given", c)
		}
		mask |= 1 << c
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	const access = 0x0200 | 0x0400 // PROCESS_SET_INFORMATION, PROCESS_QUERY_INFORMATION
	h, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err == nil {
		defer syscall.CloseHandle(h)
		var r uintptr
		if r, _, err = setProcessAffinityMaskProc.Call(uintptr(h), uintptr(mask)); r != 0 {
			return nil
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	return errors.Join(errors.New("setting the CPUs a run may use"), err)
}
