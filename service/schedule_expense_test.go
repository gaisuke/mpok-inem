package main

import (
	"fmt"
	"testing"
	"time"
)

// Rent paid to a landlord leaves the household: it needs a payee and no
// destination pocket. Modelling it as a transfer to a fake pocket would put a
// pocket in the books that holds nothing, so the shape is explicit — and the
// endpoint must refuse anything in between rather than guess.
func TestExpenseScheduleNeedsAPayeeAndNoDestination(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 5000000)

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1800000,
		"day_of_month": 2, "payee": "Bu Nur Ciputat", "category": "sewa",
		"note": "kontrakan Ciputat",
	}, 201)
	if created["id"] == nil {
		t.Fatal("creating an expense schedule returned no id")
	}

	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1000, "day_of_month": 2}, 400) // no payee
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "to_pocket_id": main, "amount_idr": 1000,
		"day_of_month": 2, "payee": "Bu Nur"}, 400) // a destination makes no sense
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "amount_idr": 1000, "day_of_month": 2, "payee": "Bu Nur"}, 400) // nothing pays
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1000, "day_of_month": 2,
		"payee": "Bu Nur", "starts_on": "11-2026"}, 400) // a loose date is refused, not guessed

	plans := a.list("GET", "/v1/schedules", 200)
	if len(plans) != 1 {
		t.Fatalf("plans: %v", plans)
	}
	s := plans[0].(map[string]any)
	if s["kind"] != "expense" || s["payee"] != "Bu Nur Ciputat" || s["category"] != "sewa" {
		t.Fatalf("expense plan lost its identity: %v", s)
	}
	if s["to"] != "" {
		t.Errorf("an expense has no destination pocket, got %q", s["to"])
	}
	if s["status"] == "not_started" {
		t.Errorf("a plan without starts_on runs from the beginning, got %v", s["status"])
	}
}

// A plan agreed before it begins — rent from next month — must not ask about the
// months before it starts. This is the whole reason starts_on exists.
func TestExpenseScheduleDoesNotAskBeforeItsStartMonth(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 20000000)

	now := time.Now()
	nextMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)
	dueNextMonth := nextMonth.AddDate(0, 0, 1).Format("2006-01-02") // the 2nd: when the money moves

	a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1800000,
		"day_of_month": 2, "payee": "Bu Nur Ciputat", "category": "sewa",
		"note": "kontrakan Ciputat", "starts_on": dueNextMonth,
	}, 201)

	plans := a.list("GET", "/v1/schedules", 200)
	if len(plans) != 1 {
		t.Fatalf("plans: %v", plans)
	}
	s := plans[0].(map[string]any)
	if s["status"] != "not_started" {
		t.Errorf("status before the start month = %v, want not_started", s["status"])
	}
	if s["starts_on"] != dueNextMonth {
		t.Errorf("starts_on = %v, want %v", s["starts_on"], dueNextMonth)
	}

	if pending := a.list("GET", "/v1/schedules?pending=true", 200); len(pending) != 0 {
		t.Errorf("nothing may be pending before the start month: %v", pending)
	}
	ran := a.obj("POST", "/v1/schedules/run", map[string]any{}, 200)
	if booked := ran["ran"].([]any); len(booked) != 0 {
		t.Errorf("a plan that has not started was booked: %v", booked)
	}

	// on its day next month it is due, and the reminder will ask about it
	due := a.list("GET", "/v1/schedules?date="+dueNextMonth, 200)
	if len(due) != 1 {
		t.Fatalf("plans next month: %v", due)
	}
	if st := due[0].(map[string]any)["status"]; st != "due_today" {
		t.Errorf("status on its own day = %v, want due_today", st)
	}
	pending := a.list("GET", "/v1/schedules?date="+dueNextMonth+"&pending=true", 200)
	if len(pending) != 1 {
		t.Errorf("the plan must be pending on its due date: %v", pending)
	}

}

// Booking an expense writes a normal transaction from the paying pocket — not a
// transfer, because no pocket receives the money.
func TestExpenseScheduleBookingWritesATransaction(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 20000000)

	today := time.Now().Format("2006-01-02")
	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1800000,
		"day_of_month": time.Now().Day(), "payee": "Bu Nur Ciputat", "category": "sewa",
		"note": "kontrakan Ciputat",
	}, 201)
	id := int(created["id"].(float64))

	before := a.balance(main)
	ran := a.obj("POST", "/v1/schedules/run", map[string]any{"date": today, "ids": []int{id}}, 200)
	entries := ran["ran"].([]any)
	if len(entries) != 1 {
		t.Fatalf("run: %v", ran)
	}
	entry := entries[0].(map[string]any)
	if entry["txn_id"] == nil {
		t.Fatalf("an expense must be booked as a transaction, not a transfer: %v", entry)
	}
	if entry["transfer_id"] != nil {
		t.Errorf("no pocket received this money: %v", entry)
	}

	if after := a.balance(main); after != before-1800000 {
		t.Errorf("balance %d -> %d, want a drop of 1800000", before, after)
	}

	txn := a.reqJSON("GET", fmt.Sprintf("/v1/transactions/%v", entry["txn_id"]), a.uid)
	if txn["category"] != "sewa" {
		t.Errorf("category = %v, want sewa", txn["category"])
	}
	if txn["direction"] != "out" {
		t.Errorf("direction = %v, want out", txn["direction"])
	}
	if note, _ := txn["note"].(string); note == "" {
		t.Error("the entry must carry a note")
	}
	if source, _ := txn["source"].(string); source != "scheduled" {
		t.Errorf("source = %v, want scheduled", source)
	}

	// booking the same plan twice in one month is refused by the ledger, not by
	// the caller remembering to skip it
	again := a.obj("POST", "/v1/schedules/run", map[string]any{"date": today, "ids": []int{id}}, 200)
	if booked := again["ran"].([]any); len(booked) != 0 {
		t.Errorf("one plan must only book once per month: %v", again)
	}
	if len(again["skipped"].([]any)) == 0 {
		t.Error("a refused booking must say why it was skipped")
	}
}

// An expense that overdraws its pocket is still recorded (it mirrors reality)
// but the answer says so, the same way a transfer does.
func TestExpenseScheduleWarnsWhenThePocketGoesNegative(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 100000)

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1800000,
		"day_of_month": time.Now().Day(), "payee": "Bu Nur Ciputat", "category": "sewa",
	}, 201)
	id := int(created["id"].(float64))

	ran := a.obj("POST", "/v1/schedules/run", map[string]any{
		"date": time.Now().Format("2006-01-02"), "ids": []int{id}}, 200)
	warnings := ran["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("an overdraw must be reported: %v", ran)
	}
	if a.balance(main) >= 0 {
		t.Error("the entry should still be recorded, leaving the pocket negative")
	}
}

// The start can be moved and cleared; an empty value means "every month".
func TestExpenseScheduleStartCanBeChanged(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 5000000)

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "expense", "from_pocket_id": main, "amount_idr": 1000,
		"day_of_month": 2, "payee": "Bu Nur Ciputat", "category": "sewa",
	}, 201)
	id := int(created["id"].(float64))

	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"starts_on": "2026-11-02"}, 200)
	plans := a.list("GET", "/v1/schedules?date=2026-10-02", 200)
	if len(plans) != 1 || plans[0].(map[string]any)["status"] != "not_started" {
		t.Fatalf("after setting a start date: %v", plans)
	}
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"starts_on": ""}, 200)
	plans = a.list("GET", "/v1/schedules?date=2026-10-02", 200)
	if len(plans) != 1 || plans[0].(map[string]any)["status"] == "not_started" {
		t.Fatalf("clearing the start date must re-enable the plan: %v", plans)
	}
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"starts_on": "nope"}, 400)
}
