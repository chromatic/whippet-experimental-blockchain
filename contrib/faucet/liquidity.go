package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TokenInfo represents a UAP lineage's aggregated state
type TokenInfo struct {
	Origin       string `json:"origin"`
	Multiplier   int64  `json:"multiplier"`
	Ticker       string `json:"ticker,omitempty"`
	MetadataHash string `json:"metadata_hash,omitempty"`
	Supply       int64  `json:"supply"`  // sum of unspent value * multiplier
	Holders      int    `json:"holders"` // count of distinct unspent pubkeys
	MintHeight   int64  `json:"mint_height"`
}

// Order is a maker's standing sell order for a UAP position
type Order struct {
	TxID         string `json:"txid"`
	Vout         uint32 `json:"vout"`
	Multiplier   int64  `json:"multiplier"`
	PaymentValue int64  `json:"payment_value"`
	BackingValue int64  `json:"backing_value"`
	CreatedAt    int64  `json:"created_at"`
	PubKey       string `json:"pubkey,omitempty"`
	Origin       string `json:"origin,omitempty"` // lineage origin, not in JSON but used in tests/logic
}

// Executor is the interface for actually executing purchases
type Executor interface {
	Execute(order *Order) error
}

// DryRunExecutor records what it would buy without spending anything
type DryRunExecutor struct {
	Executed   []*Order
	TotalSpent int64
}

func (d *DryRunExecutor) Execute(order *Order) error {
	d.Executed = append(d.Executed, order)
	d.TotalSpent += order.PaymentValue
	return nil
}

// LiquidityEngine decides whether to bid on a UAP order
type LiquidityEngine struct {
	db            *sql.DB
	indexerURL    string
	dailySpendCap int64
	httpClient    *http.Client

	// Cached data from indexer
	tokens map[string]TokenInfo
	orders []Order
}

// NewLiquidityEngine creates a new liquidity engine
func NewLiquidityEngine(db *sql.DB, indexerURL string, dailySpendCap int64) (*LiquidityEngine, error) {
	if dailySpendCap <= 0 {
		return nil, fmt.Errorf("dailySpendCap must be positive")
	}

	if indexerURL == "" {
		return nil, fmt.Errorf("indexerURL must be specified")
	}

	// X may not exceed the budget the faucet already runs on: faucet_amount
	// coins per claim times daily_claim_limit claims. Those two numbers were
	// chosen deliberately by an operator who understands what they cost, which
	// makes their product the only figure here with any authority behind it.
	//
	// The par rule keeps the desk from being drained, but says nothing about
	// how much coin ends up parked in inventory in a day -- and inventory
	// cannot be dripped to new users. A mistyped X is therefore the one input
	// that can starve the faucet while every individual purchase it makes
	// remains perfectly sound.
	amount, err := getFaucetAmount(db)
	if err != nil {
		return nil, fmt.Errorf("reading faucet_amount to bound the liquidity cap: %w", err)
	}
	limit, err := getDailyClaimLimit(db)
	if err != nil {
		return nil, fmt.Errorf("reading daily_claim_limit to bound the liquidity cap: %w", err)
	}
	impliedBudget := int64(amount*100000000) * int64(limit)
	if dailySpendCap > impliedBudget {
		return nil, fmt.Errorf(
			"liquidity daily cap of %d satoshis exceeds the faucet's own implied daily budget of %d "+
				"(%v coins x %d claims); lower the cap, or raise faucet_amount/daily_claim_limit deliberately",
			dailySpendCap, impliedBudget, amount, limit)
	}

	engine := &LiquidityEngine{
		db:            db,
		indexerURL:    indexerURL,
		dailySpendCap: dailySpendCap,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		tokens: make(map[string]TokenInfo),
		orders: []Order{},
	}

	return engine, nil
}

// ReloadFromIndexer fetches fresh token and order data from the indexer
func (engine *LiquidityEngine) ReloadFromIndexer() error {
	// Fetch tokens
	resp, err := engine.httpClient.Get(engine.indexerURL + "/api/tokens")
	if err != nil {
		return fmt.Errorf("failed to fetch tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("indexer returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokens []TokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return fmt.Errorf("failed to parse tokens JSON: %w", err)
	}

	engine.tokens = make(map[string]TokenInfo)
	for _, t := range tokens {
		engine.tokens[t.Origin] = t
	}

	// Fetch orders
	resp, err = engine.httpClient.Get(engine.indexerURL + "/api/orders")
	if err != nil {
		return fmt.Errorf("failed to fetch orders: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("indexer returned status %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(&engine.orders); err != nil {
		return fmt.Errorf("failed to parse orders JSON: %w", err)
	}

	return nil
}

// IsEligible checks if an order meets all purchase criteria
func (engine *LiquidityEngine) IsEligible(order *Order) (bool, string) {
	// Rule 1: Never bid above par
	if order.PaymentValue > order.BackingValue {
		return false, "payment_value exceeds backing_value"
	}

	// Rule 2: Backing value must be positive
	if order.BackingValue <= 0 {
		return false, "backing_value must be positive"
	}

	// Find the token lineage using the order's origin
	token, exists := engine.tokens[order.Origin]
	if !exists {
		return false, "lineage origin not found"
	}

	// Rule 3: Holders >= 2
	if token.Holders < 2 {
		return false, "token has fewer than 2 holders"
	}

	// Rule 4: Purchase count (at most 4 lifetime per lineage)
	purchaseCount, err := engine.getPurchaseCount(order.Origin)
	if err != nil {
		return false, fmt.Sprintf("failed to check purchase count: %v", err)
	}
	if purchaseCount >= 4 {
		return false, "lineage has reached maximum of 4 purchases"
	}

	// Rule 5: Per-lineage cumulative spend (at most 20% of supply)
	cumulativeSpend, err := engine.getCumulativeSpend(order.Origin)
	if err != nil {
		return false, fmt.Sprintf("failed to check cumulative spend: %v", err)
	}

	// Supply is TOKEN UNITS, not satoshis: the indexer builds it as
	// satMul(unspentValue, multiplier) -- see uap-indexer/store.go. Spending
	// is measured in satoshis, so the cap must be converted to satoshis before
	// the two are compared. Taking 20% of Supply directly compares against a
	// quantity `multiplier` times larger, inflating the cap by exactly the
	// multiplier; above a multiplier of 5 the concentration limit then never
	// binds at all.
	//
	// The division is exact rather than lossy: Supply was produced by
	// multiplying the satoshi backing by this same multiplier, so dividing it
	// back out recovers the backing precisely. Dividing first also keeps the
	// * 20 from overflowing on a large lineage.
	if token.Multiplier <= 0 {
		return false, "token multiplier is not positive"
	}
	backingSats := token.Supply / token.Multiplier
	maxSpendPerLineage := (backingSats * 20) / 100
	if cumulativeSpend+order.PaymentValue > maxSpendPerLineage {
		return false, "purchase would exceed 20% of supply cap"
	}

	// Rule 6: Daily spend cap
	dailySpend, err := engine.getDailySpend()
	if err != nil {
		return false, fmt.Sprintf("failed to check daily spend: %v", err)
	}
	if dailySpend+order.PaymentValue > engine.dailySpendCap {
		return false, "purchase would exceed daily spend cap"
	}

	return true, ""
}

// RecordPurchase logs a purchase to the daily-spend ledger
func (engine *LiquidityEngine) RecordPurchase(origin string, order *Order) error {
	outpoint := fmt.Sprintf("%s:%d", order.TxID, order.Vout)
	now := time.Now().Unix()

	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		outpoint,
		order.PaymentValue,
		now,
	)
	return err
}

// getPurchaseCount returns how many times a lineage has been purchased
func (engine *LiquidityEngine) getPurchaseCount(origin string) (int, error) {
	var count int
	err := engine.db.QueryRow(
		`SELECT COUNT(*) FROM uap_purchases WHERE origin=?`,
		origin,
	).Scan(&count)
	return count, err
}

// getCumulativeSpend returns total satoshis spent on a lineage
func (engine *LiquidityEngine) getCumulativeSpend(origin string) (int64, error) {
	var total sql.NullInt64
	err := engine.db.QueryRow(
		`SELECT SUM(amount) FROM uap_purchases WHERE origin=?`,
		origin,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// getDailySpend returns how much has been spent today (in satoshis)
func (engine *LiquidityEngine) getDailySpend() (int64, error) {
	// Start of today in UTC
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()

	var total sql.NullInt64
	err := engine.db.QueryRow(
		`SELECT SUM(amount) FROM uap_purchases WHERE timestamp >= ?`,
		startOfDay,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// initLiquidityDB creates the necessary database tables
func initLiquidityDB(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS uap_purchases (
		id INTEGER PRIMARY KEY,
		origin TEXT NOT NULL,
		txid_vout TEXT NOT NULL,
		amount INTEGER NOT NULL,
		timestamp INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_uap_purchases_origin ON uap_purchases(origin);
	CREATE INDEX IF NOT EXISTS idx_uap_purchases_timestamp ON uap_purchases(timestamp);
	`

	_, err := db.Exec(schema)
	return err
}
