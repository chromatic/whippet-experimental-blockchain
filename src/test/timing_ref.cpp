// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

/**
 * Reference calculator for the timing simulator (contrib/timing-sim).
 *
 * Reads one command per line on stdin and writes one reply per line on
 * stdout. It runs the real GetNextWorkRequired() and ContextualCheckBlockHeader()
 * on a synthetic chain, under the regtest parameters selected by
 * -timingprofile / -timingfloor:
 *
 *   RESET [height [bits_hex]]         OK
 *   APPEND <time> <bits_hex>          OK <index>
 *   TIP <index>                       OK
 *   NEXTBITS <time>                   BITS <hex>
 *   CHECK <time> <bits_hex> <adjusted> OK | REJECT <reason>
 *
 * Anything else replies ERR <message>.
 */

#include "chain.h"
#include "chainparams.h"
#include "consensus/validation.h"
#include "pow.h"
#include "primitives/block.h"
#include "util.h"
#include "validation.h"

#include <deque>
#include <iostream>
#include <sstream>
#include <stdexcept>
#include <string>

namespace {

const int64_t BASE_TIME = 1767254400;
const uint32_t BASE_BITS = 0x207fffff;
const int DEFAULT_BASE_HEIGHT = 145000; // every-block retarget regime, as mainnet
const int HISTORY = 12;                 // ancestors synthesised below the base
const int64_t HISTORY_SPACING = 6;

std::deque<CBlockIndex> blocks;
CBlockIndex* tip = NULL;
std::vector<CBlockIndex*> byIndex; // index 0 is the base block

void Reset(int height, uint32_t baseBits)
{
    blocks.clear();
    byIndex.clear();
    CBlockIndex* prev = NULL;
    for (int i = 0; i < HISTORY; i++) {
        blocks.emplace_back();
        CBlockIndex& b = blocks.back();
        b.nHeight = height - (HISTORY - 1) + i;
        b.nTime = BASE_TIME - HISTORY_SPACING * (HISTORY - 1 - i);
        b.nBits = baseBits;
        b.pprev = prev;
        prev = &b;
    }
    tip = &blocks.back();
    byIndex.push_back(tip);
}

CBlockHeader MakeHeader(int64_t time, uint32_t bits)
{
    CBlockHeader header;
    header.nVersion = 0x00620004;
    header.nTime = time;
    header.nBits = bits;
    return header;
}

std::string Handle(const std::string& line)
{
    std::istringstream in(line);
    std::string cmd;
    in >> cmd;
    if (cmd == "RESET") {
        int height = DEFAULT_BASE_HEIGHT;
        std::string bits;
        int given;
        if (in >> given) { // a failed extraction would zero the default
            height = given;
            in >> bits;
        }
        Reset(height, bits.empty() ? BASE_BITS : strtoul(bits.c_str(), NULL, 16));
        return "OK";
    }
    if (cmd == "APPEND") {
        int64_t time;
        std::string bits;
        if (!(in >> time >> bits))
            return "ERR usage: APPEND <time> <bits_hex>";
        blocks.emplace_back();
        CBlockIndex& b = blocks.back();
        b.nHeight = tip->nHeight + 1;
        b.nTime = time;
        b.nBits = strtoul(bits.c_str(), NULL, 16);
        b.pprev = tip;
        tip = &b;
        byIndex.push_back(tip);
        return strprintf("OK %d", (int)byIndex.size() - 1);
    }
    if (cmd == "TIP") {
        size_t index;
        if (!(in >> index) || index >= byIndex.size())
            return "ERR bad index";
        tip = byIndex[index];
        return "OK";
    }
    if (cmd == "NEXTBITS") {
        int64_t time;
        if (!(in >> time))
            return "ERR usage: NEXTBITS <time>";
        CBlockHeader header = MakeHeader(time, 0);
        return strprintf("BITS %08x", GetNextWorkRequired(tip, &header, Params().GetConsensus(tip->nHeight + 1)));
    }
    if (cmd == "CHECK") {
        int64_t time, adjusted;
        std::string bits;
        if (!(in >> time >> bits >> adjusted))
            return "ERR usage: CHECK <time> <bits_hex> <adjusted>";
        CBlockHeader header = MakeHeader(time, strtoul(bits.c_str(), NULL, 16));
        CValidationState state;
        if (ContextualCheckBlockHeader(header, state, tip, adjusted))
            return "OK";
        return "REJECT " + state.GetRejectReason();
    }
    return "ERR unknown command";
}

} // namespace

int main(int argc, char* argv[])
{
    try {
        SetupEnvironment();
        fPrintToDebugLog = false;
        fPrintToConsole = false;
        ParseParameters(argc, argv);
        SelectParams(CBaseChainParams::REGTEST);
    } catch (const std::exception& e) {
        std::cerr << "timing_ref: " << e.what() << std::endl;
        return 1;
    }
    Reset(DEFAULT_BASE_HEIGHT, BASE_BITS);

    std::string line;
    while (std::getline(std::cin, line)) {
        if (line.empty())
            continue;
        std::string reply;
        try {
            reply = Handle(line);
        } catch (const std::exception& e) {
            reply = std::string("ERR ") + e.what();
        }
        std::cout << reply << std::endl;
    }
    return 0;
}
