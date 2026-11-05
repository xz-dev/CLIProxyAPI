package bodyspool

const (
	// TicketHeader carries the capacity ticket id from the inner hops; CPA consumes it and never
	// forwards it to a provider.
	TicketHeader = "X-AI-Gateway-Capacity-Ticket"
	// TicketThreshold is the body size at and above which a ticket is required (D4).
	TicketThreshold = 64 << 20
	// TicketSuffix completes the ledger file name: every hop opens <ledger>/<id>.ticket (D4).
	TicketSuffix = ".ticket"
	ticketIDLen  = 32 // 128-bit id, lowercase hex
)

// ValidTicketID reports whether id is 32 lowercase hex characters.
func ValidTicketID(id string) bool {
	if len(id) != ticketIDLen {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
