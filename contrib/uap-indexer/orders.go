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
// an EC library, deliberately staying dependency-free -- so it can't
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

// PublishOrder validates and stores a maker's signed order. The
// referenced position must be a known, unspent UAP output whose
// multiplier matches what the order claims.
func (idx *Index) PublishOrder(o *Order) error {
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

	k := positionKey(o.TxID, o.Vout)
	pos, ok := idx.Positions[k]
	if !ok {
		return fmt.Errorf("unknown position %s", k)
	}
	if pos.Spent {
		return fmt.Errorf("position %s is already spent", k)
	}
	if pos.Multiplier != o.Multiplier {
		return fmt.Errorf("multiplier mismatch: position has %d, order claims %d", pos.Multiplier, o.Multiplier)
	}

	o.PubKey = pos.PubKey
	o.CreatedAt = time.Now().Unix()
	if idx.Orders == nil {
		idx.Orders = make(map[string]*Order)
	}
	idx.Orders[k] = o
	return nil
}

// ListOrders returns all open orders (i.e. whose underlying position is
// still unspent), optionally filtered to a specific multiplier.
func (idx *Index) ListOrders(multiplierFilter *int64) []Order {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var out []Order
	for k, o := range idx.Orders {
		pos := idx.Positions[k]
		if pos == nil || pos.Spent {
			continue // stale: position vanished (reorg) or got spent (filled or moved elsewhere)
		}
		if multiplierFilter != nil && o.Multiplier != *multiplierFilter {
			continue
		}
		out = append(out, *o)
	}
	return out
}

// GetOrder looks up a single open order by the outpoint it sells.
func (idx *Index) GetOrder(txid string, vout uint32) (Order, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	k := positionKey(txid, vout)
	o, ok := idx.Orders[k]
	if !ok {
		return Order{}, false
	}
	if pos := idx.Positions[k]; pos == nil || pos.Spent {
		return Order{}, false
	}
	return *o, true
}

// CancelOrder removes a published order. As a cheap (non-cryptographic)
// identity check against casual griefing, the caller must reproduce the
// exact scriptSig that was originally published -- not real
// authentication, just enough friction that only someone who already had
// the order's signed contents can remove it. The maker can always
// unilaterally invalidate their own order for real by spending the
// position elsewhere; this is purely a relay-hygiene convenience.
func (idx *Index) CancelOrder(txid string, vout uint32, scriptSigHex string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	k := positionKey(txid, vout)
	o, ok := idx.Orders[k]
	if !ok {
		return fmt.Errorf("no such order")
	}
	if o.ScriptSig != scriptSigHex {
		return fmt.Errorf("script_sig does not match the published order")
	}
	delete(idx.Orders, k)
	return nil
}
