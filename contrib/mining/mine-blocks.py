#!/usr/bin/env python3
"""
Mine blocks on the Whippet network at a steady pace (every 30 seconds).
Optionally generate transactions between configured addresses.
"""

import sys
import time
import random
import json
import os
from datetime import datetime
from pathlib import Path

# Add the test framework to path
sys.path.append('qa/rpc-tests/test_framework')
from authproxy import AuthServiceProxy

def load_addresses(config_file):
    """Load addresses from JSON configuration file."""
    if not os.path.exists(config_file):
        print(f"Error: Configuration file not found: {config_file}")
        print("Create a JSON file with the following format:")
        print("""
{
  "mining_address": "YOUR_MINING_ADDRESS",
  "recipient_addresses": [
    "ADDRESS1",
    "ADDRESS2",
    "ADDRESS3"
  ]
}
""")
        sys.exit(1)

    try:
        with open(config_file, 'r') as f:
            config = json.load(f)
        
        if 'mining_address' not in config:
            print("Error: 'mining_address' not found in configuration file")
            sys.exit(1)
        
        mining_address = config['mining_address']
        recipient_addresses = config.get('recipient_addresses', [])
        
        if not mining_address:
            print("Error: 'mining_address' cannot be empty")
            sys.exit(1)
        
        print(f"Loaded configuration from {config_file}")
        print(f"  Mining address: {mining_address}")
        print(f"  Recipient addresses: {len(recipient_addresses)}")
        
        return mining_address, recipient_addresses
    
    except json.JSONDecodeError as e:
        print(f"Error: Invalid JSON in configuration file: {e}")
        sys.exit(1)
    except Exception as e:
        print(f"Error loading configuration: {e}")
        sys.exit(1)

def mine_blocks(rpc_url, num_blocks=100, mining_address=None, recipient_addresses=None, 
                enable_transactions=False, block_interval=30):
    """Mine blocks at a steady pace with optional transactions."""

    # Use a long timeout for mining operations (10 minutes per block)
    rpc = AuthServiceProxy(rpc_url, timeout=600)

    # Get starting block height
    start_height = rpc.getblockcount()
    print(f"Starting at block height: {start_height}")
    print(f"Mining {num_blocks} blocks at {block_interval}s intervals...")
    if mining_address:
        print(f"Mining to address: {mining_address}")
    if enable_transactions and recipient_addresses:
        print(f"Transactions enabled: 3-5 txs per block to {len(recipient_addresses)} recipients")
    print()
    print(f"{'Block':<8} {'Time':<20} {'Mine (s)':<12} {'Wait (s)':<12} {'Difficulty':<15}")
    print("-" * 85)

    total_start = time.time()
    blocks_before_240 = []
    blocks_after_240 = []

    for i in range(num_blocks):
        block_num = start_height + i + 1
        iteration_start = time.time()

        start_dt = datetime.fromtimestamp(iteration_start).strftime('%Y-%m-%d %H:%M:%S')

        # Mine one block with retry on RPC errors
        max_retries = 5
        retry_count = 0
        mine_start = time.time()
        
        while retry_count < max_retries:
            try:
                if mining_address:
                    blockhash = rpc.generatetoaddress(1, mining_address)[0]
                else:
                    blockhash = rpc.generate(1)[0]
                break  # Success, exit retry loop
            except Exception as e:
                retry_count += 1
                print(f"  [Retry {retry_count}/{max_retries}] RPC error: {e}")
                if retry_count >= max_retries:
                    print(f"  [FAILED] Skipping block {block_num} after {max_retries} retries")
                    continue
                time.sleep(2)  # Wait before retry

        if retry_count >= max_retries:
            continue  # Skip this block and move to next

        mine_time = time.time() - mine_start

        # Get block difficulty
        try:
            block_info = rpc.getblock(blockhash)
            difficulty = block_info['difficulty']
        except Exception as e:
            print(f"  [Warning] Could not get block info: {e}")
            difficulty = 0.0

        # Make transactions if enabled
        if enable_transactions and recipient_addresses:
            num_txs = random.randint(3, 5)
            for tx_num in range(num_txs):
                try:
                    # Random amount between 2k and 10k coins per transaction
                    amount = random.randint(2000, 10000)
                    # Random recipient address
                    recipient = random.choice(recipient_addresses)

                    # Send transaction
                    txid = rpc.sendtoaddress(recipient, amount)
                    print(f"  [TX {tx_num+1}/{num_txs}] {amount:,} COIN → {recipient[:10]}... (txid: {txid[:16]}...)")
                except Exception as e:
                    print(f"  [TX {tx_num+1}/{num_txs}] Failed: {e}")

        # Calculate wait time to maintain block interval
        elapsed = time.time() - iteration_start
        wait_time = max(0, block_interval - elapsed)
        
        print(f"{block_num:<8} {start_dt:<20} {mine_time:<12.3f} {wait_time:<12.1f} {difficulty:<15.6f}")

        # Track timing for averaging (before and after block 240)
        if block_num <= 240:
            blocks_before_240.append(mine_time)
        else:
            blocks_after_240.append(mine_time)

        # Wait for next block interval
        if i < num_blocks - 1:  # Don't wait after last block
            time.sleep(wait_time)

    total_time = time.time() - total_start
    avg_time = total_time / num_blocks

    print()
    print(f"Total time: {total_time:.2f} seconds")
    print(f"Average time per block: {avg_time:.6f} seconds")

    # Print separate averages for before/after block 240
    if blocks_before_240:
        avg_before = sum(blocks_before_240) / len(blocks_before_240)
        print(f"Average mining time before block 240 ({len(blocks_before_240)} blocks): {avg_before:.6f} seconds")

    if blocks_after_240:
        avg_after = sum(blocks_after_240) / len(blocks_after_240)
        print(f"Average mining time after block 240 ({len(blocks_after_240)} blocks): {avg_after:.6f} seconds")

    print(f"Final block height: {rpc.getblockcount()}")

if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(
        description='Mine Whippet blocks at a steady pace with optional transactions',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Example configuration file (addresses.json):
  {
    "mining_address": "YOUR_WHIPPET_ADDRESS",
    "recipient_addresses": [
      "ADDRESS1",
      "ADDRESS2",
      "ADDRESS3"
    ]
  }

Examples:
  # Mine 100 blocks every 30 seconds
  python3 mine-blocks.py -n 100 -c addresses.json

  # Mine with transactions enabled every 60 seconds
  python3 mine-blocks.py -n 50 -c addresses.json --enable-transactions --interval 60

  # Mine to regtest network
  python3 mine-blocks.py -n 10 -c addresses.json --regtest
        """
    )
    parser.add_argument('-n', '--num-blocks', type=int, default=100,
                        help='Number of blocks to mine (default: 100)')
    parser.add_argument('-c', '--config', type=str, required=True,
                        help='JSON configuration file with addresses (required)')
    parser.add_argument('--enable-transactions', action='store_true',
                        help='Generate 3-5 transactions per block')
    parser.add_argument('--interval', type=int, default=30,
                        help='Seconds between blocks (default: 30)')
    parser.add_argument('--rpc-url', type=str,
                        default='http://rpcuser:rpcpass@127.0.0.1:33665',
                        help='RPC URL (default: mainnet port 33665)')
    parser.add_argument('--datadir', type=str, default=None,
                        help='Data directory (will start daemon if provided)')
    parser.add_argument('--regtest', action='store_true',
                        help='Use regtest mode (default: mainnet)')

    args = parser.parse_args()

    # Load addresses from configuration file
    mining_address, recipient_addresses = load_addresses(args.config)

    # Validate transaction settings
    if args.enable_transactions and not recipient_addresses:
        print("Warning: --enable-transactions specified but no recipient_addresses in config")
        print("         Transactions will be disabled")
        args.enable_transactions = False

    # If datadir is provided, start the daemon first
    if args.datadir:
        import subprocess
        import os

        network_flag = '-regtest' if args.regtest else ''
        print(f"Starting whippetd with datadir: {args.datadir}")
        daemon_cmd = ['./src/whippetd', '-daemon', f'-datadir={args.datadir}',
                      '-rpcuser=rpcuser', '-rpcpassword=rpcpass']
        if network_flag:
            daemon_cmd.append(network_flag)
        subprocess.run(daemon_cmd)
        time.sleep(5)  # Give daemon time to start and initialize

        # Update RPC URL based on network
        if args.regtest and args.rpc_url == 'http://rpcuser:rpcpass@127.0.0.1:33665':
            args.rpc_url = 'http://rpcuser:rpcpass@127.0.0.1:53665'

    try:
        mine_blocks(args.rpc_url, args.num_blocks, mining_address, 
                    recipient_addresses, args.enable_transactions, args.interval)
    except KeyboardInterrupt:
        print("\n\nInterrupted by user")
    except Exception as e:
        print(f"\nError: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)
    finally:
        # Stop daemon if we started it
        if args.datadir:
            print("\nStopping daemon...")
            network_flag = '-regtest' if args.regtest else ''
            stop_cmd = ['./src/whippet-cli', f'-datadir={args.datadir}', 'stop']
            if network_flag:
                stop_cmd.insert(1, network_flag)
            subprocess.run(stop_cmd, stderr=subprocess.DEVNULL)
