"""Real-node smoke tests on regtest (need Docker, the built image, and free ports 18101/18102)."""
import shutil
import subprocess

import pytest

import confirm_regtest


def _image_present() -> bool:
    if not shutil.which("docker"):
        return False
    out = subprocess.run(["docker", "image", "inspect", "whippet-ubuntu24"], capture_output=True)
    return out.returncode == 0


pytestmark = pytest.mark.skipif(not _image_present(), reason="needs docker and the whippet-ubuntu24 image")


def test_far_future_r0_accepts_the_block_and_inherits_the_lead():
    m = confirm_regtest.run_far_future("R0", duration_s=240, attack_at=60, offset_s=300)
    assert m["attack_rejected"] is False
    assert m["max_lead"] >= 295
    assert m["max_stall"] <= 6


def test_far_future_r1_rejects_the_block_as_too_new():
    m = confirm_regtest.run_far_future("R1", duration_s=240, attack_at=60, offset_s=300)
    assert m["attack_rejected"] is True
    assert m["max_lead"] <= 6


def test_nodes_refuse_to_start_when_a_port_is_busy():
    import socket
    with socket.socket() as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.bind(("127.0.0.1", confirm_regtest.PORTS["a"][1]))
        s.listen(1)
        with pytest.raises(RuntimeError, match="in use"):
            with confirm_regtest.Nodes("whippet"):
                pass


def test_easing_wait_r1e_bits_match_the_python_rule_and_peers_accept():
    m = confirm_regtest.run_easing_wait("R1e", gaps=(20, 66, 156))
    assert m["all_match"] and m["all_accepted"]
    assert [r["eased"] for r in m["rows"]] == [False, True, True]
