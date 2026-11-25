"""LarkAgentX's Python packages, installed once into the data folder from
the hash-locked requirements.txt, one folder per lock: a changed lock gets a
fresh install and the old one is removed.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

PIP_WAIT = 900


class DepsError(RuntimeError):
    """pip could not install them."""


def ensure(requirements, data_dir):
    """The folder to put on sys.path. Raises DepsError with pip's last lines."""
    with open(requirements, "rb") as f:
        lock = hashlib.sha256(f.read()).hexdigest()[:16]
    want = os.path.join(data_dir, "deps-" + lock)
    if os.path.isfile(os.path.join(want, ".ok")):
        return want
    os.makedirs(data_dir, exist_ok=True)
    tmp = tempfile.mkdtemp(prefix=".deps-", dir=data_dir)
    try:
        cmd = [sys.executable, "-m", "pip", "install", "--disable-pip-version-check", "--no-input", "--quiet",
               "--require-hashes", "--no-compile", "--target", tmp, "-r", requirements]
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=PIP_WAIT)
        if p.returncode != 0:
            tail = p.stdout.decode("utf-8", "replace").strip().splitlines()[-6:]
            raise DepsError("安装依赖失败：" + " / ".join(tail))
        open(os.path.join(tmp, ".ok"), "w").close()
        shutil.rmtree(want, ignore_errors=True)
        os.rename(tmp, want)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    for name in os.listdir(data_dir):
        if name.startswith("deps-") and os.path.join(data_dir, name) != want:
            shutil.rmtree(os.path.join(data_dir, name), ignore_errors=True)
    return want
