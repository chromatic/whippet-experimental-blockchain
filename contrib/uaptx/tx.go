// Package uaptx builds and signs OP_MINT / OP_MINT_TRANSFER transactions,
// ported from contrib/uap-js/uap.js. It talks to no network: it only builds
// scripts, serializes transactions, and signs them. Callers supply UTXO
// details and are responsible for broadcasting the resulting hex themselves.
package uaptx

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// TxIn is a transaction input. Txid is hex, big-endian/display order
// (matching what a node's RPC reports), Vout is the output index, ScriptSig
// is the raw (already-assembled) scriptSig, and Sequence defaults to
// 0xffffffff when left at zero... except Go's zero value for uint32 is 0,
// which is a real, distinct sequence number, so callers must set it
// explicitly. DefaultSequence is provided for that.
type TxIn struct {
	Txid      string
	Vout      uint32
	ScriptSig []byte
	Sequence  uint32

	// scriptSigLength, when non-nil, overrides the declared length prefix
	// written ahead of ScriptSig during serialization, without changing the
	// bytes actually written. This exists solely for signatureHash's
	// scriptCode, where the node can write a length prefix that disagrees
	// with the bytes that follow (see serializeScriptCode). Nothing else
	// should ever set it.
	scriptSigLength *int
}

// TxOut is a transaction output.
type TxOut struct {
	Value        int64
	ScriptPubKey []byte
}

// Tx is a transaction: { version, vin, vout, locktime }, legacy
// (pre-segwit) format only.
type Tx struct {
	Version  uint32
	Vin      []TxIn
	Vout     []TxOut
	Locktime uint32
}

// DefaultSequence is the sequence number used when none is specified.
const DefaultSequence uint32 = 0xffffffff

func encodeVarInt(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		out := make([]byte, 3)
		out[0] = 0xfd
		binary.LittleEndian.PutUint16(out[1:], uint16(n))
		return out
	case n <= 0xffffffff:
		out := make([]byte, 5)
		out[0] = 0xfe
		binary.LittleEndian.PutUint32(out[1:], uint32(n))
		return out
	default:
		panic("uaptx: varint too large for this helper")
	}
}

func encodeUint32LE(n uint32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, n)
	return out
}

func encodeInt64LE(n int64) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(n))
	return out
}

// reverseBytes returns a new slice with the bytes of b in reverse order.
func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, v := range b {
		out[len(b)-1-i] = v
	}
	return out
}

// SerializeTx serializes tx in legacy (pre-segwit) wire format, matching
// CTransaction::Serialize.
func SerializeTx(tx Tx) []byte {
	version := tx.Version
	if version == 0 {
		version = 1
	}
	var out []byte
	out = append(out, encodeUint32LE(version)...)
	out = append(out, encodeVarInt(uint64(len(tx.Vin)))...)
	for _, vin := range tx.Vin {
		txidBytes, err := hex.DecodeString(vin.Txid)
		if err != nil {
			panic(fmt.Sprintf("uaptx: invalid txid hex %q: %v", vin.Txid, err))
		}
		// A txid is exactly 32 bytes. hex.DecodeString catches only non-hex
		// and odd lengths, so without this a short txid gets written at its
		// short length and shifts every byte after it -- outpoints, scripts,
		// values, all of it -- while still producing clean-looking hex that
		// signs without complaint. There is no valid transaction on the other
		// side of that, so refuse to build one.
		if len(txidBytes) != 32 {
			panic(fmt.Sprintf("uaptx: txid %q is %d bytes, must be 32", vin.Txid, len(txidBytes)))
		}
		txidLE := reverseBytes(txidBytes)
		scriptSig := vin.ScriptSig
		declaredLength := len(scriptSig)
		if vin.scriptSigLength != nil {
			declaredLength = *vin.scriptSigLength
		}
		sequence := vin.Sequence
		out = append(out, txidLE...)
		out = append(out, encodeUint32LE(vin.Vout)...)
		out = append(out, encodeVarInt(uint64(declaredLength))...)
		out = append(out, scriptSig...)
		out = append(out, encodeUint32LE(sequence)...)
	}
	out = append(out, encodeVarInt(uint64(len(tx.Vout)))...)
	for _, vout := range tx.Vout {
		out = append(out, encodeInt64LE(vout.Value)...)
		out = append(out, encodeVarInt(uint64(len(vout.ScriptPubKey)))...)
		out = append(out, vout.ScriptPubKey...)
	}
	out = append(out, encodeUint32LE(tx.Locktime)...)
	return out
}

// TxToHex returns the hex encoding of SerializeTx(tx).
func TxToHex(tx Tx) string {
	return hex.EncodeToString(SerializeTx(tx))
}
