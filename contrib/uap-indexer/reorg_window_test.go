package main

import (
	"fmt"
	"testing"
)

// The undo log (HeightLog) used to grow forever: one entry per block,
// each retaining the keys created by that block and a full *UTXO record
// for every UTXO it spent. Nothing removed entries except UndoBlock, so a
// node that never reorged accumulated all of it -- unbounded memory, and
// unbounded state file, for history that can never be used. Undo is only
// reachable from the tip backwards, so anything older than the deepest
// reorg worth surviving is dead weight.
//
// Bounding it introduces a real failure mode that must not be silent: a
// reorg deeper than the retained window cannot be undone at all. The
// index is then holding state from an orphaned chain, and the only honest
// response is to rebuild. These tests pin both halves -- that the log is
// actually bounded, and that exceeding it is detected rather than ignored.

func applyEmptyBlockAt(idx *Index, height int64) *RPCBlock {
	b := makeBlock(fmt.Sprintf("h%d", height), height, []RPCTx{{
		TxID: txid(fmt.Sprintf("tx%d", height)),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{ordinaryVout(0, 1.0)},
	}})
	idx.ApplyBlock(b)
	return b
}

func TestHeightLogIsBoundedByReorgWindow(t *testing.T) {
	idx := NewIndex()
	idx.ReorgWindow = 10

	const blocks = 200
	for h := int64(0); h < blocks; h++ {
		applyEmptyBlockAt(idx, h)
	}

	idx.mu.RLock()
	logLen := undoLogCount(t, idx)
	heightsLen := heightCount(t, idx)
	idx.mu.RUnlock()

	// window + the tip itself; allow the tip block to be extra.
	if max := int(idx.ReorgWindow) + 1; logLen > max {
		t.Errorf("HeightLog holds %d entries after %d blocks, want at most %d", logLen, blocks, max)
	}
	if max := int(idx.ReorgWindow) + 1; heightsLen > max {
		t.Errorf("Heights holds %d entries after %d blocks, want at most %d", heightsLen, blocks, max)
	}
}

func TestUndoStillWorksInsideTheWindow(t *testing.T) {
	idx := NewIndex()
	idx.ReorgWindow = 10
	pubkey := fakePubKey(0x02)
	for h := int64(0); h < 50; h++ {
		applyEmptyBlockAt(idx, h)
	}
	// A block well inside the window, carrying real state to roll back.
	idx.ApplyBlock(makeBlock("h50", 50, []RPCTx{{
		TxID: txid("mint50"),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey, 7, 1.0)},
	}}))
	for h := int64(51); h < 55; h++ {
		applyEmptyBlockAt(idx, h)
	}

	for h := int64(54); h >= 50; h-- {
		if !idx.CanUndo(h) {
			t.Fatalf("CanUndo(%d) is false, but %d is inside the %d-block window below tip %d",
				h, h, idx.ReorgWindow, 54)
		}
		idx.UndoBlock(h)
	}

	if _, ok := mustPosition(t, idx, txid("mint50"), 0); ok {
		t.Error("position from an undone block survived")
	}
	if tip, _ := idx.Tip(); tip != 49 {
		t.Errorf("tip after rolling back to 49: got %d", tip)
	}
}

func TestCanUndoIsFalseBeyondTheWindow(t *testing.T) {
	idx := NewIndex()
	idx.ReorgWindow = 10
	for h := int64(0); h < 100; h++ {
		applyEmptyBlockAt(idx, h)
	}

	// Heights well below the window have had their undo log discarded.
	for _, h := range []int64{0, 50, 80} {
		if idx.CanUndo(h) {
			t.Errorf("CanUndo(%d) is true, but its undo log was pruned (tip 99, window 10)", h)
		}
	}
	if !idx.CanUndo(99) {
		t.Error("CanUndo(99) is false for the tip itself")
	}
	if !idx.CanUndo(95) {
		t.Error("CanUndo(95) is false, but it is inside the window")
	}
}

// TestUndoBeyondWindowDoesNotSilentlySucceed is the important one. Before
// bounding, UndoBlock on a height with no log entry deleted the Heights
// record and returned, reporting nothing -- so a caller walking back
// through a deep reorg would "successfully" undo blocks it had no data
// for, and carry on with state from the orphaned chain.
func TestUndoBeyondWindowDoesNotSilentlySucceed(t *testing.T) {
	idx := NewIndex()
	idx.ReorgWindow = 10
	pubkey := fakePubKey(0x02)
	idx.ApplyBlock(makeBlock("h0", 0, []RPCTx{{
		TxID: txid("mint0"),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey, 7, 1.0)},
	}}))
	for h := int64(1); h < 100; h++ {
		applyEmptyBlockAt(idx, h)
	}

	if idx.CanUndo(0) {
		t.Fatal("precondition: height 0 should be outside the window")
	}
	// The position from the pruned block must still be there afterwards:
	// the caller has to learn it cannot roll back, not be told it did.
	idx.UndoBlock(0)
	if _, ok := mustPosition(t, idx, txid("mint0"), 0); !ok {
		t.Error("UndoBlock past the window removed state it had no undo log for")
	}
}

// TestResetClearsEverything covers the recovery path: when a reorg is
// deeper than the window, the only correct move is to discard the index
// and resync. A Reset that left anything behind would silently mix two
// chains.
func TestResetClearsEverything(t *testing.T) {
	idx := NewIndex()
	pubkey := fakePubKey(0x02)
	idx.ApplyBlock(makeBlock("h0", 0, []RPCTx{{
		TxID: txid("mint0"),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey, 7, 1.0), p2pkhVoutFor(1, 0x05, 2.0)},
	}}))
	if err := idx.PublishOrder(&Order{
		TxID: txid("mint0"), Vout: 0, Multiplier: 7,
		ScriptSig: makeOrderScriptSig(), PaymentScript: "76a914" + hash160Hex(1) + "88ac",
		PaymentValue: 5000000,
	}); err != nil {
		t.Fatal(err)
	}

	idx.Reset()

	s := mustStatus(t, idx)
	if s.TipHeight != -1 || s.TipHash != "" {
		t.Errorf("tip after Reset: got %d/%q, want -1/\"\"", s.TipHeight, s.TipHash)
	}
	if s.PositionCount != 0 {
		t.Errorf("positions after Reset: got %d, want 0", s.PositionCount)
	}
	if s.OrderCount != 0 {
		t.Errorf("orders after Reset: got %d, want 0", s.OrderCount)
	}
	if got := mustAllTokens(t, idx); len(got) != 0 {
		t.Errorf("lineages after Reset: got %d, want 0", len(got))
	}
	if _, ok := mustUTXO(t, idx, txid("mint0"), 1); ok {
		t.Error("UTXO survived Reset")
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if undoLogCount(t, idx) != 0 || heightCount(t, idx) != 0 ||
		positionCount(t, idx) != 0 || utxoCount(t, idx) != 0 {
		t.Errorf("index not fully cleared: heightlog=%d heights=%d bypubkey=%d byhash160=%d",
			undoLogCount(t, idx), heightCount(t, idx), positionCount(t, idx), utxoCount(t, idx))
	}
	if idx.PrunedBelow != 0 {
		t.Errorf("PrunedBelow after Reset: got %d, want 0", idx.PrunedBelow)
	}
}

// TestPruningSurvivesSaveLoad verifies PrunedBelow persists. If it reset
// to zero on restart, CanUndo would start claiming it could roll back
// into heights whose log was discarded before the restart.
func TestPruningSurvivesSaveLoad(t *testing.T) {
	path := t.TempDir() + "/state.sqlite"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.ReorgWindow = 10
	for h := int64(0); h < 100; h++ {
		applyEmptyBlockAt(idx, h)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatal(err)
	}

	store2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	loaded, err := store2.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PrunedBelow != idx.PrunedBelow {
		t.Errorf("PrunedBelow across save/load: got %d, want %d", loaded.PrunedBelow, idx.PrunedBelow)
	}
	if loaded.CanUndo(50) {
		t.Error("after reload, CanUndo(50) is true for a height pruned before the save")
	}
}
