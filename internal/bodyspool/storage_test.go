//go:build linux

package bodyspool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCheckDirRules(t *testing.T) {
	ok := t.TempDir()
	if err := os.Chmod(ok, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(ok); err != nil {
		t.Fatalf("valid dir refused: %v", err)
	}
	if err := CheckDir(""); err == nil {
		t.Fatal("empty dir accepted")
	}
	if err := os.Chmod(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(ok); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("mode 0755 accepted: %v", err)
	}
	_ = os.Chmod(ok, 0o700)
	if err := os.WriteFile(filepath.Join(ok, "left"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(ok); err == nil || !strings.Contains(err.Error(), `"left"`) {
		t.Fatalf("regular file accepted: %v", err)
	}
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		shm, err := os.MkdirTemp("/dev/shm", "lrc-")
		if err == nil {
			defer os.RemoveAll(shm)
			if err := CheckDir(shm); err == nil || !strings.Contains(err.Error(), "tmpfs") {
				t.Fatalf("tmpfs accepted: %v", err)
			}
		}
	}
}

func TestDiskFloorIs507AndNeverMemory(t *testing.T) {
	o := opts(t)
	o.Reserve = 1 << 62
	_, sp, err := Ingest(strings.NewReader(`{"image_url":"`+strings.Repeat("A", 1<<20)+`"}`), o)
	var e *Error
	if !errors.As(err, &e) || e.Status != 507 || e.Code != "insufficient_storage" || sp != nil {
		t.Fatalf("got %v", err)
	}
}

func ticket(t *testing.T) (string, string) {
	dir := t.TempDir()
	id := strings.Repeat("ab", 16)
	if err := os.WriteFile(filepath.Join(dir, id), []byte("n=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir, id
}

func TestLeaseHoldsSharedLock(t *testing.T) {
	dir, id := ticket(t)
	l, err := AcquireLease(dir, id, TicketThreshold, "cpa")
	if err != nil || l == nil {
		t.Fatalf("lease: %v", err)
	}
	fd, _ := unix.Open(filepath.Join(dir, id), unix.O_RDONLY, 0)
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("exclusive lock granted while the lease is held")
	}
	l.Release()
	l.Release()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("exclusive lock after release: %v", err)
	}
}

func TestLeaseRefusals(t *testing.T) {
	dir, id := ticket(t)
	cases := []struct {
		dir, id string
		size    int64
		code    string
	}{
		{dir, "", TicketThreshold, "capacity_ticket_missing"},
		{"", id, TicketThreshold, "capacity_ticket_missing"},
		{"", "", TicketThreshold, "capacity_ticket_missing"},
		{dir, "XYZ", 1, "capacity_ticket_invalid"},
		{dir, strings.Repeat("cd", 16), 1, "capacity_ticket_invalid"},
	}
	for _, c := range cases {
		_, err := AcquireLease(c.dir, c.id, c.size, "cpa")
		var e *Error
		if !errors.As(err, &e) || e.Code != c.code || e.Status != 503 {
			t.Errorf("%+v: got %v", c, err)
		}
	}
	if l, err := AcquireLease(dir, "", TicketThreshold-1, "cpa"); l != nil || err != nil {
		t.Fatalf("small ticketless request: %v %v", l, err)
	}
	// a ticket released (unlinked) while the hop waited for LOCK_SH is refused
	fd, _ := unix.Open(filepath.Join(dir, id), unix.O_RDONLY, 0)
	_ = unix.Flock(fd, unix.LOCK_EX)
	done := make(chan error, 1)
	go func() { _, err := AcquireLease(dir, id, TicketThreshold, "cpa"); done <- err }()
	time.Sleep(200 * time.Millisecond) // let the goroutine open the ticket and block in LOCK_SH
	_ = os.Remove(filepath.Join(dir, id))
	_ = unix.Close(fd)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "already released") {
		t.Fatalf("unlinked ticket: %v", err)
	}
}

func cgroup(t *testing.T, max, current string) *Charger {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "memory.max"), []byte(max+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "memory.current"), []byte(current+"\n"), 0o600)
	return &Charger{CgroupDir: dir, Hop: "cpa"}
}

func TestChargeRefusesAtMargin(t *testing.T) {
	c := cgroup(t, "503316480", "100000000") // 480m, ~95 MiB used
	if err := c.Charge(300 << 20); err != nil {
		t.Fatalf("fits: %v", err)
	}
	err := c.Charge(64 << 20)
	var e *Error
	if !errors.As(err, &e) || e.Status != 503 || e.Code != "insufficient_memory" || e.Hop != "cpa" {
		t.Fatalf("over margin: %v", err)
	}
	c.Release(300 << 20)
	if c.Outstanding() != 0 {
		t.Fatal("release did not return the charge")
	}
	if err := cgroup(t, "max", "1").Charge(1 << 40); err != nil {
		t.Fatalf("unlimited cgroup refused: %v", err)
	}
}

// TestConcurrentChargeNeverCrossesLimit: concurrent charges never commit more than the headroom.
func TestConcurrentChargeNeverCrossesLimit(t *testing.T) {
	c := cgroup(t, "503316480", "50331648") // 480m limit, 48 MiB current → 400 MiB usable
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := c.NewAccount()
			err := a.Charge(16 << 20)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
			} else {
				admitted++
			}
		}()
	}
	wg.Wait()
	if admitted != 25 || refused != 39 || c.Outstanding() != 25*16<<20 {
		t.Fatalf("admitted=%d refused=%d outstanding=%d", admitted, refused, c.Outstanding())
	}
}
