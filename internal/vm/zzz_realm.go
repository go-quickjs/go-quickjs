package vm

import "weak"

// cellCollected is what a cleanup of a registration does once its target
// has been collected (watchForCollection): it queues the cell, if its
// registry has not gone with it. It runs on a goroutine of its own, and
// reads only what is set before the cleanup is, which never changes.
func cellCollected[_ any](q *cleanupQueue, w weak.Pointer[finalizationCell]) {
	if c := w.Value(); c != nil {
		q.push(pendingCleanup{registry: c.registry, held: c.held, cell: c})
	}
}

// keepRegistry has the realm of reg's callback keep reg, which a registration
// has just been made in (watchForCollection). The registries that have no
// cell left are let go of as the set doubles, which costs a constant for each
// registration.
func keepRegistry[_ any](r *Runtime, reg *Object) {
	d, ok := reg.dataOf().(*finalizationData)
	if !ok || !d.cleanup.IsObject() {
		return
	}
	re := r.Realm
	if o, err := r.functionRealm(d.cleanup.Object()); err == nil {
		re = o
	}
	if len(re.finalizing) >= re.finalizingAt {
		for o := range re.finalizing {
			if d, ok := o.dataOf().(*finalizationData); !ok || len(d.cells) == 0 {
				delete(re.finalizing, o)
			}
		}
		re.finalizingAt = max(2*len(re.finalizing), 8)
	}
	if re.finalizing == nil {
		re.finalizing = map[*Object]struct{}{}
	}
	re.finalizing[reg] = struct{}{}
}
