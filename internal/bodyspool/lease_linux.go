//go:build linux

package bodyspool

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lease is a held shared flock on a capacity ticket. Closing the fd drops the lock.
type Lease struct{ f *os.File }

// Release drops the lease. Safe on nil and twice.
func (l *Lease) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// AcquireLease takes the ticket lease for a request of size bytes (D4): the ticket file is opened
// O_RDONLY|O_CLOEXEC on the read-only ledger mount, LOCK_SH is taken blocking (no timer, no
// TryLock), and st_nlink > 0 is checked after the grant. It returns nil, nil for a small request
// without a ticket. It fails closed: without a ledger, 64 MiB or more is refused.
func AcquireLease(ledgerDir, id string, size int64, hop string) (*Lease, error) {
	if id == "" || ledgerDir == "" {
		if size >= TicketThreshold {
			if ledgerDir == "" && id != "" {
				return nil, &Error{Status: 503, Code: "capacity_ticket_missing", Hop: hop, Limit: TicketThreshold,
					Message: hop + ": no capacity ledger is configured, so a request of 64 MiB or more cannot be leased"}
			}
			return nil, TicketMissing(hop, size)
		}
		return nil, nil
	}
	if !ValidTicketID(id) {
		return nil, TicketInvalid(hop, "malformed id")
	}
	fd, err := unix.Open(filepath.Join(ledgerDir, id), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, TicketInvalid(hop, "cannot open ticket")
	}
	f := os.NewFile(uintptr(fd), id)
	for {
		err = unix.Flock(fd, unix.LOCK_SH)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, TicketInvalid(hop, "cannot lock ticket")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Nlink == 0 {
		_ = f.Close() // released while we waited: no reservation backs this request
		return nil, TicketInvalid(hop, "ticket already released")
	}
	return &Lease{f: f}, nil
}
