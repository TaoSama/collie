"""Back up or remove one Antigravity session using the cleanup skill."""

import argparse
import json
import os
import re
import shutil
import sqlite3
import tempfile
from pathlib import Path


def read_varint(buf, i):
    val, shift = 0, 0
    while True:
        b = buf[i]; i += 1
        val |= (b & 0x7f) << shift
        if not (b & 0x80): return val, i
        shift += 7


def strip_pb(path, tid):
    tb = tid.encode()
    if not os.path.isfile(path): return
    buf = open(path, "rb").read()
    out, i, dropped = bytearray(), 0, 0
    while i < len(buf):
        start = i
        tag, i = read_varint(buf, i)
        fn, wt = tag >> 3, tag & 7
        if wt == 2:
            ln, i = read_varint(buf, i)
            val = buf[i:i+ln]; i += ln
            drop = False
            if fn in (1, 2) and len(val) > 2 and val[0] == 0x0a:
                klen, ki = read_varint(val, 1)
                if val[ki:ki+klen] == tb:
                    drop = True
            if drop:
                dropped += 1
            else:
                out.extend(buf[start:i])
        elif wt == 0: _, i = read_varint(buf, i); out.extend(buf[start:i])
        elif wt == 1: i += 8; out.extend(buf[start:i])
        elif wt == 5: i += 4; out.extend(buf[start:i])
        else: out.extend(buf[start:]); break
    if dropped:
        mode = os.stat(path).st_mode & 0o777
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".jetbox.")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(out)
            os.chmod(tmp, mode)
            os.replace(tmp, path)
        except Exception:
            os.unlink(tmp); raise
        print(f"  stripped {dropped} entry(s) from {path}")


def atomic_json(path, fn):
    if not os.path.isfile(path): return
    d = json.load(open(path))
    if not fn(d): return
    mode = os.stat(path).st_mode & 0o777
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".cache.")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(d, f, indent=2, ensure_ascii=False)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except Exception:
        os.unlink(tmp); raise
    print(f"  updated {path}")


def inventory(root, tid):
    removable = [
        *root.glob(f"conversations/{tid}.*"),
        root / f"annotations/{tid}.pbtxt",
        root / f"presence/{tid}.lock",
    ]
    shared = [
        *root.glob("conversation_summaries.db*"),
        root / "jetbox_summaries_proto.pb",
        root / "cache/conversation_metadata.json",
        root / "cache/last_conversations.json",
    ]
    files = [path for path in removable if path.exists()]
    brain = root / "brain" / tid
    if brain.is_dir():
        files.append(brain)
    return files, [path for path in shared if path.exists()]


def backup(targets):
    dest = Path(tempfile.mkdtemp(prefix="agy-cleanup."))
    try:
        for root, removable, shared in targets:
            for path in removable + shared:
                copy = dest / root.name / path.relative_to(root)
                copy.parent.mkdir(parents=True, exist_ok=True)
                if path.is_dir():
                    shutil.copytree(path, copy, symlinks=True)
                else:
                    shutil.copy2(path, copy)
    except OSError:
        shutil.rmtree(dest)
        raise
    return dest


def clean(root, tid, removable, shared):
    print(f"== cleaning {root}")
    for path in removable:
        if path == root / "brain" / tid:
            shutil.rmtree(path)
        else:
            path.unlink()
        print("  removed", path)
    db = root / "conversation_summaries.db"
    if db in shared:
        c = sqlite3.connect(db, timeout=5)
        c.execute("DELETE FROM conversation_summaries WHERE conversation_id = ?", (tid,))
        for tbl, q in (("conversations_fts", "DELETE FROM conversations_fts WHERE conversation_id = ?"),
                       ("conversation_parents", "DELETE FROM conversation_parents WHERE conversation_id = ? OR parent_id = ?")):
            if c.execute("SELECT 1 FROM sqlite_master WHERE name = ?", (tbl,)).fetchone():
                args = (tid,) if tbl == "conversations_fts" else (tid, tid)
                c.execute(q, args)
        c.commit()
        c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        c.close()
        print(f"  cleaned {db}")
    summary = root / "jetbox_summaries_proto.pb"
    if summary in shared:
        strip_pb(summary, tid)
    metadata = root / "cache/conversation_metadata.json"
    if metadata in shared:
        atomic_json(metadata, lambda d: bool(
            (d.pop(tid, None) is not None) |
            (isinstance(d.get("conversations"), dict) and d["conversations"].pop(tid, None) is not None)
        ))
    last = root / "cache/last_conversations.json"
    if last in shared:
        atomic_json(last, lambda d: bool(
            [d.pop(k) for k in [k for k, v in d.items() if v == tid]]
        ))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("backup", "clean"))
    parser.add_argument("--id", required=True, help="Antigravity conversation ID")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f-]{16,}", args.id):
        parser.error("--id 必须是有效的 Antigravity 会话 ID")
    roots = (Path.home() / ".gemini" / name for name in ("antigravity", "antigravity-cli"))
    targets = [(root, *inventory(root, args.id)) for root in roots if root.is_dir()]
    try:
        saved = backup(targets)
    except OSError as err:
        parser.exit(1, f"备份失败，已停止清理：{err}\n")
    print(saved, flush=True)
    if args.action == "clean":
        for root, removable, shared in targets:
            clean(root, args.id, removable, shared)


if __name__ == "__main__":
    main()
