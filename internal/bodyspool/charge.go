package bodyspool

import (
	"sync/atomic"
	"time"
)

// Charger tracks request memory accounting for release and reclamation only.
// It does not impose a memory-admission limit; the container enforces its hard limit.
type Charger struct {
	CgroupDir   string // legacy configuration identity for accounting reuse across reloads
	Hop         string
	outstanding atomic.Int64

	// ReleaseReclaim (debug.FreeOSMemory in production, nil = off) is run by ReclaimReleased.
	ReleaseReclaim     func()
	releaseRunning     atomic.Bool
	lastReleaseReclaim atomic.Int64 // UnixNano of the last ReleaseReclaim run, 0 = never
	clock              func() time.Time
}

// Outstanding returns the bytes currently charged.
func (c *Charger) Outstanding() int64 { return c.outstanding.Load() }

// Charge records step bytes without rejecting a request based on memory pressure.
// Keep the error return for existing ingest and fallback callbacks.
func (c *Charger) Charge(step int64) error {
	if step > 0 {
		c.outstanding.Add(step)
	}
	return nil
}

// Release returns n charged bytes.
func (c *Charger) Release(n int64) {
	if n > 0 {
		c.outstanding.Add(-n)
	}
}

// Account tracks the charges of one request so they are all released when its handler returns.
type Account struct {
	c       *Charger
	charged atomic.Int64
}

// NewAccount starts the charge account of one request.
func (c *Charger) NewAccount() *Account { return &Account{c: c} }

// Charge charges step bytes for this request.
func (a *Account) Charge(step int64) error {
	if a == nil || a.c == nil {
		return nil
	}
	if err := a.c.Charge(step); err != nil {
		return err
	}
	a.charged.Add(step)
	return nil
}

// Release returns every byte this request charged and reports how many it returned. Safe twice
// (the second call returns 0).
func (a *Account) Release() int64 {
	if a == nil || a.c == nil {
		return 0
	}
	n := a.charged.Swap(0)
	a.c.Release(n)
	return n
}

// ReclaimAfterRelease is the smallest released request charge after which the caller should call
// ReclaimReleased. A smaller request leaves less garbage than the GC's own headroom over the
// baseline live heap, so the GC pacer covers it.
const ReclaimAfterRelease int64 = 16 << 20

// reclaimInterval rate-limits ReleaseReclaim.
const reclaimInterval = time.Second

// ReclaimReleased runs ReleaseReclaim (debug.FreeOSMemory) in the background after a large
// request has finished and dropped its references. Its buffers are garbage by then, but the Go heap
// keeps them until a later GC (the forced GC runs only every 2 min when nothing allocates) and
// returns the pages to the OS only slowly. Triggered only by a finished request, never by a timer:
// one run at a time and at most once per reclaimInterval. The request path does one CAS; a call
// that finds a run in progress, or the last run less than reclaimInterval ago, does nothing.
func (c *Charger) ReclaimReleased() {
	if c == nil || c.ReleaseReclaim == nil || !c.releaseRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.releaseRunning.Store(false)
		now := c.now()
		if last := c.lastReleaseReclaim.Load(); last != 0 && now.Sub(time.Unix(0, last)) < reclaimInterval {
			return
		}
		c.ReleaseReclaim()
		c.lastReleaseReclaim.Store(now.UnixNano())
	}()
}

func (c *Charger) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}
