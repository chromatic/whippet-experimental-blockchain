package uaptx

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Opcodes, matching src/script/script.h.
const (
	opMint          = 0xb5
	opMintTransfer  = 0xba
	opPushData1     = 0x4c
	opPushData2     = 0x4d
	opPushData4     = 0x4e
	opCodeSeparator = 0xab
	op0             = 0x00
	op1             = 0x51 // OP_1..OP_16 are 0x51..0x60
)

// UAPOriginSize is the fixed size of a lineage origin: exactly 32 bytes (SHA256).
const UAPOriginSize = 32

// SIGHASH types.
const (
	SighashAll          = 1
	SighashNone         = 2
	SighashSingle       = 3
	SighashAnyoneCanPay = 0x80
)

// pushData encodes data as a minimal-push, matching
// CScript::operator<<(vector<uchar>).
func pushData(data []byte) []byte {
	n := len(data)
	switch {
	case n < opPushData1:
		out := make([]byte, 0, 1+n)
		out = append(out, byte(n))
		return append(out, data...)
	case n <= 0xff:
		out := make([]byte, 0, 2+n)
		out = append(out, opPushData1, byte(n))
		return append(out, data...)
	case n <= 0xffff:
		out := make([]byte, 0, 3+n)
		out = append(out, opPushData2, byte(n&0xff), byte((n>>8)&0xff))
		return append(out, data...)
	default:
		panic("uaptx: push too large for this helper")
	}
}

// scriptNumBytes encodes n as a minimal CScriptNum push, matching
// CScript::operator<<(int64_t).
func scriptNumBytes(n int64) []byte {
	if n == 0 {
		return []byte{}
	}
	neg := n < 0
	abs := n
	if neg {
		abs = -n
	}
	var out []byte
	for abs > 0 {
		out = append(out, byte(abs&0xff))
		abs >>= 8
	}
	if out[len(out)-1]&0x80 != 0 {
		if neg {
			out = append(out, 0x80)
		} else {
			out = append(out, 0x00)
		}
	} else if neg {
		out[len(out)-1] |= 0x80
	}
	return out
}

// pushMultiplier pushes a UAP multiplier in its canonical encoding: OP_0 for
// zero, OP_1..OP_16 for 1..16, and a minimal data push of a minimal
// CScriptNum above that. See uap.js's pushMultiplier for the consensus/
// standardness reasoning this must match exactly.
func pushMultiplier(n int64) []byte {
	if n < 0 || n > 2147483647 {
		panic(fmt.Sprintf("uaptx: multiplier must be an integer in [0, 2147483647], got %d", n))
	}
	if n == 0 {
		return []byte{op0}
	}
	if n <= 16 {
		return []byte{byte(op1 + (n - 1))}
	}
	return pushData(scriptNumBytes(n))
}

// OriginFromOutpoint derives a lineage origin from an outpoint:
//
//	origin = SHA256(txid_internal_bytes || n_little_endian)
//
// txid is the transaction ID in big-endian (display order); it is reversed
// to internal byte order before hashing. n is the output index.
func OriginFromOutpoint(txid string, n uint32) []byte {
	// Reverse the txid from big-endian to internal byte order
	txidBytes, err := hex.DecodeString(txid)
	if err != nil || len(txidBytes) != 32 {
		panic(fmt.Sprintf("uaptx: invalid txid %q: must be 32 bytes of hex, got err=%v len=%d", txid, err, len(txidBytes)))
	}
	txidLE := reverseBytes(txidBytes)

	// Encode n as 4 bytes, little-endian
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], n)

	// Hash txid || n
	h := sha256.Sum256(append(txidLE, buf[:]...))
	return h[:]
}

// BuildMintScript builds a fresh OP_MINT output script (v2 format):
//
//	<recipient_pubkey> <multiplier> OP_MINT
//
// The salt has been removed; lineage identity is derived from the
// outpoint when the mint is first spent.
func BuildMintScript(pubkey []byte, multiplier int64) []byte {
	var out []byte
	out = append(out, pushData(pubkey)...)
	out = append(out, pushMultiplier(multiplier)...)
	out = append(out, opMint)
	return out
}

// BuildTransferScript builds an OP_MINT_TRANSFER covenant output script (v2 format):
//
//	<recipient_pubkey> <multiplier> <origin32> OP_MINT_TRANSFER
//
// origin must be exactly 32 bytes and represents the lineage identity.
func BuildTransferScript(pubkey []byte, multiplier int64, origin []byte) []byte {
	if len(origin) != UAPOriginSize {
		panic(fmt.Sprintf("uaptx: origin must be exactly %d bytes, got %d", UAPOriginSize, len(origin)))
	}
	var out []byte
	out = append(out, pushData(pubkey)...)
	out = append(out, pushMultiplier(multiplier)...)
	out = append(out, pushData(origin)...)
	out = append(out, opMintTransfer)
	return out
}

// step is one instruction of a CScript::GetOp2-style walk.
type step struct {
	ok     bool
	pos    int
	opcode int
}

// nextOp advances one instruction, mirroring CScript::GetOp2 in
// src/script/script.h. On failure pos still reports how far the walk got,
// because serializeScriptCode writes up to exactly that point.
func nextOp(script []byte, pc int) step {
	if pc >= len(script) {
		return step{ok: false, pos: pc, opcode: -1}
	}
	opcode := int(script[pc])
	pc++
	if opcode <= opPushData4 {
		size := opcode
		switch opcode {
		case opPushData1:
			if len(script)-pc < 1 {
				return step{ok: false, pos: pc, opcode: opcode}
			}
			size = int(script[pc])
			pc++
		case opPushData2:
			if len(script)-pc < 2 {
				return step{ok: false, pos: pc, opcode: opcode}
			}
			size = int(script[pc]) | int(script[pc+1])<<8
			pc += 2
		case opPushData4:
			if len(script)-pc < 4 {
				return step{ok: false, pos: pc, opcode: opcode}
			}
			size = int(script[pc]) | int(script[pc+1])<<8 | int(script[pc+2])<<16 | int(script[pc+3])<<24
			pc += 4
		}
		if len(script)-pc < size {
			return step{ok: false, pos: pc, opcode: opcode}
		}
		pc += size
	}
	return step{ok: true, pos: pc, opcode: opcode}
}

// scriptCodeResult is the output of serializeScriptCode: the bytes to write
// for a scriptCode with every OP_CODESEPARATOR removed, and the length
// prefix to declare ahead of them (which can differ from len(bytes) -- see
// serializeScriptCode's doc comment).
type scriptCodeResult struct {
	bytes          []byte
	declaredLength int
}

// serializeScriptCode serializes a scriptCode for signing, matching
// CTransactionSignatureSerializer::SerializeScriptCode in
// src/script/interpreter.cpp: every OP_CODESEPARATOR is dropped from the
// signed preimage.
//
// The node computes the length prefix and the emitted bytes by two
// different routes -- the prefix from scriptCode.size() minus the separator
// count, the bytes from an opcode walk. For any well-formed script the two
// agree. They diverge only when the script ends mid-push, where the walk
// stops early and the node deliberately writes a length longer than what
// follows it.
func serializeScriptCode(scriptCode []byte) scriptCodeResult {
	var runs [][]byte
	separators := 0
	begin := 0
	pc := 0
	for {
		s := nextOp(scriptCode, pc)
		if !s.ok {
			pc = s.pos
			break
		}
		pc = s.pos
		if s.opcode == opCodeSeparator {
			runs = append(runs, scriptCode[begin:pc-1])
			begin = pc
			separators++
		}
	}
	if begin != len(scriptCode) {
		runs = append(runs, scriptCode[begin:pc])
	}
	var bytesOut []byte
	for _, r := range runs {
		bytesOut = append(bytesOut, r...)
	}
	if bytesOut == nil {
		bytesOut = []byte{}
	}
	return scriptCodeResult{bytes: bytesOut, declaredLength: len(scriptCode) - separators}
}
