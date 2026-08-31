package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"sync"
)

// Position is one UAP mint or transfer output the indexer has observed.
type Position struct {
	TxID        string `json:"txid"`
	Vout        uint32 `json:"vout"`
	PubKey      string `json:"pubkey"` // hex-encoded recipient pubkey
	Multiplier  int64  `json:"multiplier"`
	Value       int64  `json:"value"` // satoshis
	IsMint      bool   `json:"is_mint"`
	Height      int64  `json:"height"`
	Spent       bool   `json:"spent"`
	SpentTxID   string `json:"spent_txid,omitempty"`
	SpentHeight int64  `json:"spent_height,omitempty"`
	// Origin is the lineage identity: hex SHA256 of the originating mint's
	// outpoint, the same 32 bytes every transfer covenant in the lineage
	// carries in its script. Read straight off a transfer's script; derived
	// from its own outpoint for a mint. Never empty for a parsed position --
	// there is no orphan case any more, because identity no longer has to be
	// reconstructed by following the spend graph.
	Origin string `json:"origin,omitempty"`

	// Script is this output's own scriptPubKey, hex-encoded.
	//
	// A wallet spending this position must sign with this exact script as
	// the scriptCode, so it is kept verbatim rather than reassembled from
	// the parsed fields -- the only safe version is the one the chain has.
	// The salt that once made this field indispensable is gone, but the
	// reasoning has not changed: a reassembled script that differs from the
	// real one by a single byte produces a signature over the wrong
	// scriptCode, and the spend simply fails to verify.
	Script string `json:"script_hex,omitempty"`

	// Metadata is the token's ticker and metadata hash, declared by an OP_RETURN in
	// the mint's own transaction. Set only on mints: it belongs to the
	// lineage, and a transfer must not be able to rewrite it (see
	// ApplyBlock). Stored on the position rather than in a side table so
	// UndoBlock removes it for free when the mint is rolled back.
	Metadata *TokenMetadata `json:"metadata,omitempty"`
}

func positionKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}

// UTXO is one ordinary pay-to-pubkey-hash output.
type UTXO struct {
	TxID    string `json:"txid"`
	Vout    uint32 `json:"vout"`
	Hash160 string `json:"hash160"` // hex
	Value   int64  `json:"value"`   // satoshis
	Height  int64  `json:"height"`
}

// heightChange records everything a single block did to the index, so a
// reorg can undo exactly that block without rescanning from genesis.
type heightChange struct {
	Created      []string `json:"created"`
	SpentKeys    []string `json:"spent_keys"`
	UTXOsCreated []string `json:"utxos_created"`
	UTXOsSpent   []*UTXO  `json:"utxos_spent"`

	// OrdersPruned are the standing orders this block removed because the
	// position they sell was spent. Kept whole rather than by key: the row
	// is gone by the time undo runs, and the peer relays that would
	// otherwise be the only way to get it back are optional.
	OrdersPruned []*Order `json:"orders_pruned,omitempty"`
}

// Index is the indexer's state. Almost all of it lives in the Store; what
// remains here is the handful of O(1) facts that every write touches and
// every /status serves, kept in memory to avoid a query for them.
//
// The write lock serialises block application against itself and against
// order publishing. Reads do not take it at all: they go to the database,
// which handles concurrency itself, so an API request can no longer stall
// block ingestion.
type Index struct {
	mu sync.RWMutex

	store    *Store
	storeErr error

	TipHeight int64
	TipHash   string

	// ReorgWindow is how many blocks below the tip keep their undo log.
	// Config rather than data, so it is not persisted: the operator's
	// current setting should win over whatever the last run used.
	ReorgWindow int64

	// PrunedBelow is the lowest height whose undo log is still retained.
	// This IS data -- it records what was discarded -- so it persists.
	// Resetting it on load would make the reorg walk believe it could roll
	// back into heights whose logs were dropped before the restart.
	PrunedBelow int64

	// lastCancelNonce is the most recently issued Order.CancelNonce (see
	// orders.go's publishOrder and cancel_auth.go). It exists only to tell
	// one publish of an order apart from the next, for the lifetime of
	// this process, so a cancel signature captured for one publish cannot
	// authorize cancelling a later, identically-signed republish of the
	// same order. It is not persisted: nonces are never compared across a
	// restart, only for exact equality against whatever the currently
	// stored order carries, so resetting to zero on load costs nothing.
	// Guarded by mu like every other write here.
	lastCancelNonce int64

	// pendingSpends maps known position outpoints to the txids of
	// unconfirmed transactions spending them. Rebuilt on each poll of the
	// mempool and guarded by mu alongside every other write.
	pendingSpends *PendingSpendSet
}

// satMul multiplies, pinning to MaxInt64 on overflow rather than wrapping.
//
// Two hazards here, both reachable by anyone who can mint: a multiplier of
// zero is a valid position (consensus encodes it as OP_0), so the overflow
// guard must never divide by it -- it used to, and GET /tokens answered a
// remote "integer divide by zero" panic for the cost of one mint. And a
// single value*multiplier fitting in an int64 does not mean their sum does.
// Saturating keeps one hostile lineage from taking down the endpoint for
// every other token; the figure is a display aggregate, not something
// consensus depends on.
func satMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// satAdd adds, pinning to MaxInt64 on overflow.
func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// DefaultReorgWindow is how many blocks of undo history the index keeps
// by default. At Whippet's 6-second target this is a little over 24
// hours, which is far beyond any reorg a functioning chain produces;
// anything deeper is a chain split serious enough that rebuilding the
// index is the right answer anyway.
const DefaultReorgWindow = 15000

func newIndex(store *Store) *Index {
	return &Index{
		store:         store,
		TipHeight:     -1,
		ReorgWindow:   DefaultReorgWindow,
		pendingSpends: NewPendingSpendSet(),
	}
}

// NewIndex returns an index backed by a private in-memory database.
//
// Production goes through OpenStore(path).LoadIndex(); this is for tests
// and for anything that wants an index without a file behind it. It panics
// rather than returning an error because the only way opening an in-memory
// SQLite with a fixed schema fails is a process too broken to continue --
// and there is nothing a caller could usefully do about it.
func NewIndex() *Index {
	store, err := OpenMemoryStore()
	if err != nil {
		panic("uap-indexer: cannot open an in-memory state database: " + err.Error())
	}
	return newIndex(store)
}

// StoreErr reports the latched store failure, if any.
//
// It gates every later write rather than merely being reported, because
// carrying on is silently unrecoverable: if block N's write fails and
// N+1's succeeds, the stored tip claims N+1 while N's rows were never
// written, and a restart resumes past a hole nothing can detect. Refusing
// all further work leaves the database coherent at the last block that
// fully committed, which a restart can simply continue from.
func (idx *Index) StoreErr() error {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.storeErr
}

// write runs fn inside one transaction, latching any failure. The callback
// must not mutate idx's in-memory fields: it returns the new values, which
// are applied only once the transaction has committed, so a failed block
// leaves the tip exactly where it was.
func (idx *Index) write(fn func(t *storeTx) (storeMeta, error)) {
	if idx.storeErr != nil {
		return
	}
	t, err := idx.store.begin()
	if err != nil {
		idx.storeErr = err
		return
	}
	defer t.rollback()

	meta, err := fn(t)
	if err != nil {
		idx.storeErr = err
		return
	}
	if err := t.putMeta(meta); err != nil {
		idx.storeErr = err
		return
	}
	if err := t.commit(); err != nil {
		idx.storeErr = err
		return
	}
	idx.TipHeight = meta.tipHeight
	idx.TipHash = meta.tipHash
	idx.PrunedBelow = meta.prunedBelow
}

// meta snapshots the in-memory fields as the starting point for a write.
// Callers must hold idx.mu.
func (idx *Index) meta() storeMeta {
	return storeMeta{
		tipHeight:   idx.TipHeight,
		tipHash:     idx.TipHash,
		prunedBelow: idx.PrunedBelow,
	}
}

// CanUndo reports whether the index still holds the undo log for a height,
// i.e. whether UndoBlock on it would actually reverse anything.
//
// Callers walking a reorg backwards must check this. UndoBlock on a pruned
// height is a no-op, and a caller that assumed otherwise would carry on
// believing it had rolled back onto the new chain while still holding
// state from the orphaned one.
func (idx *Index) CanUndo(height int64) bool {
	ok, err := idx.store.CanUndo(height)
	return err == nil && ok
}

// IsPruned reports whether a height falls below the retained undo window,
// i.e. whether the index once had an opinion about it and has since
// discarded it.
//
// This is the distinction HashAtHeight cannot make on its own: it reports
// "no opinion" both for a height never indexed and for one whose record
// was pruned. Those demand opposite responses from the reorg walk -- stop,
// versus rebuild -- so the caller needs to tell them apart.
func (idx *Index) IsPruned(height int64) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return height < idx.PrunedBelow
}

// Reset discards everything, returning the index to its freshly created
// state. Used when a reorg runs deeper than the retained window, where
// rebuilding from scratch is the only way back to a coherent view.
func (idx *Index) Reset() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.storeErr != nil {
		return
	}
	if err := idx.store.truncate(); err != nil {
		idx.storeErr = err
		return
	}
	idx.TipHeight = -1
	idx.TipHash = ""
	idx.PrunedBelow = 0
}

// HashAtHeight returns what the indexer currently believes the block hash
// at the given height is, and whether it has an opinion at all.
func (idx *Index) HashAtHeight(height int64) (string, bool) {
	hash, ok, err := idx.store.HashAtHeight(height)
	if err != nil {
		return "", false
	}
	return hash, ok
}

// Tip returns the indexer's current tip height and hash.
func (idx *Index) Tip() (int64, string) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.TipHeight, idx.TipHash
}

// ApplyBlock indexes one connected block: newly created UAP positions and
// P2PKH UTXOs, and previously-indexed positions and UTXOs spent by this
// block's inputs.
func (idx *Index) ApplyBlock(block *RPCBlock) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.write(func(t *storeTx) (storeMeta, error) {
		return idx.applyBlock(t, block)
	})
}

func (idx *Index) applyBlock(t *storeTx, block *RPCBlock) (storeMeta, error) {
	meta := idx.meta()
	change := &heightChange{}

	for _, tx := range block.Tx {
		for _, vin := range tx.Vin {
			if vin.Coinbase != "" {
				continue
			}
			k := positionKey(vin.TxID, vin.Vout)

			pos, err := t.position(k)
			if err != nil {
				return meta, err
			}
			if pos != nil && !pos.Spent {
				pos.Spent = true
				pos.SpentTxID = tx.TxID
				pos.SpentHeight = block.Height
				if err := t.putPosition(pos); err != nil {
					return meta, err
				}
				if err := t.removeFromLineage(pos); err != nil {
					return meta, err
				}
				change.SpentKeys = append(change.SpentKeys, k)

				// Any standing order for this position is now stale
				// (filled, or the maker moved it some other way). The
				// whole order goes into the undo log, because if this
				// block is orphaned the fill did not happen and the maker
				// is still offering it -- see undoBlock.
				order, had, err := t.order(k)
				if err != nil {
					return meta, err
				}
				if had {
					if err := t.delOrder(k); err != nil {
						return meta, err
					}
					change.OrdersPruned = append(change.OrdersPruned, &order)
				}

			}

			utxo, err := t.utxo(k)
			if err != nil {
				return meta, err
			}
			if utxo != nil {
				// Store the whole record: undo has to put it back, and by
				// then the row is gone.
				change.UTXOsSpent = append(change.UTXOsSpent, utxo)
				if err := t.delUTXO(k); err != nil {
					return meta, err
				}
			}
		}

		// Determine the lineage origin for outputs created by this
		// A mint may declare its token's ticker/metadata hash in an
		// OP_RETURN output of the same transaction. Found once per
		// transaction; applied only to mints below, so a transfer carrying
		// its own OP_RETURN cannot rename a token for the holders who did
		// not sign it.
		var txMetadata *TokenMetadata
		for _, vout := range tx.Vout {
			if md := ParseMetadata(vout.ScriptPubKey.Hex); md != nil {
				txMetadata = md
				break
			}
		}

		for _, vout := range tx.Vout {
			k := positionKey(tx.TxID, vout.N)

			parsed, err := ParseUAPScript(vout.ScriptPubKey.Hex)
			if err == nil && parsed != nil {
				pkHex := hex.EncodeToString(parsed.PubKey)
				pos := &Position{
					TxID:       tx.TxID,
					Vout:       vout.N,
					PubKey:     pkHex,
					Multiplier: parsed.Multiplier,
					Value:      int64(vout.Value),
					IsMint:     parsed.IsMint,
					Height:     block.Height,
					// Kept verbatim, not reassembled from the parsed
					// fields: this is the scriptCode a spender has to sign
					// over, so the only safe version of it is the one the
					// chain actually has.
					Script: vout.ScriptPubKey.Hex,
				}
				if parsed.IsMint {
					// A mint's lineage is SHA256 of its own outpoint. The
					// mint script cannot carry that value -- it depends on
					// the txid, which depends on the script -- so consensus
					// only checks it when the mint is first spent. The
					// indexer is under no such constraint: it can see the
					// outpoint the moment the output is indexed, and the
					// derivation is deterministic, so a freshly minted token
					// has a lineage here before anyone spends it.
					origin, err := UapOriginFromOutpoint(tx.TxID, vout.N)
					if err != nil {
						return meta, err
					}
					pos.Origin = hex.EncodeToString(origin)
					pos.Metadata = txMetadata
				} else {
					// A transfer states its lineage outright. This used to be
					// reconstructed by following the spend graph, guessing
					// when a transaction spent several positions; the origin
					// being in the script is what retired all of that.
					pos.Origin = hex.EncodeToString(parsed.Origin)
				}
				if err := t.putPosition(pos); err != nil {
					return meta, err
				}
				if err := t.addToLineage(pos); err != nil {
					return meta, err
				}
				change.Created = append(change.Created, k)
				continue
			}

			// Not a UAP script; check whether it is a plain P2PKH.
			if hash160 := ParseP2PKHScript(vout.ScriptPubKey.Hex); hash160 != nil {
				utxo := &UTXO{
					TxID:    tx.TxID,
					Vout:    vout.N,
					Hash160: hex.EncodeToString(hash160),
					Value:   int64(vout.Value),
					Height:  block.Height,
				}
				if err := t.putUTXO(utxo); err != nil {
					return meta, err
				}
				change.UTXOsCreated = append(change.UTXOsCreated, k)
			}
		}
	}

	if err := t.putHeight(block.Height, block.Hash, change); err != nil {
		return meta, err
	}
	meta.tipHeight = block.Height
	meta.tipHash = block.Hash

	// Discard undo logs that have fallen out of the reorg window. Undo is
	// only reachable from the tip backwards, so history below the deepest
	// reorg worth surviving is dead weight, and retaining it grew the
	// database without bound.
	if idx.ReorgWindow > 0 {
		cutoff := block.Height - idx.ReorgWindow
		if cutoff >= meta.prunedBelow {
			if err := t.pruneHeightsBelow(cutoff + 1); err != nil {
				return meta, err
			}
			meta.prunedBelow = cutoff + 1
		}
	}
	return meta, nil
}

// UndoBlock reverses everything ApplyBlock did for the given height. Used
// when a reorg is detected and the indexed chain must roll back before
// re-applying the new best chain.
func (idx *Index) UndoBlock(height int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.write(func(t *storeTx) (storeMeta, error) {
		return idx.undoBlock(t, height)
	})
}

func (idx *Index) undoBlock(t *storeTx, height int64) (storeMeta, error) {
	meta := idx.meta()

	change, err := t.undoLog(height)
	if err != nil {
		return meta, err
	}
	if change == nil {
		// No undo log: either this height was never applied, or its log
		// has been pruned out of the reorg window. Either way there is
		// nothing to reverse, and removing the height's hash would only
		// destroy the evidence that we disagree with the node here.
		// Callers check CanUndo to tell the two apart.
		return meta, nil
	}

	// Remove created UAP positions.
	for _, k := range change.Created {
		pos, err := t.position(k)
		if err != nil {
			return meta, err
		}
		if pos != nil {
			// A position created and spent within this same block was
			// already withdrawn from its lineage when it was spent;
			// withdrawing it twice would corrupt the running totals.
			if !pos.Spent {
				if err := t.removeFromLineage(pos); err != nil {
					return meta, err
				}
			}
		}
		if err := t.delPosition(k); err != nil {
			return meta, err
		}
	}

	// Unmark spent UAP positions. Any created by this same block are gone
	// by now, so this only touches ones that outlive it.
	for _, k := range change.SpentKeys {
		pos, err := t.position(k)
		if err != nil {
			return meta, err
		}
		if pos == nil {
			continue
		}
		pos.Spent = false
		pos.SpentTxID = ""
		pos.SpentHeight = 0
		if err := t.putPosition(pos); err != nil {
			return meta, err
		}
		if err := t.addToLineage(pos); err != nil {
			return meta, err
		}
	}

	// Put back any orders this block pruned because their position was
	// spent. The block is being undone, so the spend did not happen: the
	// position is unspent again and the maker's offer stands, at the same
	// price and with the same signature.
	//
	// Skipped for positions this block also created, which the loop above
	// has just deleted -- restoring those would leave an order row naming
	// an outpoint the chain no longer has. Defensive, and knowingly
	// untested: an order cannot exist for a position created and spent
	// within one block, because publishing one needs the position to
	// already be visible and both halves happen under the same write lock
	// PublishOrder takes. The guard stays because that reasoning lives in
	// the locking, nowhere near here.
	createdByThisBlock := make(map[string]bool, len(change.Created))
	for _, k := range change.Created {
		createdByThisBlock[k] = true
	}
	for _, o := range change.OrdersPruned {
		if createdByThisBlock[positionKey(o.TxID, o.Vout)] {
			continue
		}
		if err := t.putOrder(o); err != nil {
			return meta, err
		}
	}

	// Remove created P2PKH UTXOs.
	createdHere := make(map[string]bool, len(change.UTXOsCreated))
	for _, k := range change.UTXOsCreated {
		createdHere[k] = true
		if err := t.delUTXO(k); err != nil {
			return meta, err
		}
	}

	// Restore spent P2PKH UTXOs -- except any this same block also
	// created. Those two lists overlap when one transaction pays an
	// address and a later transaction in the same block spends that
	// output. Undoing the block erases both halves, so the output should
	// simply cease to exist; restoring it would leave the index holding a
	// UTXO belonging to a transaction that is no longer in its chain, and
	// handing it out as spendable to whoever queried that address.
	//
	// (The position path above is immune to this by construction: its
	// restore loop only touches keys still present, and the delete loop
	// has just removed them.)
	for _, utxo := range change.UTXOsSpent {
		if createdHere[positionKey(utxo.TxID, utxo.Vout)] {
			continue
		}
		if err := t.putUTXO(utxo); err != nil {
			return meta, err
		}
	}

	if err := t.delHeight(height); err != nil {
		return meta, err
	}

	// Move the tip down, atomically with the rest of the rollback. Doing
	// this in the caller instead meant a reader could catch the index
	// between the two steps and see a tip naming a block whose positions
	// and UTXOs had already been removed -- and left TipHash untouched,
	// since the caller only knew how to decrement the height, so /status
	// reported a height and a hash from different blocks.
	//
	// If there is no record of height-1 -- the database began mid-chain,
	// or startHeight is above 0 -- the hash is cleared rather than
	// guessed. An empty hash makes HashAtHeight report "no opinion", which
	// is the honest answer, and the reorg walk stops at startHeight anyway.
	if meta.tipHeight == height {
		meta.tipHeight = height - 1
		hash, err := t.hashAt(height - 1)
		if err != nil {
			return meta, err
		}
		meta.tipHash = hash
	}
	return meta, nil
}

// ---------------------------------------------------------------------
// Reads. These go straight to the database and do not take idx.mu: SQLite
// serialises them itself, so a slow query can no longer stall ingestion.
// ---------------------------------------------------------------------

// PositionsForPubKey returns all known positions for a recipient pubkey
// (hex-encoded), optionally filtered to unspent only.
func (idx *Index) PositionsForPubKey(pubkeyHex string, unspentOnly bool) ([]Position, error) {
	return idx.store.PositionsForPubKey(pubkeyHex, unspentOnly)
}

// PositionsForPubKeyPage is PositionsForPubKey over one page, also
// reporting whether more rows follow.
func (idx *Index) PositionsForPubKeyPage(pubkeyHex string, unspentOnly bool, page Page) ([]Position, bool, error) {
	return idx.store.PositionsForPubKeyPage(pubkeyHex, unspentOnly, page)
}

// Position looks up a single position by outpoint.
func (idx *Index) Position(txid string, vout uint32) (Position, bool, error) {
	return idx.store.Position(positionKey(txid, vout))
}

// UTXO looks up a single UTXO by outpoint.
func (idx *Index) UTXO(txid string, vout uint32) (UTXO, bool, error) {
	return idx.store.UTXO(positionKey(txid, vout))
}

// UTXOsForHash160 returns all known UTXOs for a hash160 (hex-encoded).
func (idx *Index) UTXOsForHash160(hash160Hex string) ([]UTXO, error) {
	return idx.store.UTXOsForHash160(hash160Hex)
}

// UTXOsForHash160Page is UTXOsForHash160 over one page, largest value
// first so a truncated page is still the most spendable one.
func (idx *Index) UTXOsForHash160Page(hash160Hex string, page Page) ([]UTXO, bool, error) {
	return idx.store.UTXOsForHash160Page(hash160Hex, page)
}

// AllTokens returns every lineage that still has unspent positions.
// Positions with an empty Origin belong to no lineage and are excluded.
func (idx *Index) AllTokens() ([]TokenInfo, error) {
	return idx.store.AllTokens()
}

// AllTokensPage is AllTokens over one page.
func (idx *Index) AllTokensPage(page Page) ([]TokenInfo, bool, error) {
	return idx.store.AllTokensPage(page)
}

// Token returns a single lineage by origin, or ok=false if the origin is
// unknown or has no unspent positions.
func (idx *Index) Token(origin string) (TokenInfo, bool, error) {
	return idx.store.Token(origin)
}

// Status is a snapshot of indexer progress for the /status endpoint.
type Status struct {
	TipHeight     int64  `json:"tip_height"`
	TipHash       string `json:"tip_hash"`
	PositionCount int    `json:"position_count"`
	OrderCount    int    `json:"order_count"`

	// Healthy is false once the store has latched a persistent write error
	// (see StoreErr) and stopped indexing new blocks. TipHeight/TipHash
	// above are then stale -- frozen at the last block that fully
	// committed -- and every block since is silently missing from the
	// index. Deliberately not omitempty: the whole point is a field that is
	// present and false in the unhealthy case, so a caller that only reads
	// the JSON body (rather than the HTTP status -- see writeStatus in
	// api.go) still has something unambiguous to key an alert on.
	Healthy bool `json:"healthy"`

	// StoreErr is the latched error's message, empty when Healthy is true.
	StoreErr string `json:"store_error,omitempty"`
}

// TokenInfo represents a UAP lineage aggregated across its unspent positions.
type TokenInfo struct {
	Origin       string `json:"origin"` // hex SHA256 of the originating mint's outpoint
	Multiplier   int64  `json:"multiplier"`
	Ticker       string `json:"ticker,omitempty"`
	MetadataHash string `json:"metadata_hash,omitempty"`
	Supply       int64  `json:"supply"`  // sum of unspent value * multiplier
	Holders      int    `json:"holders"` // count of distinct unspent pubkeys
	MintHeight   int64  `json:"mint_height"`
}

func (idx *Index) StatusSnapshot() (Status, error) {
	idx.mu.RLock()
	s := Status{
		TipHeight: idx.TipHeight,
		TipHash:   idx.TipHash,
		Healthy:   idx.storeErr == nil,
	}
	if idx.storeErr != nil {
		s.StoreErr = idx.storeErr.Error()
	}
	idx.mu.RUnlock()

	var err error
	if s.PositionCount, err = idx.store.CountPositions(); err != nil {
		return s, err
	}
	if s.OrderCount, err = idx.store.CountOrders(); err != nil {
		return s, err
	}
	return s, nil
}
