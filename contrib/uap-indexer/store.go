package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// Store is the indexer's state, held in a SQLite database. It is not a
// cache in front of an in-memory index -- it *is* the index. The only
// things the process keeps in RAM are the tip, the prune watermark and a
// couple of counters, all of them O(1).
//
// Two problems drove this. The first was persistence cost: the original
// whole-state JSON snapshot was O(everything indexed) per save, 2.4 seconds
// and 184MB of marshalling at 200k blocks, performed while holding the
// index's read lock. Each block now commits its own changes in one
// transaction, at a flat ~0.25ms regardless of chain length.
//
// The second was memory. Holding every position, UTXO and their two
// reverse indexes in Go maps meant RSS grew without bound alongside the
// chain, to hold what the database on disk already had. SQLite's own page
// cache keeps the hot pages resident and evicts the rest, which is the
// same job done by something better at it.
//
// All writes arrive from the index under its write lock, so the store does
// no locking of its own beyond what SQLite already does. Reads go straight
// to the database and are safe to run concurrently with a write.
type Store struct {
	db   *sql.DB
	path string

	// keepalive holds a memory-backed database open. A `mode=memory`
	// SQLite database is destroyed when its last connection closes, and
	// database/sql is free to close idle pooled connections whenever it
	// likes -- so without a connection pinned for the store's lifetime an
	// idle in-memory index would silently empty itself. Nil for
	// file-backed stores, which have no such problem.
	keepalive *sql.Conn

	// rowsWritten counts row-level writes issued, for the test that pins
	// down per-block cost. Atomic because it is read outside the index
	// lock.
	rowsWritten atomic.Int64

	stmts stmtSet
}

type stmtSet struct {
	selPosition  *sql.Stmt
	selUTXO      *sql.Stmt
	selLineage   *sql.Stmt
	selHolder    *sql.Stmt
	selOrder     *sql.Stmt
	selTombstone *sql.Stmt
	selUndo      *sql.Stmt
	selHash      *sql.Stmt

	putPosition  *sql.Stmt
	delPosition  *sql.Stmt
	putUTXO      *sql.Stmt
	delUTXO      *sql.Stmt
	putOrder     *sql.Stmt
	delOrder     *sql.Stmt
	putTombstone *sql.Stmt
	delTombstone *sql.Stmt
	putHeight    *sql.Stmt
	delHeight    *sql.Stmt
	pruneBelow   *sql.Stmt
	putMeta      *sql.Stmt
	putLineage   *sql.Stmt
	delLineage   *sql.Stmt
	putHolder    *sql.Stmt
	delHolder    *sql.Stmt
	delHolders   *sql.Stmt
}

// ORDER BY clauses for paginated queries. Each must have a total order:
// the final columns form a unique tiebreaker so LIMIT/OFFSET cannot skip
// or duplicate rows when the sort columns have ties. See page.go.
const (
	// PositionsOrderBy orders by height (oldest first), with outpoint as tiebreak.
	PositionsOrderBy = "height, txid, vout"
	// UtxosOrderBy orders by value (largest first), with outpoint as tiebreak.
	UtxosOrderBy = "value DESC, txid, vout"
	// TokensOrderBy orders by origin, which is unique (the token's mint outpoint).
	TokensOrderBy = "l.origin"
	// OrdersOrderBy orders by key (the outpoint), which is unique and the primary key.
	OrdersOrderBy = "o.key"
)

// Heights and their undo logs live in one table because they are always
// written, pruned and rolled back together: every ApplyBlock writes both,
// UndoBlock drops both, and the reorg window discards both. Splitting them
// would only create a way for the two to disagree.
//
// The lineages/lineage_holders pair is the maintained aggregate behind
// GET /tokens. Computing it on demand is not an option: as a GROUP BY over
// the positions table it measured 660ms at 200k blocks, and as an
// in-process scan 474ms -- per request, and under the read lock. It is
// maintained here rather than in RAM because a Go-side aggregate and the
// rows it summarises are updated separately and can therefore disagree;
// updated inside the same transaction as the rows, it cannot. (It also has
// to leave RAM to be worth anything: the holder set alone is one entry per
// unspent position, which is the very thing being moved out.)
const storeSchema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS positions (
    key          TEXT PRIMARY KEY,
    txid         TEXT    NOT NULL,
    vout         INTEGER NOT NULL,
    pubkey       TEXT    NOT NULL,
    multiplier   INTEGER NOT NULL,
    value        INTEGER NOT NULL,
    is_mint      INTEGER NOT NULL,
    height       INTEGER NOT NULL,
    spent        INTEGER NOT NULL,
    spent_txid   TEXT    NOT NULL DEFAULT '',
    spent_height INTEGER NOT NULL DEFAULT 0,
    origin       TEXT    NOT NULL DEFAULT '',
    script       TEXT    NOT NULL DEFAULT '',
    metadata     BLOB
);
CREATE INDEX IF NOT EXISTS positions_by_pubkey ON positions(pubkey);
CREATE TABLE IF NOT EXISTS utxos (
    key     TEXT PRIMARY KEY,
    txid    TEXT    NOT NULL,
    vout    INTEGER NOT NULL,
    hash160 TEXT    NOT NULL,
    value   INTEGER NOT NULL,
    height  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS utxos_by_hash160 ON utxos(hash160);
CREATE TABLE IF NOT EXISTS heights (
    height INTEGER PRIMARY KEY,
    hash   TEXT NOT NULL,
    undo   BLOB
);
CREATE TABLE IF NOT EXISTS orders (
    key  TEXT PRIMARY KEY,
    data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS order_tombstones (
    key        TEXT PRIMARY KEY,
    script_sig TEXT    NOT NULL,
    at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lineages (
    origin         TEXT PRIMARY KEY,
    multiplier     INTEGER NOT NULL,
    unspent_value  INTEGER NOT NULL,
    holder_count   INTEGER NOT NULL,
    position_count INTEGER NOT NULL,
    mint_height    INTEGER NOT NULL,
    has_mint       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lineage_holders (
    origin TEXT    NOT NULL,
    pubkey TEXT    NOT NULL,
    n      INTEGER NOT NULL,
    PRIMARY KEY (origin, pubkey)
);
`

// CurrentSchemaVersion is the on-disk schema shape this binary understands.
// Bump it whenever storeSchema changes in a way that a query written
// against the old shape would misread or fail on -- a column added,
// renamed, retyped, or a table repurposed. Purely additive changes that
// existing queries are indifferent to (a new index, say) don't need a bump.
//
// The same reasoning applies when no column changes at all but the
// validity rules or meaning of a persisted value change. metadata is
// PERSISTED, not re-derived: putPosition JSON-marshals a TokenMetadata into
// positions.metadata, and scanToken unmarshals it back out. An existing
// database therefore holds blobs like {"ticker":"whippet-token-x","name":"..."}
// written under the old five-push/lowercase-tolerant rules. json.Unmarshal
// silently ignores the now-unknown "name" key rather than erroring, so
// without a version bump /api/tokens would keep serving long, mixed-case
// tickers that no mint under the new rules could ever produce -- a silent
// mismatch between what's on disk and what the current binary considers
// valid. The version gate in checkSchemaVersion below is a hard open
// failure and the only mechanism that forces an operator to reindex, so
// bump here whenever a persisted value's validity rules or meaning change,
// not only when a column does.
const CurrentSchemaVersion = 3

// schemaVersionKey is the meta row that records CurrentSchemaVersion at the
// time a database was created or last confirmed compatible.
const schemaVersionKey = "schema_version"

// storePragmas are appended to every DSN. WAL keeps a reader from blocking
// the block writer; synchronous=NORMAL is the right trade here because the
// entire database is re-derivable by replaying the chain, so surviving a
// power cut is not worth an fsync per block.
const storePragmas = "&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=busy_timeout(5000)"

// OpenStore opens the state database at path, creating it and its schema
// if it does not exist.
func OpenStore(path string) (*Store, error) {
	return openStore(path, "file:"+path+"?_txlock=immediate"+storePragmas, false)
}

var memStoreSeq atomic.Int64

// OpenMemoryStore creates a private, in-memory state database. Used by
// tests and by anything that wants an index without a file behind it;
// production goes through OpenStore.
func OpenMemoryStore() (*Store, error) {
	// cache=shared is what lets the several connections in database/sql's
	// pool see the same database rather than each getting a private empty
	// one -- the classic ":memory:" trap.
	name := fmt.Sprintf("uap-index-mem-%d", memStoreSeq.Add(1))
	dsn := "file:" + name + "?mode=memory&cache=shared&_txlock=immediate" + storePragmas
	return openStore(name, dsn, true)
}

func openStore(path, dsn string, memory bool) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if memory {
		conn, err := db.Conn(context.Background())
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("pinning in-memory database: %w", err)
		}
		s.keepalive = conn
	}
	if _, err := db.Exec(storeSchema); err != nil {
		s.Close()
		return nil, fmt.Errorf("creating schema in %s: %w", path, err)
	}
	if err := s.checkSchemaVersion(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.prepare(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// checkSchemaVersion enforces that the database's on-disk shape matches what
// this binary knows how to read.
//
// A mismatch is refused rather than migrated: this indexer has no migration
// machinery, and letting a mismatched database through means the first sign
// of trouble is a confusing SQL error (or worse, a successful-looking
// misread) surfacing deep inside some later query, long after the operator
// could have done anything about it. Failing loudly here, before any block
// is applied or any request served, turns that into one actionable message
// naming both versions.
//
// A database with no schema_version key at all predates this check. Every
// such database was built from the same storeSchema this binary now calls
// version 1 -- there has never been another shape in the wild -- so it is
// stamped as version 1 here rather than refused. This is not a precedent
// for treating a future absent key as safe: once CurrentSchemaVersion moves
// past 1, an absent key will still mean exactly "version 1", because that
// is the last version that ever shipped without writing this row.
func (s *Store) checkSchemaVersion() error {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, schemaVersionKey).Scan(&raw)
	switch {
	case err == sql.ErrNoRows:
		return s.stampSchemaVersion(CurrentSchemaVersion)
	case err != nil:
		return fmt.Errorf("reading schema_version from %s: %w", s.path, err)
	}
	var version int
	if _, err := fmt.Sscan(raw, &version); err != nil {
		return fmt.Errorf("parsing schema_version %q in %s: %w", raw, s.path, err)
	}
	if version != CurrentSchemaVersion {
		return fmt.Errorf(
			"state database %s has schema version %d, this binary understands version %d; "+
				"refusing to open it rather than risk misreading or corrupting it -- "+
				"run the matching binary version against it, or delete it and let this "+
				"binary rebuild the index from scratch",
			s.path, version, CurrentSchemaVersion)
	}
	return nil
}

// stampSchemaVersion writes (or overwrites) the schema_version row.
func (s *Store) stampSchemaVersion(version int) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersionKey, fmt.Sprint(version))
	if err != nil {
		return fmt.Errorf("stamping schema_version in %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) prepare() error {
	for _, p := range []struct {
		dst   **sql.Stmt
		query string
	}{
		{&s.stmts.selPosition, `SELECT txid, vout, pubkey, multiplier, value, is_mint,
			height, spent, spent_txid, spent_height, origin, script, metadata
			FROM positions WHERE key = ?`},
		{&s.stmts.selUTXO, `SELECT txid, vout, hash160, value, height FROM utxos WHERE key = ?`},
		{&s.stmts.selLineage, `SELECT multiplier, unspent_value, holder_count,
			position_count, mint_height, has_mint FROM lineages WHERE origin = ?`},
		{&s.stmts.selHolder, `SELECT n FROM lineage_holders WHERE origin = ? AND pubkey = ?`},
		{&s.stmts.selOrder, `SELECT data FROM orders WHERE key = ?`},
		{&s.stmts.selTombstone, `SELECT script_sig FROM order_tombstones WHERE key = ?`},
		{&s.stmts.selUndo, `SELECT undo FROM heights WHERE height = ?`},
		{&s.stmts.selHash, `SELECT hash FROM heights WHERE height = ?`},

		{&s.stmts.putPosition, `INSERT INTO positions
			(key, txid, vout, pubkey, multiplier, value, is_mint, height, spent, spent_txid, spent_height, origin, script, metadata)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET
			  spent=excluded.spent, spent_txid=excluded.spent_txid,
			  spent_height=excluded.spent_height, origin=excluded.origin,
			  script=excluded.script, metadata=excluded.metadata`},
		{&s.stmts.delPosition, `DELETE FROM positions WHERE key = ?`},
		{&s.stmts.putUTXO, `INSERT INTO utxos (key, txid, vout, hash160, value, height)
			VALUES (?,?,?,?,?,?) ON CONFLICT(key) DO NOTHING`},
		{&s.stmts.delUTXO, `DELETE FROM utxos WHERE key = ?`},
		{&s.stmts.putOrder, `INSERT INTO orders (key, data) VALUES (?,?)
			ON CONFLICT(key) DO UPDATE SET data=excluded.data`},
		{&s.stmts.delOrder, `DELETE FROM orders WHERE key = ?`},
		{&s.stmts.putTombstone, `INSERT INTO order_tombstones (key, script_sig, at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET script_sig=excluded.script_sig, at=excluded.at`},
		{&s.stmts.delTombstone, `DELETE FROM order_tombstones WHERE key = ?`},
		{&s.stmts.putHeight, `INSERT INTO heights (height, hash, undo) VALUES (?,?,?)
			ON CONFLICT(height) DO UPDATE SET hash=excluded.hash, undo=excluded.undo`},
		{&s.stmts.delHeight, `DELETE FROM heights WHERE height = ?`},
		{&s.stmts.pruneBelow, `DELETE FROM heights WHERE height < ?`},
		{&s.stmts.putMeta, `INSERT INTO meta (key, value) VALUES (?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`},
		{&s.stmts.putLineage, `INSERT INTO lineages
			(origin, multiplier, unspent_value, holder_count, position_count, mint_height, has_mint)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(origin) DO UPDATE SET
			  multiplier=excluded.multiplier, unspent_value=excluded.unspent_value,
			  holder_count=excluded.holder_count, position_count=excluded.position_count,
			  mint_height=excluded.mint_height, has_mint=excluded.has_mint`},
		{&s.stmts.delLineage, `DELETE FROM lineages WHERE origin = ?`},
		{&s.stmts.putHolder, `INSERT INTO lineage_holders (origin, pubkey, n) VALUES (?,?,?)
			ON CONFLICT(origin, pubkey) DO UPDATE SET n=excluded.n`},
		{&s.stmts.delHolder, `DELETE FROM lineage_holders WHERE origin = ? AND pubkey = ?`},
		{&s.stmts.delHolders, `DELETE FROM lineage_holders WHERE origin = ?`},
	} {
		stmt, err := s.db.Prepare(p.query)
		if err != nil {
			return fmt.Errorf("preparing %q: %w", p.query, err)
		}
		*p.dst = stmt
	}
	return nil
}

func (s *Store) Close() error {
	if s.keepalive != nil {
		s.keepalive.Close()
		s.keepalive = nil
	}
	return s.db.Close()
}

// RowsWritten reports how many row-level writes the store has issued since
// it was opened.
func (s *Store) RowsWritten() int64 { return s.rowsWritten.Load() }

// ---------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------

// storeTx is a single atomic unit of work. Everything a block does --
// reading which positions its inputs spend, writing the results, updating
// the lineage aggregates -- happens inside one, so the database is never
// observed partway through a block, and the aggregates can never describe
// a different set of rows than the ones actually stored.
//
// Reads go through the transaction rather than the database directly, so a
// block that spends an output one of its own earlier transactions created
// sees it.
type storeTx struct {
	tx   *sql.Tx
	s    *Store
	rows int64
}

func (s *Store) begin() (*storeTx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &storeTx{tx: tx, s: s}, nil
}

func (t *storeTx) commit() error {
	if err := t.tx.Commit(); err != nil {
		return err
	}
	t.s.rowsWritten.Add(t.rows)
	return nil
}

func (t *storeTx) rollback() { t.tx.Rollback() }

func (t *storeTx) exec(stmt *sql.Stmt, args ...interface{}) error {
	if _, err := t.tx.Stmt(stmt).Exec(args...); err != nil {
		return err
	}
	t.rows++
	return nil
}

// scanner is whatever a row came from: *sql.Row or *sql.Rows.
type scanner interface{ Scan(...interface{}) error }

// scanPosition reads one position row. Shared by the in-transaction and
// read-only paths so the column order is written down once.
func scanPosition(row scanner) (*Position, error) {
	var (
		pos      Position
		metadata []byte
	)
	err := row.Scan(&pos.TxID, &pos.Vout, &pos.PubKey, &pos.Multiplier, &pos.Value,
		&pos.IsMint, &pos.Height, &pos.Spent, &pos.SpentTxID, &pos.SpentHeight,
		&pos.Origin, &pos.Script, &metadata)
	if err != nil {
		return nil, err
	}
	if len(metadata) > 0 {
		var md TokenMetadata
		if err := json.Unmarshal(metadata, &md); err != nil {
			return nil, err
		}
		pos.Metadata = &md
	}
	return &pos, nil
}

func scanUTXO(row scanner) (*UTXO, error) {
	var u UTXO
	if err := row.Scan(&u.TxID, &u.Vout, &u.Hash160, &u.Value, &u.Height); err != nil {
		return nil, err
	}
	return &u, nil
}

// --- reads within a transaction ---

func (t *storeTx) position(key string) (*Position, error) {
	pos, err := scanPosition(t.tx.Stmt(t.s.stmts.selPosition).QueryRow(key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return pos, err
}

func (t *storeTx) utxo(key string) (*UTXO, error) {
	u, err := scanUTXO(t.tx.Stmt(t.s.stmts.selUTXO).QueryRow(key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// order reads the stored order for an outpoint, regardless of whether the
// position behind it is still unspent.
func (t *storeTx) order(key string) (Order, bool, error) {
	var data []byte
	err := t.tx.Stmt(t.s.stmts.selOrder).QueryRow(key).Scan(&data)
	if err == sql.ErrNoRows {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, err
	}
	var o Order
	if err := json.Unmarshal(data, &o); err != nil {
		return Order{}, false, err
	}
	return o, true, nil
}

// hashAt reads a height's recorded block hash from inside the transaction.
//
// It must not go to the database directly. A write transaction holds
// SQLite's write lock, so a query issued on another pooled connection
// waits for a commit that cannot happen until the query returns -- a
// deadlock, and one that only shows up for the in-memory (shared-cache)
// store, where readers and the writer contend at table granularity rather
// than through WAL snapshots. Reading inside the transaction is also the
// only way to see this block's own changes.
func (t *storeTx) hashAt(height int64) (string, error) {
	var hash string
	err := t.tx.Stmt(t.s.stmts.selHash).QueryRow(height).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

func (t *storeTx) undoLog(height int64) (*heightChange, error) {
	var raw []byte
	err := t.tx.Stmt(t.s.stmts.selUndo).QueryRow(height).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var change heightChange
	if err := json.Unmarshal(raw, &change); err != nil {
		return nil, fmt.Errorf("undo log at height %d: %w", height, err)
	}
	return &change, nil
}

// --- writes within a transaction ---

func (t *storeTx) putPosition(pos *Position) error {
	var metadata []byte
	if pos.Metadata != nil {
		raw, err := json.Marshal(pos.Metadata)
		if err != nil {
			return err
		}
		metadata = raw
	}
	return t.exec(t.s.stmts.putPosition,
		positionKey(pos.TxID, pos.Vout), pos.TxID, pos.Vout, pos.PubKey,
		pos.Multiplier, pos.Value, pos.IsMint, pos.Height, pos.Spent,
		pos.SpentTxID, pos.SpentHeight, pos.Origin, pos.Script, metadata)
}

func (t *storeTx) delPosition(key string) error {
	return t.exec(t.s.stmts.delPosition, key)
}

func (t *storeTx) putUTXO(u *UTXO) error {
	return t.exec(t.s.stmts.putUTXO,
		positionKey(u.TxID, u.Vout), u.TxID, u.Vout, u.Hash160, u.Value, u.Height)
}

func (t *storeTx) delUTXO(key string) error { return t.exec(t.s.stmts.delUTXO, key) }

func (t *storeTx) putOrder(o *Order) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return t.exec(t.s.stmts.putOrder, positionKey(o.TxID, o.Vout), data)
}

func (t *storeTx) delOrder(key string) error { return t.exec(t.s.stmts.delOrder, key) }

// tombstone reports the scriptSig of a cancelled order for this outpoint,
// if one was withdrawn here. Empty means none.
func (t *storeTx) tombstone(key string) (string, error) {
	var scriptSig string
	err := t.tx.Stmt(t.s.stmts.selTombstone).QueryRow(key).Scan(&scriptSig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return scriptSig, err
}

func (t *storeTx) putTombstone(key, scriptSig string, at int64) error {
	return t.exec(t.s.stmts.putTombstone, key, scriptSig, at)
}

func (t *storeTx) delTombstone(key string) error {
	return t.exec(t.s.stmts.delTombstone, key)
}

func (t *storeTx) putHeight(height int64, hash string, undo *heightChange) error {
	var raw []byte
	if undo != nil {
		b, err := json.Marshal(undo)
		if err != nil {
			return err
		}
		raw = b
	}
	return t.exec(t.s.stmts.putHeight, height, hash, raw)
}

func (t *storeTx) delHeight(height int64) error { return t.exec(t.s.stmts.delHeight, height) }

func (t *storeTx) pruneHeightsBelow(height int64) error {
	return t.exec(t.s.stmts.pruneBelow, height)
}

type storeMeta struct {
	tipHeight   int64
	tipHash     string
	prunedBelow int64
	ambiguous   int64
}

func (t *storeTx) putMeta(m storeMeta) error {
	for _, kv := range [][2]interface{}{
		{"tip_height", m.tipHeight},
		{"tip_hash", m.tipHash},
		{"pruned_below", m.prunedBelow},
		{"ambiguous_lineage_count", m.ambiguous},
	} {
		if err := t.exec(t.s.stmts.putMeta, kv[0], fmt.Sprint(kv[1])); err != nil {
			return err
		}
	}
	return nil
}

// --- lineage aggregate maintenance ---

// lineageAgg is one lineage's running summary, as stored.
//
// unspentValue holds the sum of raw values, not of value*multiplier. Every
// position in a lineage carries the same multiplier -- consensus enforces
// that on transfer -- so multiplying once at read time is equivalent, and
// it keeps the running total reversible. Accumulating saturated products
// would not be: once a total pins to MaxInt64 there is no subtracting back
// out of it, and a reorg would leave the figure stuck there permanently.
type lineageAgg struct {
	multiplier    int64
	unspentValue  int64
	holderCount   int64
	positionCount int64
	mintHeight    int64
	hasMint       bool
}

func (t *storeTx) lineage(origin string) (*lineageAgg, error) {
	var a lineageAgg
	err := t.tx.Stmt(t.s.stmts.selLineage).QueryRow(origin).Scan(
		&a.multiplier, &a.unspentValue, &a.holderCount,
		&a.positionCount, &a.mintHeight, &a.hasMint)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (t *storeTx) holderN(origin, pubkey string) (int64, error) {
	var n int64
	err := t.tx.Stmt(t.s.stmts.selHolder).QueryRow(origin, pubkey).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (t *storeTx) putLineage(origin string, a *lineageAgg) error {
	return t.exec(t.s.stmts.putLineage, origin, a.multiplier, a.unspentValue,
		a.holderCount, a.positionCount, a.mintHeight, a.hasMint)
}

// addToLineage folds a now-unspent position into its lineage summary.
func (t *storeTx) addToLineage(pos *Position) error {
	if pos.Origin == "" {
		return nil // orphan: parent unknown, belongs to no lineage
	}
	agg, err := t.lineage(pos.Origin)
	if err != nil {
		return err
	}
	if agg == nil {
		agg = &lineageAgg{}
	}
	n, err := t.holderN(pos.Origin, pos.PubKey)
	if err != nil {
		return err
	}
	if n == 0 {
		agg.holderCount++
	}
	if err := t.exec(t.s.stmts.putHolder, pos.Origin, pos.PubKey, n+1); err != nil {
		return err
	}

	agg.multiplier = pos.Multiplier
	if pos.Value > 0 {
		agg.unspentValue = satAdd(agg.unspentValue, pos.Value)
	}
	agg.positionCount++
	if pos.IsMint {
		agg.hasMint = true
		agg.mintHeight = pos.Height
	}
	return t.putLineage(pos.Origin, agg)
}

// removeFromLineage withdraws a position that has become spent or has been
// rolled back.
func (t *storeTx) removeFromLineage(pos *Position) error {
	if pos.Origin == "" {
		return nil
	}
	agg, err := t.lineage(pos.Origin)
	if err != nil || agg == nil {
		return err
	}
	n, err := t.holderN(pos.Origin, pos.PubKey)
	if err != nil {
		return err
	}
	if n <= 1 {
		if err := t.exec(t.s.stmts.delHolder, pos.Origin, pos.PubKey); err != nil {
			return err
		}
		if n == 1 {
			agg.holderCount--
		}
	} else if err := t.exec(t.s.stmts.putHolder, pos.Origin, pos.PubKey, n-1); err != nil {
		return err
	}

	if pos.Value > 0 {
		agg.unspentValue -= pos.Value
		// Defensive, and knowingly untested: under correct maintenance
		// the total cannot go below zero, since every subtraction pairs
		// with an earlier addition of the same value. It is kept because
		// the one way that pairing does break is saturation -- satAdd
		// pins at MaxInt64 and there is no subtracting back out of it --
		// and a total that went negative from there would render as a
		// supply of zero for every holder of the token, permanently.
		if agg.unspentValue < 0 {
			agg.unspentValue = 0
		}
	}
	agg.positionCount--
	if pos.IsMint {
		agg.hasMint = false
		agg.mintHeight = 0
	}
	if agg.positionCount <= 0 {
		// A lineage with no unspent positions is not reported at all, so
		// drop it rather than leave an empty shell behind.
		//
		// The holder sweep is defensive and knowingly untested: by the
		// time positionCount reaches zero every holder row has already
		// been decremented to zero and deleted above, so no test can
		// distinguish this line from its absence. It stays because the
		// failure it prevents is silent and permanent -- a stray row
		// makes holderN return non-zero for a holder the lineage no
		// longer has, so if a reorg re-applies the mint the resurrected
		// lineage under-counts its holders forever.
		if err := t.exec(t.s.stmts.delHolders, pos.Origin); err != nil {
			return err
		}
		return t.exec(t.s.stmts.delLineage, pos.Origin)
	}
	return t.putLineage(pos.Origin, agg)
}

// ---------------------------------------------------------------------
// Read-only queries, served straight from the database
// ---------------------------------------------------------------------

func (s *Store) Position(key string) (Position, bool, error) {
	pos, err := scanPosition(s.stmts.selPosition.QueryRow(key))
	if err == sql.ErrNoRows {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, err
	}
	return *pos, true, nil
}

func (s *Store) PositionsForPubKey(pubkey string, unspentOnly bool) ([]Position, error) {
	out, _, err := s.PositionsForPubKeyPage(pubkey, unspentOnly, Unlimited)
	return out, err
}

// PositionsForPubKeyPage is PositionsForPubKey over a window. The second
// result reports whether further rows exist beyond it.
func (s *Store) PositionsForPubKeyPage(pubkey string, unspentOnly bool, page Page) ([]Position, bool, error) {
	q := `SELECT txid, vout, pubkey, multiplier, value, is_mint, height, spent,
		spent_txid, spent_height, origin, script, metadata FROM positions WHERE pubkey = ?`
	if unspentOnly {
		q += ` AND spent = 0`
	}
	// Oldest first, outpoint as the tiebreak so the order is total.
	q += ` ORDER BY ` + PositionsOrderBy + page.sqlSuffix()
	rows, err := s.db.Query(q, pubkey)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		pos, err := scanPosition(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, *pos)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, hasMore := truncate(out, page)
	return out, hasMore, nil
}

func (s *Store) UTXO(key string) (UTXO, bool, error) {
	u, err := scanUTXO(s.stmts.selUTXO.QueryRow(key))
	if err == sql.ErrNoRows {
		return UTXO{}, false, nil
	}
	if err != nil {
		return UTXO{}, false, err
	}
	return *u, true, nil
}

func (s *Store) UTXOsForHash160(hash160 string) ([]UTXO, error) {
	out, _, err := s.UTXOsForHash160Page(hash160, Unlimited)
	return out, err
}

// UTXOsForHash160Page is UTXOsForHash160 over a window, LARGEST VALUE
// FIRST. The order is deliberate: a wallet funds transactions from this
// list, so if it only ever reads the first page that page must be the one
// most likely to cover the amount. See page.go.
func (s *Store) UTXOsForHash160Page(hash160 string, page Page) ([]UTXO, bool, error) {
	rows, err := s.db.Query(
		`SELECT txid, vout, hash160, value, height FROM utxos WHERE hash160 = ?`+
			` ORDER BY `+UtxosOrderBy+page.sqlSuffix(), hash160)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []UTXO
	for rows.Next() {
		u, err := scanUTXO(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, hasMore := truncate(out, page)
	return out, hasMore, nil
}

// tokenQuery renders the maintained aggregate as the API's TokenInfo. The
// join reaches back to the mint's own row for the token's declared
// ticker: a lineage's origin is by construction the mint's outpoint
// key, so this finds it even once the mint has been spent onward, which it
// normally is immediately. Only the mint is consulted, so a later holder
// cannot attach metadata of their own.
const tokenQuery = `SELECT l.origin, l.multiplier, l.unspent_value, l.holder_count,
		l.mint_height, l.has_mint, p.is_mint, p.metadata
	FROM lineages l LEFT JOIN positions p ON p.key = l.origin`

func scanToken(row scanner) (TokenInfo, error) {
	var (
		t            TokenInfo
		unspentValue int64
		holders      int64
		mintHeight   int64
		hasMint      bool
		mintIsMint   sql.NullBool
		mintMetadata []byte
	)
	if err := row.Scan(&t.Origin, &t.Multiplier, &unspentValue, &holders,
		&mintHeight, &hasMint, &mintIsMint, &mintMetadata); err != nil {
		return TokenInfo{}, err
	}
	t.Supply = satMul(unspentValue, t.Multiplier)
	t.Holders = int(holders)
	if hasMint {
		// Only an unspent mint contributes a height. A mint is normally
		// spent into a covenant immediately, so this is usually zero.
		t.MintHeight = mintHeight
	}
	if mintIsMint.Valid && mintIsMint.Bool && len(mintMetadata) > 0 {
		var md TokenMetadata
		if err := json.Unmarshal(mintMetadata, &md); err != nil {
			return TokenInfo{}, err
		}
		t.Ticker = md.Ticker
		t.MetadataHash = md.MetadataHash
	}
	return t, nil
}

func (s *Store) AllTokens() ([]TokenInfo, error) {
	out, _, err := s.AllTokensPage(Unlimited)
	return out, err
}

// AllTokensPage is AllTokens over a window, ordered by origin -- the
// lineage's own outpoint, which is unique, so the order is total.
func (s *Store) AllTokensPage(page Page) ([]TokenInfo, bool, error) {
	rows, err := s.db.Query(tokenQuery + ` ORDER BY ` + TokensOrderBy + page.sqlSuffix())
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, hasMore := truncate(out, page)
	return out, hasMore, nil
}

func (s *Store) Token(origin string) (TokenInfo, bool, error) {
	t, err := scanToken(s.db.QueryRow(tokenQuery+` WHERE l.origin = ?`, origin))
	if err == sql.ErrNoRows {
		return TokenInfo{}, false, nil
	}
	if err != nil {
		return TokenInfo{}, false, err
	}
	return t, true, nil
}

// orderQuery serves only open orders: the join drops any whose position has
// been spent or has vanished in a reorg. Doing it here rather than in Go
// keeps "open" defined in exactly one place.
const orderQuery = `SELECT o.data, p.value FROM orders o JOIN positions p ON p.key = o.key WHERE p.spent = 0`

func scanOrders(rows *sql.Rows) ([]Order, error) {
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var data []byte
		var backing int64
		if err := rows.Scan(&data, &backing); err != nil {
			return nil, err
		}
		var o Order
		if err := json.Unmarshal(data, &o); err != nil {
			return nil, err
		}
		// BackingValue comes off the joined position, never off the stored
		// blob -- even though publishOrder also sets it, because the publish
		// handler echoes the order straight back and that copy has to be
		// right too. Deriving it here is what makes it right for rows written
		// before the field existed: those blobs carry no backing_value key,
		// so json.Unmarshal leaves it zero, and zero renders as an unbacked
		// position -- exactly the ambiguity the field was added to remove.
		// The join is already here to decide whether the order is open, so
		// this costs nothing, and it cannot drift: an outpoint's value is
		// immutable, which makes the position the only honest source.
		o.BackingValue = backing
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) ListOrders(multiplier *int64) ([]Order, error) {
	out, _, err := s.ListOrdersPage(multiplier, Unlimited)
	return out, err
}

// ListOrdersPage is ListOrders over a window, oldest order first with the
// outpoint as a tiebreak so the order is total.
func (s *Store) ListOrdersPage(multiplier *int64, page Page) ([]Order, bool, error) {
	q, args := orderQuery, []interface{}{}
	if multiplier != nil {
		q += ` AND p.multiplier = ?`
		args = append(args, *multiplier)
	}
	// o.key is the outpoint and the table's primary key: unique, so this
	// is a total order. There is no created_at COLUMN -- created_at lives
	// inside the JSON blob in o.data -- so ordering by it would be a
	// runtime SQL error on every request.
	q += ` ORDER BY ` + OrdersOrderBy + page.sqlSuffix()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	orders, err := scanOrders(rows)
	if err != nil {
		return nil, false, err
	}
	orders, hasMore := truncate(orders, page)
	return orders, hasMore, nil
}

func (s *Store) Order(key string) (Order, bool, error) {
	rows, err := s.db.Query(orderQuery+` AND o.key = ?`, key)
	if err != nil {
		return Order{}, false, err
	}
	orders, err := scanOrders(rows)
	if err != nil || len(orders) == 0 {
		return Order{}, false, err
	}
	return orders[0], true, nil
}

// StoredOrder returns an order regardless of whether its position is still
// unspent. Distinct from Order, which serves only *open* orders: cancelling
// needs the row itself, not the API's view of it.
func (s *Store) StoredOrder(key string) (Order, bool, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM orders WHERE key = ?`, key).Scan(&data)
	if err == sql.ErrNoRows {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, err
	}
	var o Order
	if err := json.Unmarshal(data, &o); err != nil {
		return Order{}, false, err
	}
	return o, true, nil
}

func (s *Store) HashAtHeight(height int64) (string, bool, error) {
	var hash string
	err := s.db.QueryRow(`SELECT hash FROM heights WHERE height = ?`, height).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

func (s *Store) CanUndo(height int64) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM heights WHERE height = ? AND undo IS NOT NULL`, height).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) count(table string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n, err
}

func (s *Store) CountPositions() (int, error) { return s.count("positions") }
func (s *Store) CountUTXOs() (int, error)     { return s.count("utxos") }
func (s *Store) CountOrders() (int, error)    { return s.count("orders") }

// CountTombstones reports how many withdrawn orders are being remembered.
func (s *Store) CountTombstones() (int, error) { return s.count("order_tombstones") }

// CountUndoLogs reports how many heights still carry an undo log.
func (s *Store) CountUndoLogs() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM heights WHERE undo IS NOT NULL`).Scan(&n)
	return n, err
}

func (s *Store) loadMeta() (storeMeta, error) {
	// A database with no meta rows is a fresh one, and its tip is "no
	// blocks yet" -- the same -1 an empty index uses. Defaulting to 0
	// instead would claim genesis was already indexed.
	m := storeMeta{tipHeight: -1}
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return m, err
		}
		switch key {
		case "tip_height":
			fmt.Sscan(value, &m.tipHeight)
		case "tip_hash":
			m.tipHash = value
		case "pruned_below":
			fmt.Sscan(value, &m.prunedBelow)
		case "ambiguous_lineage_count":
			fmt.Sscan(value, &m.ambiguous)
		}
	}
	return m, rows.Err()
}

// truncate empties every table. Used by Index.Reset when a reorg runs
// deeper than the retained undo window.
func (s *Store) truncate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// order_tombstones is deliberately absent. Everything else here is
	// either derived from the chain and about to be rebuilt, or (orders)
	// ephemeral data that peers will re-mirror -- and that re-mirroring is
	// exactly the problem: wiping the tombstones would mean every deep
	// reorg forgot every cancellation the relay had ever accepted, and the
	// next poll restored precisely the orders makers had withdrawn.
	for _, table := range []string{
		"positions", "utxos", "heights", "orders",
		"meta", "lineages", "lineage_holders",
	} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return err
		}
		s.rowsWritten.Add(1)
	}
	// Wiping meta above also erases schema_version. Leaving it unset would
	// make the reset database indistinguishable from a genuine
	// pre-versioning one on the next open -- harmless today, since both
	// cases are stamped as CurrentSchemaVersion, but only by coincidence of
	// this being version 1. Writing it back explicitly keeps that from
	// being an accident future versions have to remember.
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		schemaVersionKey, fmt.Sprint(CurrentSchemaVersion)); err != nil {
		return err
	}
	s.rowsWritten.Add(1)
	return tx.Commit()
}

// LoadIndex builds an Index backed by this store. Only the tip and a few
// counters are read; everything else stays in the database and is queried
// on demand, so startup cost does not grow with the size of the index.
func (s *Store) LoadIndex() (*Index, error) {
	meta, err := s.loadMeta()
	if err != nil {
		return nil, err
	}
	idx := newIndex(s)
	idx.TipHeight = meta.tipHeight
	idx.TipHash = meta.tipHash
	idx.PrunedBelow = meta.prunedBelow
	idx.AmbiguousLineageCount = meta.ambiguous
	return idx, nil
}
