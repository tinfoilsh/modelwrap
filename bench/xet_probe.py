"""Definitively detect which download path huggingface_hub takes.

Monkeypatches `xet_get` and `http_get` in file_download.py so we can see
exactly which one is called for a single file, plus snapshots established
TCP connections during the download to fingerprint the endpoint
(Xet native CAS -> cas-server.xethub.hf.co ; bridge/CDN -> *.cloudfront.net).

Runs twice: Xet enabled, then HF_HUB_DISABLE_XET=1, to see if disabling
changes the path or the peers.
"""

import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.request

REPO = os.environ.get("PROBE_REPO", "Qwen/Qwen2.5-0.5B-Instruct")
FILE = os.environ.get("PROBE_FILE", "model.safetensors")
REV = os.environ.get("PROBE_REV", "main")

import huggingface_hub.file_download as fd  # noqa: E402

_orig_xet = fd.xet_get if hasattr(fd, "xet_get") else None
_orig_http = fd.http_get

calls = {"xet_get": 0, "http_get": 0}


def spy_xet(*a, **k):
    calls["xet_get"] += 1
    print("  >>> xet_get() CALLED (native Xet CAS protocol)", flush=True)
    return _orig_xet(*a, **k)


def spy_http(*a, **k):
    calls["http_get"] += 1
    print("  >>> http_get() CALLED (plain HTTPS / bridge redirect)", flush=True)
    return _orig_http(*a, **k)


if _orig_xet is not None:
    fd.xet_get = spy_xet
fd.http_get = spy_http

from huggingface_hub import hf_hub_download  # noqa: E402
from huggingface_hub.utils._runtime import is_xet_available  # noqa: E402


def api(path):
    return json.load(urllib.request.urlopen(f"https://huggingface.co{path}"))


def xet_hash_present():
    tree = api(f"/api/models/{REPO}/tree/{REV}?recursive=true")
    for e in tree:
        if e.get("path") == FILE:
            return e.get("lfs", {}).get("oid"), "xetHash" in e, e.get("size")
    return None, False, None


def snapshot_peers(pid):
    try:
        out = subprocess.check_output(
            ["ss", "-tnp"], text=True, stderr=subprocess.DEVNULL
        )
    except subprocess.CalledProcessError:
        return {}
    peers = {}
    for line in out.splitlines():
        if "ESTAB" not in line or f"pid={pid}" not in line:
            continue
        m = re.search(r"([\d.]+):(\d+)\s+([\d.]+):(\d+)", line)
        if not m:
            continue
        peer = m.group(3)
        if peer.startswith("127."):
            continue
        if peer not in peers:
            try:
                peers[peer] = socket.gethostbyaddr(peer)[0]
            except socket.herror:
                peers[peer] = "?"
    return peers


def run_once(label, disable_xet):
    outdir = f"/tmp/xetprobe-{label}"
    shutil.rmtree(outdir, ignore_errors=True)
    os.makedirs(outdir)
    cache = f"/tmp/xetprobe-cache-{label}"
    shutil.rmtree(cache, ignore_errors=True)
    env = dict(os.environ)
    env["HF_HOME"] = cache
    if disable_xet:
        env["HF_HUB_DISABLE_XET"] = "1"
    for k in ("HF_HUB_DISABLE_XET",):
        os.environ[k] = env.get(k, "")
    import huggingface_hub.constants as constants

    constants.HF_HUB_DISABLE_XET = bool(disable_xet)

    calls["xet_get"] = 0
    calls["http_get"] = 0
    print(f"\n=== {label} ===", flush=True)
    print(f"  HF_HUB_DISABLE_XET={constants.HF_HUB_DISABLE_XET}", flush=True)

    pid = os.getpid()
    peers = {}
    done = threading.Event()

    def poll():
        while not done.is_set():
            peers.update(snapshot_peers(pid))
            time.sleep(0.02)

    t = threading.Thread(target=poll, daemon=True)
    t.start()
    start = time.time()
    path = hf_hub_download(REPO, FILE, revision=REV, local_dir=outdir)
    done.set()
    elapsed = time.time() - start
    size = os.path.getsize(path)
    mibs = size / (1 << 20) / elapsed
    print(f"  size={size} time={elapsed:.2f}s {mibs:.0f} MiB/s", flush=True)
    print(
        f"  calls: xet_get={calls['xet_get']} http_get={calls['http_get']}", flush=True
    )
    print(f"  peers ({len(peers)}):", flush=True)
    for ip, host in sorted(peers.items()):
        tag = ""
        if "xethub" in host and "bridge" not in host:
            tag = "  <-- XET NATIVE CAS"
        elif "xethub" in host:
            tag = "  <-- XET BRIDGE"
        elif "cloudfront" in host:
            tag = "  <-- CDN/CloudFront"
        print(f"    {ip} {host}{tag}", flush=True)
    shutil.rmtree(outdir, ignore_errors=True)
    shutil.rmtree(cache, ignore_errors=True)
    return elapsed, mibs


def main():
    oid, has_xet, size = xet_hash_present()
    print(f"repo={REPO} file={FILE} rev={REV}")
    print(f"  lfs.oid={oid}")
    print(f"  size={size}")
    print(f"  xetHash present: {has_xet}")
    print(f"  hf_xet importable: {is_xet_available()}")
    print(f"  huggingface_hub: {__import__('huggingface_hub').__version__}")
    run_once("xet-enabled", disable_xet=False)
    run_once("xet-disabled", disable_xet=True)


if __name__ == "__main__":
    main()
