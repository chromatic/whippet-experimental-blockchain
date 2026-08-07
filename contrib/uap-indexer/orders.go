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
	CreatedAt     int64  `json:"created_at"`     // unix seconds
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
	o.CreatedAt = time.Now().Unix()
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

// ListOrders returns all open orders (i.e. whose underlying position is
// still unspent), optionally filtered to a specific multiplier.
func (idx *Index) ListOrders(multiplierFilter *int64) ([]Order, error) {
	return idx.store.ListOrders(multiplierFilter)
}

// GetOrder looks up a single open order by the outpoint it sells.
func (idx *Index) GetOrder(txid string, vout uint32) (Order, bool, error) {
	return idx.store.Order(positionKey(txid, vout))
}

// CancelOrder removes a published order. As a cheap (non-cryptographic)
// identity check against casual griefing, the caller must reproduce the
// exact scriptSig that was originally published -- not real
// authentication, just enough friction that only someone who already had
// the order's signed contents can remove it. The maker can always
// unilaterally invalidate their own order for real by spending the
// position elsewhere; this is purely a relay-hygiene convenience.
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
func (idx *Index) CancelOrder(txid string, vout uint32, scriptSigHex string) error {
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
	if o.ScriptSig != scriptSigHex {
		return fmt.Errorf("script_sig does not match the published order")
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
