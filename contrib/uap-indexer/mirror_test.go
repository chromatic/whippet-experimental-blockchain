package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMirrorPeers(t *testing.T) {
	got := parseMirrorPeers(" http://a:1 , http://b:2,,http://c:3 ")
	want := []string{"http://a:1", "http://b:2", "http://c:3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseMirrorPeersEmpty(t *testing.T) {
	if got := parseMirrorPeers(""); len(got) != 0 {
		t.Errorf("expected no peers for empty string, got %v", got)
	}
}

// mirrorOnce pulls a peer's orders and republishes them by revalidating
// against our OWN chain view -- not by trusting the peer's data. This
// confirms both halves of that: a peer's order for a position we also
// know about gets adopted, while one for a position we've never heard of
// (the whole point -- a relay never blindly trusts another relay) is
// silently skipped rather than injected.
func TestMirrorOncePullsAndRevalidates(t *testing.T) {
	idx := NewIndex()
	knownPos := &Position{TxID: "known", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 500000000}
	idx.Positions[positionKey(knownPos.TxID, knownPos.Vout)] = knownPos

	peerOrders := []Order{
		{TxID: "known", Vout: 0, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 100},
		{TxID: "unknown-to-us", Vout: 0, Multiplier: 1000, ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 200},
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerOrders)
	}))
	defer peer.Close()

	mirrorOnce(peer.Client(), idx, peer.URL)

	if _, ok := idx.GetOrder("known", 0); !ok {
		t.Error("expected the order for a position we know about to be adopted")
	}
	if _, ok := idx.GetOrder("unknown-to-us", 0); ok {
		t.Error("expected the order for a position we've never indexed to be rejected, not blindly trusted")
	}
}

func TestMirrorOnceHandlesPeerErrorsGracefully(t *testing.T) {
	idx := NewIndex()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer peer.Close()

	// Must not panic and must leave the index untouched.
	mirrorOnce(peer.Client(), idx, peer.URL)
	if len(idx.Orders) != 0 {
		t.Errorf("expected no orders after a failed peer fetch, got %d", len(idx.Orders))
	}
}
