package main

import (
	"fmt"
	"testing"
)

// The content producer queue: topics in, drafts out, reviewed by a human.
func TestContentTopicsAndDraftsLifecycle(t *testing.T) {
	a := newAPI(t)

	// filing trends: duplicates are reported, not errors
	made := a.obj("POST", "/v1/content/topics", map[string]any{
		"topics": []string{"cara nabung buat pemula", "belajar cloud dari nol"},
		"niche":  "keuangan pribadi", "source": "threads_search", "score": 3.5}, 201)
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

	// a draft needs words, a niche and a fingerprint
	a.want("POST", "/v1/content/drafts", map[string]any{"niche": "life", "fingerprint": "f1"}, 400)
	a.want("POST", "/v1/content/drafts", map[string]any{"body": "x", "fingerprint": "f1"}, 400)
	a.want("POST", "/v1/content/drafts", map[string]any{"niche": "life", "body": "x"}, 400)
	a.want("POST", "/v1/content/drafts", map[string]any{"niche": "life", "body": "x",
		"fingerprint": "f1", "platform": "facebook"}, 400)

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
