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
				// A single tx fetch failure should not fail the whole update.
				// Log it (in real code), but continue with the rest of the mempool.
				// For now, skip this tx silently.
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
