#!/bin/bash
set -e

echo "╔════════════════════════════════════════════════════════════════════════════╗"
echo "║              WHIPPET GENESIS & INITIAL MINING AUTOMATION                  ║"
echo "╚════════════════════════════════════════════════════════════════════════════╝"
echo ""

MINING_ADDRESS="WPJScvzvEEY7NAnJYh6KaioZeB5SDrgAB6"
TIMESTAMP=1735689600

echo "Step 1: Compile genesis miner..."
g++ -O3 -o mine-genesis src/mine-genesis.cpp -lcrypto
echo "✓ Compiled"
echo ""

echo "Step 2: Mining genesis blocks (this may take a few minutes)..."
echo "Mining at difficulty 0x1e0ffff0..."
./mine-genesis > genesis_output.txt 2>&1

echo "✓ Genesis blocks mined!"
echo ""

echo "Step 3: Extracting genesis block data..."
MAINNET_NONCE=$(grep -A 20 "MAINNET" genesis_output.txt | grep "^Nonce:" | head -1 | awk '{print $2}')
MAINNET_HASH=$(grep -A 20 "MAINNET" genesis_output.txt | grep "^Hash:" | head -1 | awk '{print $2}')
TESTNET_NONCE=$(grep -A 20 "TESTNET" genesis_output.txt | grep "^Nonce:" | tail -1 | awk '{print $2}')
TESTNET_HASH=$(grep -A 20 "TESTNET" genesis_output.txt | grep "^Hash:" | tail -1 | awk '{print $2}')

echo "Mainnet: nonce=$MAINNET_NONCE, hash=$MAINNET_HASH"
echo "Testnet: nonce=$TESTNET_NONCE, hash=$TESTNET_HASH"
echo ""

echo "Step 4: Updating chainparams.cpp..."
# Update mainnet genesis
sed -i "s/genesis = CreateGenesisBlock([0-9]*, [0-9]*, 0x1e0ffff0, 1, 88 \* COIN);/genesis = CreateGenesisBlock($TIMESTAMP, $MAINNET_NONCE, 0x1e0ffff0, 1, 88 * COIN);/g" src/chainparams.cpp

# Update mainnet hash assertion
sed -i "s/assert(consensus.hashGenesisBlock == uint256S(\"0x[0-9a-f]*\"));/assert(consensus.hashGenesisBlock == uint256S(\"0x$MAINNET_HASH\"));/1" src/chainparams.cpp

# Update testnet genesis (in testnet section)
sed -i "0,/genesis = CreateGenesisBlock([0-9]*, [0-9]*, 0x1e0ffff0, 1, 88 \* COIN);/{s//genesis = CreateGenesisBlock($TIMESTAMP, $MAINNET_NONCE, 0x1e0ffff0, 1, 88 * COIN);/;};/genesis = CreateGenesisBlock([0-9]*, [0-9]*, 0x1e0ffff0, 1, 88 \* COIN);/s//genesis = CreateGenesisBlock($TIMESTAMP, $TESTNET_NONCE, 0x1e0ffff0, 1, 88 * COIN)/" src/chainparams.cpp

echo "✓ Updated chainparams.cpp"
echo ""

echo "Step 5: Recompiling Whippet..."
make -j$(nproc) > /dev/null 2>&1 || make -j4
echo "✓ Compiled successfully"
echo ""

echo "Step 6: Starting Whippet node..."
./src/whippetd -daemon
sleep 3
echo "✓ Node started"
echo ""

echo "Step 7: Mining initial blocks to $MINING_ADDRESS..."
echo "Mining 101 blocks (needed for coinbase maturity)..."
./src/whippet-cli generate 10001

BALANCE=$(./src/whippet-cli getbalance)
echo ""
echo "╔════════════════════════════════════════════════════════════════════════════╗"
echo "║                           MINING COMPLETE!                                ║"
echo "╚════════════════════════════════════════════════════════════════════════════╝"
echo ""
echo "Blockchain Status:"
./src/whippet-cli getblockchaininfo | grep -E '"blocks"|"difficulty"|"bestblockhash"'
echo ""
echo "Balance: $BALANCE WHIP"
echo ""
echo "✓ Whippet is ready for use!"
echo "✓ Node is running on port 33666"
echo ""
echo "Useful commands:"
echo "  ./src/whippet-cli getblockchaininfo"
echo "  ./src/whippet-cli getbalance"
echo "  ./src/whippet-cli generatetoaddress 10 $MINING_ADDRESS"
echo "  ./src/whippet-cli stop"
echo ""
