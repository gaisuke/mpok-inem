package main

import (
	"fmt"
	"testing"
)

// A rate can be recorded by hand, and a recorded rate wins (2026-10-04).
//
// The best source is the bank's own page, and a bank page may be unreachable
// from this host — Jago's sits behind Cloudflare, which blocks this IP. So the
// number can be pushed in by whoever *can* read it (the agent, with its own
// fetcher), and nothing else has to know the difference.
func TestStoredRateWinsOverTheProviders(t *testing.T) {
	a := newAPI(t)
	card := a.pocket("eCard", "cash", 500000)

	// start from a clean day, otherwise another test's row would answer for us
	if _, err := a.conn.Exec(`DELETE FROM expense.fx_rates WHERE day=current_date`); err != nil {
		t.Fatalf("clearing fx_rates: %v", err)
	}
	old := fxFetch
	fxFetch = func() (float64, string, error) { return 17000, "stub-market", nil }
	t.Cleanup(func() { fxFetch = old })

	// the bank's own rate, pushed in
	saved := a.obj("POST", "/v1/fx", map[string]any{
		"rate": 17892.0, "source": "jago"}, 200)
	if saved["source"] != "jago" || saved["stale"] != false {
		t.Fatalf("saving a rate: %v", saved)
	}

	// and it is what the day answers with — not the provider's number
	got := a.obj("GET", "/v1/fx", nil, 200)
	if got["rate"].(float64) != 17892.0 || got["source"] != "jago" {
		t.Fatalf("a stored rate must win: %v", got)
	}

	// a dollar plan is therefore converted with the bank's rate
	a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": card, "amount_usd": 11,
		"day_of_month": 4, "payee": "OpenCode", "category": "langganan",
	}, 201)
	s := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)
	if est := int64(s["estimate_idr"].(float64)); est != 196800 { // 11 × 17892 → 1968.12 → 1968
		t.Fatalf("estimate with the bank's rate = %d, want 196800", est)
	}

	// garbage is refused rather than stored
	a.want("POST", "/v1/fx", map[string]any{"rate": 0}, 400)
	a.want("POST", "/v1/fx", map[string]any{"rate": -5}, 400)
	a.want("POST", "/v1/fx", map[string]any{"rate": 17892, "pair": "EURIDR"}, 400)
	a.want("POST", "/v1/fx", map[string]any{"rate": 17892, "day": "04-10-2026"}, 400)

	// a day with a stored rate is not re-fetched, so a correction sticks
	a.obj("POST", "/v1/fx", map[string]any{"rate": 17900.0, "source": "koreksi"}, 200)
	if got := a.obj("GET", "/v1/fx", nil, 200); got["rate"].(float64) != 17900.0 {
		t.Fatalf("the corrected rate should stick: %v", got)
	}
	_ = fmt.Sprint()
}
