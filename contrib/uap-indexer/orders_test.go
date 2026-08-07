package main

import (
	"encoding/hex"
	"testing"
)

func fakeDER() []byte {
	// Not a real signature, just DER-shaped enough to pass the structural
	// check: 0x30 <len> 0x02 <len> <r...> 0x02 <len> <s...>
	return []byte{
		0x30, 0x06,
		0x02, 0x01, 0x01,
		0x02, 0x01, 0x02,
	}
}

func TestValidateScriptSigAccepts(t *testing.T) {
	der := fakeDER()
	sigAndHashType := append(append([]byte{}, der...), sighashOrderType)
	push := append([]byte{byte(len(sigAndHashType))}, sigAndHashType...)
	if err := validateScriptSig(hex.EncodeToString(push)); err != nil {
		t.Fatalf("expected valid script_sig, got error: %v", err)
	}
}

func TestValidateScriptSigRejectsWrongHashType(t *testing.T) {
	der := fakeDER()
	sigAndHashType := append(append([]byte{}, der...), 0x01) // SIGHASH_ALL, not SINGLE|ANYONECANPAY
	push := append([]byte{byte(len(sigAndHashType))}, sigAndHashType...)
	if err := validateScriptSig(hex.EncodeToString(push)); err == nil {
		t.Fatal("expected rejection for wrong hashtype")
	}
}

func TestValidateScriptSigRejectsNonDER(t *testing.T) {
	sigAndHashType := append([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, sighashOrderType)
	push := append([]byte{byte(len(sigAndHashType))}, sigAndHashType...)
	if err := validateScriptSig(hex.EncodeToString(push)); err == nil {
		t.Fatal("expected rejection for non-DER-shaped signature")
	}
}

func TestValidateScriptSigRejectsBadPushLength(t *testing.T) {
	der := fakeDER()
	sigAndHashType := append(append([]byte{}, der...), sighashOrderType)
	// Claim a push length longer than the actual data.
	push := append([]byte{byte(len(sigAndHashType) + 5)}, sigAndHashType...)
	if err := validateScriptSig(hex.EncodeToString(push)); err == nil {
		t.Fatal("expected rejection for mismatched push length")
	}
}

func TestValidateScriptSigRejectsInvalidHex(t *testing.T) {
	if err := validateScriptSig("not-hex"); err == nil {
		t.Fatal("expected rejection for invalid hex")
	}
}

func makeOrderInputs(t *testing.T) (der []byte) {
	t.Helper()
	return fakeDER()
}

func signedPush(t *testing.T) string {
	t.Helper()
	der := makeOrderInputs(t)
	sigAndHashType := append(append([]byte{}, der...), sighashOrderType)
	push := append([]byte{byte(len(sigAndHashType))}, sigAndHashType...)
	return hex.EncodeToString(push)
}

func TestOrderLifecycle(t *testing.T) {
	idx := NewIndex()

	// Seed a position directly (bypassing ApplyBlock -- we're testing the
	// order logic, not block indexing).
	pos := &Position{TxID: "abc123", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 5000000000, IsMint: false, Height: 10}
	seedPosition(t, idx, pos)

	scriptSig := signedPush(t)
	order := &Order{
		TxID:          pos.TxID,
		Vout:          pos.Vout,
		Multiplier:    1000,
		ScriptSig:     scriptSig,
		PaymentScript: "76a914" + "00000000000000000000000000000000000000" + "88ac",
		PaymentValue:  700000000,
	}

	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder failed: %v", err)
	}
	if order.PubKey != pos.PubKey {
		t.Errorf("expected order to inherit pubkey %s from position, got %s", pos.PubKey, order.PubKey)
	}

	got, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout)
	if !ok {
		t.Fatal("expected to find the published order")
	}
	if got.PaymentValue != order.PaymentValue {
		t.Errorf("payment value mismatch: got %d, want %d", got.PaymentValue, order.PaymentValue)
	}

	orders := mustListOrders(t, idx, nil)
	if len(orders) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(orders))
	}

	wrongMultiplier := int64(500)
	if orders := mustListOrders(t, idx, &wrongMultiplier); len(orders) != 0 {
		t.Errorf("expected 0 orders for a non-matching multiplier filter, got %d", len(orders))
	}

	// Publishing against an unknown position must fail.
	badOrder := &Order{TxID: "doesnotexist", Vout: 0, Multiplier: 1000, ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(badOrder); err == nil {
		t.Error("expected PublishOrder to reject an order on an unknown position")
	}

	// Publishing with a mismatched multiplier must fail.
	mismatchOrder := &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 999, ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(mismatchOrder); err == nil {
		t.Error("expected PublishOrder to reject a multiplier mismatch")
	}

	// Cancelling with the wrong scriptSig must fail.
	if err := idx.CancelOrder(pos.TxID, pos.Vout, "00"); err == nil {
		t.Error("expected CancelOrder to reject a non-matching script_sig")
	}
	// Cancelling with the right one succeeds.
	if err := idx.CancelOrder(pos.TxID, pos.Vout, scriptSig); err != nil {
		t.Errorf("expected CancelOrder to succeed, got: %v", err)
	}
	if _, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout); ok {
		t.Error("expected order to be gone after cancellation")
	}
}

func TestOrderRejectedForSpentPosition(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: "spent1", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1, Spent: true}
	seedPosition(t, idx, pos)

	order := &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(order); err == nil {
		t.Error("expected PublishOrder to reject an order on an already-spent position")
	}
}

func TestOrderPrunedWhenPositionSpent(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: "prune1", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1}
	seedPosition(t, idx, pos)
	seedOrder(t, idx, &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000})

	block := &RPCBlock{
		Hash: "blockhash", Height: 1,
		Tx: []RPCTx{{
			TxID: "filltx",
			Vin:  []RPCVin{{TxID: pos.TxID, Vout: pos.Vout}},
			Vout: []RPCVout{},
		}},
	}
	idx.ApplyBlock(block)

	if _, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout); ok {
		t.Error("expected order to be pruned once its position was spent")
	}
	if orderCount(t, idx) != 0 {
		t.Errorf("expected no stored orders, has %d", orderCount(t, idx))
	}
}

// TestOrderForAPositionLostToAReorgIsNotServed covers the gap that order
// pruning inside ApplyBlock does not: pruning fires when a position is
// *spent*, but a reorg can make a position disappear without it ever being
// spent, and nothing deletes the order row in that case.
//
// The row surviving is tolerable -- orders are ephemeral, non-consensus
// data and the maker can republish -- but serving it is not. A taker who
// fetched it would build a fill against an outpoint that no longer exists
// on the indexed chain, and only find out when their node rejected the
// broadcast. The join in the order query is what stops that, and this is
// the only path that exercises it.
func TestOrderForAPositionLostToAReorgIsNotServed(t *testing.T) {
	idx := NewIndex()
	salt := []byte("0123456789abcdef")

	idx.ApplyBlock(makeBlock("hash1", 1, []RPCTx{{
		TxID: "mint", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, salt, 1.0)},
	}}))
	if err := idx.PublishOrder(&Order{
		TxID: "mint", Vout: 0, Multiplier: 1000,
		ScriptSig: makeOrderScriptSig(), PaymentScript: "5678", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// The block that created the position is orphaned.
	idx.UndoBlock(1)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	if _, ok := mustPosition(t, idx, "mint", 0); ok {
		t.Fatal("precondition: the position should be gone after the undo")
	}

	if _, ok := mustGetOrder(t, idx, "mint", 0); ok {
		t.Error("an order was served for a position the chain no longer contains")
	}
	if got := mustListOrders(t, idx, nil); len(got) != 0 {
		t.Errorf("ListOrders returned %d orders for vanished positions, want 0", len(got))
	}
}

// --- cancellation edge cases ---

// A failed cancel must leave no trace. Writing the tombstone before
// checking the scriptSig, or writing it on the failure path, would hand
// anyone who can guess an outpoint a way to permanently block that order
// from ever being mirrored in again -- a denial of service that needs no
// signature and survives restarts.
func TestCancelWithWrongScriptSigChangesNothing(t *testing.T) {
	idx := NewIndex()
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 500000000,
	})
	real := signedPush(t)
	if err := idx.PublishOrder(&Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: real, PaymentScript: "00", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	if err := idx.CancelOrder("known", 0, "48"+real[2:]); err == nil {
		t.Error("expected a cancel with the wrong script_sig to be rejected")
	}

	if _, ok := mustGetOrder(t, idx, "known", 0); !ok {
		t.Error("a rejected cancel removed the order anyway")
	}
	n, err := idx.store.CountTombstones()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a rejected cancel wrote %d tombstone(s); it must leave no trace", n)
	}
}

func TestCancelUnknownOrderIsRejected(t *testing.T) {
	idx := NewIndex()
	if err := idx.CancelOrder("never-existed", 0, signedPush(t)); err == nil {
		t.Error("expected cancelling an order that was never published to fail")
	}
	n, err := idx.store.CountTombstones()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("cancelling a nonexistent order wrote %d tombstone(s), want 0", n)
	}
}

// TestPublishReplacesAnExistingOrderForTheSamePosition pins current
// behaviour, which is last-write-wins.
//
// This is a known weakness, not a design goal. The relay does no
// cryptographic verification -- it cannot, without an EC implementation
// and a reconstruction of the maker's sighash -- so it cannot tell the
// maker's own replacement from a stranger overwriting a listing with a
// structurally-valid but meaningless signature. Both orderings lose:
// first-write-wins would instead let anyone squat an outpoint before its
// owner ever listed it.
//
// What it costs is noise in the listing, not funds: a taker who builds a
// fill from a bogus order simply has it rejected by their own node, which
// is the authoritative check throughout this design. Recorded here so the
// behaviour is a decision rather than an accident.
func TestPublishReplacesAnExistingOrderForTheSamePosition(t *testing.T) {
	idx := NewIndex()
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 500000000,
	})
	first := signedPush(t)
	if err := idx.PublishOrder(&Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: first, PaymentScript: "00", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	der := []byte{0x30, 0x06, 0x02, 0x01, 0x09, 0x02, 0x01, 0x09}
	sig := append(der, sighashOrderType)
	second := hex.EncodeToString(append([]byte{byte(len(sig))}, sig...))
	if err := idx.PublishOrder(&Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: second, PaymentScript: "00", PaymentValue: 999,
	}); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	o, ok := mustGetOrder(t, idx, "known", 0)
	if !ok {
		t.Fatal("no order served after the replacement")
	}
	if o.ScriptSig != second || o.PaymentValue != 999 {
		t.Errorf("expected the later publish to win, got %+v", o)
	}
	if n := orderCount(t, idx); n != 1 {
		t.Errorf("%d order rows for one outpoint, want 1", n)
	}
}

// --- orders and reorgs ---

// TestOrphanedFillRestoresTheOrder covers a taker's fill being reorged out.
//
// The order is pruned when its position is spent, which is right while the
// fill stands. But if the block carrying the fill is orphaned, the fill did
// not happen: the position is unspent again and the maker is still offering
// it at the same price, signed with the same fragment. Leaving the listing
// deleted silently retires an offer nobody withdrew.
//
// This is not an exotic case on a 6-second chain. Short reorgs are ordinary
// there in a way they are not on a ten-minute one, so "every orphaned fill
// quietly delists the maker" is a recurring loss, not a rare one.
func TestOrphanedFillRestoresTheOrder(t *testing.T) {
	idx := NewIndex()
	salt := []byte("0123456789abcdef")
	sig := signedPush(t)

	idx.ApplyBlock(makeBlock("h1", 1, []RPCTx{{
		TxID: "mint", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, salt, 1.0)},
	}}))
	order := &Order{TxID: "mint", Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "76a914", PaymentValue: 700000000}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// A taker fills it.
	idx.ApplyBlock(makeBlock("h2", 2, []RPCTx{{
		TxID: "fill", Vin: []RPCVin{spendVin("mint", 0)},
		Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x03), 1000, 1.0)},
	}}))
	if _, ok := mustGetOrder(t, idx, "mint", 0); ok {
		t.Fatal("precondition: a filled order should not still be listed")
	}

	// ...and that block is orphaned.
	idx.UndoBlock(2)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	pos, ok := mustPosition(t, idx, "mint", 0)
	if !ok || pos.Spent {
		t.Fatalf("precondition: the position should be back and unspent, got ok=%v spent=%v", ok, pos.Spent)
	}

	got, ok := mustGetOrder(t, idx, "mint", 0)
	if !ok {
		t.Fatal("the maker's order was not restored when the fill was orphaned; " +
			"an offer nobody withdrew has been silently retired")
	}
	if got.ScriptSig != sig || got.PaymentValue != 700000000 || got.PaymentScript != "76a914" {
		t.Errorf("the restored order does not match the original: got %+v", got)
	}
	// Restoring must not resurrect it twice or leave a duplicate behind.
	if n := orderCount(t, idx); n != 1 {
		t.Errorf("%d order rows after the restore, want 1", n)
	}
}

// Undoing a block that both created and spent a position must leave other
// makers' orders alone and add no junk rows of its own.
//
// Note what this does *not* cover: undoBlock also skips restoring an order
// whose position the same block created. That guard is unreachable and
// knowingly untested -- publishing an order requires the position to
// already exist, and creation and spend inside one block happen
// atomically under the index write lock that PublishOrder also needs, so
// nothing can slip an order in between. It is kept because the invariant
// making it unreachable lives in the locking rather than anywhere near
// this code, and the row it would leave behind would be dead forever.
func TestUndoOfASameBlockCreateAndSpendLeavesOtherOrdersAlone(t *testing.T) {
	idx := NewIndex()
	salt := []byte("0123456789abcdef")
	sig := signedPush(t)

	// One block both creates the position and spends it.
	idx.ApplyBlock(makeBlock("h1", 1, []RPCTx{{
		TxID: "mint", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, salt, 1.0)},
	}}))
	if err := idx.PublishOrder(&Order{TxID: "mint", Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "00", PaymentValue: 100}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	idx.ApplyBlock(makeBlock("h2", 2, []RPCTx{
		{
			TxID: "mint2", Vin: []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, fakePubKey(0x04), 500, salt, 1.0)},
		},
		{
			TxID: "spend2", Vin: []RPCVin{spendVin("mint2", 0)},
			Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x05), 500, 1.0)},
		},
	}))

	idx.UndoBlock(2)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	if _, ok := mustPosition(t, idx, "mint2", 0); ok {
		t.Fatal("precondition: mint2 should have been removed by the undo")
	}
	// The order for the position that survived is untouched.
	if _, ok := mustGetOrder(t, idx, "mint", 0); !ok {
		t.Error("an unrelated order was lost")
	}
	if n := orderCount(t, idx); n != 1 {
		t.Errorf("%d order rows, want 1: the undo left a row for a position that no longer exists", n)
	}
}

// The counterpart to TestOrderForAPositionLostToAReorgIsNotServed: when
// the reorg re-mines the same transaction, the listing comes back.
//
// A reorg usually replaces the block, not the transactions in it. The
// outpoint is unchanged (a txid commits to the whole transaction, so the
// same key can only ever mean the same output), the signature still
// covers it, and the maker never withdrew anything -- so the offer is
// still live and should be served again without the maker doing anything.
//
// This is why the order row is left in place while its position is absent
// rather than deleted outright. It costs a row that is dead forever in the
// case where the transaction is never re-mined; deleting it would instead
// silently retire the maker's offer in the far commoner case where it is.
func TestOrderReturnsWhenItsTransactionIsReMined(t *testing.T) {
	idx := NewIndex()
	salt := []byte("0123456789abcdef")
	sig := signedPush(t)

	mintTx := RPCTx{
		TxID: "mint", Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, salt, 1.0)},
	}
	idx.ApplyBlock(makeBlock("h1-orphaned", 1, []RPCTx{mintTx}))
	if err := idx.PublishOrder(&Order{TxID: "mint", Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "76a914", PaymentValue: 700000000}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// The block is orphaned; while the position is absent, nothing is served.
	idx.UndoBlock(1)
	if _, ok := mustGetOrder(t, idx, "mint", 0); ok {
		t.Fatal("an order was served while its position did not exist")
	}

	// The replacement block carries the same transaction.
	idx.ApplyBlock(makeBlock("h1-winner", 1, []RPCTx{mintTx}))
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}

	got, ok := mustGetOrder(t, idx, "mint", 0)
	if !ok {
		t.Fatal("the maker's order did not come back when its transaction was re-mined")
	}
	if got.ScriptSig != sig || got.PaymentValue != 700000000 {
		t.Errorf("the restored order does not match the original: got %+v", got)
	}
}
