package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Durability tests for the SQLite-backed store: what happens to the state
// on disk when something goes wrong partway through, as opposed to
// store_test.go's tests, which cover the store behaving correctly end to
// end.
//
// All three scenarios here open their databases in t.TempDir() and never
// touch anything outside it. None of them execute the uap-indexer binary
// or talk to a node; they drive Store/Index directly, the same as every
// other test in this package.

// ---------------------------------------------------------------------
// 1. WAL tail corruption
// ---------------------------------------------------------------------

// walConnDSN reproduces the DSN OpenStore builds, for a second connection
// to the same file opened directly through database/sql rather than
// through the Store type. Needed only by the corruption test below, to
// hold the file open the way a concurrent reader would.
func walConnDSN(path string) string {
	return "file:" + path + "?_txlock=immediate" + storePragmas
}

// TestWALTailCorruptionFailsLoudlyRatherThanServingGarbage covers the
// scenario every other durability test here assumes doesn't happen: the
// state file's write-ahead log has a damaged tail when the indexer starts
// up. That can come from a torn write extending further than a single
// frame, a bad sector, or any other partial corruption -- the dangerous
// case is not that the corruption exists, it's an indexer that opens the
// damaged file anyway and quietly starts serving whatever was in the
// pages it *could* read as if it were the complete, correct chain state.
// A market maker or taker acting on that would be trading against
// numbers nobody can vouch for.
//
// Getting a WAL file to actually corrupt is less trivial than it sounds:
// SQLite auto-checkpoints (folds the WAL into the main database file and
// truncates it) when the *last* connection to a database closes, and
// Store.Close() closes the store's entire connection pool, so a literal
// "write data, Close(), corrupt the file" sequence finds nothing left to
// corrupt -- the checkpoint already ran and the WAL is empty. That
// checkpoint is suppressed here by holding open a second, independent
// connection to the same file for the lifetime of the test, standing in
// for the concurrent readers this store is explicitly designed to allow
// (see the "Reads go straight to the database" comment on Store) that may
// still be attached when the writer side shuts down. Store.Close() itself
// still returns nil -- this is what "closed cleanly" looks like from the
// caller's side, not a crash.
func TestWALTailCorruptionFailsLoudlyRatherThanServingGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	for _, b := range storeChain(150) {
		idx.ApplyBlock(b)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("applying the seed chain: %v", err)
	}

	// A second connection, held open for the rest of the test, so that
	// Store.Close() below is not the last connection to the file and
	// does not get to auto-checkpoint the WAL away.
	reader, err := sql.Open("sqlite", walConnDSN(path))
	if err != nil {
		t.Fatalf("opening reader connection: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	var one int
	if err := reader.QueryRow(`SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("reader connection unusable: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}

	walPath := path + "-wal"
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("reading %s: %v (the WAL was checkpointed away -- "+
			"the reader connection above should have prevented that)", walPath, err)
	}
	if len(data) == 0 {
		t.Fatal("WAL file is empty; nothing to corrupt")
	}
	// Garble the last quarter of the file. A real torn write usually
	// damages only the final, in-progress frame, but going further back
	// is the more demanding version of this test: it corrupts frames
	// that were genuinely committed, not just one that was mid-flight.
	// A fixed seed keeps the corrupted bytes -- and therefore whether
	// this reliably reproduces a failure -- the same on every run.
	n := len(data)
	tail := n / 4
	rng := rand.New(rand.NewSource(42))
	for i := n - tail; i < n; i++ {
		data[i] = byte(rng.Intn(256))
	}
	if err := os.WriteFile(walPath, data, 0o644); err != nil {
		t.Fatalf("writing corrupted WAL: %v", err)
	}

	// The actual assertion: reopening must not end in a store that
	// silently serves whatever it managed to read. Either OpenStore (or
	// the LoadIndex that follows it) reports an error -- observed
	// behaviour on this driver/config is "file is not a database" or
	// "database disk image is malformed", surfaced from the schema
	// bootstrap or the first meta-table read -- or, if it were to open
	// without error, none of the corrupted region's data content must be
	// wrong. This test doesn't need to pick which; a corrupted WAL tail
	// must not open successfully, full stop.
	reopened, err := OpenStore(path)
	if err != nil {
		t.Logf("OpenStore correctly refused a corrupted state file: %v", err)
		return
	}
	defer reopened.Close()
	if _, err := reopened.LoadIndex(); err != nil {
		t.Logf("LoadIndex correctly refused a corrupted state file: %v", err)
		return
	}
	t.Fatal("a corrupted WAL tail opened without error: the indexer would start " +
		"serving state from a database it never validated")
}

// ---------------------------------------------------------------------
// 2. Disk full
// ---------------------------------------------------------------------

// TestDiskFullRollsBackAndKeepsServingReads uses PRAGMA max_page_count to
// give the database a hard, deterministic size ceiling, then writes past
// it. This is the same failure SQLite reports for a genuinely full disk
// (SQLITE_FULL) without needing an actual full filesystem, and it is what
// exercises the rollback path: a write that can't allocate the pages it
// needs must undo everything it had done so far in that transaction, not
// leave a half-written block sitting in the tables.
//
// The second half of this test is the point the task called out
// specifically: a store that can no longer write must not also stop
// answering reads. The API's GET endpoints have nothing to do with the
// ingester filling up the disk, and a market maker checking their open
// orders should not go dark because the indexer ran out of space writing
// new blocks.
func TestDiskFullRollsBackAndKeepsServingReads(t *testing.T) {
	idx, store, _ := storeTestIndex(t)
	// database/sql pools multiple physical connections by default, and
	// max_page_count is a per-connection runtime limit, not something
	// stored in the database file. Pinning the pool to one connection is
	// the only way to guarantee every statement below -- writes issued
	// through storeTx, and the PRAGMA that set the limit -- actually
	// share the connection the limit was set on.
	store.db.SetMaxOpenConns(1)

	seed := storeChain(3)
	for _, b := range seed {
		idx.ApplyBlock(b)
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var pageCount int
	if err := store.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		t.Fatalf("reading page_count: %v", err)
	}
	// A small margin above the current size: enough room for some more
	// blocks (page reuse from the free list means the exact number
	// isn't predictable), not the whole 200-block chain below.
	if _, err := store.db.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pageCount+5)); err != nil {
		t.Fatalf("setting max_page_count: %v", err)
	}

	// lastGoodCount/lastGoodTip track state as of the last block that
	// fully committed. The page-count margin above is deliberately
	// loose -- SQLite reuses freed pages before allocating new ones, so
	// exactly how many more blocks fit before SQLITE_FULL hits depends
	// on the free list, not just the row count -- so this test doesn't
	// assume the cap bites on the very next block, only that whichever
	// block it does bite on rolls back cleanly.
	lastGoodCount := positionCount(t, idx)
	lastGoodTip, _ := idx.Tip()

	rest := storeChain(200)[3:]
	failedHeight := int64(-1)
	for _, b := range rest {
		idx.ApplyBlock(b)
		if err := idx.StoreErr(); err != nil {
			failedHeight = b.Height
			break
		}
		lastGoodCount = positionCount(t, idx)
		lastGoodTip, _ = idx.Tip()
	}
	if failedHeight < 0 {
		t.Fatal("never hit SQLITE_FULL; the page-count cap did not take effect " +
			"(is something still running on a second pooled connection?)")
	}

	// Rollback: nothing from the failing block landed anywhere, and the
	// tip did not move past the last block that fully committed.
	if got := positionCount(t, idx); got != lastGoodCount {
		t.Errorf("position count after a full-disk write: got %d, want %d "+
			"(the failed transaction left a partial write behind)", got, lastGoodCount)
	}
	if h, _ := idx.Tip(); h != lastGoodTip {
		t.Errorf("tip after a full-disk write: got %d, want %d", h, lastGoodTip)
	}
	if _, ok, _ := idx.Position(fmt.Sprintf("tx%d", failedHeight), 0); ok {
		t.Errorf("the position from the block that failed to write (height %d) "+
			"is visible anyway", failedHeight)
	}

	// Reads: the store must still answer queries while full. A store
	// that returned an error, an empty result, or blocked here would be
	// unavailable in exactly the way an operator can't just wait out --
	// the disk being full doesn't affect the pages already on it.
	toks, err := idx.AllTokens()
	if err != nil {
		t.Errorf("AllTokens while full: %v", err)
	}
	if len(toks) == 0 {
		t.Error("AllTokens returned nothing while full, want the tokens minted before the disk filled")
	}
	if _, ok, err := idx.Position("tx0", 0); err != nil || !ok {
		t.Errorf("Position(tx0:0) while full: ok=%v err=%v, want a hit with no error", ok, err)
	}
}

// ---------------------------------------------------------------------
// 3. Crash mid-write
// ---------------------------------------------------------------------
//
// What this proves, and what it does not:
//
// The store's pragmas set synchronous=NORMAL under journal_mode=WAL (see
// storePragmas in store.go). Under that combination SQLite does not fsync
// on every commit, only at WAL checkpoints -- so a commit can return
// success while its frames still live only in the OS page cache, not on
// physical media. Because of that, this test proves survival of a
// *process* crash (SIGKILL), not survival of a *power-loss* crash: the
// killed subprocess below dies with the OS and its page cache both still
// fully intact, so anything the process wrote is still exactly where
// SQLite left it once the parent process reopens the file. A real power
// cut, which drops the page cache along with the process, is a strictly
// harder case that this configuration does not claim to survive and that
// this test does not exercise. Proving that would need synchronous=FULL
// (or an actual power-loss test harness), and would cost an fsync per
// block -- the trade-off storePragmas' own comment explains was
// deliberately not made, because this store's entire content is
// re-derivable by replaying the chain.
//
// What SIGKILL *does* stand in for reasonably: an OOM kill, a supervisor
// sending SIGKILL after a hung shutdown, `kill -9` from an operator, or a
// panic in code that bypassed normal cleanup. All of those leave the OS
// and its page cache running, which is the case this test covers.

const (
	crashHelperEnv     = "UAP_DURABILITY_CRASH_HELPER"
	crashHelperPathEnv = "UAP_DURABILITY_CRASH_DBPATH"

	// Prefix the helper writes to stdout after each block commits, so the
	// parent can wait on committed state instead of on the clock. It has
	// to be distinctive because the child is a `go test` binary and mixes
	// its own output into the same stream.
	crashHelperCommitted = "UAP-COMMITTED "
)

// crashChainBlockAt builds the same single block storeChain(n)[h] would
// build at index h. It has to exist separately because the crash helper
// below writes for a fixed wall-clock duration rather than a fixed block
// count, and building the whole chain up front (storeChain, at n in the
// hundreds of thousands) would itself eat most of that duration before a
// single block was ever applied. TestCrashHelperBlocksMatchStoreChain
// pins the two generators against each other so this duplication can't
// silently drift from storeChain's definition.
func crashChainBlockAt(h int) *RPCBlock {
	salt := []byte("0123456789abcdef")
	tx := RPCTx{
		TxID: fmt.Sprintf("tx%d", h),
		Vin:  []RPCVin{coinbaseVin()},
		Vout: []RPCVout{
			uapMintVout(0, fakePubKey(byte(0x02+h%3)), int64(100+h), salt, 1.0),
			p2pkhVoutFor(1, byte(0x40+h), 2.0),
		},
	}
	if h > 0 {
		tx.Vin = append(tx.Vin, spendVin(fmt.Sprintf("tx%d", h-1), 1))
	}
	return makeBlock(fmt.Sprintf("hash%d", h), int64(h), []RPCTx{tx})
}

// TestCrashHelperBlocksMatchStoreChain guards the duplication in
// crashChainBlockAt: if storeChain's block shape ever changes and this
// helper isn't updated to match, the crash test below would still pass,
// but silently against a chain no longer equivalent to the rest of this
// file's fixtures. Comparing rendered blocks catches that immediately
// instead.
func TestCrashHelperBlocksMatchStoreChain(t *testing.T) {
	want := storeChain(10)
	for h := 0; h < 10; h++ {
		got := crashChainBlockAt(h)
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want[h])
		if string(gj) != string(wj) {
			t.Fatalf("crashChainBlockAt(%d) diverged from storeChain:\n got %s\nwant %s", h, gj, wj)
		}
	}
}

// TestCrashHelperProcess is not a test in its own right. Run normally (no
// UAP_DURABILITY_CRASH_HELPER in the environment) it does nothing and
// passes immediately. TestCrashMidWriteRecoversToAConsistentPrefix below
// re-execs the test binary with that variable set and
// -test.run restricted to this one test, which is the standard Go
// pattern for getting a real, killable child process out of `go test`
// (the same trick net/http and os/exec's own tests use) -- go test itself
// offers no other supported way to spawn a subprocess that is still this
// binary.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		return
	}
	path := os.Getenv(crashHelperPathEnv)
	store, err := OpenStore(path)
	if err != nil {
		os.Exit(2)
	}
	idx, err := store.LoadIndex()
	if err != nil {
		os.Exit(3)
	}
	// Loop far longer than the parent will let this process live; the
	// parent's SIGKILL is what actually ends it. Generating each block
	// on the fly (rather than building storeChain(n) up front) matters
	// here: the parent only waits a fixed, short duration before
	// killing this process, and that duration needs to be spent writing
	// blocks, not constructing a large slice of them first.
	for h := 0; h < 50_000_000; h++ {
		idx.ApplyBlock(crashChainBlockAt(h))
		if err := idx.StoreErr(); err != nil {
			os.Exit(4)
		}
		// Announce each committed height so the parent can wait for real
		// committed state rather than guessing at a duration. This is
		// printed after StoreErr so a height only ever appears once the
		// transaction for it has actually gone in.
		fmt.Printf("%s%d\n", crashHelperCommitted, h)
	}
	os.Exit(0)
}

// TestCrashMidWriteRecoversToAConsistentPrefix starts a child process
// that writes blocks as fast as it can, SIGKILLs it partway through, and
// checks that the parent reopening the same database file lands on a
// state that is bit-identical to a fresh store built by replaying blocks
// 0..tip from scratch. "Bit-identical to a fresh replay" is what makes
// this a meaningful assertion rather than just "it didn't panic": every
// block commits in one transaction (see storeTx and idx.write), so the
// only two outcomes a correct implementation has are "this block's
// changes are entirely present" or "entirely absent" -- there is no
// partial-block state for a correct implementation to recover into, and
// this test would catch it if there were one.
//
// See the comment above crashHelperEnv for what this does and does not
// prove about durability: process crash, not power loss.
func TestCrashMidWriteRecoversToAConsistentPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$")
	cmd.Env = append(os.Environ(), crashHelperEnv+"=1", crashHelperPathEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}
	// Wait until the helper has demonstrably committed a run of blocks
	// before killing it. A fixed sleep was the obvious thing here and it
	// is the wrong one: the case worth exercising is being killed
	// *mid-stream*, and how long it takes to get there depends on the
	// machine, on -race, and on whatever else a CI runner is doing. Too
	// short and the test fails on a slow box having proved nothing; too
	// long and it burns that same margin on every fast one.
	//
	// The helper announcing its committed heights is the signal. Two
	// cheaper-looking signals do not work: the WAL file crosses any size
	// threshold you pick well before the first transaction commits (it
	// is where pages go on their way to being committed, not a record of
	// what has been), and opening the store here to read its tip would
	// put a second connection on a database the helper is actively
	// writing, changing the thing being measured.
	const commitsBeforeKill = 200
	seen := -1
	scanner := bufio.NewScanner(stdout)
	killAt := time.AfterFunc(10*time.Second, func() { _ = cmd.Process.Kill() })
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, crashHelperCommitted) {
			continue // ordinary `go test` chatter from the child
		}
		h, err := strconv.Atoi(strings.TrimPrefix(line, crashHelperCommitted))
		if err != nil {
			continue
		}
		seen = h
		if h >= commitsBeforeKill {
			break
		}
	}
	killAt.Stop()
	if seen < commitsBeforeKill {
		// Not fatal on its own -- kill and let the tip check below
		// report, since it says more about what actually survived.
		t.Logf("helper only reported %d committed blocks before the wait ended", seen+1)
	}
	// A failure here is not worth stopping on: the only way it happens is
	// that the watchdog above already killed the process, which is a
	// state the tip check below reports on perfectly well.
	_ = cmd.Process.Kill()
	_ = cmd.Wait() // expected to report a signal-killed exit; nothing to check

	recoveredStore, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopening after SIGKILL: %v", err)
	}
	t.Cleanup(func() { recoveredStore.Close() })
	recovered, err := recoveredStore.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex after SIGKILL: %v", err)
	}
	tip, _ := recovered.Tip()
	if tip < 0 {
		t.Fatal("no blocks survived the kill; the helper process did not get to write " +
			"anything in the time given (increase the sleep above)")
	}
	t.Logf("recovered at tip %d after SIGKILL", tip)

	freshIdx, _, _ := storeTestIndex(t)
	for h := 0; h <= int(tip); h++ {
		freshIdx.ApplyBlock(crashChainBlockAt(h))
	}
	if err := freshIdx.StoreErr(); err != nil {
		t.Fatalf("replaying the reference chain: %v", err)
	}

	got := snapshotIndex(t, recovered)
	want := snapshotIndex(t, freshIdx)
	if got != want {
		t.Errorf("state recovered after SIGKILL does not match a fresh replay to the same tip:\n got %s\nwant %s", got, want)
	}
}
