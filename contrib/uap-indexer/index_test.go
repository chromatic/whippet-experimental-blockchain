package main

import (
	"encoding/hex"
	"os"
	"testing"
)

// Helper: build a synthetic RPCVout with a UAP mint script.
func uapMintVout(n uint32, pubkey []byte, multiplier int64, salt []byte, value float64) RPCVout {
	return voutWithScript(n, value, mintScriptBytes(pubkey, multiplier, salt))
}

// Helper: build a synthetic RPCVout with a UAP transfer script.
func uapTransferVout(n uint32, pubkey []byte, multiplier int64, value float64) RPCVout {
	return voutWithScript(n, value, transferScriptBytes(pubkey, multiplier))
}

// Helper: build a WUAP metadata payload as ParseMetadata expects it --
// OP_RETURN followed by five pushes: magic, version, ticker, name, hash.
func wuapPayload(ticker, name string, hash []byte) []byte {
	out := []byte{0x6a}
	out = append(out, buildPush([]byte("WUAP"))...)
	out = append(out, buildPush([]byte{0x01})...)
	out = append(out, buildPush([]byte(ticker))...)
	out = append(out, buildPush([]byte(name))...)
	out = append(out, buildPush(hash)...)
	return out
}

// Helper: build a synthetic RPCVout carrying a raw OP_RETURN script.
func opReturnVout(n uint32, script []byte) RPCVout {
	return voutWithScript(n, 0, script)
}

// Helper: build a synthetic RPCVout with an ordinary (non-UAP) script:
// P2PKH over an all-zero hash160.
func ordinaryVout(n uint32, value float64) RPCVout {
	return voutWithScript(n, value, p2pkhScriptBytes(make([]byte, 20)))
}

// Helper: build a synthetic RPCVin that spends an earlier output.
func spendVin(txid string, vout uint32) RPCVin {
	return RPCVin{TxID: txid, Vout: vout}
}

// Helper: build a synthetic coinbase input.
func coinbaseVin() RPCVin {
	return RPCVin{Coinbase: "01"}
}

// Helper: build a minimal RPCBlock.
func makeBlock(hash string, height int64, txs []RPCTx) *RPCBlock {
	return &RPCBlock{
		Hash:   hash,
		Height: height,
		Tx:     txs,
	}
}

// TestApplyBlockCreatesMintPositions verifies that ApplyBlock creates Position
// entries for UAP mint outputs, with correct fields populated.
func TestApplyBlockCreatesMintPositions(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")
	vout := uapMintVout(0, pubkey1, 1000, salt, 5.5)

	block := makeBlock("hash1", 100, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{vout},
	}})

	idx.ApplyBlock(block)

	// Verify position was created with correct values.
	pos, ok := mustPosition(t, idx, "tx1", 0)
	if !ok {
		t.Fatal("position not found after ApplyBlock")
	}
	if pos.TxID != "tx1" {
		t.Errorf("TxID mismatch: got %q, want %q", pos.TxID, "tx1")
	}
	if pos.Vout != 0 {
		t.Errorf("Vout mismatch: got %d, want 0", pos.Vout)
	}
	if pos.Multiplier != 1000 {
		t.Errorf("Multiplier mismatch: got %d, want 1000", pos.Multiplier)
	}
	if pos.Value != 550000000 {
		t.Errorf("Value mismatch: got %d, want 550000000 satoshis", pos.Value)
	}
	if !pos.IsMint {
		t.Error("IsMint should be true for a mint output")
	}
	if pos.Height != 100 {
		t.Errorf("Height mismatch: got %d, want 100", pos.Height)
	}
	if pos.Spent {
		t.Error("Spent should be false for a newly created position")
	}
	if pos.PubKey != hex.EncodeToString(pubkey1) {
		t.Errorf("PubKey mismatch")
	}
}

// TestApplyBlockCreatesTransferPositions verifies ApplyBlock creates Position
// entries for UAP transfer outputs.
func TestApplyBlockCreatesTransferPositions(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x03)
	vout := uapTransferVout(1, pubkey1, 42, 3.14)

	block := makeBlock("hash1", 50, []RPCTx{{
		TxID: "tx_transfer",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{vout},
	}})

	idx.ApplyBlock(block)

	pos, ok := mustPosition(t, idx, "tx_transfer", 1)
	if !ok {
		t.Fatal("position not found")
	}
	if pos.IsMint {
		t.Error("IsMint should be false for a transfer output")
	}
	if pos.Multiplier != 42 {
		t.Errorf("Multiplier mismatch: got %d, want 42", pos.Multiplier)
	}
}

// TestApplyBlockIgnoresNonUAPOutputs verifies that ApplyBlock does not create
// positions for ordinary (non-UAP) outputs.
func TestApplyBlockIgnoresNonUAPOutputs(t *testing.T) {
	idx := NewIndex()
	ordinaryVout1 := ordinaryVout(0, 1.0)
	ordinaryVout2 := ordinaryVout(1, 2.0)

	block := makeBlock("hash1", 1, []RPCTx{{
		TxID: "ordinary_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{ordinaryVout1, ordinaryVout2},
	}})

	idx.ApplyBlock(block)

	// No positions should be created.
	if positionCount(t, idx) != 0 {
		t.Errorf("expected 0 positions, got %d", positionCount(t, idx))
	}
}

// TestApplyBlockSpendsPreviousPosition verifies that when a tx spends an
// earlier UAP output, that position is marked as spent with correct details.
func TestApplyBlockSpendsPreviousPosition(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Block 1: create a UAP position
	block1 := makeBlock("hash1", 10, []RPCTx{{
		TxID: "create_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block1)

	// Verify it's unspent
	pos, ok := mustPosition(t, idx, "create_tx", 0)
	if !ok {
		t.Fatal("position not created")
	}
	if pos.Spent {
		t.Error("position should be unspent after creation")
	}

	// Block 2: spend that position
	block2 := makeBlock("hash2", 11, []RPCTx{{
		TxID: "spend_tx",
		Vin: []RPCVin{
			{TxID: "create_tx", Vout: 0},
		},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}})
	idx.ApplyBlock(block2)

	// Verify position is now marked spent with correct metadata
	pos, ok = mustPosition(t, idx, "create_tx", 0)
	if !ok {
		t.Fatal("position should still exist")
	}
	if !pos.Spent {
		t.Error("position should be marked spent")
	}
	if pos.SpentTxID != "spend_tx" {
		t.Errorf("SpentTxID mismatch: got %q, want %q", pos.SpentTxID, "spend_tx")
	}
	if pos.SpentHeight != 11 {
		t.Errorf("SpentHeight mismatch: got %d, want 11", pos.SpentHeight)
	}
}

// TestApplyBlockIgnoresCoinbaseInputs verifies that coinbase inputs don't
// try to spend any positions (they can't).
func TestApplyBlockIgnoresCoinbaseInputs(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Create a position
	block1 := makeBlock("hash1", 1, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block1)

	// Next block with only a coinbase input (no spending)
	block2 := makeBlock("hash2", 2, []RPCTx{{
		TxID: "coinbase_only",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{ordinaryVout(0, 1.0)},
	}})
	idx.ApplyBlock(block2)

	// Original position should still be unspent
	pos, ok := mustPosition(t, idx, "tx1", 0)
	if !ok {
		t.Fatal("position vanished")
	}
	if pos.Spent {
		t.Error("coinbase inputs should not mark positions as spent")
	}
}

// TestApplyBlockRecordsBlockHash verifies that ApplyBlock records the block
// hash at its height in the Heights map.
func TestApplyBlockRecordsBlockHash(t *testing.T) {
	idx := NewIndex()
	block := makeBlock("blockhash123", 42, []RPCTx{})
	idx.ApplyBlock(block)

	hash, ok := idx.HashAtHeight(42)
	if !ok {
		t.Fatal("hash not found at height")
	}
	if hash != "blockhash123" {
		t.Errorf("hash mismatch: got %q, want %q", hash, "blockhash123")
	}
}

// TestApplyBlockUpdatesHeightLog verifies that ApplyBlock records which
// positions were created and spent in the HeightLog, for undo purposes.
func TestApplyBlockUpdatesHeightLog(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Create two positions
	block1 := makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "tx1",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx2",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 2000, salt, 2.0)},
		},
	})
	idx.ApplyBlock(block1)

	change, ok := undoLogAt(t, idx, 1)
	if !ok {
		t.Fatal("height log entry not found")
	}
	if len(change.Created) != 2 {
		t.Errorf("expected 2 created positions, got %d", len(change.Created))
	}
	if len(change.SpentKeys) != 0 {
		t.Errorf("expected 0 spent positions, got %d", len(change.SpentKeys))
	}
}

// TestUndoBlockRemovesCreatedPositions verifies that UndoBlock removes
// positions that were created in that block.
func TestUndoBlockRemovesCreatedPositions(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Create a position
	block := makeBlock("hash1", 100, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block)

	// Verify it exists
	if _, ok := mustPosition(t, idx, "tx1", 0); !ok {
		t.Fatal("position should exist after ApplyBlock")
	}

	// Undo that block
	idx.UndoBlock(100)

	// Verify it's gone
	if _, ok := mustPosition(t, idx, "tx1", 0); ok {
		t.Error("position should be removed after UndoBlock")
	}
}

// TestUndoBlockUnmarksSpentPositions verifies that UndoBlock unmarks positions
// that were marked as spent in that block.
func TestUndoBlockUnmarksSpentPositions(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Block 1: create a position
	block1 := makeBlock("hash1", 10, []RPCTx{{
		TxID: "create_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block1)

	// Block 2: spend it
	block2 := makeBlock("hash2", 11, []RPCTx{{
		TxID: "spend_tx",
		Vin:  []RPCVin{spendVin("create_tx", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}})
	idx.ApplyBlock(block2)

	// Verify it's marked spent
	pos, ok := mustPosition(t, idx, "create_tx", 0)
	if !ok || !pos.Spent {
		t.Fatal("position should be spent")
	}

	// Undo block 2
	idx.UndoBlock(11)

	// Verify it's now unspent with cleared metadata
	pos, ok = mustPosition(t, idx, "create_tx", 0)
	if !ok {
		t.Fatal("position should still exist")
	}
	if pos.Spent {
		t.Error("position should be unmarked as spent after UndoBlock")
	}
	if pos.SpentTxID != "" {
		t.Errorf("SpentTxID should be cleared, got %q", pos.SpentTxID)
	}
	if pos.SpentHeight != 0 {
		t.Errorf("SpentHeight should be cleared, got %d", pos.SpentHeight)
	}
}

// TestUndoBlockRemovesBlockHashEntry verifies that UndoBlock removes the
// recorded block hash.
func TestUndoBlockRemovesBlockHashEntry(t *testing.T) {
	idx := NewIndex()
	block := makeBlock("hash1", 50, []RPCTx{})
	idx.ApplyBlock(block)

	// Hash should exist
	if _, ok := idx.HashAtHeight(50); !ok {
		t.Fatal("hash should exist after ApplyBlock")
	}

	// Undo it
	idx.UndoBlock(50)

	// Hash should be gone
	if _, ok := idx.HashAtHeight(50); ok {
		t.Error("hash should be removed after UndoBlock")
	}
}

// TestUndoBlockWithNoLogEntry verifies that UndoBlock does nothing at all
// for a height it has no undo log for -- in particular, that it leaves the
// Heights entry alone.
//
// This test previously asserted the opposite: that the Heights entry was
// removed. That became actively harmful once the undo log was bounded.
// Deleting the hash makes HashAtHeight report "no opinion" at that height,
// which is exactly the condition syncOnce's reorg walk treats as "we agree
// with the node, stop looking". An index holding state from an orphaned
// chain and unable to roll back would therefore erase the one piece of
// evidence saying so, and then report itself in sync.
//
// Leaving the hash in place keeps the disagreement visible. Callers use
// CanUndo to tell "nothing to undo here" from "cannot undo here", and
// rebuild in the second case.
func TestUndoBlockWithNoLogEntry(t *testing.T) {
	idx := NewIndex()

	// A hash with no accompanying log entry: undo data we no longer have.
	seedHeight(t, idx, 42, "orphaned_hash")

	if idx.CanUndo(42) {
		t.Fatal("precondition: CanUndo should be false with no log entry")
	}

	idx.UndoBlock(42) // must not panic

	if _, ok := idx.HashAtHeight(42); !ok {
		t.Error("UndoBlock discarded the block hash for a height it could not undo, " +
			"hiding the disagreement from the reorg detector")
	}
}

// TestReorgSimpleChainRollback tests the scenario: apply blocks 1, 2, 3,
// then undo 3 and 2, verifying that the state exactly matches what it was
// after block 1.
func TestReorgSimpleChainRollback(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	// Block 1: create position A
	block1 := makeBlock("hash1", 100, []RPCTx{{
		TxID: "tx_a",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block1)

	// Block 2: create position B
	block2 := makeBlock("hash2", 101, []RPCTx{{
		TxID: "tx_b",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey2, 2000, salt, 2.0)},
	}})
	idx.ApplyBlock(block2)

	// Block 3: create position C
	block3 := makeBlock("hash3", 102, []RPCTx{{
		TxID: "tx_c",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 3000, salt, 3.0)},
	}})
	idx.ApplyBlock(block3)

	// Verify we have A, B, C
	if positionCount(t, idx) != 3 {
		t.Fatalf("expected 3 positions before reorg, got %d", positionCount(t, idx))
	}

	// Undo blocks 3 and 2
	idx.UndoBlock(102)
	idx.UndoBlock(101)

	// Verify state matches after block 1
	if positionCount(t, idx) != 1 {
		t.Errorf("expected 1 position after reorg, got %d", positionCount(t, idx))
	}
	if _, ok := mustPosition(t, idx, "tx_a", 0); !ok {
		t.Error("position A should still exist")
	}
	if _, ok := mustPosition(t, idx, "tx_b", 0); ok {
		t.Error("position B should have been removed")
	}
	if _, ok := mustPosition(t, idx, "tx_c", 0); ok {
		t.Error("position C should have been removed")
	}

	// Verify block hashes
	if hash, ok := idx.HashAtHeight(100); !ok || hash != "hash1" {
		t.Error("hash at height 100 should still be hash1")
	}
	if _, ok := idx.HashAtHeight(101); ok {
		t.Error("hash at height 101 should be removed")
	}
	if _, ok := idx.HashAtHeight(102); ok {
		t.Error("hash at height 102 should be removed")
	}
}

// TestReorgRespendingPositionFromEarlierBlock tests the critical case: apply
// blocks 1 and 2, where block 2 spends a position created in block 1. Then
// undo block 2, verifying that the position is unmarked as spent.
func TestReorgRespendingPositionFromEarlierBlock(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	// Block 1: create position
	block1 := makeBlock("hash1", 100, []RPCTx{{
		TxID: "tx_create",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block1)

	// Verify position exists and is unspent
	pos, ok := mustPosition(t, idx, "tx_create", 0)
	if !ok || pos.Spent {
		t.Fatal("position should exist and be unspent after block 1")
	}

	// Block 2: spend the position from block 1
	block2 := makeBlock("hash2", 101, []RPCTx{{
		TxID: "tx_spend",
		Vin:  []RPCVin{spendVin("tx_create", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey2, 1000, 0.5)},
	}})
	idx.ApplyBlock(block2)

	// Verify position is marked spent
	pos, ok = mustPosition(t, idx, "tx_create", 0)
	if !ok || !pos.Spent {
		t.Fatal("position should be marked spent after block 2")
	}
	if pos.SpentTxID != "tx_spend" || pos.SpentHeight != 101 {
		t.Fatal("spent metadata should be recorded")
	}

	// Undo block 2
	idx.UndoBlock(101)

	// Verify position is back to unspent with cleared metadata
	pos, ok = mustPosition(t, idx, "tx_create", 0)
	if !ok {
		t.Fatal("position should still exist")
	}
	if pos.Spent {
		t.Error("position should be unmarked as spent after reorg")
	}
	if pos.SpentTxID != "" || pos.SpentHeight != 0 {
		t.Error("spent metadata should be cleared")
	}

	// Verify the spending position (from block 2) is gone
	if _, ok := mustPosition(t, idx, "tx_spend", 0); ok {
		t.Error("position created in block 2 should be removed")
	}
}

// TestHashAtHeightPresentAndAbsent tests that HashAtHeight returns the
// recorded hash when present, and indicates absence when not present.
func TestHashAtHeightPresentAndAbsent(t *testing.T) {
	idx := NewIndex()

	// Record hashes at heights 10, 20, 30
	seedHeight(t, idx, 10, "hash_10")
	seedHeight(t, idx, 20, "hash_20")
	seedHeight(t, idx, 30, "hash_30")

	// Test present heights
	tests := []struct {
		height   int64
		wantHash string
		wantOk   bool
	}{
		{10, "hash_10", true},
		{20, "hash_20", true},
		{30, "hash_30", true},
		{15, "", false},
		{50, "", false},
		{-1, "", false},
	}

	for _, tt := range tests {
		hash, ok := idx.HashAtHeight(tt.height)
		if ok != tt.wantOk {
			t.Errorf("HashAtHeight(%d): got ok=%v, want %v", tt.height, ok, tt.wantOk)
		}
		if ok && hash != tt.wantHash {
			t.Errorf("HashAtHeight(%d): got %q, want %q", tt.height, hash, tt.wantHash)
		}
	}
}

// TestPositionsForPubKeyAll tests that PositionsForPubKey returns all
// positions (spent and unspent) for a given pubkey.
func TestPositionsForPubKeyAll(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	// Create two positions for pubkey1, one for pubkey2
	block1 := makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "tx_1a",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx_1b",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 2000, salt, 2.0)},
		},
		{
			TxID: "tx_2",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey2, 3000, salt, 3.0)},
		},
	})
	idx.ApplyBlock(block1)

	// Spend one of pubkey1's positions (the spend tx also creates a new position for pubkey1)
	block2 := makeBlock("hash2", 2, []RPCTx{{
		TxID: "spend",
		Vin:  []RPCVin{spendVin("tx_1a", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}})
	idx.ApplyBlock(block2)

	// Get all positions for pubkey1 (spent and unspent)
	// We should have: tx_1a:0 (spent), tx_1b:0 (unspent), spend:0 (unspent)
	pkHex := hex.EncodeToString(pubkey1)
	positions := mustPositionsForPubKey(t, idx, pkHex, false)

	if len(positions) != 3 {
		t.Fatalf("expected 3 positions for pubkey1, got %d", len(positions))
	}

	// Verify only one is spent
	spent := 0
	for _, pos := range positions {
		if pos.Spent {
			spent++
		}
	}
	if spent != 1 {
		t.Errorf("expected 1 spent position, got %d", spent)
	}
}

// TestPositionsForPubKeyUnspentOnly tests that PositionsForPubKey with
// unspentOnly=true returns only unspent positions.
func TestPositionsForPubKeyUnspentOnly(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Create two positions
	block1 := makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "tx1",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx2",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 2000, salt, 2.0)},
		},
	})
	idx.ApplyBlock(block1)

	// Spend one of them
	block2 := makeBlock("hash2", 2, []RPCTx{{
		TxID: "spend",
		Vin:  []RPCVin{spendVin("tx1", 0)},
		Vout: []RPCVout{},
	}})
	idx.ApplyBlock(block2)

	// Get all positions (unspentOnly=false)
	pkHex := hex.EncodeToString(pubkey1)
	allPositions := mustPositionsForPubKey(t, idx, pkHex, false)
	if len(allPositions) != 2 {
		t.Errorf("expected 2 total positions, got %d", len(allPositions))
	}

	// Get only unspent positions (unspentOnly=true)
	unspentPositions := mustPositionsForPubKey(t, idx, pkHex, true)
	if len(unspentPositions) != 1 {
		t.Errorf("expected 1 unspent position, got %d", len(unspentPositions))
	}
	if unspentPositions[0].Spent {
		t.Error("unspent-only query should not return spent positions")
	}
}

// TestPositionsForPubKeyUnknownPubKey tests that PositionsForPubKey returns
// an empty slice for an unknown pubkey (no panic).
func TestPositionsForPubKeyUnknownPubKey(t *testing.T) {
	idx := NewIndex()
	positions := mustPositionsForPubKey(t, idx, "unknown", false)
	if len(positions) != 0 {
		t.Errorf("expected 0 positions for unknown pubkey, got %d", len(positions))
	}
}

// TestPositionLookup tests basic position lookup by outpoint.
func TestPositionLookup(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	block := makeBlock("hash1", 1, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block)

	// Lookup existing position
	pos, ok := mustPosition(t, idx, "tx1", 0)
	if !ok {
		t.Fatal("position not found")
	}
	if pos.Multiplier != 1000 {
		t.Errorf("multiplier mismatch: got %d, want 1000", pos.Multiplier)
	}

	// Lookup non-existent position
	_, ok = mustPosition(t, idx, "tx1", 999)
	if ok {
		t.Error("expected Position to return ok=false for non-existent vout")
	}

	_, ok = mustPosition(t, idx, "nonexistent", 0)
	if ok {
		t.Error("expected Position to return ok=false for non-existent txid")
	}
}

// TestStateRoundTripsThroughTheStore tests round-trip persistence: apply
// blocks through a store, reopen it, and verify the reloaded index matches.
func TestStateRoundTripsThroughTheStore(t *testing.T) {
	dbPath := t.TempDir() + "/state.sqlite"
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	idx1, err := store.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	// Block 1: create positions
	block1 := makeBlock("hash1", 100, []RPCTx{
		{
			TxID: "tx_a",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx_b",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapTransferVout(0, pubkey2, 2000, 2.0)},
		},
	})
	idx1.ApplyBlock(block1)

	// Block 2: spend position A
	block2 := makeBlock("hash2", 101, []RPCTx{{
		TxID: "tx_spend",
		Vin:  []RPCVin{spendVin("tx_a", 0)},
		Vout: []RPCVout{},
	}})
	idx1.ApplyBlock(block2)

	// Add an order. Published through the real path rather than poked
	// into the map, because it is PublishOrder that writes it to the
	// store -- a direct map write would test nothing about persistence.
	order := &Order{
		TxID:          "tx_b",
		Vout:          0,
		Multiplier:    2000,
		ScriptSig:     makeOrderScriptSig(),
		PaymentScript: "5678",
		PaymentValue:  100,
	}
	if err := idx1.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	if err := idx1.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}

	// Reopen, as a restart would.
	if err := store.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer store2.Close()
	idx2, err := store2.LoadIndex()
	if err != nil {
		t.Fatalf("reloading index: %v", err)
	}

	// Verify positions match
	if positionCount(t, idx2) != 2 {
		t.Errorf("expected 2 positions, got %d", positionCount(t, idx2))
	}

	pos, ok := mustPosition(t, idx2, "tx_a", 0)
	if !ok {
		t.Fatal("position tx_a:0 not found after load")
	}
	if !pos.Spent || pos.SpentTxID != "tx_spend" || pos.SpentHeight != 101 {
		t.Error("spent position metadata mismatch after load")
	}

	pos, ok = mustPosition(t, idx2, "tx_b", 0)
	if !ok {
		t.Fatal("position tx_b:0 not found after load")
	}
	if pos.Spent {
		t.Error("position tx_b:0 should be unspent")
	}

	// Verify block hashes match
	if hash, ok := idx2.HashAtHeight(100); !ok || hash != "hash1" {
		t.Error("hash at height 100 mismatch")
	}
	if hash, ok := idx2.HashAtHeight(101); !ok || hash != "hash2" {
		t.Error("hash at height 101 mismatch")
	}

	// Verify tip state
	if idx2.TipHeight != 101 || idx2.TipHash != "hash2" {
		t.Errorf("tip mismatch: got (%d, %q), want (101, %q)", idx2.TipHeight, idx2.TipHash, "hash2")
	}

	// Verify orders match
	if orderCount(t, idx2) != 1 {
		t.Errorf("expected 1 order, got %d", orderCount(t, idx2))
	}
	if o, ok := mustGetOrder(t, idx2, "tx_b", 0); !ok {
		t.Fatal("order not found after load")
	} else if o.PaymentValue != 100 {
		t.Errorf("order payment value mismatch: got %d, want 100", o.PaymentValue)
	}
}

// TestOpenStoreOnAFreshPathYieldsAnEmptyIndex covers the first-run path:
// a database that does not exist yet is created, and loading it gives the
// same index NewIndex would. There is deliberately no separate "no state
// file" branch to get wrong.
func TestOpenStoreOnAFreshPathYieldsAnEmptyIndex(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/fresh.sqlite")
	if err != nil {
		t.Fatalf("OpenStore on a fresh path: %v", err)
	}
	defer store.Close()
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex on an empty database: %v", err)
	}
	if idx == nil {
		t.Fatal("LoadIndex should return a fresh Index, not nil")
	}
	if positionCount(t, idx) != 0 {
		t.Errorf("fresh Index should have empty Positions, got %d", positionCount(t, idx))
	}
	if idx.TipHeight != -1 {
		t.Errorf("fresh Index should have TipHeight=-1, got %d", idx.TipHeight)
	}
}

// TestByPubKeyMaintenance tests that the ByPubKey index is properly
// maintained when positions are created and removed.
func TestByPubKeyMaintenance(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	pk1Hex := hex.EncodeToString(pubkey1)
	pk2Hex := hex.EncodeToString(pubkey2)

	// Create positions
	block1 := makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "tx1",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx2",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapTransferVout(0, pubkey2, 2000, 2.0)},
		},
	})
	idx.ApplyBlock(block1)

	// Verify ByPubKey is populated
	if n := len(mustPositionsForPubKey(t, idx, pk1Hex, false)); n != 1 {
		t.Errorf("expected 1 position for pk1, got %d", n)
	}
	if n := len(mustPositionsForPubKey(t, idx, pk2Hex, false)); n != 1 {
		t.Errorf("expected 1 position for pk2, got %d", n)
	}

	// Undo and verify ByPubKey is cleaned up
	idx.UndoBlock(1)

	if n := len(mustPositionsForPubKey(t, idx, pk1Hex, false)); n != 0 {
		t.Errorf("expected 0 positions for pk1 after undo, got %d", n)
	}
	if n := len(mustPositionsForPubKey(t, idx, pk2Hex, false)); n != 0 {
		t.Errorf("expected 0 positions for pk2 after undo, got %d", n)
	}
}

// TestStatusSnapshot tests that StatusSnapshot returns correct counts.
func TestStatusSnapshot(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Empty snapshot
	status := mustStatus(t, idx)
	if status.TipHeight != -1 || status.PositionCount != 0 || status.OrderCount != 0 {
		t.Error("empty index should have TipHeight=-1 and counts of 0")
	}

	// After applying a block
	block := makeBlock("hash1", 42, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}})
	idx.ApplyBlock(block)

	status = mustStatus(t, idx)
	if status.TipHeight != 42 {
		t.Errorf("TipHeight mismatch: got %d, want 42", status.TipHeight)
	}
	if status.TipHash != "hash1" {
		t.Errorf("TipHash mismatch: got %q, want %q", status.TipHash, "hash1")
	}
	if status.PositionCount != 1 {
		t.Errorf("PositionCount mismatch: got %d, want 1", status.PositionCount)
	}
	if status.OrderCount != 0 {
		t.Errorf("OrderCount mismatch: got %d, want 0", status.OrderCount)
	}
}

// TestMultipleTxsInSingleBlock tests that a block with multiple txs that
// create and spend positions is handled correctly.
func TestMultipleTxsInSingleBlock(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)
	salt := []byte("0123456789abcdef")

	// First block: create two positions
	block1 := makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "tx1",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
		},
		{
			TxID: "tx2",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey2, 2000, salt, 2.0)},
		},
	})
	idx.ApplyBlock(block1)

	// Second block: spend tx1:0, create new position, and leave tx2:0 unspent
	block2 := makeBlock("hash2", 2, []RPCTx{
		{
			TxID: "tx3",
			Vin:  []RPCVin{spendVin("tx1", 0)},
			Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
		},
		{
			TxID: "tx4",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 3000, salt, 3.0)},
		},
	})
	idx.ApplyBlock(block2)

	// Verify state
	pos1, ok := mustPosition(t, idx, "tx1", 0)
	if !ok || !pos1.Spent {
		t.Error("tx1:0 should be marked spent")
	}

	pos2, ok := mustPosition(t, idx, "tx2", 0)
	if !ok || pos2.Spent {
		t.Error("tx2:0 should be unspent")
	}

	pos3, ok := mustPosition(t, idx, "tx3", 0)
	if !ok || pos3.Spent {
		t.Error("tx3:0 should exist and be unspent")
	}

	pos4, ok := mustPosition(t, idx, "tx4", 0)
	if !ok || pos4.Spent {
		t.Error("tx4:0 should exist and be unspent")
	}

	if positionCount(t, idx) != 4 {
		t.Errorf("expected 4 positions, got %d", positionCount(t, idx))
	}
}

// TestOpenStoreRejectsAFileThatIsNotADatabase checks that pointing
// -statefile at the wrong thing fails loudly at startup. Silently treating
// it as empty would resync from genesis and then write over whatever the
// file actually was.
func TestOpenStoreRejectsAFileThatIsNotADatabase(t *testing.T) {
	badPath := t.TempDir() + "/bad.sqlite"
	if err := os.WriteFile(badPath, []byte("this is not a database, it is a note"), 0644); err != nil {
		t.Fatalf("failed to write the decoy file: %v", err)
	}

	store, err := OpenStore(badPath)
	if err == nil {
		store.Close()
		t.Error("OpenStore accepted a file that is not a SQLite database")
	}
}

// TestMultipleBlocksAndVouts tests a scenario with multiple vouts per tx,
// some UAP and some not.
func TestMultipleBlocksAndVouts(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	block := makeBlock("hash1", 1, []RPCTx{{
		TxID: "tx_mixed",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{
			ordinaryVout(0, 1.0),
			uapMintVout(1, pubkey1, 1000, salt, 2.0),
			ordinaryVout(2, 3.0),
			uapTransferVout(3, pubkey1, 2000, 4.0),
		},
	}})
	idx.ApplyBlock(block)

	// Only vouts 1 and 3 should be indexed
	if positionCount(t, idx) != 2 {
		t.Errorf("expected 2 positions from mixed tx, got %d", positionCount(t, idx))
	}

	if _, ok := mustPosition(t, idx, "tx_mixed", 1); !ok {
		t.Error("vout 1 should be indexed")
	}
	if _, ok := mustPosition(t, idx, "tx_mixed", 3); !ok {
		t.Error("vout 3 should be indexed")
	}
	if _, ok := mustPosition(t, idx, "tx_mixed", 0); ok {
		t.Error("vout 0 (ordinary) should not be indexed")
	}
	if _, ok := mustPosition(t, idx, "tx_mixed", 2); ok {
		t.Error("vout 2 (ordinary) should not be indexed")
	}
}

// TestApplyBlockCreatesP2PKHUTXOs verifies that ApplyBlock creates UTXO entries
// for ordinary P2PKH outputs.
func TestApplyBlockCreatesP2PKHUTXOs(t *testing.T) {
	idx := NewIndex()
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}

	// Build a P2PKH script
	p2pkhScript := []byte{0x76, 0xa9, 0x14}
	p2pkhScript = append(p2pkhScript, hash160...)
	p2pkhScript = append(p2pkhScript, 0x88, 0xac)

	vout := RPCVout{
		Value: coins(2.5),
		N:     0,
	}
	vout.ScriptPubKey.Hex = hex.EncodeToString(p2pkhScript)

	block := makeBlock("hash1", 100, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{vout},
	}})

	idx.ApplyBlock(block)

	// Verify UTXO was created
	utxo, ok := mustUTXO(t, idx, "tx1", 0)
	if !ok {
		t.Fatal("UTXO not found after ApplyBlock")
	}
	if utxo.TxID != "tx1" {
		t.Errorf("TxID mismatch: got %q, want %q", utxo.TxID, "tx1")
	}
	if utxo.Vout != 0 {
		t.Errorf("Vout mismatch: got %d, want 0", utxo.Vout)
	}
	if utxo.Value != 250000000 {
		t.Errorf("Value mismatch: got %d, want 250000000 satoshis", utxo.Value)
	}
	if utxo.Height != 100 {
		t.Errorf("Height mismatch: got %d, want 100", utxo.Height)
	}
	if utxo.Hash160 != hex.EncodeToString(hash160) {
		t.Errorf("Hash160 mismatch")
	}
}

// TestApplyBlockIgnoresNonP2PKH verifies that non-P2PKH outputs are not indexed
// as UTXOs, but UAP outputs are still indexed as Positions.
func TestApplyBlockIgnoresNonP2PKH(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Create a P2SH script (should be ignored)
	p2shScript := []byte{0xa9, 0x14}
	p2shScript = append(p2shScript, make([]byte, 20)...)
	p2shScript = append(p2shScript, 0x87)

	p2shVout := RPCVout{Value: coins(1.0), N: 0}
	p2shVout.ScriptPubKey.Hex = hex.EncodeToString(p2shScript)

	block := makeBlock("hash1", 1, []RPCTx{{
		TxID: "tx1",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{
			p2shVout,
			uapMintVout(1, pubkey1, 1000, salt, 2.0),
		},
	}})

	idx.ApplyBlock(block)

	// P2SH output should not be indexed as UTXO
	if _, ok := mustUTXO(t, idx, "tx1", 0); ok {
		t.Error("P2SH output should not be indexed as UTXO")
	}

	// But UAP output should still be indexed as Position
	if _, ok := mustPosition(t, idx, "tx1", 1); !ok {
		t.Error("UAP output should still be indexed as Position")
	}
}

// TestApplyBlockSpendsUTXO verifies that when a UTXO is spent, it's removed
// from the index.
func TestApplyBlockSpendsUTXO(t *testing.T) {
	idx := NewIndex()
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}

	// Build a P2PKH script
	p2pkhScript := []byte{0x76, 0xa9, 0x14}
	p2pkhScript = append(p2pkhScript, hash160...)
	p2pkhScript = append(p2pkhScript, 0x88, 0xac)

	vout := RPCVout{Value: coins(1.0), N: 0}
	vout.ScriptPubKey.Hex = hex.EncodeToString(p2pkhScript)

	// Block 1: create a UTXO
	block1 := makeBlock("hash1", 10, []RPCTx{{
		TxID: "create_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{vout},
	}})
	idx.ApplyBlock(block1)

	// Verify UTXO exists
	if _, ok := mustUTXO(t, idx, "create_tx", 0); !ok {
		t.Fatal("UTXO should exist after block 1")
	}

	// Block 2: spend the UTXO
	block2 := makeBlock("hash2", 11, []RPCTx{{
		TxID: "spend_tx",
		Vin:  []RPCVin{spendVin("create_tx", 0)},
		Vout: []RPCVout{},
	}})
	idx.ApplyBlock(block2)

	// Verify UTXO is gone
	if _, ok := mustUTXO(t, idx, "create_tx", 0); ok {
		t.Error("UTXO should be removed when spent")
	}
}

// TestUndoBlockRestoresSpentUTXO verifies that UndoBlock restores a spent UTXO
// with its original value and height (not zeroes).
func TestUndoBlockRestoresSpentUTXO(t *testing.T) {
	idx := NewIndex()
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}

	// Build a P2PKH script
	p2pkhScript := []byte{0x76, 0xa9, 0x14}
	p2pkhScript = append(p2pkhScript, hash160...)
	p2pkhScript = append(p2pkhScript, 0x88, 0xac)

	vout := RPCVout{Value: coins(3.14), N: 0}
	vout.ScriptPubKey.Hex = hex.EncodeToString(p2pkhScript)

	// Block 1: create a UTXO
	block1 := makeBlock("hash1", 50, []RPCTx{{
		TxID: "create_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{vout},
	}})
	idx.ApplyBlock(block1)

	utxo1, _ := mustUTXO(t, idx, "create_tx", 0)
	originalValue := utxo1.Value
	originalHeight := utxo1.Height

	// Block 2: spend the UTXO
	block2 := makeBlock("hash2", 51, []RPCTx{{
		TxID: "spend_tx",
		Vin:  []RPCVin{spendVin("create_tx", 0)},
		Vout: []RPCVout{},
	}})
	idx.ApplyBlock(block2)

	// Verify UTXO is gone
	if _, ok := mustUTXO(t, idx, "create_tx", 0); ok {
		t.Fatal("UTXO should be removed when spent")
	}

	// Undo block 2
	idx.UndoBlock(51)

	// Verify UTXO is restored with original value and height
	utxo2, ok := mustUTXO(t, idx, "create_tx", 0)
	if !ok {
		t.Fatal("UTXO should be restored after UndoBlock")
	}
	if utxo2.Value != originalValue {
		t.Errorf("restored UTXO value mismatch: got %d, want %d", utxo2.Value, originalValue)
	}
	if utxo2.Height != originalHeight {
		t.Errorf("restored UTXO height mismatch: got %d, want %d", utxo2.Height, originalHeight)
	}
}

// TestApplyAndUndoSymmetry verifies that ApplyBlock followed by UndoBlock
// results in byte-identical index state.
func TestApplyAndUndoSymmetry(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Build two P2PKH scripts for UTXOs
	makeP2PKH := func(i byte) []byte {
		hash160 := make([]byte, 20)
		hash160[0] = i
		script := []byte{0x76, 0xa9, 0x14}
		script = append(script, hash160...)
		script = append(script, 0x88, 0xac)
		return script
	}

	p2pkhVout1 := RPCVout{Value: coins(1.5), N: 0}
	p2pkhVout1.ScriptPubKey.Hex = hex.EncodeToString(makeP2PKH(1))

	p2pkhVout2 := RPCVout{Value: coins(2.5), N: 1}
	p2pkhVout2.ScriptPubKey.Hex = hex.EncodeToString(makeP2PKH(2))

	// Block 1: create both a UAP position and two P2PKH UTXOs
	block1 := makeBlock("hash1", 100, []RPCTx{{
		TxID: "mixed_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{
			p2pkhVout1,
			uapMintVout(1, pubkey1, 1000, salt, 5.5),
			p2pkhVout2,
		},
	}})
	idx.ApplyBlock(block1)

	// Save the state after block 1
	state1 := snapshotIndex(t, idx)

	// Block 2: spend one UTXO
	block2 := makeBlock("hash2", 101, []RPCTx{{
		TxID: "spend_tx",
		Vin:  []RPCVin{spendVin("mixed_tx", 0)},
		Vout: []RPCVout{},
	}})
	idx.ApplyBlock(block2)

	// Undo block 2
	idx.UndoBlock(101)

	// Verify state matches after block 1
	state1After := snapshotIndex(t, idx)
	if state1 != state1After {
		t.Error("index state after undo does not match state after block 1")
	}

	// Undo block 1
	idx.UndoBlock(100)

	// Verify index is back to fresh
	state3 := snapshotIndex(t, idx)
	freshIdx := NewIndex()
	freshState := snapshotIndex(t, freshIdx)
	if state3 != freshState {
		t.Logf("After undo state: %s", state3)
		t.Logf("Fresh state:      %s", freshState)
		t.Error("index state after full undo does not match fresh index")
	}
}

// --- Lineage identity -------------------------------------------------------
//
// `multiplier` is not a token identity. Anyone may mint a fresh lineage with
// any multiplier, so two entirely unrelated tokens can share one.
//
// Consensus already guarantees they can never be merged on-chain:
// CheckUapOutputConservation runs once per UAP input, and each run
// independently requires the transaction's *total* same-multiplier covenant
// output value to fit within that one input's own value -- two inputs from
// different lineages cannot both satisfy that against a shared output total.
// That property is covered on the consensus side by
// transfer_rejects_merging_independent_lineages in src/test/uap_mint_tests.cpp.
//
// The tests below pin the indexer's half of the picture.

// TestTwoLineagesSameMultiplierStayDistinct verifies that two independent mints
// sharing a multiplier -- and even a recipient pubkey, the worst case for
// conflation -- are tracked as separate positions, and that spending one has no
// effect on the other.
func TestTwoLineagesSameMultiplierStayDistinct(t *testing.T) {
	idx := NewIndex()
	const mult = 1000
	holder := fakePubKey(0x02)

	// Two unrelated mints in the same block: same multiplier, same recipient,
	// different salts and different transactions.
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: "mint_lineage_a",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, holder, mult, []byte("salt_for_lineage_a"), 1500.0)},
		},
		{
			TxID: "mint_lineage_b",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, holder, mult, []byte("salt_for_lineage_b"), 2500.0)},
		},
	}))

	a, okA := mustPosition(t, idx, "mint_lineage_a", 0)
	b, okB := mustPosition(t, idx, "mint_lineage_b", 0)
	if !okA || !okB {
		t.Fatal("both independent mints should be indexed")
	}
	if a.TxID == b.TxID {
		t.Fatal("the two lineages must not collapse onto one outpoint")
	}
	if a.Multiplier != mult || b.Multiplier != mult {
		t.Fatalf("both should carry multiplier %d, got %d and %d", mult, a.Multiplier, b.Multiplier)
	}
	if a.Value == b.Value {
		t.Error("test setup is weak: the two lineages should hold different values")
	}

	// Spend lineage A only, forwarding it into a transfer covenant.
	next := fakePubKey(0x03)
	idx.ApplyBlock(makeBlock("hashB", 101, []RPCTx{{
		TxID: "spend_lineage_a",
		Vin:  []RPCVin{spendVin("mint_lineage_a", 0)},
		Vout: []RPCVout{uapTransferVout(0, next, mult, 1499.0)},
	}}))

	a, _ = mustPosition(t, idx, "mint_lineage_a", 0)
	b, _ = mustPosition(t, idx, "mint_lineage_b", 0)
	if !a.Spent {
		t.Error("lineage A should be marked spent")
	}
	if b.Spent {
		t.Error("lineage B must NOT be affected by spending lineage A")
	}
	if b.SpentTxID != "" || b.SpentHeight != 0 {
		t.Errorf("lineage B should carry no spend metadata, got %q/%d", b.SpentTxID, b.SpentHeight)
	}

	// A's continuation is its own position, not a mutation of either mint.
	cont, ok := mustPosition(t, idx, "spend_lineage_a", 0)
	if !ok {
		t.Fatal("lineage A's transfer output should be indexed")
	}
	if cont.IsMint {
		t.Error("a transfer output should not be recorded as a mint")
	}

	// Undoing the spend must restore A without disturbing B.
	idx.UndoBlock(101)
	a, _ = mustPosition(t, idx, "mint_lineage_a", 0)
	b, _ = mustPosition(t, idx, "mint_lineage_b", 0)
	if a.Spent {
		t.Error("lineage A should be unspent again after reorg")
	}
	if b.Spent {
		t.Error("lineage B should still be untouched after reorg")
	}
	if _, ok := mustPosition(t, idx, "spend_lineage_a", 0); ok {
		t.Error("lineage A's transfer output should be gone after reorg")
	}
}

// TestMintOriginIsSelfAndTransfersInheritOrigin verifies lineage tracking.
// A mint's origin is itself (its outpoint); transfers inherit the origin of the position they spend.
func TestMintOriginIsSelfAndTransfersInheritOrigin(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Block 1: create a mint at origin_one:0
	idx.ApplyBlock(makeBlock("hash1", 100, []RPCTx{{
		TxID: "origin_one",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}}))

	// A mint's origin is its own outpoint
	pos1, ok := mustPosition(t, idx, "origin_one", 0)
	if !ok {
		t.Fatal("mint position not found")
	}
	expectedOrigin := "origin_one:0"
	if pos1.Origin != expectedOrigin {
		t.Errorf("mint origin mismatch: got %q, want %q", pos1.Origin, expectedOrigin)
	}

	// Block 2: transfer the mint to a new output (transfer:0)
	idx.ApplyBlock(makeBlock("hash2", 101, []RPCTx{{
		TxID: "transfer",
		Vin:  []RPCVin{spendVin("origin_one", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}}))

	// The transfer inherits the origin from the mint it spent
	pos2, ok := mustPosition(t, idx, "transfer", 0)
	if !ok {
		t.Fatal("transfer position not found")
	}
	if pos2.Origin != expectedOrigin {
		t.Errorf("transfer origin mismatch: got %q, want %q", pos2.Origin, expectedOrigin)
	}

	// Block 3: transfer again (transfer2:0 spends transfer:0)
	idx.ApplyBlock(makeBlock("hash3", 102, []RPCTx{{
		TxID: "transfer2",
		Vin:  []RPCVin{spendVin("transfer", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.25)},
	}}))

	// The second transfer also inherits the original mint's origin
	pos3, ok := mustPosition(t, idx, "transfer2", 0)
	if !ok {
		t.Fatal("second transfer position not found")
	}
	if pos3.Origin != expectedOrigin {
		t.Errorf("second transfer origin mismatch: got %q, want %q", pos3.Origin, expectedOrigin)
	}
}

// TestTwoIndependentMints produce distinct lineages.
func TestTwoIndependentMintsProduceTwoLineages(t *testing.T) {
	idx := NewIndex()
	const mult = 1000
	holder := fakePubKey(0x02)

	// Block 1: create two independent mints at the same multiplier
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: "origin_one",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, holder, mult, []byte("salt_for_origin_one"), 1500.0)},
		},
		{
			TxID: "origin_two",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, holder, mult, []byte("salt_for_origin_two"), 2500.0)},
		},
	}))

	pos1, ok := mustPosition(t, idx, "origin_one", 0)
	if !ok {
		t.Fatal("first mint not found")
	}
	pos2, ok := mustPosition(t, idx, "origin_two", 0)
	if !ok {
		t.Fatal("second mint not found")
	}

	// Each mint is its own origin
	if pos1.Origin != "origin_one:0" {
		t.Errorf("origin_one origin mismatch: got %q, want %q", pos1.Origin, "origin_one:0")
	}
	if pos2.Origin != "origin_two:0" {
		t.Errorf("origin_two origin mismatch: got %q, want %q", pos2.Origin, "origin_two:0")
	}

	// They must be two distinct lineages, not merged
	if pos1.Origin == pos2.Origin {
		t.Error("two independent mints must have different origins")
	}
}

// TestOrphanTransferHasEmptyOrigin verifies that a transfer whose parent
// was never indexed (or is below the start height) gets an empty origin
// and is excluded from the tokens index.
func TestOrphanTransferHasEmptyOrigin(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	// Block 1: Create a transfer that spends a non-existent position
	// (simulating indexing starting mid-chain, or missing history)
	idx.ApplyBlock(makeBlock("hash1", 100, []RPCTx{{
		TxID: "orphan_transfer",
		Vin:  []RPCVin{spendVin("never_indexed", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 1.0)},
	}}))

	// The orphan position should exist but have empty origin
	pos, ok := mustPosition(t, idx, "orphan_transfer", 0)
	if !ok {
		t.Fatal("orphan transfer position should be created even with unknown parent")
	}
	if pos.Origin != "" {
		t.Errorf("orphan transfer origin should be empty, got %q", pos.Origin)
	}
}

// TestUndoBlockRestoresLineageCorrectly verifies that undo correctly rolls back
// lineage assignments. Apply three blocks, undo to different states, check
// lineage is restored correctly.
func TestUndoBlockRestoresLineageCorrectly(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	// Block 1: mint
	idx.ApplyBlock(makeBlock("hash1", 100, []RPCTx{{
		TxID: "mint_tx",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, salt, 1.0)},
	}}))

	// Block 2: transfer
	idx.ApplyBlock(makeBlock("hash2", 101, []RPCTx{{
		TxID: "transfer_tx",
		Vin:  []RPCVin{spendVin("mint_tx", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}}))

	// Record state after block 2 (before block 3 was applied)
	mintPosAfter2, _ := mustPosition(t, idx, "mint_tx", 0)
	transferPosAfter2, _ := mustPosition(t, idx, "transfer_tx", 0)

	// Block 3: another transfer
	idx.ApplyBlock(makeBlock("hash3", 102, []RPCTx{{
		TxID: "transfer2_tx",
		Vin:  []RPCVin{spendVin("transfer_tx", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.25)},
	}}))

	// Verify lineage before undo
	pos3, _ := mustPosition(t, idx, "transfer2_tx", 0)
	if pos3.Origin != "mint_tx:0" {
		t.Fatalf("before undo: transfer2 origin should be mint_tx:0, got %q", pos3.Origin)
	}

	// Undo block 3
	idx.UndoBlock(102)

	// After undo, transfer2_tx should be gone
	if _, ok := mustPosition(t, idx, "transfer2_tx", 0); ok {
		t.Error("after undo block 3: transfer2_tx should be removed")
	}

	// transfer_tx should still exist with correct origin
	pos2, ok := mustPosition(t, idx, "transfer_tx", 0)
	if !ok {
		t.Fatal("after undo block 3: transfer_tx should still exist")
	}
	if pos2.Origin != "mint_tx:0" {
		t.Errorf("after undo block 3: transfer origin should be mint_tx:0, got %q", pos2.Origin)
	}

	// Undo block 2
	idx.UndoBlock(101)

	// After undo, transfer_tx should be gone
	if _, ok := mustPosition(t, idx, "transfer_tx", 0); ok {
		t.Error("after undo block 2: transfer_tx should be removed")
	}

	// mint_tx should still exist with its own origin
	pos1, ok := mustPosition(t, idx, "mint_tx", 0)
	if !ok {
		t.Fatal("after undo block 2: mint_tx should still exist")
	}
	if pos1.Origin != "mint_tx:0" {
		t.Errorf("after undo block 2: mint origin should be mint_tx:0, got %q", pos1.Origin)
	}

	// Re-apply block 2 and verify the positions match what they were before
	idx.ApplyBlock(makeBlock("hash2", 101, []RPCTx{{
		TxID: "transfer_tx",
		Vin:  []RPCVin{spendVin("mint_tx", 0)},
		Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, 0.5)},
	}}))

	// Verify the positions match
	mintPosRe, _ := mustPosition(t, idx, "mint_tx", 0)
	transferPosRe, _ := mustPosition(t, idx, "transfer_tx", 0)

	// Compare key fields (don't compare entire struct as it might have ordering differences)
	if mintPosRe.Spent != mintPosAfter2.Spent || mintPosRe.Origin != mintPosAfter2.Origin {
		t.Errorf("mint_tx position mismatch after re-apply: before Spent=%v, after Spent=%v; before Origin=%q, after Origin=%q",
			mintPosAfter2.Spent, mintPosRe.Spent, mintPosAfter2.Origin, mintPosRe.Origin)
	}
	if transferPosRe.Origin != transferPosAfter2.Origin || transferPosRe.Spent != transferPosAfter2.Spent {
		t.Errorf("transfer_tx position mismatch after re-apply: before Origin=%q, after Origin=%q; before Spent=%v, after Spent=%v",
			transferPosAfter2.Origin, transferPosRe.Origin, transferPosAfter2.Spent, transferPosRe.Spent)
	}
}

// TestUndoBlockRollsBackTip verifies that UndoBlock reverses the tip, not
// just the block's contents.
//
// ApplyBlock sets both TipHeight and TipHash, so "reverses everything
// ApplyBlock did" has to include them. It previously did not: the caller
// in syncOnce decremented idx.TipHeight itself afterwards and never
// touched TipHash at all, which left /status reporting a height and a
// hash belonging to different blocks -- and persisted that mismatched
// pair to the state file, so a restart resumed from it.
func TestUndoBlockRollsBackTip(t *testing.T) {
	idx := NewIndex()
	pubkey := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")

	idx.ApplyBlock(makeBlock("hashA", 10, []RPCTx{{
		TxID: "txA",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey, 5, salt, 1.0)},
	}}))
	idx.ApplyBlock(makeBlock("hashB", 11, []RPCTx{{
		TxID: "txB",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, pubkey, 6, salt, 1.0)},
	}}))

	idx.UndoBlock(11)

	s := mustStatus(t, idx)
	if s.TipHeight != 10 {
		t.Errorf("TipHeight after undo: got %d, want 10", s.TipHeight)
	}
	if s.TipHash != "hashA" {
		t.Errorf("TipHash after undo: got %q, want %q (the hash of the height it fell back to)", s.TipHash, "hashA")
	}
}

// TestUndoBlockToEmptyResetsTip verifies the boundary: undoing the only
// block leaves the index in its pristine "nothing indexed" state, so the
// caller resumes from startHeight rather than from height 0 of a chain it
// no longer has.
func TestUndoBlockToEmptyResetsTip(t *testing.T) {
	idx := NewIndex()
	idx.ApplyBlock(makeBlock("hashA", 0, []RPCTx{{
		TxID: "txA",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{ordinaryVout(0, 1.0)},
	}}))

	idx.UndoBlock(0)

	s := mustStatus(t, idx)
	if s.TipHeight != -1 {
		t.Errorf("TipHeight after undoing the only block: got %d, want -1", s.TipHeight)
	}
	if s.TipHash != "" {
		t.Errorf("TipHash after undoing the only block: got %q, want empty", s.TipHash)
	}
}

// TestUndoBlockWithGapDoesNotInventATip guards the case where the index
// has no record of the height below the one being undone (a state file
// that starts mid-chain, or a startHeight above 0). The tip must fall back
// to a height it can name, not to a hash it never saw.
func TestUndoBlockWithGapDoesNotInventATip(t *testing.T) {
	idx := NewIndex()
	idx.ApplyBlock(makeBlock("hash500", 500, []RPCTx{{
		TxID: "tx500",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{ordinaryVout(0, 1.0)},
	}}))

	idx.UndoBlock(500)

	s := mustStatus(t, idx)
	if s.TipHash != "" {
		t.Errorf("TipHash after undoing to an unknown height: got %q, want empty", s.TipHash)
	}
	if s.TipHeight != 499 {
		t.Errorf("TipHeight after undo: got %d, want 499", s.TipHeight)
	}
}

// TestUndoDoesNotResurrectAUTXOTheSameBlockCreated covers the overlap
// between an undo log's two UTXO lists.
//
// One transaction pays an address; a later transaction in the same block
// spends that output. The key lands in both UTXOsCreated and UTXOsSpent, so
// undo deletes it and then -- if the restore loop is unconditional --
// immediately puts it back. The result is a UTXO belonging to a transaction
// that is no longer in the indexed chain, handed out as spendable to anyone
// querying that address, and it survives every subsequent block because
// nothing will ever spend it again.
//
// The position path is immune to the same overlap by accident: its restore
// loop only touches keys still in the map, and the delete loop has just
// removed them. The UTXO restore had no such guard.
func TestUndoDoesNotResurrectAUTXOTheSameBlockCreated(t *testing.T) {
	idx := NewIndex()

	// Block 0 establishes a UTXO that outlives the reorg, so the test can
	// tell "restored nothing" apart from "restored correctly".
	idx.ApplyBlock(makeBlock("hash0", 0, []RPCTx{{
		TxID: "old",
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{p2pkhVoutFor(0, 0x11, 3.0)},
	}}))

	// Block 1 creates a UTXO and spends it, plus spends the older one.
	idx.ApplyBlock(makeBlock("hash1", 1, []RPCTx{
		{
			TxID: "payer",
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{p2pkhVoutFor(0, 0x22, 1.0)},
		},
		{
			TxID: "spender",
			Vin:  []RPCVin{spendVin("payer", 0), spendVin("old", 0)},
			Vout: []RPCVout{p2pkhVoutFor(0, 0x33, 3.5)},
		},
	}))
	if _, ok := mustUTXO(t, idx, "payer", 0); ok {
		t.Fatal("setup: payer:0 should already be spent within block 1")
	}

	idx.UndoBlock(1)

	if _, ok := mustUTXO(t, idx, "payer", 0); ok {
		t.Error("undo restored a UTXO that the undone block itself created; " +
			"it now names a transaction the index no longer has")
	}
	if got := mustUTXOsForHash160(t, idx, hash160Hex(0x22)); len(got) != 0 {
		t.Errorf("hash160 index still lists %d UTXO(s) for the resurrected output", len(got))
	}
	// The genuinely older UTXO must come back.
	if _, ok := mustUTXO(t, idx, "old", 0); !ok {
		t.Error("undo failed to restore a UTXO created by an earlier block")
	}
	if got := mustUTXOsForHash160(t, idx, hash160Hex(0x11)); len(got) != 1 {
		t.Errorf("hash160 index lists %d UTXO(s) for the restored output, want 1", len(got))
	}
}
