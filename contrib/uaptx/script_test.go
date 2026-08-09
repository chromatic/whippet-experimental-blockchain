package uaptx

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestPushDataSmall(t *testing.T) {
	got := pushData([]byte{0x01, 0x02, 0x03})
	want := []byte{0x03, 0x01, 0x02, 0x03}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

func TestPushDataPushData1(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 0x4c) // 76 bytes, >= OP_PUSHDATA1 threshold
	got := pushData(data)
	if got[0] != opPushData1 || got[1] != 0x4c {
		t.Fatalf("expected PUSHDATA1 76, got %x", got[:2])
	}
	if !bytes.Equal(got[2:], data) {
		t.Errorf("payload mismatch")
	}
}

func TestPushDataPushData2(t *testing.T) {
	data := bytes.Repeat([]byte{0xCD}, 0x100) // 256 bytes
	got := pushData(data)
	if got[0] != opPushData2 || got[1] != 0x00 || got[2] != 0x01 {
		t.Fatalf("expected PUSHDATA2 LE(256), got %x", got[:3])
	}
}

func TestScriptNumBytesZero(t *testing.T) {
	if got := scriptNumBytes(0); len(got) != 0 {
		t.Errorf("expected empty encoding for 0, got %x", got)
	}
}

func TestScriptNumBytesPositiveNoSignExtension(t *testing.T) {
	// 17 = 0x11, top bit clear, fits in one byte
	got := scriptNumBytes(17)
	want := []byte{0x11}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

func TestScriptNumBytesPositiveNeedsSignExtensionByte(t *testing.T) {
	// 128 = 0x80, top bit set, needs an extra 0x00 byte to stay positive
	got := scriptNumBytes(128)
	want := []byte{0x80, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

func TestScriptNumBytesNegative(t *testing.T) {
	got := scriptNumBytes(-17)
	want := []byte{0x91} // 0x11 | 0x80
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

func TestScriptNumBytesNegativeNeedsSignExtensionByte(t *testing.T) {
	got := scriptNumBytes(-128)
	want := []byte{0x80, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

func TestPushMultiplierZero(t *testing.T) {
	got := pushMultiplier(0)
	if !bytes.Equal(got, []byte{op0}) {
		t.Errorf("got %x want OP_0", got)
	}
}

func TestPushMultiplierOneToSixteen(t *testing.T) {
	for n := int64(1); n <= 16; n++ {
		got := pushMultiplier(n)
		want := []byte{byte(op1 + n - 1)}
		if !bytes.Equal(got, want) {
			t.Errorf("multiplier %d: got %x want %x", n, got, want)
		}
	}
}

func TestPushMultiplierAboveSixteenUsesDataPush(t *testing.T) {
	got := pushMultiplier(17)
	want := pushData(scriptNumBytes(17))
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
	// And it must NOT be an OP_N opcode.
	if len(got) == 1 && got[0] >= op1 && got[0] <= op1+15 {
		t.Errorf("17 must not encode as an OP_N opcode, got %x", got)
	}
}

func TestPushMultiplierRejectsOutOfRange(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic for negative multiplier")
		}
	}()
	pushMultiplier(-1)
}

func TestBuildMintScriptLayout(t *testing.T) {
	pubkey := bytes.Repeat([]byte{0x02}, 33)
	salt := bytes.Repeat([]byte{0x11}, 16)
	got := BuildMintScript(pubkey, 5, salt)

	var want []byte
	want = append(want, pushData(pubkey)...)
	want = append(want, pushMultiplier(5)...)
	want = append(want, pushData(salt)...)
	want = append(want, opMint)

	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
	if got[len(got)-1] != opMint {
		t.Errorf("script must end in OP_MINT (0xb5), got last byte %x", got[len(got)-1])
	}
}

func TestBuildMintScriptRejectsShortSalt(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic for salt < 16 bytes")
		}
	}()
	BuildMintScript([]byte{0x02}, 1, make([]byte, 15))
}

func TestBuildTransferScriptLayout(t *testing.T) {
	pubkey := bytes.Repeat([]byte{0x03}, 33)
	got := BuildTransferScript(pubkey, 3)

	var want []byte
	want = append(want, pushData(pubkey)...)
	want = append(want, pushMultiplier(3)...)
	want = append(want, opMintTransfer)

	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
	if got[len(got)-1] != opMintTransfer {
		t.Errorf("script must end in OP_MINT_TRANSFER (0xba), got last byte %x", got[len(got)-1])
	}
}

func TestNextOpSimpleOpcode(t *testing.T) {
	s := nextOp([]byte{0x51, 0xab}, 0)
	if !s.ok || s.pos != 1 || s.opcode != 0x51 {
		t.Errorf("got %+v", s)
	}
}

func TestNextOpPush(t *testing.T) {
	script, _ := hex.DecodeString("03aabbcc51")
	s := nextOp(script, 0)
	if !s.ok || s.pos != 4 || s.opcode != 0x03 {
		t.Errorf("got %+v", s)
	}
}

func TestNextOpTruncatedPushFails(t *testing.T) {
	script, _ := hex.DecodeString("05aabb") // claims 5 bytes, only 2 follow
	s := nextOp(script, 0)
	if s.ok {
		t.Errorf("expected truncated push to fail, got %+v", s)
	}
	if s.pos != 1 {
		t.Errorf("expected pos to stop right after the opcode (1), got %d", s.pos)
	}
}

func TestSerializeScriptCodeStripsCodeSeparator(t *testing.T) {
	script, _ := hex.DecodeString("51ab52")
	res := serializeScriptCode(script)
	want, _ := hex.DecodeString("5152")
	if !bytes.Equal(res.bytes, want) {
		t.Errorf("got %x want %x", res.bytes, want)
	}
	if res.declaredLength != len(want) {
		t.Errorf("declaredLength = %d, want %d", res.declaredLength, len(want))
	}
}

func TestSerializeScriptCodeNoSeparator(t *testing.T) {
	script, _ := hex.DecodeString("5152")
	res := serializeScriptCode(script)
	if !bytes.Equal(res.bytes, script) {
		t.Errorf("got %x want %x", res.bytes, script)
	}
	if res.declaredLength != len(script) {
		t.Errorf("declaredLength = %d, want %d", res.declaredLength, len(script))
	}
}

func TestSerializeScriptCodeTruncatedPushDeclaredLengthExceedsBytes(t *testing.T) {
	// "05aabb" claims a 5-byte push but only 2 bytes follow -- the walk
	// stops at pos=1 (right after the length opcode), so bytes emitted is
	// just that one opcode byte, but declaredLength is the full script
	// length (no separators seen).
	script, _ := hex.DecodeString("05aabb")
	res := serializeScriptCode(script)
	if len(res.bytes) != 1 {
		t.Errorf("expected the walk to stop after the opcode byte, got %d bytes: %x", len(res.bytes), res.bytes)
	}
	if res.declaredLength != len(script) {
		t.Errorf("declaredLength = %d, want %d (full script, no separators)", res.declaredLength, len(script))
	}
}
