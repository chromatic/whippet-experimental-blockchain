package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// Cancellation authorization.
//
// Withdrawing an order used to be "authorized" by echoing back the order's
// own script_sig -- but that value is exactly what GET /orders publishes to
// every visitor, so it was never a credential. See the removed comment on
// CancelOrder (git history) for the fuller history; this file is the real
// replacement.
//
// A cancellation is now authorized the same way the order itself was made:
// a signature from the position's private key. The relay already knows the
// position's compressed pubkey -- Order.PubKey, set server-side in
// publishOrder from the position, never taken from the client -- so it can
// verify a cancel signature against it without carrying any new trust
// assumption.
//
// # What exactly is signed
//
// cancelMessage binds four things: a fixed domain-separation string (so a
// signature made for this purpose can never be mistaken for one made for
// another -- in particular the maker's own SIGHASH_SINGLE|ANYONECANPAY
// order signature, which is a signature over a transaction preimage, not
// this string, and cannot collide with it), the exact outpoint, the exact
// script_sig of the order being withdrawn, and the order's cancel_nonce.
//
// Binding to script_sig means a signature captured for one order cannot be
// replayed against a different order on the same outpoint (e.g. a
// repriced relist): the two orders carry different script_sigs, and the
// cancellation message differs.
//
// Binding to cancel_nonce closes the one gap script_sig alone leaves:
// ECDSA signing here is deterministic (RFC 6979), so a maker who withdraws
// an order and then republishes an *identical* one (same outpoint, same
// price, same payment script) gets back the bit-for-bit same script_sig.
// Without a nonce in the message, a captured cancel signature for the
// first listing would still verify against the second. cancel_nonce is
// assigned by the relay itself in publishOrder, via Index.nextCancelNonce
// (never by the client), and is guaranteed to differ on every publish --
// local republish or mirrored adoption alike, even two within the same
// wall-clock instant -- so the old cancel signature never matches the
// message the relay reconstructs for the new order. An earlier version of
// this design reused the existing created_at field for this instead of
// adding cancel_nonce; that only has one-second resolution, so a
// cancel/republish cycle completing within the same second would have
// produced two orders with an identical created_at *and* an identical
// script_sig -- an identical message, so a captured signature would still
// replay. cancel_nonce is a distinct field for exactly this reason.
//
// Replay in general: capturing a cancel request and replaying it
// immediately does nothing a network observer couldn't already ask the
// maker to do again, since the first successful cancel deletes the order
// -- a replay hits "no such order" and fails. The only case worth
// engineering around is the republish-with-identical-terms case above,
// which cancel_nonce handles without needing a client-supplied random
// value or a freshness window.
//
// Cross-relay replay: cancel_nonce is relay-local. The same order mirrored
// into two relays gets two different cancel_nonce values (each assigns its
// own at the moment it locally publishes or adopts it -- see
// AdoptMirroredOrder), so the message a cancel signs at relay A does not
// match the message relay B reconstructs for its own copy, and a
// signature captured at A cannot cancel the order at B. This matches how
// cancellation already worked before this change: CancelOrder's tombstone
// is local to the relay it is filed with, and mirroring is a one-way pull
// with no cancel/delete propagation (see mirror.go) -- a maker who wants
// an order gone everywhere must cancel at every relay that has it, same
// as before. Making a cancel signature relay-portable would be a new,
// wider capability nothing else in this design offers, not a restoration
// of one that existed.
//
// Clock dependence: effectively none. cancel_nonce is seeded from the wall
// clock (nextCancelNonce) but forced strictly increasing in-process
// regardless of what the clock does, and verification only ever checks it
// for byte-equality against what publishOrder already recorded -- never
// against wall-clock time, and never within a "freshness window". A
// skewed or even backward-jumping relay clock affects only the *value*
// nextCancelNonce happens to seed from, never whether a given signature
// still matches the order it was made for.
const cancelDomain = "whippet-uap-cancel-order-v1"

// cancelMessage is the exact byte string a maker signs to authorize
// withdrawing order (txid, vout) whose current on-file script_sig is
// scriptSigHex and whose current on-file cancel_nonce is cancelNonce. Both
// sides (contrib/uap-js's signCancelOrder and this file) must construct it
// identically.
func cancelMessage(txid string, vout uint32, scriptSigHex string, cancelNonce int64) []byte {
	s := cancelDomain + "\n" +
		txid + "\n" +
		strconv.FormatUint(uint64(vout), 10) + "\n" +
		scriptSigHex + "\n" +
		strconv.FormatInt(cancelNonce, 10)
	return []byte(s)
}

// hash256 is Bitcoin-style double SHA-256, matching the hashing already
// used for the maker's own order signature (contrib/uap-js/uap.js's
// signatureHash, via hash256 in sha256.js).
func hash256(b []byte) []byte {
	h1 := sha256.Sum256(b)
	h2 := sha256.Sum256(h1[:])
	return h2[:]
}

// verifyCancelSignature reports whether sigHex is a valid DER-encoded
// ECDSA signature, by the key pubKeyHex, over cancelMessage(txid, vout,
// scriptSigHex, cancelNonce).
func verifyCancelSignature(pubKeyHex, txid string, vout uint32, scriptSigHex string, cancelNonce int64, sigHex string) error {
	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return fmt.Errorf("invalid pubkey hex on file: %w", err)
	}
	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("invalid pubkey on file: %w", err)
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}
	sig, err := ecdsa.ParseDERSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	digest := hash256(cancelMessage(txid, vout, scriptSigHex, cancelNonce))
	if !sig.Verify(digest, pubKey) {
		return fmt.Errorf("signature does not authorize cancelling this order")
	}
	return nil
}
