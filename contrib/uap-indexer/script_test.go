package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"testing"
)

// buildPush encodes a single data push using minimal-push rules matching
// Bitcoin script (and Whippet's own CScript::operator<<).
func buildPush(data []byte) []byte {
	n := len(data)
	switch {
	case n == 0:
		return []byte{0x00}
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

// buildMultiplier encodes a multiplier the way a UAP output must: OP_0 for
// zero, OP_1..OP_16 for 1..16, and a minimal data push above that. This is
// exactly what CScript::operator<<(int64_t) emits, and exactly what
// SCRIPT_VERIFY_MINIMALDATA demands of the same bytes at execution time.
func buildMultiplier(v int64) []byte {
	if v >= 1 && v <= 16 {
		return []byte{byte(0x50 + v)}
	}
	return buildPush(buildScriptNum(v))
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
	out = append(out, buildMultiplier(multiplier)...)
	out = append(out, buildPush(salt)...)
	out = append(out, opMint)
	return out
}

func transferScriptBytes(pubkey []byte, multiplier int64) []byte {
	var out []byte
	out = append(out, buildPush(pubkey)...)
	out = append(out, buildMultiplier(multiplier)...)
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
	// A multiplier in 1..16 must be written with the small-integer opcode
	// OP_1..OP_16. That is the encoding SCRIPT_VERIFY_MINIMALDATA requires
	// when the covenant is executed at spend time, and consensus
	// (ParseUapOutputScript) accepts only that same canonical form -- so
	// there is exactly one byte encoding per position, and every position
	// consensus honours can be spent by a standard transaction.
	pk := fakePubKey(0x02)
	for _, mult := range []int64{1, 10, 16} {
		var smallInt []byte
		smallInt = append(smallInt, buildPush(pk)...)
		smallInt = append(smallInt, byte(0x50+mult))
		smallInt = append(smallInt, opMintTransfer)

		parsed, err := ParseUAPScript(hex.EncodeToString(smallInt))
		if err != nil || parsed == nil {
			t.Fatalf("multiplier %d: OP_%d encoding must parse, got %v, err=%v", mult, mult, parsed, err)
		}
		if parsed.Multiplier != mult {
			t.Errorf("multiplier = %d, want %d", parsed.Multiplier, mult)
		}

		// The same value as an explicit one-byte data push is NOT canonical,
		// so it is not a UAP output. Indexing it would advertise a position
		// consensus does not enforce.
		var dataPush []byte
		dataPush = append(dataPush, buildPush(pk)...)
		dataPush = append(dataPush, 0x01, byte(mult))
		dataPush = append(dataPush, opMintTransfer)

		parsed, err = ParseUAPScript(hex.EncodeToString(dataPush))
		if err != nil {
			t.Fatalf("multiplier %d: unexpected error: %v", mult, err)
		}
		if parsed != nil {
			t.Errorf("multiplier %d: non-canonical data push must not parse, got %+v", mult, parsed)
		}
	}
}

// Every element of a UAP output must use its shortest encoding. Anything
// else is a script the node would refuse to relay a spend of, so the
// indexer must not report it as a tradeable position.
func TestParseRejectsNonCanonicalEncodings(t *testing.T) {
	pk := fakePubKey(0x02)
	salt := []byte("0123456789abcdef") // 16 bytes

	cases := []struct {
		name   string
		script []byte
	}{
		{"pubkey via OP_PUSHDATA1", concat([]byte{0x4c, 0x21}, pk, buildMultiplier(1000), buildPush(salt), []byte{opMint})},
		{"salt via OP_PUSHDATA1", concat(buildPush(pk), buildMultiplier(1000), []byte{0x4c, 0x10}, salt, []byte{opMint})},
		{"multiplier via OP_PUSHDATA1", concat(buildPush(pk), []byte{0x4c, 0x02, 0xe8, 0x03}, []byte{opMintTransfer})},
		{"multiplier 0 as a one-byte push", concat(buildPush(pk), []byte{0x01, 0x00}, []byte{opMintTransfer})},
		{"non-minimal CScriptNum (trailing zero)", concat(buildPush(pk), []byte{0x03, 0xe8, 0x03, 0x00}, []byte{opMintTransfer})},
		{"multiplier as OP_1NEGATE", concat(buildPush(pk), []byte{0x4f}, []byte{opMintTransfer})},
	}
	for _, c := range cases {
		parsed, err := ParseUAPScript(hex.EncodeToString(c.script))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if parsed != nil {
			t.Errorf("%s: must not parse, got %+v", c.name, parsed)
		}
	}

	// Guard: the canonical form of the same position does parse, so the
	// cases above are failing for their stated reason and not by accident.
	parsed, err := ParseUAPScript(hex.EncodeToString(mintScriptBytes(pk, 1000, salt)))
	if err != nil || parsed == nil {
		t.Fatalf("canonical mint must parse, got %v, err=%v", parsed, err)
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
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

// TestParseP2PKHScript verifies that ParseP2PKHScript correctly identifies
// valid P2PKH scripts and rejects invalid ones.
func TestParseP2PKHScript(t *testing.T) {
	// Build a valid P2PKH script: OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}
	validScript := []byte{0x76, 0xa9, 0x14}
	validScript = append(validScript, hash160...)
	validScript = append(validScript, 0x88, 0xac)

	// Test valid P2PKH script
	result := ParseP2PKHScript(hex.EncodeToString(validScript))
	if result == nil {
		t.Fatal("ParseP2PKHScript should parse a valid P2PKH script")
	}
	if len(result) != 20 {
		t.Errorf("expected 20-byte hash160, got %d bytes", len(result))
	}
	if !bytes.Equal(result, hash160) {
		t.Errorf("hash160 mismatch")
	}

	// Test with UAP mint script (should return nil)
	pk := fakePubKey(0x02)
	salt := []byte("0123456789abcdef")
	uapScript := mintScriptBytes(pk, 1000, salt)
	result = ParseP2PKHScript(hex.EncodeToString(uapScript))
	if result != nil {
		t.Error("ParseP2PKHScript should reject UAP mint script")
	}

	// Test with UAP transfer script (should return nil)
	uapTransfer := transferScriptBytes(pk, 1000)
	result = ParseP2PKHScript(hex.EncodeToString(uapTransfer))
	if result != nil {
		t.Error("ParseP2PKHScript should reject UAP transfer script")
	}

	// Test with P2SH script (should return nil)
	p2shScript := []byte{0xa9, 0x14}
	p2shScript = append(p2shScript, make([]byte, 20)...)
	p2shScript = append(p2shScript, 0x87)
	result = ParseP2PKHScript(hex.EncodeToString(p2shScript))
	if result != nil {
		t.Error("ParseP2PKHScript should reject P2SH script")
	}

	// Test with truncated 24-byte script (should return nil)
	truncated := []byte{0x76, 0xa9, 0x14}
	truncated = append(truncated, make([]byte, 19)...)
	truncated = append(truncated, 0x88, 0xac)
	result = ParseP2PKHScript(hex.EncodeToString(truncated))
	if result != nil {
		t.Error("ParseP2PKHScript should reject 24-byte truncated script")
	}

	// Test with 25-byte script with one wrong opcode (should return nil)
	wrongOpcode := []byte{0x76, 0xa9, 0x14}
	wrongOpcode = append(wrongOpcode, make([]byte, 20)...)
	wrongOpcode = append(wrongOpcode, 0x89, 0xac) // 0x89 instead of 0x88
	result = ParseP2PKHScript(hex.EncodeToString(wrongOpcode))
	if result != nil {
		t.Error("ParseP2PKHScript should reject script with wrong opcode")
	}
}

// TestAddressRoundTrip verifies that EncodeAddress and DecodeAddress are
// inverses of each other for many hash160 values.
func TestAddressRoundTrip(t *testing.T) {
	// Test with various hash160 values, including leading zeros
	testCases := []struct {
		name    string
		version byte
		hash160 []byte
	}{
		{"normal", 73, bytes.Repeat([]byte{0x01}, 20)},
		{"leading zero", 73, append([]byte{0x00}, bytes.Repeat([]byte{0x01}, 19)...)},
		{"multiple leading zeros", 73, append(append([]byte{0x00}, []byte{0x00}...), bytes.Repeat([]byte{0x01}, 18)...)},
		{"all zeros", 73, make([]byte, 20)},
		{"testnet", 113, bytes.Repeat([]byte{0xaa}, 20)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			addr := EncodeAddress(tc.hash160, tc.version)
			if addr == "" {
				t.Fatal("EncodeAddress should not return empty string")
			}

			decodedVersion, decodedHash160, err := DecodeAddress(addr)
			if err != nil {
				t.Fatalf("DecodeAddress failed: %v", err)
			}

			if decodedVersion != tc.version {
				t.Errorf("version mismatch: got %d, want %d", decodedVersion, tc.version)
			}
			if !bytes.Equal(decodedHash160, tc.hash160) {
				t.Errorf("hash160 mismatch: got %x, want %x", decodedHash160, tc.hash160)
			}
		})
	}
}

// TestDecodeAddressRejectsCorruptedChecksum verifies that a corrupted checksum
// is detected and rejected.
func TestDecodeAddressRejectsCorruptedChecksum(t *testing.T) {
	hash160 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hash160[i] = byte(i + 1)
	}
	addr := EncodeAddress(hash160, 73)

	// Corrupt one character in the middle
	addrRunes := []rune(addr)
	// Find a character to corrupt (skip if it's '1' to make corruption obvious)
	for i := len(addrRunes) / 2; i < len(addrRunes); i++ {
		if addrRunes[i] != '1' {
			if addrRunes[i] == '2' {
				addrRunes[i] = '3'
			} else {
				addrRunes[i] = '2'
			}
			break
		}
	}
	corruptedAddr := string(addrRunes)

	_, _, err := DecodeAddress(corruptedAddr)
	if err == nil {
		t.Error("DecodeAddress should reject an address with corrupted checksum")
	}
}

// TestDecodeAddressRejectsNonBase58Character verifies that non-base58 characters
// are rejected.
func TestDecodeAddressRejectsNonBase58Character(t *testing.T) {
	// Base58 alphabet excludes 0, O, I, l
	invalidChars := []rune{'0', 'O', 'I', 'l', '!', '@', '#'}

	for _, ch := range invalidChars {
		invalidAddr := "1" + string(ch) + "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
		_, _, err := DecodeAddress(invalidAddr)
		if err == nil {
			t.Errorf("DecodeAddress should reject address with invalid character '%c'", ch)
		}
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

func TestSharedFixtures(t *testing.T) {
	// Load the shared fixture file: ../../src/test/data/uap_script_vectors.json
	fixtures, err := loadFixtures("../../src/test/data/uap_script_vectors.json")
	if err != nil {
		t.Fatalf("failed to load fixtures: %v", err)
	}

	for _, vec := range fixtures {
		t.Run(vec.Comment, func(t *testing.T) {
			parsed, err := ParseUAPScript(vec.Script)
			if err != nil && vec.Valid {
				t.Errorf("expected no error for valid script, got: %v", err)
				return
			}
			if vec.Valid {
				if parsed == nil {
					t.Error("expected a parsed result, got nil")
					return
				}
				if parsed.IsMint != vec.IsMint {
					t.Errorf("IsMint: got %v, want %v", parsed.IsMint, vec.IsMint)
				}
				if parsed.Multiplier != vec.Multiplier {
					t.Errorf("Multiplier: got %d, want %d", parsed.Multiplier, vec.Multiplier)
				}
				if hex.EncodeToString(parsed.PubKey) != vec.PubKey {
					t.Errorf("PubKey: got %s, want %s", hex.EncodeToString(parsed.PubKey), vec.PubKey)
				}
			} else {
				if parsed != nil {
					t.Errorf("expected no match for invalid script, got: %+v", parsed)
				}
			}
		})
	}
}

// scriptVectorFixture represents a single test vector from the fixture file.
type scriptVectorFixture struct {
	Comment    string `json:"comment"`
	Script     string `json:"script"`
	Valid      bool   `json:"valid"`
	IsMint     bool   `json:"is_mint"`
	PubKey     string `json:"pubkey"`
	Multiplier int64  `json:"multiplier"`
}

// loadFixtures loads the shared fixture JSON file.
func loadFixtures(path string) ([]scriptVectorFixture, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fixtures []scriptVectorFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		return nil, err
	}
	return fixtures, nil
}

// base58Encode must preserve leading zero bytes as leading '1' characters.
// The address round-trip cases above cannot cover this: Whippet's version
// bytes (73, 113) are never zero, so a real address payload never begins
// with 0x00 and that branch is never reached through EncodeAddress. Test
// the primitive directly, so the property is pinned rather than incidental.
func TestBase58PreservesLeadingZeros(t *testing.T) {
	cases := [][]byte{
		{0x00},
		{0x00, 0x00},
		{0x00, 0x01, 0x02},
		{0x00, 0x00, 0xff},
		{0x01, 0x02, 0x03}, // control: no leading zero
	}
	for _, in := range cases {
		enc := base58Encode(in)
		wantOnes := 0
		for _, b := range in {
			if b != 0x00 {
				break
			}
			wantOnes++
		}
		gotOnes := 0
		for _, c := range enc {
			if c != '1' {
				break
			}
			gotOnes++
		}
		if gotOnes != wantOnes {
			t.Errorf("base58Encode(%x) = %q: %d leading '1's, want %d", in, enc, gotOnes, wantOnes)
		}
		if dec := base58Decode(enc); !bytes.Equal(dec, in) {
			t.Errorf("base58 round-trip: %x -> %q -> %x", in, enc, dec)
		}
	}
}

// Addresses produced by the real node must decode with this package's
// version byte and 20-byte hash160. These five were generated by
// `getnewaddress` on a regtest whippetd (regtest shares mainnet's version
// byte 73), so they pin the Go base58 implementation against the node's own.
func TestDecodeRealNodeAddresses(t *testing.T) {
	addrs := []string{
		"WbCheTMkQ5BNUK4bjERuXxfph8Tg2ALpeT",
		"WT7cF8YBXga8WT24t9t87Cg4PFe2W3QgJz",
		"WXKzgwa1NwFxJVK7gf1LMwU8m9ncrk65iA",
		"WSJURUdADkMTRnNiiNG5SBUm5Agmi7eP8d",
		"WhqwZj7VZVyPW16cocHEzQYrC5vJHT6WjK",
	}
	for _, a := range addrs {
		version, hash160, err := DecodeAddress(a)
		if err != nil {
			t.Errorf("DecodeAddress(%q): %v", a, err)
			continue
		}
		if version != MainnetVersion {
			t.Errorf("DecodeAddress(%q): version = %d, want %d", a, version, MainnetVersion)
		}
		if len(hash160) != 20 {
			t.Errorf("DecodeAddress(%q): hash160 is %d bytes, want 20", a, len(hash160))
		}
		if got := EncodeAddress(hash160, version); got != a {
			t.Errorf("re-encode mismatch: %q -> %q", a, got)
		}
	}
}

// TestParseMetadataHappyPath verifies valid metadata is parsed correctly.
func TestParseMetadataHappyPath(t *testing.T) {
	// Build a valid metadata OP_RETURN:
	// OP_RETURN + WUAP magic + version + ticker len + ticker + name len + name + hash160 len + hash160
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	script = append(script, buildPush([]byte("BTC"))...)
	script = append(script, buildPush([]byte("Bitcoin"))...)
	hashBytes := make([]byte, 32)
	for i := 0; i < 32; i++ {
		hashBytes[i] = byte(i)
	}
	script = append(script, buildPush(hashBytes)...)

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed == nil {
		t.Fatal("expected metadata parse, got nil")
	}
	if parsed.Ticker != "BTC" {
		t.Errorf("ticker mismatch: got %q, want %q", parsed.Ticker, "BTC")
	}
	if parsed.Name != "Bitcoin" {
		t.Errorf("name mismatch: got %q, want %q", parsed.Name, "Bitcoin")
	}
	if parsed.MetadataHash != hex.EncodeToString(hashBytes) {
		t.Errorf("metadata_hash mismatch")
	}
}

// TestParseMetadataWithoutHash verifies metadata_hash can be omitted (0 bytes).
func TestParseMetadataWithoutHash(t *testing.T) {
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	script = append(script, buildPush([]byte("DOG"))...)
	script = append(script, buildPush([]byte("Dogecoin"))...)
	script = append(script, buildPush([]byte{})...) // empty hash

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed == nil {
		t.Fatal("expected metadata parse, got nil")
	}
	if parsed.Ticker != "DOG" {
		t.Errorf("ticker mismatch: got %q, want %q", parsed.Ticker, "DOG")
	}
	if parsed.Name != "Dogecoin" {
		t.Errorf("name mismatch: got %q, want %q", parsed.Name, "Dogecoin")
	}
	if parsed.MetadataHash != "" {
		t.Errorf("metadata_hash should be empty, got %q", parsed.MetadataHash)
	}
}

// TestParseMetadataRejectsNonOpReturn rejects non-OP_RETURN scripts.
func TestParseMetadataRejectsNonOpReturn(t *testing.T) {
	// A regular P2PKH script
	script := []byte{0x76, 0xa9, 0x14}
	script = append(script, make([]byte, 20)...)
	script = append(script, 0x88, 0xac)

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for non-OP_RETURN script")
	}
}

// TestParseMetadataRejectsWrongMagic rejects non-WUAP OP_RETURN outputs.
func TestParseMetadataRejectsWrongMagic(t *testing.T) {
	var script []byte
	script = append(script, 0x6a)                         // OP_RETURN
	script = append(script, buildPush([]byte("XXXX"))...) // wrong magic

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for wrong magic")
	}
}

// TestParseMetadataRejectsTruncatedPayload rejects truncated length bytes.
func TestParseMetadataRejectsTruncatedPayload(t *testing.T) {
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	// Missing ticker and rest of payload

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for truncated payload")
	}
}

// TestParseMetadataRejectsInvalidUTF8 rejects non-UTF8 fields.
func TestParseMetadataRejectsInvalidUTF8(t *testing.T) {
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	// Invalid UTF-8 ticker
	invalidUTF8 := []byte{0xff, 0xfe}
	script = append(script, buildPush(invalidUTF8)...)

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for invalid UTF-8")
	}
}

// TestParseMetadataRejectsControlCharacters rejects control chars in ticker/name.
func TestParseMetadataRejectsControlCharacters(t *testing.T) {
	cases := []byte{
		0x00, // NUL
		0x0a, // LF
		0x0d, // CR
		0x1f, // US
		0x7f, // DEL
	}
	for _, ctrl := range cases {
		var script []byte
		script = append(script, 0x6a) // OP_RETURN
		script = append(script, buildPush([]byte("WUAP"))...)
		script = append(script, buildPush([]byte{0x01})...) // version
		// Ticker with control character
		script = append(script, buildPush([]byte{'B', 'T', ctrl, 'C'})...)

		parsed := ParseMetadata(hex.EncodeToString(script))
		if parsed != nil {
			t.Errorf("expected nil for ticker with control char 0x%02x", ctrl)
		}
	}
}

// TestParseMetadataRejectsOversizedTicket rejects ticker > 16 bytes.
func TestParseMetadataRejectsOversizedTicket(t *testing.T) {
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	// 17-byte ticker (too long)
	script = append(script, buildPush([]byte("THISTICKERISTOLONG"))...)

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for oversized ticker")
	}
}

// TestParseMetadataRejectsOversizedName rejects name > 64 bytes.
func TestParseMetadataRejectsOversizedName(t *testing.T) {
	var script []byte
	script = append(script, 0x6a) // OP_RETURN
	script = append(script, buildPush([]byte("WUAP"))...)
	script = append(script, buildPush([]byte{0x01})...) // version
	script = append(script, buildPush([]byte("BTC"))...)
	// 65-byte name (too long)
	longName := make([]byte, 65)
	for i := 0; i < 65; i++ {
		longName[i] = 'A'
	}
	script = append(script, buildPush(longName)...)

	parsed := ParseMetadata(hex.EncodeToString(script))
	if parsed != nil {
		t.Error("expected nil for oversized name")
	}
}

// TestParseMetadataRejectsWrongHashLength rejects hash that is not 0 or 32 bytes.
func TestParseMetadataRejectsWrongHashLength(t *testing.T) {
	cases := []int{1, 16, 31, 33, 64}
	for _, hashLen := range cases {
		var script []byte
		script = append(script, 0x6a) // OP_RETURN
		script = append(script, buildPush([]byte("WUAP"))...)
		script = append(script, buildPush([]byte{0x01})...) // version
		script = append(script, buildPush([]byte("BTC"))...)
		script = append(script, buildPush([]byte("Bitcoin"))...)
		script = append(script, buildPush(make([]byte, hashLen))...)

		parsed := ParseMetadata(hex.EncodeToString(script))
		if parsed != nil {
			t.Errorf("expected nil for hash length %d", hashLen)
		}
	}
}

// Exactly five pushes, nothing more. A trailing push would mean two
// different on-chain scripts present as the same token metadata.
func TestParseMetadataRejectsTrailingPush(t *testing.T) {
	base := wuapPayload("WHIP", "Whippet Token", nil)
	if ParseMetadata(hex.EncodeToString(base)) == nil {
		t.Fatal("setup: the five-push form must parse")
	}
	withExtra := append(append([]byte{}, base...), buildPush([]byte("extra"))...)
	if md := ParseMetadata(hex.EncodeToString(withExtra)); md != nil {
		t.Errorf("a sixth push must not parse, got %+v", md)
	}
}
