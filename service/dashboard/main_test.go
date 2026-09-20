package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeInemd stands in for the real service and records what the dashboard asked
// for, so the passthrough (path, query, header) can be asserted.
type fakeInemd struct {
	path  string
	query string
	user  string
	body  string
	code  int
}

func newFakeDash(t *testing.T, f *fakeInemd) (*server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path, f.query, f.user = r.URL.Path, r.URL.RawQuery, r.Header.Get("X-User-ID")
		code := f.code
		if code == 0 {
			code = 200
		}
		body := f.body
		if body == "" {
			body = `{"ok":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	s := &server{upstream: up.URL, userID: "7", client: up.Client()}
	return s, up
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestPassthroughPathsAndHeader(t *testing.T) {
	cases := []struct {
		request, wantPath, wantQueryPart string
	}{
		{"/api/pockets", "/v1/pockets", ""},
		{"/api/summary?period=today", "/v1/expense/summary", "from=" + time.Now().Format("2006-01-02")},
		{"/api/txns?period=month&limit=50", "/v1/transactions", "limit=50"},
		{"/api/notes", "/v1/notes", "limit=100"},
		{"/api/notes?tag=bills", "/v1/notes", "tag=bills"},
		{"/api/notes?q=wifi%20bill", "/v1/notes/search", "q=wifi%20bill"},
		{"/api/notes/12", "/v1/notes/12", ""},
		{"/api/meals?day=2026-09-01", "/v1/meals", "day=2026-09-01"},
		{"/api/day", "/v1/nutrition/daily", "day=" + time.Now().Format("2006-01-02")},
		{"/api/target", "/v1/nutrition/target", "day=" + time.Now().Format("2006-01-02")},
	}
	for _, c := range cases {
		t.Run(c.request, func(t *testing.T) {
			f := &fakeInemd{}
			s, _ := newFakeDash(t, f)
			code, body := get(t, s.handler(), c.request)
			if code != 200 {
				t.Fatalf("status %d (%s)", code, body)
			}
			if f.path != c.wantPath {
				t.Fatalf("upstream path %q, want %q", f.path, c.wantPath)
			}
			if c.wantQueryPart != "" && !strings.Contains(f.query, c.wantQueryPart) {
				t.Fatalf("upstream query %q, want it to contain %q", f.query, c.wantQueryPart)
			}
			if f.user != "7" {
				t.Fatalf("X-User-ID %q, want 7", f.user)
			}
		})
	}
}

func TestReadOnlySurface(t *testing.T) {
	f := &fakeInemd{}
	s, _ := newFakeDash(t, f)
	h := s.handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/pockets", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("%s /api/pockets should not be routed, got %d", method, rec.Code)
		}
		if f.path != "" {
			t.Fatalf("%s reached inemd (%s) — the dashboard must never write", method, f.path)
		}
	}

	if code, _ := get(t, h, "/api/nope"); code != 404 {
		t.Fatalf("unknown api path: %d", code)
	}
	if code, _ := get(t, h, "/seed"); code != 404 {
		t.Fatalf("unknown page: %d", code)
	}
}

func TestIndexIsServedAndOffline(t *testing.T) {
	f := &fakeInemd{}
	s, _ := newFakeDash(t, f)
	code, body := get(t, s.handler(), "/")
	if code != 200 {
		t.Fatalf("index status %d", code)
	}
	for _, needle := range []string{"Mpok Inem", "/api/summary", "/api/notes", "/api/meals", "read-only"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("index is missing %q", needle)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "cdn.") {
		t.Fatal("index must not depend on external assets")
	}
}

func TestBasicAuthIsEnforcedWhenConfigured(t *testing.T) {
	f := &fakeInemd{}
	s, _ := newFakeDash(t, f)
	s.authUser, s.passwd = "dani", "s3cret"
	h := s.handler()

	if code, _ := get(t, h, "/api/pockets"); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/pockets", nil)
	req.SetBasicAuth("dani", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong password: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/pockets", nil)
	req.SetBasicAuth("dani", "s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || f.path != "/v1/pockets" {
		t.Fatalf("authenticated request: %d %s", rec.Code, f.path)
	}
	if rec.Header().Get("WWW-Authenticate") != "" && rec.Code == 401 {
		t.Fatal("authenticated response should not challenge")
	}
}

func TestUpstreamErrorsArePassedThrough(t *testing.T) {
	f := &fakeInemd{code: 409, body: `{"error":"pocket has entries"}`}
	s, _ := newFakeDash(t, f)
	code, body := get(t, s.handler(), "/api/pockets")
	if code != 409 || !strings.Contains(body, "pocket has entries") {
		t.Fatalf("upstream error not passed through: %d %s", code, body)
	}

	down := &server{upstream: "http://127.0.0.1:1", userID: "1", client: &http.Client{Timeout: time.Second}}
	code, body = get(t, down.handler(), "/api/pockets")
	if code != http.StatusBadGateway {
		t.Fatalf("unreachable inemd: %d %s", code, body)
	}
	var msg map[string]string
	if err := json.Unmarshal([]byte(body), &msg); err != nil || msg["error"] == "" {
		t.Fatalf("bad gateway body: %s", body)
	}
}

func TestPocketDetailMergesPocketLedgerAndTransfers(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-User-ID") != "9" {
			t.Errorf("missing X-User-ID on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/pockets/3":
			_, _ = io.WriteString(w, `{"id":3,"name":"Cash","type":"cash","balance_idr":13000}`)
		case "/v1/transactions":
			_, _ = io.WriteString(w, `[
				{"id":2,"direction":"out","amount_idr":25000,"category":"food","note":"kopi","created_at":"2026-09-20T09:05:00+08:00"},
				{"id":1,"direction":"in","amount_idr":5000,"category":"refund","note":"","created_at":"2026-09-19T08:00:00+08:00"}]`)
		case "/v1/transfers":
			_, _ = io.WriteString(w, `[
				{"id":5,"from_pocket_id":3,"from":"Cash","to":"Savings","amount_idr":50000,"note":"save","created_at":"2026-09-20T10:00:00+08:00"},
				{"id":6,"from_pocket_id":2,"from":"BRImo","to":"Cash","amount_idr":100000,"note":"","created_at":"2026-09-18T07:00:00+08:00"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	s := &server{upstream: up.URL, userID: "9", client: up.Client()}

	code, body := get(t, s.handler(), "/api/pocket?id=3&period=month")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var d struct {
		Pocket struct {
			Name    string `json:"name"`
			Balance int64  `json:"balance_idr"`
		} `json:"pocket"`
		Totals struct {
			Out, In, TransferOut, TransferIn int64
		} `json:"totals"`
		Movements []struct {
			Kind      string `json:"kind"`
			Direction string `json:"direction"`
			Amount    int64  `json:"amount_idr"`
			Label     string `json:"label"`
			At        string `json:"at"`
		} `json:"movements"`
	}
	// totals/movement keys are named explicitly in the handler; decode loosely
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw["pocket"].(map[string]any)["name"] != "Cash" {
		t.Fatalf("pocket passthrough: %s", body)
	}
	totals := raw["totals"].(map[string]any)
	if totals["out_idr"].(float64) != 25000 || totals["in_idr"].(float64) != 5000 ||
		totals["transfer_out_idr"].(float64) != 50000 || totals["transfer_in_idr"].(float64) != 100000 {
		t.Fatalf("totals: %v", totals)
	}
	// transfers carry the direction relative to *this* pocket, and both kinds are
	// merged newest-first
	moves := raw["movements"].([]any)
	if len(moves) != 4 {
		t.Fatalf("movements: %v", moves)
	}
	first := moves[0].(map[string]any)
	if first["kind"] != "transfer" || first["direction"] != "out" || first["label"] != "→ Savings · save" {
		t.Fatalf("outgoing transfer row: %v", first)
	}
	second := moves[1].(map[string]any)
	if second["kind"] != "txn" || second["direction"] != "out" || second["label"] != "kopi" {
		t.Fatalf("txn row: %v", second)
	}
	last := moves[3].(map[string]any)
	if last["direction"] != "in" || last["label"] != "← BRImo" {
		t.Fatalf("incoming transfer row: %v", last)
	}
	// an empty note falls back to the category
	if moves[2].(map[string]any)["label"] != "refund" {
		t.Fatalf("category fallback: %v", moves[2])
	}

	wantQueries := []string{
		"/v1/pockets/3?",
		"/v1/transactions?month=" + time.Now().Format("2006-01") + "&pocket_id=3&limit=500",
		"/v1/transfers?month=" + time.Now().Format("2006-01") + "&pocket_id=3&limit=500",
	}
	if len(got) != len(wantQueries) {
		t.Fatalf("upstream calls: %v", got)
	}
	for i, want := range wantQueries {
		if got[i] != want {
			t.Fatalf("call %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestPocketDetailGuards(t *testing.T) {
	f := &fakeInemd{}
	s, _ := newFakeDash(t, f)
	if code, _ := get(t, s.handler(), "/api/pocket"); code != 400 {
		t.Fatalf("missing id: %d", code)
	}
	if code, _ := get(t, s.handler(), "/api/pocket?id=abc"); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := get(t, s.handler(), "/api/pocket?id=1&period=nonsense"); code != 400 {
		t.Fatalf("bad period: %d", code)
	}
	down := &server{upstream: "http://127.0.0.1:1", userID: "1", client: &http.Client{Timeout: time.Second}}
	if code, _ := get(t, down.handler(), "/api/pocket?id=1"); code != http.StatusBadGateway {
		t.Fatalf("unreachable inemd: %d", code)
	}
}

func TestPeriodQuery(t *testing.T) {
	today := time.Now()
	iso := func(d time.Time) string { return d.Format("2006-01-02") }
	weekStart := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))

	cases := []struct {
		period, want string
		bad          bool
	}{
		{"", "?month=" + today.Format("2006-01"), false},
		{"month", "?month=" + today.Format("2006-01"), false},
		{today.Format("2006-01"), "?month=" + today.Format("2006-01"), false},
		{"today", "?from=" + iso(today) + "&to=" + iso(today), false},
		{"yesterday", "?from=" + iso(today.AddDate(0, 0, -1)) + "&to=" + iso(today.AddDate(0, 0, -1)), false},
		{"week", "?from=" + iso(weekStart) + "&to=" + iso(today), false},
		{"last-week", "?from=" + iso(weekStart.AddDate(0, 0, -7)) + "&to=" + iso(weekStart.AddDate(0, 0, -1)), false},
		{"last-month", "?month=" + time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location()).AddDate(0, 0, -1).Format("2006-01"), false},
		{"2026-01-05..2026-02-03", "?from=2026-01-05&to=2026-02-03", false},
		{"2026-13-05..2026-02-03", "", true},
		{"nonsense", "", true},
		{"2026-1", "", true},
	}
	for _, c := range cases {
		t.Run(c.period, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/summary?period="+c.period, nil)
			got, err := periodQuery(req)
			if c.bad {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("periodQuery(%q) = %q (%v), want %q", c.period, got, err, c.want)
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/meals?day=2026-09-01", nil)
	if got := dayOr(req, "2026-01-01"); got != "2026-09-01" {
		t.Fatalf("dayOr: %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/meals?day=01-09-2026", nil)
	if got := dayOr(req, "2026-01-01"); got != "2026-01-01" {
		t.Fatalf("dayOr should fall back: %q", got)
	}
	for _, c := range []struct{ in, want string }{{"", "100"}, {"50", "50"}, {"0", "100"}, {"abc", "100"}, {"501", "500"}} {
		req = httptest.NewRequest(http.MethodGet, "/api/txns?limit="+c.in, nil)
		if got := limitOr(req, "100", "500"); got != c.want {
			t.Fatalf("limitOr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := urlQueryEscape("wifi bill & home"); got != "wifi%20bill%20%26%20home" {
		t.Fatalf("urlQueryEscape: %q", got)
	}
	monday := monday(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)) // a Sunday
	if monday.Format("2006-01-02") != "2026-09-14" || monday.Hour() != 0 {
		t.Fatalf("monday(): %v", monday)
	}
}
