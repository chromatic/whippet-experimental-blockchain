module github.com/chromatic/whippet/contrib/faucet

go 1.24.2

require (
	github.com/mattn/go-sqlite3 v1.14.33
	github.com/whippet/whippet/contrib/whippetrpc v0.0.0
)

replace github.com/whippet/whippet/contrib/whippetrpc => ../whippetrpc
