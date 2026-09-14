package main

import (
	"errors"
	"fmt"
)

// probeRawTxLookup determines whether this relay's node can answer
// getrawtransaction for a CONFIRMED transaction -- the capability GET
// /rawtx (api.go) depends on, which every wallet's fill-time position
// verification (verifyPosition, contrib/uap-web/market.js) depends on in
// turn.
//
// A default whippetd only indexes mempool transactions; getrawtransaction
// on anything else fails with "No such mempool transaction. Use -txindex
// to enable blockchain transaction queries" (src/rpc/rawtransaction.cpp)
// UNLESS the transaction still has at least one unspent output, in which
// case GetTransaction's slow path (validation.cpp) locates it via the
// current UTXO set and rereads the containing block from disk. That
// fallback is exactly what makes a live, unsold UAP position work without
// -txindex at all -- a position for sale is, by definition, unspent -- but
// it silently stops working the moment every output of a transaction has
// been spent, and there is no way to distinguish "this node has no
// confirmed-tx capability at all" from "this specific transaction happens
// to be fully spent" by testing a real position, some of which might
// legitimately not exist yet on a freshly deployed relay.
//
// So this probes with a transaction that is guaranteed to still have an
// unspent output on ANY chain, at ANY point past its first block: the
// coinbase of the current tip. Consensus (coinbase maturity) refuses to
// let anyone spend a coinbase before 100 confirmations, and a block at the
// tip has at most one confirmation, so that coinbase cannot have been
// spent yet -- it is exactly as "confirmed and unspent" as a live UAP
// position, without requiring one to already exist.
//
// The genesis block's own coinbase would NOT work for this: like most
// Bitcoin-derived chains, it is specially excluded from the UTXO set
// (verified by hand against a real regtest node -- see the commit message
// for this file), so a probe landing on height 0 would misreport a
// perfectly healthy node as broken. Height >= 1 avoids that.
//
// Returns:
//   - checked=false when nothing was learned (no blocks past genesis yet,
//     or a transport failure reaching the node) -- the caller should try
//     again later rather than treating this as a verified answer.
//   - checked=true, ok=true when confirmed-transaction lookup was proven
//     to work.
//   - checked=true, ok=false, with detail carrying the node's own error
//     message, when it was proven NOT to work.
func probeRawTxLookup(rpc *RPCClient) (checked bool, ok bool, detail string) {
	height, err := rpc.GetBlockCount()
	if err != nil {
		return false, false, fmt.Sprintf("could not reach the node to check: %v", err)
	}
	if height < 1 {
		// Nothing to probe with yet, and nothing to sell yet either: a
		// chain with no blocks past genesis has no UAP activation height
		// behind it. Left unchecked rather than guessed at.
		return false, false, "chain has no blocks past genesis yet"
	}

	hash, err := rpc.GetBlockHash(height)
	if err != nil {
		return false, false, fmt.Sprintf("could not fetch the tip block hash: %v", err)
	}
	block, err := rpc.GetBlockVerbose(hash)
	if err != nil {
		return false, false, fmt.Sprintf("could not fetch the tip block: %v", err)
	}
	if len(block.Tx) == 0 {
		return false, false, "tip block reported no transactions at all"
	}
	coinbaseTxid := block.Tx[0].TxID

	if _, err := rpc.GetRawTransactionHex(coinbaseTxid); err != nil {
		var rejection *NodeRejection
		if errors.As(err, &rejection) {
			// The node answered, and it answered no. That is a real,
			// trustworthy result: confirmed-transaction lookup does not
			// work on this node.
			return true, false, rejection.Message
		}
		// A transport failure while running the probe itself proves
		// nothing either way -- the same distinction writeStoreError and
		// broadcastHandler already draw elsewhere in this package between
		// "the node said no" and "the call never reached the node".
		return false, false, fmt.Sprintf("probe request failed: %v", err)
	}
	return true, true, ""
}
