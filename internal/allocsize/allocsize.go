// Package allocsize says what Go's allocator takes for an allocation: the
// size class a small one is rounded up to, or a large one's whole pages.
// The memory meters count with it what the heap holds rather than what a
// struct's size says, which the class can exceed by half: on a 32-bit
// platform a String of 28 bytes with the 16 of its own text beside it is
// one allocation of 48.
package allocsize

// classes is Go's size classes, the sizes a small allocation is rounded up
// to: the runtime's table (internal/runtime/gc's SizeClassToSize, earlier
// runtime's class_to_size), the same on every platform and unchanged from
// Go 1.24 to 1.27. TestClassesAreTheRuntimes checks it against the classes
// runtime.MemStats reports.
var classes = [...]uint16{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224,
	240, 256, 288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768,
	896, 1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200,
	3456, 4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240,
	10880, 12288, 13568, 14336, 16384, 18432, 19072, 20480, 21760, 24576,
	27264, 28672, 32768,
}

// pageSize is the allocator's page, which a large allocation is a whole
// number of.
const pageSize = 8192

// Of is the bytes Go's allocator takes for an allocation of n bytes: 0 for
// none, a small one's size class, and a large one's pages.
func Of(n int) int {
	if n <= 0 {
		return 0
	}
	if n > int(classes[len(classes)-1]) {
		return (n + pageSize - 1) &^ (pageSize - 1)
	}
	lo, hi := 0, len(classes)-1
	for lo < hi {
		if m := int(uint(lo+hi) >> 1); int(classes[m]) < n {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return int(classes[lo])
}
