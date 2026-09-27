package quickjs

import "github.com/go-quickjs/go-quickjs/internal/hostjobs"

// Work from other goroutines reaches a runtime through hostjobs, which is
// filled in here.
func init() {
	hostjobs.Attach = func(rt any, post func(func())) {
		if r := rt.(*Runtime); !r.closed {
			r.rt.AttachHostLoop(post)
		}
	}
	// Post may be called from any goroutine.
	hostjobs.Post = func(rt any, fn func()) {
		// Posting is what other goroutines do, so it reads nothing of the
		// runtime but the one field that is safe for them to.
		if vr := rt.(*Runtime).posting.Load(); vr != nil {
			vr.PostFromElsewhere(fn)
		}
	}
	hostjobs.Abort = func(rt any, done <-chan struct{}) {
		if r := rt.(*Runtime); !r.closed {
			r.rt.SetAbort(done)
		}
	}
	hostjobs.Ready = func(rt any) <-chan struct{} {
		if r := rt.(*Runtime); !r.closed {
			return r.rt.HostJobsReady()
		}
		return nil
	}
}
