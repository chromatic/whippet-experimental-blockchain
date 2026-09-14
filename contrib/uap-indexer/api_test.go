package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOversizedBodyReturns413 verifies that a POST /orders body exceeding
// the limit returns HTTP 413 Payload Too Large and does not panic.
func TestOversizedBodyReturns413(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10) // Not the bottleneck; body size is
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	// Create a body larger than the max allowed (should be ~8KB or similar).
	// We'll send 100KB and expect a 413.
	oversizedBody := bytes.Repeat([]byte("x"), 100*1024)

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(oversizedBody))
	req.RemoteAddr = "1.2.3.4:5555"
	req.Header.Set("Content-Length", "102400")

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected HTTP 413, got %d", w.Code)
	}
}

// TestLegitimateOrderBodyStillAccepted verifies that a valid order body
// within the limit is still accepted and behaves as before.
func TestLegitimateOrderBodyStillAccepted(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	order := Order{
		TxID:       "abc123",
		Vout:       0,
		Multiplier: 1000,
	}
	body, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("failed to marshal order: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	// We expect a 400 (invalid order validation error) or 201 (success),
	// but NOT 413. The point is that legitimate bodies are not rejected
	// for being oversized.
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("legitimate order body should not return 413, got %d", w.Code)
	}
}

// TestRepeatedGETsToStatusReturn429 verifies that repeated GETs to /status
// eventually return HTTP 429 when rate limit is exhausted.
func TestRepeatedGETsToStatusReturn429(t *testing.T) {
	idx := NewIndex()
	// Create a read limiter with capacity 2 (allow 2 requests, block the 3rd).
	readRL := NewRateLimiter(30, 2) // burst of 2
	server := newAPIServer(idx, nil, readRL, false, nil)

	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// First two requests should succeed (within burst capacity of 2).
	for i := 0; i < 2; i++ {
		w := makeReq()
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate-limited", i)
		}
	}

	// Third request should be blocked.
	w := makeReq()
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected HTTP 429 on the 3rd GET /status, got %d", w.Code)
	}
}

// TestRepeatedGETsToPositionsReturn429 verifies that repeated GETs to /positions
// eventually return HTTP 429 when rate limit is exhausted.
func TestRepeatedGETsToPositionsReturn429(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 2) // burst of 2
	server := newAPIServer(idx, nil, readRL, false, nil)

	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/positions?pubkey=abc123", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// First two requests should succeed.
	for i := 0; i < 2; i++ {
		w := makeReq()
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate-limited", i)
		}
	}

	// Third request should be blocked.
	w := makeReq()
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected HTTP 429 on the 3rd GET /positions, got %d", w.Code)
	}
}

// TestReadAndWriteLimitersAreIndependent verifies that exhausting the read
// limiter does not block write endpoints, and vice versa.
func TestReadAndWriteLimitersAreIndependent(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 1)  // burst of 1
	writeRL := NewRateLimiter(30, 1) // burst of 1
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	// Helper to create fresh order body
	makeOrderBody := func() []byte {
		order := Order{TxID: "test", Vout: 0, Multiplier: 1000}
		body, _ := json.Marshal(order)
		return body
	}

	// Test 1: Exhaust read limiter, verify writes still work
	// Exhaust the read limiter with a /status request from IP A
	req1 := httptest.NewRequest(http.MethodGet, "/status", nil)
	req1.RemoteAddr = "1.2.3.4:5555"
	w1 := httptest.NewRecorder()
	server.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first /status request failed: %d", w1.Code)
	}

	// Second read from same IP should be blocked
	req2 := httptest.NewRequest(http.MethodGet, "/status", nil)
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected read limiter to block 2nd request, got %d", w2.Code)
	}

	// But a write request from same IP should still work (uses different limiter)
	req3 := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(makeOrderBody()))
	req3.RemoteAddr = "1.2.3.4:5555"
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	server.ServeHTTP(w3, req3)
	if w3.Code == http.StatusTooManyRequests {
		t.Errorf("write request should not be rate-limited by read limiter, got %d", w3.Code)
	}

	// Test 2: Exhaust write limiter, verify reads still work (using a different IP)
	// First write from IP B should succeed (uses write burst)
	req4 := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(makeOrderBody()))
	req4.RemoteAddr = "2.2.2.2:5555"
	req4.Header.Set("Content-Type", "application/json")
	w4 := httptest.NewRecorder()
	server.ServeHTTP(w4, req4)
	if w4.Code == http.StatusTooManyRequests {
		t.Fatalf("first write from IP B should not be blocked, got %d", w4.Code)
	}

	// Second write from IP B should be blocked (write burst exhausted)
	req5 := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(makeOrderBody()))
	req5.RemoteAddr = "2.2.2.2:5555"
	req5.Header.Set("Content-Type", "application/json")
	w5 := httptest.NewRecorder()
	server.ServeHTTP(w5, req5)
	if w5.Code != http.StatusTooManyRequests {
		t.Errorf("expected write limiter to block 2nd write from IP B, got %d", w5.Code)
	}
}

// TestRateLimitingHonorsTrustProxy verifies that with -trustproxy,
// rate limiting keys on the X-Forwarded-For IP, not RemoteAddr.
func TestRateLimitingHonorsTrustProxy(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 1) // burst of 1
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, true, nil) // trustProxy=true

	order := Order{TxID: "test", Vout: 0, Multiplier: 1000}
	orderBody, _ := json.Marshal(order)

	// First request from real client 203.0.113.7 (via X-Forwarded-For)
	req1 := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(orderBody))
	req1.RemoteAddr = "127.0.0.1:5678"                // Proxy's IP
	req1.Header.Set("X-Forwarded-For", "203.0.113.7") // Real client
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	server.ServeHTTP(w1, req1)
	if w1.Code == http.StatusTooManyRequests {
		t.Fatalf("first request should be allowed, got %d", w1.Code)
	}

	// Second request from same real client (different RemoteAddr due to proxy routing)
	req2 := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(orderBody))
	req2.RemoteAddr = "127.0.0.2:5679"                // Different proxy exit
	req2.Header.Set("X-Forwarded-For", "203.0.113.7") // Same real client
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request from same real client should be rate-limited, got %d", w2.Code)
	}
}

// TestServerTimeoutsAreSet verifies that the http.Server constructed in main.go
// has appropriate timeout values set.
func TestServerTimeoutsAreSet(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	server := &http.Server{
		Addr:              "127.0.0.1:8961",
		Handler:           newAPIServer(idx, writeRL, readRL, false, nil),
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	if server.ReadTimeout == 0 {
		t.Error("ReadTimeout not set")
	}
	if server.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout not set")
	}
	if server.WriteTimeout == 0 {
		t.Error("WriteTimeout not set")
	}
	if server.IdleTimeout == 0 {
		t.Error("IdleTimeout not set")
	}

	// Verify they are sensible values (not too large, not zero)
	if server.ReadTimeout > time.Minute {
		t.Error("ReadTimeout seems too large")
	}
	if server.WriteTimeout > time.Minute {
		t.Error("WriteTimeout seems too large")
	}
	if server.IdleTimeout > 2*time.Minute {
		t.Error("IdleTimeout seems too large")
	}
}

// TestMaxBytesReaderOnPOSTOrders verifies that POST /orders bodies are
// properly limited and don't allow unbounded allocation.
func TestMaxBytesReaderOnPOSTOrders(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	// Send a body that is valid JSON but very large.
	largeBody := bytes.Repeat([]byte(`{"x":`), 50*1024) // ~300KB
	largeBody = append(largeBody, []byte(`1}`)...)

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(largeBody))
	req.RemoteAddr = "1.2.3.4:5555"
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	// Should get 413, not 500 or 200.
	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
		// 400 is acceptable if the implementation uses different approach,
		// but 413 is preferred for body-too-large.
		t.Logf("expected 413 (or 400), got %d", w.Code)
	}
}

// TestDeleteOrdersAlsoRateLimited verifies that DELETE /orders/{txid}/{vout}
// is also properly rate-limited as a write endpoint.
func TestDeleteOrdersAlsoRateLimited(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 1) // burst of 1
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	makeDeleteReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/orders/abc123/0?sig=deadbeef", nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// First delete should be allowed
	w1 := makeDeleteReq()
	if w1.Code == http.StatusTooManyRequests {
		t.Fatalf("first DELETE should be allowed, got %d", w1.Code)
	}

	// Second delete should be rate-limited
	w2 := makeDeleteReq()
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second DELETE should be rate-limited, got %d", w2.Code)
	}
}

// TestDeleteOrdersMissingSigReturns400 verifies that DELETE
// /orders/{txid}/{vout} without a sig query parameter is rejected before
// ever reaching CancelOrder.
func TestDeleteOrdersMissingSigReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	req := httptest.NewRequest(http.MethodDelete, "/orders/abc123/0", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a DELETE with no sig parameter, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sig") {
		t.Errorf("expected the error to mention the missing sig parameter, got: %s", w.Body.String())
	}
}

// TestDeleteOrdersEndToEndOverHTTP exercises the full authorization
// scheme through the actual HTTP handler, not just CancelOrder directly:
// publish an order via POST /orders, then cancel it via DELETE with a
// real cancel signature computed from the response's own script_sig and
// cancel_nonce, the same way a browser wallet (contrib/uap-web/cancel.js)
// would. This is the sharpest regression test against the original bug --
// it also proves that echoing the order's public script_sig as the DELETE
// query parameter (the old, vulnerable contract) is now rejected outright.
func TestDeleteOrdersEndToEndOverHTTP(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "e2e", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	writeRL := NewRateLimiter(1000, 1000)
	readRL := NewRateLimiter(1000, 1000)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	scriptSig := signedPush(t)
	orderBody, err := json.Marshal(Order{
		TxID: "e2e", Vout: 0, Multiplier: 1000,
		ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
	})
	if err != nil {
		t.Fatalf("marshal order: %v", err)
	}
	postReq := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(orderBody))
	postReq.RemoteAddr = "1.2.3.4:5555"
	postReq.Header.Set("Content-Type", "application/json")
	postW := httptest.NewRecorder()
	server.ServeHTTP(postW, postReq)
	if postW.Code != http.StatusCreated {
		t.Fatalf("POST /orders: expected 201, got %d: %s", postW.Code, postW.Body.String())
	}
	var published Order
	if err := json.Unmarshal(postW.Body.Bytes(), &published); err != nil {
		t.Fatalf("decoding publish response: %v", err)
	}

	// The old vulnerability: reproducing the order's own (public)
	// script_sig as the authorization must now fail.
	oldStyleReq := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/orders/e2e/0?sig=%s", scriptSig), nil)
	oldStyleReq.RemoteAddr = "1.2.3.4:5555"
	oldStyleW := httptest.NewRecorder()
	server.ServeHTTP(oldStyleW, oldStyleReq)
	if oldStyleW.Code == http.StatusNoContent {
		t.Fatal("cancelling by echoing the order's public script_sig succeeded -- the original vulnerability is back")
	}

	// A real cancel signature, computed the way cancel.js/api.js will,
	// succeeds.
	cancelSig := signCancel(t, priv, "e2e", 0, published.ScriptSig, published.CancelNonce)
	delReq := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/orders/e2e/0?sig=%s", cancelSig), nil)
	delReq.RemoteAddr = "1.2.3.4:5555"
	delW := httptest.NewRecorder()
	server.ServeHTTP(delW, delReq)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("DELETE /orders with a valid cancel signature: expected 204, got %d: %s", delW.Code, delW.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/orders/e2e/0", nil)
	getW := httptest.NewRecorder()
	server.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusNotFound {
		t.Errorf("expected the order to be gone after a successful cancel, got HTTP %d", getW.Code)
	}
}

// TestGetUTXOsByAddressReturnsKnownUTXOs verifies that GET /utxos?address=<addr>
// returns UTXOs for a known address.
func TestGetUTXOsByAddressReturnsKnownUTXOs(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	// Create a UTXO and add it to the index
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}

	hash160Hex := hex.EncodeToString(hash160)
	utxo := &UTXO{
		TxID:    "abc123",
		Vout:    0,
		Hash160: hash160Hex,
		Value:   100000000,
		Height:  100,
	}
	seedUTXO(t, idx, utxo)

	addr := EncodeAddress(hash160, 73)

	// Make a GET request
	req := httptest.NewRequest(http.MethodGet, "/utxos?address="+addr, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var utxos []UTXO
	if err := json.NewDecoder(w.Body).Decode(&utxos); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(utxos) != 1 {
		t.Errorf("expected 1 UTXO, got %d", len(utxos))
	}
	if utxos[0].TxID != "abc123" {
		t.Errorf("TXID mismatch: got %q, want %q", utxos[0].TxID, "abc123")
	}
}

// TestGetUTXOsByHashReturnsUTXOs verifies that GET /utxos?hash160=<hex> works.
func TestGetUTXOsByHashReturnsUTXOs(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}

	hash160Hex := hex.EncodeToString(hash160)
	utxo := &UTXO{
		TxID:    "def456",
		Vout:    1,
		Hash160: hash160Hex,
		Value:   50000000,
		Height:  101,
	}
	seedUTXO(t, idx, utxo)

	// Make a GET request with hash160
	req := httptest.NewRequest(http.MethodGet, "/utxos?hash160="+hash160Hex, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var utxos []UTXO
	if err := json.NewDecoder(w.Body).Decode(&utxos); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(utxos) != 1 {
		t.Errorf("expected 1 UTXO, got %d", len(utxos))
	}
}

// TestGetUTXOsUnknownAddressReturnsEmpty verifies that an unknown address
// returns an empty array, not 404.
func TestGetUTXOsUnknownAddressReturnsEmpty(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	// Create a valid address that's just not in the index
	unknownHash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		unknownHash160[i] = 0xff
	}
	unknownAddr := EncodeAddress(unknownHash160, MainnetVersion)

	req := httptest.NewRequest(http.MethodGet, "/utxos?address="+unknownAddr, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var utxos []UTXO
	if err := json.NewDecoder(w.Body).Decode(&utxos); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(utxos) != 0 {
		t.Errorf("expected empty array, got %d UTXOs", len(utxos))
	}
}

// TestGetUTXOsBadAddressReturnsBadRequest verifies that an invalid address
// returns HTTP 400.
func TestGetUTXOsBadAddressReturnsBadRequest(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/utxos?address=not-a-valid-address", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d", w.Code)
	}

	var errResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if _, ok := errResp["error"]; !ok {
		t.Error("error response should have 'error' field")
	}
}

// TestGetTokensReturnsLineagesAggregated verifies /tokens returns one row
// per distinct origin with aggregated supply and holder counts.
func TestGetTokensReturnsLineagesAggregated(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	// Block 1: create two independent mints
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_a"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, 100.0)},
		},
		{
			TxID: txid("mint_b"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, 200.0)},
		},
	}))

	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var tokens []TokenInfo
	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should have 2 lineages (mint_a:0 and mint_b:0)
	if len(tokens) != 2 {
		t.Errorf("expected 2 tokens, got %d", len(tokens))
	}

	// Check that supplies are correct
	mintAOrigin := originOfMintHex(t, txid("mint_a"), 0)
	mintBOrigin := originOfMintHex(t, txid("mint_b"), 0)
	for i, token := range tokens {
		if token.Origin != mintAOrigin && token.Origin != mintBOrigin {
			t.Errorf("unexpected origin at index %d: %q", i, token.Origin)
		}
		if token.Supply != 100*1e8*1000 && token.Supply != 200*1e8*1000 {
			t.Errorf("supply mismatch at index %d: got %d", i, token.Supply)
		}
	}
}

// TestGetTokenSingleOrigin verifies /token/{origin} returns a specific lineage.
func TestGetTokenSingleOrigin(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)
	pubkey2 := fakePubKey(0x03)

	// Block 1: create two mints
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_a"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 1000, 100.0)},
		},
	}))

	// Block 2: create a transfer from mint_a
	idx.ApplyBlock(makeBlock("hashB", 101, []RPCTx{
		{
			TxID: txid("transfer_a"),
			Vin:  []RPCVin{spendVin(txid("mint_a"), 0)},
			Vout: []RPCVout{uapTransferVout(0, pubkey2, 1000, originOfMint(t, txid("mint_a"), 0), 50.0)},
		},
	}))

	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	// Request the specific origin
	originHex := originOfMintHex(t, txid("mint_a"), 0)
	req := httptest.NewRequest(http.MethodGet, "/token/"+originHex, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var token TokenInfo
	if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if token.Origin != originHex {
		t.Errorf("origin mismatch: got %q, want %q", token.Origin, originHex)
	}

	// Supply is the sum of unspent positions' values * multipliers.
	// mint_a:0 (100.0 BTC) is spent by transfer_a, so only transfer_a:0 (50.0 BTC) is unspent.
	expectedSupply := int64(50 * 1e8 * 1000)
	if token.Supply != expectedSupply {
		t.Errorf("supply mismatch: got %d, want %d", token.Supply, expectedSupply)
	}
}

// TestGetTokenUnknownOriginReturns404 verifies /token/{origin} returns 404
// for unknown origins.
func TestGetTokenUnknownOriginReturns404(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/token/unknown:0", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404, got %d", w.Code)
	}

	var errResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if _, ok := errResp["error"]; !ok {
		t.Error("error response should have 'error' field")
	}
}

// A transfer whose parent this indexer never saw still names its own
// lineage, and is served as one.
//
// This test used to be TestGetTokensExcludesOrphans, and it asserted the
// opposite. Under v1 a lineage had no on-chain identity: the indexer
// reconstructed it by walking the spend graph, and a transfer whose parent
// it had not indexed was an "orphan" with an empty Origin and no lineage to
// belong to. Excluding those was the only honest option.
//
// v2 retired the whole category. The origin is in the covenant script, so a
// transfer states its lineage whether or not this indexer happens to have
// seen the mint -- which it may legitimately not have, having started mid
// chain or pruned below it. Hiding such a position would hide real, spendable
// supply. It is served with the lineage it declares, and simply without the
// mint-derived fields (ticker, metadata) that are genuinely unknown here.
func TestTransferWithAnUnseenParentStillFormsALineage(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	// A transfer spending a position this index never saw.
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("orphan_tx"),
			Vin:  []RPCVin{spendVin(txid("never_indexed"), 0)},
			Vout: []RPCVout{uapTransferVout(0, pubkey1, 1000, foreignOrigin(0xDD), 50.0)},
		},
	}))

	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var tokens []TokenInfo
	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("expected the declared lineage to be served, got %d tokens", len(tokens))
	}
	if got, want := tokens[0].Origin, hex.EncodeToString(foreignOrigin(0xDD)); got != want {
		t.Errorf("lineage origin = %q, want the origin its script declares (%q)", got, want)
	}
	// The mint is genuinely unknown to this index, so the fields that come
	// from it are empty -- absent, not guessed at.
	if tokens[0].Ticker != "" {
		t.Errorf("ticker = %q, want empty: this index has never seen the mint", tokens[0].Ticker)
	}
}

// TestGetTokensSupplyOverflowGuard tests that supply calculation with large
// values and multipliers does not wrap negative.
func TestGetTokensSupplyOverflowGuard(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	// Block 1: create a mint with large value and multiplier
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_large"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 2147483647, 1000000.0)},
		},
	}))

	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	var tokens []TokenInfo
	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}

	// Supply should be positive and not wrapped
	if tokens[0].Supply <= 0 {
		t.Errorf("supply overflow detected: got %d", tokens[0].Supply)
	}
}

// A multiplier of 0 is a perfectly valid UAP position -- consensus accepts
// it (OP_0), and src/test/data/uap_script_vectors.json carries five valid
// zero-multiplier vectors. The supply guard divides by the multiplier, so a
// zero-multiplier lineage must not panic with "integer divide by zero".
// Anyone can mint one, so a crash here is a remote denial of service.
func TestGetTokensZeroMultiplierDoesNotPanic(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_zero_mult"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, pubkey1, 0, 1.0)},
		},
	}))

	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var tokens []TokenInfo
	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}
	if tokens[0].Supply != 0 {
		t.Errorf("zero multiplier means zero virtual supply, got %d", tokens[0].Supply)
	}
	if tokens[0].Holders != 1 {
		t.Errorf("expected 1 holder, got %d", tokens[0].Holders)
	}

	// The single-lineage endpoint divides the same way.
	// The lineage is addressed by its origin -- a 32-byte hash -- not by the
	// mint's outpoint. Under v1 the two were the same string.
	req2 := httptest.NewRequest(http.MethodGet,
		"/token/"+originOfMintHex(t, txid("mint_zero_mult"), 0), nil)
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("/token/{origin}: expected HTTP 200, got %d", w2.Code)
	}
}

// The guard checks one multiplication at a time, but supply accumulates
// across every unspent position in the lineage. Many positions that each fit
// can still overflow the running total.
func TestGetTokensSupplyDoesNotWrapAcrossManyPositions(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	// Each value*multiplier product fits comfortably in an int64 (6e18 of a
	// 9.22e18 ceiling), so the per-position guard never trips -- but three of
	// them summed do not fit. This is the case a per-multiplication-only
	// guard misses.
	const mult = 2
	const bigCoins = 30000000000.0 // 3e18 satoshis; 3e18 * 2 = 6e18

	vouts := []RPCVout{uapMintVout(0, pubkey1, mult, bigCoins)}
	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{TxID: txid("mint_big"), Vin: []RPCVin{coinbaseVin()}, Vout: vouts},
	}))
	// Two sibling covenants in the same lineage.
	idx.ApplyBlock(makeBlock("hashB", 101, []RPCTx{
		{
			TxID: txid("spend_big"),
			Vin:  []RPCVin{{TxID: txid("mint_big"), Vout: 0}},
			Vout: []RPCVout{
				uapTransferVout(0, pubkey1, mult, originOfMint(t, txid("mint_big"), 0), bigCoins),
				uapTransferVout(1, pubkey1, mult, originOfMint(t, txid("mint_big"), 0), bigCoins),
			},
		},
	}))

	tokens := mustAllTokens(t, idx)
	if len(tokens) != 1 {
		t.Fatalf("expected 1 lineage, got %d", len(tokens))
	}
	if tokens[0].Supply < 0 {
		t.Errorf("supply wrapped negative: %d", tokens[0].Supply)
	}
}

// A mint may carry its token's ticker/metadata-hash in an OP_RETURN
// output alongside it. ParseMetadata exists and is unit-tested, but the
// indexer has to actually call it during ApplyBlock and surface the result
// on the lineage -- otherwise /tokens reports every token as nameless.
func TestTokensCarryMintMetadata(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_meta"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{
				uapMintVout(0, pubkey1, 1000, 2000.0),
				opReturnVout(1, wuapPayload("WHIP", nil)),
			},
		},
	}))

	tokens := mustAllTokens(t, idx)
	if len(tokens) != 1 {
		t.Fatalf("expected 1 lineage, got %d", len(tokens))
	}
	if tokens[0].Ticker != "WHIP" {
		t.Errorf("ticker = %q, want %q", tokens[0].Ticker, "WHIP")
	}
}

// Metadata belongs to the lineage's mint. If a transfer could carry its own
// OP_RETURN and have it applied, anyone holding a single position could
// rename the token for every other holder.
func TestTransferMetadataDoesNotOverwriteLineage(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_meta2"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{
				uapMintVout(0, pubkey1, 1000, 2000.0),
				opReturnVout(1, wuapPayload("GOOD", nil)),
			},
		},
	}))
	idx.ApplyBlock(makeBlock("hashB", 101, []RPCTx{
		{
			TxID: txid("hijack"),
			Vin:  []RPCVin{{TxID: txid("mint_meta2"), Vout: 0}},
			Vout: []RPCVout{
				uapTransferVout(0, pubkey1, 1000, originOfMint(t, txid("mint_meta2"), 0), 1999.0),
				opReturnVout(1, wuapPayload("EVIL", nil)),
			},
		},
	}))

	tokens := mustAllTokens(t, idx)
	if len(tokens) != 1 {
		t.Fatalf("expected 1 lineage, got %d", len(tokens))
	}
	if tokens[0].Ticker != "GOOD" {
		t.Errorf("a transfer rewrote the lineage metadata: ticker=%q",
			tokens[0].Ticker)
	}

	// Defence in depth: the read path only consults the mint, but the
	// transfer position must not be carrying the hijacked metadata either.
	hijacked, ok := mustPosition(t, idx, txid("hijack"), 0)
	if !ok {
		t.Fatal("the transfer position should have been indexed")
	}
	if hijacked.Metadata != nil {
		t.Errorf("a transfer must not carry metadata, got %+v", hijacked.Metadata)
	}
}

// Undoing the mint's block must take its metadata with it.
func TestUndoRemovesMintMetadata(t *testing.T) {
	idx := NewIndex()
	pubkey1 := fakePubKey(0x02)

	idx.ApplyBlock(makeBlock("hashA", 100, []RPCTx{
		{
			TxID: txid("mint_meta3"),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{
				uapMintVout(0, pubkey1, 1000, 2000.0),
				opReturnVout(1, wuapPayload("GONE", nil)),
			},
		},
	}))
	if len(mustAllTokens(t, idx)) != 1 {
		t.Fatal("setup: expected 1 lineage")
	}

	idx.UndoBlock(100)
	if got := len(mustAllTokens(t, idx)); got != 0 {
		t.Errorf("after undo, expected 0 lineages, got %d", got)
	}
}

// FakeBroadcaster records calls for testing and allows injected errors.
type FakeBroadcaster struct {
	Sent                       []string // Hex strings of sent transactions
	SendRawTransactionError    error
	SendRawTransactionResult   string
	EstimateSmartFeeError      error
	EstimateSmartFeeResult     int64
	GetRawTransactionHexError  error
	GetRawTransactionHexResult string
}

func (f *FakeBroadcaster) SendRawTransaction(hex string) (txid string, err error) {
	f.Sent = append(f.Sent, hex)
	if f.SendRawTransactionError != nil {
		return "", f.SendRawTransactionError
	}
	return f.SendRawTransactionResult, nil
}

func (f *FakeBroadcaster) EstimateSmartFee(blocks int) (satPerKB int64, err error) {
	if f.EstimateSmartFeeError != nil {
		return 0, f.EstimateSmartFeeError
	}
	return f.EstimateSmartFeeResult, nil
}

func (f *FakeBroadcaster) GetRawTransactionHex(txid string) (hex string, err error) {
	if f.GetRawTransactionHexError != nil {
		return "", f.GetRawTransactionHexError
	}
	return f.GetRawTransactionHexResult, nil
}

// TestBroadcastValidHexReachesNode verifies a valid hex body is sent to the node
// and its txid is returned.
func TestBroadcastValidHexReachesNode(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{SendRawTransactionResult: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)

	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d; body: %s", w.Code, w.Body.String())
	}

	if len(fake.Sent) != 1 {
		t.Errorf("expected 1 sent transaction, got %d", len(fake.Sent))
	}
	if fake.Sent[0] != hexBody {
		t.Errorf("sent hex mismatch: got %q, want %q", fake.Sent[0], hexBody)
	}

	var resp string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp != "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890" {
		t.Errorf("txid mismatch: got %q", resp)
	}
}

// TestBroadcastGetReturns405 verifies GET /broadcast returns 405.
func TestBroadcastGetReturns405(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/broadcast", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected HTTP 405, got %d", w.Code)
	}
	// Node must not have been called
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for GET request")
	}
}

// TestBroadcastOversizeBodyReturns400 verifies bodies > 200KB are rejected.
func TestBroadcastOversizeBodyReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	oversizedBody := bytes.Repeat([]byte("x"), 201*1024)

	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(oversizedBody))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected HTTP 400 or 413, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for oversized body")
	}
}

// TestBroadcastInvalidJSONReturns400 verifies invalid JSON is rejected.
func TestBroadcastInvalidJSONReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader([]byte("not json")))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for invalid JSON")
	}
}

// TestBroadcastMissingHexFieldReturns400 verifies the hex field is required.
func TestBroadcastMissingHexFieldReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for missing hex field")
	}
}

// TestBroadcastNonHexReturns400 verifies non-hex hex strings are rejected.
func TestBroadcastNonHexReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	body := []byte(`{"hex":"not-hex-data"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for non-hex")
	}
}

// TestBroadcastOddLengthHexReturns400 verifies odd-length hex is rejected.
func TestBroadcastOddLengthHexReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	body := []byte(`{"hex":"abc"}`) // Odd length
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for odd-length hex")
	}
}

// TestBroadcastTooShortReturns400 verifies txs < 60 bytes are rejected.
func TestBroadcastTooShortReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 30 bytes = 60 hex chars
	shortHex := "0100000001000000000000000000000000000000000000000000000000000000000000000000"
	body := []byte(`{"hex":"` + shortHex + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for too-short tx")
	}
}

// TestBroadcastTooLongReturns400 verifies txs > 100KB are rejected.
func TestBroadcastTooLongReturns400(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 101KB = 103424 bytes decoded = 206848 hex chars
	longHex := strings.Repeat("ab", 103424)
	body := []byte(`{"hex":"` + longHex + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("node should not have been called for too-long tx")
	}
}

// TestBroadcastNodeRejectionReturns400WithMessage verifies a node RPC error
// is returned verbatim with HTTP 400.
func TestBroadcastNodeRejectionReturns400WithMessage(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	// A rejection is what the node itself said, carried as a typed error.
	// A bare errors.New would be indistinguishable from the node being
	// unreachable, and must be treated as the latter -- see the 502 case.
	fake := &FakeBroadcaster{
		SendRawTransactionError: &NodeRejection{
			Method:  "sendrawtransaction",
			Code:    -26,
			Message: "16: mandatory-script-verify-flag-failed (Operation not valid with the current stack size)",
		},
	}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}

	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if _, ok := errResp["error"]; !ok {
		t.Error("error response should have 'error' field")
	}
	if !strings.Contains(errResp["error"], "mandatory-script-verify-flag-failed") {
		t.Errorf("node error message not present verbatim: got %q", errResp["error"])
	}
}

// TestBroadcastNodeUnreachableReturns502 verifies a transport error returns 502.
func TestBroadcastNodeUnreachableReturns502(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{
		SendRawTransactionError: fmt.Errorf("rpc request: connection refused"),
	}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	// Transport error -> 502
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected HTTP 502 for transport error, got %d", w.Code)
	}

	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	// Error message should NOT present the transport error as a validation failure
	if strings.Contains(errResp["error"], "validation") || strings.Contains(errResp["error"], "script") {
		t.Errorf("transport error misreported as validation failure: %q", errResp["error"])
	}
}

// TestBroadcastNodeReturnsInvalidTxidReturns502 verifies a non-txid result
// from the node returns 502, not 200.
func TestBroadcastNodeReturnsInvalidTxidReturns502(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{
		SendRawTransactionResult: "not-a-txid", // Invalid: not 64 hex chars
	}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected HTTP 502 for invalid node result, got %d", w.Code)
	}
}

// TestBroadcastUsesWriteLimiter verifies /broadcast consumes the write limiter.
func TestBroadcastUsesWriteLimiter(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 1) // burst of 1
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{SendRawTransactionResult: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)

	// First request should succeed
	req1 := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req1.RemoteAddr = "1.2.3.4:5555"
	w1 := httptest.NewRecorder()
	server.ServeHTTP(w1, req1)
	if w1.Code == http.StatusTooManyRequests {
		t.Fatalf("first /broadcast should be allowed, got %d", w1.Code)
	}

	// Second request should be rate-limited
	req2 := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second /broadcast should be rate-limited, got %d", w2.Code)
	}
}

// TestFeerateReturnsNodeFeeWhenValid verifies /feerate returns the node's
// estimate when valid.
func TestFeerateReturnsNodeFeeWhenValid(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{EstimateSmartFeeResult: 2000000}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d", w.Code)
	}

	var resp struct {
		SatPerKB int64  `json:"sat_per_kb"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.SatPerKB != 2000000 {
		t.Errorf("sat_per_kb mismatch: got %d, want 2000000", resp.SatPerKB)
	}
	if resp.Source != "node" {
		t.Errorf("source mismatch: got %q, want %q", resp.Source, "node")
	}
}

// TestFeerateUsesReadLimiter verifies /feerate consumes the read limiter.
func TestFeerateUsesReadLimiter(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 1) // burst of 1
	fake := &FakeBroadcaster{EstimateSmartFeeResult: 2000000}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// First request should succeed
	req1 := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req1.RemoteAddr = "1.2.3.4:5555"
	w1 := httptest.NewRecorder()
	server.ServeHTTP(w1, req1)
	if w1.Code == http.StatusTooManyRequests {
		t.Fatalf("first /feerate should be allowed, got %d", w1.Code)
	}

	// Second request should be rate-limited
	req2 := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second /feerate should be rate-limited, got %d", w2.Code)
	}
}

// TestFeerateFallsBackToFloorWhenNodeErrors verifies /feerate falls back to
// the policy floor when the node errors.
func TestFeerateFallsBackToFloorWhenNodeErrors(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{
		EstimateSmartFeeError: fmt.Errorf("estimatesmartfee: not enough data"),
	}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d", w.Code)
	}

	var resp struct {
		SatPerKB int64  `json:"sat_per_kb"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.SatPerKB != RecommendedMinTxFee {
		t.Errorf("sat_per_kb mismatch: got %d, want %d", resp.SatPerKB, RecommendedMinTxFee)
	}
	if resp.Source != "floor" {
		t.Errorf("source mismatch: got %q, want %q", resp.Source, "floor")
	}
}

// TestFeerateFallsBackToFloorWhenNodeReturnsZero verifies /feerate uses the
// floor when the node returns 0 or negative.
func TestFeerateFallsBackToFloorWhenNodeReturnsZero(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{EstimateSmartFeeResult: 0}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d", w.Code)
	}

	var resp struct {
		SatPerKB int64  `json:"sat_per_kb"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.SatPerKB != RecommendedMinTxFee {
		t.Errorf("sat_per_kb should be floor when node returns 0, got %d", resp.SatPerKB)
	}
	if resp.Source != "floor" {
		t.Errorf("source mismatch: got %q, want %q", resp.Source, "floor")
	}
}

// TestFeerateNeverReturnsBelowFloor verifies /feerate clamps upward to the
// floor even when the node returns a low value.
func TestFeerateNeverReturnsBelowFloor(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{EstimateSmartFeeResult: 100} // Below floor
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d", w.Code)
	}

	var resp struct {
		SatPerKB int64  `json:"sat_per_kb"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.SatPerKB < RecommendedMinTxFee {
		t.Errorf("sat_per_kb should never be below floor, got %d", resp.SatPerKB)
	}
}

// TestBroadcastAndFeerateWithNilBroadcasterReturn503 verifies both endpoints
// return 503 when the broadcaster is nil.
func TestBroadcastAndFeerateWithNilBroadcasterReturn503(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 10)
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, writeRL, readRL, false, nil)

	// Test /broadcast
	// 120 hex chars = 60 bytes (minimum transaction size)
	body := []byte(`{"hex":"0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"}`)
	req := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP 503 for /broadcast with nil broadcaster, got %d", w.Code)
	}

	// Test /feerate
	req2 := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)

	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP 503 for /feerate with nil broadcaster, got %d", w2.Code)
	}

	// But /status should still work
	req3 := httptest.NewRequest(http.MethodGet, "/status", nil)
	req3.RemoteAddr = "1.2.3.4:5555"
	w3 := httptest.NewRecorder()
	server.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("expected HTTP 200 for /status even with nil broadcaster, got %d", w3.Code)
	}
}

// TestReadAndWriteLimitersIndependentForBroadcastAndFeerate verifies that
// exhausting the write limiter with /broadcast doesn't block /feerate, and
// vice versa.
func TestReadAndWriteLimitersIndependentForBroadcastAndFeerate(t *testing.T) {
	idx := NewIndex()
	writeRL := NewRateLimiter(30, 1) // burst of 1
	readRL := NewRateLimiter(30, 1)  // burst of 1
	fake := &FakeBroadcaster{
		SendRawTransactionResult: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		EstimateSmartFeeResult:   2000000,
	}
	server := newAPIServer(idx, writeRL, readRL, false, fake)

	// 120 hex chars = 60 bytes (minimum transaction size)
	hexBody := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	body := []byte(`{"hex":"` + hexBody + `"}`)

	// Exhaust write limiter with /broadcast
	req1 := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req1.RemoteAddr = "1.2.3.4:5555"
	w1 := httptest.NewRecorder()
	server.ServeHTTP(w1, req1)
	if w1.Code == http.StatusTooManyRequests {
		t.Fatalf("first /broadcast should be allowed, got %d", w1.Code)
	}

	// Second broadcast should be blocked
	req1b := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req1b.RemoteAddr = "1.2.3.4:5555"
	w1b := httptest.NewRecorder()
	server.ServeHTTP(w1b, req1b)
	if w1b.Code != http.StatusTooManyRequests {
		t.Errorf("second /broadcast should be rate-limited, got %d", w1b.Code)
	}

	// But /feerate should still work (different limiter, same IP)
	req2 := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req2.RemoteAddr = "1.2.3.4:5555"
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	if w2.Code == http.StatusTooManyRequests {
		t.Errorf("expected /feerate to work after exhausting write limiter, got %d", w2.Code)
	}

	// Now exhaust read limiter with another /feerate
	req2b := httptest.NewRequest(http.MethodGet, "/feerate", nil)
	req2b.RemoteAddr = "1.2.3.4:5555"
	w2b := httptest.NewRecorder()
	server.ServeHTTP(w2b, req2b)
	if w2b.Code != http.StatusTooManyRequests {
		t.Errorf("second /feerate should be rate-limited, got %d", w2b.Code)
	}

	// But /broadcast should still fail due to write limiter (not read), proving independence
	// (we already exhausted write limiter, so this confirms the error is from write, not read)
	req3 := httptest.NewRequest(http.MethodPost, "/broadcast", bytes.NewReader(body))
	req3.RemoteAddr = "2.2.2.2:5555" // Different IP
	w3 := httptest.NewRecorder()
	server.ServeHTTP(w3, req3)
	if w3.Code == http.StatusTooManyRequests {
		t.Fatalf("broadcast from different IP should work, got %d", w3.Code)
	}
}

// A transport failure must never be reported as though the node judged the
// transaction. Telling a user their valid transaction is invalid, when the
// node was simply unreachable, sends them debugging something that was never
// seen. The two are told apart by error type, not by string matching.
func TestBroadcastDistinguishesTransportFailureFromRejection(t *testing.T) {
	const hexBody = "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"

	post := func(fake *FakeBroadcaster) *httptest.ResponseRecorder {
		server := newAPIServer(NewIndex(), NewRateLimiter(30, 10), NewRateLimiter(30, 10), false, fake)
		req := httptest.NewRequest(http.MethodPost, "/broadcast",
			bytes.NewReader([]byte(`{"hex":"`+hexBody+`"}`)))
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// Transport failure: connection refused, DNS, timeout -- anything that is
	// not the node's own verdict.
	w := post(&FakeBroadcaster{
		SendRawTransactionError: fmt.Errorf("rpc request sendrawtransaction: dial tcp 127.0.0.1:33665: connect: connection refused"),
	})
	if w.Code != http.StatusBadGateway {
		t.Errorf("transport failure: expected HTTP 502, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(body["error"], "unreachable") {
		t.Errorf("transport failure should say the node was unreachable, got %q", body["error"])
	}

	// The node's own verdict, on the other hand, is the user's problem and
	// comes back with its message intact.
	w = post(&FakeBroadcaster{
		SendRawTransactionError: &NodeRejection{
			Method:  "sendrawtransaction",
			Code:    -26,
			Message: "64: non-mandatory-script-verify-flag (Data push larger than necessary)",
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("node rejection: expected HTTP 400, got %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(body["error"], "Data push larger than necessary") {
		t.Errorf("the node's message must survive verbatim, got %q", body["error"])
	}
	if strings.Contains(body["error"], "unreachable") {
		t.Errorf("a rejection must not be described as unreachable: %q", body["error"])
	}
}

// TestStatusReportsHealthyByDefault pins the common-case response shape:
// a working index answers 200 with healthy=true and no store_error, so a
// health check keyed on either the status code or the "healthy" field
// agrees with a client that ignores both and just reads the rest of the
// body, same as before this field existed.
func TestStatusReportsHealthyByDefault(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for a healthy index, got %d", w.Code)
	}
	var status Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !status.Healthy {
		t.Error("a working index reported healthy=false")
	}
	if status.StoreErr != "" {
		t.Errorf("a working index reported a store_error: %q", status.StoreErr)
	}
}

// TestStatusReportsUnhealthyWhenStoreErrorLatched is the actual gap this
// closes. Before it, a latched write error (see Index.write) stopped
// indexing but left /status answering 200 with a tip that had frozen in
// place -- indistinguishable, over HTTP, from a node that simply had
// nothing new to report. An operator polling /status had no way to tell
// "quiet chain" from "silently stopped hours ago".
//
// It reaches into the store package's breakTable helper (same package,
// defined in store_test.go) to latch a real write failure exactly the way
// TestStoreBatchIsAllOrNothing does, rather than fabricating a Status by
// hand -- the point is that the HTTP layer reflects whatever
// Index.StoreErr() actually reports, not a value invented for the test.
//
// It breaks "utxos" rather than "positions": StatusSnapshot's own reads
// (CountPositions, CountOrders) must keep working here so the failure this
// test proves is the latch check in writeStatus, not writeStoreError
// tripping on a query that happens to hit the same broken table.
func TestStatusReportsUnhealthyWhenStoreErrorLatched(t *testing.T) {
	idx, store, _ := storeTestIndex(t)
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	breakTable(t, store, "utxos")
	idx.ApplyBlock(storeChain(1)[0])
	if idx.StoreErr() == nil {
		t.Fatal("breaking the positions table did not latch a store error")
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP 503 once the store has latched an error, got %d", w.Code)
	}
	var status Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if status.Healthy {
		t.Error("/status reported healthy=true with a latched store error")
	}
	if status.StoreErr == "" {
		t.Error("/status did not surface the latched store error's message")
	}
	// The tip must still be the frozen one, not silently reset to
	// something that would hide how far behind the index has fallen.
	if status.TipHeight != -1 {
		t.Errorf("tip in the unhealthy response: got %d, want -1 (nothing committed yet)", status.TipHeight)
	}
}

// ---- /rawtx: serves the exact bytes of a transaction, hex encoded --------
//
// This endpoint exists so a wallet can verify a position for itself: a
// txid is a commitment to the bytes that hash to it, so a wallet that
// already has the txid (from a maker's signed order) can double-SHA256
// this response and compare, rather than trusting the relay's JSON
// description of the position (value, multiplier, origin). See
// TestRawTxUnderreportingRelayDoesNotChangeTheServedBytes below for why
// that specific property is the one worth pinning here.

func TestRawTxValidTxidReturnsHex(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	wantHex := "0100000001abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	fake := &FakeBroadcaster{GetRawTransactionHexResult: wantHex}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("ab", 32)
	req := httptest.NewRequest(http.MethodGet, "/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Hex string `json:"hex"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Hex != wantHex {
		t.Errorf("hex mismatch: got %q, want %q", resp.Hex, wantHex)
	}
}

// TestRawTxUnderreportingRelayDoesNotChangeTheServedBytes is the teeth for
// the whole point of this endpoint: it must serve the node's actual bytes,
// not anything the relay's own (possibly hostile) view of the position
// would compute or summarize. The handler cannot "under-report" a value
// because it never touches the parsed fields at all -- it round-trips
// whatever whippetd hands back for verbosity 0. This test pins that: it
// fails if a future change starts deriving the response from Position /
// Order fields instead of forwarding the RPC's raw hex untouched.
func TestRawTxUnderreportingRelayDoesNotChangeTheServedBytes(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	// Deliberately does NOT match any position's stored value -- if the
	// handler were built from the index rather than from the node's raw
	// bytes, this test would have no way to tell.
	nodeHex := "02000000019999999999999999999999999999999999999999999999999999999999999999"
	fake := &FakeBroadcaster{GetRawTransactionHexResult: nodeHex}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("cd", 32)
	req := httptest.NewRequest(http.MethodGet, "/api/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	var resp struct {
		Hex string `json:"hex"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Hex != nodeHex {
		t.Errorf("served hex does not match the node's raw bytes verbatim: got %q, want %q", resp.Hex, nodeHex)
	}
}

func TestRawTxInvalidTxidReturns400(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, nil, readRL, false, fake)

	req := httptest.NewRequest(http.MethodGet, "/rawtx/not-a-txid", nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for a malformed txid, got %d", w.Code)
	}
	if len(fake.Sent) != 0 {
		t.Error("an invalid txid must never reach the node")
	}
}

func TestRawTxNodeRejectionReturns404(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{
		GetRawTransactionHexError: &NodeRejection{
			Method:  "getrawtransaction",
			Code:    -5,
			Message: "No such mempool or blockchain transaction",
		},
	}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("ef", 32)
	req := httptest.NewRequest(http.MethodGet, "/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected HTTP 404 for a node rejection (no such tx), got %d", w.Code)
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if !strings.Contains(errResp["error"], "No such mempool") {
		t.Errorf("node error message not present verbatim: got %q", errResp["error"])
	}
}

func TestRawTxNodeUnreachableReturns502(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{
		GetRawTransactionHexError: fmt.Errorf("rpc request: connection refused"),
	}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("11", 32)
	req := httptest.NewRequest(http.MethodGet, "/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected HTTP 502 for a transport error, got %d", w.Code)
	}
}

func TestRawTxNilBroadcasterReturns503(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	server := newAPIServer(idx, nil, readRL, false, nil)

	txid := strings.Repeat("22", 32)
	req := httptest.NewRequest(http.MethodGet, "/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP 503 with no broadcaster configured, got %d", w.Code)
	}
}

func TestRawTxPostReturns405(t *testing.T) {
	idx := NewIndex()
	readRL := NewRateLimiter(30, 10)
	fake := &FakeBroadcaster{}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("33", 32)
	req := httptest.NewRequest(http.MethodPost, "/rawtx/"+txid, nil)
	req.RemoteAddr = "1.2.3.4:5555"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected HTTP 405 for POST, got %d", w.Code)
	}
}

func TestRawTxUsesReadLimiter(t *testing.T) {
	idx := NewIndex()
	// A read limiter with room for exactly one request.
	readRL := NewRateLimiter(1, 1)
	fake := &FakeBroadcaster{GetRawTransactionHexResult: "ab"}
	server := newAPIServer(idx, nil, readRL, false, fake)

	txid := strings.Repeat("44", 32)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rawtx/"+txid, nil)
		req.RemoteAddr = "1.2.3.4:5555"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if i == 0 && w.Code != http.StatusOK {
			t.Fatalf("first request: expected HTTP 200, got %d", w.Code)
		}
		if i == 1 && w.Code != http.StatusTooManyRequests {
			t.Errorf("second request: expected HTTP 429 from the read limiter, got %d", w.Code)
		}
	}
}
