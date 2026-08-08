package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Pagination tests.
//
// The properties that matter are not "a limit is applied" but the two ways
// paging silently corrupts a result set: rows appearing on two pages, and
// rows appearing on none. Both come from an ORDER BY that can tie, so the
// walk-every-page tests below are the real content here.

func pagedGet(t testing.TB, server http.Handler, path string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d (%s)", path, w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("GET %s: response is not a JSON array: %v (%s)", path, err, w.Body.String())
	}
	return w, rows
}

// indexWithUTXOs builds an index holding n UTXOs for one hash160, with
// distinct values so an ordering by value is observable.
func indexWithUTXOs(t testing.TB, n int) (*Index, string) {
	t.Helper()
	idx := NewIndex()
	hash160 := make([]byte, 20)
	var txs []RPCTx
	for i := 0; i < n; i++ {
		txs = append(txs, RPCTx{
			TxID: fmt.Sprintf("%064x", i+1),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{voutWithScript(0, float64(i+1), p2pkhScriptBytes(hash160))},
		})
	}
	idx.ApplyBlock(makeBlock("hash1", 100, txs))
	return idx, fmt.Sprintf("%x", hash160)
}

func TestListEndpointsAreCappedByDefault(t *testing.T) {
	// Without a cap, one request serialises the whole table.
	idx, hash := indexWithUTXOs(t, defaultPageLimit+25)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	w, rows := pagedGet(t, server, "/utxos?hash160="+hash)
	if len(rows) != defaultPageLimit {
		t.Errorf("expected the default cap of %d rows, got %d", defaultPageLimit, len(rows))
	}
	if got := w.Header().Get("X-Has-More"); got != "true" {
		t.Errorf("a truncated response must say so: X-Has-More = %q", got)
	}
	if got := w.Header().Get("X-Next-Offset"); got != fmt.Sprint(defaultPageLimit) {
		t.Errorf("X-Next-Offset = %q, want %d", got, defaultPageLimit)
	}
}

func TestShortResultSetSaysThereIsNoMore(t *testing.T) {
	idx, hash := indexWithUTXOs(t, 3)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	w, rows := pagedGet(t, server, "/utxos?hash160="+hash)
	if len(rows) != 3 {
		t.Fatalf("expected all 3 rows, got %d", len(rows))
	}
	if got := w.Header().Get("X-Has-More"); got != "false" {
		t.Errorf("X-Has-More = %q, want false", got)
	}
	if w.Header().Get("X-Next-Offset") != "" {
		t.Error("X-Next-Offset must be absent when there is no next page")
	}
}

// indexWithTiedUTXOs gives every UTXO the SAME value, so an ORDER BY on
// value alone is genuinely ambiguous and only the outpoint tiebreak makes
// the order total.
//
// Honest limitation: deleting the tiebreak does NOT fail this test.
// SQLite happens to return tied rows in rowid order for this query plan,
// consistently enough that paging still works without it. The tiebreak is
// there because that is a property of the current plan and not a promise
// -- add an index, change the query, or change SQLite version and it can
// stop holding. What this test does pin is the property itself: every row
// visited exactly once across a full walk.
func indexWithTiedUTXOs(t testing.TB, n int) (*Index, string) {
	t.Helper()
	idx := NewIndex()
	hash160 := make([]byte, 20)
	var txs []RPCTx
	for i := 0; i < n; i++ {
		txs = append(txs, RPCTx{
			TxID: fmt.Sprintf("%064x", i+1),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{voutWithScript(0, 1.0, p2pkhScriptBytes(hash160))},
		})
	}
	idx.ApplyBlock(makeBlock("hash1", 100, txs))
	return idx, fmt.Sprintf("%x", hash160)
}

func TestPagingVisitsEveryRowExactlyOnceWithTiedValues(t *testing.T) {
	const total = 57
	idx, hash := indexWithTiedUTXOs(t, total)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	seen := map[string]int{}
	offset, pages := 0, 0
	for {
		pages++
		if pages > 100 {
			t.Fatal("paging did not terminate")
		}
		w, rows := pagedGet(t, server, fmt.Sprintf("/utxos?hash160=%s&limit=10&offset=%d", hash, offset))
		for _, r := range rows {
			seen[fmt.Sprintf("%v:%v", r["txid"], r["vout"])]++
		}
		if w.Header().Get("X-Has-More") != "true" {
			break
		}
		offset += len(rows)
	}

	if len(seen) != total {
		t.Errorf("walked %d distinct outpoints, want %d -- rows were skipped", len(seen), total)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("outpoint %s appeared %d times across pages, want exactly 1", k, n)
		}
	}
}

func TestPagingVisitsEveryRowExactlyOnce(t *testing.T) {
	// Distinct values, so ordering is unambiguous; the tied-value variant
	// above covers the harder case.
	const total = 57
	idx, hash := indexWithUTXOs(t, total)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	seen := map[string]int{}
	offset, pages := 0, 0
	for {
		pages++
		if pages > 100 {
			t.Fatal("paging did not terminate")
		}
		w, rows := pagedGet(t, server, fmt.Sprintf("/utxos?hash160=%s&limit=10&offset=%d", hash, offset))
		for _, r := range rows {
			seen[fmt.Sprintf("%v:%v", r["txid"], r["vout"])]++
		}
		if w.Header().Get("X-Has-More") != "true" {
			break
		}
		offset += len(rows)
	}

	if len(seen) != total {
		t.Errorf("walked %d distinct outpoints, want %d", len(seen), total)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("outpoint %s appeared %d times across pages, want exactly 1", k, n)
		}
	}
}

func TestUTXOPageIsLargestValueFirst(t *testing.T) {
	// A wallet funds from this list. If the first page were an arbitrary
	// slice, a user holding plenty could be told they cannot afford an
	// amount the truncated page does not cover.
	idx, hash := indexWithUTXOs(t, 40)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	_, rows := pagedGet(t, server, "/utxos?hash160="+hash+"&limit=5")
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(rows))
	}
	prev := int64(1 << 62)
	for i, r := range rows {
		v, ok := r["value"].(float64)
		if !ok {
			t.Fatalf("row %d has no numeric value: %v", i, r["value"])
		}
		if int64(v) > prev {
			t.Errorf("row %d value %d is larger than the row before it (%d): not sorted descending", i, int64(v), prev)
		}
		prev = int64(v)
	}
	// The largest of 40 coins must be on the first page of 5.
	if top, _ := rows[0]["value"].(float64); int64(top) != 40*100000000 {
		t.Errorf("first row value = %d, want the largest coin (%d)", int64(top), int64(40*100000000))
	}
}

func TestBadPaginationParametersAreRejected(t *testing.T) {
	idx, hash := indexWithUTXOs(t, 3)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	for _, q := range []string{
		"limit=abc", "limit=0", "limit=-1", "limit=1001", "offset=-1", "offset=xyz",
	} {
		req := httptest.NewRequest(http.MethodGet, "/utxos?hash160="+hash+"&"+q, nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", q, w.Code)
		}
	}
}

func TestPaginationKeepsTheBareArrayShape(t *testing.T) {
	// Every deployed wallet does `if (!Array.isArray(result)) throw`, so a
	// {items, total} envelope would break all of them at once. The page
	// metadata lives in headers precisely to avoid that.
	idx, hash := indexWithUTXOs(t, 3)
	server := newAPIServer(idx, NewRateLimiter(10000, 10000), NewRateLimiter(10000, 10000), false, nil)

	for _, path := range []string{
		"/utxos?hash160=" + hash,
		"/positions?pubkey=" + fmt.Sprintf("%x", fakePubKey(0x02)),
		"/orders",
		"/tokens",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d (%s)", path, w.Code, w.Body.String())
		}
		var arr []any
		if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
			t.Errorf("GET %s must return a bare JSON array, got %s", path, w.Body.String())
		}
	}
}

// The paginated store calls must be exercised against real SQL, not just
// through the handlers: an ORDER BY naming a column that does not exist
// builds fine and fails at runtime. That is not hypothetical -- the first
// draft of ListOrdersPage ordered by o.created_at, which is a JSON field
// inside the data blob and not a column at all.
func TestPaginatedStoreQueriesExecute(t *testing.T) {
	idx, hash := indexWithUTXOs(t, 5)
	pubkey := fmt.Sprintf("%x", fakePubKey(0x02))

	if _, _, err := idx.UTXOsForHash160Page(hash, Page{Limit: 2}); err != nil {
		t.Errorf("UTXOsForHash160Page: %v", err)
	}
	if _, _, err := idx.PositionsForPubKeyPage(pubkey, true, Page{Limit: 2}); err != nil {
		t.Errorf("PositionsForPubKeyPage: %v", err)
	}
	if _, _, err := idx.AllTokensPage(Page{Limit: 2}); err != nil {
		t.Errorf("AllTokensPage: %v", err)
	}
	if _, _, err := idx.ListOrdersPage(nil, Page{Limit: 2}); err != nil {
		t.Errorf("ListOrdersPage: %v", err)
	}
	m := int64(1000)
	if _, _, err := idx.ListOrdersPage(&m, Page{Limit: 2}); err != nil {
		t.Errorf("ListOrdersPage(filtered): %v", err)
	}
}

func TestUnlimitedPageStillReturnsEverything(t *testing.T) {
	// Internal callers (mirroring, tests) pass Unlimited and must not be
	// silently capped.
	idx, hash := indexWithUTXOs(t, defaultPageLimit+10)
	rows, hasMore, err := idx.UTXOsForHash160Page(hash, Unlimited)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore {
		t.Error("an unlimited page can never have more")
	}
	if len(rows) != defaultPageLimit+10 {
		t.Errorf("got %d rows, want %d", len(rows), defaultPageLimit+10)
	}
}
