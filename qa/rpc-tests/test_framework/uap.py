#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""Shared helpers for the UAP functional tests.

Every UAP test file used to carry its own copy of mint_script,
transfer_script and sign_spend. That is how all five of them came to be
testing the v1 format at once: the covenant changed, the C++ and the unit
tests moved with it, and five separate Python copies of the old shape stayed
behind -- still green, because a script the interpreter no longer recognises
is simply a script, and a test that only asserts "rejected" cannot tell
"rejected for the reason I meant" from "rejected because this is no longer a
covenant at all".

One definition, imported everywhere, means the next format change breaks
every test that needs to change, loudly, in one place.

The v2 covenant format:

    mint      <pubkey> <multiplier> OP_MINT
    transfer  <pubkey> <multiplier> <origin32> OP_MINT_TRANSFER

A mint carries no origin: its lineage is derived from the outpoint it is
spent at, which it cannot contain without circularity. A transfer carries
its lineage forward unchanged.
"""

import hashlib
import struct

from .script import (
    CScript, OP_MINT, OP_MINT_TRANSFER, SignatureHash, SIGHASH_ALL,
    OP_DUP, OP_HASH160, OP_EQUALVERIFY, OP_CHECKSIG, hash160,
)
from .key import CECKey

COIN = 100000000

# src/script/interpreter.cpp, OP_MINT rule 2.
MINT_ENTRY_FEE = 1000 * COIN
# rule 3. src/script/script.h, UAP_ORIGIN_SIZE.
ORIGIN_SIZE = 32
# rule 4.
MAX_UAP_MULTIPLIER = 2147483647
MAX_VIRTUAL_BALANCE = 1 << 48

FEE = 1000000  # RECOMMENDED_MIN_TX_FEE, src/amount.h

# A mandatory failure is a consensus rejection; a non-mandatory one is only
# policy. Tests that mean "consensus refuses this" must assert the first --
# if one ever reports "non-mandatory", the rule has become advisory.
MANDATORY = "mandatory-script-verify-flag-failed"
NON_MANDATORY = "non-mandatory-script-verify-flag"



def make_key(seed):
    """A deterministic compressed key. `seed` must be 32 bytes."""
    assert len(seed) == 32, "a 32-byte seed, so fixtures are reproducible"
    key = CECKey()
    key.set_secretbytes(seed)
    key.set_compressed(True)
    return key


def origin_of(txid_hex, n):
    """A lineage's identity: SHA256(txid_internal_bytes || n as 4-byte LE).

    Spelled out here rather than obtained from the node, because this
    formula IS the definition of a lineage and three implementations have to
    agree on it byte for byte (this one, contrib/uap-js, contrib/uaptx).
    `txid_hex` is in display order, so it is reversed to internal byte
    order first -- getting that backwards produces a plausible-looking
    32 bytes that no node will ever agree with.
    """
    txid_internal = bytes.fromhex(txid_hex)[::-1]
    return hashlib.sha256(txid_internal + struct.pack("<I", n)).digest()


def mint_script(pubkey, multiplier):
    """<pubkey> <multiplier> OP_MINT -- a fresh, unassigned position."""
    return CScript([pubkey, multiplier, OP_MINT])


def transfer_script(pubkey, multiplier, origin):
    """<pubkey> <multiplier> <origin32> OP_MINT_TRANSFER -- a position in a lineage."""
    return CScript([pubkey, multiplier, origin, OP_MINT_TRANSFER])


def p2pkh_script(pubkey):
    return CScript([OP_DUP, OP_HASH160, hash160(pubkey), OP_EQUALVERIFY, OP_CHECKSIG])


def sign_spend(script_code, key, tx, n_in, hashtype=SIGHASH_ALL):
    """Sign input n_in against a UAP covenant scriptPubKey.

    Spending a UAP output needs scriptSig = <sig> and nothing else: the
    pubkey, multiplier and origin all come from the scriptPubKey being
    spent, not from the scriptSig.
    """
    sighash, err = SignatureHash(script_code, tx, n_in, hashtype)
    assert err is None, "SignatureHash failed: %s" % (err,)
    sig = key.sign(sighash) + bytes([hashtype & 0xff])
    return CScript([sig])
