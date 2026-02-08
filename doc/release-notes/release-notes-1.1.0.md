Whippet Core version 1.1.0 is now available from:

  <https://github.com/whippet/whippet/releases/tag/v1.1.0/>

This is a major version release introducing consensus changes including new script opcodes for the Universal Asset Protocol (UAP). All nodes must upgrade to 1.1.0 to remain on the main chain.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/whippet/whippet/issues>

To receive notifications about updates, subscribe to the release mailing list:

  <https://lists.whippet.com/mailman/listinfo/whippet-releases>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../INSTALL.md).

Note: This release contains consensus changes and is not compatible with prior
versions. All nodes must upgrade to 1.1.0.


Notable changes
================

Consensus Changes
==================

This release introduces consensus-level changes that require all nodes to upgrade.
Network compatibility and security depend on prompt adoption of this release.

Universal Asset Protocol (UAP) - Token Minting and Introspection
-----------------------------------------------------------------

Three new script opcodes have been added to support the Universal Asset Protocol,
enabling token creation, transfer, and verification within script execution:

### OP_MINT (0xb5) - Token Minting Opcode

OP_MINT enables the creation of new tokens on the blockchain. Scripts using OP_MINT
follow the pattern: `[multiplier] [salt] OP_MINT`

**Features:**
- Tokens are identified by a unique salt (≥16 bytes)
- Token supply is determined by a multiplier value (e.g., 1000 tokens per WHIP)
- Entry fee requirement: input value ≥ 1000 COIN satoshis prevents spam
- Full validation in both mempool (policy) and block (consensus) rules

**Policy Rules:**
- Salt must be ≥16 bytes
- Entry fee must be ≥1000 COIN satoshis
- Script must be exactly [data] [data] OP_MINT format

**Example:**
```
Script: 1000 "unique_salt_16bytes_or_more" OP_MINT
Result: Creates a token with 1000 tokens per WHIP
```

### OP_INSPECT (0xb7) - Output Introspection Opcode

OP_INSPECT allows scripts to examine transaction output properties at execution time.
Pattern: `[field_selector] OP_INSPECT` pushes the selected field value onto the stack.

**Supported Field Selectors:**
- 0: version (protocol version)
- 1: input_index (current input index)
- 12: output_value (current output's nValue in satoshis)
- Other field selectors for extended introspection

**Use Cases:**
- Token value verification in covenants
- Cross-output validation
- Token conservation checking

**Example:**
```
Script: 0 OP_INSPECT 1 EQUAL
Result: Pushes protocol version and verifies it equals 1
```

### OP_INSPECT_SELF (0xb6) - Script Introspection Opcode

OP_INSPECT_SELF allows scripts to examine their own scriptPubKey at execution time.
Pattern: `OP_INSPECT_SELF` pushes the entire scriptPubKey onto the stack.

**Use Cases:**
- Script self-reference and validation
- Covenant enforcement
- Template verification

**Example:**
```
Script: OP_INSPECT_SELF OP_INSPECT_SELF EQUAL
Result: Verifies the script appears twice (both outputs have same script)
```

Protocol Version Update
-----------------------

- Protocol version updated to 70016
- Nodes running versions prior to 70016 will not be compatible with this network
- Peers running older protocol versions will be disconnected

Network Activation
-------------------

The consensus changes in this release activate immediately upon block validation.
All nodes must upgrade before processing blocks containing these new opcodes.

**Upgrade Deadline:** Immediate - upgrade before first block with new opcodes


New Features
============

UAP Transaction Type Recognition
---------------------------------

New transaction script types have been added to the script type classifier:

- **TX_OP_MINT**: Scripts matching the OP_MINT pattern are now recognized and validated
- **TX_OP_TRANSFER**: Simple UAP covenant transfers (pubkey + OP_CHECKSIG) are recognized

These types enable specialized handling in the relay policy and transaction validation.


Testing and Quality Assurance
==============================

Comprehensive Python Test Suite
--------------------------------

Three new Python integration tests provide end-to-end testing of UAP features:

1. **uap_transactions.py** - Tests token mechanics
   - Token conservation validation
   - Minting and spending flow
   - Transaction validity in mempool and blocks

2. **uap_mint_transfer_spend.py** - Tests individual features
   - OP_MINT validation and constraints
   - OP_INSPECT and OP_INSPECT_SELF opcodes
   - UAP transfer covenants
   - Multi-level transaction chains
   - End-to-end token lifecycle

3. **uap_integration.py** - Integration tests (extended suite)
   - Complex multi-party scenarios
   - Covenant preservation across transactions
   - Large-scale token operations

All tests verify:
- Correct token supply and conservation
- Proper entry fee enforcement
- Salt validation requirements
- Script execution semantics
- Covenant enforcement


Building and Installation
==========================

Standard build process applies:

```bash
./autogen.sh
./configure
make
make check
sudo make install
```

For detailed build instructions, see [INSTALL.md](../INSTALL.md).


Known Issues
============

None known at release time. Please report any issues at:
<https://github.com/whippet/whippet/issues>


Upgrading from Prior Versions
==============================

**Important:** This release contains consensus changes. Follow these steps:

1. **Backup your wallet** before upgrading:
   ```bash
   whippet-cli dumpwallet backup.txt
   ```

2. **Stop your Whippet daemon**:
   ```bash
   whippet-cli stop
   ```

3. **Upgrade Whippet Core** using your package manager or by building from source

4. **Start the daemon** (blockchain will be validated during startup):
   ```bash
   whippetd
   ```

5. **Verify the upgrade**:
   ```bash
   whippet-cli getinfo
   # Should show version 1.1.0 and protocol version 70016
   ```

Block validation may take several minutes as the daemon re-validates the blockchain
with the new consensus rules. This is normal and expected.


Acknowledgments
===============

The UAP implementation and testing were developed to provide advanced script capabilities
and token support for the Whippet blockchain.

Thank you to all contributors and testers who made this release possible.


Miscellaneous
=============

- Documentation: See [doc/uap-minting-guide.md](../uap-minting-guide.md) for comprehensive
  guide to using OP_MINT and UAP features
- Source code review: Use `git log --oneline v1.0.0..v1.1.0` to review changes
- Previous releases: [Release history](./README.md)


Technical Details
=================

File Changes
------------

Key files modified in this release:

- `src/script/script.h`, `src/script/script.cpp` - Opcode definitions
- `src/script/interpreter.cpp`, `src/script/interpreter.h` - Opcode execution
- `src/script/standard.cpp`, `src/script/standard.h` - Script type classification
- `src/policy/policy.cpp` - OP_MINT policy validation
- `src/version.h` - Protocol version (70016)
- `configure.ac`, `src/clientversion.h` - Version information
- `qa/rpc-tests/` - Python test suite

Commit Hashes
-------------

Key commits in this release:

- `04dc76eee` - script: add OP_INSPECT_SELF opcode and tests
- `506e0d205` - script: add OP_INSPECT opcode and tests
- `1891aea14` - script: add OP_MINT opcode and policy/standard checks
- `0c1a8b79f` - test: register UAP Python tests in RPC test suite


Release Information
===================

**Release Date:** February 8, 2026
**Version:** 1.1.0
**Protocol Version:** 70016
**GitHub Tag:** v1.1.0

For the latest information, visit:
- Website: <https://whippet.com/>
- GitHub: <https://github.com/whippet/whippet/>
- Documentation: <https://docs.whippet.com/>
