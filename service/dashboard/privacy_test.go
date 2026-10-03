package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Privacy mode hides money on the way out (2026-10-03).
//
// A live stream or a screenshot is not a place for the household's balances.
// The masking is done inside this process, so the real figure never reaches the
// browser at all — masking in the page would leave the number in the response,
// one dev-tools panel away from the audience.
func TestPrivacyModeHidesMoney(t *testing.T) {
	f := &fakeInemd{body: `[{"id":2,"name":"BRImo","type":"cash","balance_idr":913666}]`}
	s := dash(t, f, &fakeGate{})
	s.passwd = "s3cret"
	h := s.handler()
	t.Cleanup(func() { setPrivacy(false) })

	call := func(method, path, body string) (int, string) {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.SetBasicAuth("dani", "s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}

	// off by default: the real figure is served
	if code, body := call(http.MethodGet, "/api/pockets", ""); code != 200 || !strings.Contains(body, "913666") {
		t.Fatalf("privacy off should serve the figure: %d %s", code, body)
	}

	// on: the field stays, the value does not
	if code, body := call(http.MethodPost, "/api/privacy", `{"on":true}`); code != 200 || !strings.Contains(body, `"on":true`) {
		t.Fatalf("turning privacy on: %d %s", code, body)
	}
	code, body := call(http.MethodGet, "/api/pockets", "")
	if code != 200 || !strings.Contains(body, `"balance_idr":null`) || strings.Contains(body, "913666") {
		t.Fatalf("privacy on should mask the figure: %d %s", code, body)
	}
	// the rest of the payload survives, so the page still renders
	if !strings.Contains(body, `"name":"BRImo"`) {
		t.Fatalf("masking must not eat the payload: %s", body)
	}
	// the switch reports its own state, and is not itself a money field
	if code, body := call(http.MethodGet, "/api/privacy", ""); code != 200 || !strings.Contains(body, `"on":true`) {
		t.Fatalf("privacy state: %d %s", code, body)
	}
	// a bad body is refused and the mode stays on
	if code, _ := call(http.MethodPost, "/api/privacy", `{"on":"maybe"}`); code != 400 {
		t.Fatalf("bad body should be refused: %d", code)
	}
	if code, body := call(http.MethodGet, "/api/pockets", ""); code != 200 || strings.Contains(body, "913666") {
		t.Fatalf("a refused toggle must not un-hide anything: %d %s", code, body)
	}

	// off again: the figures come back
	if code, _ := call(http.MethodPost, "/api/privacy", `{"on":false}`); code != 200 {
		t.Fatalf("turning privacy off failed: %d", code)
	}
	if code, body := call(http.MethodGet, "/api/pockets", ""); code != 200 || !strings.Contains(body, "913666") {
		t.Fatalf("privacy off should serve the figure again: %d %s", code, body)
	}
}

// The pocket-detail merge builds its own JSON instead of passing the upstream
// body through, so it needs its own guard against the mask being skipped.
func TestPrivacyAlsoCoversPocketDetail(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/pockets/3":
			_, _ = io.WriteString(w, `{"id":3,"name":"Cash","type":"cash","balance_idr":913666}`)
		case "/v1/transactions":
			_, _ = io.WriteString(w, `[{"id":2,"direction":"out","amount_idr":18000,"category":"transport","note":"bensin","created_at":"2026-09-20T09:05:00+07:00"}]`)
		case "/v1/transfers":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	s := &server{upstream: up.URL, gate: newFakeGate(t, &fakeGate{}).URL, userID: "1", client: up.Client()}
	setPrivacy(false)
	t.Cleanup(func() { setPrivacy(false) })
	setPrivacy(true)

	code, body := get(t, s.handler(), "/api/pocket?id=3&period=month", goodToken)
	if code != 200 {
		t.Fatalf("pocket detail: %d %s", code, body)
	}
	for _, leak := range []string{"913666", "18000"} {
		if strings.Contains(body, leak) {
			t.Fatalf("pocket detail leaked %s while privacy was on: %s", leak, body)
		}
	}
	if !strings.Contains(body, `"name":"Cash"`) || !strings.Contains(body, `"balance_idr":null`) {
		t.Fatalf("pocket detail lost its non-money fields: %s", body)
	}
}
