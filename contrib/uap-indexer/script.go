package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// UAP opcodes, must match src/script/script.h.
const (
	opMint         = 0xb5
	opMintTransfer = 0xba
	uapOriginSize  = 32 // origin is a SHA256 hash
)

// UapOriginFromOutpoint derives a lineage origin from an outpoint.
// origin = SHA256(txid_bytes || n as 4-byte little-endian)
// where txid_bytes is the REVERSE (internal bytes) of the txid as displayed by RPC.
func UapOriginFromOutpoint(txidHex string, n uint32) ([]byte, error) {
	// Decode the txid from RPC display format (big-endian display)
	txidBytes, err := hex.DecodeString(txidHex)
	if err != nil {
		return nil, fmt.Errorf("invalid txid hex: %w", err)
	}
	if len(txidBytes) != 32 {
		return nil, fmt.Errorf("txid must be 32 bytes, got %d", len(txidBytes))
	}

	// Reverse to get internal bytes
	for i := 0; i < 16; i++ {
		txidBytes[i], txidBytes[31-i] = txidBytes[31-i], txidBytes[i]
	}

	// Append n as 4-byte little-endian
	nBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(nBytes, n)

	// SHA256(txid_bytes || n_bytes)
	h := sha256.New()
	h.Write(txidBytes)
	h.Write(nBytes)
	origin := h.Sum(nil)

	return origin, nil
}

// ParsedUAP is the decoded form of a UAP mint or transfer output script.
// This is a best-effort mirror of ParseUapOutputScript in
// src/script/script.cpp -- it exists purely for observational indexing and
// is not consensus-authoritative. If the on-chain opcode logic changes,
// this must be updated to match or the index will silently drift from what
// the chain actually enforces. src/test/data/uap_script_vectors.json is the
// shared fixture that holds the two in agreement.
type ParsedUAP struct {
	PubKey     []byte
	Multiplier int64
	Origin     []byte // 32 bytes for transfers, empty for mints
	IsMint     bool   // true: OP_MINT (fresh mint); false: OP_MINT_TRANSFER (covenant)
}

// scriptPush is one decoded element of a script: either pushed data, or a
// non-push opcode (Data is nil). Opcode always holds the raw leading byte,
// which is what makes the minimal-encoding check below possible.
type scriptPush struct {
	IsPush bool
	Opcode byte
	Data   []byte
}

// isMinimalPush reports whether p uses the canonical (shortest) encoding for
// the data it pushes -- the BIP62 rule the node enforces as
// SCRIPT_VERIFY_MINIMALDATA, mirroring CheckMinimalPush in src/script/script.cpp.
// ParseUapOutputScript requires it of every element of a UAP output, so
// anything else is not a UAP output as far as consensus is concerned.
func isMinimalPush(p scriptPush) bool {
	switch n := len(p.Data); {
	case n == 0:
		return p.Opcode == 0x00 // OP_0
	case n == 1 && p.Data[0] >= 1 && p.Data[0] <= 16:
		return p.Opcode == 0x50+p.Data[0] // OP_1 .. OP_16
	case n == 1 && p.Data[0] == 0x81:
		return p.Opcode == 0x4f // OP_1NEGATE
	case n <= 75:
		return int(p.Opcode) == n
	case n <= 255:
		return p.Opcode == 0x4c // OP_PUSHDATA1
	case n <= 65535:
		return p.Opcode == 0x4d // OP_PUSHDATA2
	default:
		return true
	}
}

// isMinimalScriptNum reports whether data is the shortest byte string
// encoding its value -- CScriptNum's own minimality rule, which is separate
// from isMinimalPush (0x0100 is a minimal 2-byte *push* of a non-minimal
// *number*).
func isMinimalScriptNum(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	// The most significant byte carries only the sign bit, so it is
	// redundant unless the byte below it has its own high bit set.
	if data[len(data)-1]&0x7f == 0 {
		return len(data) > 1 && data[len(data)-2]&0x80 != 0
	}
	return true
}

// readPushes decodes every element of a script. Returns an error if the
// script is malformed (truncated push, etc).
func readPushes(script []byte) ([]scriptPush, error) {
	var out []scriptPush
	i := 0
	for i < len(script) {
		b := script[i]
		i++
		switch {
		case b >= 1 && b <= 0x4b:
			if i+int(b) > len(script) {
				return nil, fmt.Errorf("truncated push at offset %d", i-1)
			}
			out = append(out, scriptPush{IsPush: true, Opcode: b, Data: script[i : i+int(b)]})
			i += int(b)
		case b == 0x4c: // OP_PUSHDATA1
			if i+1 > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA1")
			}
			n := int(script[i])
			i++
			if i+n > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA1 payload")
			}
			out = append(out, scriptPush{IsPush: true, Opcode: b, Data: script[i : i+n]})
			i += n
		case b == 0x4d: // OP_PUSHDATA2
			if i+2 > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA2")
			}
			n := int(script[i]) | int(script[i+1])<<8
			i += 2
			if i+n > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA2 payload")
			}
			out = append(out, scriptPush{IsPush: true, Opcode: b, Data: script[i : i+n]})
			i += n
		case b == 0x4e: // OP_PUSHDATA4
			if i+4 > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA4")
			}
			n := int(script[i]) | int(script[i+1])<<8 | int(script[i+2])<<16 | int(script[i+3])<<24
			i += 4
			if i+n > len(script) {
				return nil, fmt.Errorf("truncated PUSHDATA4 payload")
			}
			out = append(out, scriptPush{IsPush: true, Opcode: b, Data: script[i : i+n]})
			i += n
		case b == 0x00: // OP_0: an empty data push, and <= OP_PUSHDATA4
			out = append(out, scriptPush{IsPush: true, Opcode: b, Data: []byte{}})
		// NOTE: OP_1..OP_16 (0x51..0x60) and OP_1NEGATE (0x4f) deliberately
		// fall through to the non-push case below. They push a value only
		// when *executed*, so they are not data pushes; the multiplier field
		// is the one place a UAP script may use them, and ParseUAPScript
		// handles that itself.
		default:
			out = append(out, scriptPush{IsPush: false, Opcode: b})
		}
	}
	return out, nil
}

// scriptNumToInt64 decodes a Bitcoin-script "CScriptNum" byte string:
// little-endian magnitude, sign carried in the high bit of the last byte.
func scriptNumToInt64(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > 8 {
		return 0, fmt.Errorf("script number too long (%d bytes)", len(data))
	}
	var result int64
	for i, b := range data {
		result |= int64(b) << (8 * uint(i))
	}
	if data[len(data)-1]&0x80 != 0 {
		result &^= int64(0x80) << (8 * uint(len(data)-1))
		result = -result
	}
	return result, nil
}

// ParseUAPScript recognizes:
//
//	<pubkey> <multiplier> OP_MINT                           (fresh mint)
//	<pubkey> <multiplier> <origin32> OP_MINT_TRANSFER       (transfer/covenant)
//
// and returns nil, nil for any script that doesn't match (not an error --
// most outputs on the chain are ordinary, non-UAP outputs).
func ParseUAPScript(scriptHex string) (*ParsedUAP, error) {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return nil, fmt.Errorf("invalid script hex: %w", err)
	}
	if len(raw) < 2 {
		return nil, nil
	}
	last := raw[len(raw)-1]
	if last != opMint && last != opMintTransfer {
		return nil, nil
	}
	isMint := last == opMint

	pushes, err := readPushes(raw)
	if err != nil {
		return nil, nil // malformed script; not a UAP output we can trust
	}

	wantLen := 3 // pubkey, multiplier, OP_MINT for mints
	if !isMint {
		wantLen = 4 // pubkey, multiplier, origin, OP_MINT_TRANSFER for transfers
	}
	if len(pushes) != wantLen {
		return nil, nil
	}

	pk := pushes[0]
	if !pk.IsPush || !isMinimalPush(pk) || (len(pk.Data) != 33 && len(pk.Data) != 65) {
		return nil, nil
	}

	// The multiplier is canonically encoded: OP_0 for zero, OP_1..OP_16 for
	// 1..16, and a minimal data push of a minimal CScriptNum above that.
	// This is the same encoding SCRIPT_VERIFY_MINIMALDATA demands of these
	// bytes when the covenant is executed at spend time, which is why
	// consensus accepts nothing else -- a position encoded any other way
	// would be one the network refuses to relay a spend of.
	var multiplier int64
	mult := pushes[1]
	switch {
	case !mult.IsPush && mult.Opcode >= 0x51 && mult.Opcode <= 0x60: // OP_1 .. OP_16
		multiplier = int64(mult.Opcode - 0x50)
	case mult.IsPush && isMinimalPush(mult) && isMinimalScriptNum(mult.Data):
		multiplier, err = scriptNumToInt64(mult.Data)
		if err != nil {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if multiplier < 0 || multiplier > 2147483647 {
		return nil, nil
	}

	var origin []byte
	if isMint {
		// Mint: next element must be OP_MINT
		if pushes[2].IsPush || pushes[2].Opcode != opMint {
			return nil, nil
		}
		// Mints carry no origin
		origin = []byte{}
	} else {
		// Transfer: next element must be a 32-byte push (the origin)
		originElem := pushes[2]
		if !originElem.IsPush || !isMinimalPush(originElem) || len(originElem.Data) != 32 {
			return nil, nil
		}
		origin = make([]byte, 32)
		copy(origin, originElem.Data)

		// Then OP_MINT_TRANSFER
		if pushes[3].IsPush || pushes[3].Opcode != opMintTransfer {
			return nil, nil
		}
	}

	pubkey := make([]byte, len(pk.Data))
	copy(pubkey, pk.Data)

	return &ParsedUAP{PubKey: pubkey, Multiplier: multiplier, Origin: origin, IsMint: isMint}, nil
}

// ParseP2PKHScript returns the 20-byte hash160 from a standard
// pay-to-pubkey-hash scriptPubKey, or nil if the script is not P2PKH.
// A P2PKH scriptPubKey is exactly 25 bytes:
// OP_DUP OP_HASH160 <0x14> <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
// = 76 a9 14 <20 bytes> 88 ac
func ParseP2PKHScript(scriptHex string) []byte {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return nil
	}
	if len(raw) != 25 {
		return nil
	}
	// Check the opcodes: OP_DUP OP_HASH160 <0x14> <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
	if raw[0] != 0x76 || raw[1] != 0xa9 || raw[2] != 0x14 || raw[23] != 0x88 || raw[24] != 0xac {
		return nil
	}
	hash160 := make([]byte, 20)
	copy(hash160, raw[3:23])
	return hash160
}
