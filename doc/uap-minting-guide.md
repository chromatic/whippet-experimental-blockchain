# UAP Minting and Token Spending Guide

This guide explains how to create OP_MINT transactions to mint tokens and spend those tokens using UAP (Universal Asset Protocol) smart contracts.

## Table of Contents

1. [Overview](#overview)
2. [Creating OP_MINT Transactions](#creating-opmint-transactions)
3. [Token Transfer via Covenants](#token-transfer-via-covenants)
4. [Spending Minted Tokens](#spending-minted-tokens)
5. [Complete Example: End-to-End Workflow](#complete-example-end-to-end-workflow)
6. [Advanced Features](#advanced-features)

## Overview

> **Design note:** an earlier version of OP_MINT required no signature to
> spend, which meant the real WHIP value locked in a mint output was
> spendable by anyone, not just the intended recipient, and the declared
> multiplier was discarded rather than enforced. The design below fixes both:
> spending now requires the recipient's signature, and every output of a
> spending transaction must carry the same, enforced multiplier.

### What is OP_MINT?

OP_MINT is an opcode used as an output's scriptPubKey to mint new tokens on
the Whippet blockchain. Like any Bitcoin-style script, it is evaluated when
that output is later *spent*, not when it is created — so all of its rules
are enforced at spend time, against the position's own declared fields:

1. **Signature**: only the recipient pubkey embedded in the script may authorize a spend
2. **Entry Fee**: the position must hold ≥ 1000 COIN satoshis to mint
3. **Salt Validation**: the salt (unique identifier) must be at least 16 bytes
4. **Overflow Guard**: `(input value in whole coins) × multiplier` must not exceed 2^48
5. **One-Shot**: a mint transaction may not also spend another UAP position as a sibling input
6. **Strict Script**: every output of the spending transaction must itself be a conforming `OP_MINT_TRANSFER` covenant carrying the same multiplier, with total output value not exceeding the input's value (the difference becomes miner fee)

### Token Multiplier

Tokens use a **multiplier** to represent fractional amounts:
- `Multiplier` = 10 means 1 WHIP represents 10 tokens
- `Multiplier` = 1000 means 1 WHIP represents 1,000 tokens
- Token value = output_value (satoshis) × multiplier / 1

**Example**: If you mint 1 WHIP (100,000 satoshis) with multiplier 1000, you create 100,000,000 tokens.

The multiplier is declared in the output's own script and is read back by
`OP_INSPECT` (selector 11, "virtual balance") — it is not a hardcoded
constant, and every output in a transfer chain must carry the same value as
the position it spends.

### Entry Fee Requirement

To prevent spam, a fresh OP_MINT requires the position being minted to hold
value ≥ 1000 COIN satoshis. This is checked when that position is later
spent (against its own value), which is equivalent to checking it at mint
time since neither the script nor the value can change in between. Transfers
(`OP_MINT_TRANSFER`) are not subject to this floor — only the initial mint.

## Creating OP_MINT Transactions

### Step 1: Build the OP_MINT Script

The OP_MINT script consists of four elements:

```
<recipient_pubkey> <multiplier> <salt> OP_MINT
```

**Parameters:**
- `recipient_pubkey`: the only key that may authorize spending this position
- `multiplier`: Integer defining token granularity (e.g., 100, 1000)
- `salt`: Unique byte string ≥ 16 bytes to identify this token

**Example using Whippet RPC:**

```bash
# Create a script with a recipient pubkey, multiplier=1000, and a unique salt
# recipient_pubkey: 33-byte compressed pubkey
# multiplier: 1000 (varint)
# salt: "my_token_salt_16" (16 bytes)
# opcode: OP_MINT

scriptPubKey = [recipient_pubkey, 1000, "my_token_salt_16", OP_MINT]
```

### Step 2: Create and Fund the Transaction

1. **Get a UTXO** with sufficient value (≥ 1000 COIN satoshis):
   ```bash
   utxo=$(whippet-cli listunspent | jq '.[0]')
   txid=$(echo $utxo | jq -r '.txid')
   vout=$(echo $utxo | jq -r '.vout')
   amount=$(echo $utxo | jq -r '.amount')
   ```

2. **Create a transaction** spending this UTXO:
   ```bash
   # Create transaction with inputs and outputs
   inputs='[{"txid":"'$txid'", "vout":'$vout'}]'
   outputs='{"'$(whippet-cli getnewaddress)'": '$(echo "$amount - 0.01" | bc)'}'
   rawtx=$(whippet-cli createrawtransaction "$inputs" "$outputs")
   ```

3. **Modify the scriptPubKey** to use OP_MINT (use raw transaction manipulation):
   - Replace the default scriptPubKey of the first output with your OP_MINT script
   - Set the output value to your desired token amount in WHIP

4. **Sign the transaction**:
   ```bash
   signed=$(whippet-cli signrawtransaction "$rawtx" | jq -r '.hex')
   ```

5. **Broadcast to the network**:
   ```bash
   txid=$(whippet-cli sendrawtransaction "$signed")
   whippet-cli generate 1
   echo "Minted tokens in transaction: $txid"
   ```

### Example OP_MINT Transaction

Here's a concrete example using Python (similar to the test framework):

```python
from test_framework.messages import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import CScript, OP_MINT

# 1. Get a UTXO
utxo = node.listunspent()[0]
txid = utxo["txid"]
vout = utxo["vout"]
amount = utxo["amount"]

# 2. Define token parameters
recipient_key = CECKey()
recipient_key.set_secretbytes(b"recipient_key_unique_bytes_123")
recipient_pubkey = recipient_key.get_pubkey()
multiplier = 1000  # 1 WHIP = 1,000 tokens
salt = b"my_unique_token_salt_1234"  # Must be ≥ 16 bytes

# 3. Build OP_MINT script
mint_script = CScript([recipient_pubkey, multiplier, salt, OP_MINT])

# 4. Create the transaction
inputs = [{"txid": txid, "vout": vout}]
outputs = {node.getnewaddress(): float(amount) - 0.01}
rawtx = node.createrawtransaction(inputs, outputs)

# 5. Modify scriptPubKey to OP_MINT
tx = FromHex(CTransaction(), rawtx)
tx.vout[0].scriptPubKey = mint_script
rawtx = ToHex(tx)

# 6. Sign and broadcast
signed = node.signrawtransaction(rawtx)["hex"]
txid = node.sendrawtransaction(signed)
node.generate(1)  # Include in a block

print(f"Token minted in transaction: {txid}")
print(f"Token supply: {amount} WHIP × {multiplier} = {amount * multiplier} tokens")
```

## Token Transfer via Covenants

Tokens created with OP_MINT are constrained by a dedicated **OP_MINT_TRANSFER**
covenant opcode (not a generic hand-assembled script) that enforces how they
can be transferred. This is checked by consensus directly, not by convention.

### UAP Transfer Template

```
<recipient_pubkey> <multiplier> OP_MINT_TRANSFER
```

**What it does, when this output is later spent:**
1. Requires a valid signature from `recipient_pubkey`
2. Requires every output of the spending transaction to itself be an
   `OP_MINT_TRANSFER` covenant carrying the *same* multiplier
3. Requires `sum(output values) <= input value` (tokens are conserved; the
   difference becomes miner fee, never new tokens)

### Building a Transfer Script

```python
def build_uap_transfer_script(recipient_pubkey, multiplier):
    """Build a UAP_TRANSFER covenant output script."""
    return CScript([recipient_pubkey, multiplier, OP_MINT_TRANSFER])
```

### Spending Minted Tokens

To spend tokens from an OP_MINT or OP_MINT_TRANSFER output, you must:

1. **Create a new transaction** that spends the position
2. **Set every output to an `OP_MINT_TRANSFER` covenant** carrying the same multiplier
3. **Ensure token conservation**: `sum(output values) <= input value`
4. **Sign scriptSig with the recipient key** named in the position being spent — the signature is verified against `SignatureHash(scriptCode, tx, nIn, SIGHASH_ALL, ...)`, the same construction used for a normal P2PK spend

### Example: Spending Minted Tokens

```python
# 1. Get the minted output from a previous OP_MINT transaction
mint_txid = "..."  # From earlier OP_MINT transaction
mint_tx = node.getrawtransaction(mint_txid, True)
minted_output = mint_tx["vout"][0]
mint_value = minted_output["value"]  # In WHIP
mint_script = CScript(bytes.fromhex(minted_output["scriptPubKey"]["hex"]))

# 2. Create recipient keys and covenant. minter_key must be the same key
#    named in the OP_MINT script being spent.
recipient_key = CECKey()
recipient_key.set_secretbytes(b"recipient_key_unique_bytes_123")
recipient_pubkey = recipient_key.get_pubkey()

covenant_script = build_uap_transfer_script(recipient_pubkey, multiplier)

# 3. Create spend transaction
spend_input = CTxIn(COutPoint(int(mint_txid, 16), 0))
spend_tx = CTransaction()
spend_tx.vin = [spend_input]

# Calculate output amounts (conserving tokens)
output_amount = mint_value - 0.005  # Leave 5 millitoshi for fees
spend_tx.vout = [
    CTxOut(int(output_amount * COIN), covenant_script)
]

# 4. Sign with the key named in the position being spent (mint_script here)
sighash = SignatureHash(mint_script, spend_tx, 0, SIGHASH_ALL, 0, SIGVERSION_BASE)
sig = minter_key.sign(sighash) + bytes([SIGHASH_ALL])
spend_tx.vin[0].scriptSig = CScript([sig])
spend_raw = ToHex(spend_tx)

# 5. Broadcast
spend_txid = node.sendrawtransaction(spend_raw)
node.generate(1)

print(f"Tokens spent and transferred in: {spend_txid}")
print(f"Token conservation: {mint_value} WHIP input → {output_amount} WHIP output")
```

## Complete Example: End-to-End Workflow

> **Note:** the walkthrough below has not yet been updated to the
> `<recipient_pubkey> <multiplier> [<salt>] OP_MINT[_TRANSFER]` script format
> and signed-spend flow described above (it still uses the old two-field
> mint script and the old `OP_INSPECT`-based covenant). Use the "Building a
> Transfer Script" and "Spending Minted Tokens" sections above as the source
> of truth; this section is a known follow-up.

This example demonstrates the complete flow:
1. Mint 10,000 tokens
2. Transfer to recipient
3. Recipient spends tokens

```python
import sys
from decimal import Decimal
from test_framework.test_framework import BitcoinTestFramework
from test_framework.messages import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import CScript, OP_MINT
from test_framework.util import *

COIN = 100000000

def build_mint_script(multiplier, salt):
    return CScript([multiplier, salt, OP_MINT])

def build_uap_transfer_script(multiplier, recipient_pubkey):
    from test_framework.script import OP_INSPECT, OP_INSPECT_SELF, OP_DUP, OP_EQUAL, OP_VERIFY, OP_CHECKSIG
    return CScript([
        recipient_pubkey, multiplier, OP_INSPECT, OP_INSPECT_SELF, OP_INSPECT_SELF,
        OP_DUP, OP_INSPECT, OP_INSPECT_SELF, OP_INSPECT_SELF, OP_EQUAL, OP_VERIFY,
        recipient_pubkey, OP_CHECKSIG
    ])

class TokenEndToEndTest(BitcoinTestFramework):
    def setup_network(self):
        self.setup_nodes()

    def run_test(self):
        node = self.nodes[0]

        print("\n=== Step 1: Mint 10,000 tokens ===")

        # Get UTXO
        utxo = node.listunspent()[0]
        multiplier = 1000
        salt = b"e2e_test_salt_16_bytes_plus"

        # Build mint script
        mint_script = build_mint_script(multiplier, salt)

        # Create transaction
        inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
        amount = float(utxo["amount"])
        outputs = {node.getnewaddress(): amount - 0.01}
        rawtx = node.createrawtransaction(inputs, outputs)

        # Modify scriptPubKey
        tx = FromHex(CTransaction(), rawtx)
        tx.vout[0].scriptPubKey = mint_script
        rawtx = ToHex(tx)

        # Sign and broadcast
        signed = node.signrawtransaction(rawtx)["hex"]
        mint_txid = node.sendrawtransaction(signed)
        node.generate(1)

        mint_tx = node.getrawtransaction(mint_txid, True)
        mint_value = Decimal(str(mint_tx["vout"][0]["value"]))
        total_tokens = mint_value * multiplier

        print(f"✓ Minted: {mint_value} WHIP = {total_tokens} tokens")
        print(f"✓ Mint TxID: {mint_txid}")

        print("\n=== Step 2: Transfer tokens to recipient ===")

        # Create recipient key
        recipient_key = CECKey()
        recipient_key.set_secretbytes(b"recipient_private_key_16bytes_1")
        recipient_pubkey = recipient_key.get_pubkey()

        # Build covenant script
        covenant_script = build_uap_transfer_script(multiplier, recipient_pubkey)

        # Create spend transaction
        spend_input = CTxIn(COutPoint(int(mint_txid, 16), 0))
        spend_tx = CTransaction()
        spend_tx.vin = [spend_input]

        transfer_amount = mint_value - Decimal('0.005')
        spend_tx.vout = [
            CTxOut(int(transfer_amount * COIN), covenant_script)
        ]

        # Sign (empty scriptSig for OP_MINT)
        spend_tx.vin[0].scriptSig = CScript([])
        spend_raw = ToHex(spend_tx)

        # Broadcast
        transfer_txid = node.sendrawtransaction(spend_raw)
        node.generate(1)

        transfer_value = Decimal(str(transfer_amount))
        transfer_tokens = transfer_value * multiplier

        print(f"✓ Transferred: {transfer_value} WHIP = {transfer_tokens} tokens")
        print(f"✓ Transfer TxID: {transfer_txid}")

        print("\n=== Step 3: Recipient spends tokens ===")

        # Get transfer output
        transfer_tx = node.getrawtransaction(transfer_txid, True)
        transfer_output = transfer_tx["vout"][0]

        # Recipient creates a new transaction
        recipient_spend_input = CTxIn(COutPoint(int(transfer_txid, 16), 0))
        recipient_spend_tx = CTransaction()
        recipient_spend_tx.vin = [recipient_spend_input]

        spend_amount = transfer_value - Decimal('0.001')
        recipient_spend_tx.vout = [
            CTxOut(int(spend_amount * COIN), build_uap_transfer_script(multiplier, recipient_pubkey))
        ]

        # Recipient signs with their key
        recipient_spend_tx.vin[0].scriptSig = CScript([recipient_pubkey.to_bytes()])
        recipient_spend_raw = ToHex(recipient_spend_tx)

        # Broadcast
        final_txid = node.sendrawtransaction(recipient_spend_raw)
        node.generate(1)

        final_value = Decimal(str(spend_amount))
        final_tokens = final_value * multiplier

        print(f"✓ Recipient spent: {final_value} WHIP = {final_tokens} tokens")
        print(f"✓ Spend TxID: {final_txid}")

        print("\n=== Token Chain Verification ===")
        print(f"Minted:    {mint_value} WHIP = {total_tokens} tokens")
        print(f"Final:     {final_value} WHIP = {final_tokens} tokens")
        print(f"Total fee: {mint_value - final_value} WHIP")
        print(f"✓ End-to-end token workflow completed successfully!")

if __name__ == "__main__":
    test = TokenEndToEndTest()
    test.main()
```

## Advanced Features

### OP_INSPECT: Output Introspection

`OP_INSPECT` allows you to read properties of transaction outputs within a script:

```
index OP_INSPECT → nValue (on stack)
```

**Usage example:**
```
# Verify output 0 has value 1000 satoshis
0 OP_INSPECT 1000 OP_EQUAL
```

### OP_INSPECT_SELF: Script Introspection

`OP_INSPECT_SELF` pushes the current script onto the stack, enabling self-referential covenant scripts:

```
OP_INSPECT_SELF → scriptPubKey (on stack)
```

**Usage example - verify outputs use same script:**
```
OP_INSPECT_SELF OP_INSPECT_SELF OP_EQUAL
```

### Multi-Level Transactions

You can build complex transaction chains with token splits and recombinations:

1. **Minting**: Create initial token supply
2. **Level 1**: Mint → Multiple recipients (1-to-N transfer)
3. **Level 2**: Each recipient can further transfer (N-to-M transfer)
4. **Level N**: Unlimited depth of transfers with covenant preservation

**Key principles:**
- Tokens are conserved at each level (tokens_in ≥ tokens_out + fees)
- Each output must use a covenant script to maintain token validity
- Transaction fees are paid from token value (reducing total token supply by fee amount)

### Token Fee Calculations

When transferring tokens, calculate fees as follows:

```
Input tokens:  input_value × multiplier
Output tokens: sum(output_values) × multiplier
Fee tokens:    Input tokens - Output tokens

Example:
- Input:  1 WHIP × 1000 = 1,000 tokens
- Fee:    0.001 WHIP (1000 × 1000 = 1,000 tokens burned)
- Output: 0.999 WHIP × 1000 = 999,000 tokens
- Net:    1,000,000 - 999,000 = 1,000 tokens paid as fees
```

## Testing Your Implementation

The Whippet test framework includes comprehensive tests for OP_MINT:

```bash
# Run all UAP tests
make check

# Run only OP_MINT tests
qa/rpc-tests/uap_mint_transfer_spend.py
```

## References

- [OP_MINT Specification](doc/uap-specification.md)
- [UAP Smart Contract Guide](doc/uap-smart-contracts.md)
- [Whippet RPC API](doc/REST-interface.md)
- [Transaction Creation Guide](doc/developer-notes.md)
