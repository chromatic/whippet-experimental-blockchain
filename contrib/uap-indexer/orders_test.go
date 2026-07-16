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
	idx.Positions[positionKey(pos.TxID, pos.Vout)] = pos

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

	got, ok := idx.GetOrder(pos.TxID, pos.Vout)
	if !ok {
		t.Fatal("expected to find the published order")
	}
	if got.PaymentValue != order.PaymentValue {
		t.Errorf("payment value mismatch: got %d, want %d", got.PaymentValue, order.PaymentValue)
	}

	orders := idx.ListOrders(nil)
	if len(orders) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(orders))
	}

	wrongMultiplier := int64(500)
	if orders := idx.ListOrders(&wrongMultiplier); len(orders) != 0 {
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
	if _, ok := idx.GetOrder(pos.TxID, pos.Vout); ok {
		t.Error("expected order to be gone after cancellation")
	}
}

func TestOrderRejectedForSpentPosition(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: "spent1", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1, Spent: true}
	idx.Positions[positionKey(pos.TxID, pos.Vout)] = pos

	order := &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 1}
	if err := idx.PublishOrder(order); err == nil {
		t.Error("expected PublishOrder to reject an order on an already-spent position")
	}
}

func TestOrderPrunedWhenPositionSpent(t *testing.T) {
	idx := NewIndex()
	pos := &Position{TxID: "prune1", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1}
	k := positionKey(pos.TxID, pos.Vout)
	idx.Positions[k] = pos
	idx.Orders[k] = &Order{TxID: pos.TxID, Vout: pos.Vout, Multiplier: 1000}

	block := &RPCBlock{
		Hash: "blockhash", Height: 1,
		Tx: []RPCTx{{
			TxID: "filltx",
			Vin:  []RPCVin{{TxID: pos.TxID, Vout: pos.Vout}},
			Vout: []RPCVout{},
		}},
	}
	idx.ApplyBlock(block)

	if _, ok := idx.GetOrder(pos.TxID, pos.Vout); ok {
		t.Error("expected order to be pruned once its position was spent")
	}
	if len(idx.Orders) != 0 {
		t.Errorf("expected Orders map to be empty, has %d entries", len(idx.Orders))
	}
}
