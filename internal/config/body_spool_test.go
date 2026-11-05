package config

import "testing"

func TestBodySpoolDefaultsOffAndParsesV8(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nserver:\n  port: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BodySpool.Enabled {
		t.Fatal("body spool must default to off")
	}
	cfg, err = ParseConfigBytes([]byte("config-version: 8\nserver:\n  port: 1\n  body-spool:\n    enabled: true\n    dir: /spool\n    ledger-dir: /ledger\n    fallback-limit-bytes: 99999999999\n"))
	if err != nil {
		t.Fatal(err)
	}
	eff := cfg.BodySpool.Effective()
	if !eff.Enabled || eff.Dir != "/spool" || eff.LedgerDir != "/ledger" || eff.FallbackLimitBytes != 16<<20 ||
		eff.InlineLimitBytes != 16<<20 || eff.TicketedInlineLimitBytes != 8<<20 || eff.ThresholdBytes != 64<<10 {
		t.Fatalf("effective config %+v", eff)
	}
}
