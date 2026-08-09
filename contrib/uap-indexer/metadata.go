package main

import (
	"encoding/hex"
)

// TokenMetadata holds the parsed metadata from an OP_RETURN output on a mint.
type TokenMetadata struct {
	Ticker       string `json:"ticker"`
	MetadataHash string `json:"metadata_hash,omitempty"` // hex, 32 bytes
}

// ParseMetadata extracts token metadata from an OP_RETURN scriptPubKey.
// Returns nil for any output that is not a well-formed WUAP metadata record:
// - an ordinary OP_RETURN (not WUAP)
// - wrong magic or version
// - truncated or lying length bytes
// - a ticker outside the 1-8 byte [A-Z0-9] charset
// - anything other than exactly four pushes
//
// Treat the payload as hostile (written by the minter); every field read must
// be bounds-checked before slicing, and no field is truncated silently.
func ParseMetadata(scriptHex string) *TokenMetadata {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return nil
	}

	// Must be at least: OP_RETURN + push(WUAP) + push(version byte) + ...
	if len(raw) < 2 {
		return nil
	}

	// Check for OP_RETURN opcode
	if raw[0] != 0x6a {
		return nil
	}

	pushes, err := readPushes(raw[1:])
	if err != nil {
		return nil
	}

	// Magic and version are read before the push count, because the push
	// count is version-specific: v1 had five pushes, v2 has four, and a
	// future v3 may have some other number. Checking the count first would
	// mean rejecting a v3 record as structurally malformed rather than as a
	// version this build does not know how to read -- the same nil today,
	// but the wrong reason to hang a diagnostic on later.
	if len(pushes) < 2 {
		return nil
	}

	// First push must be "WUAP" magic
	if !pushes[0].IsPush || len(pushes[0].Data) != 4 {
		return nil
	}
	if string(pushes[0].Data) != "WUAP" {
		return nil
	}

	// Second push must be the version byte. Version 0x01 was the retired
	// five-push form (magic, version, ticker, name, hash) and is rejected
	// explicitly rather than parsed: this format is 0x02 only.
	if !pushes[1].IsPush || len(pushes[1].Data) != 1 {
		return nil
	}
	version := pushes[1].Data[0]
	if version != 0x02 {
		return nil
	}

	// Exactly four pushes: magic, version, ticker, metadata_hash -- nothing
	// more, nothing less. A trailing push would let two different scripts
	// parse to the same metadata, and give a minter somewhere to hide bytes
	// that consumers never see.
	if len(pushes) != 4 {
		return nil
	}

	// Third push is ticker: 1-8 bytes, every byte in [A-Z0-9]. Strict, no
	// case folding here -- folding happens only in the wallet UI before the
	// script is built, so that a lowercase ticker on chain can never exist
	// as a second record rendering identically to its uppercase twin.
	if !pushes[2].IsPush {
		return nil
	}
	ticker := string(pushes[2].Data)
	if len(ticker) < 1 || len(ticker) > 8 {
		return nil
	}
	for i := 0; i < len(ticker); i++ {
		c := ticker[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')) {
			return nil
		}
	}

	// Fourth push is metadata_hash (must be 0 or 32 bytes).
	if !pushes[3].IsPush {
		return nil
	}
	hashLen := len(pushes[3].Data)
	if hashLen != 0 && hashLen != 32 {
		return nil
	}

	var metadataHash string
	if hashLen == 32 {
		metadataHash = hex.EncodeToString(pushes[3].Data)
	}

	return &TokenMetadata{
		Ticker:       ticker,
		MetadataHash: metadataHash,
	}
}
