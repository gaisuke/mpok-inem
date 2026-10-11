package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A forecast response in the shape inemd returns for
// GET /v1/expense/forecast?month=YYYY-MM&months=3.
const forecastBody = `{
  "month":"2026-11",
  "based_on":["2026-08","2026-09","2026-10"],
  "window":3,
  "members":[{
    "user_id":1,"name":"Dani","me":true,
    "history_months":["2026-08","2026-09","2026-10"],
    "spend_forecast_idr":200000,"spend_median_idr":100000,"one_off_total_idr":9349000,
    "categories":[{"category":"drink","direction":"out","months_seen":3,"window":3,
      "median_month_idr":100000,"forecast_idr":100000,"pattern":"rutin","entries":6}],
    "one_off":[{"id":191,"date":"2026-10-09","category":"elektronik","amount_idr":9349000,
      "note":"Xiaomi 17T","pocket":"Jago Utama","why":"187x lebih besar dari pola transaksi biasanya"}],
    "hints":["drink: rata-rata Rp100.000/bulan dari 3 bulan — muncul tiap bulan, ini yang paling bisa ditekan"]}],
  "plans":[{"id":2,"kind":"transfer","label":"Jago Utama → Tabungan Jago (tabungan)","amount_idr":1000000,"variable":false,"day_of_month":1}],
  "generated_at":"2026-10-11T10:00:00+07:00"
}`

// (a) the page's two knobs — which month and how many months to learn from —
// must reach inemd, and the caller's identity must go with them.
func TestForecastForwardsMonthAndWindow(t *testing.T) {
	f, g := &fakeInemd{body: forecastBody}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, body := get(t, h, "/api/forecast?month=2026-11&months=3", goodToken)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if f.path != "/v1/expense/forecast" {
		t.Fatalf("upstream path %q, want /v1/expense/forecast", f.path)
	}
	if !strings.Contains(f.query, "month=2026-11") || !strings.Contains(f.query, "months=3") {
		t.Fatalf("upstream query %q should carry month and months", f.query)
	}
	if f.user != "7" {
		t.Fatalf("X-User-ID %q, want 7", f.user)
	}
}

// With no arguments the route still resolves; inemd then applies its own
// defaults (next month, 3 months back).
func TestForecastWithNoArguments(t *testing.T) {
	f, g := &fakeInemd{body: forecastBody}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, body := get(t, h, "/api/forecast", goodToken)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if f.path != "/v1/expense/forecast" {
		t.Fatalf("upstream path %q", f.path)
	}
	if f.query != "" {
		t.Fatalf("no arguments should forward no query, got %q", f.query)
	}
}

// A typo in the knobs is caught here, so it cannot masquerade as an empty
// forecast; the bad request never reaches inemd.
func TestForecastRejectsBadKnobs(t *testing.T) {
	f, g := &fakeInemd{body: forecastBody}, &fakeGate{}
	h := dash(t, f, g).handler()

	for _, path := range []string{
		"/api/forecast?month=2026-13",
		"/api/forecast?month=november",
		"/api/forecast?months=0",
		"/api/forecast?months=7",
		"/api/forecast?months=many",
	} {
		if code, _ := get(t, h, path, goodToken); code != 400 {
			t.Fatalf("%s: want 400, got %d", path, code)
		}
	}
	if f.calls != 0 {
		t.Fatalf("a bad forecast request reached inemd (%d calls)", f.calls)
	}
}

// (b) with privacy off, the body is served as inemd wrote it.
func TestForecastPassedThroughAsIs(t *testing.T) {
	setPrivacy(false)
	t.Cleanup(func() { setPrivacy(false) })

	f, g := &fakeInemd{body: forecastBody}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, body := get(t, h, "/api/forecast?month=2026-11&months=3", goodToken)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("bad json: %v (%s)", err, body)
	}
	if raw["month"] != "2026-11" || int(raw["window"].(float64)) != 3 {
		t.Fatalf("envelope: %v", raw)
	}
	member := raw["members"].([]any)[0].(map[string]any)
	if int(member["spend_forecast_idr"].(float64)) != 200000 ||
		int(member["one_off_total_idr"].(float64)) != 9349000 {
		t.Fatalf("member totals: %v", member)
	}
	if member["name"] != "Dani" || member["me"] != true {
		t.Fatalf("member identity: %v", member)
	}
	plan := raw["plans"].([]any)[0].(map[string]any)
	if int(plan["amount_idr"].(float64)) != 1000000 || plan["label"] == "" {
		t.Fatalf("plan: %v", plan)
	}
}

// (c) under privacy the money fields end in _idr, so the same mask that covers
// the rest of the API turns them into null — and everything else survives.
func TestForecastHidesMoneyUnderPrivacy(t *testing.T) {
	setPrivacy(false)
	t.Cleanup(func() { setPrivacy(false) })

	f := &fakeInemd{body: forecastBody}
	h := dash(t, f, &fakeGate{}).handler()
	setPrivacy(true)

	code, body := get(t, h, "/api/forecast?month=2026-11&months=3", goodToken)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	// the figures must be gone, every one of them
	for _, leak := range []string{"200000", "1000000", "9349000", "100000"} {
		if strings.Contains(body, leak) {
			t.Fatalf("privacy on leaked %s: %s", leak, body)
		}
	}
	// the fields stay (as null), so the page still knows what to draw
	for _, want := range []string{
		`"spend_forecast_idr":null`, `"spend_median_idr":null`, `"one_off_total_idr":null`,
		`"forecast_idr":null`, `"median_month_idr":null`, `"amount_idr":null`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("masking dropped %s from the payload: %s", want, body)
		}
	}
	// ...and the non-money shape is intact
	for _, want := range []string{`"name":"Dani"`, `"category":"drink"`, `"pattern":"rutin"`,
		`"Xiaomi 17T"`, `"why":"187x`, `"based_on":["2026-08"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("masking ate a non-money field %s: %s", want, body)
		}
	}
}

// The page reaches the new tab: the markup must carry the tab, the section and
// the loader hook, and must keep the no-external-assets rule.
func TestIndexHasForecastTab(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()
	code, body := get(t, h, "/", "")
	if code != 200 {
		t.Fatalf("index status %d", code)
	}
	for _, needle := range []string{`data-tab="ramalan"`, `id="ramalan"`, "/api/forecast", "loadForecast", "Ramalan"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("index is missing %q", needle)
		}
	}
}
