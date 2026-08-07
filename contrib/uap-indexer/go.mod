module github.com/whippet/whippet/contrib/uap-indexer

go 1.25.0

require (
	github.com/btcsuite/btcd/btcec/v2 v2.5.0
	github.com/whippet/whippet/contrib/whippetrpc v0.0.0
)

require github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.56.0
)

replace github.com/whippet/whippet/contrib/whippetrpc => ../whippetrpc
