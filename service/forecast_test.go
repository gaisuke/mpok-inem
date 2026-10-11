package main

import (
	"testing"
)

// A forecast is only useful if a spike does not become the new normal.
func TestMedianIgnoresTheSpike(t *testing.T) {
	if got := median([]int64{100000, 300000, 9000000}); got != 300000 {
		t.Fatalf("median = %d, want 300000 (the mean would say 3.1jt)", got)
	}
	if got := median([]int64{50000}); got != 50000 {
		t.Fatalf("median of one = %d", got)
	}
	if got := median(nil); got != 0 {
		t.Fatalf("median of nothing = %d", got)
	}
	if got := median([]int64{40000, 60000}); got != 50000 {
		t.Fatalf("median of two = %d, want the middle", got)
	}
}

// An event, not a habit: this month's phone is not next month's phone.
func TestOneOffNeedsSizeAndContrast(t *testing.T) {
	if !oneOff(9349000, 12000) {
		t.Fatal("a 9,3jt phone against a 12rb habit is an event")
	}
	if oneOff(50000, 40000) {
		t.Fatal("50rb against 40rb is a slightly bigger habit, not an event")
	}
	if oneOff(600000, 50000) {
		t.Fatal("600rb is big for a month but not a one-off event")
	}
	if !oneOff(1200000, 50000) {
		t.Fatal("1,2jt against a 50rb habit is an event")
	}
	// a category seen once must not be its own yardstick
	if got := baseline([]int64{9349000}, []int64{50000, 50000, 60000, 9349000}); got != 55000 {
		t.Fatalf("baseline for a one-entry category = %d, want the general pattern", got)
	}
	if got := baseline([]int64{40000, 60000, 80000}, []int64{50000}); got != 60000 {
		t.Fatalf("a category with history judges itself: %d", got)
	}
}

// The window is built from completed months, never the month being forecast.
func TestMonthListIsThePast(t *testing.T) {
	got := monthList("2026-11", 3)
	want := []string{"2026-08", "2026-09", "2026-10"}
	if len(got) != 3 {
		t.Fatalf("months = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("months = %v, want %v", got, want)
		}
	}
}

// The whole point: habits are forecast, the phone is named and set aside, and a
// category that appears in one of three months is amortised instead of dropped.
func TestForecastSeparatesHabitsFromEvents(t *testing.T) {
	a := newAPI(t)
	card := a.pocket("Jago Utama", "cash", 20000000)

	// three finished months of a coffee-and-snack habit
	for _, day := range []string{"2026-08-05", "2026-08-20", "2026-09-05", "2026-09-20",
		"2026-10-02", "2026-10-04"} {
		a.obj("POST", "/v1/transactions", map[string]any{
			"pocket_id": card, "direction": "out", "amount_idr": 50000,
			"category": "drink", "note": "kopi", "date": day}, 201)
	}
	// one month only: a quarterly thing, amortised over the window
	a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": card, "direction": "out", "amount_idr": 300000,
		"category": "server", "note": "VPS", "date": "2026-10-03"}, 201)
	// the event: a phone, once
	phone := a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": card, "direction": "out", "amount_idr": 9349000,
		"category": "elektronik", "note": "Xiaomi 17T", "date": "2026-10-09"}, 201)

	out := a.obj("GET", "/v1/expense/forecast?month=2026-11&months=3", nil, 200)
	if out["month"] != "2026-11" {
		t.Fatalf("month: %v", out["month"])
	}
	members := out["members"].([]any)
	if len(members) == 0 {
		t.Fatal("no members in the forecast")
	}
	me := members[0].(map[string]any)
	if me["me"] != true {
		t.Fatalf("the caller comes first and is marked as me: %v", me["me"])
	}

	// the habit: 100rb a month, forecast as such
	byCat := map[string]map[string]any{}
	for _, c := range me["categories"].([]any) {
		cm := c.(map[string]any)
		byCat[cm["category"].(string)] = cm
	}
	if got := int64(byCat["drink"]["forecast_idr"].(float64)); got != 100000 {
		t.Fatalf("drink forecast = %d, want 100000", got)
	}
	if byCat["drink"]["pattern"] != "rutin" {
		t.Fatalf("drink pattern = %v, want rutin", byCat["drink"]["pattern"])
	}
	// seen in one of three months: 300rb → 100rb a month, not zero and not 300rb
	if got := int64(byCat["server"]["forecast_idr"].(float64)); got != 100000 {
		t.Fatalf("amortised server = %d, want 100000", got)
	}
	if _, present := byCat["elektronik"]; present {
		t.Fatalf("the phone must not become a monthly habit: %v", byCat["elektronik"])
	}

	events := me["one_off"].([]any)
	if len(events) != 1 {
		t.Fatalf("one_off = %v", events)
	}
	ev := events[0].(map[string]any)
	if int64(ev["id"].(float64)) != int64(phone["id"].(float64)) {
		t.Fatalf("the event should be the phone: %v", ev)
	}
	if ev["why"] == "" {
		t.Fatal("an excluded event must say why it was excluded")
	}
	// and it is reported, not hidden
	if got := int64(me["one_off_total_idr"].(float64)); got != 9349000 {
		t.Fatalf("one-off total = %d", got)
	}
	if got := int64(me["spend_forecast_idr"].(float64)); got != 200000 {
		t.Fatalf("spend forecast = %d, want 200000 (100rb habit + 100rb amortised)", got)
	}

	// the window is not optional knowledge: bad input is refused
	a.want("GET", "/v1/expense/forecast?month=nope", nil, 400)
	a.want("GET", "/v1/expense/forecast?months=0", nil, 400)
	a.want("GET", "/v1/expense/forecast?months=99", nil, 400)
}

// A partner's spending is forecast too — through the pockets they shared, which
// is exactly how the ledger serves them everywhere else.
func TestForecastIncludesTheOtherMember(t *testing.T) {
	a := newAPI(t)
	her := mkUser(t, a.conn, 2, "Test Pipit")
	mine := a.pocket("Jago Utama", "cash", 5000000)

	// her shared pocket, and a month of her own spending
	herPocket := int(a.reqJSONObj("POST", "/v1/pockets", her, map[string]any{
		"name": "seabank", "type": "cash", "opening_balance_idr": 2000000,
		"visibility": "shared"}, 201)["id"].(float64))
	a.reqJSONObj("POST", "/v1/transactions", her, map[string]any{
		"pocket_id": herPocket, "direction": "out", "amount_idr": 300000,
		"category": "belanja", "note": "bulanan", "date": "2026-10-04"}, 201)
	// and one in a pocket she keeps to herself: it must not leak into his view
	herPrivate := int(a.reqJSONObj("POST", "/v1/pockets", her, map[string]any{
		"name": "rahasia", "type": "cash", "opening_balance_idr": 100000}, 201)["id"].(float64))
	a.reqJSONObj("POST", "/v1/transactions", her, map[string]any{
		"pocket_id": herPrivate, "direction": "out", "amount_idr": 700000,
		"category": "pribadi", "note": "jangan kelihatan", "date": "2026-10-05"}, 201)
	_ = mine

	out := a.obj("GET", "/v1/expense/forecast?month=2026-11", nil, 200)
	members := out["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %v", members)
	}
	herView := members[1].(map[string]any)
	if herView["me"] != false || herView["name"] != "Test Pipit" {
		t.Fatalf("second member: %v", herView)
	}
	cats := herView["categories"].([]any)
	if len(cats) != 1 || cats[0].(map[string]any)["category"] != "belanja" {
		t.Fatalf("her shared spending only: %v", cats)
	}
	if got := int64(herView["spend_forecast_idr"].(float64)); got == 0 {
		t.Fatalf("her forecast should not be zero: %v", herView)
	}
	for _, c := range cats {
		if c.(map[string]any)["category"] == "pribadi" {
			t.Fatal("a private pocket leaked into the household forecast")
		}
	}
}
