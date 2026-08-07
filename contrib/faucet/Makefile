FAUCET_BIN=faucet

all: $(FAUCET_BIN)


$(FAUCET_BIN): faucet.go
	go build -ldflags="-s -w" -buildmode=pie -o $(FAUCET_BIN) faucet.go
	strip $(FAUCET_BIN)
	@if command -v upx >/dev/null 2>&1; then \
		echo "Compressing with upx --best --lzma..."; \
		upx --best --lzma $(FAUCET_BIN); \
	else \
		echo "upx not found, skipping compression."; \
	fi

clean:
	rm -f $(FAUCET_BIN)