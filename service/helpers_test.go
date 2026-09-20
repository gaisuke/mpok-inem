package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDateRange(t *testing.T) {
	cases := []struct {
		name, query, from, to, errPart string
	}{
		{"no filter", "", "", "", ""},
		{"both", "?from=2026-09-01&to=2026-09-30", "2026-09-01", "2026-09-30", ""},
		{"from only", "?from=2026-09-01", "2026-09-01", "2026-09-01", ""},
		{"to only", "?to=2026-09-30", "2026-09-30", "2026-09-30", ""},
		{"reversed", "?from=2026-09-30&to=2026-09-01", "", "", "before"},
		{"bad from", "?from=30-09-2026&to=2026-09-30", "", "", "from must be"},
		{"bad to", "?from=2026-09-01&to=nope", "", "", "to must be"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x"+c.query, nil)
			from, to, msg := dateRange(req)
			if c.errPart != "" {
				if msg == "" || !contains(msg, c.errPart) {
					t.Fatalf("want error containing %q, got %q", c.errPart, msg)
				}
				return
			}
			if msg != "" || from != c.from || to != c.to {
				t.Fatalf("got from=%q to=%q err=%q; want %q..%q", from, to, msg, c.from, c.to)
			}
		})
	}
}

func TestLimitParam(t *testing.T) {
	cases := []struct {
		query string
		want  int
		err   bool
	}{
		{"", 50, false},
		{"?limit=1", 1, false},
		{"?limit=200", 200, false},
		{"?limit=0", 0, true},
		{"?limit=-5", 0, true},
		{"?limit=99999", 0, true},
		{"?limit=abc", 0, true},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x"+c.query, nil)
		got, msg := limitParam(req, 50, 200)
		if c.err {
			if msg == "" {
				t.Fatalf("%q: want an error, got %d", c.query, got)
			}
			continue
		}
		if msg != "" || got != c.want {
			t.Fatalf("%q: got %d (%s), want %d", c.query, got, msg, c.want)
		}
	}
}

func TestConfirmOKAndPathID(t *testing.T) {
	for _, c := range []struct {
		query string
		want  bool
	}{{"", false}, {"?confirm=false", false}, {"?confirm=TRUE", false}, {"?confirm=true", true}} {
		req := httptest.NewRequest(http.MethodDelete, "/x"+c.query, nil)
		if got := confirmOK(req); got != c.want {
			t.Fatalf("%q: confirmOK = %v, want %v", c.query, got, c.want)
		}
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/pockets/42", nil)
	req.SetPathValue("id", "42")
	if id, err := pathID(req, "id"); err != nil || id != 42 {
		t.Fatalf("pathID: %d %v", id, err)
	}
	req.SetPathValue("id", "nope")
	if _, err := pathID(req, "id"); err == nil {
		t.Fatal("pathID should reject a non-numeric id")
	}
}

func TestTargetDay(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	req := httptest.NewRequest(http.MethodGet, "/v1/nutrition/target", nil)
	if day, msg := targetDay(req); msg != "" || day != today {
		t.Fatalf("default day: %q %q", day, msg)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/nutrition/target?day=2026-01-02", nil)
	if day, msg := targetDay(req); msg != "" || day != "2026-01-02" {
		t.Fatalf("explicit day: %q %q", day, msg)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/nutrition/target?day=02-01-2026", nil)
	if _, msg := targetDay(req); msg == "" {
		t.Fatal("bad day should error")
	}
}

func TestQID(t *testing.T) {
	if id, err := qID("7"); err != nil || id != 7 {
		t.Fatalf("qID(7): %d %v", id, err)
	}
	if _, err := qID("seven"); err == nil {
		t.Fatal("qID should reject non-numeric input")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
