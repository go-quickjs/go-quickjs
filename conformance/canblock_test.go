package conformance_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/conformance"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

func TestCanBlockFlag(t *testing.T) {
	for _, canBlock := range []bool{true, false} {
		meta := conformance.Metadata{Flags: map[string]bool{"raw": true, "CanBlockIsFalse": !canBlock}}
		if !canBlock {
			meta.Negative = &conformance.Negative{Phase: "runtime", Type: "TypeError"}
		}
		var stats vm.JITStats
		got, reason := runOne(&conformance.Suite{}, &conformance.Test{
			Path: "canblock.js", Source: `
				var view = new Int32Array(new SharedArrayBuffer(4));
				if (Atomics.wait(view, 0, 0, 0) !== 'timed-out') throw new Error('wait result');
			`,
			Meta: meta,
		}, nil, &stats)
		if got != resultPass {
			t.Errorf("canBlock=%v: result = %v (%s)", canBlock, got, reason)
		}
	}
}

func TestNonblockingMainWithBlockingAgent(t *testing.T) {
	var stats vm.JITStats
	got, reason := runOne(&conformance.Suite{}, &conformance.Test{
		Path: "canblock.js", Source: `
			$262.agent.start("$262.agent.report(Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 0));");
			if ($262.agent.getReport() !== 'timed-out') throw new Error('agent could not block');
		`,
		Meta: conformance.Metadata{Flags: map[string]bool{"raw": true, "CanBlockIsFalse": true}},
	}, nil, &stats)
	if got != resultPass {
		t.Fatalf("result = %v (%s), want pass", got, reason)
	}
}
