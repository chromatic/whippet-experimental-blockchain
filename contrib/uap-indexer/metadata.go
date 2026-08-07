package main

import (
	"encoding/hex"
	"unicode/utf8"
)

// TokenMetadata holds the parsed metadata from an OP_RETURN output on a mint.
type TokenMetadata struct {
	Ticker       string `json:"ticker"`
	Name         string `json:"name"`
	MetadataHash string `json:"metadata_hash,omitempty"` // hex, 32 bytes
}

// ParseMetadata extracts token metadata from an OP_RETURN scriptPubKey.
// Returns nil for any output that is not a well-formed WUAP metadata record:
// - an ordinary OP_RETURN (not WUAP)
// - wrong magic or version
// - truncated or lying length bytes
// - fields that violate length bounds or UTF-8 validity
// - control characters in ticker or name
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

	// Parse the pushes in the script
	pushes, err := readPushes(raw[1:])
	if err != nil || len(pushes) < 3 {
		return nil
	}

	// First push must be "WUAP" magic
	if !pushes[0].IsPush || len(pushes[0].Data) != 4 {
		return nil
	}
	if string(pushes[0].Data) != "WUAP" {
		return nil
	}

	// Second push must be version byte (currently 0x01)
	if !pushes[1].IsPush || len(pushes[1].Data) != 1 {
		return nil
	}
	version := pushes[1].Data[0]
	if version != 0x01 {
		return nil
	}

	// Third push is ticker
	if !pushes[2].IsPush {
		return nil
	}
	ticker := string(pushes[2].Data)
	if len(ticker) < 1 || len(ticker) > 16 {
		return nil
	}
	if !utf8.ValidString(ticker) {
		return nil
	}
	if hasControlChars(ticker) {
		return nil
	}

	// Fourth push is name (may be empty)
	if len(pushes) < 4 {
		return nil
	}
	if !pushes[3].IsPush {
		return nil
	}
	name := string(pushes[3].Data)
	if len(name) > 64 {
		return nil
	}
	if !utf8.ValidString(name) {
		return nil
	}
	if hasControlChars(name) {
		return nil
	}

	// Fifth push is metadata_hash (must be 0 or 32 bytes). Require exactly
	// five pushes and nothing after: a trailing push would let two different
	// scripts parse to the same metadata, and give a minter somewhere to
	// hide bytes that consumers never see.
	if len(pushes) != 5 {
		return nil
	}
	if !pushes[4].IsPush {
		return nil
	}
	hashLen := len(pushes[4].Data)
	if hashLen != 0 && hashLen != 32 {
		return nil
	}

	var metadataHash string
	if hashLen == 32 {
		metadataHash = hex.EncodeToString(pushes[4].Data)
	}

	return &TokenMetadata{
		Ticker:       ticker,
		Name:         name,
		MetadataHash: metadataHash,
	}
}

// hasControlChars returns true if the string contains any control characters
// (below 0x20 or 0x7f).
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
