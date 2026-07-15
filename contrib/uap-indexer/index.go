package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
)

// Position is one UAP mint or transfer output the indexer has observed.
type Position struct {
	TxID        string `json:"txid"`
	Vout        uint32 `json:"vout"`
	PubKey      string `json:"pubkey"` // hex-encoded recipient pubkey
	Multiplier  int64  `json:"multiplier"`
	Value       int64  `json:"value"` // satoshis
	IsMint      bool   `json:"is_mint"`
	Height      int64  `json:"height"`
	Spent       bool   `json:"spent"`
	SpentTxID   string `json:"spent_txid,omitempty"`
	SpentHeight int64  `json:"spent_height,omitempty"`
}

func positionKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}

// heightChange records everything a single block did to the index, so a
// reorg can undo exactly that block without rescanning from genesis.
type heightChange struct {
	Created   []string `json:"created"`
	SpentKeys []string `json:"spent_keys"`
}

// Index is the indexer's full in-memory state. It is safe for concurrent
// use by the polling goroutine and the HTTP API.
type Index struct {
	mu sync.RWMutex

	Positions map[string]*Position       `json:"positions"`
	ByPubKey  map[string]map[string]bool `json:"by_pubkey"`
	Heights   map[int64]string           `json:"heights"`    // height -> block hash
	HeightLog map[int64]*heightChange    `json:"height_log"` // height -> undo log
	TipHeight int64                      `json:"tip_height"`
	TipHash   string                     `json:"tip_hash"`
	Orders    map[string]*Order          `json:"orders"` // positionKey -> standing sell order
}

func NewIndex() *Index {
	return &Index{
		Positions: make(map[string]*Position),
		ByPubKey:  make(map[string]map[string]bool),
		Heights:   make(map[int64]string),
		HeightLog: make(map[int64]*heightChange),
		Orders:    make(map[string]*Order),
		TipHeight: -1,
	}
}

// HashAtHeight returns what the indexer currently believes the block hash
// at the given height is, and whether it has an opinion at all.
func (idx *Index) HashAtHeight(height int64) (string, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	h, ok := idx.Heights[height]
	return h, ok
}

// ApplyBlock indexes one connected block: newly created UAP positions, and
// previously-indexed positions spent by this block's inputs.
func (idx *Index) ApplyBlock(block *RPCBlock) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	change := &heightChange{}

	for _, tx := range block.Tx {
		for _, vin := range tx.Vin {
			if vin.Coinbase != "" {
				continue
			}
			k := positionKey(vin.TxID, vin.Vout)
			if pos, ok := idx.Positions[k]; ok && !pos.Spent {
				pos.Spent = true
				pos.SpentTxID = tx.TxID
				pos.SpentHeight = block.Height
				change.SpentKeys = append(change.SpentKeys, k)
				// Any standing order for this position is now stale
				// (filled, or the maker moved it some other way).
				delete(idx.Orders, k)
			}
		}
		for _, vout := range tx.Vout {
			parsed, err := ParseUAPScript(vout.ScriptPubKey.Hex)
			if err != nil || parsed == nil {
				continue
			}
			k := positionKey(tx.TxID, vout.N)
			pkHex := hex.EncodeToString(parsed.PubKey)
			pos := &Position{
				TxID:       tx.TxID,
				Vout:       vout.N,
				PubKey:     pkHex,
				Multiplier: parsed.Multiplier,
				Value:      int64(math.Round(vout.Value * 1e8)),
				IsMint:     parsed.IsMint,
				Height:     block.Height,
			}
			idx.Positions[k] = pos
			if idx.ByPubKey[pkHex] == nil {
				idx.ByPubKey[pkHex] = make(map[string]bool)
			}
			idx.ByPubKey[pkHex][k] = true
			change.Created = append(change.Created, k)
		}
	}

	idx.Heights[block.Height] = block.Hash
	idx.HeightLog[block.Height] = change
	idx.TipHeight = block.Height
	idx.TipHash = block.Hash
}

// UndoBlock reverses everything ApplyBlock did for the given height. Used
// when a reorg is detected and the indexed chain must roll back before
// re-applying the new best chain.
func (idx *Index) UndoBlock(height int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	change, ok := idx.HeightLog[height]
	if !ok {
		delete(idx.Heights, height)
		return
	}
	for _, k := range change.Created {
		if pos, ok := idx.Positions[k]; ok {
			delete(idx.ByPubKey[pos.PubKey], k)
		}
		delete(idx.Positions, k)
	}
	for _, k := range change.SpentKeys {
		if pos, ok := idx.Positions[k]; ok {
			pos.Spent = false
			pos.SpentTxID = ""
			pos.SpentHeight = 0
		}
	}
	delete(idx.HeightLog, height)
	delete(idx.Heights, height)
}

// PositionsForPubKey returns a snapshot copy of all known positions for a
// recipient pubkey (hex-encoded), optionally filtered to unspent only.
func (idx *Index) PositionsForPubKey(pubkeyHex string, unspentOnly bool) []Position {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var out []Position
	for k := range idx.ByPubKey[pubkeyHex] {
		pos := idx.Positions[k]
		if pos == nil {
			continue
		}
		if unspentOnly && pos.Spent {
			continue
		}
		out = append(out, *pos)
	}
	return out
}

// Position looks up a single position by outpoint.
func (idx *Index) Position(txid string, vout uint32) (Position, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	pos, ok := idx.Positions[positionKey(txid, vout)]
	if !ok {
		return Position{}, false
	}
	return *pos, true
}

// Status is a snapshot of indexer progress for the /status endpoint.
type Status struct {
	TipHeight     int64  `json:"tip_height"`
	TipHash       string `json:"tip_hash"`
	PositionCount int    `json:"position_count"`
	OrderCount    int    `json:"order_count"`
}

func (idx *Index) StatusSnapshot() Status {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return Status{TipHeight: idx.TipHeight, TipHash: idx.TipHash, PositionCount: len(idx.Positions), OrderCount: len(idx.Orders)}
}

// persisted is the on-disk snapshot format, saved periodically so a
// restart doesn't require rescanning from genesis.
type persisted struct {
	Positions map[string]*Position       `json:"positions"`
	ByPubKey  map[string]map[string]bool `json:"by_pubkey"`
	Heights   map[int64]string           `json:"heights"`
	HeightLog map[int64]*heightChange    `json:"height_log"`
	TipHeight int64                      `json:"tip_height"`
	TipHash   string                     `json:"tip_hash"`
	Orders    map[string]*Order          `json:"orders"`
}

func (idx *Index) Save(path string) error {
	idx.mu.RLock()
	p := persisted{
		Positions: idx.Positions,
		ByPubKey:  idx.ByPubKey,
		Heights:   idx.Heights,
		HeightLog: idx.HeightLog,
		TipHeight: idx.TipHeight,
		TipHash:   idx.TipHash,
		Orders:    idx.Orders,
	}
	data, err := json.Marshal(p)
	idx.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewIndex(), nil
	}
	if err != nil {
		return nil, err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	idx := NewIndex()
	if p.Positions != nil {
		idx.Positions = p.Positions
	}
	if p.ByPubKey != nil {
		idx.ByPubKey = p.ByPubKey
	}
	if p.Heights != nil {
		idx.Heights = p.Heights
	}
	if p.HeightLog != nil {
		idx.HeightLog = p.HeightLog
	}
	if p.Orders != nil {
		idx.Orders = p.Orders
	}
	idx.TipHeight = p.TipHeight
	idx.TipHash = p.TipHash
	return idx, nil
}
