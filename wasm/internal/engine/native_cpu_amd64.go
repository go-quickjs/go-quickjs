package engine

import "golang.org/x/sys/cpu"

// nativeSIMDOn says the CPU has what the SIMD handlers use: SSSE3, SSE4.1
// and SSE4.2. Without, rSIMD's table is nativeSIMDExits.
var nativeSIMDOn = cpu.X86.HasSSSE3 && cpu.X86.HasSSE41 && cpu.X86.HasSSE42
