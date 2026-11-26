"""Feishu's incoming WebSocket connection and packet dispatch."""
import asyncio
import gzip
import logging
import time
import zlib
from concurrent.futures import ThreadPoolExecutor

import message_view

log = logging.getLogger("larkx")

CMD_PUSH_MESSAGES = 6
CMD_PUSH_SID = 6000  # only sids: the packets are pulled by them
CMD_PUSH_DOC_FEED = 14004  # comments, replies and mentions in cloud documents
BAD_FRAME_EVERY = 600
UP_AFTER = 5  # seconds a websocket stays open before it counts as up


class PushStream:
    def __init__(self, U, mu, get_client, deliver, doc_comments, signed_out=lambda: True):
        self.U = U
        self._mu = mu
        self._get_client = get_client
        self._deliver = deliver
        self._doc_comments = doc_comments
        self._signed_out = signed_out  # Feishu confirms the session is gone (not just unreachable)
        self._loop = None
        self._task = None
        self._bad_frames, self._bad_warned = 0, 0.0
        self._pool = ThreadPoolExecutor(max_workers=4)  # handles pushes; outlives each connection's loop

    def listen(self, on_message, on_link, stop):
        """Keeps the websocket open until stop is set or Feishu signs the session out."""
        wait = 2
        while not stop.is_set() and self._get_client() is not None:
            loop = asyncio.new_event_loop()
            started = time.time()
            try:
                task = loop.create_task(self._session(on_message))
                with self._mu:
                    self._loop, self._task = loop, task
                loop.call_later(UP_AFTER, lambda: task.done() or on_link("up"))
                loop.run_until_complete(task)
                wait = 2
            except asyncio.CancelledError:
                return
            except self.U.auth.AuthExpired as e:
                if self._signed_out():
                    on_link("expired", str(e))
                    return
                log.warning("websocket: Feishu could not confirm the session, trying again: %s", str(e).splitlines()[0])
            except Exception as e:
                log.warning("websocket: %s", e)
                if time.time() - started < UP_AFTER:
                    wait = min(wait * 2, 60)
            finally:
                with self._mu:
                    if self._task is task:
                        self._loop = self._task = None
                loop.close()
            on_link("down")
            stop.wait(wait)

    def stop(self):
        with self._mu:
            loop, task = self._loop, self._task
        if loop and task:
            loop.call_soon_threadsafe(task.cancel)

    async def _session(self, on_message):
        """One websocket connection: upstream's address, heartbeat and acks; the pushes are read here, so
        compressed frames, sid-only pushes and document comments are not lost."""
        import websockets

        c = self._get_client()
        async with websockets.connect(c.build_ws_url()) as ws:
            beat = asyncio.create_task(c.heartbeat_loop(ws))
            try:
                async for raw in ws:
                    try:
                        packet = self._packet(raw)
                    except Exception as e:
                        self._bad_frame(e)
                        continue
                    if packet is None:
                        continue
                    if packet.sid:
                        await c.send_ack(ws, packet.sid)
                    self._pool.submit(self._push, packet.cmd, packet.payload, packet, on_message, time.monotonic())
            finally:
                beat.cancel()

    def _packet(self, raw):
        """The frame's packet (lark improto.Packet), or None for a frame that carries none (JSON)."""
        frame = self.U.P.Frame.FromString(raw)
        enc = (frame.payloadEncoding or "").lower()
        body = frame.payload
        if enc == "json":
            return None
        if enc == "gzip" or (not enc and body[:2] == b"\x1f\x8b"):
            body = gzip.decompress(body)
        elif enc in ("deflate", "zlib"):
            body = zlib.decompress(body)
        elif enc and enc not in ("identity", "pb"):
            raise ValueError("unknown payloadEncoding %r" % enc)
        return self.U.L.improto.Packet.FromString(body)

    def _bad_frame(self, err):
        """Frames that can't be read: counted, said at most every ten minutes."""
        self._bad_frames += 1
        if time.time() - self._bad_warned >= BAD_FRAME_EVERY:
            log.warning("%d websocket frame(s) could not be read, last: %r", self._bad_frames, err)
            self._bad_frames, self._bad_warned = 0, time.time()

    def _push(self, cmd, payload, packet, on_message, got=None):
        """One pushed packet's messages to on_message, on a worker thread."""
        try:
            if cmd == CMD_PUSH_MESSAGES:
                for msg in self._messages(payload):
                    self._deliver(msg, on_message, got)
            elif cmd == CMD_PUSH_SID:
                sids = list(dict.fromkeys(x for x in [packet.sid] + [str(n) for n in packet.compactedSids] if x))
                pulled = (self._get_client().api("improto.PullPacketsBySidsRequest", {"sids": sids}) or {}).get("packets") or {}
                for p in pulled.values():
                    if int(p.get("cmd") or 0) in (CMD_PUSH_MESSAGES, CMD_PUSH_DOC_FEED):
                        self._push(int(p["cmd"]), p.get("payload") or b"", None, on_message, got)
            elif cmd == CMD_PUSH_DOC_FEED:
                for m in self._doc_comments(payload):
                    on_message(m)
        except Exception as e:
            log.warning("push %s: %s", cmd, e)

    def _messages(self, payload):
        """The raw messages of a push share the same decoder as inbox history."""
        U = self.U
        request = U.P.PushMessagesRequest()
        request.ParseFromString(payload)
        raw = U.decoders.protobuf_to_dict(request).get("messages") or {}
        return [message_view.decode(U, entry) for entry in raw.values()]
