//go:build linux

package bodyspool

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool/bodyspooltest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// waitIdle waits until the background release run has finished.
func waitIdle(t *testing.T, c *Charger) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if !c.releaseRunning.Load() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("release run did not finish")
}

// ReclaimReleased runs one release at a time and at most once per second, only when called.
func TestReclaimReleasedSingleFlightAndRateLimited(t *testing.T) {
	now := time.Unix(1000, 0)
	var runs atomic.Int32
	started, unblock := make(chan struct{}), make(chan struct{})
	c := &Charger{clock: func() time.Time { return now }}
	c.ReleaseReclaim = func() {
		if runs.Add(1) == 1 {
			close(started)
			<-unblock
		}
	}

	c.ReclaimReleased()
	<-started
	c.ReclaimReleased() // a run is in progress: nothing
	close(unblock)
	waitIdle(t, c)
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d during one run, want 1", got)
	}

	now = now.Add(reclaimInterval - time.Nanosecond)
	c.ReclaimReleased() // less than 1 s after the last run: nothing
	waitIdle(t, c)
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d within the interval, want 1", got)
	}

	now = now.Add(time.Nanosecond)
	c.ReclaimReleased()
	waitIdle(t, c)
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d after the interval, want 2", got)
	}

	(&Charger{}).ReclaimReleased() // no ReleaseReclaim: nothing, no panic
	(*Charger)(nil).ReclaimReleased()
}

// Close records the charge it released, once; NewRuntime wires the release to FreeOSMemory.
func TestRequestReleasedBytes(t *testing.T) {
	rt, err := NewRuntime(testSpoolConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Charger.ReleaseReclaim == nil {
		t.Fatal("NewRuntime left ReleaseReclaim unset")
	}
	r := rt.NewRequest("", true)
	if err := r.Charge(3 * ChargeStep); err != nil {
		t.Fatal(err)
	}
	if got := r.ReleasedBytes(); got != 0 {
		t.Fatalf("ReleasedBytes before Close = %d", got)
	}
	r.Close()
	r.Close()
	if got := r.ReleasedBytes(); got != 3*ChargeStep {
		t.Fatalf("ReleasedBytes = %d, want %d", got, 3*ChargeStep)
	}
	if got := rt.Charger.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after Close", got)
	}
}

func testSpoolConfig(t *testing.T) config.BodySpoolConfig {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	return config.BodySpoolConfig{Enabled: true, CgroupDir: bodyspooltest.CgroupDir(t), Dir: dir, DiskReserveBytes: 1}
}
