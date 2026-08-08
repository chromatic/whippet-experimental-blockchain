package main

import (
	"errors"
	"net/http"
	"strconv"
)

// Pagination for the list endpoints.
//
// # Why the responses stay bare arrays
//
// The obvious shape for a paginated endpoint is {items, total, next}. That
// is not what these return, because api.js hard-fails on anything that is
// not an array ("getUtxos: response is not an array") and every deployed
// wallet would break the moment the indexer was upgraded. The page
// metadata therefore travels in headers, which old clients ignore and new
// ones can read.
//
// # Why there is a default cap at all
//
// Without one, /positions for a busy pubkey or /tokens on a mature chain
// serialises the whole table into one response: unbounded memory on the
// server and an unbounded download for a phone on mobile data. A cap that
// the client cannot raise past maxPageLimit makes the worst case finite.
//
// # The hazard this creates, and what is done about it
//
// Truncating /utxos is not cosmetic: the wallet funds transactions from
// that list, so a silently short page can report a low balance and fail to
// cover an amount the user really holds. Two things address it. The rows
// come back largest-value first, so a truncated first page is the most
// spendable one and covers any realistic amount; and X-Has-More says
// plainly that there is more, so a client can page rather than guess.
//
// # Ordering
//
// Every paginated query has a total order, with the outpoint as the final
// tiebreak. An ORDER BY that can tie is not good enough for LIMIT/OFFSET:
// SQLite may return tied rows in a different order between two queries, so
// a row can appear on both pages or on neither.

const (
	defaultPageLimit = 500
	maxPageLimit     = 1000
)

// Page is a half-open window over a result set. A zero Limit means
// unlimited and is used by internal callers (mirroring, tests) that are
// not serving a remote client.
type Page struct {
	Limit  int
	Offset int
}

// Unlimited is the page internal callers pass when they genuinely want
// every row.
var Unlimited = Page{}

func (p Page) unlimited() bool { return p.Limit <= 0 }

// sqlSuffix renders the LIMIT/OFFSET clause. It asks for one row more than
// requested so the caller can tell whether a further page exists without a
// second COUNT query over the same table.
func (p Page) sqlSuffix() string {
	if p.unlimited() {
		return ""
	}
	return " LIMIT " + strconv.Itoa(p.Limit+1) + " OFFSET " + strconv.Itoa(p.Offset)
}

// truncate trims the extra probe row, reporting whether it was there.
func truncate[T any](rows []T, p Page) ([]T, bool) {
	if p.unlimited() || len(rows) <= p.Limit {
		return rows, false
	}
	return rows[:p.Limit], true
}

var errBadPage = errors.New("bad pagination parameter")

// parsePage reads ?limit= and ?offset=. Both are optional. A malformed or
// out-of-range value is an error rather than something to clamp silently:
// a client that asked for limit=abc has a bug, and quietly serving it 500
// rows hides that.
func parsePage(r *http.Request) (Page, error) {
	p := Page{Limit: defaultPageLimit}

	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return Page{}, errBadPage
		}
		if n > maxPageLimit {
			return Page{}, errBadPage
		}
		p.Limit = n
	}
	if s := r.URL.Query().Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return Page{}, errBadPage
		}
		p.Offset = n
	}
	return p, nil
}

// writePageHeaders describes the window that was actually served. Sent
// before the body, so they are set on the ResponseWriter before writeJSON.
func writePageHeaders(w http.ResponseWriter, p Page, returned int, hasMore bool) {
	h := w.Header()
	h.Set("X-Page-Limit", strconv.Itoa(p.Limit))
	h.Set("X-Page-Offset", strconv.Itoa(p.Offset))
	h.Set("X-Page-Count", strconv.Itoa(returned))
	if hasMore {
		h.Set("X-Has-More", "true")
		h.Set("X-Next-Offset", strconv.Itoa(p.Offset+returned))
	} else {
		h.Set("X-Has-More", "false")
	}
}

// pageErrorMessage is what a client sees for a malformed limit/offset. It
// names the bounds rather than just saying "invalid", because the usual
// cause is asking for more rows than the cap allows.
const pageErrorMessage = "invalid limit or offset: limit must be 1..1000, offset must be >= 0"
