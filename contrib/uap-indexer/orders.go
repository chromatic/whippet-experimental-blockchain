package main

import (
	"encoding/hex"
	"fmt"
	"time"
)

// Order is a maker's standing, signed sell order for a UAP position,
// using SIGHASH_SINGLE|ANYONECANPAY (see doc/uap-marketplace-design.md
// and contrib/uap-js's signMakerOrder/fillOrder). The relay never holds
// funds or private keys -- it only stores and serves signed fragments
// that are worthless without a taker completing them exactly as signed.
type Order struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Multiplier    int64  `json:"multiplier"`
	PubKey        string `json:"pubkey"`         // maker's pubkey, read from the position itself
	ScriptSig     string `json:"script_sig"`     // hex; the maker's signed push (DER sig + hashtype byte)
	PaymentScript string `json:"payment_script"` // hex
	PaymentValue  int64  `json:"payment_value"`  // satoshis
	CreatedAt     int64  `json:"created_at"`     // unix seconds, for display

	// BackingValue is the satoshi value locked in the position this order sells.
	// Essential for buyers to assess whether the position is fully backed.
	// Populated at publish time from the position itself.
	BackingValue int64 `json:"backing_value"` // satoshis

	// Origin is the lineage of the position being sold: which token this is.
	//
	// The taker cannot work it out for themselves, and this is not a
	// convenience. An order carries the maker's scriptSig, never their
	// scriptPubKey, so for a transfer the origin is simply not visible from
	// the taker's side -- and deriving it from the maker's outpoint is
	// correct only when the position is a fresh mint being sold for the
	// first time. A taker who guessed would build a transaction that looks
	// well-formed, signs cleanly, and is rejected by consensus, because its
	// covenant output would name a lineage with no input in the transaction.
	//
	// Read off the position, like BackingValue, so it cannot drift.
	Origin string `json:"origin"` // hex, 32 bytes

	// CancelNonce is a relay-assigned value, unique to this publish, that
	// a cancellation for this order must sign over (see cancel_auth.go).
	// It exists because CreatedAt's one-second resolution is not fine
	// enough: signing is deterministic, so a maker who cancels and then
	// republishes an *identical* order (same terms, same script_sig)
	// within the same wall-clock second would otherwise get back a
	// message identical to the one a captured cancel signature already
	// authorized. CancelNonce always changes across a publish/republish/
	// mirror-adopt regardless of clock resolution -- see nextCancelNonce.
	//
	// Encoded as a JSON string (the `,string` tag), not a bare number:
	// nextCancelNonce seeds from UnixNano, which by 2026 already exceeds
	// 2^53 and so is not exactly representable as a JS/JSON double. A
	// client that round-tripped it through Number would sign a rounded
	// value that silently mismatches what the relay has on file, and
	// every cancel would fail verification. contrib/uap-js's
	// signCancelOrder and contrib/uap-web's api.js both treat this field
	// as a string for exactly this reason -- never coerce it to Number.
	CancelNonce int64 `json:"cancel_nonce,string"`

	// Status is the order's confirmation state. Empty or "confirmed" means
	// the position is unspent on-chain. "pending_fill" means the position
	// is being spent by an unconfirmed transaction (see PendingTxid).
	// Only set by GetOrder when the position is in the pending set; absent
	// from ListOrders results (which exclude pending orders).
	Status string `json:"status,omitempty"`

	// PendingTxid is the txid of the unconfirmed transaction spending this
	// order's position, when Status is "pending_fill". Empty otherwise.
	PendingTxid string `json:"pending_txid,omitempty"`
}

const (
	sighashSingle       = 0x03
	sighashAnyoneCanPay = 0x80
	sighashOrderType    = sighashSingle | sighashAnyoneCanPay
)

// validateScriptSig does a structural sanity check on a maker's scriptSig:
// it must be a single minimal push of a signature ending in the
// SIGHASH_SINGLE|ANYONECANPAY byte, with something DER-shaped before it.
// This is NOT full cryptographic verification -- the relay doesn't carry
// an EC library -- so it can't
// confirm the signature actually verifies against the position's pubkey.
// A taker's own node is the real, authoritative check when a fill is
// broadcast; this only keeps obviously-malformed orders out of the relay.
func validateScriptSig(scriptSigHex string) error {
	raw, err := hex.DecodeString(scriptSigHex)
	if err != nil {
		return fmt.Errorf("invalid script_sig hex: %w", err)
	}
	if len(raw) < 9 {
		return fmt.Errorf("script_sig too short to be a signature push")
	}
	pushLen := int(raw[0])
	if pushLen < 1 || pushLen > 0x4b || len(raw) != 1+pushLen {
		return fmt.Errorf("script_sig must be a single minimal data push")
	}
	sigAndHashType := raw[1:]
	hashType := sigAndHashType[len(sigAndHashType)-1]
	if hashType != sighashOrderType {
		return fmt.Errorf("expected SIGHASH_SINGLE|ANYONECANPAY (0x%02x), got 0x%02x", sighashOrderType, hashType)
	}
	der := sigAndHashType[:len(sigAndHashType)-1]
	if len(der) < 8 || der[0] != 0x30 {
		return fmt.Errorf("does not look like a DER-encoded ECDSA signature")
	}
	return nil
}

// PublishOrder validates and stores a maker's signed order, submitted
// directly to this relay. The referenced position must be a known, unspent
// UAP output whose multiplier matches what the order claims.
//
// A local publish clears any tombstone for the outpoint: it is an explicit
// act by whoever holds the signed fragment, so whatever earlier withdrawal
// is on record has been superseded.
func (idx *Index) PublishOrder(o *Order) error {
	return idx.publishOrder(o, false)
}

// AdoptMirroredOrder stores an order pulled from a peer relay. Identical
// to PublishOrder except that it honours tombstones instead of clearing
// them -- see the note on CancelOrder for why the two paths differ.
func (idx *Index) AdoptMirroredOrder(o *Order) error {
	return idx.publishOrder(o, true)
}

func (idx *Index) publishOrder(o *Order, fromMirror bool) error {
	if err := validateScriptSig(o.ScriptSig); err != nil {
		return err
	}
	if o.PaymentValue <= 0 {
		return fmt.Errorf("payment_value must be positive")
	}
	if _, err := hex.DecodeString(o.PaymentScript); err != nil {
		return fmt.Errorf("invalid payment_script hex: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.storeErr != nil {
		return idx.storeErr
	}

	k := positionKey(o.TxID, o.Vout)

	// The position check and the write share one transaction. Separately
	// they would race the block poller: a position could be spent between
	// "is it unspent?" and the insert, storing an order that was already
	// dead -- and, worse, one that ApplyBlock's pruning has already been
	// past and will never revisit.
	t, err := idx.store.begin()
	if err != nil {
		return err
	}
	defer t.rollback()

	pos, err := t.position(k)
	if err != nil {
		return err
	}
	if pos == nil {
		return fmt.Errorf("unknown position %s", k)
	}
	if fromMirror {
		// A peer that mirrored this order before it was withdrawn will
		// keep offering it, and it still passes every validity check --
		// the position is real, unspent and matching -- so without this
		// the maker's cancel is undone on the next poll, and on every
		// poll after that.
		//
		// Matched on the exact scriptSig: a tombstone withdraws one
		// signed fragment, not the outpoint. A maker who signs a fresh
		// order at a different price is making a new offer, and a peer
		// relaying that is doing its job.
		withdrawn, err := t.tombstone(k)
		if err != nil {
			return err
		}
		if withdrawn != "" && withdrawn == o.ScriptSig {
			return fmt.Errorf("order for %s was withdrawn here", k)
		}
	}
	if pos.Spent {
		return fmt.Errorf("position %s is already spent", k)
	}
	if pos.Multiplier != o.Multiplier {
		return fmt.Errorf("multiplier mismatch: position has %d, order claims %d", pos.Multiplier, o.Multiplier)
	}

	o.PubKey = pos.PubKey
	o.BackingValue = pos.Value
	o.Origin = pos.Origin
	o.CreatedAt = time.Now().Unix()
	o.CancelNonce = idx.nextCancelNonce()
	if err := t.putOrder(o); err != nil {
		return err
	}
	if !fromMirror {
		if err := t.delTombstone(k); err != nil {
			return err
		}
	}
	return t.commit()
}

// nextCancelNonce returns a value strictly greater than every value it has
// returned before in this process, even if called twice within the same
// wall-clock nanosecond or across a clock adjustment that moves time
// backward. It is seeded from the wall clock (so, in the ordinary case
// where the clock only moves forward, successive nonces still reflect
// roughly when they were issued) but never repeats or moves backward
// regardless of what the clock does, which is the only property the
// cancel-signature scheme in cancel_auth.go actually needs from it.
//
// Callers must hold idx.mu -- publishOrder already does for the whole of
// its critical section, which is the only caller.
func (idx *Index) nextCancelNonce() int64 {
	n := time.Now().UnixNano()
	if n <= idx.lastCancelNonce {
		n = idx.lastCancelNonce + 1
	}
	idx.lastCancelNonce = n
	return n
}

// ListOrders returns all open orders (i.e. whose underlying position is
// still unspent and not pending a fill), optionally filtered to a specific
// multiplier. Orders whose positions are in the pending spend set are excluded.
func (idx *Index) ListOrders(multiplierFilter *int64) ([]Order, error) {
	// Goes through the paged path so "open" is defined once. Filtering here
	// as well would be a second copy of the rule: harmless while unlimited
	// (there is no LIMIT to miscount), and free to drift out of agreement
	// with the real one the moment either changes.
	orders, _, err := idx.ListOrdersPage(multiplierFilter, Unlimited)
	return orders, err
}

// ListOrdersPage is ListOrders over one page. Orders whose positions are
// pending fills are excluded from the results.
func (idx *Index) ListOrdersPage(multiplierFilter *int64, page Page) ([]Order, bool, error) {
	// The pending outpoints go INTO the query rather than being used to filter
	// its result, so LIMIT counts only rows that will actually be served and
	// hasMore describes the same set the caller got back. Filtering afterwards
	// let a page of entirely-pending orders come back empty with hasMore true.
	idx.mu.RLock()
	pending := idx.pendingSpends.Keys()
	idx.mu.RUnlock()

	return idx.store.ListOrdersPage(multiplierFilter, pending, page)
}

// GetOrder looks up a single open order by the outpoint it sells.
// If the position is in the pending spend set, the returned order will have
// Status="pending_fill" and PendingTxid set to the filling transaction's ID.
func (idx *Index) GetOrder(txid string, vout uint32) (Order, bool, error) {
	outpoint := positionKey(txid, vout)
	o, ok, err := idx.store.Order(outpoint)
	if err != nil {
		return Order{}, false, err
	}
	if !ok {
		return Order{}, false, nil
	}

	// Check if the position is in the pending spend set.
	if pendingTxid, isPending := idx.IsPendingSpend(outpoint); isPending {
		o.Status = "pending_fill"
		o.PendingTxid = pendingTxid
	}

	return o, true, nil
}

// CancelOrder removes a published order. The caller must supply a valid
// ECDSA signature, by the position's own pubkey (Order.PubKey, set
// server-side from the position itself, never taken from the client), over
// cancelMessage(txid, vout, <the order's current script_sig>, <the order's
// current cancel_nonce>) -- see cancel_auth.go for the full authorization
// scheme: what exactly is signed, replay handling, cross-relay scope, and
// the absence of any clock dependence. The maker can always unilaterally
// invalidate their own order for real by spending the position elsewhere;
// this is a relay-hygiene action, not a consensus one.
//
// The withdrawal is recorded as a tombstone so that mirroring cannot undo
// it. That is the limit of what a cancel can promise: peers still hold the
// signed fragment and may go on serving it to their own users. This stops
// *this* relay from resurrecting what its own user withdrew, nothing
// wider.
//
// Tombstones are never pruned, deliberately. Pruning them when the
// position is spent would be the obvious trigger -- a spent position can
// never carry a valid order again -- but a reorg can unspend it, and the
// tombstone would already be gone, so the peer's stale order would come
// back on the next poll. They are also cheap to keep: the table is keyed
// by outpoint, so repeated publish/cancel cycles on one position leave one
// row, and its size is bounded by the number of distinct positions ever
// cancelled -- necessarily smaller than the positions table itself.
func (idx *Index) CancelOrder(txid string, vout uint32, cancelSigHex string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.storeErr != nil {
		return idx.storeErr
	}

	k := positionKey(txid, vout)
	o, ok, err := idx.store.StoredOrder(k)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such order")
	}
	if err := verifyCancelSignature(o.PubKey, txid, vout, o.ScriptSig, o.CancelNonce, cancelSigHex); err != nil {
		return fmt.Errorf("cancel signature does not authorize this order: %w", err)
	}

	t, err := idx.store.begin()
	if err != nil {
		return err
	}
	defer t.rollback()
	if err := t.delOrder(k); err != nil {
		return err
	}
	if err := t.putTombstone(k, o.ScriptSig, time.Now().Unix()); err != nil {
		return err
	}
	return t.commit()
}
