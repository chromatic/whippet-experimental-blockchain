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

IMAGE = os.environ.get("WHIPPET_TIMING_IMAGE", "whippet-ubuntu24")
PROFILES = {"R0": "whippet", "R1": "scaled-drift"}
DRIFT = {"R0": 7200, "R1": 72}
FLOOR = 4
PORTS = {"a": (18111, 18101), "b": (18112, 18102)}  # (p2p, rpc)
START = BASE_TIME + 100  # first mocktime; the regtest genesis block is at BASE_TIME


class Nodes:
    """Two connected regtest nodes in one container, on the host network."""

    def __init__(self, profile: str):
        self.profile = profile
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
            args = (f"-regtest -datadir=/data/{node} -port={p2p} -rpcport={rpc} -rpcuser=u -rpcpassword=p "
                    f"-timingprofile={self.profile} -timingfloor={FLOOR} -listen=1 -server=1 -printtoconsole=0")
            if node == "b":
                args += f" -addnode=127.0.0.1:{PORTS['a'][0]}"
            cmd.append(f"/src/src/whippetd {args} &")
        subprocess.run(["docker", "run", "-d", "--rm", "--network", "host", "--name", self.name,
                        "-u", f"{os.getuid()}:{os.getgid()}", "-v", f"{REPO}:/src", "-v", f"{self.dir}:/data",
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
    ap.add_argument("--scenario", choices=["far_future", "walk_ahead"], required=True)
    ap.add_argument("--rules", choices=sorted(PROFILES), required=True)
    args = ap.parse_args()
    real = (run_far_future if args.scenario == "far_future" else run_walk_ahead)(args.rules)
    compare(args.scenario, args.rules, real)


if __name__ == "__main__":
    main()
