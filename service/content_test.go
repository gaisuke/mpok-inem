package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The content producer queue: topics in, drafts out, reviewed by a human.
func TestContentTopicsAndDraftsLifecycle(t *testing.T) {
	a := newAPI(t)

	// filing trends: duplicates are reported, not errors
	made := a.obj("POST", "/v1/content/topics", map[string]any{
		"topics": []string{"cara nabung buat pemula", "belajar cloud dari nol"},
		"niche":  "keuangan pribadi", "source": "threads_search", "score": 3.5,
		"angle":          "momen pertama kali sadar uang habis di akhir bulan",
		"material_title": "I don't want to read what you didn't write",
		"material_url":   "https://example.com/bahan"}, 201)
	if len(made["saved"].([]any)) != 2 {
		t.Fatalf("topics saved: %v", made)
	}
	again := a.obj("POST", "/v1/content/topics", map[string]any{"topic": "cara nabung buat pemula"}, 201)
	if len(again["saved"].([]any)) != 0 || int(again["already_known"].(float64)) != 1 {
		t.Fatalf("a known topic should be reported, not duplicated: %v", again)
	}
	a.want("POST", "/v1/content/topics", map[string]any{}, 400)

	topics := a.list("GET", "/v1/content/topics", 200)
	if len(topics) != 2 {
		t.Fatalf("topic list: %v", topics)
	}
	first := topics[0].(map[string]any)
	topicID := int(first["id"].(float64))
	if first["status"] != "new" || first["source"] != "threads_search" {
		t.Fatalf("topic fields: %v", first)
	}
	// the bank keeps the ANGLE and the MATERIAL, not article text: the post is
	// written by hand from the angle, the material only backs the facts up
	if first["angle"] != "momen pertama kali sadar uang habis di akhir bulan" ||
		first["material_url"] != "https://example.com/bahan" {
		t.Fatalf("angle/material not kept: %v", first)
	}
	if got := len(a.list("GET", "/v1/content/topics?status=new", 200)); got != 2 {
		t.Fatalf("filter by status: %d", got)
	}
	if got := len(a.list("GET", "/v1/content/topics?status=used", 200)); got != 0 {
		t.Fatalf("nothing is used yet: %d", got)
	}
	a.want("GET", "/v1/content/topics?status=bogus", nil, 400)
	a.want("PATCH", fmt.Sprintf("/v1/content/topics/%d", topicID), map[string]any{"status": "bogus"}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/content/topics/%d", topicID), map[string]any{}, 400)
	a.want("PATCH", "/v1/content/topics/999999", map[string]any{"status": "skipped"}, 404)
	a.want("PATCH", fmt.Sprintf("/v1/content/topics/%d", topicID), map[string]any{"score": 9.0, "niche": "teknologi"}, 200)
	if got := a.list("GET", "/v1/content/topics?niche=teknologi", 200); len(got) != 1 {
		t.Fatalf("topic niche update: %v", got)
	}

	// a draft needs words and a niche. The fingerprint is NOT demanded any more:
	// the editor in the dashboard writes drafts by hand, so the key is derived
	// from the words when the author did not bring one.
	a.want("POST", "/v1/content/drafts", map[string]any{"niche": "life", "fingerprint": "f1"}, 400)
	a.want("POST", "/v1/content/drafts", map[string]any{"body": "x", "fingerprint": "f1"}, 400)
	a.want("POST", "/v1/content/drafts", map[string]any{"niche": "life", "body": "x",
		"fingerprint": "f1", "platform": "facebook"}, 400)
	byHand := a.obj("POST", "/v1/content/drafts", map[string]any{"niche": "life",
		"topic": "ditulis sendiri", "body": "Isi yang ditulis sendiri di dashboard",
		"material_url": "https://example.com/bahan"}, 201)
	handID := int(byHand["id"].(float64))
	// the same words twice produce the same derived key, so nothing is queued twice
	if code, _ := a.call("POST", "/v1/content/drafts", map[string]any{"niche": "life",
		"topic": "ditulis sendiri", "body": "Isi yang ditulis sendiri di dashboard"}); code != 409 {
		t.Fatalf("derived fingerprint must dedupe: %d", code)
	}
	hand := a.obj("GET", fmt.Sprintf("/v1/content/drafts/%d", handID), nil, 200)
	if hand["fingerprint"] == "" || hand["material_url"] != "https://example.com/bahan" {
		t.Fatalf("hand-written draft: %v", hand)
	}
	a.want("DELETE", fmt.Sprintf("/v1/content/drafts/%d?confirm=true", handID), nil, 200)

	d := a.obj("POST", "/v1/content/drafts", map[string]any{
		"topic_id": topicID, "niche": "keuangan pribadi", "topic": "cara nabung buat pemula",
		"hook": "Hook pertama", "body": "Isi post pertama",
		"parts":       []string{"Isi post pertama", "bagian dua"},
		"fingerprint": "fp-nabung", "model": "deepseek-v4.1-flash"}, 201)
	draftID := int(d["id"].(float64))

	// the topic is now spoken for
	for _, tp := range a.list("GET", "/v1/content/topics?status=used", 200) {
		if int(tp.(map[string]any)["id"].(float64)) != topicID {
			t.Fatalf("wrong topic marked used: %v", tp)
		}
	}

	// the same idea cannot be queued twice
	if code, _ := a.call("POST", "/v1/content/drafts", map[string]any{
		"niche": "life", "topic": "x", "body": "y", "fingerprint": "fp-nabung"}); code != 409 {
		t.Fatalf("duplicate fingerprint: %d", code)
	}

	// review: pending → approved → published, and the words can be fixed first
	pending := a.list("GET", "/v1/content/drafts?status=pending", 200)
	if len(pending) != 1 {
		t.Fatalf("pending drafts: %v", pending)
	}
	row := pending[0].(map[string]any)
	if row["status"] != "pending" || len(row["parts"].([]any)) != 2 || row["hook"] != "Hook pertama" {
		t.Fatalf("draft fields: %v", row)
	}
	got := a.obj("GET", fmt.Sprintf("/v1/content/drafts/%d", draftID), nil, 200)
	if got["model"] != "deepseek-v4.1-flash" || got["published_at"] != "" {
		t.Fatalf("draft detail: %v", got)
	}
	a.want("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{"status": "bogus"}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{"body": "  "}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{}, 400)
	edited := a.obj("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID),
		map[string]any{"body": "Isi yang sudah diperbaiki"}, 200)
	if edited["body"] != "Isi yang sudah diperbaiki" {
		t.Fatalf("edit did not stick: %v", edited)
	}
	a.want("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{"status": "approved"}, 200)
	pub := a.obj("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{"status": "published"}, 200)
	if pub["status"] != "published" || pub["published_at"] == "" {
		t.Fatalf("publishing should stamp the time: %v", pub)
	}

	// runs are recorded even when they find nothing
	a.want("POST", "/v1/content/runs", map[string]any{"slot": "pagi", "topics_found": 0, "drafts_made": 0,
		"note": "Threads keyword search belum disetujui Meta"}, 201)
	a.want("POST", "/v1/content/runs", map[string]any{"topics_found": 0}, 400)
	runs := a.list("GET", "/v1/content/runs", 200)
	if len(runs) != 1 || runs[0].(map[string]any)["slot"] != "pagi" {
		t.Fatalf("runs: %v", runs)
	}

	stats := a.obj("GET", "/v1/content/stats", nil, 200)
	if int(stats["drafts_published"].(float64)) != 1 || int(stats["topics_new"].(float64)) != 1 {
		t.Fatalf("stats: %v", stats)
	}

	// "rapikan dengan AI" is refused before it reaches the engine, and when the
	// engine is down the editor is told which service is missing instead of
	// silently losing the text
	a.want("POST", "/v1/content/polish", map[string]any{"text": "   "}, 400)
	a.want("POST", "/v1/content/polish", map[string]any{"text": strings.Repeat("x", 8001)}, 400)
	// a closed port stands in for "the engine is not running right now" — the
	// editor must be told which service is missing, not lose the text silently
	t.Setenv("INEM_ENGINE_URL", "http://127.0.0.1:9")
	if code, body := a.req("POST", "/v1/content/polish",
		map[string]any{"text": "Halo duni", "niche": "life"}, a.uid, true); code != 502 {
		t.Fatalf("engine down should be reported as 502, got %d %s", code, body)
	}
	// and when it is up, the engine's answer is passed through unchanged: this
	// service holds no LLM key and must not invent one
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/polish" {
			t.Errorf("engine path: %s", r.URL.Path)
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["niche"] != "life" {
			t.Errorf("niche not forwarded: %v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"Halo dunia."}`))
	}))
	defer fake.Close()
	t.Setenv("INEM_ENGINE_URL", fake.URL)
	if code, body := a.req("POST", "/v1/content/polish",
		map[string]any{"text": "Halo duni", "niche": "life"}, a.uid, true); code != 200 {
		t.Fatalf("polish passthrough: %d %s", code, body)
	}

	// deleting needs confirmation, and one member cannot touch another's queue
	a.want("DELETE", fmt.Sprintf("/v1/content/drafts/%d", draftID), nil, 400)
	pipit := mkUser(t, a.conn, 2, "Pipit")
	if _, err := a.conn.Exec(`UPDATE inem_auth.users SET scope='finance' WHERE id=$1`, pipit); err != nil {
		t.Fatalf("scope update: %v", err)
	}
	// the scope gate answers before ownership: a finance member never gets this
	// feature, so she cannot even learn whether a draft id exists
	if code, _ := a.req("GET", fmt.Sprintf("/v1/content/drafts/%d", draftID), nil, pipit, true); code != 403 {
		t.Fatalf("her read of my draft: %d", code)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/content/drafts/%d", draftID), map[string]any{"status": "approved"}, pipit, true); code != 403 {
		t.Fatalf("her edit of my draft: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/content/drafts/%d?confirm=true", draftID), nil, pipit, true); code != 403 {
		t.Fatalf("her delete of my draft: %d", code)
	}
	// a finance-only member does not get this feature at all — not in the UI and
	// not at the API, so nothing depends on a hidden button
	if code, body := a.req("GET", "/v1/content/topics", nil, pipit, true); code != 403 {
		t.Fatalf("her topics should be refused, got %d %s", code, body)
	}
	if code, _ := a.req("POST", "/v1/content/topics", map[string]any{"topic": "x"}, pipit, true); code != 403 {
		t.Fatalf("her topic write should be refused: %d", code)
	}
	if code, _ := a.req("GET", "/v1/content/drafts", nil, pipit, true); code != 403 {
		t.Fatalf("her drafts should be refused: %d", code)
	}
	if code, _ := a.req("GET", "/v1/content/stats", nil, pipit, true); code != 403 {
		t.Fatalf("her stats should be refused: %d", code)
	}
	// and it opens the moment her scope does
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/admin/users/%d", pipit), nil, a.uid, true); code == 200 {
		t.Log("scope change endpoint exists")
	}
	if _, err := a.conn.Exec(`UPDATE inem_auth.users SET scope='full' WHERE id=$1`, pipit); err != nil {
		t.Fatalf("scope update: %v", err)
	}
	if code, body := a.req("GET", "/v1/content/topics", nil, pipit, true); code != 200 {
		t.Fatalf("with full scope it should work: %d %s", code, body)
	}
	a.want("DELETE", fmt.Sprintf("/v1/content/drafts/%d?confirm=true", draftID), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/content/drafts/%d?confirm=true", draftID), nil, 404)
	a.want("DELETE", fmt.Sprintf("/v1/content/topics/%d?confirm=true", topicID), nil, 200)
}
