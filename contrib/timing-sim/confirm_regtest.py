"""Replay timing scenarios on real regtest nodes and compare them with the simulator.

    python3 confirm_regtest.py --scenario far_future|walk_ahead --rules R0|R1

Two regtest nodes run in a Docker container with -timingprofile. Node B is the
honest network; node A is the attacker, whose clock (setmocktime) is moved to
stamp blocks ahead. Mismatches are reported as findings; the exit status stays 0.
"""
import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "qa" / "rpc-tests" / "test_framework"))
from authproxy import AuthServiceProxy, JSONRPCException  # noqa: E402

from chain import BASE_TIME  # noqa: E402

OLD_REPO = Path(os.environ.get("WHIPPET_OLD_REPO", REPO.parent / "whippet-old-baseline"))  # built at 89233fb96~1
IMAGE = os.environ.get("WHIPPET_TIMING_IMAGE", "whippet-ubuntu24")
PROFILES = {"R0": "whippet", "R1": "scaled-drift", "R1e": "scaled-drift-easing"}
DRIFT = {"R0": 7200, "R1": 72, "R1e": 72}
EASING_TAU = 30
EASING_FREE = 36
FLOOR = 4
PORTS = {"a": (18111, 18101), "b": (18112, 18102)}  # (p2p, rpc)
START = BASE_TIME + 100  # first mocktime; the regtest genesis block is at BASE_TIME


class Nodes:
    """Two connected regtest nodes in one container, on the host network.

    `specs` runs a different binary or extra arguments per node: {"a": (binary, extra_args), ...}. The
    current tree is /src and the old baseline (OLD_REPO) is /old.
    """

    def __init__(self, profile: str, specs: dict = None):
        self.profile = profile
        self.specs = specs or {}
        self.name = f"timing-confirm-{os.getpid()}"
        self.dir = tempfile.mkdtemp(prefix="timing-confirm-")

    def __enter__(self):
        for p2p, rpc in PORTS.values():  # a stale node on these ports would silently be reused
            for port in (p2p, rpc):
                with socket.socket() as probe:
                    probe.settimeout(0.5)
                    if probe.connect_ex(("127.0.0.1", port)) == 0:
                        shutil.rmtree(self.dir, ignore_errors=True)
                        raise RuntimeError(f"port {port} is in use; stop the other node or container first")
        cmd = []
        for node, (p2p, rpc) in PORTS.items():
            os.makedirs(f"{self.dir}/{node}")
            binary, extra = self.specs.get(node, ("/src/src/whippetd", ""))
            args = (f"-regtest -datadir=/data/{node} -port={p2p} -rpcport={rpc} -rpcuser=u -rpcpassword=p "
                    f"-listen=1 -server=1 -printtoconsole=0 {extra}")
            if self.profile:
                args += f" -timingprofile={self.profile} -timingfloor={FLOOR}"
            if node == "b":
                args += f" -addnode=127.0.0.1:{PORTS['a'][0]}"
            cmd.append(f"{binary} {args} &")
        subprocess.run(["docker", "run", "-d", "--rm", "--network", "host", "--name", self.name,
                        "-u", f"{os.getuid()}:{os.getgid()}", "-v", f"{REPO}:/src", "-v", f"{OLD_REPO}:/old:ro",
                        "-v", f"{self.dir}:/data",
                        "--entrypoint", "bash", IMAGE, "-c", " ".join(cmd) + " wait"],
                       check=True, capture_output=True)
        try:
            self.a = self._rpc("a")
            self.b = self._rpc("b")
            deadline = time.time() + 60
            while time.time() < deadline and self.b.getconnectioncount() < 1:
                time.sleep(0.5)
        except BaseException:
            self.__exit__()
            raise
        return self

    def _rpc(self, node: str) -> AuthServiceProxy:
        for _ in range(240):  # refused or an error while the node is still loading
            proxy = AuthServiceProxy(f"http://u:p@127.0.0.1:{PORTS[node][1]}", timeout=120)  # fresh: a failed one stays broken
            try:
                proxy.getblockcount()
                return proxy
            except Exception:
                time.sleep(0.25)
        raise RuntimeError(f"node {node} did not start")

    def __exit__(self, *exc):
        subprocess.run(["docker", "rm", "-f", self.name], capture_output=True)
        shutil.rmtree(self.dir, ignore_errors=True)

    def synced(self, timeout: float = 5.0) -> bool:
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.a.getbestblockhash() == self.b.getbestblockhash():
                return True
            time.sleep(0.05)
        return False


def _tip_time(node) -> int:
    return node.getblockheader(node.getbestblockhash())["time"]


class Honest:
    """Honest miner on node B: one block per 6s of mocktime, retried every second while it fails."""

    def __init__(self, nodes: Nodes):
        self.nodes = nodes
        self.addr = nodes.b.getnewaddress()
        self.pending = False
        self.stalled = 0
        self.max_stall = 0
        self.total_stall = 0

    def step(self, t: int) -> None:
        if not (self.pending or t % 6 == 0):
            return
        try:
            self.nodes.b.generatetoaddress(1, self.addr)
            self.pending = False
            self.stalled = 0
        except JSONRPCException:
            self.pending = True
            self.stalled += 1
            self.total_stall += 1
            self.max_stall = max(self.max_stall, self.stalled)


def _attack_block(nodes: Nodes, addr: str, stamp: int, now: int) -> bool:
    """Mine one block on A with its clock at `stamp`. True if B accepted it; if not, A drops it."""
    if not nodes.synced():
        raise RuntimeError("nodes out of sync before the attack block")
    nodes.a.setmocktime(stamp)
    try:
        block = nodes.a.generatetoaddress(1, addr)[0]
    except JSONRPCException:
        nodes.a.setmocktime(now)
        return False
    nodes.a.setmocktime(now)
    time.sleep(0.3)
    if nodes.b.getbestblockhash() == block:
        return True
    nodes.a.invalidateblock(block)
    nodes.synced()
    return False


def _prime(nodes: Nodes) -> None:
    """A few honest blocks so that both nodes have a chain to extend."""
    for n in (nodes.a, nodes.b):
        n.setmocktime(START)
    addr = nodes.b.getnewaddress()
    for i in range(3):
        for n in (nodes.a, nodes.b):
            n.setmocktime(START + 6 * i)
        nodes.b.generatetoaddress(1, addr)
    nodes.synced()


def run_far_future(rules: str, duration_s: int = 1200, attack_at: int = 600, offset_s: int = 300) -> dict:
    with Nodes(PROFILES[rules]) as nodes:
        _prime(nodes)
        honest = Honest(nodes)
        addr_a = nodes.a.getnewaddress()
        max_lead = 0
        attack_rejected = None
        for t in range(18, duration_s):
            now = START + t
            nodes.a.setmocktime(now)
            nodes.b.setmocktime(now)
            honest.step(t)
            if t == attack_at:
                attack_rejected = not _attack_block(nodes, addr_a, now + offset_s, now)
            max_lead = max(max_lead, _tip_time(nodes.b) - now)
        return {"attack_rejected": attack_rejected, "max_lead": max_lead,
                "max_stall": honest.max_stall, "total_stall": honest.total_stall}


# Regtest cannot replay a long fixed schedule: LWMA sees stamped solvetimes under the target
# and keeps making blocks harder, while a scripted run never slows down. Past about 1500s a
# single generatetoaddress takes minutes. The simulator has that feedback; this does not.
def run_walk_ahead(rules: str, duration_s: int = 1200, first_s: int = 60, step_s: int = 15,
                   every_s: int = 10) -> dict:
    with Nodes(PROFILES[rules]) as nodes:
        _prime(nodes)
        honest = Honest(nodes)
        addr_a = nodes.a.getnewaddress()
        max_lead = accepted = rejected = 0
        for t in range(18, duration_s):
            now = START + t
            nodes.a.setmocktime(now)
            nodes.b.setmocktime(now)
            honest.step(t)
            if t % every_s == 0:
                tip = _tip_time(nodes.b)
                latest = now + DRIFT[rules]
                want = now + first_s if tip - now < first_s else tip + step_s
                stamp = min(want, latest)
                if tip + FLOOR <= latest:  # otherwise the attacker pauses until the clock catches up
                    if _attack_block(nodes, addr_a, stamp, now):
                        accepted += 1
                    else:
                        rejected += 1
            max_lead = max(max_lead, _tip_time(nodes.b) - now)
        return {"attack_accepted": accepted, "attack_rejected": rejected, "max_lead": max_lead,
                "max_stall": honest.max_stall, "total_stall": honest.total_stall}


def _target(bits_hex: str) -> int:
    from chain import bits_to_target
    return bits_to_target(int(bits_hex, 16))


def run_walk_ahead_modelled(rules: str, duration_s: int = 1200, seed: int = 1, honest_rate: float = 1.0,
                            attacker_rate: float = 0.2, first_s: int = 60, step_s: int = 15, trace: bool = False,
                            base_shift: int = 6) -> dict:
    """Walk-ahead replay in which block finding follows the node's own target, as in the simulator.

    run_walk_ahead() mines on a fixed schedule, so LWMA keeps raising the difficulty until real
    hashing takes minutes per block. Here each second a miner finds a block with the simulator's
    probability, 1 - exp(-hashrate / 6 * target / base_target), taking the target from the node's
    getblocktemplate at the stamp it would use. The block rate then follows the difficulty, and the
    difficulty stays near where it started. Only the choice of when to mine differs: the rules run
    in the real nodes.
    """
    import math
    import random
    from chain import BASE_BITS, bits_to_target
    rng = random.Random(seed)
    # The unit of hashrate is one block per 6s at this target. It sits base_shift bits below regtest's
    # powLimit (2**255) because LWMA multiplies a sum of targets by a sum of solvetimes in 256 bits: from
    # a target near 2**255 that product wraps whenever the window is slow, which mainnet's powLimit
    # cannot do. The simulator's base target is far below the limit for the same reason.
    base_target = bits_to_target(BASE_BITS) >> base_shift

    def rate(h: float, bits_hex: str) -> float:
        return h / 6 * _target(bits_hex) / base_target

    with Nodes(PROFILES[rules]) as nodes:
        _prime(nodes)
        honest = Honest(nodes)
        addr_a = nodes.a.getnewaddress()
        max_lead = attacker_blocks = 0
        st = {"honest_expected": 0.0, "honest_drawn": 0, "honest_failed": 0, "attacker_expected": 0.0,
              "attacker_drawn": 0, "attacker_rejected": 0, "attacker_skipped": 0}
        for t in range(18, duration_s):
            now = START + t
            nodes.a.setmocktime(now)
            nodes.b.setmocktime(now)
            try:  # a refused template (tip too far ahead of the clock) is the honest stall itself
                bits = nodes.b.getblocktemplate()["bits"]
                honest.stalled = 0
            except JSONRPCException:
                bits = None
                honest.stalled += 1
                honest.total_stall += 1
                honest.max_stall = max(honest.max_stall, honest.stalled)
            # As in the simulator, a block that is not found this second is not retried: no patience.
            if bits:
                st["honest_expected"] += 1 - math.exp(-rate(honest_rate, bits))
            if bits and rng.random() < 1 - math.exp(-rate(honest_rate, bits)):
                st["honest_drawn"] += 1
                try:
                    nodes.b.generatetoaddress(1, honest.addr)
                except JSONRPCException:
                    st["honest_failed"] += 1
            tip = _tip_time(nodes.b)
            latest = now + DRIFT[rules]
            want = now + first_s if tip - now < first_s else tip + step_s
            stamp = min(want, latest)
            if tip + FLOOR <= latest:
                nodes.a.setmocktime(stamp)
                try:
                    bits = nodes.a.getblocktemplate()["bits"]
                except JSONRPCException:
                    bits = None
                nodes.a.setmocktime(now)
                if bits:
                    st["attacker_expected"] += 1 - math.exp(-rate(attacker_rate, bits))
                if bits and rng.random() < 1 - math.exp(-rate(attacker_rate, bits)):
                    st["attacker_drawn"] += 1
                    if _attack_block(nodes, addr_a, stamp, now):
                        attacker_blocks += 1
                    else:
                        st["attacker_rejected"] += 1
            else:
                st["attacker_skipped"] += 1
            max_lead = max(max_lead, _tip_time(nodes.b) - now)
        height = nodes.b.getblockcount()
        return {"blocks_per_hour": (height - 2) * 3600 / duration_s, "attacker_blocks": attacker_blocks,
                "max_lead": max_lead, "max_stall": honest.max_stall, "total_stall": honest.total_stall,
                "final_difficulty": nodes.b.getdifficulty(), **st,
                **({"chain": [(h["time"], int(h["bits"], 16), h.get("height")) for h in
                              (nodes.b.getblockheader(nodes.b.getblockhash(i)) for i in range(1, height + 1))]}
                   if trace else {})}


def run_easing_wait(rules: str = "R1e", gaps=(20, 36, 37, 51, 66, 96, 156, 306)) -> dict:
    """After the honest chain goes quiet for `gap` seconds, the next block must carry the eased bits.

    For each gap: set both nodes' clocks to tip + gap, mine one block on B, and compare its nBits with the
    Python LWMA-with-easing computed from the parent's last 11 headers. Node A must accept the block.
    """
    import lwma
    with Nodes(PROFILES[rules]) as nodes:
        _prime(nodes)
        honest = Honest(nodes)
        # Blocks at the 4s floor are faster than the 6s target, so LWMA pulls the target well below
        # powLimit and the easing below is not hidden by the clamp.
        now = START + 18
        for _ in range(60):
            now += 4
            for n in (nodes.a, nodes.b):
                n.setmocktime(now)
            nodes.b.generatetoaddress(1, honest.addr)
        rows = []
        for gap in gaps:
            nodes.synced()
            tip = nodes.b.getblockheader(nodes.b.getbestblockhash())
            recent = []
            h = tip
            for _ in range(lwma.N + 1):
                recent.append((h["time"], int(h["bits"], 16)))
                h = nodes.b.getblockheader(h["previousblockhash"])
            recent.reverse()
            now = max(now, tip["time"] + gap)
            for n in (nodes.a, nodes.b):
                n.setmocktime(now)
            block = nodes.b.generatetoaddress(1, honest.addr)[0]
            header = nodes.b.getblockheader(block)
            expected = lwma.next_bits(recent, candidate_time=header["time"], tau_wait_s=EASING_TAU,
                                      free_wait_s=EASING_FREE)
            plain = lwma.next_bits(recent)
            nodes.synced()
            rows.append({"gap": header["time"] - tip["time"], "bits": int(header["bits"], 16),
                         "expected": expected, "uneased": plain,
                         "eased": expected != plain, "bits_match": int(header["bits"], 16) == expected,
                         "peer_accepted": nodes.a.getbestblockhash() == block})
            now += 6
        return {"rows": rows, "all_match": all(r["bits_match"] for r in rows),
                "all_accepted": all(r["peer_accepted"] for r in rows)}


ACTIVATION_HEIGHT = 100


def run_activation(new_miner: bool, activation: int = ACTIVATION_HEIGHT) -> dict:
    """Old and new binaries across the drift-cap activation height (soft fork), plain regtest rules.

    The new node runs -timingdriftcapheight=`activation`; the old node is the commit before the soft
    fork. Node A mines and node B follows. Blocks are 1s apart up to `activation - 5`, then:

    new_miner=True  (A new, B old): the new node mines across the height with every block stamped
                    exactly 72s past the old node's clock. The old node must follow every block.
    new_miner=False (A old, B new): the old node mines blocks stamped 300s ahead. Before the height the
                    new node accepts them (the last one is block activation-1); from it, the new node refuses a block 73s ahead and accepts
                    one 72s ahead. After the refusal the new node mines a longer chain and the old node
                    must reorganise onto it.
    """
    new = ("/src/src/whippetd", f"-timingdriftcapheight={activation}")
    old = ("/old/src/whippetd", "")
    specs = {"a": new, "b": old} if new_miner else {"a": old, "b": new}
    out = {"activation": activation, "new_miner": new_miner}
    with Nodes("", specs) as nodes:
        miner, other = nodes.a, nodes.b
        addr = miner.getnewaddress()
        now = START
        for n in (nodes.a, nodes.b):
            n.setmocktime(now)

        def tick(secs: int = 1) -> None:
            nonlocal now
            now += secs
            for n in (nodes.a, nodes.b):
                n.setmocktime(now)

        def mine(stamp_ahead: int) -> bool:
            """Mine one block on the miner with its clock stamp_ahead seconds past the shared clock."""
            miner.setmocktime(now + stamp_ahead)
            try:
                miner.generatetoaddress(1, addr)
                return True
            except JSONRPCException:
                return False
            finally:
                miner.setmocktime(now)

        while miner.getblockcount() < activation - 5:
            tick()
            mine(0)
        nodes.synced(30)
        out["synced_before"] = nodes.a.getbestblockhash() == nodes.b.getbestblockhash()

        if new_miner:
            followed = []
            while miner.getblockcount() < activation + 20:
                tick()
                assert mine(72)
                time.sleep(0.15)
                followed.append(other.getbestblockhash() == miner.getbestblockhash())
            out.update(blocks_across=len(followed), old_followed_every_block=all(followed),
                       height=miner.getblockcount(), old_height=other.getblockcount())
            return out

        # Old miner, new validator. The last block below the activation height may be 300s ahead.
        while miner.getblockcount() < activation - 2:
            tick()
            mine(0)
        tick()
        mine(300)
        time.sleep(0.3)
        out["last_pre_activation_block_300s_accepted"] = (
            miner.getblockcount() == activation - 1 and other.getbestblockhash() == miner.getbestblockhash())
        tick(300)  # the 300s lead is gone
        nodes.synced(30)
        out["height_before"] = other.getblockcount()
        out["synced_at_activation"] = nodes.a.getbestblockhash() == nodes.b.getbestblockhash()
        # From the activation height (this is block `activation`): 72s ahead is accepted, 73s is not.
        tick()
        mine(72)
        time.sleep(0.3)
        out["post_activation_72s_accepted"] = other.getbestblockhash() == miner.getbestblockhash()
        fork_height = other.getblockcount()
        tick()
        mine(73)
        time.sleep(0.5)
        out["post_activation_73s_refused"] = other.getblockcount() == fork_height
        out["old_node_kept_it"] = miner.getblockcount() == fork_height + 1
        # The new node mines on; once its chain is longer the old node follows it.
        addr_b = other.getnewaddress()
        for _ in range(2):
            tick()
            other.generatetoaddress(1, addr_b)
        time.sleep(1)
        out["old_node_reorganised"] = miner.getbestblockhash() == other.getbestblockhash()
        out["height"] = other.getblockcount()
        return out


EASING_ACTIVATION_HEIGHT = 100
PREFORK_REPO = Path(os.environ.get("WHIPPET_PREFORK_REPO", REPO.parent / "whippet-old-prefork"))  # built at c7729597f


def run_easing_activation(new_miner: bool, activation: int = EASING_ACTIVATION_HEIGHT, gap: int = 66) -> dict:
    """Old and new binaries across the easing hard fork height. Both run every-block LWMA with easing off.

    The new node adds -timingeasingheight=`activation` (the mainnet row: tau 30, free wait 36s). The old
    node is the commit before that row (PREFORK_REPO). Blocks are 4s apart (the floor), so LWMA pulls the
    target below powLimit, except for two blocks that follow a quiet `gap` seconds after their parent:

    - block activation-3: a long gap before the height. Neither rule eases it; both nodes must agree.
    - block `activation`: a long gap from the height. The new node requires eased nBits, the old node
      the plain ones, so a block from either side is refused by the other.

    new_miner=True  (A new, B old): the new node mines the eased block; the old node refuses it. The old
                    node then mines 4s blocks (no easing involved) and the new node follows them.
    new_miner=False (A old, B new): the old node mines the plain block; the new node refuses it. The new
                    node then mines its own eased block; the old node refuses that, and the two stay apart.
    """
    import lwma
    global OLD_REPO
    OLD_REPO = PREFORK_REPO  # Nodes mounts it as /old
    new = ("/src/src/whippetd", f"-timingeasingtau=0 -timingeasingheight={activation}")
    old = ("/old/src/whippetd", "-timingeasingtau=0")
    specs = {"a": new, "b": old} if new_miner else {"a": old, "b": new}
    out = {"activation": activation, "gap": gap, "new_miner": new_miner}
    with Nodes("scaled-drift-easing", specs) as nodes:
        miner, other = nodes.a, nodes.b
        addr_a, addr_b = nodes.a.getnewaddress(), nodes.b.getnewaddress()
        now = START
        for n in (nodes.a, nodes.b):
            n.setmocktime(now)

        def clocks(t: int) -> None:
            for n in (nodes.a, nodes.b):
                n.setmocktime(t)

        def mine(node, addr, after: int) -> str:
            """One block on `node` stamped `after` seconds after the current tip (both clocks move there)."""
            nonlocal now
            now = node.getblockheader(node.getbestblockhash())["time"] + after
            clocks(now)
            return node.generatetoaddress(1, addr)[0]

        def run_to(height: int) -> None:
            while miner.getblockcount() < height:
                mine(miner, addr_a, 4)
                nodes.synced(5)

        def bits_in_force(node, candidate: int):
            """Plain and eased nBits the Python rule gives for the block after node's tip."""
            tip = node.getblockheader(node.getbestblockhash())
            recent, h = [], tip
            for _ in range(lwma.N + 1):
                recent.append((h["time"], int(h["bits"], 16)))
                h = node.getblockheader(h["previousblockhash"])
            recent.reverse()
            return (lwma.next_bits(recent), lwma.next_bits(recent, candidate_time=candidate,
                                                           tau_wait_s=EASING_TAU, free_wait_s=EASING_FREE))

        run_to(activation - 4)
        mine(miner, addr_a, gap)  # block activation-3, before the height
        time.sleep(0.3)
        out["pre_activation_long_gap_followed"] = (
            miner.getblockcount() == activation - 3 and other.getbestblockhash() == miner.getbestblockhash())
        run_to(activation - 1)
        out["synced_before"] = nodes.synced(30)
        out["height_before"] = miner.getblockcount()

        tip_time = miner.getblockheader(miner.getbestblockhash())["time"]
        plain, eased = bits_in_force(miner, tip_time + gap)
        out["eased_differs_from_plain"] = plain != eased
        # Block `activation`, after a quiet gap.
        block = mine(miner, addr_a, gap)
        header = miner.getblockheader(block)
        time.sleep(0.5)
        out["miner_bits"] = header["bits"]
        out["miner_bits_are"] = ("eased" if int(header["bits"], 16) == eased else
                                 "plain" if int(header["bits"], 16) == plain else "other")
        out["other_refused"] = (other.getblockcount() == activation - 1 and
                                other.getbestblockhash() != block)
        if new_miner:
            # The old node carries on from its own tip with ordinary 4s blocks; no block waits, so the new
            # node accepts them and, with more work, leaves the eased block behind.
            for _ in range(3):
                mine(other, addr_b, 4)
            time.sleep(1.5)
            out["new_node_followed_old_chain"] = miner.getbestblockhash() == other.getbestblockhash()
        else:
            # The new node mines its own block after the same quiet gap, which the old node refuses in turn.
            other_header = other.getblockheader(other.getbestblockhash())
            _, eased_b = bits_in_force(other, other_header["time"] + gap)
            block_b = mine(other, addr_b, gap)
            time.sleep(0.5)
            out["new_node_bits_are_eased"] = int(other.getblockheader(block_b)["bits"], 16) == eased_b
            for _ in range(3):
                mine(miner, addr_a, 4)
                mine(other, addr_b, 4)
            time.sleep(1.5)
            out["old_node_stayed_on_its_chain"] = miner.getbestblockhash() != other.getbestblockhash()
        out["height_a"], out["height_b"] = nodes.a.getblockcount(), nodes.b.getblockcount()
        return out


def compare(scenario: str, rules: str, real: dict) -> None:
    """Print the real result next to the simulator's and a verdict (a finding, never a gate)."""
    import sim
    path = Path(__file__).parent / "scenarios" / f"{scenario}.json"
    sc = json.loads(path.read_text())
    sc["legacy_miner"] = False  # real nodes only have the fixed miner
    for m in sc["miners"]:  # replay the same fixed schedule as the real run, so only the rules differ
        if m["strategy"] == "honest":
            m["params"] = {"scripted_every_s": 6}
        elif m["strategy"] == "walk_ahead":
            m["params"] = {**m["params"], "scripted_every_s": 10}
    sc["duration_s"] = 1200
    sim_res = sim.run_scenario(sc, rules, seed=1)
    print(f"\n{scenario} {rules}\n  real: {real}\n  sim:  { {k: round(v, 1) for k, v in sim_res.metrics.items()} }")
    lead_tolerance = max(6, 0.02 * max(real["max_lead"], sim_res.metrics["max_lead"]))
    ok = (abs(real["max_lead"] - sim_res.metrics["max_lead"]) <= lead_tolerance
          and abs(real["max_stall"] - sim_res.metrics["max_stall"]) <= 12)
    print("  verdict:", "agree" if ok else "FINDING: real nodes and simulator disagree")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--scenario", choices=["far_future", "walk_ahead", "walk_ahead_modelled", "easing_wait", "activation", "easing_activation"], required=True)
    ap.add_argument("--rules", choices=sorted(PROFILES))
    args = ap.parse_args()
    if args.scenario == "activation":
        print("new miner, old follower:", run_activation(True))
        print("old miner, new validator:", run_activation(False))
        return
    if args.scenario == "easing_activation":
        print("new miner, old follower:", run_easing_activation(True))
        print("old miner, new validator:", run_easing_activation(False))
        return
    if not args.rules:
        ap.error("--rules is required for this scenario")
    if args.scenario == "walk_ahead_modelled":
        print(run_walk_ahead_modelled(args.rules))
        return
    if args.scenario == "easing_wait":
        result = run_easing_wait(args.rules)
        for r in result.pop("rows"):
            print(r)
        print(result)
        return
    real = (run_far_future if args.scenario == "far_future" else run_walk_ahead)(args.rules)
    compare(args.scenario, args.rules, real)


if __name__ == "__main__":
    main()
