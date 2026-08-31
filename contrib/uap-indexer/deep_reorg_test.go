package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// End-to-end cover for the deep-reorg recovery path in syncOnce, driven
// through the real RPCClient against a fake node.
//
// This path is worth testing at this level rather than in isolation
// because the failure it guards against is a hang, not a wrong answer.
// UndoBlock is a no-op for a height whose log has been pruned, and it is
// the undo that moves the tip -- so a reorg walk that keeps calling
// UndoBlock without checking CanUndo never makes progress and never
// terminates. A unit test of UndoBlock alone cannot see that.

// fakeNode serves getblockcount/getblockhash/getblock for a chain the
// test can swap out underneath the indexer.
type fakeNode struct {
	t      *testing.T
	blocks []*RPCBlock // index == height
}

func (f *fakeNode) handler(w http.ResponseWriter, r *http.Request) {
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

	switch req.Method {
	case "getblockcount":
		reply(int64(len(f.blocks) - 1))
	case "getblockhash":
		h := int64(req.Params[0].(float64))
		if h < 0 || h >= int64(len(f.blocks)) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"result": nil,
				"error":  map[string]interface{}{"code": -8, "message": "Block height out of range"},
				"id":     "uap-indexer",
			})
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
	default:
		f.t.Fatalf("fake node: unexpected method %q", req.Method)
	}
}

// chainOf builds `n` blocks tagged so two chains are distinguishable by
// hash, txid and minted pubkey.
func chainOf(tag string, n int, forkFrom []*RPCBlock, forkAt int) []*RPCBlock {
	var out []*RPCBlock
	out = append(out, forkFrom[:forkAt]...)
	for h := forkAt; h < n; h++ {
		out = append(out, makeBlock(fmt.Sprintf("%shash%d", tag, h), int64(h), []RPCTx{{
			TxID: txid(fmt.Sprintf("%stx%d", tag, h)),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, fakePubKey(byte(0x02)), int64(h+1), 1.0)},
		}}))
	}
	return out
}

func TestSyncRecoversFromReorgDeeperThanWindow(t *testing.T) {
	// A generous ceiling: the bug being guarded against is an infinite
	// loop, so the test has to fail rather than hang the suite.
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			panic("syncOnce did not terminate: the reorg walk is not making progress")
		}
	}()
	defer close(done)

	chainA := chainOf("A", 40, nil, 0)
	node := &fakeNode{t: t, blocks: chainA}
	srv := httptest.NewServer(http.HandlerFunc(node.handler))
	defer srv.Close()

	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}

	idx := NewIndex()
	idx.ReorgWindow = 5 // far shallower than the fork below

	blocksApplied := 0
	if err := syncOnce(rpc, idx, 0, &blocksApplied); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if tip, hash := idx.Tip(); tip != 39 || hash != "Ahash39" {
		t.Fatalf("after initial sync: tip %d/%s, want 39/Ahash39", tip, hash)
	}
	if _, ok := mustPosition(t, idx, txid("Atx20"), 0); !ok {
		t.Fatal("chain A position missing after initial sync")
	}

	// Chain B forks at height 10 -- 29 blocks below the tip, far outside
	// the 5-block undo window.
	chainB := chainOf("B", 40, chainA, 10)
	node.blocks = chainB

	if err := syncOnce(rpc, idx, 0, &blocksApplied); err != nil {
		t.Fatalf("sync after deep reorg: %v", err)
	}

	if tip, hash := idx.Tip(); tip != 39 || hash != "Bhash39" {
		t.Errorf("after deep reorg: tip %d/%s, want 39/Bhash39", tip, hash)
	}
	if _, ok := mustPosition(t, idx, txid("Btx20"), 0); !ok {
		t.Error("chain B position missing after rebuild")
	}
	if _, ok := mustPosition(t, idx, txid("Atx20"), 0); ok {
		t.Error("chain A position survived a rebuild; the index is mixing two chains")
	}
	// The shared prefix must be re-indexed too, not merely retained.
	if _, ok := mustPosition(t, idx, txid("Atx5"), 0); !ok {
		t.Error("shared-prefix position missing: the rebuild did not resync from the start")
	}
}

// TestSyncHandlesShallowReorgWithoutRebuilding is the counterpart: a
// reorg inside the window must be rolled back incrementally, not trigger
// the sledgehammer. Without this, a Reset-on-any-reorg implementation
// would pass the test above and quietly resync the whole chain every time
// a single block was replaced.
func TestSyncHandlesShallowReorgWithoutRebuilding(t *testing.T) {
	chainA := chainOf("A", 40, nil, 0)
	node := &fakeNode{t: t, blocks: chainA}
	srv := httptest.NewServer(http.HandlerFunc(node.handler))
	defer srv.Close()

	rpc := &RPCClient{url: srv.URL + "/", http: srv.Client()}

	idx := NewIndex()
	idx.ReorgWindow = 20

	blocksApplied := 0
	if err := syncOnce(rpc, idx, 0, &blocksApplied); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Fork 3 blocks below the tip: comfortably inside the window.
	chainB := chainOf("B", 40, chainA, 37)
	node.blocks = chainB

	// Count only the blocks the reorg itself causes to be applied.
	blocksApplied = 0
	if err := syncOnce(rpc, idx, 0, &blocksApplied); err != nil {
		t.Fatalf("sync after shallow reorg: %v", err)
	}

	if tip, hash := idx.Tip(); tip != 39 || hash != "Bhash39" {
		t.Errorf("after shallow reorg: tip %d/%s, want 39/Bhash39", tip, hash)
	}
	if _, ok := mustPosition(t, idx, txid("Btx38"), 0); !ok {
		t.Error("new-branch position missing after shallow reorg")
	}
	if _, ok := mustPosition(t, idx, txid("Atx38"), 0); ok {
		t.Error("orphaned position survived a shallow reorg")
	}
	// The decisive check: how many blocks were actually applied. A
	// three-block fork needs three. A Reset-and-resync would need all
	// forty, and would still produce the correct final state -- so
	// nothing above this line can tell the two apart.
	//
	// (PrunedBelow cannot serve here, which is how this was found: a
	// rebuild re-prunes to exactly the same value, so asserting on it
	// passed against a mutant that reset on every reorg.)
	if blocksApplied != 3 {
		t.Errorf("applied %d blocks for a 3-block fork; a full rebuild would apply 40. "+
			"The reorg is not being handled incrementally.", blocksApplied)
	}
}
