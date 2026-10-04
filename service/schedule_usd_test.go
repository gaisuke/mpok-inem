package main

import (
	"fmt"
	"strings"
	"testing"
)

// A plan priced in dollars is converted with the day's rate (2026-10-04).
//
// The AI subscription is billed as $11 on a card, so the rupiah figure is only
// known when the bank converts it. The plan therefore stores the dollar price,
// and one rate per day is fetched and cached — the reminder, the booking and the
// dashboard must quote the same number, and the entry must say which rate made it.
func TestDollarPlanUsesTheDaysRate(t *testing.T) {
	a := newAPI(t)
	card := a.pocket("eCard", "cash", 500000)

	// the network is not part of a unit test: fix the rate a provider would give
	old := fxFetch
	fxFetch = func() (float64, string, error) { return 17888.613936, "stub", nil }
	t.Cleanup(func() { fxFetch = old })

	// the rate itself is a readable endpoint: the agent quotes it in the reminder
	fx := a.obj("GET", "/v1/fx", nil, 200)
	if fx["pair"] != "USDIDR" || fx["day"] == "" || fx["source"] != "stub" || fx["stale"] != false {
		t.Fatalf("kurs hari ini: %v", fx)
	}
	if rate, _ := fx["rate"].(float64); rate < 1000 {
		t.Fatalf("rate looks wrong: %v", fx["rate"])
	}

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": card, "amount_usd": 11,
		"day_of_month": 4, "payee": "OpenCode", "category": "langganan",
		"note": "langganan AI OpenCode",
	}, 201)
	id := int(created["id"].(float64))

	// 11 × 17888.613936 = 196,774.75 → rounded to the nearest 100, so the number
	// looks like money and is identical everywhere it is shown
	const wantEst = int64(196800)

	plans := a.list("GET", "/v1/schedules", 200)
	if len(plans) != 1 {
		t.Fatalf("plans: %v", plans)
	}
	s := plans[0].(map[string]any)
	if s["amount_usd"] != 11.0 {
		t.Fatalf("the dollar price is the plan's own figure: %v", s)
	}
	if s["amount_idr"] != nil {
		t.Fatalf("a dollar plan carries no rupiah figure of its own: %v", s["amount_idr"])
	}
	if got := int64(s["estimate_idr"].(float64)); got != wantEst {
		t.Fatalf("estimate_idr = %d, want %d", got, wantEst)
	}

	// one price, never two
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": card, "amount_idr": 1000, "amount_usd": 11,
		"day_of_month": 4, "payee": "OpenCode"}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": card, "amount_usd": -1,
		"day_of_month": 4, "payee": "OpenCode"}, 400)

	// booking needs no amount from the human: the day's rate supplies it
	ran := a.obj("POST", "/v1/schedules/run", map[string]any{"ids": []int{id}}, 200)
	entries := ran["ran"].([]any)
	if len(entries) != 1 {
		t.Fatalf("ran: %v (skipped: %v)", entries, ran["skipped"])
	}
	entry := entries[0].(map[string]any)
	if got := int64(entry["amount_idr"].(float64)); got != wantEst {
		t.Fatalf("booked %d, want the day's estimate %d", got, wantEst)
	}

	// the entry explains itself: which price, which rate
	txns := a.list("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", card), 200)
	if len(txns) != 1 {
		t.Fatalf("txns: %v", txns)
	}
	note := txns[0].(map[string]any)["note"].(string)
	if !strings.Contains(note, "USD 11") || !strings.Contains(note, "17889") {
		t.Fatalf("the note must carry the price and the rate used: %q", note)
	}
	if !strings.Contains(note, "OpenCode") {
		t.Fatalf("the payee must survive: %q", note)
	}
	if got := a.balance(card); got != 500000-wantEst {
		t.Fatalf("balance = %d, want %d", got, 500000-wantEst)
	}

	// an explicit amount still wins: the bank's figure is the truth, the estimate
	// is only a forecast
	a.obj("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"amount_idr": 186568}, 200)
	after := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)
	if after["amount_idr"] != 186568.0 || after["amount_usd"] != nil || after["estimate_idr"] != nil {
		t.Fatalf("switching to a rupiah figure should clear the dollar one: %v", after)
	}
}

// Without a rate there is no honest number, so the plan asks instead of guessing:
// booking an invented amount would put money in the ledger that never moved.
func TestDollarPlanWithoutARateAsksInsteadOfGuessing(t *testing.T) {
	a := newAPI(t)
	card := a.pocket("eCard", "cash", 500000)

	old := fxFetch
	fxFetch = func() (float64, string, error) { return 0, "", fmt.Errorf("sumber kurs mati") }
	t.Cleanup(func() { fxFetch = old })
	// a rate may already be cached from another test in this process: today's has
	// to go, otherwise this test would pass for the wrong reason
	if _, err := a.conn.Exec(`DELETE FROM expense.fx_rates WHERE day=current_date`); err != nil {
		t.Fatalf("clearing today's rate: %v", err)
	}

	if code, _ := a.call("GET", "/v1/fx", nil); code != 400 {
		t.Fatalf("with no rate and no provider the endpoint must say so, got %d", code)
	}

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": card, "amount_usd": 11,
		"day_of_month": 4, "payee": "OpenCode", "category": "langganan",
	}, 201)
	id := int(created["id"].(float64))

	s := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)
	if s["estimate_idr"] != nil {
		t.Fatalf("no rate means no estimate, not a made-up one: %v", s["estimate_idr"])
	}

	ran := a.obj("POST", "/v1/schedules/run", map[string]any{"ids": []int{id}}, 200)
	if len(ran["ran"].([]any)) != 0 {
		t.Fatalf("nothing may be booked without a number: %v", ran["ran"])
	}
	skipped := ran["skipped"].([]any)
	if len(skipped) != 1 || !strings.Contains(skipped[0].(map[string]any)["reason"].(string), "nominal") {
		t.Fatalf("the skip must say why: %v", skipped)
	}

	// and an amount from the human still books it
	a.obj("POST", "/v1/schedules/run", map[string]any{"ids": []int{id}, "amount_idr": 186568}, 200)
	if got := a.balance(card); got != 500000-186568 {
		t.Fatalf("balance = %d, want %d", got, 500000-186568)
	}
}
