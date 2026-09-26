package main

import (
	"fmt"
	"testing"
	"time"
)

// A replacement plan has a beginning and an end: October's salary puts the
// savings back, and nothing should be asked for in November. Without an end date
// the plan would keep moving that money out of the salary forever.
func TestPlanWindowHidesItBeforeAndAfterItsMonths(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 10000000)
	saving := a.pocket("Tabungan Jago", "savings", 18000000)

	now := time.Now()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)
	first := month.Format("2006-01-02")
	last := month.AddDate(0, 1, -1).Format("2006-01-02") // last day of that month
	after := month.AddDate(0, 1, 0).Format("2006-01-02") // first day of the month after

	a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 2000000,
		"day_of_month": 1, "note": "ganti tabungan yang dipakai bayar kontrakan",
		"starts_on": first, "ends_on": last,
	}, 201)

	// a window that ends before it starts is nonsense, and a loose date is refused
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000,
		"day_of_month": 1, "starts_on": last, "ends_on": first}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000,
		"day_of_month": 1, "ends_on": "31-10-2026"}, 400)

	// before its month: not started, and never pending
	plans := a.list("GET", "/v1/schedules", 200)
	if len(plans) != 1 || plans[0].(map[string]any)["status"] != "not_started" {
		t.Fatalf("before the window: %v", plans)
	}
	if p := a.list("GET", "/v1/schedules?pending=true", 200); len(p) != 0 {
		t.Errorf("nothing may be pending before the window: %v", p)
	}
	if ran := a.obj("POST", "/v1/schedules/run", map[string]any{}, 200); len(ran["ran"].([]any)) != 0 {
		t.Errorf("nothing may be booked before the window opens: %v", ran)
	}

	// inside its month: due, and pending so the reminder asks
	due := a.list("GET", "/v1/schedules?date="+first, 200)
	if len(due) != 1 || due[0].(map[string]any)["status"] != "due_today" {
		t.Fatalf("inside the window: %v", due)
	}
	if p := a.list("GET", "/v1/schedules?date="+first+"&pending=true", 200); len(p) != 1 {
		t.Errorf("the plan must be pending inside its window: %v", p)
	}
	if ends := due[0].(map[string]any)["ends_on"]; ends != last {
		t.Errorf("ends_on = %v, want %v", ends, last)
	}

	// after its month: finished and silent
	afterPlans := a.list("GET", "/v1/schedules?date="+after, 200)
	if len(afterPlans) != 1 || afterPlans[0].(map[string]any)["status"] != "finished" {
		t.Fatalf("after the window: %v", afterPlans)
	}
	if p := a.list("GET", "/v1/schedules?date="+after+"&pending=true", 200); len(p) != 0 {
		t.Errorf("a finished plan must stop asking: %v", p)
	}
	// the endpoint refuses future dates, so this is also how we prove the past
	a.want("POST", "/v1/schedules/run", map[string]any{"date": after}, 400)
}

// Booking the only month of a windowed plan closes the plan itself: a temporary
// replacement that keeps asking forever is noise, and noise is how reminders get
// ignored.
func TestWindowedPlanClosesItselfWhenItsLastMonthIsBooked(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 10000000)
	saving := a.pocket("Tabungan Jago", "savings", 18000000)

	now := time.Now()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	lastOfMonth := firstOfMonth.AddDate(0, 1, -1)

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 2000000,
		"day_of_month": now.Day(), "note": "ganti tabungan bulan ini",
		"starts_on": firstOfMonth.Format("2006-01-02"),
		"ends_on":   lastOfMonth.Format("2006-01-02"),
	}, 201)
	id := int(created["id"].(float64))

	before := a.balance(saving)
	booked := a.obj("POST", "/v1/schedules/run", map[string]any{
		"date": now.Format("2006-01-02"), "ids": []int{id}}, 200)
	entries := booked["ran"].([]any)
	if len(entries) != 1 {
		t.Fatalf("booking the window's only month: %v", booked)
	}
	if entry := entries[0].(map[string]any); entry["finished"] != true {
		t.Errorf("the last month of a windowed plan must close it: %v", entry)
	}
	if after := a.balance(saving); after != before+2000000 {
		t.Errorf("savings %d -> %d, want +2000000", before, after)
	}

	plans := a.list("GET", "/v1/schedules", 200)
	if len(plans) != 1 {
		t.Fatalf("plans: %v", plans)
	}
	if active, _ := plans[0].(map[string]any)["active"].(bool); active {
		t.Error("a plan whose last month was booked must be inactive")
	}

	// and it does not come back next month
	next := firstOfMonth.AddDate(0, 1, 1).Format("2006-01-02")
	if st := a.list("GET", "/v1/schedules?date="+next, 200)[0].(map[string]any)["status"]; st != "finished" {
		t.Errorf("status after the plan ended = %v, want finished", st)
	}
}

// The window can be reopened: "berhenti dulu" is not "hapus".
func TestPlanEndDateCanBeCleared(t *testing.T) {
	a := newAPI(t)
	main := a.pocket("Jago Utama", "cash", 5000000)
	saving := a.pocket("Tabungan Jago", "savings", 0)

	created := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000,
		"day_of_month": 1, "ends_on": "2026-10-31"}, 201)
	id := int(created["id"].(float64))

	if p := a.list("GET", "/v1/schedules?date=2026-11-01", 200); p[0].(map[string]any)["status"] != "finished" {
		t.Fatalf("plan should be finished in November: %v", p)
	}
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"ends_on": ""}, 200)
	if p := a.list("GET", "/v1/schedules?date=2026-11-01", 200); p[0].(map[string]any)["status"] == "finished" {
		t.Fatalf("clearing ends_on must reopen the plan: %v", p)
	}
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", id), map[string]any{"ends_on": "nope"}, 400)
}
