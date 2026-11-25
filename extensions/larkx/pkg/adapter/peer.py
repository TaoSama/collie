"""One JSON frame a line over stdin and stdout (extensions/README.md §3).

Either side may call the other and wait, or send an event. Calls from the
host run on a small pool so a slow send never holds up a ping.
"""
import itertools
import json
import sys
import threading
from concurrent.futures import Future, ThreadPoolExecutor
from concurrent.futures import TimeoutError as FutureTimeout

CALL_WAIT = 60


class Error(Exception):
    """An answer that failed: code for programs, message for people."""

    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


class Peer:
    def __init__(self, handle, rd=None, wr=None, workers=8):
        self._handle = handle
        self._rd = rd or sys.stdin.buffer
        self._wr = wr or sys.stdout.buffer
        self._wlock = threading.Lock()
        self._ids = itertools.count(1)
        self._waiting = {}
        self._wmu = threading.Lock()
        self._pool = ThreadPoolExecutor(max_workers=workers)
        self.closed = threading.Event()

    def _write(self, frame):
        line = json.dumps(frame, ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n"
        with self._wlock:
            self._wr.write(line)
            self._wr.flush()

    def notify(self, event, params):
        self._write({"event": event, "params": params})

    def call(self, method, params, wait=CALL_WAIT):
        fid = "p%d" % next(self._ids)
        fut = Future()
        with self._wmu:
            self._waiting[fid] = fut
        try:
            self._write({"id": fid, "method": method, "params": params})
            return fut.result(timeout=wait)
        except FutureTimeout:
            raise Error("timeout", "the host did not answer %s" % method)
        finally:
            with self._wmu:
                self._waiting.pop(fid, None)

    def serve(self):
        """Reads frames until stdin closes."""
        try:
            for line in self._rd:
                line = line.strip()
                if not line:
                    continue
                try:
                    frame = json.loads(line)
                except ValueError:
                    sys.stderr.write("larkx: a frame that is not JSON\n")
                    continue
                if frame.get("method"):
                    self._pool.submit(self._answer, frame)
                elif frame.get("id"):
                    with self._wmu:
                        fut = self._waiting.get(frame["id"])
                    if fut is None:
                        continue
                    err = frame.get("error")
                    if err:
                        fut.set_exception(Error(err.get("code", ""), err.get("message", "")))
                    else:
                        fut.set_result(frame.get("result"))
        finally:
            self.closed.set()
            with self._wmu:
                for fut in self._waiting.values():
                    if not fut.done():
                        fut.set_exception(Error("closed", "the host went away"))

    def _answer(self, frame):
        fid = frame.get("id")
        try:
            result = self._handle(frame["method"], frame.get("params") or {})
            out = {"id": fid, "result": result if result is not None else {}}
        except Error as e:
            out = {"id": fid, "error": {"code": e.code, "message": e.message}}
        except Exception as e:  # anything else is a bug here, said as such
            out = {"id": fid, "error": {"code": "internal", "message": "%s: %s" % (type(e).__name__, e)}}
        if fid:
            self._write(out)
