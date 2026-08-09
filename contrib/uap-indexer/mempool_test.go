package main

import (
	"testing"
)

// TestOrderExcludedFromListWhenPositionPending: publish an order, then mark
// its position as spent by an unconfirmed transaction. ListOrders must omit it.
func TestOrderExcludedFromListWhenPositionPending(t *testing.T) {
	idx := NewIndex()

	// Create a position and seed it directly.
	_, pubKeyHex := testMakerKey(t)
	pos := &Position{
		TxID:       "tx001",
		Vout:       0,
		PubKey:     pubKeyHex,
		Multiplier: 1000,
		Value:      5000000000,
		IsMint:     false,
		Height:     10,
	}
	seedPosition(t, idx, pos)

	// Publish an order on that position.
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

	// Verify the order appears in ListOrders.
	orders, err := idx.ListOrders(nil)
	if err != nil {
		t.Fatalf("ListOrders failed: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}

	// Now mark the position as pending (spent by unconfirmed tx).
	pendingTxid := "unconf_fill_001"
	idx.SetPendingSpend(positionKey(pos.TxID, pos.Vout), pendingTxid)

	// ListOrders must now exclude it.
	orders, err = idx.ListOrders(nil)
	if err != nil {
		t.Fatalf("ListOrders failed after SetPendingSpend: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("expected 0 orders after marking position pending, got %d", len(orders))
	}

	// But GetOrder must still return it with pending status.
	foundOrder, ok, err := idx.GetOrder(pos.TxID, pos.Vout)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if !ok {
		t.Fatal("GetOrder returned not found, expected found")
	}
	if foundOrder.Status != "pending_fill" {
		t.Errorf("expected status 'pending_fill', got %q", foundOrder.Status)
	}
	if foundOrder.PendingTxid != pendingTxid {
		t.Errorf("expected pending_txid %q, got %q", pendingTxid, foundOrder.PendingTxid)
	}

	// When we clear the pending spend, the order should reappear in ListOrders.
	idx.ClearPendingSpend(positionKey(pos.TxID, pos.Vout))
	orders, err = idx.ListOrders(nil)
	if err != nil {
		t.Fatalf("ListOrders failed after ClearPendingSpend: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order after clearing pending, got %d", len(orders))
	}

	// And GetOrder should no longer show pending status.
	foundOrder, ok, err = idx.GetOrder(pos.TxID, pos.Vout)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if !ok {
		t.Fatal("GetOrder returned not found after clearing pending")
	}
	if foundOrder.Status != "" && foundOrder.Status != "confirmed" {
		t.Errorf("expected status to be empty or 'confirmed', got %q", foundOrder.Status)
	}
	if foundOrder.PendingTxid != "" {
		t.Errorf("expected pending_txid to be empty, got %q", foundOrder.PendingTxid)
	}
}

// TestGetRawMempoolCaching: the RPC client should cache txs by txid.
func TestGetRawMempoolIntegration(t *testing.T) {
	// We cannot test GetRawMempool directly without a real node, but we can
	// test the data structures that will hold its output.
	// This is more of a structure test than a functional one.

	// Verify that a PendingSpendSet can be created and read safely.
	ps := NewPendingSpendSet()

	// Add some pending spends.
	op1 := "tx001:0"
	op2 := "tx002:1"
	ps.Set(op1, "fill_tx_001")
	ps.Set(op2, "fill_tx_002")

	// Read them back.
	if txid := ps.Get(op1); txid != "fill_tx_001" {
		t.Errorf("expected 'fill_tx_001', got %q", txid)
	}
	if txid := ps.Get(op2); txid != "fill_tx_002" {
		t.Errorf("expected 'fill_tx_002', got %q", txid)
	}

	// Non-existent key should return empty.
	if txid := ps.Get("tx999:5"); txid != "" {
		t.Errorf("expected empty, got %q", txid)
	}

	// Remove one and verify.
	ps.Delete(op1)
	if txid := ps.Get(op1); txid != "" {
		t.Errorf("expected empty after delete, got %q", txid)
	}
	if txid := ps.Get(op2); txid != "fill_tx_002" {
		t.Errorf("expected 'fill_tx_002' still present, got %q", txid)
	}
}
