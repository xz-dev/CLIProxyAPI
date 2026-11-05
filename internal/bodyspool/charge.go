package bodyspool

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MemoryMargin is the margin kept under memory.max by every charge (D4).
const MemoryMargin = 32 << 20

// Charger is the per-hop memory charge (D4): an atomic byte counter of memory that in-flight
// requests may still use, checked against the hop's own cgroup before each step is allocated.
// No count cap, no lock beyond the counter, no timer, no soft accounting.
type Charger struct {
	CgroupDir   string // cgroup v2 directory holding memory.max and memory.current
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

func readCgroupInt(path string) (int64, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	s := strings.TrimSpace(string(raw))
	if s == "max" {
		return math.MaxInt64, false, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, true, err
}

// Check reads memory.max and memory.current once. NewRuntime refuses to start when either is
// missing or unreadable, so the charge never runs without its cgroup (no soft accounting).
func (c *Charger) Check() error {
	for _, name := range []string{"memory.max", "memory.current"} {
		if _, _, err := readCgroupInt(filepath.Join(c.CgroupDir, name)); err != nil {
			return fmt.Errorf("body spool: cgroup-dir %q: cannot read %s: %w", c.CgroupDir, name, err)
		}
	}
	return nil
}

// Charge reserves step bytes if memory.max − memory.current − outstanding ≥ step + 32 MiB, and
// otherwise refuses with 503 insufficient_memory naming the hop and the bytes. A cgroup without a
// memory limit (memory.max "max") has no cgroup OOM, so it admits. A missing or unreadable
// memory.max refuses.
func (c *Charger) Charge(step int64) error {
	if step <= 0 {
		return nil
	}
	limit, limited, err := readCgroupInt(filepath.Join(c.CgroupDir, "memory.max"))
	if err != nil {
		return c.refuse(step, 0, fmt.Sprintf("cannot read memory.max: %v", err))
	}
	if !limited {
		c.outstanding.Add(step)
		return nil
	}
	current, _, err := readCgroupInt(filepath.Join(c.CgroupDir, "memory.current"))
	if err != nil {
		return c.refuse(step, 0, fmt.Sprintf("cannot read memory.current: %v", err))
	}
	for {
		out := c.outstanding.Load()
		avail := limit - current - out
		if avail < step+MemoryMargin {
			return c.refuse(step, avail, "")
		}
		if c.outstanding.CompareAndSwap(out, out+step) {
			return nil
		}
	}
}

// Release returns n charged bytes.
func (c *Charger) Release(n int64) {
	if n > 0 {
		c.outstanding.Add(-n)
	}
}

func (c *Charger) refuse(step, avail int64, why string) error {
	msg := fmt.Sprintf("%s: memory charge of %d bytes refused: %d bytes available under memory.max minus outstanding charges, %d byte margin required", c.Hop, step, avail, MemoryMargin)
	if why != "" {
		msg = fmt.Sprintf("%s: memory charge of %d bytes refused: %s", c.Hop, step, why)
	}
	return &Error{Status: 503, Code: "insufficient_memory", Hop: c.Hop, Limit: step + MemoryMargin, Message: msg}
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
