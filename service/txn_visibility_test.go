package main

import (
	"fmt"
	"testing"
)

// A secret entry stays with its owner (2026-10-03).
//
// Pocket sharing exists so a partner can read a shared pocket's ledger, and
// that is the point — until a surprise gift shows up there, note and amount
// included. A 'secret' entry is readable only by the member who wrote it: the
// partner's view of the shared pocket does not contain the line at all, while
// the pocket balance still counts it, because the money really did leave.
func TestSecretTxnStaysWithItsOwner(t *testing.T) {
	a := newAPI(t)
	her := mkUser(t, a.conn, 2, "Test Pipit")

	mine := int(a.obj("POST", "/v1/pockets", map[string]any{
		"name": "BRImo", "type": "cash", "opening_balance_idr": 500000,
		"visibility": "shared"}, 201)["id"].(float64))

	gift := int(a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": mine, "direction": "out", "amount_idr": 135203,
		"category": "belanja", "note": "belanja online", "visibility": "secret"}, 201)["id"].(float64))
	a.spend(mine, "out", 5000, "transport", "parkir")

	ledger := fmt.Sprintf("/v1/transactions?pocket_id=%d", mine)

	// the owner still sees it, and knows it is hidden
	own := a.reqList("GET", ledger, a.uid)
	if len(own) != 2 {
		t.Fatalf("owner should see both entries, got %d", len(own))
	}
	flagged := false
	for _, row := range own {
		rm := row.(map[string]any)
		if int(rm["id"].(float64)) == gift {
			if rm["visibility"] != "secret" {
				t.Fatalf("owner should see visibility=secret, got %v", rm["visibility"])
			}
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("the secret entry is missing from the owner's own ledger")
	}

	// her view of the same shared pocket: no line, no note, no amount
	hers := a.reqList("GET", ledger, her)
	if len(hers) != 1 {
		t.Fatalf("partner should see only the ordinary entry, got %d: %v", len(hers), hers)
	}
	if int(hers[0].(map[string]any)["id"].(float64)) == gift {
		t.Fatal("the secret entry leaked into the partner's ledger view")
	}
	// and it cannot be fetched one by one either
	if code, _ := a.req("GET", fmt.Sprintf("/v1/transactions/%d", gift), nil, her, true); code != 404 {
		t.Fatalf("partner fetching the secret entry: status %d, want 404", code)
	}

	// the balance still tells the truth: the money is gone, whatever it bought
	if got, want := a.balanceOf(her, mine), int64(500000-135203-5000); got != want {
		t.Fatalf("balance through the shared pocket: %d, want %d", got, want)
	}

	// after the surprise it goes back to an ordinary line
	a.obj("PATCH", fmt.Sprintf("/v1/transactions/%d", gift), map[string]any{"visibility": "normal"}, 200)
	if got := a.reqList("GET", ledger, her); len(got) != 2 {
		t.Fatalf("partner should see the entry once it is normal again, got %d", len(got))
	}

	// only the two known values are accepted
	a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": mine, "direction": "out", "amount_idr": 1000,
		"visibility": "maybe"}, 400)
	a.obj("PATCH", fmt.Sprintf("/v1/transactions/%d", gift), map[string]any{"visibility": "maybe"}, 400)
}
