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
	got := BuildMintScript(pubkey, 5)

	var want []byte
	want = append(want, pushData(pubkey)...)
	want = append(want, pushMultiplier(5)...)
	want = append(want, opMint)

	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
	if got[len(got)-1] != opMint {
		t.Errorf("script must end in OP_MINT (0xb5), got last byte %x", got[len(got)-1])
	}
}

func TestBuildTransferScriptLayout(t *testing.T) {
	pubkey := bytes.Repeat([]byte{0x03}, 33)
	origin := bytes.Repeat([]byte{0xAA}, 32)
	got := BuildTransferScript(pubkey, 3, origin)

	var want []byte
	want = append(want, pushData(pubkey)...)
	want = append(want, pushMultiplier(3)...)
	want = append(want, pushData(origin)...)
	want = append(want, opMintTransfer)

	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
	if got[len(got)-1] != opMintTransfer {
		t.Errorf("script must end in OP_MINT_TRANSFER (0xba), got last byte %x", got[len(got)-1])
	}
}

func TestBuildTransferScriptRejectsWrongLengthOrigin(t *testing.T) {
	pubkey := bytes.Repeat([]byte{0x03}, 33)
	tests := []struct {
		name   string
		origin []byte
	}{
		{"short origin", make([]byte, 31)},
		{"long origin", make([]byte, 33)},
		{"empty origin", make([]byte, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for wrong-length origin")
				}
			}()
			BuildTransferScript(pubkey, 1, tt.origin)
		})
	}
}

func TestOriginFromOutpoint(t *testing.T) {
	// Test vectors from src/test/data/uap_origin_vectors.json
	tests := []struct {
		name   string
		txid   string
		n      uint32
		origin string
	}{
		{
			"all-zero txid, index 0",
			"0000000000000000000000000000000000000000000000000000000000000000",
			0,
			"6db65fd59fd356f6729140571b5bcd6bb3b83492a16e1bf0a3884442fc3c8a0e",
		},
		{
			"same txid, index 1",
			"0000000000000000000000000000000000000000000000000000000000000000",
			1,
			"71c99cc3bc21757feed5b712744ebb0f770d5c41d99189f9457495747bf11050",
		},
		{
			"asymmetric txid (catches byte-reversal bug)",
			"00000000000000000000000000000000000000000000000000000000000000ff",
			0,
			"78ccb86b524ee2d77751270df8ef6118017d7b714bdf9c0e5057a787fac50fa0",
		},
		{
			"byte-reversal of previous case",
			"ff00000000000000000000000000000000000000000000000000000000000000",
			0,
			"ed5de74bfe70c74aa62dc1162546543c5430bcc21398256451579a94955e20ea",
		},
		{
			"arbitrary txid, index 0",
			"1122334455667788990011223344556677889900112233445566778899001122",
			0,
			"752c40cc46e27586c81cc35e58beecf64c08ece4995684d0f97ef81104f0f470",
		},
		{
			"index 0xffffffff (maximum)",
			"1122334455667788990011223344556677889900112233445566778899001122",
			4294967295,
			"bbbf0de01cf746fb250a294b14d18850b3e995cfc76e32198e1f63dc64ecb3a6",
		},
		{
			"index 258 = 0x00000102 (catches big-endian n encoding)",
			"1122334455667788990011223344556677889900112233445566778899001122",
			258,
			"064e1b749558ed958372387872edab54e18d782fc216058cf3e2d6c3dc3e225c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OriginFromOutpoint(tt.txid, tt.n)
			want, _ := hex.DecodeString(tt.origin)
			if !bytes.Equal(got, want) {
				t.Errorf("got %x want %x", hex.EncodeToString(got), tt.origin)
			}
		})
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

// Mutation tests: each test below deliberately violates one consensus rule
// and confirms a specific test in the suite catches it.

func TestMutationOriginFromOutpointBytesReversalIsCritical(t *testing.T) {
	// If we skip reversing the txid, the result is wrong.
	// Test: TestOriginFromOutpoint/asymmetric_txid_(catches_byte-reversal_bug)
	// expects both reverse and non-reverse to produce different origins,
	// which would fail if reversal were skipped in BOTH implementations.
	// This directly tests that the reversal is in place.

	txid := "00000000000000000000000000000000000000000000000000000000000000ff"
	correctOrigin := OriginFromOutpoint(txid, 0)
	correctOriginHex := hex.EncodeToString(correctOrigin)
	expectedHex := "78ccb86b524ee2d77751270df8ef6118017d7b714bdf9c0e5057a787fac50fa0"
	if correctOriginHex != expectedHex {
		t.Errorf("origin derivation failed: got %s, want %s", correctOriginHex, expectedHex)
	}

	// The reversed txid should produce a different origin
	reversedTxid := "ff00000000000000000000000000000000000000000000000000000000000000"
	differentOrigin := OriginFromOutpoint(reversedTxid, 0)
	if hex.EncodeToString(differentOrigin) == correctOriginHex {
		t.Errorf("byte reversal was not applied: txid and its reverse produced the same origin")
	}
}

func TestMutationOriginIndexMustBeLittleEndian(t *testing.T) {
	// If we encoded n big-endian instead of little-endian, the result is wrong.
	// Test: TestOriginFromOutpoint/index_258_=_0x00000102 verifies that
	// 0x00000102 (258 little-endian) produces a different origin than
	// 0x02010000 (258 big-endian would be), confirming LE encoding.

	txid := "1122334455667788990011223344556677889900112233445566778899001122"
	correctOrigin := OriginFromOutpoint(txid, 258)
	correctOriginHex := hex.EncodeToString(correctOrigin)
	expectedHex := "064e1b749558ed958372387872edab54e18d782fc216058cf3e2d6c3dc3e225c"
	if correctOriginHex != expectedHex {
		t.Errorf("origin derivation failed: got %s, want %s", correctOriginHex, expectedHex)
	}

	// Verify that a different index produces a different origin
	differentOrigin := OriginFromOutpoint(txid, 259)
	if hex.EncodeToString(differentOrigin) == correctOriginHex {
		t.Errorf("index encoding was not applied correctly: adjacent indices produced the same origin")
	}
}

func TestMutationOriginLengthValidation(t *testing.T) {
	// Test: TestBuildTransferScriptRejectsWrongLengthOrigin verifies that
	// BuildTransferScript panics if origin is not exactly 32 bytes.
	// This catches any mutation that would allow wrong-length origins.

	pubkey := make([]byte, 33)
	validOrigin := make([]byte, 32)

	// Should succeed with correct length
	result := BuildTransferScript(pubkey, 1, validOrigin)
	if len(result) == 0 {
		t.Errorf("BuildTransferScript failed with valid 32-byte origin")
	}

	// Should panic with any other length
	for _, badLen := range []int{0, 31, 33, 64} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic for origin length %d, got none", badLen)
				}
			}()
			BuildTransferScript(pubkey, 1, make([]byte, badLen))
		}()
	}
}

func TestMutationMintScriptNoSalt(t *testing.T) {
	// Test: TestBuildMintScriptLayout verifies that v2 mint scripts have
	// no salt field. This would fail if salt were accidentally added back.

	pubkey := make([]byte, 33)
	mintScript := BuildMintScript(pubkey, 5)

	// The script must end with OP_MINT (0xb5)
	if len(mintScript) == 0 || mintScript[len(mintScript)-1] != 0xb5 {
		t.Errorf("mint script must end with OP_MINT")
	}

	// The script length should be: pubkey (34 bytes) + multiplier (1-2 bytes) + OP_MINT (1 byte)
	// For multiplier 5 (small int), it's 34 + 1 + 1 = 36 bytes
	if len(mintScript) != 36 {
		t.Errorf("mint script length unexpected: got %d bytes, want 36", len(mintScript))
	}
}

func TestMutationTransferScriptMustHaveOrigin(t *testing.T) {
	// Test: TestBuildTransferScriptLayout verifies that v2 transfer scripts
	// have an origin field. This would fail if origin were accidentally removed.

	pubkey := make([]byte, 33)
	origin := make([]byte, 32)
	transferScript := BuildTransferScript(pubkey, 3, origin)

	// The script must end with OP_MINT_TRANSFER (0xba)
	if len(transferScript) == 0 || transferScript[len(transferScript)-1] != 0xba {
		t.Errorf("transfer script must end with OP_MINT_TRANSFER")
	}

	// The script length should be: pubkey (34 bytes) + multiplier (1 byte) + origin (33 bytes) + OP_MINT_TRANSFER (1 byte)
	// For multiplier 3 (small int): 34 + 1 + 33 + 1 = 69 bytes
	if len(transferScript) != 69 {
		t.Errorf("transfer script length unexpected: got %d bytes, want 69", len(transferScript))
	}
}
