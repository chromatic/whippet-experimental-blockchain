package main

import (
	"encoding/hex"
	"testing"
)

// buildPush encodes a single data push using minimal-push rules matching
// Bitcoin script (and Whippet's own CScript::operator<<).
func buildPush(data []byte) []byte {
	n := len(data)
	switch {
	case n == 0:
		return []byte{0x00}
	case n == 1 && data[0] >= 1 && data[0] <= 16:
		return []byte{0x50 + data[0]}
	case n <= 0x4b:
		return append([]byte{byte(n)}, data...)
	case n <= 0xff:
		return append([]byte{0x4c, byte(n)}, data...)
	default:
		panic("test helper doesn't support pushes this large")
	}
}

// buildScriptNum encodes an int64 the way CScriptNum::getvch() would.
func buildScriptNum(v int64) []byte {
	if v == 0 {
		return nil
	}
	neg := v < 0
	abs := v
	if neg {
		abs = -abs
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

func fakePubKey(prefix byte) []byte {
	pk := make([]byte, 33)
	pk[0] = prefix
	for i := 1; i < 33; i++ {
		pk[i] = byte(i)
	}
	return pk
}

func mintScriptBytes(pubkey []byte, multiplier int64, salt []byte) []byte {
	var out []byte
	out = append(out, buildPush(pubkey)...)
	out = append(out, buildPush(buildScriptNum(multiplier))...)
	out = append(out, buildPush(salt)...)
	out = append(out, opMint)
	return out
}

func transferScriptBytes(pubkey []byte, multiplier int64) []byte {
	var out []byte
	out = append(out, buildPush(pubkey)...)
	out = append(out, buildPush(buildScriptNum(multiplier))...)
	out = append(out, opMintTransfer)
	return out
}

func TestParseMintScript(t *testing.T) {
	pk := fakePubKey(0x02)
	salt := []byte("0123456789abcdef") // 16 bytes
	script := mintScriptBytes(pk, 1000, salt)

	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected a match, got nil")
	}
	if !parsed.IsMint {
		t.Error("expected IsMint = true")
	}
	if parsed.Multiplier != 1000 {
		t.Errorf("multiplier = %d, want 1000", parsed.Multiplier)
	}
	if hex.EncodeToString(parsed.PubKey) != hex.EncodeToString(pk) {
		t.Errorf("pubkey mismatch")
	}
}

func TestParseTransferScript(t *testing.T) {
	pk := fakePubKey(0x03)
	script := transferScriptBytes(pk, 42)

	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected a match, got nil")
	}
	if parsed.IsMint {
		t.Error("expected IsMint = false")
	}
	if parsed.Multiplier != 42 {
		t.Errorf("multiplier = %d, want 42", parsed.Multiplier)
	}
}

func TestParseSmallIntMultiplier(t *testing.T) {
	// multiplier=10 encodes as OP_10 (a single opcode byte, not a
	// generic data push) under minimal-push rules -- make sure we
	// decode that form correctly too.
	pk := fakePubKey(0x02)
	script := transferScriptBytes(pk, 10)
	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil || parsed == nil {
		t.Fatalf("expected a match, got %v, err=%v", parsed, err)
	}
	if parsed.Multiplier != 10 {
		t.Errorf("multiplier = %d, want 10", parsed.Multiplier)
	}
}

func TestParseRejectsShortSalt(t *testing.T) {
	pk := fakePubKey(0x02)
	script := mintScriptBytes(pk, 1000, []byte("tooshort"))
	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed != nil {
		t.Fatal("expected no match for a salt under 16 bytes")
	}
}

func TestParseRejectsNegativeMultiplier(t *testing.T) {
	pk := fakePubKey(0x02)
	script := transferScriptBytes(pk, -5)
	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed != nil {
		t.Fatal("expected no match for a negative multiplier")
	}
}

func TestParseIgnoresOrdinaryScripts(t *testing.T) {
	// A plain P2PKH-shaped script; must not be mistaken for a UAP output.
	script := []byte{0x76, 0xa9, 0x14}
	script = append(script, make([]byte, 20)...)
	script = append(script, 0x88, 0xac)

	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed != nil {
		t.Fatal("expected no match for a non-UAP script")
	}
}

func TestParseRejectsTruncatedScript(t *testing.T) {
	// A push claiming more bytes than are actually present, ending in
	// OP_MINT so it reaches the push-decoding path (readPushes) rather
	// than being filtered out earlier by the last-byte check.
	script := []byte{0x21, 0x01, 0x02, opMint} // claims a 33-byte push, only has 2
	parsed, err := ParseUAPScript(hex.EncodeToString(script))
	if err != nil {
		t.Fatalf("expected graceful nil, not an error: %v", err)
	}
	if parsed != nil {
		t.Fatal("expected no match for a truncated/malformed script")
	}
}

func TestParseInvalidHex(t *testing.T) {
	_, err := ParseUAPScript("not-hex")
	if err == nil {
		t.Fatal("expected an error for invalid hex input")
	}
}
