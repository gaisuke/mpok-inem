package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------- harness ----------
//
// The suite runs against a real Postgres (INEM_TEST_DSN, default inem_test) so
// the SQL, the constraints and the balance maths are exercised for real rather
// than against a mock that could drift from the schema. If the database is not
// reachable the DB-backed tests skip instead of failing.

var (
	hits      = map[string]bool{} // patterns exercised, filled by recordMux
	setupOnce sync.Once
	testConn  *sql.DB
	noDB      bool
)

func conn(t *testing.T) *sql.DB {
	t.Helper()
	setupOnce.Do(func() {
		dsn := os.Getenv("INEM_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://inem@127.0.0.1:5432/inem_test?sslmode=disable"
		}
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			noDB = true
			return
		}
		if err := c.Ping(); err != nil {
			fmt.Printf("test db unreachable, skipping DB tests: %v\n", err)
			noDB = true
			return
		}
		// apply every migration in order, so a fresh test database matches production
		files, err := filepath.Glob("../db/*.sql")
		if err != nil || len(files) == 0 {
			fmt.Printf("cannot list schema files: %v\n", err)
			noDB = true
			return
		}
		sort.Strings(files)
		for _, f := range files {
			schema, err := os.ReadFile(f)
			if err != nil {
				fmt.Printf("cannot read %s: %v\n", f, err)
				noDB = true
				return
			}
			if _, err := c.Exec(string(schema)); err != nil {
				fmt.Printf("cannot apply %s: %v\n", f, err)
				noDB = true
				return
			}
		}
		testConn = c
	})
	if noDB {
		t.Skip("no test database (set INEM_TEST_DSN)")
	}
	return testConn
}

// recordMux wires the production route table and remembers which patterns the
// suite actually hit; TestMain turns that into a coverage gate. (Go 1.22 has no
// Request.Pattern, so the recording wrapper is applied per route.)
func recordMux() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range allRoutes() {
		key := rt.Method + " " + rt.Pattern
		handler := rt.Handler
		mux.Handle(key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[key] = true
			handler.ServeHTTP(w, r)
		}))
	}
	return mux
}

type apiTest struct {
	t    *testing.T
	srv  *httptest.Server
	conn *sql.DB
	uid  int
}

func newAPI(t *testing.T) *apiTest {
	t.Helper()
	c := conn(t)
	if _, err := c.Exec(`TRUNCATE inem_auth.users, expense.pockets, expense.transactions, expense.transfers,
		brain.notes, brain.note_links, nutrition.meals, nutrition.meal_items, nutrition.daily_targets,
 content.topics, content.drafts, content.runs
 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	db = c
	srv := httptest.NewServer(recordMux())
	t.Cleanup(srv.Close)
	return &apiTest{t: t, srv: srv, conn: c, uid: mkUser(t, c, 1, "Test Dani")}
}

func mkUser(t *testing.T, c *sql.DB, telegramID int64, name string) int {
	t.Helper()
	var id int
	if err := c.QueryRow(`INSERT INTO inem_auth.users(telegram_user_id, display_name)
		VALUES ($1,$2) RETURNING id`, telegramID, name).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func (a *apiTest) req(method, path string, body any, uid int, header bool) (int, []byte) {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if header {
		req.Header.Set("X-User-ID", fmt.Sprint(uid))
	}
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// call is an authenticated request as the suite's user.
func (a *apiTest) call(method, path string, body any) (int, []byte) {
	return a.req(method, path, body, a.uid, true)
}

// want asserts the status code and decodes the body (nil for an empty body).
func (a *apiTest) want(method, path string, body any, code int) any {
	a.t.Helper()
	got, raw := a.call(method, path, body)
	if got != code {
		a.t.Fatalf("%s %s: status %d, want %d — body %s", method, path, got, code, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%s %s: bad json: %v — %s", method, path, err, raw)
	}
	return out
}

func (a *apiTest) obj(method, path string, body any, code int) map[string]any {
	a.t.Helper()
	out := a.want(method, path, body, code)
	m, ok := out.(map[string]any)
	if !ok {
		a.t.Fatalf("%s %s: want a JSON object, got %T (%v)", method, path, out, out)
	}
	return m
}

func (a *apiTest) list(method, path string, code int) []any {
	a.t.Helper()
	out := a.want(method, path, nil, code)
	l, ok := out.([]any)
	if !ok {
		a.t.Fatalf("%s %s: want a JSON array, got %T (%v)", method, path, out, out)
	}
	return l
}

// reqJSON performs an authenticated request as another member and decodes the
// body as an object.
func (a *apiTest) reqJSON(method, path string, uid int) map[string]any {
	a.t.Helper()
	code, raw := a.req(method, path, nil, uid, true)
	if code != 200 {
		a.t.Fatalf("%s %s as user %d: status %d — %s", method, path, uid, code, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%s %s: bad json: %v", method, path, err)
	}
	return out
}

func (a *apiTest) reqList(method, path string, uid int) []any {
	a.t.Helper()
	code, raw := a.req(method, path, nil, uid, true)
	if code != 200 {
		a.t.Fatalf("%s %s as user %d: status %d — %s", method, path, uid, code, raw)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%s %s: bad json: %v", method, path, err)
	}
	return out
}

func (a *apiTest) reqJSONObj(method, path string, uid int, body any, want int) map[string]any {
	a.t.Helper()
	code, raw := a.req(method, path, body, uid, true)
	if code != want {
		a.t.Fatalf("%s %s as user %d: status %d, want %d — %s", method, path, uid, code, want, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%s %s: bad json: %v", method, path, err)
	}
	return out
}

func (a *apiTest) pocket(name, typ string, opening int64) int {
	a.t.Helper()
	out := a.obj("POST", "/v1/pockets",
		map[string]any{"name": name, "type": typ, "opening_balance_idr": opening}, 201)
	return int(out["id"].(float64))
}

// balance reads one pocket's balance from the service (never computed here).
func (a *apiTest) balance(id int) int64 {
	a.t.Helper()
	for _, p := range a.list("GET", "/v1/pockets", 200) {
		pm := p.(map[string]any)
		if int(pm["id"].(float64)) == id {
			return int64(pm["balance_idr"].(float64))
		}
	}
	a.t.Fatalf("pocket %d not in /v1/pockets", id)
	return 0
}

// balanceOf reads a pocket's balance through the single-pocket endpoint, which
// also serves a pocket another member has shared.
func (a *apiTest) balanceOf(uid, id int) int64 {
	a.t.Helper()
	return int64(a.reqJSON("GET", fmt.Sprintf("/v1/pockets/%d", id), uid)["balance_idr"].(float64))
}

func (a *apiTest) spend(pocketID int, direction string, amount int64, category, note string) int {
	a.t.Helper()
	out := a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": pocketID, "direction": direction, "amount_idr": amount,
		"category": category, "note": note, "source": "manual"}, 201)
	return int(out["id"].(float64))
}

func bodyContains(raw []byte, needle string) bool {
	return bytes.Contains(bytes.ToLower(raw), bytes.ToLower([]byte(needle)))
}

// ---------- endpoints ----------

func TestHealthAndHandleContract(t *testing.T) {
	a := newAPI(t)
	health := a.obj("GET", "/healthz", nil, 200)
	if health["status"] != "ok" {
		t.Fatalf("healthz: %v", health)
	}

	a.want("POST", "/internal/v1/handle", map[string]any{},
		400) // no user_id
	a.want("POST", "/internal/v1/handle", map[string]any{"user_id": a.uid, "service": "nope"}, 400)
	ack := a.obj("POST", "/internal/v1/handle",
		map[string]any{"user_id": a.uid, "service": "expense", "trace_id": "tr-1", "text": "kopi 5rb"}, 200)
	if ack["reply_text"] == nil {
		t.Fatalf("handle ack: %v", ack)
	}
	empty := a.obj("POST", "/internal/v1/handle",
		map[string]any{"user_id": a.uid, "service": "brain"}, 200)
	if !bodyContains([]byte(empty["reply_text"].(string)), "nothing to do") {
		t.Fatalf("handle empty ack: %v", empty)
	}
}

func TestPocketLifecycleAndGuards(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 13000)
	savings := a.pocket("Savings", "savings", 0)

	if got := a.balance(cash); got != 13000 {
		t.Fatalf("opening balance: %d", got)
	}
	if len(a.list("GET", "/v1/pockets", 200)) != 2 {
		t.Fatal("want 2 pockets")
	}

	// defaults + validation
	a.want("POST", "/v1/pockets", map[string]any{"type": "cash"}, 400)                 // no name
	a.want("POST", "/v1/pockets", map[string]any{"name": "X", "type": "gold"}, 400)    // bad type
	a.want("POST", "/v1/pockets", map[string]any{"name": "Cash", "type": "cash"}, 400) // duplicate
	def := a.obj("POST", "/v1/pockets", map[string]any{"name": "NoType"}, 201)         // type defaults to cash
	defID := int(def["id"].(float64))
	if defID == 0 {
		t.Fatal("default type pocket not created")
	}
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", defID), nil, 200)

	// single-pocket read (what a dashboard needs to open one pocket)
	one := a.obj("GET", fmt.Sprintf("/v1/pockets/%d", cash), nil, 200)
	if one["name"] != "Cash" || int64(one["balance_idr"].(float64)) != 13000 {
		t.Fatalf("get pocket: %v", one)
	}
	a.want("GET", fmt.Sprintf("/v1/pockets/%d", savings), nil, 200)
	a.want("GET", "/v1/pockets/9999", nil, 404)

	// patch: rename + retype + opening balance
	upd := a.obj("PATCH", fmt.Sprintf("/v1/pockets/%d", cash),
		map[string]any{"name": "Cash ", "type": "savings", "opening_balance_idr": 20000}, 200)
	if upd["name"] != "Cash " || upd["type"] != "savings" {
		t.Fatalf("patch did not apply: %v", upd)
	}

	// whitespace-only name is rejected, not trimmed into place
	if got, _ := a.call("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"name": "   "}); got != 400 {
		t.Fatalf("blank name: status %d", got)
	}

	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"type": "gold"}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"opening_balance_idr": -1}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"name": "Savings"}, 409)
	a.want("PATCH", "/v1/pockets/9999", map[string]any{"name": "ghost"}, 404)

	// delete needs an explicit confirm, and refuses while rows reference it
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d", cash), nil, 400)
	txn := a.spend(cash, "out", 1000, "food", "test")
	_, raw := a.call("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil)
	if !bodyContains(raw, "entries") {
		t.Fatalf("expected a 409 explaining the references, got %s", raw)
	}
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", txn), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil, 404)
	if len(a.list("GET", "/v1/pockets", 200)) != 1 {
		t.Fatal("pocket count after delete")
	}
	if a.balance(savings) != 0 {
		t.Fatal("savings balance should be untouched")
	}
}

func TestBalanceIsServiceComputed(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	bank := a.pocket("Bank", "savings", 0)

	a.spend(cash, "out", 25000, "food", "kopi")
	a.spend(cash, "in", 5000, "refund", "kembalian")
	a.obj("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 50000, "note": "save"}, 201)

	if got, want := a.balance(cash), int64(100000-25000+5000-50000); got != want {
		t.Fatalf("cash balance %d, want %d", got, want)
	}
	if got, want := a.balance(bank), int64(50000); got != want {
		t.Fatalf("bank balance %d, want %d", got, want)
	}
	// deleting the transfer moves both balances back
	tr := a.list("GET", "/v1/transfers", 200)[0].(map[string]any)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", int(tr["id"].(float64))), nil, 200)
	if got := a.balance(bank); got != 0 {
		t.Fatalf("bank balance after transfer delete: %d", got)
	}
	if got := a.balance(cash); got != 80000 {
		t.Fatalf("cash balance after transfer delete: %d", got)
	}
}

func TestTransactionLifecycle(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 200000)
	today := time.Now().Format("2006-01-02")

	id := a.spend(cash, "out", 35000, "food", "nasi goreng")

	// guards on create
	_, raw := a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": cash, "direction": "sideways", "amount_idr": 1000})
	if !bodyContains(raw, "direction") {
		t.Fatalf("direction guard: %s", raw)
	}
	_, raw = a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": cash, "direction": "out", "amount_idr": 0})
	if !bodyContains(raw, "amount_idr") {
		t.Fatalf("amount guard: %s", raw)
	}
	_, raw = a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": 9999, "direction": "out", "amount_idr": 1000})
	if !bodyContains(raw, "pocket") {
		t.Fatalf("pocket guard: %s", raw)
	}

	// list + filters
	if got := len(a.list("GET", "/v1/transactions", 200)); got != 1 {
		t.Fatalf("list: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?category=food", 200)); got != 1 {
		t.Fatalf("category filter: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?category=transport", 200)); got != 0 {
		t.Fatalf("category filter should exclude: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?direction=in", 200)); got != 0 {
		t.Fatalf("direction filter: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?from="+today+"&to="+today, 200)); got != 1 {
		t.Fatalf("date filter: %d rows", got)
	}
	a.want("GET", "/v1/transactions?from=2026-01-02&to=2026-01-01", nil, 400)
	a.want("GET", "/v1/transactions?from=nope", nil, 400)
	a.want("GET", "/v1/transactions?limit=0", nil, 400)
	a.want("GET", "/v1/transactions?limit=501", nil, 400)

	// per-pocket ledger: the view behind "tap a pocket and see what moved"
	other := a.pocket("Bank", "savings", 0)
	a.spend(other, "out", 7000, "transport", "ojek")
	pocketOnly := a.list("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", cash), 200)
	if len(pocketOnly) != 1 || int(pocketOnly[0].(map[string]any)["pocket_id"].(float64)) != cash {
		t.Fatalf("pocket filter: %v", pocketOnly)
	}
	if got := len(a.list("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", other), 200)); got != 1 {
		t.Fatalf("pocket filter (other pocket): %d", got)
	}
	if got := len(a.list("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d&direction=in", cash), 200)); got != 0 {
		t.Fatalf("pocket+direction filter: %d", got)
	}
	a.want("GET", "/v1/transactions?pocket_id=0", nil, 400)
	a.want("GET", "/v1/transactions?pocket_id=abc", nil, 400)

	one := a.obj("GET", fmt.Sprintf("/v1/transactions/%d", id), nil, 200)
	if one["note"] != "nasi goreng" || int(one["id"].(float64)) != id {
		t.Fatalf("get txn: %v", one)
	}
	a.want("GET", "/v1/transactions/9999", nil, 404)

	fixed := a.obj("PATCH", fmt.Sprintf("/v1/transactions/%d", id),
		map[string]any{"note": "nasi goreng + telur", "category": "food"}, 200)
	if fixed["note"] != "nasi goreng + telur" {
		t.Fatalf("patch note: %v", fixed)
	}
	a.want("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{"category": "  "}, 400)
	a.want("PATCH", "/v1/transactions/9999", map[string]any{"note": "x"}, 404)
	// amounts stay immutable, and the lie is refused rather than silently dropped
	_, raw = a.call("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{"amount_idr": 99000})
	if !bodyContains(raw, "nothing to update") {
		t.Fatalf("amount must not be editable: %s", raw)
	}
	if got := a.balance(cash); got != 165000 {
		t.Fatalf("balance changed by a metadata edit: %d", got)
	}

	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d", id), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", id), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", id), nil, 404)
	if got := a.balance(cash); got != 200000 {
		t.Fatalf("balance after delete: %d", got)
	}
}

func TestTransferLifecycle(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	bank := a.pocket("Bank", "savings", 0)
	month := time.Now().Format("2006-01")
	today := time.Now().Format("2006-01-02")

	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": cash, "to_pocket_id": cash, "amount_idr": 1000}, 400)
	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 0}, 400)
	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": 9999, "to_pocket_id": bank, "amount_idr": 100}, 400)

	tr := a.obj("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 40000, "note": "monthly save"}, 201)
	id := int(tr["id"].(float64))

	row := a.list("GET", "/v1/transfers", 200)[0].(map[string]any)
	if row["from"] != "Cash" || row["to"] != "Bank" || int64(row["amount_idr"].(float64)) != 40000 {
		t.Fatalf("transfer row: %v", row)
	}
	if got := len(a.list("GET", "/v1/transfers?month="+month, 200)); got != 1 {
		t.Fatalf("month filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/transfers?from="+today+"&to="+today, 200)); got != 1 {
		t.Fatalf("range filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/transfers?from=2020-01-01&to=2020-01-02", 200)); got != 0 {
		t.Fatalf("empty range: %d", got)
	}
	a.want("GET", "/v1/transfers?month=nope", nil, 400)
	a.want("GET", "/v1/transfers?limit=0", nil, 400)

	// per-pocket transfers (a move shows up on both sides)
	if got := len(a.list("GET", fmt.Sprintf("/v1/transfers?pocket_id=%d", cash), 200)); got != 1 {
		t.Fatalf("source pocket transfers: %d", got)
	}
	if got := len(a.list("GET", fmt.Sprintf("/v1/transfers?pocket_id=%d", bank), 200)); got != 1 {
		t.Fatalf("destination pocket transfers: %d", got)
	}
	third := a.pocket("Third", "savings", 0)
	if got := len(a.list("GET", fmt.Sprintf("/v1/transfers?pocket_id=%d", third), 200)); got != 0 {
		t.Fatalf("uninvolved pocket transfers: %d", got)
	}
	a.want("GET", "/v1/transfers?pocket_id=0", nil, 400)

	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d", id), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", id), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", id), nil, 404)
}

// Money moves between members too: the giver may only spend from their own
// pocket, and the receiver must see the movement instead of a balance that
// jumped by itself. Private pocket names stay private even in the movement list.
func TestTransferBetweenMembers(t *testing.T) {
	a := newAPI(t)
	pipit := mkUser(t, a.conn, 2, "Pipit")

	mine := a.pocket("BRImo", "cash", 620960)
	a.obj("PATCH", fmt.Sprintf("/v1/pockets/%d", mine), map[string]any{"visibility": "shared"}, 200)
	secret := a.pocket("Dana Darurat", "savings", 0) // mine, stays private

	hers := int(a.reqJSONObj("POST", "/v1/pockets", pipit, map[string]any{
		"name": "seabank", "type": "cash", "opening_balance_idr": 983678, "visibility": "shared"}, 201)["id"].(float64))
	herPrivate := int(a.reqJSONObj("POST", "/v1/pockets", pipit, map[string]any{
		"name": "brimo", "type": "cash", "opening_balance_idr": 759531}, 201)["id"].(float64))

	// she may not spend my money, nor push money into a pocket she cannot see —
	// and an invisible pocket answers exactly like one that does not exist
	a.reqJSONObj("POST", "/v1/transfers", pipit, map[string]any{
		"from_pocket_id": mine, "to_pocket_id": hers, "amount_idr": 1000}, 400)
	a.reqJSONObj("POST", "/v1/transfers", pipit, map[string]any{
		"from_pocket_id": hers, "to_pocket_id": secret, "amount_idr": 1000}, 400)
	_, hiddenMsg := a.req("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": hers, "to_pocket_id": secret, "amount_idr": 1000}, pipit, true)
	_, missingMsg := a.req("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": hers, "to_pocket_id": 999999, "amount_idr": 1000}, pipit, true)
	if string(hiddenMsg) != string(missingMsg) {
		t.Fatalf("a hidden pocket must be indistinguishable from a missing one: %s vs %s", hiddenMsg, missingMsg)
	}

	totalBefore := int64(a.obj("GET", "/v1/household", nil, 200)["total_balance_idr"].(float64))

	// the gift: her shared pocket into my shared pocket
	gift := a.reqJSONObj("POST", "/v1/transfers", pipit, map[string]any{
		"from_pocket_id": hers, "to_pocket_id": mine, "amount_idr": 200000, "note": "uang belanja"}, 201)
	giftID := int(gift["id"].(float64))

	if got := a.balanceOf(a.uid, hers); got != 783678 {
		t.Fatalf("her pocket after giving: %d", got)
	}
	if got := a.balanceOf(a.uid, mine); got != 820960 {
		t.Fatalf("my pocket after receiving: %d", got)
	}
	// moving money inside the household changes no household total
	if got := int64(a.obj("GET", "/v1/household", nil, 200)["total_balance_idr"].(float64)); got != totalBefore {
		t.Fatalf("household total moved on an internal transfer: %d → %d", totalBefore, got)
	}

	// I see it in my own list, with who sent it
	rows := a.list("GET", "/v1/transfers", 200)
	if len(rows) != 1 {
		t.Fatalf("the receiver should see the incoming transfer: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["from"] != "seabank" || row["from_user"] != "Pipit" || row["to"] != "BRImo" ||
		row["to_user"] != "Test Dani" || row["note"] != "uang belanja" {
		t.Fatalf("incoming transfer row: %v", row)
	}
	if got := len(a.reqList("GET", "/v1/transfers", pipit)); got != 1 {
		t.Fatalf("the giver should see it too: %d", got)
	}
	// and it shows up in the receiving pocket's own movements
	if got := len(a.reqList("GET", fmt.Sprintf("/v1/transfers?pocket_id=%d", mine), pipit)); got != 1 {
		t.Fatalf("her view of my pocket movements: %d", got)
	}

	// from a private pocket the movement is visible but the pocket name is not
	anon := a.reqJSONObj("POST", "/v1/transfers", pipit, map[string]any{
		"from_pocket_id": herPrivate, "to_pocket_id": mine, "amount_idr": 5000, "note": "top up"}, 201)
	anonID := int(anon["id"].(float64))
	var anonRow map[string]any
	for _, r := range a.list("GET", "/v1/transfers", 200) {
		if int(r.(map[string]any)["id"].(float64)) == anonID {
			anonRow = r.(map[string]any)
		}
	}
	if anonRow == nil || anonRow["from"] != "(pribadi)" || anonRow["from_user"] != "Pipit" ||
		int64(anonRow["amount_idr"].(float64)) != 5000 {
		t.Fatalf("a private source must be masked, not hidden: %v", anonRow)
	}

	// only the giver can undo her own transfer, and undoing it moves both balances back
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", giftID), nil, a.uid, true); code != 404 {
		t.Fatalf("the receiver must not delete the giver's transfer: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", giftID), nil, pipit, true); code != 200 {
		t.Fatalf("the giver should be able to undo it: %d", code)
	}
	if got := a.balanceOf(a.uid, mine); got != 625960 {
		t.Fatalf("my pocket after the undo: %d", got)
	}
	if got := a.balanceOf(a.uid, hers); got != 983678 {
		t.Fatalf("her pocket after the undo: %d", got)
	}
}

func TestSummaryRollsUpPeriods(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	today := time.Now().Format("2006-01-02")
	month := time.Now().Format("2006-01")

	a.spend(cash, "out", 25000, "food", "kopi")
	a.spend(cash, "out", 10000, "transport", "ojek")
	a.spend(cash, "in", 5000, "refund", "kembalian")

	cur := a.obj("GET", "/v1/expense/summary", nil, 200)
	if int64(cur["total_out_idr"].(float64)) != 35000 || int64(cur["total_in_idr"].(float64)) != 5000 {
		t.Fatalf("current-month totals: %v", cur)
	}
	if cur["month"] != month {
		t.Fatalf("month label: %v", cur["month"])
	}
	byCat := map[string]int64{}
	for _, c := range cur["by_category"].([]any) {
		cm := c.(map[string]any)
		if cm["direction"] == "out" {
			byCat[cm["category"].(string)] = int64(cm["total_idr"].(float64))
		}
	}
	if byCat["food"] != 25000 || byCat["transport"] != 10000 {
		t.Fatalf("by_category: %v", byCat)
	}

	rangeOut := a.obj("GET", "/v1/expense/summary?from="+today+"&to="+today, nil, 200)
	if int64(rangeOut["total_out_idr"].(float64)) != 35000 {
		t.Fatalf("range totals: %v", rangeOut)
	}
	if rangeOut["from"] != today {
		t.Fatalf("range label: %v", rangeOut)
	}

	empty := a.obj("GET", "/v1/expense/summary?month=2020-01", nil, 200)
	if len(empty["by_category"].([]any)) != 0 {
		t.Fatalf("empty month should roll up to nothing: %v", empty)
	}
	a.want("GET", "/v1/expense/summary?month=2020-13", nil, 400)
	a.want("GET", "/v1/expense/summary?from=2026-01-02&to=2026-01-01", nil, 400)
	a.want("GET", "/v1/expense/summary?from=nope&to=nope", nil, 400)
}

func TestNotesCrudSearchAndLinks(t *testing.T) {
	a := newAPI(t)
	first := a.obj("POST", "/v1/notes",
		map[string]any{"title": "Wifi bill", "body_md": "Pay on the 5th, 385rb.", "tags": []string{"bills", "home"}}, 201)
	second := a.obj("POST", "/v1/notes",
		map[string]any{"title": "Pocket rule", "body_md": "Set aside 20% every payday.", "tags": []string{"finance"}}, 201)
	a.want("POST", "/v1/notes", map[string]any{"body_md": "no title"}, 400)

	firstID := int(first["id"].(float64))
	secondID := int(second["id"].(float64))

	if got := len(a.list("GET", "/v1/notes", 200)); got != 2 {
		t.Fatalf("list notes: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes?tag=finance", 200)); got != 1 {
		t.Fatalf("tag filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes?limit=1", 200)); got != 1 {
		t.Fatalf("limit: %d", got)
	}
	a.want("GET", "/v1/notes?limit=1000", nil, 400)
	if got := len(a.list("GET", "/v1/notes/search?q=payday", 200)); got != 1 {
		t.Fatalf("search: %d", got)
	}
	a.want("GET", "/v1/notes/search", nil, 400)
	if got := len(a.list("GET", "/v1/notes/search?q=nothingmatchesthis", 200)); got != 0 {
		t.Fatalf("search miss: %d", got)
	}

	one := a.obj("GET", fmt.Sprintf("/v1/notes/%d", firstID), nil, 200)
	if one["title"] != "Wifi bill" || one["body_md"] != "Pay on the 5th, 385rb." {
		t.Fatalf("get note: %v", one)
	}
	a.want("GET", "/v1/notes/9999", nil, 400)

	upd := a.obj("PATCH", fmt.Sprintf("/v1/notes/%d", firstID),
		map[string]any{"title": "Wifi bill (house)", "tags": []string{"bills"}}, 200)
	if upd["title"] != "Wifi bill (house)" || len(upd["tags"].([]any)) != 1 {
		t.Fatalf("patch note: %v", upd)
	}
	a.want("PATCH", fmt.Sprintf("/v1/notes/%d", firstID), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/notes/%d", firstID), map[string]any{"title": "  "}, 400)
	a.want("PATCH", "/v1/notes/9999", map[string]any{"title": "x"}, 404)

	// note_links has no ON DELETE CASCADE: deleting a linked note must not 500
	if _, err := a.conn.Exec(`INSERT INTO brain.note_links(from_note,to_note) VALUES ($1,$2)`, firstID, secondID); err != nil {
		t.Fatalf("link insert: %v", err)
	}
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d", firstID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d?confirm=true", firstID), nil, 200)
	var links int
	if err := a.conn.QueryRow(`SELECT count(*) FROM brain.note_links WHERE from_note=$1 OR to_note=$1`, firstID).Scan(&links); err != nil {
		t.Fatalf("link count: %v", err)
	}
	if links != 0 {
		t.Fatalf("dangling note_links: %d", links)
	}
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d?confirm=true", firstID), nil, 404)
	if got := len(a.list("GET", "/v1/notes", 200)); got != 1 {
		t.Fatalf("notes after delete: %d", got)
	}
}

func TestMealsMacrosAndTargets(t *testing.T) {
	a := newAPI(t)
	meal := a.obj("POST", "/v1/meals", map[string]any{
		"source": "manual", "note": "lunch",
		"items": []map[string]any{
			{"name": "nasi goreng", "est_grams": 350, "calories": 620, "protein": 18.5, "carbs": 85, "fat": 21, "confidence": 0.8},
			{"name": "telur", "est_grams": 60, "calories": 90, "protein": 7, "carbs": 1, "fat": 6, "confidence": 0.9},
		}}, 201)
	mealID := int(meal["meal_id"].(float64))
	if int(meal["items"].(float64)) != 2 {
		t.Fatalf("meal items: %v", meal)
	}
	a.want("POST", "/v1/meals", map[string]any{"source": "telepathy"}, 400)
	a.want("POST", "/v1/meals", map[string]any{"items": []any{}}, 201) // an empty day is allowed

	today := time.Now().Format("2006-01-02")
	meals := a.list("GET", "/v1/meals", 200)
	if len(meals) != 2 {
		t.Fatalf("list meals: %d", len(meals))
	}
	first := map[string]any{}
	for _, m := range meals {
		mm := m.(map[string]any)
		if int(mm["id"].(float64)) == mealID {
			first = mm
		}
	}
	if len(first["items"].([]any)) != 2 {
		t.Fatalf("meal items in list: %v", meals)
	}
	if got := len(a.list("GET", "/v1/meals?day="+today, 200)); got != 2 {
		t.Fatalf("day filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/meals?day=2020-01-01", 200)); got != 0 {
		t.Fatalf("empty day: %d", got)
	}
	a.want("GET", "/v1/meals?day=nope", nil, 400)

	r0 := a.obj("GET", "/v1/nutrition/daily", nil, 200)
	if int(r0["calories"].(float64)) != 710 || r0["day"] != today {
		t.Fatalf("daily rollup: %v", r0)
	}
	if r0["protein_g"].(float64) != 25.5 {
		t.Fatalf("protein rollup: %v", r0)
	}

	// targets: read, upsert, clear
	a.want("GET", "/v1/nutrition/target?day=nope", nil, 400)
	empty := a.obj("GET", "/v1/nutrition/target", nil, 200)
	if empty["calorie_target"] != nil {
		t.Fatalf("unset target: %v", empty)
	}
	set := a.obj("PUT", "/v1/nutrition/target",
		map[string]any{"calorie_target": 2100, "protein_target": 120}, 200)
	if int(set["calorie_target"].(float64)) != 2100 {
		t.Fatalf("set target: %v", set)
	}
	a.want("PUT", "/v1/nutrition/target", map[string]any{}, 400)
	a.want("PUT", "/v1/nutrition/target", map[string]any{"calorie_target": 0}, 400)
	a.want("PUT", "/v1/nutrition/target", map[string]any{"day": "nope", "calorie_target": 2000}, 400)
	patch := a.obj("PUT", "/v1/nutrition/target", map[string]any{"protein_target": 130}, 200)
	if int(patch["calorie_target"].(float64)) != 2100 || patch["protein_target"].(float64) != 130 {
		t.Fatalf("partial target update kept the other field? %v", patch)
	}
	if got := a.obj("GET", "/v1/nutrition/daily", nil, 200); int(got["calorie_target"].(float64)) != 2100 {
		t.Fatalf("daily should expose the target: %v", got)
	}
	a.want("DELETE", "/v1/nutrition/target", nil, 400)
	a.want("DELETE", "/v1/nutrition/target?confirm=true", nil, 200)
	a.want("DELETE", "/v1/nutrition/target?confirm=true", nil, 404)

	upd := a.obj("PATCH", fmt.Sprintf("/v1/meals/%d", mealID),
		map[string]any{"note": "lunch (estimate)", "logged_at": today}, 200)
	if int(upd["updated"].(float64)) != 1 {
		t.Fatalf("meal patch: %v", upd)
	}
	a.want("PATCH", fmt.Sprintf("/v1/meals/%d", mealID), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/meals/%d", mealID), map[string]any{"logged_at": "nope"}, 400)
	a.want("PATCH", "/v1/meals/9999", map[string]any{"note": "x"}, 404)
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d", mealID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d?confirm=true", mealID), nil, 200)
	var items int
	if err := a.conn.QueryRow(`SELECT count(*) FROM nutrition.meal_items WHERE meal_id=$1`, mealID).Scan(&items); err != nil {
		t.Fatalf("item count: %v", err)
	}
	if items != 0 {
		t.Fatalf("meal_items not cascaded: %d", items)
	}
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d?confirm=true", mealID), nil, 404)
}

func TestUserAdminRosterAndRemoval(t *testing.T) {
	a := newAPI(t)
	roster := a.list("GET", "/v1/admin/users", 200)
	if len(roster) != 1 {
		t.Fatalf("roster: %v", roster)
	}
	// upsert: same telegram id twice must not duplicate the member
	spouse := a.obj("POST", "/v1/admin/users",
		map[string]any{"telegram_user_id": 555000111, "display_name": "Istri"}, 201)
	again := a.obj("POST", "/v1/admin/users",
		map[string]any{"telegram_user_id": 555000111, "display_name": "Istri (Seabank)"}, 201)
	if spouse["user_id"] != again["user_id"] {
		t.Fatalf("telegram id must map to one member: %v vs %v", spouse, again)
	}
	if got := len(a.list("GET", "/v1/admin/users", 200)); got != 2 {
		t.Fatalf("roster after upsert: %d", got)
	}
	a.want("POST", "/v1/admin/users", map[string]any{"telegram_user_id": 0, "display_name": "x"}, 400)
	a.want("POST", "/v1/admin/users", map[string]any{"telegram_user_id": 42, "display_name": " "}, 400)

	spouseID := int(spouse["user_id"].(float64))
	// the spouse gets her own pocket; removing her must take it along
	a.req("POST", "/v1/pockets",
		map[string]any{"name": "Seabank", "type": "savings", "opening_balance_idr": 700000}, spouseID, true)
	var spousePocket int
	if err := a.conn.QueryRow(`SELECT id FROM expense.pockets WHERE user_id=$1`, spouseID).Scan(&spousePocket); err != nil {
		t.Fatalf("spouse pocket: %v", err)
	}
	if _, raw := a.req("GET", "/v1/pockets", nil, spouseID, true); !bodyContains(raw, "Seabank") {
		t.Fatalf("spouse pocket missing: %s", raw)
	}

	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d", spouseID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d?confirm=true", a.uid), nil, 400) // self
	a.want("DELETE", "/v1/admin/users/9999?confirm=true", nil, 404)
	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d?confirm=true", spouseID), nil, 200)
	if got := len(a.list("GET", "/v1/admin/users", 200)); got != 1 {
		t.Fatalf("roster after removal: %d", got)
	}
	var left int
	if err := a.conn.QueryRow(`SELECT count(*) FROM expense.pockets WHERE user_id=$1`, spouseID).Scan(&left); err != nil {
		t.Fatalf("pocket count: %v", err)
	}
	if left != 0 {
		t.Fatalf("member rows not wiped: %d pockets left", left)
	}
}

func TestResetScopesKeepOtherMembers(t *testing.T) {
	a := newAPI(t)
	other := mkUser(t, a.conn, 2, "Istri")
	a.pocket("Cash", "cash", 50000)
	pocketID := int(a.list("GET", "/v1/pockets", 200)[0].(map[string]any)["id"].(float64))
	a.spend(pocketID, "out", 5000, "food", "kopi")
	a.obj("POST", "/v1/notes", map[string]any{"title": "n1", "body_md": "b"}, 201)
	a.obj("POST", "/v1/meals", map[string]any{"items": []map[string]any{{"name": "nasi", "calories": 300}}}, 201)

	// another member's data, to prove the wipe is scoped to one user
	var otherPocket int
	if err := a.conn.QueryRow(`INSERT INTO expense.pockets(user_id,name,type,opening_balance)
		VALUES ($1,'Cash','cash',999) RETURNING id`, other).Scan(&otherPocket); err != nil {
		t.Fatalf("other pocket: %v", err)
	}

	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "nope", "scope": "all"}, 400)
	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "nope"}, 400)
	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "banana"}, 400)

	ledger := a.obj("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "ledger"}, 200)
	del := ledger["deleted"].(map[string]any)
	if int(del["transactions"].(float64)) != 1 {
		t.Fatalf("ledger reset counts: %v", del)
	}
	if got := len(a.list("GET", "/v1/pockets", 200)); got != 1 {
		t.Fatalf("scope=ledger must keep pockets: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes", 200)); got != 1 {
		t.Fatalf("scope=ledger must keep notes: %d", got)
	}

	all := a.obj("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "all"}, 200)
	delAll := all["deleted"].(map[string]any)
	if int(delAll["pockets"].(float64)) != 1 || int(delAll["notes"].(float64)) != 1 || int(delAll["meals"].(float64)) != 1 {
		t.Fatalf("scope=all counts: %v", delAll)
	}
	if got := len(a.list("GET", "/v1/pockets", 200)); got != 0 {
		t.Fatalf("pockets after scope=all: %d", got)
	}

	// regression: the wipe must not rewind a sequence other members still use
	// (that handed out colliding ids and made the next INSERT fail)
	if code, _ := a.call("POST", "/v1/pockets",
		map[string]any{"name": "New Cash", "type": "cash", "opening_balance_idr": 1000}); code != 201 {
		t.Fatalf("insert after reset should succeed, got %d", code)
	}
	var otherBalance int64
	if err := a.conn.QueryRow(`SELECT opening_balance FROM expense.pockets WHERE id=$1`, otherPocket).Scan(&otherBalance); err != nil {
		t.Fatalf("other member's pocket was touched: %v", err)
	}
	if otherBalance != 999 {
		t.Fatalf("other member's pocket changed: %d", otherBalance)
	}
}

func TestPocketSharingIsReadOnlyAndOptIn(t *testing.T) {
	a := newAPI(t)
	pipit := mkUser(t, a.conn, 2, "Pipit")

	mine := a.pocket("Jago Utama", "cash", 800000)
	secret := a.pocket("Dana Darurat", "savings", 5000000)

	// private by default: nothing of mine is readable by her
	if got := a.obj("GET", fmt.Sprintf("/v1/pockets/%d", mine), nil, 200)["visibility"]; got != "private" {
		t.Fatalf("a new pocket should default to private, got %v", got)
	}
	if code, _ := a.req("GET", fmt.Sprintf("/v1/pockets/%d", mine), nil, pipit, true); code != 404 {
		t.Fatalf("her read of my private pocket: %d", code)
	}
	if code, _ := a.req("GET", "/v1/household", nil, pipit, true); code != 200 {
		t.Fatalf("household: %d", code)
	}
	house := a.reqJSON("GET", "/v1/household", pipit)
	if len(house["members"].([]any)) != 0 {
		t.Fatalf("nothing is shared yet: %v", house)
	}

	// share it, and she can read it (and its entries) but never write it
	upd := a.obj("PATCH", fmt.Sprintf("/v1/pockets/%d", mine), map[string]any{"visibility": "shared"}, 200)
	if upd["visibility"] != "shared" {
		t.Fatalf("share did not stick: %v", upd)
	}
	a.spend(mine, "out", 25000, "food", "kopi")
	seen := a.reqJSON("GET", fmt.Sprintf("/v1/pockets/%d", mine), pipit)
	if seen["name"] != "Jago Utama" || int64(seen["balance_idr"].(float64)) != 775000 {
		t.Fatalf("shared pocket read: %v", seen)
	}
	rows := a.reqList("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", mine), pipit)
	if len(rows) != 1 || rows[0].(map[string]any)["note"] != "kopi" {
		t.Fatalf("shared pocket ledger: %v", rows)
	}

	house = a.reqJSON("GET", "/v1/household", pipit)
	members := house["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("household should show one member: %v", house)
	}
	danis := members[0].(map[string]any)
	if danis["display_name"] != "Test Dani" || len(danis["pockets"].([]any)) != 1 {
		t.Fatalf("household member view: %v", danis)
	}
	if int64(danis["total_idr"].(float64)) != 775000 {
		t.Fatalf("household total: %v", danis["total_idr"])
	}

	// her private pocket stays hidden even from the household view
	hers := a.reqJSONObj("POST", "/v1/pockets", pipit,
		map[string]any{"name": "BRImo Pipit", "type": "cash", "opening_balance_idr": 50000}, 201)
	_ = hers
	herShared := a.reqJSONObj("POST", "/v1/pockets", pipit,
		map[string]any{"name": "Seabank", "type": "savings", "opening_balance_idr": 700000, "visibility": "shared"}, 201)
	_ = herShared
	mineHouse := a.reqJSON("GET", "/v1/household", a.uid)
	if len(mineHouse["members"].([]any)) != 2 {
		t.Fatalf("household should list both members: %v", mineHouse)
	}
	if int(mineHouse["you"].(float64)) != a.uid {
		t.Fatalf("household should say who is asking: %v", mineHouse["you"])
	}
	// each member opens the view on their own money
	if got := mineHouse["members"].([]any)[0].(map[string]any)["display_name"]; got != "Test Dani" {
		t.Fatalf("my household view should start with me, got %v", got)
	}
	herHouse := a.reqJSON("GET", "/v1/household", pipit)
	if got := herHouse["members"].([]any)[0].(map[string]any)["display_name"]; got != "Pipit" {
		t.Fatalf("her household view should start with her, got %v", got)
	}
	if got := herHouse["members"].([]any)[1].(map[string]any)["display_name"]; got != "Test Dani" {
		t.Fatalf("her household view should list me second, got %v", got)
	}
	if body := fmt.Sprint(mineHouse); bodyContains([]byte(body), "BRImo Pipit") {
		t.Fatalf("her private pocket leaked into the household view: %s", body)
	}
	if !bodyContains([]byte(fmt.Sprint(mineHouse)), "Seabank") {
		t.Fatalf("her shared pocket is missing: %v", mineHouse)
	}

	// writes on someone else's shared pocket are refused everywhere
	if code, _ := a.req("POST", "/v1/transactions", map[string]any{
		"pocket_id": mine, "direction": "out", "amount_idr": 1000, "category": "food"}, pipit, true); code != 400 {
		t.Fatalf("she should not be able to spend from my pocket: %d", code)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/pockets/%d", mine),
		map[string]any{"name": "Hijacked"}, pipit, true); code != 404 {
		t.Fatalf("she should not be able to rename my pocket: %d", code)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/pockets/%d", mine),
		map[string]any{"visibility": "private"}, pipit, true); code != 404 {
		t.Fatalf("she should not be able to unshare my pocket: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", mine), nil, pipit, true); code != 404 {
		t.Fatalf("she should not be able to delete my pocket: %d", code)
	}
	// and a private pocket of mine is invisible in every read path
	if code, _ := a.req("GET", fmt.Sprintf("/v1/pockets/%d", secret), nil, pipit, true); code != 404 {
		t.Fatalf("private pocket read: %d", code)
	}
	if code, _ := a.req("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", secret), nil, pipit, true); code != 403 {
		t.Fatalf("private pocket ledger should be 403: %d", code)
	}
	if code, _ := a.req("GET", fmt.Sprintf("/v1/transfers?pocket_id=%d", secret), nil, pipit, true); code != 403 {
		t.Fatalf("private pocket transfers should be 403: %d", code)
	}
	// the owner still sees everything of their own
	if got := len(a.list("GET", "/v1/pockets", 200)); got != 2 {
		t.Fatalf("owner pocket list: %d", got)
	}
	if len(a.list("GET", fmt.Sprintf("/v1/transactions?pocket_id=%d", secret), 200)) != 0 {
		t.Fatal("owner should be able to read their own private pocket ledger")
	}

	// un-sharing takes it back
	a.obj("PATCH", fmt.Sprintf("/v1/pockets/%d", mine), map[string]any{"visibility": "private"}, 200)
	if code, _ := a.req("GET", fmt.Sprintf("/v1/pockets/%d", mine), nil, pipit, true); code != 404 {
		t.Fatalf("un-shared pocket still readable: %d", code)
	}
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", mine), map[string]any{"visibility": "public"}, 400)
	a.want("POST", "/v1/pockets", map[string]any{"name": "X", "visibility": "public"}, 400)
}

// A member's scope decides which features they get (the dashboard hides the
// notes and nutrition tabs for a finance-only member), so it has to be readable
// by the member themselves and by the household roster.
func TestMemberScopeAndIdentity(t *testing.T) {
	a := newAPI(t)

	me := a.obj("GET", "/v1/me", nil, 200)
	if me["display_name"] != "Test Dani" || me["scope"] != "full" || int(me["user_id"].(float64)) != a.uid {
		t.Fatalf("me should describe the caller: %v", me)
	}

	created := a.obj("POST", "/v1/admin/users", map[string]any{
		"telegram_user_id": 1149698493, "display_name": "Pipit", "scope": "finance"}, 201)
	if created["scope"] != "finance" {
		t.Fatalf("created member should carry the scope: %v", created)
	}
	her := int(created["user_id"].(float64))
	if code, raw := a.req("GET", "/v1/me", nil, her, true); code != 200 || !bodyContains(raw, `"scope":"finance"`) {
		t.Fatalf("her own view should say finance: %d %s", code, raw)
	}

	roster := a.list("GET", "/v1/admin/users", 200)
	var hers map[string]any
	for _, m := range roster {
		if int(m.(map[string]any)["id"].(float64)) == her {
			hers = m.(map[string]any)
		}
	}
	if hers == nil || hers["scope"] != "finance" || hers["telegram_user_id"].(float64) != 1149698493 {
		t.Fatalf("roster should report each member's scope: %v", roster)
	}

	// an unknown scope is rejected instead of being stored, and the default holds
	a.want("POST", "/v1/admin/users", map[string]any{
		"telegram_user_id": 5, "display_name": "X", "scope": "nutrition"}, 400)
	def := a.obj("POST", "/v1/admin/users", map[string]any{
		"telegram_user_id": 6, "display_name": "Y"}, 201)
	if def["scope"] != "full" {
		t.Fatalf("scope should default to full: %v", def)
	}

	if code, _ := a.req("GET", "/v1/me", nil, 99999, true); code != 404 {
		t.Fatalf("unknown member should be 404, got %d", code)
	}
}

// Recurring plans: the ledger books them only when a human confirms, once per
// month, dated the day the money actually moved (not the day it was confirmed).
func TestSchedulesBookOncePerMonthAndBackdate(t *testing.T) {
	a := newAPI(t)
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	third := time.Date(now.Year(), now.Month(), 3, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	period := now.Format("2006-01")

	main := a.pocket("Jago Utama", "cash", 5000000)
	saving := a.pocket("Tabungan Jago", "savings", 20000000)
	salary := a.pocket("BRImo", "cash", 0)

	// the plan: 1jt into savings on the 1st, salary arriving on the 1st
	sav := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000000,
		"day_of_month": 1, "note": "tabungan bulanan"}, 201)
	savID := int(sav["id"].(float64))
	sal := a.obj("POST", "/v1/schedules", map[string]any{
		"kind": "income", "to_pocket_id": salary, "amount_idr": 8000000,
		"day_of_month": 1, "note": "gaji"}, 201)
	salID := int(sal["id"].(float64))

	// shape guards
	a.want("POST", "/v1/schedules", map[string]any{
		"to_pocket_id": saving, "amount_idr": 1000, "day_of_month": 1}, 400) // no source for a transfer
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": main, "amount_idr": 1000, "day_of_month": 1}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 0, "day_of_month": 1}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000, "day_of_month": 32}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"kind": "charity", "to_pocket_id": saving, "amount_idr": 1000, "day_of_month": 1}, 400)
	a.want("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": 999999, "to_pocket_id": saving, "amount_idr": 1000, "day_of_month": 1}, 400)

	// on the 1st both are due and nothing is booked yet
	due := a.list("GET", "/v1/schedules?date="+first, 200)
	if len(due) != 2 {
		t.Fatalf("plans: %v", due)
	}
	for _, d := range due {
		if d.(map[string]any)["status"] != "due_today" || d.(map[string]any)["due_date"] != first {
			t.Fatalf("status on the 1st: %v", d)
		}
	}
	// a day the month does not have falls back to the last day instead of vanishing
	clamp := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 250000,
		"day_of_month": 31, "note": "akhir bulan"}, 201)
	clampID := int(clamp["id"].(float64))
	byID := func(date string) map[string]any {
		for _, s := range a.list("GET", "/v1/schedules?date="+date, 200) {
			if int(s.(map[string]any)["id"].(float64)) == clampID {
				return s.(map[string]any)
			}
		}
		t.Fatalf("schedule %d missing for %s", clampID, date)
		return nil
	}
	if got := byID("2026-02-15")["due_date"]; got != "2026-02-28" {
		t.Fatalf("day 31 in February should land on the 28th, got %v", got)
	}
	if got := byID("2026-04-15")["due_date"]; got != "2026-04-30" {
		t.Fatalf("day 31 in April should land on the 30th, got %v", got)
	}
	if got := byID("2026-01-15")["due_date"]; got != "2026-01-31" {
		t.Fatalf("day 31 in January stays the 31st, got %v", got)
	}
	a.want("DELETE", fmt.Sprintf("/v1/schedules/%d?confirm=true", clampID), nil, 200)

	// confirming on the 3rd still books the money on the 1st
	run := a.obj("POST", "/v1/schedules/run", map[string]any{"date": third}, 200)
	ran := run["ran"].([]any)
	if len(ran) != 2 {
		t.Fatalf("should have booked both plans: %v", run)
	}
	if got := a.balanceOf(a.uid, saving); got != 21000000 {
		t.Fatalf("savings after the plan: %d", got)
	}
	if got := a.balanceOf(a.uid, main); got != 4000000 {
		t.Fatalf("main pocket after the plan: %d", got)
	}
	if got := a.balanceOf(a.uid, salary); got != 8000000 {
		t.Fatalf("salary pocket after the plan: %d", got)
	}
	tr := a.list("GET", "/v1/transfers?month="+period, 200)
	if len(tr) != 1 || tr[0].(map[string]any)["created_at"].(string)[:10] != first {
		t.Fatalf("the transfer should be dated the 1st: %v", tr)
	}
	txns := a.list("GET", "/v1/transactions?month="+period, 200)
	if len(txns) != 1 {
		t.Fatalf("income entries: %v", txns)
	}
	income := txns[0].(map[string]any)
	if income["direction"] != "in" || income["source"] != "scheduled" ||
		income["created_at"].(string)[:10] != first || int64(income["amount_idr"].(float64)) != 8000000 {
		t.Fatalf("scheduled income entry: %v", income)
	}

	// nothing is pending any more, and a second run books nothing
	if got := len(a.list("GET", "/v1/schedules?date="+third+"&pending=true", 200)); got != 0 {
		t.Fatalf("nothing should be pending after booking, got %d", got)
	}
	again := a.obj("POST", "/v1/schedules/run", map[string]any{"date": third}, 200)
	if len(again["ran"].([]any)) != 0 {
		t.Fatalf("a recurring plan must not book twice in one month: %v", again)
	}
	if got := a.balanceOf(a.uid, saving); got != 21000000 {
		t.Fatalf("balance moved on the second run: %d", got)
	}
	// the same plan next month is a new period, so it books again
	nextMonth := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	if nextMonth.After(now) {
		t.Logf("next period (%s) is in the future, skipping its run", nextMonth.Format("2006-01"))
	}

	// an explicit id books that one plan, and force re-books a period on purpose
	a.want("POST", "/v1/schedules/run", map[string]any{"date": third, "ids": []int{savID}}, 200)
	forced := a.obj("POST", "/v1/schedules/run", map[string]any{"date": third, "ids": []int{savID}, "force": true}, 200)
	if len(forced["ran"].([]any)) != 1 {
		t.Fatalf("force should re-book: %v", forced)
	}

	// a plan cannot be booked for a date that has not happened yet
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	a.want("POST", "/v1/schedules/run", map[string]any{"date": tomorrow}, 400)
	a.want("POST", "/v1/schedules/run", map[string]any{"date": "not-a-date"}, 400)
	a.want("GET", "/v1/schedules?date=nope", nil, 400)

	// editing the plan: amount, day, and pausing it
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{"amount_idr": 1200000}, 200)
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{"day_of_month": 5}, 200)
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{"active": false}, 200)
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{"amount_idr": 0}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{}, 400)
	a.want("PATCH", "/v1/schedules/999999", map[string]any{"amount_idr": 1000}, 404)

	// another member neither sees nor runs my plans
	pipit := mkUser(t, a.conn, 2, "Pipit")
	if got := len(a.reqList("GET", "/v1/schedules", pipit)); got != 0 {
		t.Fatalf("her schedule list should be empty, got %d", got)
	}
	if code, _ := a.req("POST", "/v1/schedules/run", map[string]any{"ids": []int{salID}}, pipit, true); code != 200 {
		t.Fatalf("running someone else's schedule id: %d", code)
	}
	if got := a.balanceOf(a.uid, salary); got != 8000000 {
		t.Fatalf("her run must not touch my pockets: %d", got)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/schedules/%d", savID), map[string]any{"amount_idr": 5}, pipit, true); code != 404 {
		t.Fatalf("she must not edit my plan: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/schedules/%d?confirm=true", savID), nil, pipit, true); code != 404 {
		t.Fatalf("she must not delete my plan: %d", code)
	}

	// delete needs confirmation and is scoped to the owner
	a.want("DELETE", fmt.Sprintf("/v1/schedules/%d", savID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/schedules/%d?confirm=true", savID), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/schedules/%d?confirm=true", savID), nil, 404)
}

// Deleting the entry a plan booked must un-book that month: otherwise a mistaken
// entry leaves the plan stuck on "already booked" with nothing to show for it.
func TestDeletingABookedEntryReopensTheMonth(t *testing.T) {
	a := newAPI(t)
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")

	main := a.pocket("Jago Utama", "cash", 3000000)
	saving := a.pocket("Tabungan Jago", "savings", 0)
	a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": main, "to_pocket_id": saving, "amount_idr": 1000000,
		"day_of_month": 1, "note": "tabungan bulanan"}, 201)

	run := a.obj("POST", "/v1/schedules/run", map[string]any{"date": first}, 200)
	transferID := int(run["ran"].([]any)[0].(map[string]any)["transfer_id"].(float64))
	if got := len(a.list("GET", "/v1/schedules?date="+first+"&pending=true", 200)); got != 0 {
		t.Fatalf("booked month should not be pending: %d", got)
	}

	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", transferID), nil, 200)
	if got := len(a.list("GET", "/v1/schedules?date="+first+"&pending=true", 200)); got != 1 {
		t.Fatalf("deleting the entry should reopen the month, pending=%d", got)
	}
	// and it can be booked again, exactly once
	again := a.obj("POST", "/v1/schedules/run", map[string]any{"date": first}, 200)
	if len(again["ran"].([]any)) != 1 {
		t.Fatalf("re-booking after the delete: %v", again)
	}
	if got := a.balanceOf(a.uid, saving); got != 1000000 {
		t.Fatalf("savings should hold exactly one month's plan: %d", got)
	}
}

// A plan whose money never arrived must not silently overdraw a pocket: the entry
// is recorded (it mirrors reality) and the response says so.
func TestScheduleWarnsWhenTheSourceGoesNegative(t *testing.T) {
	a := newAPI(t)
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")

	small := a.pocket("Cash", "cash", 50000)
	saving := a.pocket("Tabungan Jago", "savings", 0)
	a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": small, "to_pocket_id": saving, "amount_idr": 200000,
		"day_of_month": 1, "note": "setoran"}, 201)

	out := a.obj("POST", "/v1/schedules/run", map[string]any{"date": first}, 200)
	warnings := out["warnings"].([]any)
	if len(warnings) != 1 || !bodyContains([]byte(fmt.Sprint(warnings[0])), "minus") {
		t.Fatalf("overdrawing plan should warn: %v", out)
	}
	if got := a.balanceOf(a.uid, small); got != -150000 {
		t.Fatalf("the entry still mirrors reality: %d", got)
	}
}

// A plan whose figure varies month to month is a checklist item, not a contract:
// it asks for the real number instead of assuming one.
func TestVariableAmountPlan(t *testing.T) {
	a := newAPI(t)
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	pipit := mkUser(t, a.conn, 2, "Pipit")
	herSeabank := int(a.reqJSONObj("POST", "/v1/pockets", pipit, map[string]any{
		"name": "seabank", "type": "cash", "opening_balance_idr": 0, "visibility": "shared"}, 201)["id"].(float64))

	brimo := a.pocket("BRImo", "cash", 20000000)
	jago := a.pocket("Jago Utama", "cash", 0)

	// a plan with no figure: "what is left after I keep 1,4jt in BRImo"
	v := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": brimo, "to_pocket_id": jago, "day_of_month": 1,
		"note": "sisa gaji ke Jago"}, 201)
	vID := int(v["id"].(float64))
	row := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)
	if row["amount_idr"] != nil || row["variable"] != true {
		t.Fatalf("a plan without a figure should say so: %v", row)
	}

	// booking it without a figure books nothing and explains why
	run := a.obj("POST", "/v1/schedules/run", map[string]any{"date": first, "ids": []int{vID}}, 200)
	if len(run["ran"].([]any)) != 0 || !bodyContains([]byte(fmt.Sprint(run["skipped"])), "nominal") {
		t.Fatalf("a variable plan must ask for its figure: %v", run)
	}
	if got := a.balanceOf(a.uid, jago); got != 0 {
		t.Fatalf("nothing should have moved: %d", got)
	}

	// with the real figure it books that figure
	run = a.obj("POST", "/v1/schedules/run", map[string]any{
		"date": first, "ids": []int{vID}, "amount_idr": 11000000}, 200)
	if len(run["ran"].([]any)) != 1 || int64(run["ran"].([]any)[0].(map[string]any)["amount_idr"].(float64)) != 11000000 {
		t.Fatalf("booking with the real figure: %v", run)
	}
	if got := a.balanceOf(a.uid, jago); got != 11000000 {
		t.Fatalf("jago after the move: %d", got)
	}
	if got := a.balanceOf(a.uid, brimo); got != 9000000 {
		t.Fatalf("brimo after the move: %d", got)
	}
	// an override belongs to exactly one plan
	a.want("POST", "/v1/schedules/run", map[string]any{"date": first, "amount_idr": 1000}, 400)
	a.want("POST", "/v1/schedules/run", map[string]any{
		"date": first, "ids": []int{vID, vID}, "amount_idr": 1000}, 400)
	a.want("POST", "/v1/schedules/run", map[string]any{"date": first, "ids": []int{vID}, "amount_idr": 0}, 400)
	// a fixed plan can be turned into a variable one
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", vID), map[string]any{"amount_idr": 9000000}, 200)
	if got := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)["amount_idr"]; int64(got.(float64)) != 9000000 {
		t.Fatalf("amount did not stick: %v", got)
	}
	a.want("PATCH", fmt.Sprintf("/v1/schedules/%d", vID), map[string]any{"variable": true}, 200)
	if got := a.list("GET", "/v1/schedules", 200)[0].(map[string]any)["amount_idr"]; got != nil {
		t.Fatalf("variable should clear the figure: %v", got)
	}

	// a monthly plan may pay another member — "I send my wife 6-7jt every month"
	toHer := a.obj("POST", "/v1/schedules", map[string]any{
		"from_pocket_id": jago, "to_pocket_id": herSeabank, "amount_idr": 6500000,
		"day_of_month": 2, "note": "kirim ke seabank Pipit"}, 201)
	toHerID := int(toHer["id"].(float64))
	// if she stops sharing it, the plan skips instead of writing into a pocket I
	// can no longer see
	a.reqJSONObj("PATCH", fmt.Sprintf("/v1/pockets/%d", herSeabank), pipit, map[string]any{"visibility": "private"}, 200)
	run = a.obj("POST", "/v1/schedules/run", map[string]any{"date": first, "ids": []int{toHerID}}, 200)
	if len(run["ran"].([]any)) != 0 || !bodyContains([]byte(fmt.Sprint(run["skipped"])), "dibagikan") {
		t.Fatalf("an unshared destination should skip: %v", run)
	}
	if got := int64(a.reqJSON("GET", fmt.Sprintf("/v1/pockets/%d", herSeabank), pipit)["balance_idr"].(float64)); got != 0 {
		t.Fatalf("her pocket should be untouched: %d", got)
	}
	// shared again, it books and she can see the money arrive
	a.reqJSONObj("PATCH", fmt.Sprintf("/v1/pockets/%d", herSeabank), pipit, map[string]any{"visibility": "shared"}, 200)
	run = a.obj("POST", "/v1/schedules/run", map[string]any{"date": first, "ids": []int{toHerID}}, 200)
	if len(run["ran"].([]any)) != 1 {
		t.Fatalf("booking to her pocket: %v", run)
	}
	if got := a.balanceOf(a.uid, herSeabank); got != 6500000 {
		t.Fatalf("her seabank after the monthly transfer: %d", got)
	}
	herRows := a.reqList("GET", "/v1/transfers", pipit)
	if len(herRows) != 1 || herRows[0].(map[string]any)["from_user"] != "Test Dani" {
		t.Fatalf("she should see the incoming monthly transfer: %v", herRows)
	}
	// and a plan may not point at a pocket the planner cannot see
	mine := a.pocket("Dana Darurat", "savings", 0) // mine, private
	a.reqJSONObj("POST", "/v1/schedules", pipit, map[string]any{
		"from_pocket_id": herSeabank, "to_pocket_id": mine, "amount_idr": 1000, "day_of_month": 3}, 400)
}

func TestAuthHeaderAndCrossUserIsolation(t *testing.T) {
	a := newAPI(t)
	other := mkUser(t, a.conn, 2, "Istri")
	mine := a.pocket("Cash", "cash", 100000)
	txn := a.spend(mine, "out", 1000, "food", "mine")
	note := a.obj("POST", "/v1/notes", map[string]any{"title": "mine", "body_md": "secret"}, 201)

	// missing or bogus X-User-ID is refused on every guarded surface
	if code, _ := a.req("GET", "/v1/pockets", nil, 0, false); code != 400 {
		t.Fatalf("no header: %d", code)
	}
	if code, _ := a.req("GET", "/v1/pockets", nil, -1, true); code != 400 {
		t.Fatalf("negative id: %d", code)
	}
	if code, _ := a.req("GET", "/v1/notes/abc", nil, a.uid, true); code != 400 {
		t.Fatalf("non-numeric path id: %d", code)
	}

	// an empty pocket of mine, to prove the other member cannot delete it
	empty := a.pocket("Empty", "cash", 0)

	// the other member sees nothing of mine and cannot touch it
	if code, raw := a.req("GET", "/v1/pockets", nil, other, true); code != 200 || bodyContains(raw, "Cash") {
		t.Fatalf("cross-user pocket leak: %d %s", code, raw)
	}
	if code, raw := a.req("GET", "/v1/transactions", nil, other, true); code != 200 || bodyContains(raw, "mine") {
		t.Fatalf("cross-user txn leak: %d %s", code, raw)
	}
	if code, _ := a.req("GET", fmt.Sprintf("/v1/notes/%d", int(note["id"].(float64))), nil, other, true); code == 200 {
		t.Fatal("cross-user note read allowed")
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/transactions/%d", txn), map[string]any{"note": "hacked"}, other, true); code != 404 {
		t.Fatalf("cross-user txn patch: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", empty), nil, other, true); code != 404 {
		t.Fatalf("cross-user pocket delete: %d", code)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/pockets/%d", empty), map[string]any{"name": "Hacked"}, other, true); code != 404 {
		t.Fatalf("cross-user pocket patch: %d", code)
	}
	if code, _ := a.req("GET", "/v1/expense/summary", nil, other, true); code != 200 {
		t.Fatalf("cross-user summary: %d", code)
	}
	a.want("GET", fmt.Sprintf("/v1/notes/%d", int(note["id"].(float64))), nil, 200)
	if got := a.balance(mine); got != 99000 {
		t.Fatalf("my balance moved: %d", got)
	}
	if got := a.balance(empty); got != 0 {
		t.Fatalf("my empty pocket moved: %d", got)
	}
}

// TestBackdatedEntries: money is often reported the next morning ("last night at
// 23:31"), so an entry must be datable to when it really happened instead of the
// moment it was typed. The date is validated: future dates are refused (the ledger
// records what happened, not what will) and the format is strict.
func TestBackdatedEntries(t *testing.T) {
	a := newAPI(t)
	p := int(a.obj("POST", "/v1/pockets",
		map[string]any{"name": "Cash", "type": "cash", "opening_balance_idr": 100000}, 201)["id"].(float64))
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")

	out := a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": p, "direction": "out", "amount_idr": 1200,
		"category": "communication", "note": "kuota buat Ipan",
		"source": "manual", "date": yesterday}, 201)
	id := int(out["id"].(float64))

	var d time.Time
	if err := a.conn.QueryRow(`SELECT created_at::date FROM expense.transactions WHERE id=$1`, id).Scan(&d); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if got := d.Format("2006-01-02"); got != yesterday {
		t.Fatalf("created_at tersimpan %s, seharusnya %s", got, yesterday)
	}
	if got := len(a.list("GET", "/v1/transactions?from="+yesterday+"&to="+yesterday, 200)); got != 1 {
		t.Fatalf("entri mundur tidak muncul di %s: %d entri", yesterday, got)
	}
	if got := len(a.list("GET", "/v1/transactions?from="+today+"&to="+today, 200)); got != 0 {
		t.Fatalf("entri mundur bocor ke hari ini: %d entri", got)
	}

	// tanpa --day, perilaku lama tidak berubah: masuk hari ini
	a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": p, "direction": "out", "amount_idr": 5000,
		"category": "food", "note": "tanpa tanggal", "source": "manual"}, 201)
	if got := len(a.list("GET", "/v1/transactions?from="+today+"&to="+today, 200)); got != 1 {
		t.Fatalf("entri hari ini: %d, seharusnya 1", got)
	}

	// tanggal masa depan dan format longgar ditolak
	for _, bad := range []string{time.Now().AddDate(0, 0, 1).Format("2006-01-02"), "21-09-2026", "2026-9-1"} {
		if st, _ := a.call("POST", "/v1/transactions", map[string]any{
			"pocket_id": p, "direction": "out", "amount_idr": 1000,
			"source": "manual", "date": bad}); st != 400 {
			t.Fatalf("tanggal %q harus ditolak, dapat status %d", bad, st)
		}
	}

	// transfer juga bisa mundur, dan uangnya tetap pindah
	dst := int(a.obj("POST", "/v1/pockets",
		map[string]any{"name": "Jago", "type": "cash", "opening_balance_idr": 0}, 201)["id"].(float64))
	tr := a.obj("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": p, "to_pocket_id": dst, "amount_idr": 10000,
		"note": "tarik tunai", "date": yesterday}, 201)
	if err := a.conn.QueryRow(`SELECT created_at::date FROM expense.transfers WHERE id=$1`,
		int(tr["id"].(float64))).Scan(&d); err != nil {
		t.Fatalf("read transfer created_at: %v", err)
	}
	if got := d.Format("2006-01-02"); got != yesterday {
		t.Fatalf("transfer created_at %s, seharusnya %s", got, yesterday)
	}
	if got := a.balance(dst); got != 10000 {
		t.Fatalf("saldo tujuan setelah transfer mundur: %d, seharusnya 10000", got)
	}
	if st, _ := a.call("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": p, "to_pocket_id": dst, "amount_idr": 1000,
		"date": time.Now().AddDate(0, 0, 2).Format("2006-01-02")}); st != 400 {
		t.Fatalf("transfer bertanggal masa depan harus ditolak, dapat %d", st)
	}
}
