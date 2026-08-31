package main

import (
	"encoding/hex"
	"encoding/json"
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
	// order logic, not block indexing). A real key is needed here (not
	// fakePubKey) because this test also exercises CancelOrder, which
	// verifies a real signature against the position's pubkey.
	priv, pubKeyHex := testMakerKey(t)
	pos := &Position{TxID: txid("abc123"), Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 5000000000, IsMint: false, Height: 10}
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
	badOrder := &Order{TxID: txid("doesnotexist"), Vout: 0, Multiplier: 1000, ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(badOrder); err == nil {
		t.Error("expected PublishOrder to reject an order on an unknown position")
	}

	// Publishing with a mismatched multiplier must fail.
	mismatchOrder := &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 999, ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(mismatchOrder); err == nil {
		t.Error("expected PublishOrder to reject a multiplier mismatch")
	}

	// Cancelling with an invalid signature must fail.
	if err := idx.CancelOrder(pos.TxID, pos.Vout, "00"); err == nil {
		t.Error("expected CancelOrder to reject a malformed cancel signature")
	}
	// Cancelling with a valid signature by the position's own key succeeds.
	cancelSig := signCancel(t, priv, pos.TxID, pos.Vout, scriptSig, order.CancelNonce)
	if err := idx.CancelOrder(pos.TxID, pos.Vout, cancelSig); err != nil {
		t.Errorf("expected CancelOrder to succeed, got: %v", err)
	}
	if _, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout); ok {
		t.Error("expected order to be gone after cancellation")
	}
}

func TestOrderRejectedForSpentPosition(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: txid("spent1"), Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1, Spent: true}
	seedPosition(t, idx, pos)

	order := &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(order); err == nil {
		t.Error("expected PublishOrder to reject an order on an already-spent position")
	}
}

func TestOrderPrunedWhenPositionSpent(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: txid("prune1"), Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1}
	seedPosition(t, idx, pos)
	seedOrder(t, idx, &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000})

	block := &RPCBlock{
		Hash: "blockhash", Height: 1,
		Tx: []RPCTx{{
			TxID: txid("filltx"),
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
	idx.ApplyBlock(makeBlock("hash1", 1, []RPCTx{{
		TxID: txid("mint"), Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, 1.0)},
	}}))
	if err := idx.PublishOrder(&Order{
		TxID: txid("mint"), Vout: 0, Multiplier: 1000,
		ScriptSig: makeOrderScriptSig(), PaymentScript: "5678", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// The block that created the position is orphaned.
	idx.UndoBlock(1)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	if _, ok := mustPosition(t, idx, txid("mint"), 0); ok {
		t.Fatal("precondition: the position should be gone after the undo")
	}

	if _, ok := mustGetOrder(t, idx, txid("mint"), 0); ok {
		t.Error("an order was served for a position the chain no longer contains")
	}
	if got := mustListOrders(t, idx, nil); len(got) != 0 {
		t.Errorf("ListOrders returned %d orders for vanished positions, want 0", len(got))
	}
}

// --- cancellation edge cases ---

// A failed cancel must leave no trace. Writing the tombstone before
// checking the cancel signature, or writing it on the failure path, would
// hand anyone who can guess an outpoint a way to permanently block that
// order from ever being mirrored in again -- a denial of service that
// needs no signature and survives restarts.
func TestCancelWithGarbageSignatureChangesNothing(t *testing.T) {
	idx := NewIndex()
	_, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: txid("known"), Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	real := signedPush(t)
	if err := idx.PublishOrder(&Order{
		TxID: txid("known"), Vout: 0, Multiplier: 1000,
		ScriptSig: real, PaymentScript: "00", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	if err := idx.CancelOrder(txid("known"), 0, "48"+real[2:]); err == nil {
		t.Error("expected a cancel with a garbage signature to be rejected")
	}

	if _, ok := mustGetOrder(t, idx, txid("known"), 0); !ok {
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

// The core of the authorization fix: a well-formed signature that simply
// was not made by the position's own key must be rejected, even though it
// verifies fine against *some* key. Before this fix, the only "credential"
// checked was the order's own script_sig -- and that value is public
// (returned by GET /orders to any visitor), so anyone could reproduce it
// and cancel someone else's order. This pins down that the replacement
// scheme actually binds cancellation to the maker's key, not to public
// data.
func TestCancelSignedByWrongKeyIsRejected(t *testing.T) {
	idx := NewIndex()
	_, ownerPubKeyHex := testMakerKey(t)
	attackerPriv, _ := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: txid("known"), Vout: 0, PubKey: ownerPubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	order := &Order{
		TxID: txid("known"), Vout: 0, Multiplier: 1000,
		ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 100,
	}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// A signature that is perfectly valid, but made by a different key
	// than the one that owns the position.
	forged := signCancel(t, attackerPriv, txid("known"), 0, order.ScriptSig, order.CancelNonce)
	if err := idx.CancelOrder(txid("known"), 0, forged); err == nil {
		t.Fatal("expected a cancel signed by the wrong key to be rejected")
	}
	if _, ok := mustGetOrder(t, idx, txid("known"), 0); !ok {
		t.Error("an order was cancelled by a signature from the wrong key")
	}
}

// A valid signature over a *different* order (different outpoint) must
// not authorize cancelling this one, even though it was made by the same
// key. This is what binding the signed message to txid/vout/script_sig
// buys: possession of one valid cancel signature does not generalize to
// "this key may cancel anything it owns" without re-signing per order.
func TestCancelSignedForADifferentOrderIsRejected(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: txid("orderA"), Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	seedPosition(t, idx, &Position{
		TxID: txid("orderB"), Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	orderA := &Order{TxID: txid("orderA"), Vout: 0, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 100}
	orderB := &Order{TxID: txid("orderB"), Vout: 0, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 200}
	if err := idx.PublishOrder(orderA); err != nil {
		t.Fatalf("PublishOrder A: %v", err)
	}
	if err := idx.PublishOrder(orderB); err != nil {
		t.Fatalf("PublishOrder B: %v", err)
	}

	// A real signature by the right key, but over order A's identity.
	sigForA := signCancel(t, priv, txid("orderA"), 0, orderA.ScriptSig, orderA.CancelNonce)
	if err := idx.CancelOrder(txid("orderB"), 0, sigForA); err == nil {
		t.Fatal("expected a cancel signature captured for a different order to be rejected")
	}
	if _, ok := mustGetOrder(t, idx, txid("orderB"), 0); !ok {
		t.Error("order B was cancelled by a signature that authorized order A")
	}
	// The legitimate signature for A still works, proving the rejection
	// above was about identity binding and not some unrelated breakage.
	if err := idx.CancelOrder(txid("orderA"), 0, sigForA); err != nil {
		t.Errorf("expected the correctly-targeted signature to succeed, got: %v", err)
	}
}

// The replay case the design has to address: a maker withdraws an order
// and then republishes an *identical* one (same outpoint, same terms) on
// the same position. Because signing is deterministic (RFC 6979), the
// republished order's script_sig is bit-for-bit identical to the
// withdrawn one's -- so a cancel signature captured on the wire for the
// first listing must not still authorize cancelling the second. Binding
// the signed message to created_at (server-assigned, changes on every
// publish) is what prevents this; this test would fail if that binding
// were dropped and only (txid, vout, script_sig) were signed.
func TestCapturedCancelSignatureDoesNotReplayAfterRepublish(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: txid("known"), Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	scriptSig := signedPush(t)
	mk := func() *Order {
		return &Order{TxID: txid("known"), Vout: 0, Multiplier: 1000, ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100}
	}

	first := mk()
	if err := idx.PublishOrder(first); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	captured := signCancel(t, priv, txid("known"), 0, scriptSig, first.CancelNonce)
	if err := idx.CancelOrder(txid("known"), 0, captured); err != nil {
		t.Fatalf("first cancel: %v", err)
	}

	// The maker relists identically. Confirm the premise: the deterministic
	// signature really is byte-for-byte the same script_sig, and that
	// cancel_nonce is nonetheless guaranteed to differ -- including in the
	// realistic case where both publishes land in the same wall-clock
	// second, which created_at alone could not have told apart.
	second := mk()
	if err := idx.PublishOrder(second); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if second.ScriptSig != scriptSig || second.CancelNonce == first.CancelNonce {
		t.Fatalf("test setup: expected identical script_sig (got %q) and a fresh cancel_nonce (first=%d second=%d)",
			second.ScriptSig, first.CancelNonce, second.CancelNonce)
	}

	// The signature captured for the first cancellation must not cancel
	// the second listing.
	if err := idx.CancelOrder(txid("known"), 0, captured); err == nil {
		t.Fatal("a captured cancel signature replayed successfully against a republished order")
	}
	if _, ok := mustGetOrder(t, idx, txid("known"), 0); !ok {
		t.Error("the republished order was cancelled by a replayed signature")
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
		TxID: txid("known"), Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 500000000,
	})
	first := signedPush(t)
	if err := idx.PublishOrder(&Order{
		TxID: txid("known"), Vout: 0, Multiplier: 1000,
		ScriptSig: first, PaymentScript: "00", PaymentValue: 100,
	}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	der := []byte{0x30, 0x06, 0x02, 0x01, 0x09, 0x02, 0x01, 0x09}
	sig := append(der, sighashOrderType)
	second := hex.EncodeToString(append([]byte{byte(len(sig))}, sig...))
	if err := idx.PublishOrder(&Order{
		TxID: txid("known"), Vout: 0, Multiplier: 1000,
		ScriptSig: second, PaymentScript: "00", PaymentValue: 999,
	}); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	o, ok := mustGetOrder(t, idx, txid("known"), 0)
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
	sig := signedPush(t)

	idx.ApplyBlock(makeBlock("h1", 1, []RPCTx{{
		TxID: txid("mint"), Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, 1.0)},
	}}))
	order := &Order{TxID: txid("mint"), Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "76a914", PaymentValue: 700000000}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// A taker fills it.
	idx.ApplyBlock(makeBlock("h2", 2, []RPCTx{{
		TxID: txid("fill"), Vin: []RPCVin{spendVin(txid("mint"), 0)},
		Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x03), 1000, originOfMint(t, txid("mint"), 0), 1.0)},
	}}))
	if _, ok := mustGetOrder(t, idx, txid("mint"), 0); ok {
		t.Fatal("precondition: a filled order should not still be listed")
	}

	// ...and that block is orphaned.
	idx.UndoBlock(2)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	pos, ok := mustPosition(t, idx, txid("mint"), 0)
	if !ok || pos.Spent {
		t.Fatalf("precondition: the position should be back and unspent, got ok=%v spent=%v", ok, pos.Spent)
	}

	got, ok := mustGetOrder(t, idx, txid("mint"), 0)
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
	sig := signedPush(t)

	// One block both creates the position and spends it.
	idx.ApplyBlock(makeBlock("h1", 1, []RPCTx{{
		TxID: txid("mint"), Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, 1.0)},
	}}))
	if err := idx.PublishOrder(&Order{TxID: txid("mint"), Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "00", PaymentValue: 100}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	idx.ApplyBlock(makeBlock("h2", 2, []RPCTx{
		{
			TxID: txid("mint2"), Vin: []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, fakePubKey(0x04), 500, 1.0)},
		},
		{
			TxID: txid("spend2"), Vin: []RPCVin{spendVin(txid("mint2"), 0)},
			Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x05), 500, originOfMint(t, txid("mint2"), 0), 1.0)},
		},
	}))

	idx.UndoBlock(2)
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}
	if _, ok := mustPosition(t, idx, txid("mint2"), 0); ok {
		t.Fatal("precondition: mint2 should have been removed by the undo")
	}
	// The order for the position that survived is untouched.
	if _, ok := mustGetOrder(t, idx, txid("mint"), 0); !ok {
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
	sig := signedPush(t)

	mintTx := RPCTx{
		TxID: txid("mint"), Vin: []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), 1000, 1.0)},
	}
	idx.ApplyBlock(makeBlock("h1-orphaned", 1, []RPCTx{mintTx}))
	if err := idx.PublishOrder(&Order{TxID: txid("mint"), Vout: 0, Multiplier: 1000,
		ScriptSig: sig, PaymentScript: "76a914", PaymentValue: 700000000}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	// The block is orphaned; while the position is absent, nothing is served.
	idx.UndoBlock(1)
	if _, ok := mustGetOrder(t, idx, txid("mint"), 0); ok {
		t.Fatal("an order was served while its position did not exist")
	}

	// The replacement block carries the same transaction.
	idx.ApplyBlock(makeBlock("h1-winner", 1, []RPCTx{mintTx}))
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error: %v", err)
	}

	got, ok := mustGetOrder(t, idx, txid("mint"), 0)
	if !ok {
		t.Fatal("the maker's order did not come back when its transaction was re-mined")
	}
	if got.ScriptSig != sig || got.PaymentValue != 700000000 {
		t.Errorf("the restored order does not match the original: got %+v", got)
	}
}

// TestOrderBackingValuePopulated verifies that when an order is published,
// the underlying position's value is captured and returned in list and get responses.
// This is critical for buyers to determine if a position is fully backed.
func TestOrderBackingValuePopulated(t *testing.T) {
	idx := NewIndex()

	// Create a position with a known backing value
	positionValue := int64(5000000000) // 50 WHIP
	multiplier := int64(1000)
	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: "02aa",
		Multiplier: multiplier, Value: positionValue, IsMint: false, Height: 10,
	}
	seedPosition(t, idx, pos)

	// Publish an order for this position
	order := &Order{
		TxID:          pos.TxID,
		Vout:          pos.Vout,
		Multiplier:    multiplier,
		ScriptSig:     signedPush(t),
		PaymentScript: "5678",
		PaymentValue:  700000000,
	}

	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder failed: %v", err)
	}

	// GetOrder should return the backing value
	got, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout)
	if !ok {
		t.Fatal("expected to find the published order")
	}
	if got.BackingValue != positionValue {
		t.Errorf("GetOrder: expected backing value %d, got %d", positionValue, got.BackingValue)
	}

	// ListOrders should also return the backing value
	orders := mustListOrders(t, idx, nil)
	if len(orders) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(orders))
	}
	if orders[0].BackingValue != positionValue {
		t.Errorf("ListOrders: expected backing value %d, got %d", positionValue, orders[0].BackingValue)
	}

	// ListOrdersPage should also return the backing value
	page := Page{Limit: 10, Offset: 0}
	ordersPage, hasMore, err := idx.ListOrdersPage(nil, page)
	if err != nil {
		t.Fatalf("ListOrdersPage failed: %v", err)
	}
	if hasMore {
		t.Fatal("expected no more pages")
	}
	if len(ordersPage) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(ordersPage))
	}
	if ordersPage[0].BackingValue != positionValue {
		t.Errorf("ListOrdersPage: expected backing value %d, got %d", positionValue, ordersPage[0].BackingValue)
	}
}

// TestOrderBackingValueDerivedFromPosition is the test with teeth for the
// migration case, which TestOrderBackingValuePopulated above cannot reach:
// publishOrder writes BackingValue into the stored blob, so that test stays
// green even if the read path never derives it at all.
//
// Orders persist as a JSON blob (orders(key, data)), so every order written
// before backing_value existed carries no such key, and json.Unmarshal leaves
// it zero. Zero renders as an unbacked position -- precisely the ambiguity the
// field was added to remove. This strips the key back out of a stored blob to
// stand in for one of those rows, then reads it back: only the join in
// scanOrders can supply the right answer.
func TestOrderBackingValueDerivedFromPosition(t *testing.T) {
	idx := NewIndex()

	positionValue := int64(5000000000)
	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: "02aa",
		Multiplier: 1000, Value: positionValue, Height: 10,
	}
	seedPosition(t, idx, pos)

	order := &Order{
		TxID: pos.TxID, Vout: pos.Vout, Multiplier: pos.Multiplier,
		ScriptSig: signedPush(t), PaymentScript: "5678", PaymentValue: 700000000,
	}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder failed: %v", err)
	}

	// Rewrite the stored blob without backing_value, as a pre-existing row
	// would have been written.
	key := positionKey(pos.TxID, pos.Vout)
	var stored []byte
	if err := idx.store.db.QueryRow(`SELECT data FROM orders WHERE key = ?`, key).Scan(&stored); err != nil {
		t.Fatalf("reading stored order: %v", err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(stored, &fields); err != nil {
		t.Fatalf("unmarshalling stored order: %v", err)
	}
	if _, ok := fields["backing_value"]; !ok {
		t.Fatal("stored blob has no backing_value key; this test no longer simulates an old row")
	}
	delete(fields, "backing_value")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("remarshalling stored order: %v", err)
	}
	if _, err := idx.store.db.Exec(`UPDATE orders SET data = ? WHERE key = ?`, legacy, key); err != nil {
		t.Fatalf("rewriting stored order: %v", err)
	}

	got, ok := mustGetOrder(t, idx, pos.TxID, pos.Vout)
	if !ok {
		t.Fatal("expected to find the published order")
	}
	if got.BackingValue != positionValue {
		t.Errorf("GetOrder: expected backing %d derived from the position, got %d", positionValue, got.BackingValue)
	}

	orders := mustListOrders(t, idx, nil)
	if len(orders) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(orders))
	}
	if orders[0].BackingValue != positionValue {
		t.Errorf("ListOrders: expected backing %d derived from the position, got %d", positionValue, orders[0].BackingValue)
	}
}

// TestListOrdersPageExcludesPendingBeforePaging pins where the pending filter
// runs. Filtering the page in Go after the query drops rows LIMIT has already
// counted, so a page whose orders are all pending comes back EMPTY while
// hasMore still reports true -- a client paging naively stops at that empty
// page and never sees the open orders behind it.
//
// Here order "aa..:0" sorts first (OrdersOrderBy is o.key) and is pending,
// while "bb..:0" is open. Asking for one order must yield the open one.
func TestListOrdersPageExcludesPendingBeforePaging(t *testing.T) {
	idx := NewIndex()

	for _, txid := range []string{"aa", "bb"} {
		pos := &Position{
			TxID: txid, Vout: 0, PubKey: "02aa",
			Multiplier: 1000, Value: 5000000000, Height: 10,
		}
		seedPosition(t, idx, pos)
		order := &Order{
			TxID: pos.TxID, Vout: pos.Vout, Multiplier: pos.Multiplier,
			ScriptSig: signedPush(t), PaymentScript: "5678", PaymentValue: 700000000,
		}
		if err := idx.PublishOrder(order); err != nil {
			t.Fatalf("PublishOrder(%s) failed: %v", txid, err)
		}
	}

	idx.SetPendingSpend(positionKey("aa", 0), "fill-tx")

	got, _, err := idx.ListOrdersPage(nil, Page{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("ListOrdersPage failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a page of 1 should hold the one open order, got %d", len(got))
	}
	if got[0].TxID != "bb" {
		t.Errorf("expected the open order bb, got %s", got[0].TxID)
	}
}

// An order must publish the lineage of the position it sells.
//
// A taker cannot derive it. The order carries the maker's scriptSig, never
// their scriptPubKey, so for a transfer the origin is invisible from the
// taker's side; and deriving it from the maker's outpoint is right only for a
// fresh mint's first sale. Without this field a taker builds a transaction
// that looks correct, signs cleanly, and is rejected by consensus for naming
// a lineage with no input in the transaction.
//
// Derived from the position on read, like BackingValue, so an order stored
// before the field existed cannot serve an empty one.
func TestOrderPublishesItsPositionsLineage(t *testing.T) {
	idx := NewIndex()
	pubkey := fakePubKey(0x02)
	lineage := hex.EncodeToString(foreignOrigin(0x5e))

	// A transfer: the case where the origin is genuinely underivable.
	idx.ApplyBlock(makeBlock("h1", 1, []RPCTx{{
		TxID: txid("sale"),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{uapTransferVout(0, pubkey, 1000, foreignOrigin(0x5e), 1.0)},
	}}))

	if err := idx.PublishOrder(&Order{
		TxID: txid("sale"), Vout: 0, Multiplier: 1000,
		ScriptSig: signedPush(t), PaymentScript: "76a914", PaymentValue: 700000000,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	got, ok := mustGetOrder(t, idx, txid("sale"), 0)
	if !ok {
		t.Fatal("the order was not served")
	}
	if got.Origin != lineage {
		t.Errorf("order origin = %q, want the position's lineage %q", got.Origin, lineage)
	}

	// ...and the listing carries it too, which is where a taker reads it.
	listed := mustListOrders(t, idx, nil)
	if len(listed) != 1 {
		t.Fatalf("expected 1 listed order, got %d", len(listed))
	}
	if listed[0].Origin != lineage {
		t.Errorf("listed order origin = %q, want %q", listed[0].Origin, lineage)
	}

	// The part that publishing alone does not prove: an order row written
	// before this field existed holds a blob with no origin in it. Reads must
	// fill it in from the position rather than serve the empty string, or
	// every order stored by an older build becomes quietly unfillable.
	// Simulated by rewriting the stored blob with the field cleared.
	stale := got
	stale.Origin = ""
	blob, err := json.Marshal(&stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.store.db.Exec(`UPDATE orders SET data = ? WHERE key = ?`,
		blob, positionKey(txid("sale"), 0)); err != nil {
		t.Fatalf("rewriting the stored order: %v", err)
	}

	reread, ok := mustGetOrder(t, idx, txid("sale"), 0)
	if !ok {
		t.Fatal("the order vanished after its blob was rewritten")
	}
	if reread.Origin != lineage {
		t.Errorf("an order stored without an origin served %q; it must be "+
			"derived from the position on read, not trusted from the blob", reread.Origin)
	}
	relisted := mustListOrders(t, idx, nil)
	if len(relisted) != 1 || relisted[0].Origin != lineage {
		t.Errorf("listing served origin %q for a blob that carried none", relisted[0].Origin)
	}
}
