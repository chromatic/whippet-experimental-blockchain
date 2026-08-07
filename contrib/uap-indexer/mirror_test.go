package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
	seedPosition(t, idx, knownPos)

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

	if _, ok := mustGetOrder(t, idx, "known", 0); !ok {
		t.Error("expected the order for a position we know about to be adopted")
	}
	if _, ok := mustGetOrder(t, idx, "unknown-to-us", 0); ok {
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
	if orderCount(t, idx) != 0 {
		t.Errorf("expected no orders after a failed peer fetch, got %d", orderCount(t, idx))
	}
}

// TestCancelledOrderIsNotResurrectedByAMirrorPull covers the interaction
// between cancellation and mirroring.
//
// Cancelling removes an order from this relay, but a peer that mirrored it
// earlier still has it and will keep offering it. On the next pull we
// revalidate it -- the position is real, unspent and matches -- so it
// passes every check and comes straight back. The maker's cancel is
// undone by their own relay, and repeating it does not help: it will be
// undone again on the next tick.
//
// This is relay hygiene, not a guarantee, and it cannot be more than that:
// the peer still holds a valid signed fragment and may serve it to its own
// users, which is exactly why the API docs say the only real way to
// withdraw an order is to spend the position. What this pins down is the
// narrower promise -- that *this* relay does not resurrect what its own
// user withdrew.
func TestCancelledOrderIsNotResurrectedByAMirrorPull(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})

	scriptSig := signedPush(t)
	order := Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
	}
	if err := idx.PublishOrder(&order); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	cancelSig := signCancel(t, priv, "known", 0, scriptSig, order.CancelNonce)
	if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	// A peer that mirrored the order before it was cancelled still has it.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Order{order})
	}))
	defer peer.Close()

	mirrorOnce(peer.Client(), idx, peer.URL)

	if _, ok := mustGetOrder(t, idx, "known", 0); ok {
		t.Error("a cancelled order was pulled back in from a peer; cancelling is not durable against mirroring")
	}
}

// A cancel must not stop the maker from deliberately offering the position
// again. The tombstone is aimed at peers re-pushing a withdrawn order, not
// at the maker's own relay: a local publish is an explicit act by whoever
// holds the signed fragment, and refusing it would make cancel a one-way
// door for the exact person it is meant to serve.
func TestCancelDoesNotBlockTheMakerRepublishing(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})

	scriptSig := signedPush(t)
	mk := func() *Order {
		return &Order{
			TxID: "known", Vout: 0, Multiplier: 1000,
			ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
		}
	}
	firstOrder := mk()
	if err := idx.PublishOrder(firstOrder); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	cancelSig := signCancel(t, priv, "known", 0, scriptSig, firstOrder.CancelNonce)
	if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if err := idx.PublishOrder(mk()); err != nil {
		t.Fatalf("republishing after a cancel: %v", err)
	}
	if _, ok := mustGetOrder(t, idx, "known", 0); !ok {
		t.Error("the maker's republished order is not being served")
	}

	// ...and having republished, a peer offering the same order is no
	// longer being resurrected against the maker's wishes -- it is simply
	// the order they are currently making, so the record of the withdrawal
	// must be gone.
	//
	// Asserted on the tombstone directly rather than through a mirror
	// pull: the order is already present from the local publish, so a
	// blocked pull and an accepted one leave identical state and the
	// difference is invisible from outside.
	n, err := idx.store.CountTombstones()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d tombstone(s) left after the maker republished; a peer offering "+
			"the same fragment would be refused even though it is the live order", n)
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Order{*mk()})
	}))
	defer peer.Close()
	mirrorOnce(peer.Client(), idx, peer.URL)
	if _, ok := mustGetOrder(t, idx, "known", 0); !ok {
		t.Error("a mirror pull removed the maker's own live order")
	}
}

// A tombstone withdraws one signed fragment, not the outpoint. If the
// maker signs a fresh order -- a different price, say -- a peer relaying
// that is doing its job, and blocking it would mean one cancel silently
// froze the position out of the mirror set for good.
func TestTombstoneOnlyBlocksTheFragmentThatWasWithdrawn(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})

	withdrawn := signedPush(t)
	withdrawnOrder := &Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: withdrawn, PaymentScript: "00", PaymentValue: 100,
	}
	if err := idx.PublishOrder(withdrawnOrder); err != nil {
		t.Fatalf("PublishOrder: %v", err)
	}
	cancelSig := signCancel(t, priv, "known", 0, withdrawn, withdrawnOrder.CancelNonce)
	if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	// A different signature, i.e. a different signed offer.
	der := []byte{0x30, 0x06, 0x02, 0x01, 0x02, 0x02, 0x01, 0x02}
	sig := append(der, sighashOrderType)
	reoffer := hex.EncodeToString(append([]byte{byte(len(sig))}, sig...))
	if reoffer == withdrawn {
		t.Fatal("test setup: the two scriptSigs must differ")
	}

	if err := idx.AdoptMirroredOrder(&Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: reoffer, PaymentScript: "00", PaymentValue: 250,
	}); err != nil {
		t.Fatalf("a differently-signed order was blocked by an unrelated tombstone: %v", err)
	}
	o, ok := mustGetOrder(t, idx, "known", 0)
	if !ok {
		t.Fatal("the re-offered order is not being served")
	}
	if o.ScriptSig != reoffer {
		t.Errorf("wrong order served: got scriptSig %s, want %s", o.ScriptSig, reoffer)
	}
}

// Cancelling has to outlive a restart. A tombstone held only in memory
// would let the first mirror poll after every restart undo every cancel
// the relay had ever accepted.
func TestTombstonesSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	scriptSig := signedPush(t)
	published := &Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
	}
	if err := idx.PublishOrder(published); err != nil {
		t.Fatal(err)
	}
	cancelSig := signCancel(t, priv, "known", 0, scriptSig, published.CancelNonce)
	if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := reopened.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}

	err = restarted.AdoptMirroredOrder(&Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
	})
	if err == nil {
		t.Error("after a restart, a peer resurrected an order this relay had cancelled")
	}
}

// Repeated publish/cancel cycles on one position must leave one tombstone,
// not a row per cycle. Otherwise a maker toggling their own order -- which
// costs nothing but a rate-limited request -- grows the database forever.
func TestRepeatedCancelsLeaveOneTombstone(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	scriptSig := signedPush(t)
	for i := 0; i < 5; i++ {
		o := &Order{
			TxID: "known", Vout: 0, Multiplier: 1000,
			ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
		}
		if err := idx.PublishOrder(o); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		cancelSig := signCancel(t, priv, "known", 0, scriptSig, o.CancelNonce)
		if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
	}
	n, err := idx.store.CountTombstones()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d tombstones after 5 publish/cancel cycles on one outpoint, want 1", n)
	}
}

// A peer is untrusted input, not just an unreliable one. These pin the
// cases where a peer sends something a naive client would choke on or,
// worse, believe.
func TestMirrorRejectsPeerOrdersThatFailOurOwnChecks(t *testing.T) {
	idx := NewIndex()
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 500000000,
	})
	seedPosition(t, idx, &Position{
		TxID: "spent", Vout: 0, PubKey: "02aa", Multiplier: 1000, Value: 1,
		Spent: true, SpentTxID: "gone", SpentHeight: 9,
	})

	cases := []struct {
		name  string
		order Order
	}{
		{"multiplier the peer got wrong", Order{
			TxID: "known", Vout: 0, Multiplier: 4242,
			ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 100}},
		{"position we have already seen spent", Order{
			TxID: "spent", Vout: 0, Multiplier: 1000,
			ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 100}},
		{"scriptSig that is not a signature push", Order{
			TxID: "known", Vout: 0, Multiplier: 1000,
			ScriptSig: "deadbeef", PaymentScript: "00", PaymentValue: 100}},
		{"payment value of zero", Order{
			TxID: "known", Vout: 0, Multiplier: 1000,
			ScriptSig: signedPush(t), PaymentScript: "00", PaymentValue: 0}},
		{"payment script that is not hex", Order{
			TxID: "known", Vout: 0, Multiplier: 1000,
			ScriptSig: signedPush(t), PaymentScript: "zz", PaymentValue: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.order
			if err := idx.AdoptMirroredOrder(&o); err == nil {
				t.Error("adopted an order that our own validation should have rejected")
			}
		})
	}
	if n := orderCount(t, idx); n != 0 {
		t.Errorf("%d orders adopted from a peer sending only invalid ones, want 0", n)
	}
}

// A peer serving something that is not a list of orders must be skipped,
// not allowed to take the poller down. MirrorPeers runs in its own
// goroutine for the life of the process; a panic here kills the mirror
// permanently, and the operator's only symptom is orders quietly ceasing
// to arrive.
func TestMirrorSurvivesMalformedPeerResponses(t *testing.T) {
	bodies := map[string]string{
		"not json":                "<html>502 Bad Gateway</html>",
		"json but not a list":     `{"error":"nope"}`,
		"list of the wrong shape": `[1,2,3]`,
		"empty body":              "",
		"null":                    "null",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			idx := NewIndex()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer peer.Close()

			mirrorOnce(peer.Client(), idx, peer.URL) // must not panic
			if n := orderCount(t, idx); n != 0 {
				t.Errorf("%d orders adopted from a malformed response, want 0", n)
			}
		})
	}
}

// A peer that streams without end must not be able to exhaust our memory.
// mirrorOnce caps the body it will read; without that cap a single hostile
// or broken peer takes the process down, and -mirror is documented as safe
// to point at relays you do not operate.
func TestMirrorCapsTheResponseBodyItWillRead(t *testing.T) {
	var served int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for i := 0; i < 64; i++ { // 64 MB, well past the 16 MB cap
			n, err := w.Write(chunk)
			atomic.AddInt64(&served, int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer peer.Close()

	idx := NewIndex()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mirrorOnce(peer.Client(), idx, peer.URL)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("mirrorOnce did not return: it is reading an unbounded body")
	}

	if got := atomic.LoadInt64(&served); got > 48<<20 {
		t.Errorf("read %d bytes from the peer; the cap is not being applied", got)
	}
	if n := orderCount(t, idx); n != 0 {
		t.Errorf("%d orders adopted from a garbage stream, want 0", n)
	}
}

// A deep reorg discards the whole index and rebuilds it from the chain.
// Orders go with it -- they are not chain-derived, and peers re-mirror
// them, which is the intended recovery. Tombstones must not: forgetting
// them would mean that same re-mirroring restored exactly the orders their
// makers had withdrawn, and one deep reorg would undo every cancellation
// the relay had ever accepted.
func TestTombstonesSurviveADeepReorgRebuild(t *testing.T) {
	idx := NewIndex()
	priv, pubKeyHex := testMakerKey(t)
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	scriptSig := signedPush(t)
	order := Order{
		TxID: "known", Vout: 0, Multiplier: 1000,
		ScriptSig: scriptSig, PaymentScript: "00", PaymentValue: 100,
	}
	if err := idx.PublishOrder(&order); err != nil {
		t.Fatal(err)
	}
	cancelSig := signCancel(t, priv, "known", 0, scriptSig, order.CancelNonce)
	if err := idx.CancelOrder("known", 0, cancelSig); err != nil {
		t.Fatal(err)
	}

	idx.Reset()
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("store error after Reset: %v", err)
	}

	// The rebuild re-indexes the chain, so the position comes back.
	seedPosition(t, idx, &Position{
		TxID: "known", Vout: 0, PubKey: pubKeyHex, Multiplier: 1000, Value: 500000000,
	})
	o := order
	if err := idx.AdoptMirroredOrder(&o); err == nil {
		t.Error("a deep-reorg rebuild forgot a cancellation; the next mirror poll " +
			"restored an order the maker had withdrawn")
	}
}
