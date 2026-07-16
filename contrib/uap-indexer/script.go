package main

import (
	"encoding/hex"
	"fmt"
)

// UAP opcodes, must match src/script/script.h.
const (
	opMint         = 0xb5
	opMintTransfer = 0xba
)

// ParsedUAP is the decoded form of a UAP mint or transfer output script.
// This is a best-effort mirror of ParseUapOutputScript in
// src/script/interpreter.cpp -- it exists purely for observational
// indexing and is not consensus-authoritative. If the on-chain opcode
// logic changes, this must be updated to match or the index will silently
// drift from what the chain actually enforces.
type ParsedUAP struct {
	PubKey     []byte
	Multiplier int64
	IsMint     bool // true: OP_MINT (fresh mint); false: OP_MINT_TRANSFER (covenant)
}

// scriptPush is one decoded element of a script: either pushed data, or a
// non-push opcode (Data is nil, Opcode holds the raw byte).
type scriptPush struct {
	IsPush bool
	Opcode byte
	Data   []byte
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
			out = append(out, scriptPush{IsPush: true, Data: script[i : i+int(b)]})
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
			out = append(out, scriptPush{IsPush: true, Data: script[i : i+n]})
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
			out = append(out, scriptPush{IsPush: true, Data: script[i : i+n]})
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
			out = append(out, scriptPush{IsPush: true, Data: script[i : i+n]})
			i += n
		case b == 0x00: // OP_0: minimal encoding of the number 0
			out = append(out, scriptPush{IsPush: true, Data: []byte{}})
		case b >= 0x51 && b <= 0x60: // OP_1..OP_16: minimal small-int push
			out = append(out, scriptPush{IsPush: true, Data: []byte{b - 0x50}})
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
//	<pubkey> <multiplier> <salt> OP_MINT           (fresh mint)
//	<pubkey> <multiplier> OP_MINT_TRANSFER         (transfer/covenant)
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

	wantLen := 3 // pubkey, multiplier, OP_MINT_TRANSFER
	if isMint {
		wantLen = 4 // pubkey, multiplier, salt, OP_MINT
	}
	if len(pushes) != wantLen {
		return nil, nil
	}

	pk := pushes[0]
	if !pk.IsPush || (len(pk.Data) != 33 && len(pk.Data) != 65) {
		return nil, nil
	}

	mult := pushes[1]
	if !mult.IsPush {
		return nil, nil
	}
	multiplier, err := scriptNumToInt64(mult.Data)
	if err != nil || multiplier < 0 {
		return nil, nil
	}

	if isMint {
		salt := pushes[2]
		if !salt.IsPush || len(salt.Data) < 16 {
			return nil, nil
		}
		if pushes[3].IsPush || pushes[3].Opcode != opMint {
			return nil, nil
		}
	} else {
		if pushes[2].IsPush || pushes[2].Opcode != opMintTransfer {
			return nil, nil
		}
	}

	pubkey := make([]byte, len(pk.Data))
	copy(pubkey, pk.Data)

	return &ParsedUAP{PubKey: pubkey, Multiplier: multiplier, IsMint: isMint}, nil
}
