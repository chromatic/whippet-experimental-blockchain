package main

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"testing"
)

// Test-side accessors for state that used to be reachable as Go maps on
// the Index and now lives in the database. They exist so assertions can
// still say "how many positions are there" without every test growing a
// SQL query, and so the ones that need to *seed* state without applying a
// whole block still can.
//
// Everything here goes through the same store the production code uses --
// nothing reaches around it -- so a test that passes is a statement about
// the real storage path.

func mustPosition(t testing.TB, idx *Index, txid string, vout uint32) (Position, bool) {
	t.Helper()
	pos, ok, err := idx.Position(txid, vout)
	if err != nil {
		t.Fatalf("Position(%s:%d): %v", txid, vout, err)
	}
	return pos, ok
}

func mustUTXO(t testing.TB, idx *Index, txid string, vout uint32) (UTXO, bool) {
	t.Helper()
	u, ok, err := idx.UTXO(txid, vout)
	if err != nil {
		t.Fatalf("UTXO(%s:%d): %v", txid, vout, err)
	}
	return u, ok
}

func mustPositionsForPubKey(t testing.TB, idx *Index, pubkey string, unspentOnly bool) []Position {
	t.Helper()
	out, err := idx.PositionsForPubKey(pubkey, unspentOnly)
	if err != nil {
		t.Fatalf("PositionsForPubKey(%s): %v", pubkey, err)
	}
	return out
}

func mustUTXOsForHash160(t testing.TB, idx *Index, hash160 string) []UTXO {
	t.Helper()
	out, err := idx.UTXOsForHash160(hash160)
	if err != nil {
		t.Fatalf("UTXOsForHash160(%s): %v", hash160, err)
	}
	return out
}

func mustAllTokens(t testing.TB, idx *Index) []TokenInfo {
	t.Helper()
	out, err := idx.AllTokens()
	if err != nil {
		t.Fatalf("AllTokens: %v", err)
	}
	return out
}

func mustToken(t testing.TB, idx *Index, origin string) (TokenInfo, bool) {
	t.Helper()
	tok, ok, err := idx.Token(origin)
	if err != nil {
		t.Fatalf("Token(%s): %v", origin, err)
	}
	return tok, ok
}

func mustListOrders(t testing.TB, idx *Index, multiplier *int64) []Order {
	t.Helper()
	out, err := idx.ListOrders(multiplier)
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	return out
}

func mustGetOrder(t testing.TB, idx *Index, txid string, vout uint32) (Order, bool) {
	t.Helper()
	o, ok, err := idx.GetOrder(txid, vout)
	if err != nil {
		t.Fatalf("GetOrder(%s:%d): %v", txid, vout, err)
	}
	return o, ok
}

func mustStatus(t testing.TB, idx *Index) Status {
	t.Helper()
	s, err := idx.StatusSnapshot()
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	return s
}

// --- counts ---

func mustCount(t testing.TB, n int, err error, what string) int {
	t.Helper()
	if err != nil {
		t.Fatalf("counting %s: %v", what, err)
	}
	return n
}

func positionCount(t testing.TB, idx *Index) int {
	t.Helper()
	n, err := idx.store.CountPositions()
	return mustCount(t, n, err, "positions")
}

func utxoCount(t testing.TB, idx *Index) int {
	t.Helper()
	n, err := idx.store.CountUTXOs()
	return mustCount(t, n, err, "utxos")
}

// orderCount counts stored order rows, including any whose position has
// since been spent. ListOrders filters those out, so the two answers differ
// and the distinction matters when checking that a row was actually removed
// rather than merely hidden.
func orderCount(t testing.TB, idx *Index) int {
	t.Helper()
	n, err := idx.store.CountOrders()
	return mustCount(t, n, err, "orders")
}

func heightCount(t testing.TB, idx *Index) int {
	t.Helper()
	var n int
	if err := idx.store.db.QueryRow(`SELECT COUNT(*) FROM heights`).Scan(&n); err != nil {
		t.Fatalf("counting heights: %v", err)
	}
	return n
}

func undoLogCount(t testing.TB, idx *Index) int {
	t.Helper()
	n, err := idx.store.CountUndoLogs()
	return mustCount(t, n, err, "undo logs")
}

// --- bulk reads ---

func allPositions(t testing.TB, idx *Index) []Position {
	t.Helper()
	rows, err := idx.store.db.Query(`SELECT txid, vout, pubkey, multiplier, value,
		is_mint, height, spent, spent_txid, spent_height, origin, metadata
		FROM positions ORDER BY key`)
	if err != nil {
		t.Fatalf("scanning positions: %v", err)
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		pos, err := scanPosition(rows)
		if err != nil {
			t.Fatalf("scanning positions: %v", err)
		}
		out = append(out, *pos)
	}
	return out
}

func allUTXOs(t testing.TB, idx *Index) []UTXO {
	t.Helper()
	rows, err := idx.store.db.Query(
		`SELECT txid, vout, hash160, value, height FROM utxos ORDER BY key`)
	if err != nil {
		t.Fatalf("scanning utxos: %v", err)
	}
	defer rows.Close()
	var out []UTXO
	for rows.Next() {
		u, err := scanUTXO(rows)
		if err != nil {
			t.Fatalf("scanning utxos: %v", err)
		}
		out = append(out, *u)
	}
	return out
}

func undoLogAt(t testing.TB, idx *Index, height int64) (*heightChange, bool) {
	t.Helper()
	tx, err := idx.store.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.rollback()
	change, err := tx.undoLog(height)
	if err != nil {
		t.Fatalf("undo log at %d: %v", height, err)
	}
	return change, change != nil
}

// --- seeding, for tests that need state without applying a block ---

func seedPosition(t testing.TB, idx *Index, pos *Position) {
	t.Helper()
	tx, err := idx.store.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.rollback()
	if err := tx.putPosition(pos); err != nil {
		t.Fatalf("seeding position: %v", err)
	}
	if !pos.Spent {
		if err := tx.addToLineage(pos); err != nil {
			t.Fatalf("seeding lineage: %v", err)
		}
	}
	if err := tx.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func seedUTXO(t testing.TB, idx *Index, utxo *UTXO) {
	t.Helper()
	tx, err := idx.store.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.rollback()
	if err := tx.putUTXO(utxo); err != nil {
		t.Fatalf("seeding utxo: %v", err)
	}
	if err := tx.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func seedOrder(t testing.TB, idx *Index, o *Order) {
	t.Helper()
	tx, err := idx.store.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.rollback()
	if err := tx.putOrder(o); err != nil {
		t.Fatalf("seeding order: %v", err)
	}
	if err := tx.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedHeight records a block hash with no undo log behind it: a height the
// index has an opinion about but cannot roll back.
func seedHeight(t testing.TB, idx *Index, height int64, hash string) {
	t.Helper()
	tx, err := idx.store.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.rollback()
	if err := tx.putHeight(height, hash, nil); err != nil {
		t.Fatalf("seeding height: %v", err)
	}
	if err := tx.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// --- vout fixtures ---
//
// The primitives every synthetic-output builder in these tests is made of.
// The builders themselves live next to the tests that use them:
// uapMintVout/uapTransferVout/opReturnVout/ordinaryVout in index_test.go,
// p2pkhVoutFor in concurrency_test.go.

// coins converts a whole-coin fixture value to satoshis. Test fixtures
// read better as "1.5 coins" than as 150000000, and the values here are
// all small enough for that to be exact. Production code must never do
// this -- see amount.go.
func coins(v float64) Amount {
	return Amount(math.Round(v * 1e8))
}

// voutWithScript assembles the RPCVout shape those builders all want: a
// value, an output index, and a scriptPubKey carried as hex.
func voutWithScript(n uint32, value float64, script []byte) RPCVout {
	vout := RPCVout{
		Value: coins(value),
		N:     n,
	}
	vout.ScriptPubKey.Hex = hex.EncodeToString(script)
	return vout
}

// seededHash160 is a deterministic stand-in for a real hash160: twenty
// bytes counting up from seed, wrapping at 0xff.
func seededHash160(seed byte) []byte {
	h := make([]byte, 20)
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// p2pkhScriptBytes wraps a twenty-byte hash in the standard P2PKH shape:
// OP_DUP OP_HASH160 <push20> hash OP_EQUALVERIFY OP_CHECKSIG.
func p2pkhScriptBytes(hash160 []byte) []byte {
	script := []byte{0x76, 0xa9, 0x14}
	script = append(script, hash160...)
	script = append(script, 0x88, 0xac)
	return script
}

// --- set comparison ---

// assertSameSet compares two collections keyed by a string: it reports
// entries of want missing from got, entries that differ, and entries in got
// that want does not have. noun names the element in failure messages
// ("position", "utxo", ...) and when carries the caller's context phrase
// ("after churn", "after reload").
//
// diff does the per-entry comparison and reports its own failures, because
// not every element type is comparable in the way a plain != implies --
// Position holds a *TokenMetadata, where == would compare pointers rather
// than the metadata they point at.
func assertSameSet[T any](
	t testing.TB,
	noun, when string,
	got, want []T,
	key func(T) string,
	diff func(t testing.TB, k string, got, want T),
) {
	t.Helper()
	index := func(elems []T) map[string]T {
		m := make(map[string]T, len(elems))
		for _, e := range elems {
			m[key(e)] = e
		}
		return m
	}
	g, w := index(got), index(want)

	for k, we := range w {
		ge, ok := g[k]
		if !ok {
			t.Errorf("%s %s missing %s", noun, k, when)
			continue
		}
		diff(t, k, ge, we)
	}
	for k := range g {
		if _, ok := w[k]; !ok {
			t.Errorf("unexpected %s %s %s", noun, k, when)
		}
	}
}

// diffComparable is the per-entry comparison for element types where ==
// means what it looks like it means.
func diffComparable[T comparable](noun, when string) func(testing.TB, string, T, T) {
	return func(t testing.TB, k string, got, want T) {
		t.Helper()
		if got != want {
			t.Errorf("%s %s differs %s:\n got %+v\nwant %+v", noun, k, when, got, want)
		}
	}
}

// snapshotIndex renders the whole indexed state as a comparable string,
// for tests that assert one state equals another.
func snapshotIndex(t testing.TB, idx *Index) string {
	t.Helper()
	tokens := mustAllTokens(t, idx)
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Origin < tokens[j].Origin })
	// The tip is deliberately excluded. Undoing block N leaves the tip at
	// N-1 whether or not the index ever saw N-1, so a fully-undone index
	// legitimately reports a different tip from a fresh one while holding
	// identical content -- which is what these comparisons are about.
	data, _ := json.Marshal(map[string]interface{}{
		"positions": allPositions(t, idx),
		"utxos":     allUTXOs(t, idx),
		"heights":   heightCount(t, idx),
		"tokens":    tokens,
	})
	return string(data)
}
