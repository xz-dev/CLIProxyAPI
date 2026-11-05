package bodyspool

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// Charge reserves step bytes if memory.max − memory.current − outstanding ≥ step + 32 MiB, and
// otherwise refuses with 503 insufficient_memory naming the hop and the bytes. A cgroup without a
// memory limit (memory.max "max" or no memory controller file) has no cgroup OOM, so it admits.
func (c *Charger) Charge(step int64) error {
	if step <= 0 {
		return nil
	}
	limit, limited, err := readCgroupInt(filepath.Join(c.CgroupDir, "memory.max"))
	if os.IsNotExist(err) || (err == nil && !limited) {
		c.outstanding.Add(step)
		return nil
	}
	if err != nil {
		return c.refuse(step, 0, fmt.Sprintf("cannot read memory.max: %v", err))
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

// Release returns every byte this request charged. Safe twice.
func (a *Account) Release() {
	if a == nil || a.c == nil {
		return
	}
	a.c.Release(a.charged.Swap(0))
}
