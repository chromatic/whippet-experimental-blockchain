package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// txindexFakeNode is a minimal JSON-RPC node for probeRawTxLookup: it
// serves getblockcount/getblockhash/getblock like fakeNode in
// deep_reorg_test.go, plus getrawtransaction, whose behavior for a
// CONFIRMED txid is exactly what this test suite needs to control --
// either it answers (a node with working confirmed-tx lookup) or it
// refuses with whippetd's real -txindex error text (a default node that
// does not).
type txindexFakeNode struct {
	t      *testing.T
	blocks []*RPCBlock // index == height

	// rawTxWorks controls what getrawtransaction says about a txid that is
	// NOT the one this fake node treats as "still in the mempool" (there
	// is none here -- every txid handed to this fake is a confirmed one,
	// which is exactly the case probeRawTxLookup needs to distinguish).
	rawTxWorks bool
}

func (f *txindexFakeNode) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatalf("fake node: bad request: %v", err)
	}

	reply := func(result interface{}) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"result": result, "error": nil, "id": "uap-indexer",
		})
	}
	replyError := func(code int, message string) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"result": nil,
			"error":  map[string]interface{}{"code": code, "message": message},
			"id":     "uap-indexer",
		})
	}

	switch req.Method {
	case "getblockcount":
		reply(int64(len(f.blocks) - 1))
	case "getblockhash":
		h := int64(req.Params[0].(float64))
		if h < 0 || h >= int64(len(f.blocks)) {
			replyError(-8, "Block height out of range")
			return
		}
		reply(f.blocks[h].Hash)
	case "getblock":
		want := req.Params[0].(string)
		for _, b := range f.blocks {
			if b.Hash == want {
				reply(b)
				return
			}
		}
		f.t.Fatalf("fake node: getblock for unknown hash %s", want)
	case "getrawtransaction":
		// Every txid this test hands out is confirmed (it comes from a
		// block already in f.blocks), never from a mempool this fake
		// doesn't model -- so rawTxWorks alone decides the answer, exactly
		// mirroring the real node's txindex-gated behavior for a confirmed
		// transaction whose specific output happens not to save it via the
		// unspent-output fallback (or, symmetrically, a node where that
		// fallback -- or -txindex -- works fine).
		if f.rawTxWorks {
			reply("deadbeef")
			return
		}
		// The exact wording whippetd returns for this case (see
		// src/rpc/rawtransaction.cpp) -- probeRawTxLookup's caller
		// (logRawTxLookupStatus, main.go) shows this verbatim to the
		// operator, so the fixture pins the real string rather than a
		// paraphrase.
		replyError(-5, "No such mempool transaction. Use -txindex to enable blockchain transaction queries. Use gettransaction for wallet transactions.")
	default:
		f.t.Fatalf("fake node: unexpected method %q", req.Method)
	}
}

func txindexChain(n int) []*RPCBlock {
	var out []*RPCBlock
	for h := 0; h < n; h++ {
		out = append(out, makeBlock(txid(fmt.Sprintf("txindex-block-%d", h)), int64(h), []RPCTx{{
			TxID: txid(fmt.Sprintf("txindex-coinbase-%d", h)),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, fakePubKey(byte(0x03)), int64(h+1), 1.0)},
		}}))
	}
	return out
}

func TestProbeRawTxLookupSucceedsWhenNodeAnswers(t *testing.T) {
	node := &txindexFakeNode{t: t, blocks: txindexChain(3), rawTxWorks: true}
	srv := httptest.NewServer(http.HandlerFunc(node.handler))
	defer srv.Close()
	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}

	checked, ok, detail := probeRawTxLookup(rpc)
	if !checked {
		t.Fatalf("expected a definitive answer, got unchecked (detail: %s)", detail)
	}
	if !ok {
		t.Errorf("expected ok=true when the node answers getrawtransaction, got false (detail: %s)", detail)
	}
	if detail != "" {
		t.Errorf("expected no detail on success, got %q", detail)
	}
}

// This is the teeth: a node that refuses confirmed-transaction lookup
// (the default whippetd behavior without -txindex) must be reported as
// broken, not silently treated as fine.
func TestProbeRawTxLookupReportsBrokenNode(t *testing.T) {
	node := &txindexFakeNode{t: t, blocks: txindexChain(3), rawTxWorks: false}
	srv := httptest.NewServer(http.HandlerFunc(node.handler))
	defer srv.Close()
	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}

	checked, ok, detail := probeRawTxLookup(rpc)
	if !checked {
		t.Fatalf("expected a definitive answer, got unchecked (detail: %s)", detail)
	}
	if ok {
		t.Error("expected ok=false when the node refuses getrawtransaction for a confirmed tx")
	}
	if detail == "" {
		t.Error("expected the node's own rejection message to be reported as detail")
	}
}

// A chain with no blocks past genesis has nothing to probe with -- this
// must come back unchecked, not report either a false "broken" or a false
// "ok".
func TestProbeRawTxLookupUncheckedOnGenesisOnlyChain(t *testing.T) {
	node := &txindexFakeNode{t: t, blocks: txindexChain(1), rawTxWorks: true}
	srv := httptest.NewServer(http.HandlerFunc(node.handler))
	defer srv.Close()
	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}

	checked, _, detail := probeRawTxLookup(rpc)
	if checked {
		t.Errorf("expected unchecked on a genesis-only chain, got a definitive answer (detail: %s)", detail)
	}
}

// SetRawTxLookupStatus/StatusSnapshot: the plumbing between the probe and
// what an operator watching /status actually sees.
func TestStatusReportsRawTxLookupBroken(t *testing.T) {
	idx := NewIndex()
	idx.SetRawTxLookupStatus(false, "No such mempool transaction. Use -txindex to enable blockchain transaction queries.")

	s, err := idx.StatusSnapshot()
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	if s.RawTxLookup != "broken" {
		t.Errorf("expected rawtx_lookup=broken, got %q", s.RawTxLookup)
	}
	if s.RawTxLookupDetail == "" {
		t.Error("expected a detail message when rawtx_lookup is broken")
	}
	// A store error and a broken rawtx lookup are unrelated failure modes:
	// this relay keeps indexing fine either way, so Healthy must stay true.
	if !s.Healthy {
		t.Error("a broken rawtx lookup must not flip Healthy false -- indexing itself is unaffected")
	}
}

func TestStatusReportsRawTxLookupOK(t *testing.T) {
	idx := NewIndex()
	idx.SetRawTxLookupStatus(true, "")

	s, err := idx.StatusSnapshot()
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	if s.RawTxLookup != "ok" {
		t.Errorf("expected rawtx_lookup=ok, got %q", s.RawTxLookup)
	}
	if s.RawTxLookupDetail != "" {
		t.Errorf("expected no detail when rawtx_lookup is ok, got %q", s.RawTxLookupDetail)
	}
}

func TestStatusReportsRawTxLookupUncheckedByDefault(t *testing.T) {
	idx := NewIndex()

	s, err := idx.StatusSnapshot()
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	if s.RawTxLookup != "unchecked" {
		t.Errorf("a fresh index that has never probed must report unchecked, got %q", s.RawTxLookup)
	}
}
