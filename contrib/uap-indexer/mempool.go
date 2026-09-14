package main

import (
	"sync"
)

// PendingSpendSet is a thread-safe map of spent outpoint -> filling txid,
// covering only outpoints that are KNOWN POSITIONS. It allows the relay to
// detect when a position is being spent by an unconfirmed transaction and
// refuse a second fill attempt before the node rejects it.
//
// Outpoints are keyed as "txid:vout" (see positionKey).
type PendingSpendSet struct {
	mu     sync.RWMutex
	spends map[string]string // outpoint key -> txid of the spending transaction
}

// NewPendingSpendSet returns a new, empty pending spend set.
func NewPendingSpendSet() *PendingSpendSet {
	return &PendingSpendSet{
		spends: make(map[string]string),
	}
}

// Set marks an outpoint as being spent by the given txid.
func (ps *PendingSpendSet) Set(outpoint, txid string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.spends[outpoint] = txid
}

// Get returns the txid spending the outpoint, or empty if not pending.
func (ps *PendingSpendSet) Get(outpoint string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.spends[outpoint]
}

// Delete removes an outpoint from the pending set.
func (ps *PendingSpendSet) Delete(outpoint string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.spends, outpoint)
}

// Clear removes all pending spends.
func (ps *PendingSpendSet) Clear() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.spends = make(map[string]string)
}

// SetPendingSpend marks a position as being spent by an unconfirmed transaction.
func (idx *Index) SetPendingSpend(outpoint, txid string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.pendingSpends.Set(outpoint, txid)
}

// ClearPendingSpend removes a position from the pending spend set.
func (idx *Index) ClearPendingSpend(outpoint string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.pendingSpends.Delete(outpoint)
}

// IsPendingSpend checks if a position is in the pending spend set.
// Callers must NOT hold idx.mu (this method acquires it).
func (idx *Index) IsPendingSpend(outpoint string) (string, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if txid := idx.pendingSpends.Get(outpoint); txid != "" {
		return txid, true
	}
	return "", false
}

// ClearAllPendingSpends clears the entire pending spend set.
// Used when rebuilding the set after polling the mempool.
func (idx *Index) ClearAllPendingSpends() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.pendingSpends.Clear()
}

// UpdatePendingSpends polls the node's mempool and rebuilds the pending spend
// set to reflect only the unconfirmed transactions that are spending known
// positions. If the RPC call fails, the error is returned and the pending set
// is LEFT UNCHANGED to avoid silently re-opening orders that are being filled.
//
// This is called on each poll tick from main() and must be safe to call
// concurrently with reads to the pending set.
func (idx *Index) UpdatePendingSpends(rpc *RPCClient) error {
	// Get the current mempool txids.
	txids, err := rpc.GetRawMempool()
	if err != nil {
		// RPC failure: do NOT clear the pending set. Silently wiping it
		// would re-open orders that are being filled. Better to report
		// the error and keep the stale pending set than to silently accept
		// bad fills.
		return err
	}

	// Cache to avoid fetching the same tx twice in one poll.
	txCache := make(map[string]*RPCTx)

	// Build a new pending set.
	newPending := NewPendingSpendSet()

	// What the current set believes, indexed by spending txid, so a
	// transaction we fail to fetch below can keep the entries it already
	// has instead of losing them to the rebuild.
	carryForward := idx.pendingSpendsByTxid()

	idx.mu.RLock()
	store := idx.store
	idx.mu.RUnlock()

	// Known positions are looked up one outpoint at a time, on the primary
	// key, rather than by reading the position table and scanning it.
	//
	// The two sides of that choice do not grow together: the mempool holds a
	// few seconds of traffic, while positions grow with the chain forever.
	// Loading them all on every poll tick would reintroduce precisely what
	// the store exists to avoid -- see the note at the top of store.go, where
	// keeping this state in Go maps made RSS grow without bound alongside the
	// chain, to hold what the database on disk already had. Here that cost
	// would land on a 5-second timer, in the steady state, forever.
	//
	// A point lookup per mempool input is O(mempool): the size of the thing
	// actually being examined.
	seen := make(map[string]bool)
	isKnownUnspentPosition := func(outpoint string) bool {
		if v, ok := seen[outpoint]; ok {
			return v
		}
		pos, found, err := store.Position(outpoint)
		v := err == nil && found && !pos.Spent
		seen[outpoint] = v
		return v
	}

	// For each mempool txid, fetch it and check if any of its inputs are known positions.
	for _, txid := range txids {
		var tx *RPCTx
		if cached, ok := txCache[txid]; ok {
			tx = cached
		} else {
			var err error
			tx, err = rpc.GetRawTransaction(txid)
			if err != nil {
				// A single tx fetch failure must not fail the whole update --
				// but this is a wholesale rebuild, so simply skipping the
				// transaction would DROP whatever it already had pending and
				// re-open an order that is in fact being filled. That is the
				// exact outcome the RPC-failure branch above refuses to
				// accept; a per-transaction hiccup deserves the same answer.
				//
				// So carry forward what the current set already says about
				// this txid. It is still in the mempool -- getrawmempool just
				// listed it -- and only our view of its inputs is missing.
				for _, outpoint := range carryForward[txid] {
					newPending.Set(outpoint, txid)
				}
				continue
			}
			txCache[txid] = tx
		}

		// Check each input to see if it's a known position.
		for _, vin := range tx.Vin {
			// Skip coinbase inputs (they have no txid).
			if vin.Coinbase != "" {
				continue
			}
			outpoint := positionKey(vin.TxID, vin.Vout)
			if isKnownUnspentPosition(outpoint) {
				newPending.Set(outpoint, txid)
			}
		}
	}

	// Atomically replace the pending set with the new one.
	idx.mu.Lock()
	idx.pendingSpends = newPending
	idx.mu.Unlock()

	return nil
}

// Keys returns the pending outpoints. Used to hand the exclusion list to the
// order query, so paging is computed over the rows that will be served.
func (ps *PendingSpendSet) Keys() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]string, 0, len(ps.spends))
	for k := range ps.spends {
		out = append(out, k)
	}
	return out
}

// pendingSpendsByTxid groups the current pending set by spending txid.
// Used by UpdatePendingSpends to survive a per-transaction fetch failure
// without dropping what it already knew.
func (idx *Index) pendingSpendsByTxid() map[string][]string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make(map[string][]string)
	for _, outpoint := range idx.pendingSpends.Keys() {
		if txid := idx.pendingSpends.Get(outpoint); txid != "" {
			out[txid] = append(out[txid], outpoint)
		}
	}
	return out
}

// NotePendingSpends adds the known positions spent by `txid` to the pending
// set, without disturbing anything else in it.
//
// UpdatePendingSpends already discovers these on its next poll. This exists
// because "next poll" is up to a whole poll interval away, and the single
// most likely moment for two takers to collide is the few seconds right
// after one of them broadcasts: that is when the order was most recently
// visible on the book, and when the second taker is most likely to be
// looking at it. The relay performed that broadcast itself, so it already
// knows -- leaving the book stale until a timer catches up is throwing away
// knowledge it has in hand.
//
// Additive, never a rebuild: this runs on the broadcast path, concurrently
// with the poller, and must not clobber what that has found.
func (idx *Index) NotePendingSpends(rpc *RPCClient, txid string) error {
	tx, err := rpc.GetRawTransaction(txid)
	if err != nil {
		return err
	}

	idx.mu.RLock()
	store := idx.store
	idx.mu.RUnlock()
	if store == nil {
		return nil
	}

	for _, vin := range tx.Vin {
		if vin.Coinbase != "" {
			continue
		}
		outpoint := positionKey(vin.TxID, vin.Vout)
		pos, found, err := store.Position(outpoint)
		if err != nil || !found || pos.Spent {
			continue
		}
		idx.SetPendingSpend(outpoint, txid)
	}
	return nil
}

// SetBroadcastHook installs the callback run after every successful
// broadcast. Called once at startup, before the HTTP server is serving.
func (idx *Index) SetBroadcastHook(fn func(txid string)) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.broadcastHook = fn
}

// noteBroadcast runs the broadcast hook, if one is installed. Called by the
// HTTP layer, which has no business knowing what the hook does.
func (idx *Index) noteBroadcast(txid string) {
	idx.mu.RLock()
	fn := idx.broadcastHook
	idx.mu.RUnlock()
	if fn != nil {
		fn(txid)
	}
}
