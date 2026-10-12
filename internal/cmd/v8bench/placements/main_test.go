package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPadFunctionsOccupy32Bytes(t *testing.T) {
	dir := t.TempDir()
	for p := range 4 {
		body := strings.Replace(string(padFile(p, 4, 0)), "package vm", "package main", 1)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pad%d.go", p)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			binary := filepath.Join(dir, "pads-"+arch)
			cmd := exec.Command("go", "build", "-o", binary, "main.go", "pad0.go", "pad1.go", "pad2.go", "pad3.go")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build padding: %v\n%s", err, output)
			}
			output, err := exec.Command("go", "tool", "nm", "-size", binary).Output()
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			var addresses [4][4]uint64
			for _, line := range strings.Split(string(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 4 || fields[2] != "T" || !strings.HasPrefix(fields[3], "main.padF") {
					continue
				}
				var p, n int
				if _, err := fmt.Sscanf(fields[3], "main.padF%d_%d", &p, &n); err != nil {
					t.Fatal(err)
				}
				address, err := strconv.ParseUint(fields[0], 16, 64)
				if err != nil {
					t.Fatal(err)
				}
				addresses[p][n] = address
				found++
			}
			if found != 16 {
				t.Fatalf("found %d padding functions, want 16", found)
			}
			for p := range addresses {
				for n := 1; n < 4; n++ {
					if addresses[p][n]-addresses[p][n-1] != 32 {
						t.Fatalf("padding must occupy a 32-byte slot: %x", addresses[p])
					}
				}
			}
		})
	}
}

// TestDesignBalanced pins that over the eight placements every target is at
// each phase four times, and every pair of targets in each combination of
// phases twice: what keeps a change from being measured at the placements
// that happen to suit it.
func TestDesignBalanced(t *testing.T) {
	var ones [4]int
	var pairs [4][4][4]int
	for n := range 8 {
		d := design(n)
		for i := range 4 {
			ones[i] += d[i]
			for j := i + 1; j < 4; j++ {
				pairs[i][j][2*d[i]+d[j]]++
			}
		}
	}
	for i := range 4 {
		if ones[i] != 4 {
			t.Errorf("target %d is at phase 1 in %d placements, want 4", i, ones[i])
		}
		for j := i + 1; j < 4; j++ {
			for c, got := range pairs[i][j] {
				if got != 2 {
					t.Errorf("targets %d and %d are in combination %d in %d placements, want 2", i, j, c, got)
				}
			}
		}
	}
}

// TestPadCountsReachTheDesign pins that the pads padCounts chooses put the
// targets at the design's phases, wherever the targets start: a pad of m
// slots moves its target and every later one by 32*m bytes.
func TestPadCountsReachTheDesign(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 1000 {
		var ref [4]uint64
		a := uint64(0x140000000) + uint64(rng.Intn(1<<20))*32
		for i := range ref {
			ref[i] = a
			a += uint64(rng.Intn(1<<16)) * 32
		}
		for n := range 8 {
			counts := padCounts(ref, n)
			var got [4]uint64
			shift := uint64(0)
			for i := range ref {
				shift += uint64(counts[i]) * 32
				got[i] = ref[i] + shift
			}
			if p := phases(got); p != design(n) {
				t.Fatalf("ref %x, placement %d: pads %v give phases %v, want %v", ref, n, counts, p, design(n))
			}
		}
	}
}
