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
	hostjobs.Post = func(rt any, fn func()) {
		if r := rt.(*Runtime); !r.closed {
			r.rt.PostFromElsewhere(fn)
		}
	}
	hostjobs.Ready = func(rt any) <-chan struct{} {
		if r := rt.(*Runtime); !r.closed {
			return r.rt.HostJobsReady()
		}
		return nil
	}
}
