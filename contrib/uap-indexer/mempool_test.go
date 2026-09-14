package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestBroadcastTakesTheOrderOffTheBookImmediately is the double-fill race,
// at the only moment it is actually likely to happen.
//
// The relay knows a position is being spent the instant it broadcasts the
// fill -- it performed the broadcast. Before the hook existed it threw that
// away and waited for the mempool poller, leaving the order on the book for
// up to a full poll interval (five seconds by default): the exact window in
// which a second taker, who was looking at the book a moment ago, signs a
// fill of their own and pays a fee for a transaction the node will refuse.
//
// Teeth: delete the idx.noteBroadcast(txid) call in api.go's broadcast
// handler and this goes red, while every other order test stays green.
func TestBroadcastTakesTheOrderOffTheBookImmediately(t *testing.T) {
	idx := NewIndex()

	_, pubKeyHex := testMakerKey(t)
	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: pubKeyHex,
		Multiplier: 1000, Value: 5000000000, Height: 10,
	}
	seedPosition(t, idx, pos)

	order := &Order{
		TxID: pos.TxID, Vout: pos.Vout, Multiplier: pos.Multiplier,
		ScriptSig: signedPush(t), PaymentScript: "5678", PaymentValue: 700000000,
	}
	if err := idx.PublishOrder(order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	fillTxid := txid("fill1")
	// Stand in for main()'s wiring. The real hook reaches the node to learn
	// which positions the transaction spends; what is under test here is
	// that the broadcast path runs it at all, before answering.
	idx.SetBroadcastHook(func(broadcastTxid string) {
		if broadcastTxid != fillTxid {
			t.Errorf("hook got txid %q, want %q", broadcastTxid, fillTxid)
		}
		idx.SetPendingSpend(positionKey(pos.TxID, pos.Vout), broadcastTxid)
	})

	server := newAPIServer(idx, nil, nil, false,
		&FakeBroadcaster{SendRawTransactionResult: fillTxid})

	// The first taker's fill goes out through the relay.
	body := strings.NewReader(`{"hex":"00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/broadcast", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("broadcast returned %d: %s", rec.Code, rec.Body.String())
	}

	// The second taker reads the book. No poll has run; the only thing that
	// can have closed this order is the broadcast itself.
	orders, err := idx.ListOrders(nil)
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("the order is still on the book straight after its fill was "+
			"broadcast (%d open): a second taker would sign a fill the node "+
			"will refuse", len(orders))
	}

	// And a taker who had already opened the order is told why, rather than
	// being left to discover it from a rejected broadcast.
	got, ok, err := idx.GetOrder(pos.TxID, pos.Vout)
	if err != nil || !ok {
		t.Fatalf("GetOrder: %v, found=%v", err, ok)
	}
	if got.Status != "pending_fill" || got.PendingTxid != fillTxid {
		t.Errorf("order reports %q/%q, want pending_fill/%s",
			got.Status, got.PendingTxid, fillTxid)
	}
}

// TestAFailedBroadcastLeavesTheOrderOpen is the other half: the hook must not
// close an order over a transaction the node refused. Without this, one
// malformed submission against an outpoint would take somebody else's order
// off the book until the next poll.
func TestAFailedBroadcastLeavesTheOrderOpen(t *testing.T) {
	idx := NewIndex()

	_, pubKeyHex := testMakerKey(t)
	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: pubKeyHex,
		Multiplier: 1000, Value: 5000000000, Height: 10,
	}
	seedPosition(t, idx, pos)
	if err := idx.PublishOrder(&Order{
		TxID: pos.TxID, Vout: pos.Vout, Multiplier: pos.Multiplier,
		ScriptSig: signedPush(t), PaymentScript: "5678", PaymentValue: 700000000,
	}); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}

	hookRan := false
	idx.SetBroadcastHook(func(string) { hookRan = true })

	server := newAPIServer(idx, nil, nil, false, &FakeBroadcaster{
		SendRawTransactionError: &NodeRejection{Message: "16: bad-txns-inputs-missingorspent"},
	})
	body := strings.NewReader(`{"hex":"00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/broadcast", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected the node's rejection to surface as 400, got %d", rec.Code)
	}
	if hookRan {
		t.Error("the broadcast hook ran for a transaction the node refused")
	}

	orders, err := idx.ListOrders(nil)
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("a refused broadcast closed the order (%d open, want 1)", len(orders))
	}
}

// TestPendingSpendSurvivesAPerTransactionFetchFailure covers the asymmetry
// that UpdatePendingSpends used to carry.
//
// It refuses to clear the pending set when getrawmempool fails, on the
// grounds that wiping it would re-open orders that are being filled. But it
// rebuilds the set wholesale, so a single getrawtransaction failure -- one
// transaction, node otherwise fine -- reached the same outcome for that
// transaction by a different route: skipped during the rebuild, and
// therefore dropped from it.
//
// Teeth: replace the carry-forward loop in UpdatePendingSpends with a plain
// `continue` and this goes red.
func TestPendingSpendSurvivesAPerTransactionFetchFailure(t *testing.T) {
	idx := NewIndex()

	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: "02aa",
		Multiplier: 1000, Value: 5000000000, Height: 10,
	}
	seedPosition(t, idx, pos)
	outpoint := positionKey(pos.TxID, pos.Vout)

	fill := txid("fill1")
	idx.SetPendingSpend(outpoint, fill)

	// A node that still lists the transaction in its mempool but will not
	// hand it over -- a transient failure, not a disappearance.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "getrawmempool":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"result":[%q],"error":null,"id":1}`, fill)
		default:
			http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}
	if err := idx.UpdatePendingSpends(rpc); err != nil {
		t.Fatalf("UpdatePendingSpends: %v", err)
	}

	if got, ok := idx.IsPendingSpend(outpoint); !ok || got != fill {
		t.Fatalf("the pending spend was dropped by a rebuild that could not "+
			"fetch the transaction (got %q, present=%v): the order is back on "+
			"the book while its fill is still in the mempool", got, ok)
	}
}

// TestPendingSpendIsForgottenWhenTheTransactionLeavesTheMempool is the
// control for the one above: carrying entries forward must not make them
// permanent. A fill that is replaced, or confirmed, or evicted must let its
// order be reconsidered -- otherwise the first fix turns into a leak that
// closes orders forever.
func TestPendingSpendIsForgottenWhenTheTransactionLeavesTheMempool(t *testing.T) {
	idx := NewIndex()

	pos := &Position{
		TxID: txid("mint1"), Vout: 0, PubKey: "02aa",
		Multiplier: 1000, Value: 5000000000, Height: 10,
	}
	seedPosition(t, idx, pos)
	outpoint := positionKey(pos.TxID, pos.Vout)
	idx.SetPendingSpend(outpoint, txid("fill1"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":[],"error":null,"id":1}`)
	}))
	defer srv.Close()

	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}
	if err := idx.UpdatePendingSpends(rpc); err != nil {
		t.Fatalf("UpdatePendingSpends: %v", err)
	}
	if _, ok := idx.IsPendingSpend(outpoint); ok {
		t.Fatal("a transaction that is no longer in the mempool is still " +
			"holding its order closed")
	}
}
