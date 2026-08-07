package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Cover for the SQLite state store that replaces the whole-state JSON
// snapshot.
//
// The property that matters here is not "state survives a restart" -- the
// JSON snapshot did that too. It is that persistence costs the same at
// block 200,000 as at block 1. The JSON writer marshalled the entire index
// on every save (2.4s / 184MB at 200k blocks, measured, and it did that
// *inside* the read lock, so it stalled ingestion as well as the API).
// TestStoreWriteCostDoesNotGrowWithChainLength is the test that actually
// pins that down; the rest establish that the cheap writer is also correct.

// storeTestIndex opens a store in a temp dir and returns a fresh index
// attached to it, plus the db path for reopening.
func storeTestIndex(t *testing.T) (*Index, *Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	return idx, store, path
}

// reopen loads a fresh index from the same file through a second handle,
// which is what a restart sees: only data that actually committed.
//
// It deliberately leaves the original store open, so assertions can still
// compare the two. Closing first would be a more literal restart but makes
// the "expected" index unreadable, and the property under test -- that the
// bytes are on disk -- is settled either way by a separate connection
// finding them.
func reopen(t *testing.T, store *Store, path string) (*Index, *Store) {
	t.Helper()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	idx, err := reopened.LoadIndex()
	if err != nil {
		t.Fatalf("reloading index: %v", err)
	}
	return idx, reopened
}

// storeChain builds n blocks: each mints a UAP position and pays a P2PKH
// output, and every block after the first spends the previous block's
// P2PKH output so undo has restorable state to work with.
func storeChain(n int) []*RPCBlock {
	salt := []byte("0123456789abcdef")
	var out []*RPCBlock
	for h := 0; h < n; h++ {
		tx := RPCTx{
			TxID: fmt.Sprintf("tx%d", h),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{
				uapMintVout(0, fakePubKey(byte(0x02+h%3)), int64(100+h), salt, 1.0),
				p2pkhVoutFor(1, byte(0x40+h), 2.0),
			},
		}
		if h > 0 {
			tx.Vin = append(tx.Vin, spendVin(fmt.Sprintf("tx%d", h-1), 1))
		}
		out = append(out, makeBlock(fmt.Sprintf("hash%d", h), int64(h), []RPCTx{tx}))
	}
	return out
}

func TestStoreRoundTripsFullState(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	chain := storeChain(6)
	for _, b := range chain {
		idx.ApplyBlock(b)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error during apply: %v", err)
	}

	loaded, _ := reopen(t, store, path)

	if got, want := tipOf(loaded), tipOf(idx); got != want {
		t.Errorf("tip after reload: got %s, want %s", got, want)
	}
	assertSamePositions(t, loaded, idx)
	assertSameUTXOs(t, loaded, idx)
	assertSameTokens(t, loaded, idx)

	// The by-pubkey and by-hash160 lookups are derived indexes; a reload
	// that rebuilt only the primary tables would leave them empty and
	// every /positions query would answer "nothing here".
	for _, pos := range allPositions(t, idx) {
		if got := mustPositionsForPubKey(t, loaded, pos.PubKey, false); len(got) == 0 {
			t.Errorf("PositionsForPubKey(%s) empty after reload", pos.PubKey)
			break
		}
	}
	for _, u := range allUTXOs(t, idx) {
		if got := mustUTXOsForHash160(t, loaded, u.Hash160); len(got) == 0 {
			t.Errorf("UTXOsForHash160(%s) empty after reload", u.Hash160)
			break
		}
	}
}

// TestStorePersistsWithoutAnExplicitSave is the behavioural difference
// from the JSON snapshot. There, state between saves lived only in RAM:
// with -saveevery=20, a kill -9 lost up to twenty blocks, and the operator
// had no way to know which. Here every block is committed as it is
// applied, so there is nothing to lose and no save interval to tune.
func TestStorePersistsWithoutAnExplicitSave(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	for _, b := range storeChain(3) {
		idx.ApplyBlock(b)
	}
	// Deliberately no Save() call, and no clean shutdown.

	loaded, _ := reopen(t, store, path)
	if h, _ := loaded.Tip(); h != 2 {
		t.Errorf("tip after unclean restart: got %d, want 2", h)
	}
	if _, ok := mustPosition(t, loaded, "tx2", 0); !ok {
		t.Error("position from the last applied block did not survive: the block was not committed as it was applied")
	}
}

// TestStoreWriteCostDoesNotGrowWithChainLength is the whole reason for
// this layer. Row writes per block must depend only on what that block
// changed, never on how much state already exists.
//
// A snapshot writer passes every other test in this file and fails this
// one, which is exactly the regression worth guarding: it is invisible in
// correctness terms and only shows up as an indexer that gets slower every
// day it runs.
func TestStoreWriteCostDoesNotGrowWithChainLength(t *testing.T) {
	idx, store, _ := storeTestIndex(t)

	chain := storeChain(200)

	for _, b := range chain[:100] {
		idx.ApplyBlock(b)
	}
	early := store.RowsWritten()
	for _, b := range chain[100:] {
		idx.ApplyBlock(b)
	}
	late := store.RowsWritten() - early

	// The two halves apply identical work, so their row counts should
	// match closely. Allow slack for the meta rows, but not for anything
	// proportional to accumulated state.
	if late > early*2 {
		t.Errorf("blocks 100-199 wrote %d rows, blocks 0-99 wrote %d; "+
			"per-block write cost is growing with the size of the index",
			late, early)
	}
	perBlock := float64(late) / 100
	if perBlock > 20 {
		t.Errorf("%.1f row writes per block; a block touching two outputs "+
			"should cost a handful, not a snapshot", perBlock)
	}
}

func TestStoreReflectsUndo(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	chain := storeChain(6)
	for _, b := range chain {
		idx.ApplyBlock(b)
	}
	for h := int64(5); h >= 3; h-- {
		idx.UndoBlock(h)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error during undo: %v", err)
	}

	loaded, _ := reopen(t, store, path)

	if h, hash := loaded.Tip(); h != 2 || hash != "hash2" {
		t.Errorf("tip after undo+reload: got %d/%s, want 2/hash2", h, hash)
	}
	if _, ok := mustPosition(t, loaded, "tx4", 0); ok {
		t.Error("position from an undone block survived the reload: undo is not reaching the store")
	}
	// tx2's P2PKH output was spent by block 3 and must be restored.
	if _, ok := mustUTXO(t, loaded, "tx2", 1); !ok {
		t.Error("UTXO restored by undo is missing after reload: the restore was not persisted")
	}
	assertSamePositions(t, loaded, idx)
	assertSameUTXOs(t, loaded, idx)
	assertSameTokens(t, loaded, idx)
}

// TestStoreDropsPrunedUndoLogs checks the reorg window bounds the file on
// disk, not just the maps in RAM. Retaining every undo log on disk while
// pruning it from memory would grow the state file without bound and, worse,
// let a reload resurrect logs the running index had already decided it could
// not honour -- CanUndo would start answering yes for heights that had been
// discarded before the restart.
func TestStoreDropsPrunedUndoLogs(t *testing.T) {
	idx, store, path := storeTestIndex(t)
	idx.ReorgWindow = 3

	for _, b := range storeChain(20) {
		idx.ApplyBlock(b)
	}

	loaded, store2 := reopen(t, store, path)
	if loaded.PrunedBelow != idx.PrunedBelow {
		t.Errorf("PrunedBelow after reload: got %d, want %d", loaded.PrunedBelow, idx.PrunedBelow)
	}
	// Restore the window so CanUndo compares like with like.
	loaded.ReorgWindow = 3

	for h := int64(0); h < 20; h++ {
		if got, want := loaded.CanUndo(h), idx.CanUndo(h); got != want {
			t.Errorf("CanUndo(%d) after reload: got %v, want %v", h, got, want)
		}
	}
	n, err := store2.CountUndoLogs()
	if err != nil {
		t.Fatalf("CountUndoLogs: %v", err)
	}
	if n > 5 {
		t.Errorf("%d undo logs on disk with a 3-block window; pruning is not reaching the store", n)
	}
}

func TestStoreOrdersSurviveRestart(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	chain := storeChain(2)
	for _, b := range chain {
		idx.ApplyBlock(b)
	}
	pos, ok := mustPosition(t, idx, "tx1", 0)
	if !ok {
		t.Fatal("expected a position to sell")
	}
	order := &Order{
		TxID: "tx1", Vout: 0, Multiplier: pos.Multiplier,
		ScriptSig: makeOrderScriptSig(), PaymentScript: "76a914" + hash160Hex(0x11) + "88ac",
		PaymentValue: 700000000,
	}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	loaded, store2 := reopen(t, store, path)
	got, ok := mustGetOrder(t, loaded, "tx1", 0)
	if !ok {
		t.Fatal("published order did not survive a restart")
	}
	if got.ScriptSig != order.ScriptSig || got.PaymentValue != order.PaymentValue || got.PubKey != pos.PubKey {
		t.Errorf("order changed across restart:\n got %+v\nwant %+v", got, *order)
	}

	// storeChain assigns tx1's position the key from fakePrivKey(0x03) (h=1,
	// prefix 0x02+h%3); CancelOrder now verifies a real signature against
	// it (see cancel_auth.go), not the order's own public script_sig.
	cancelSig := signCancel(t, fakePrivKey(0x03), "tx1", 0, order.ScriptSig, order.CancelNonce)
	if err := loaded.CancelOrder("tx1", 0, cancelSig); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	again, _ := reopen(t, store2, path)
	if _, ok := mustGetOrder(t, again, "tx1", 0); ok {
		t.Error("cancelled order came back after a restart: the delete was not persisted")
	}
}

// TestStoreOrderIsPrunedOnDiskWhenPositionIsSpent covers the automatic
// pruning path, which deletes from the orders map inside ApplyBlock rather
// than through CancelOrder. Missing it would leave a filled order on disk
// to be resurrected on the next restart -- served to takers as open, and
// unfillable, since the position behind it is gone.
func TestStoreOrderIsPrunedOnDiskWhenPositionIsSpent(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	salt := []byte("0123456789abcdef")
	pk := fakePubKey(0x02)
	idx.ApplyBlock(makeBlock("hash0", 0, []RPCTx{{
		TxID: "mint", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pk, 1000, salt, 1.0)},
	}}))
	if err := idx.PublishOrder(&Order{
		TxID: "mint", Vout: 0, Multiplier: 1000,
		ScriptSig: makeOrderScriptSig(), PaymentScript: "76a914" + hash160Hex(0x11) + "88ac",
		PaymentValue: 700000000,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	// A taker fills it: the position is spent into a transfer.
	idx.ApplyBlock(makeBlock("hash1", 1, []RPCTx{{
		TxID: "fill", Vin: []RPCVin{spendVin("mint", 0)},
		Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x03), 1000, 1.0)},
	}}))

	loaded, store2 := reopen(t, store, path)

	// Assert on the stored row, not on GetOrder/ListOrders. Both of those
	// re-check the underlying position and would hide a resurrected order
	// behind "its position is spent" -- so they cannot tell a pruned order
	// from one that is merely being filtered out on every read. The row
	// itself is what leaks: filled orders would accumulate on disk for the
	// life of the index.
	n, err := store2.CountOrders()
	if err != nil {
		t.Fatalf("CountOrders: %v", err)
	}
	if n != 0 {
		t.Errorf("%d order rows on disk after the position was spent, want 0: "+
			"pruning inside ApplyBlock is not reaching the store", n)
	}
	if got := orderCount(t, loaded); got != 0 {
		t.Errorf("%d orders in the reloaded index, want 0", got)
	}
	if _, ok := mustGetOrder(t, loaded, "mint", 0); ok {
		t.Error("a filled order is being served after a restart")
	}
}

func TestStoreResetClearsDisk(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	for _, b := range storeChain(5) {
		idx.ApplyBlock(b)
	}
	idx.Reset()
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error during reset: %v", err)
	}

	loaded, _ := reopen(t, store, path)
	if h, _ := loaded.Tip(); h != -1 {
		t.Errorf("tip after Reset+reload: got %d, want -1", h)
	}
	if n := len(allPositions(t, loaded)); n != 0 {
		t.Errorf("%d positions survived a Reset; the deep-reorg rebuild would "+
			"re-apply the new chain on top of orphaned state", n)
	}
	if n := len(allUTXOs(t, loaded)); n != 0 {
		t.Errorf("%d UTXOs survived a Reset", n)
	}
	if n := len(mustAllTokens(t, loaded)); n != 0 {
		t.Errorf("%d tokens survived a Reset", n)
	}
}

// breakTable removes a table out from under the store, so the next write
// touching it fails the way a damaged or unwritable database would.
// Unlike closing the connection this is recoverable, which is the whole
// point: the dangerous case is a failure the store gets to *retry*.
func breakTable(t *testing.T, store *Store, table string) {
	t.Helper()
	if _, err := store.db.Exec("DROP TABLE " + table); err != nil {
		t.Fatalf("dropping %s: %v", table, err)
	}
}

func healStore(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(storeSchema); err != nil {
		t.Fatalf("restoring schema: %v", err)
	}
}

// TestStoreWriteFailureIsStickyAndStopsWriting is about what happens after
// the database misbehaves once and then works again.
//
// The dangerous outcome is not the failed write, it is the *next* one
// succeeding: block N fails, block N+1 lands, and the stored tip now claims
// N+1 while block N's rows were never written. Nothing afterwards can
// detect that, and a restart resumes from N+1 with a permanent hole. So the
// first failure has to latch and refuse all further writes, leaving disk
// coherent at the last block that fully committed.
//
// This is why the test heals the store before applying more blocks. A test
// that simply closed the connection would pass against an implementation
// with no latch at all, since every later write would fail anyway.
func TestStoreWriteFailureIsStickyAndStopsWriting(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	chain := storeChain(5)
	idx.ApplyBlock(chain[0])
	idx.ApplyBlock(chain[1])
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("unexpected store error: %v", err)
	}

	breakTable(t, store, "positions")
	idx.ApplyBlock(chain[2])
	if idx.StoreErr() == nil {
		t.Fatal("a failed store write was not reported: the indexer would keep serving state it never persisted")
	}
	// The block did not land anywhere. There is no separate in-memory
	// copy to advance any more -- the store is the index -- so a rolled
	// back transaction leaves the tip exactly where it was.
	if h, _ := idx.Tip(); h != 1 {
		t.Errorf("tip after a failed block: got %d, want 1", h)
	}

	// The database is healthy again. Nothing must resume writing to it:
	// block 2 is missing and there is no way to go back and fill it in.
	healStore(t, store)
	idx.ApplyBlock(chain[3])
	idx.ApplyBlock(chain[4])
	if idx.StoreErr() == nil {
		t.Error("the store error cleared itself once writes started working again")
	}

	loaded, _ := reopen(t, store, path)
	if h, _ := loaded.Tip(); h != 1 {
		t.Errorf("stored tip after a write failure: got %d, want 1 (the last block that fully committed)", h)
	}
	if _, ok := mustPosition(t, loaded, "tx4", 0); ok {
		t.Error("a block applied after the store failed was written anyway; the disk state has a hole in it")
	}
}

// TestStoreBatchIsAllOrNothing forces a failure partway through a batch.
// The utxos table disappears, so a block's position writes succeed and its
// UTXO write does not.
//
// Without a transaction around the batch the positions would already be
// committed, and the stored tip -- written last, and therefore not at all --
// would point at the previous block. That is a database describing two
// different chain heights at once, and no later run could notice: the next
// startup would re-apply the block and find its positions mysteriously
// already present.
func TestStoreBatchIsAllOrNothing(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	chain := storeChain(3)
	idx.ApplyBlock(chain[0])
	idx.ApplyBlock(chain[1])

	breakTable(t, store, "utxos")
	idx.ApplyBlock(chain[2])
	if idx.StoreErr() == nil {
		t.Fatal("dropping the utxos table did not fail the write")
	}

	var n int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM positions WHERE key = ?`, "tx2:0").Scan(&n); err != nil {
		t.Fatalf("counting positions: %v", err)
	}
	if n != 0 {
		t.Error("a position from a block whose write failed is on disk anyway: " +
			"the batch is not committing atomically")
	}

	healStore(t, store)
	loaded, _ := reopen(t, store, path)
	if h, _ := loaded.Tip(); h != 1 {
		t.Errorf("stored tip after a partial write: got %d, want 1", h)
	}
	if _, ok := mustPosition(t, loaded, "tx2", 0); ok {
		t.Error("the failed block's position survived the restart")
	}
}

// TestStoreSurvivesMetadataAndOrphans exercises the two column shapes that
// are easy to get wrong in a schema: NULL metadata (most positions) versus
// present metadata (mints that declared one), and an empty Origin, which
// must round-trip as an orphan rather than as a lineage named "".
func TestStoreSurvivesMetadataAndOrphans(t *testing.T) {
	idx, store, path := storeTestIndex(t)

	salt := []byte("0123456789abcdef")
	idx.ApplyBlock(makeBlock("hash0", 0, []RPCTx{{
		TxID: "named", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{
			uapMintVout(0, fakePubKey(0x02), 1000, salt, 1.0),
			opReturnVout(1, wuapPayload("WHIP", "Whippet Token", make([]byte, 32))),
		},
	}, {
		TxID: "plain", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x03), 500, salt, 1.0)},
	}}))
	// An orphan: a transfer whose input the index never saw, so it
	// belongs to no lineage.
	idx.ApplyBlock(makeBlock("hash1", 1, []RPCTx{{
		TxID: "orphan", Vin: []RPCVin{spendVin("unknown", 7)},
		Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x04), 42, 1.0)},
	}}))

	loaded, _ := reopen(t, store, path)

	named, ok := mustPosition(t, loaded, "named", 0)
	if !ok {
		t.Fatal("named mint missing after reload")
	}
	if named.Metadata == nil {
		t.Fatal("metadata lost across reload")
	}
	if named.Metadata.Ticker != "WHIP" || named.Metadata.Name != "Whippet Token" {
		t.Errorf("metadata changed across reload: %+v", *named.Metadata)
	}
	plain, ok := mustPosition(t, loaded, "plain", 0)
	if !ok {
		t.Fatal("plain mint missing after reload")
	}
	if plain.Metadata != nil {
		t.Errorf("a mint with no metadata came back with %+v; NULL is not round-tripping as nil", *plain.Metadata)
	}
	orphan, ok := mustPosition(t, loaded, "orphan", 0)
	if !ok {
		t.Fatal("orphan transfer missing after reload")
	}
	if orphan.Origin != "" {
		t.Errorf("orphan came back with origin %q, want empty", orphan.Origin)
	}
	for _, tok := range mustAllTokens(t, loaded) {
		if tok.Origin == "" {
			t.Error("an origin-less position was reported as a token after reload")
		}
	}
	assertSameTokens(t, loaded, idx)
}

// assertSameTokens compares the two indexes' /tokens views. The lineage
// aggregates are derived state, so this is really a check that whatever
// the store does on load reproduces them.
func assertSameTokens(t *testing.T, got, want *Index) {
	t.Helper()
	assertSameSet(t, "token", "after reload",
		mustAllTokens(t, got), mustAllTokens(t, want),
		func(tok TokenInfo) string { return tok.Origin },
		diffComparable[TokenInfo]("token", "after reload"))
}

func tipOf(idx *Index) string {
	h, hash := idx.Tip()
	return fmt.Sprintf("%d/%s", h, hash)
}

// TestOnlyAMintCanNameALineage pins the is_mint guard in the token query.
//
// Under normal operation a lineage's origin key always names its own mint,
// so the guard never fires -- which is exactly why it is worth a test: an
// invariant nothing exercises is one a later change can quietly drop. The
// position seeded here is one the chain cannot produce (ApplyBlock attaches
// metadata to mints only), and that is the point. It stands in for any
// future path that lets a non-mint row occupy an origin key, and asserts
// that such a row still cannot rename somebody else's token.
func TestOnlyAMintCanNameALineage(t *testing.T) {
	idx, _, _ := storeTestIndex(t)

	origin := "impostor:0"
	seedPosition(t, idx, &Position{
		TxID: "impostor", Vout: 0, PubKey: "02aa", Multiplier: 1000,
		Value: 100, IsMint: false, Height: 1, Origin: origin,
		Metadata: &TokenMetadata{Ticker: "FAKE", Name: "Not Your Token"},
	})

	tok, ok := mustToken(t, idx, origin)
	if !ok {
		t.Fatal("the seeded position should form a lineage")
	}
	if tok.Ticker != "" || tok.Name != "" {
		t.Errorf("a non-mint row named the lineage: ticker=%q name=%q", tok.Ticker, tok.Name)
	}
}

// readMetaValue reads a single meta row directly, bypassing loadMeta (which
// only knows about the keys it was written to understand, and would
// silently ignore schema_version like it does any other key it doesn't
// recognise).
func readMetaValue(t *testing.T, store *Store, key string) (string, bool) {
	t.Helper()
	var value string
	err := store.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading meta key %q: %v", key, err)
	}
	return value, true
}

// TestStoreStampsSchemaVersionOnCreate pins down the common case: a brand
// new database gets today's schema version recorded, so a future binary
// with a different CurrentSchemaVersion has something to compare against.
func TestStoreStampsSchemaVersionOnCreate(t *testing.T) {
	_, store, _ := storeTestIndex(t)

	got, ok := readMetaValue(t, store, schemaVersionKey)
	if !ok {
		t.Fatal("schema_version was not written when the database was created")
	}
	if want := fmt.Sprint(CurrentSchemaVersion); got != want {
		t.Errorf("schema_version after create: got %q, want %q", got, want)
	}
}

// TestStoreAcceptsPreVersioningDatabase covers a database that predates
// this check entirely: no schema_version row, but real data in it. Opening
// it must succeed -- refusing every database that existed before this
// feature shipped would not be a compatibility policy, it would be
// bricking every deployment on upgrade -- and it must come back stamped, so
// the *next* open has something to compare against.
func TestStoreAcceptsPreVersioningDatabase(t *testing.T) {
	idx, store, path := storeTestIndex(t)
	for _, b := range storeChain(2) {
		idx.ApplyBlock(b)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error during apply: %v", err)
	}

	// Simulate a database written before schema_version existed by
	// deleting the row this run's OpenStore already wrote.
	if _, err := store.db.Exec(`DELETE FROM meta WHERE key = ?`, schemaVersionKey); err != nil {
		t.Fatalf("removing schema_version: %v", err)
	}

	loaded, reopened := reopen(t, store, path)
	if h, _ := loaded.Tip(); h != 1 {
		t.Errorf("tip after reopening a pre-versioning database: got %d, want 1", h)
	}
	got, ok := readMetaValue(t, reopened, schemaVersionKey)
	if !ok {
		t.Fatal("a pre-versioning database was not stamped on open")
	}
	if want := fmt.Sprint(CurrentSchemaVersion); got != want {
		t.Errorf("schema_version stamped on a pre-versioning database: got %q, want %q", got, want)
	}
}

// TestStoreRefusesMismatchedSchemaVersion is the actual point of this
// feature: a database whose recorded version does not match what this
// binary understands must be refused at open time, loudly and with both
// version numbers in the message, rather than let the mismatch surface
// later as a confusing SQL error or a silent misread.
func TestStoreRefusesMismatchedSchemaVersion(t *testing.T) {
	_, store, path := storeTestIndex(t)

	if err := store.stampSchemaVersion(CurrentSchemaVersion + 1); err != nil {
		t.Fatalf("stamping a future schema version: %v", err)
	}
	store.Close()

	_, err := OpenStore(path)
	if err == nil {
		t.Fatal("opening a database with a newer schema version did not fail")
	}
	wantOld := fmt.Sprint(CurrentSchemaVersion)
	wantNew := fmt.Sprint(CurrentSchemaVersion + 1)
	if !strings.Contains(err.Error(), wantNew) || !strings.Contains(err.Error(), wantOld) {
		t.Errorf("schema version mismatch error %q does not name both versions (%s and %s)",
			err.Error(), wantOld, wantNew)
	}
}

// TestStoreResetRestampsSchemaVersion checks that truncate() -- which wipes
// the meta table along with everything else -- does not leave the database
// looking pre-versioning afterwards. A reset database and a freshly created
// one are otherwise indistinguishable, and the freshly created one always
// carries a version.
func TestStoreResetRestampsSchemaVersion(t *testing.T) {
	idx, store, path := storeTestIndex(t)
	for _, b := range storeChain(2) {
		idx.ApplyBlock(b)
	}
	idx.Reset()
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error during reset: %v", err)
	}

	got, ok := readMetaValue(t, store, schemaVersionKey)
	if !ok {
		t.Fatal("schema_version did not survive Reset")
	}
	if want := fmt.Sprint(CurrentSchemaVersion); got != want {
		t.Errorf("schema_version after Reset: got %q, want %q", got, want)
	}

	// And it must actually be usable afterwards, not just present -- a
	// reopen must not trip the mismatch check on the database's own reset
	// state.
	reloaded, _ := reopen(t, store, path)
	if h, _ := reloaded.Tip(); h != -1 {
		t.Errorf("tip after reopening a reset database: got %d, want -1", h)
	}
}
