//go:build linux

package bodyspool

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestChargeDoesNotRejectMemoryPressure(t *testing.T) {
	const limit = 768 << 20
	const step = 9437184
	for _, available := range []int64{41365504, 8695808, 25743360, 0, -40169472} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			c := cgroup(t, fmt.Sprint(limit), fmt.Sprint(limit-available))
			a := c.NewAccount()
			for i := 0; i < 2; i++ {
				if err := a.Charge(step); err != nil {
					t.Fatalf("memory pressure must not reject the request: %v", err)
				}
			}
			if got := c.Outstanding(); got != 2*step {
				t.Fatalf("outstanding = %d, want %d", got, 2*step)
			}
			if got := a.Release(); got != 2*step {
				t.Fatalf("released = %d, want %d", got, 2*step)
			}
			if a.Release() != 0 || c.Outstanding() != 0 {
				t.Fatal("release must remain idempotent and return all accounting")
			}
		})
	}
}

func TestConcurrentChargeAccountsWithoutAdmissionLimit(t *testing.T) {
	c := cgroup(t, "503316480", "503316480")
	accounts := make([]*Account, 64)
	var wg sync.WaitGroup
	for i := range accounts {
		accounts[i] = c.NewAccount()
		wg.Add(1)
		go func(a *Account) {
			defer wg.Done()
			if err := a.Charge(16 << 20); err != nil {
				t.Errorf("accounting must not refuse: %v", err)
			}
		}(accounts[i])
	}
	wg.Wait()
	if got, want := c.Outstanding(), int64(64*16<<20); got != want {
		t.Fatalf("outstanding = %d, want %d", got, want)
	}
	for _, a := range accounts {
		a.Release()
	}
	if c.Outstanding() != 0 {
		t.Fatal("concurrent accounting was not fully released")
	}
}

func TestNewRuntimeDoesNotRequireMemoryCgroup(t *testing.T) {
	cfg := config.BodySpoolConfig{Enabled: true, Dir: t.TempDir(), CgroupDir: t.TempDir(), DiskReserveBytes: 1}
	if err := os.Chmod(cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(cfg, nil)
	if err != nil {
		t.Fatalf("memory cgroup files must not be required: %v", err)
	}
	if err := rt.Charger.Charge(9437184); err != nil {
		t.Fatalf("missing cgroup files must not reject a request: %v", err)
	}
	rt.Charger.Release(9437184)
}
