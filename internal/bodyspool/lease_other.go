//go:build !linux

package bodyspool

// Lease is a held shared flock on a capacity ticket (Linux only).
type Lease struct{}

// Release drops the lease.
func (l *Lease) Release() {}

// AcquireLease is unsupported off Linux; small ticketless requests pass, the rest fail closed.
func AcquireLease(ledgerDir, id string, size int64, hop string) (*Lease, error) {
	if id == "" && size < TicketThreshold {
		return nil, nil
	}
	return nil, TicketInvalid(hop, errUnsupported.Error())
}
