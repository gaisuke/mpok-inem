package main

// Content producer: the review queue for original posts.
//
// Division of labour with the generator (viral-content-engine, a separate
// process): that worker searches for trends and writes drafts; the queue of
// record lives here so drafts can be reviewed from chat and the dashboard like
// everything else — and so the engine goes through the API instead of touching
// this database.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type topic struct {
	ID            int64   `json:"id"`
	Topic         string  `json:"topic"`
	Niche         string  `json:"niche"`
	Source        string  `json:"source"`
	Score         float64 `json:"score"`
	Status        string  `json:"status"`
	Angle         string  `json:"angle"`
	MaterialTitle string  `json:"material_title"`
	MaterialURL   string  `json:"material_url"`
	CreatedAt     string  `json:"created_at"`
}

type contentDraft struct {
	ID            int64    `json:"id"`
	TopicID       *int64   `json:"topic_id"`
	Platform      string   `json:"platform"`
	Niche         string   `json:"niche"`
	Topic         string   `json:"topic"`
	Hook          string   `json:"hook"`
	Body          string   `json:"body"`
	Parts         []string `json:"parts"`
	Fingerprint   string   `json:"fingerprint"`
	Status        string   `json:"status"`
	Model         string   `json:"model"`
	MaterialTitle string   `json:"material_title"`
	MaterialURL   string   `json:"material_url"`
	PostID        string   `json:"post_id"`
	RunID         *int64   `json:"run_id"`
	CreatedAt     string   `json:"created_at"`
	PublishedAt   string   `json:"published_at"`
}

// requireFullScope gates features that are not open to a finance-only member yet
// (the content producer). Hiding the tab is not enough on its own: the feature is
// also refused at the API, so "not for her yet" does not depend on a hidden button.
func requireFullScope(w http.ResponseWriter, userID int) bool {
	var scope string
	if err := db.QueryRow(`SELECT scope FROM inem_auth.users WHERE id=$1`, userID).Scan(&scope); err != nil {
		badReq(w, err.Error())
		return false
	}
	if scope != "full" {
		forbidden(w, "fitur konten belum dibuka untuk anggota finance")
		return false
	}
	return true
}

func fullScope(next func(w http.ResponseWriter, r *http.Request, uid int)) http.HandlerFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, uid int) {
		if !requireFullScope(w, uid) {
			return
		}
		next(w, r, uid)
	})
}

func fullScopeID(next func(w http.ResponseWriter, r *http.Request, uid, id int)) http.HandlerFunc {
	return withUserID(func(w http.ResponseWriter, r *http.Request, uid, id int) {
		if !requireFullScope(w, uid) {
			return
		}
		next(w, r, uid, id)
	})
}

// listTopics: what the generator has found (or what was filed by hand).
func listTopics(w http.ResponseWriter, r *http.Request, userID int) {
	args := []any{userID}
	where := "user_id=$1"
	if s := r.URL.Query().Get("status"); s != "" {
		if !validTopicStatus(s) {
			badReq(w, "status must be new|used|skipped")
			return
		}
		args = append(args, s)
		where += fmt.Sprintf(" AND status=$%d", len(args))
	}
	if n := r.URL.Query().Get("niche"); n != "" {
		args = append(args, n)
		where += fmt.Sprintf(" AND niche=$%d", len(args))
	}
	limit, msg := limitParam(r, 50, 200)
	if msg != "" {
		badReq(w, msg)
		return
	}
	args = append(args, limit)
	rows, err := db.Query(fmt.Sprintf(`SELECT id, topic, niche, source, score, status,
		COALESCE(angle,''), COALESCE(material_title,''), COALESCE(material_url,''), created_at
		FROM content.topics WHERE %s ORDER BY score DESC, id DESC LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []topic{}
	for rows.Next() {
		var t topic
		var ts time.Time
		if err := rows.Scan(&t.ID, &t.Topic, &t.Niche, &t.Source, &t.Score, &t.Status,
			&t.Angle, &t.MaterialTitle, &t.MaterialURL, &ts); err != nil {
			badReq(w, err.Error())
			return
		}
		t.CreatedAt = ts.Format(time.RFC3339)
		out = append(out, t)
	}
	writeJSON(w, 200, out)
}

func validTopicStatus(s string) bool {
	return s == "new" || s == "used" || s == "skipped"
}

func validDraftStatus(s string) bool {
	switch s {
	case "pending", "approved", "rejected", "published", "failed":
		return true
	}
	return false
}

// createTopics: file one or many trends. Duplicates are ignored, not an error —
// the same buzzword showing up twice in a day is normal.
func createTopics(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Topic         string   `json:"topic"`
		Topics        []string `json:"topics"`
		Niche         string   `json:"niche"`
		Source        string   `json:"source"`
		Score         float64  `json:"score"`
		Angle         string   `json:"angle"`
		MaterialTitle string   `json:"material_title"`
		MaterialURL   string   `json:"material_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	items := in.Topics
	if in.Topic != "" {
		items = append(items, in.Topic)
	}
	if len(items) == 0 {
		badReq(w, "send topic or topics")
		return
	}
	if in.Niche == "" {
		in.Niche = "keuangan pribadi"
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	saved, skipped := []topic{}, 0
	for _, raw := range items {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		var t topic
		var ts time.Time
		var inserted bool
		// xmax = 0 means this row was inserted now, not matched by the conflict
		// clause — RETURNING alone cannot tell the two apart
		// On a repeat, the material is filled in only where it is still empty:
		// a later run must not overwrite a better angle with a worse one.
		err := db.QueryRow(`INSERT INTO content.topics(user_id, topic, niche, source, score, angle, material_title, material_url)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (user_id, topic) DO UPDATE SET
				score            = GREATEST(content.topics.score, EXCLUDED.score),
				angle            = COALESCE(content.topics.angle, EXCLUDED.angle),
				material_title   = COALESCE(content.topics.material_title, EXCLUDED.material_title),
				material_url     = COALESCE(content.topics.material_url, EXCLUDED.material_url)
			RETURNING id, topic, niche, source, score, status,
				COALESCE(angle,''), COALESCE(material_title,''), COALESCE(material_url,''), created_at, (xmax = 0)`,
			userID, name, in.Niche, in.Source, in.Score, in.Angle, in.MaterialTitle, in.MaterialURL).
			Scan(&t.ID, &t.Topic, &t.Niche, &t.Source, &t.Score, &t.Status,
				&t.Angle, &t.MaterialTitle, &t.MaterialURL, &ts, &inserted)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		if !inserted {
			skipped++
			continue
		}
		t.CreatedAt = ts.Format(time.RFC3339)
		saved = append(saved, t)
	}
	writeJSON(w, 201, map[string]any{"saved": saved, "already_known": skipped})
}

func updateTopic(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Status        *string  `json:"status"`
		Niche         *string  `json:"niche"`
		Score         *float64 `json:"score"`
		Angle         *string  `json:"angle"`
		MaterialTitle *string  `json:"material_title"`
		MaterialURL   *string  `json:"material_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	sets, args := []string{}, []any{userID, id}
	for _, f := range []struct {
		val *string
		col string
	}{{in.Angle, "angle"}, {in.MaterialTitle, "material_title"}, {in.MaterialURL, "material_url"}} {
		if f.val != nil {
			args = append(args, *f.val)
			sets = append(sets, fmt.Sprintf("%s=$%d", f.col, len(args)))
		}
	}
	if in.Status != nil {
		if !validTopicStatus(*in.Status) {
			badReq(w, "status must be new|used|skipped")
			return
		}
		args = append(args, *in.Status)
		sets = append(sets, fmt.Sprintf("status=$%d", len(args)))
	}
	if in.Niche != nil {
		args = append(args, *in.Niche)
		sets = append(sets, fmt.Sprintf("niche=$%d", len(args)))
	}
	if in.Score != nil {
		args = append(args, *in.Score)
		sets = append(sets, fmt.Sprintf("score=$%d", len(args)))
	}
	if len(sets) == 0 {
		badReq(w, "nothing to update: send status, niche or score")
		return
	}
	res, err := db.Exec(`UPDATE content.topics SET `+strings.Join(sets, ", ")+` WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		notFound(w, "topic not found")
		return
	}
	writeJSON(w, 200, map[string]any{"updated": 1})
}

func deleteTopic(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	res, err := db.Exec(`DELETE FROM content.topics WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "topic not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

const draftSelect = `SELECT d.id, d.topic_id, d.platform, d.niche, d.topic, COALESCE(d.hook,''),
	d.body, d.parts, d.fingerprint, d.status, COALESCE(d.model,''),
	COALESCE(d.material_title,''), COALESCE(d.material_url,''), COALESCE(d.post_id,''),
	d.run_id, d.created_at, d.published_at
	FROM content.drafts d`

func scanDraft(s interface{ Scan(...any) error }) (contentDraft, error) {
	var d contentDraft
	var parts []byte
	var ts time.Time
	var pub sql.NullTime
	err := s.Scan(&d.ID, &d.TopicID, &d.Platform, &d.Niche, &d.Topic, &d.Hook, &d.Body, &parts,
		&d.Fingerprint, &d.Status, &d.Model, &d.MaterialTitle, &d.MaterialURL, &d.PostID,
		&d.RunID, &ts, &pub)
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal(parts, &d.Parts)
	if len(d.Parts) == 0 {
		d.Parts = []string{d.Body}
	}
	d.CreatedAt = ts.Format(time.RFC3339)
	if pub.Valid {
		d.PublishedAt = pub.Time.Format(time.RFC3339)
	}
	return d, nil
}

// listDrafts: the review queue. ?status=pending is what the digest asks for.
func listDrafts(w http.ResponseWriter, r *http.Request, userID int) {
	args := []any{userID}
	where := "d.user_id=$1"
	if s := r.URL.Query().Get("status"); s != "" {
		if !validDraftStatus(s) {
			badReq(w, "status must be pending|approved|rejected|published|failed")
			return
		}
		args = append(args, s)
		where += fmt.Sprintf(" AND d.status=$%d", len(args))
	}
	if n := r.URL.Query().Get("niche"); n != "" {
		args = append(args, n)
		where += fmt.Sprintf(" AND d.niche=$%d", len(args))
	}
	limit, msg := limitParam(r, 20, 200)
	if msg != "" {
		badReq(w, msg)
		return
	}
	args = append(args, limit)
	rows, err := db.Query(fmt.Sprintf(`%s WHERE %s ORDER BY d.id DESC LIMIT $%d`, draftSelect, where, len(args)), args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []contentDraft{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, d)
	}
	writeJSON(w, 200, out)
}

func getDraft(w http.ResponseWriter, r *http.Request, userID, id int) {
	d, err := scanDraft(db.QueryRow(draftSelect+` WHERE d.user_id=$1 AND d.id=$2`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "draft not found")
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, d)
}

// createDraft: how the generator files what it wrote. The fingerprint is unique
// per member, so the same idea cannot be queued twice by two runs.
func createDraft(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		TopicID       *int64   `json:"topic_id"`
		Platform      string   `json:"platform"`
		Niche         string   `json:"niche"`
		Topic         string   `json:"topic"`
		Hook          string   `json:"hook"`
		Body          string   `json:"body"`
		Parts         []string `json:"parts"`
		Fingerprint   string   `json:"fingerprint"`
		Model         string   `json:"model"`
		MaterialTitle string   `json:"material_title"`
		MaterialURL   string   `json:"material_url"`
		RunID         *int64   `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		badReq(w, "body required")
		return
	}
	if in.Platform == "" {
		in.Platform = "threads"
	}
	if in.Platform != "threads" && in.Platform != "x" {
		badReq(w, "platform must be threads|x")
		return
	}
	if strings.TrimSpace(in.Niche) == "" {
		badReq(w, "niche required")
		return
	}
	if len(in.Parts) == 0 {
		in.Parts = []string{in.Body}
	}
	if strings.TrimSpace(in.Fingerprint) == "" {
		// Written by hand in the dashboard: the de-duplication key is derived
		// here instead of being demanded from the author.
		in.Fingerprint = fingerprintOf(in.Body)
	}
	parts, _ := json.Marshal(in.Parts)
	var id int64
	err := db.QueryRow(`INSERT INTO content.drafts(user_id, topic_id, platform, niche, topic, hook, body, parts, fingerprint, model, material_title, material_url, run_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		userID, in.TopicID, in.Platform, in.Niche, in.Topic, in.Hook, in.Body, parts,
		in.Fingerprint, in.Model, in.MaterialTitle, in.MaterialURL, in.RunID).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, 409, map[string]any{"error": "draft with this fingerprint already exists"})
			return
		}
		badReq(w, err.Error())
		return
	}
	// the topic is now spoken for
	if in.TopicID != nil {
		_, _ = db.Exec(`UPDATE content.topics SET status='used' WHERE user_id=$1 AND id=$2 AND status='new'`, userID, *in.TopicID)
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

// updateDraft: decide on a draft, or fix its words before publishing. Approving
// is a separate step from publishing on purpose: nothing goes out by accident.
func updateDraft(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Status *string   `json:"status"`
		Body   *string   `json:"body"`
		Parts  *[]string `json:"parts"`
		PostID *string   `json:"post_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	sets, args := []string{}, []any{userID, id}
	if in.Status != nil {
		if !validDraftStatus(*in.Status) {
			badReq(w, "status must be pending|approved|rejected|published|failed")
			return
		}
		args = append(args, *in.Status)
		sets = append(sets, fmt.Sprintf("status=$%d", len(args)))
		if *in.Status == "published" {
			sets = append(sets, "published_at=now()")
		}
	}
	if in.Body != nil {
		if strings.TrimSpace(*in.Body) == "" {
			badReq(w, "body must not be empty")
			return
		}
		args = append(args, *in.Body)
		sets = append(sets, fmt.Sprintf("body=$%d", len(args)))
	}
	if in.Parts != nil {
		parts, _ := json.Marshal(*in.Parts)
		args = append(args, parts)
		sets = append(sets, fmt.Sprintf("parts=$%d", len(args)))
	}
	if in.PostID != nil {
		args = append(args, *in.PostID)
		sets = append(sets, fmt.Sprintf("post_id=$%d", len(args)))
	}
	if len(sets) == 0 {
		badReq(w, "nothing to update: send status, body, parts or post_id")
		return
	}
	res, err := db.Exec(`UPDATE content.drafts SET `+strings.Join(sets, ", ")+` WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		notFound(w, "draft not found")
		return
	}
	if d, err := scanDraft(db.QueryRow(draftSelect+` WHERE d.user_id=$1 AND d.id=$2`, userID, id)); err == nil {
		writeJSON(w, 200, d)
		return
	}
	writeJSON(w, 200, map[string]any{"updated": 1})
}

// fingerprintOf is the engine's de-duplication key, reimplemented so a draft
// typed straight into the dashboard is deduplicated the same way as a generated
// one: sorted, unique, stopword-free tokens, hashed. Kept identical to
// app/content_generator.py:fingerprint on purpose.
var fpToken = regexp.MustCompile(`[a-z0-9]+`)

var fpStopwords = map[string]bool{
	"yang": true, "dan": true, "di": true, "ke": true, "dari": true, "itu": true,
	"ini": true, "aku": true, "saya": true, "kamu": true, "kita": true,
	"untuk": true, "dengan": true, "tidak": true, "gak": true, "ga": true,
	"ada": true, "juga": true, "bisa": true, "akan": true, "atau": true,
	"the": true, "a": true, "an": true, "to": true, "of": true, "is": true,
	"it": true, "and": true, "in": true, "on": true, "for": true, "my": true, "i": true,
}

func fingerprintOf(text string) string {
	seen := map[string]bool{}
	tokens := []string{}
	for _, t := range fpToken.FindAllString(strings.ToLower(text), -1) {
		if len(t) > 2 && !fpStopwords[t] && !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}
	sort.Strings(tokens)
	sum := sha1.Sum([]byte(strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])[:16]
}

// polishContent: "rapikan dengan AI" in the editor. The words stay the author's —
// the engine only fixes spelling, spacing and flow, and is told not to rewrite.
// The call is forwarded to the engine because that is where the voice prompts and
// the model credentials live; this service holds no LLM key.
func polishContent(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Text  string `json:"text"`
		Niche string `json:"niche"`
		Mode  string `json:"mode"` // "" = rapikan | "hook" = usulkan hook
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		badReq(w, "text required")
		return
	}
	if len(in.Text) > 8000 {
		badReq(w, "text too long (max 8000 chars)")
		return
	}
	engine := os.Getenv("INEM_ENGINE_URL")
	if engine == "" {
		engine = "http://127.0.0.1:8000"
	}
	payload, _ := json.Marshal(map[string]string{"text": in.Text, "niche": in.Niche, "mode": in.Mode})
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(engine, "/")+"/polish", bytes.NewReader(payload))
	if err != nil {
		badReq(w, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "engine tidak bisa dihubungi (" + engine + "): " + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("engine menolak (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func deleteDraft(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	res, err := db.Exec(`DELETE FROM content.drafts WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "draft not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// createRun: one row per scheduled run, so "it ran and found nothing" is
// distinguishable from "the job never fired".
func createRun(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Slot        string `json:"slot"`
		TopicsFound int    `json:"topics_found"`
		DraftsMade  int    `json:"drafts_made"`
		Note        string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Slot) == "" {
		badReq(w, "slot required (pagi|siang|sore)")
		return
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO content.runs(user_id, slot, topics_found, drafts_made, note)
		VALUES($1,$2,$3,$4,$5) RETURNING id`,
		userID, in.Slot, in.TopicsFound, in.DraftsMade, in.Note).Scan(&id); err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func listRuns(w http.ResponseWriter, r *http.Request, userID int) {
	limit, msg := limitParam(r, 20, 100)
	if msg != "" {
		badReq(w, msg)
		return
	}
	rows, err := db.Query(`SELECT id, slot, started_at, topics_found, drafts_made, COALESCE(note,'')
		FROM content.runs WHERE user_id=$1 ORDER BY id DESC LIMIT $2`, userID, limit)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var slot, note string
		var started time.Time
		var found, made int
		if err := rows.Scan(&id, &slot, &started, &found, &made, &note); err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "slot": slot, "started_at": started.Format(time.RFC3339),
			"topics_found": found, "drafts_made": made, "note": note})
	}
	writeJSON(w, 200, out)
}

func contentStats(w http.ResponseWriter, r *http.Request, userID int) {
	var topicsNew, draftsPending, draftsApproved, draftsPublished int
	counts := []struct {
		query string
		dest  *int
	}{
		{`SELECT count(*) FROM content.topics WHERE user_id=$1 AND status='new'`, &topicsNew},
		{`SELECT count(*) FROM content.drafts WHERE user_id=$1 AND status='pending'`, &draftsPending},
		{`SELECT count(*) FROM content.drafts WHERE user_id=$1 AND status='approved'`, &draftsApproved},
		{`SELECT count(*) FROM content.drafts WHERE user_id=$1 AND status='published'`, &draftsPublished},
	}
	for _, c := range counts {
		if err := db.QueryRow(c.query, userID).Scan(c.dest); err != nil {
			badReq(w, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"topics_new": topicsNew, "drafts_pending": draftsPending,
		"drafts_approved": draftsApproved, "drafts_published": draftsPublished,
	})
}
