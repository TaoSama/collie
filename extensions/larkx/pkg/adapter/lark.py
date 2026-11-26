"""Feishu through LarkAgentX at its pinned commit: its sign-in, websocket and
proto are used as they are; only how credentials are kept (with the host,
not in a file) and how a post is put together are this adapter's own.
"""
import asyncio
import base64
import collections
import gzip
import importlib
import logging
import os
import re
import sys
import threading
import time
import types
from concurrent.futures import ThreadPoolExecutor
import zlib
from pathlib import Path

import richtext

log = logging.getLogger("larkx")

QR_WAITING, QR_SCANNED, QR_DONE, QR_EXPIRED, QR_FAILED = "waiting", "scanned", "done", "expired", "failed"
QR_STATUS = {1: QR_WAITING, 2: QR_SCANNED, 3: QR_FAILED, 4: QR_FAILED, 5: QR_EXPIRED}

MSG_FILE, MSG_TEXT, MSG_IMAGE, MSG_SYSTEM, MSG_CARD = 3, 4, 5, 6, 14
CHAT_P2P = 1
CMD_PUT_MESSAGE = 5
WORKING_REACTION = "OnIt"  # the reaction a request gets when Collie takes it: 「在做了」
CMD_PUSH_MESSAGES = 6
CMD_PUSH_SID = 6000  # only sids: the packets are pulled by them
CMD_PUSH_DOC_FEED = 14004  # comments, replies and mentions in cloud documents
DOC_FEED_NEWS = (1, 2, 5)  # PushDocFeedRequest.Status NEW, UPDATE, CREATE
DOC_KINDS = {1: "在文档里@了你", 2: "评论了文档", 3: "回复了评论"}  # DocMessage.Type MENTION, COMMENT, REPLY
DOC_CHAT = "doc:"  # a document's comments are a chat of their own
BAD_FRAME_EVERY = 600
MEDIA_MOST = 20 << 20
UP_AFTER = 5  # seconds a websocket stays open before it counts as up
NAME_TTL = 3600
RECENT_MOST = 2000
CREDENTIAL_FIELDS = ("cookies", "saved_at", "device_id", "app_key", "user_id", "csrf_token", "web_app_id")


def is_request(text, word):
    """text starts with the trigger word, as Collie reads it: then a space, a colon or nothing."""
    s = (text or "").lstrip()
    if not word or s[:len(word)].lower() != word.lower():
        return False
    rest = s[len(word):]
    return rest == "" or rest[0].isspace() or rest[0] in ":："


class Expired(Exception):
    """Feishu signed this session out: scan again."""


class NotSent(Exception):
    """The send surely did not go out."""


class Unsure(Exception):
    """The send may have gone out: never sent again."""


def load_upstream(root, data_dir, region):
    """LarkAgentX's modules, without its package __init__ (which pulls in its agent and database)."""
    os.environ["LARKX_HOME"] = data_dir
    os.environ["LARKX_REGION"] = region
    pkg = types.ModuleType("larkx")
    pkg.__path__ = [os.path.join(root, "larkx")]
    sys.modules["larkx"] = pkg
    mod = importlib.import_module
    return types.SimpleNamespace(
        auth=mod("larkx.auth"), client=mod("larkx.client"), region=mod("larkx.region"), media=mod("larkx.media"),
        builders=mod("larkx.proto.builders"), decoders=mod("larkx.proto.decoders"), ids=mod("larkx.proto.ids"),
        P=mod("larkx.proto.proto_pb2"), L=mod("larkx.proto.lark_all_pb2"))


def make_auth(U, region, saved, on_save):
    """A LarkAuth whose credentials come from and go to the host."""

    class Auth(U.auth.LarkAuth):
        def load(self):
            d = saved or {}
            if not d.get("cookies"):
                return False
            self.region = U.region.get_region(d.get("region") or region)
            for k in CREDENTIAL_FIELDS:
                setattr(self, k, d.get(k, {} if k == "cookies" else 0 if k == "saved_at" else ""))
            return True

        def save(self):
            on_save(dump(self))

    return Auth(path=Path(os.devnull))


def dump(auth):
    """What is kept of a sign-in."""
    d = {k: getattr(auth, k) for k in CREDENTIAL_FIELDS}
    d["region"] = auth.region.name
    return d


class _Names:
    """Looked-up names, kept an hour."""

    def __init__(self):
        self._m = {}
        self._mu = threading.Lock()

    def get(self, key, look):
        with self._mu:
            hit = self._m.get(key)
        if hit and time.time() - hit[1] < NAME_TTL:
            return hit[0]
        try:
            name = look() or ""
        except Exception as e:
            log.warning("name of %s: %s", key, e)
            name = ""
        with self._mu:
            self._m[key] = (name, time.time())
            if len(self._m) > RECENT_MOST:
                old = time.time() - NAME_TTL
                self._m = {k: v for k, v in self._m.items() if v[1] > old}
        return name


class Feishu:
    def __init__(self, U, region, on_save):
        self.U = U
        self.region = region
        self.on_save = on_save
        self.client = None
        self.me = ""
        self.self_chat = ""
        self.qr = None
        self.qr_status = QR_WAITING
        self.names = _Names()
        self.recent = collections.OrderedDict()  # message id -> (root id, text)
        self.own_cids = collections.OrderedDict()  # what this adapter sent: its echo is not news
        self._mu = threading.Lock()
        self._loop = None
        self._task = None
        self._bad_frames, self._bad_warned = 0, 0.0
        self._pool = ThreadPoolExecutor(max_workers=4)  # handles pushes; outlives each connection's loop
        self._react_pool = ThreadPoolExecutor(max_workers=2)  # reactions never wait behind a push
        self.trigger = ""  # Collie's trigger word (channel.settings): your own request is marked at once
        self.marked = collections.OrderedDict()  # message ids already marked as being worked on

    # Signing in.

    def restore(self, saved):
        """Signs in with kept credentials; the account's name. Raises Expired, or OSError for the network."""
        auth = make_auth(self.U, self.region, saved, self.on_save)
        try:
            self.client = self.U.client.LarkClient(auth)  # checks the session first
        except self.U.auth.AuthExpired as e:
            # Upstream says a network failure and a signed-out session apart only in its message.
            raise (OSError if "网络" in str(e) else Expired)(str(e))
        self.me = str(auth.user_id)
        try:
            self.self_chat = str(self.client.create_chat(self.me) or "")
        except Exception as e:
            log.warning("own chat: %s", e)
        return self.user_name(self.me, self.self_chat) or self.me

    def login_begin(self):
        self.qr = self.U.auth.QrLogin(self.U.region.get_region(self.region))
        self.qr_status = QR_WAITING
        return self.qr.qr_content

    def login_poll(self):
        """One poll of the QR code: QR_WAITING, QR_SCANNED, QR_DONE, QR_EXPIRED or QR_FAILED."""
        def seen(status, step):
            self.qr_status = QR_STATUS.get(status, self.qr_status)

        ok, _ = self.qr.wait(timeout=1, interval=1, on_status=seen)
        return QR_DONE if ok else self.qr_status

    def login_finish(self):
        """The account's name; its credentials have gone to on_save."""
        auth = make_auth(self.U, self.region, None, self.on_save)
        self.qr.finish(auth)  # validates and saves
        self.qr = None
        return self.restore(dump(auth))

    def logout(self):
        self.stop()
        self.client, self.me, self.self_chat = None, "", ""

    # Receiving.

    def listen(self, on_message, on_link, stop):
        """Keeps the websocket open until stop is set or Feishu signs the session out."""
        wait = 2
        while not stop.is_set() and self.client is not None:
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
                on_link("expired", str(e))
                return
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

        c = self.client
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
                pulled = (self.client.api("improto.PullPacketsBySidsRequest", {"sids": sids}) or {}).get("packets") or {}
                for p in pulled.values():
                    if int(p.get("cmd") or 0) in (CMD_PUSH_MESSAGES, CMD_PUSH_DOC_FEED):
                        self._push(int(p["cmd"]), p.get("payload") or b"", None, on_message, got)
            elif cmd == CMD_PUSH_DOC_FEED:
                for m in self.doc_comments(payload):
                    on_message(m)
        except Exception as e:
            log.warning("push %s: %s", cmd, e)

    def _messages(self, payload):
        """A PushMessagesRequest's messages, decoded by upstream; a topic group's root opens its topic."""
        U = self.U
        frame = U.P.Frame(payload=U.P.Packet(cmd=CMD_PUSH_MESSAGES, payload=payload).SerializeToString())
        out = []
        for msg in U.decoders.decode_push_messages(frame.SerializeToString()):
            if not msg.get("from_id"):
                continue
            if msg["scope"] == "chat" and msg.get("chat_type") == 2 and self.client.is_thread_chat(msg["chat_id"]):
                msg["scope"], msg["anchor"] = U.decoders.resolve_scope(msg["msg_id"], msg["msg_type"], 2, "", "", thread_chat=True)
            out.append(msg)
        return out

    def doc_comments(self, payload):
        """A document feed push's new comments, replies and mentions as channel.receive messages."""
        from protobuf_to_dict import protobuf_to_dict

        L = self.U.L
        req = L.docs.PushDocFeedRequest.FromString(payload)
        if req.status not in DOC_FEED_NEWS:
            return []
        feed, out = req.docFeed, []
        for dm in sorted(req.docMessages.values(), key=lambda d: d.createTime):
            what = DOC_KINDS.get(dm.type)
            mid = DOC_CHAT + dm.id
            # A feed update repeats what came before, and a sid pull may bring a push again.
            if not what or not dm.id or not self._remember(mid, "", ""):
                continue
            body = (richtext.to_text(L, protobuf_to_dict(dm.richText)) if dm.HasField("richText") else "") or dm.content
            sender = str(dm.fromId)
            # The text opens with who did what, so a comment never starts with the trigger word:
            # nothing here asks for an answer that could not be sent.
            m = {"id": mid, "chat_id": DOC_CHAT + (feed.id or dm.docFeedId), "chat": feed.title or "云文档",
                 "chat_kind": "doc", "sender": sender, "owner": bool(self.me) and sender == self.me,
                 "sender_name": self.user_name(sender, ""), "thread": feed.id or dm.docFeedId,
                 "text": "（%s%s）\n%s" % (what, "：" + feed.url if feed.url else "", body.strip())}
            if dm.createTime:
                at = dm.createTime / (1000 if dm.createTime > 10 ** 12 else 1)
                m["at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at))
            out.append(m)
        return out

    def _deliver(self, msg, on_message, got=None):
        try:
            self._quick(msg, got)
        except Exception as e:
            log.warning("marking %s: %s", msg.get("msg_id"), e)
        try:
            m = self.normalize(msg)
        except Exception as e:
            log.warning("message %s: %s", msg.get("msg_id"), e)
            return
        if m is not None:
            on_message(m)

    def normalize(self, msg):
        """An upstream push message as the host's channel.receive message, or None."""
        mid, chat, sender = str(msg.get("msg_id") or ""), str(msg.get("chat_id") or ""), str(msg.get("from_id") or "")
        mtype = msg.get("msg_type")
        if not mid or not chat or mtype == MSG_SYSTEM:
            return None
        with self._mu:
            if msg.get("cid") and msg["cid"] in self.own_cids:
                return None
        root = str(msg.get("root_id") or "")
        root = "" if root == "0" else root
        out = {"id": mid, "chat_id": chat, "sender": sender, "owner": bool(self.me) and sender == self.me}
        data = msg.get("content_data") or {}
        if mtype in (richtext.MSG_POST, MSG_TEXT):
            text = richtext.to_text(self.U.L, data.get("richText") or {}) or data.get("text", "")
            if data.get("title"):
                text = "**%s**\n%s" % (data["title"], text)
        elif mtype in (MSG_IMAGE, MSG_FILE):
            text = ""
            blob = self._download(msg)
            if blob:
                out["images" if mtype == MSG_IMAGE else "files"] = [blob]
            else:
                out["unsupported"] = ["图片（没能下载）" if mtype == MSG_IMAGE else "文件 %s（没能下载）" % data.get("name", "")]
        elif mtype == MSG_CARD:
            head = data.get("cardHeader") or {}
            text = " ".join(x for x in ("[卡片]", head.get("title") or head.get("mainTitle") or "", data.get("cardDesc") or "") if x)
        else:
            text = re.sub(r",?\s*(key|imageKey|stickerId)=\S+", "", msg.get("content") or "").strip()
        out["text"] = text
        with self._mu:
            quoted = self.recent.get(str(msg.get("parent_id") or ""))
        if quoted:
            out["quote"] = quoted[1]
        if msg.get("anchor"):
            out["thread"] = str(msg["anchor"])
        self._remember(mid, root, text, again=True)
        p2p = msg.get("chat_type") == CHAT_P2P
        out["chat_kind"] = "self" if p2p and chat == self.self_chat else "dm" if p2p else "group"
        out["sender_name"] = self.user_name(sender, chat)
        info = self.chat_info(chat)
        if p2p and not out["owner"]:
            out["chat"] = out["sender_name"]
        else:
            out["chat"] = info.get("name") or ""
        if "isRemind" in info:
            out["muted"] = not info["isRemind"]  # 免打扰 is isRemind off
        folded = self.folded(chat)
        if folded is not None:
            out["folded"] = folded
        at = msg.get("create_time")
        if at:
            at = int(at) / (1000 if int(at) > 10 ** 12 else 1)
            out["at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at))
        return out

    def _remember(self, mid, root, text, again=False):
        """Keeps a message's topic root and text; False when it was kept already (unless again)."""
        with self._mu:
            if mid in self.recent and not again:
                return False
            self.recent[mid] = (root, text)
            while len(self.recent) > RECENT_MOST:
                self.recent.popitem(last=False)
            return True

    def chat_info(self, chat):
        """The chat as the signed-in person sees it (name, isRemind…), kept an hour like names."""
        if self.client is None:
            return {}
        return self.names.get("chatinfo:" + chat, lambda: self.client.get_chat_info(chat)) or {}

    def folded(self, chat):
        """The chat is in 折叠的会话: its feed card sits in a box (parentCardId). None when not known."""
        if self.client is None:
            return None

        def look():
            cards = (self.client.api("feed.PullFeedCardsByIdsRequest", {"cardIds": [chat]}) or {}).get("cards") or {}
            card = cards.get(chat)
            return "" if card is None else ("folded" if int(card.get("parentCardId") or 0) else "open")

        got = self.names.get("feedcard:" + chat, look)
        return None if not got else got == "folded"

    def user_name(self, uid, chat):
        if not uid or self.client is None:
            return ""
        return self.names.get("user:" + uid, lambda: self.client.get_user_name(uid, chat or self.self_chat or "0"))

    def _download(self, msg):
        """The message's picture or file, up to MEDIA_MOST, as a blob; None when it can't be had."""
        M, data, auth = self.U.media, msg.get("content_data") or {}, self.client.auth
        try:
            url = M.message_resource_url(msg["msg_id"], M.extract_resource_key(msg.get("msg_type", 0), data),
                                         msg["chat_id"], host=auth.region.file_host)
            buf, mime = M.download(auth, url, max_bytes=MEDIA_MOST)
        except Exception as e:
            log.warning("download %s: %s", msg.get("msg_id"), e)
            return None
        name = data.get("name") or ("%s.png" % msg.get("msg_id") if msg.get("msg_type") == MSG_IMAGE else "")
        return {"name": name, "mime": (mime or "application/octet-stream").split(";")[0],
                "data": base64.b64encode(buf).decode()}

    # Sending.

    def post(self, chat, markdown, footer="", reply_to=""):
        """Sends a post; its message id. Raises NotSent or Unsure."""
        if self.client is None:
            raise NotSent("没有登录")
        if str(chat).startswith(DOC_CHAT):
            raise NotSent("文档评论暂时只收不回")
        U = self.U
        req = U.L.messages.PutMessageRequest()
        req.type = richtext.MSG_POST
        req.chatId = str(chat)
        req.cid = U.ids.generate_request_cid()
        req.isNotified = True
        req.version = 1
        req.content.richText.CopyFrom(richtext.build(U.L, markdown, footer))
        if reply_to:
            with self._mu:
                root = (self.recent.get(reply_to) or ("",))[0] or reply_to
            req.rootId = req.parentId = root
            # A topic group refuses isReplyInThread; elsewhere a reply without it lands in the main chat.
            if not self.client.is_thread_chat(chat):
                req.isReplyInThread = True
        with self._mu:
            self.own_cids[req.cid] = True
            while len(self.own_cids) > RECENT_MOST:
                self.own_cids.popitem(last=False)
        return self._put(U.builders.wrap_packet(CMD_PUT_MESSAGE, req, U.ids.generate_long_request_id()))

    def _quick(self, msg, got):
        """Your own request is marked before Collie hears of it: nothing to ask, the trigger word is enough."""
        if not self.trigger or not self.me or str(msg.get("from_id") or "") != self.me:
            return
        if msg.get("msg_type") not in (richtext.MSG_POST, MSG_TEXT):
            return
        data = msg.get("content_data") or {}
        text = richtext.to_text(self.U.L, data.get("richText") or {}) or data.get("text", "")
        if is_request(text, self.trigger):
            self.mark_working(str(msg["msg_id"]), got)

    def mark_working(self, message_id, got=None):
        """The 在做了 reaction, once a message, off the caller's thread; times logged."""
        with self._mu:
            if message_id in self.marked:
                return
            self.marked[message_id] = True
            while len(self.marked) > RECENT_MOST:
                self.marked.popitem(last=False)
        queued = time.monotonic()

        def put():
            start = time.monotonic()
            try:
                self.react(message_id)
            except Exception as e:
                log.warning("reaction on %s: %s", message_id, e)
                return
            since = "" if got is None else " %.1fms after the push came," % ((start - got) * 1000)
            log.info("working %s:%s request sent %.1fms after it was queued, Feishu answered in %.0fms",
                     message_id, since, (start - queued) * 1000, (time.monotonic() - start) * 1000)

        self._react_pool.submit(put)

    def react(self, message_id, key=WORKING_REACTION):
        """Puts a reaction on a message, as the signed-in person."""
        if self.client is None:
            raise NotSent("没有登录")
        if str(message_id).startswith(DOC_CHAT):
            return  # a document comment is not a message
        self.client.api("reactions.PutReactionRequest", {"messageId": str(message_id), "reactionType": key})

    def _put(self, packet):
        import requests

        try:
            # Upstream's own sender only knows plain text; its gateway call is what carries a post.
            content = self.client._gateway_post(packet)
        except requests.exceptions.ConnectTimeout as e:
            raise NotSent("连不上飞书：%s" % e)
        except requests.exceptions.HTTPError as e:
            code = e.response.status_code if e.response is not None else 0
            if 400 <= code < 500:
                raise NotSent("飞书拒收（HTTP %d）" % code)
            raise Unsure("飞书答 HTTP %d，可能已经发出" % code)
        except requests.exceptions.ConnectionError as e:
            if "NewConnectionError" in str(e) or "NameResolution" in str(e):
                raise NotSent("连不上飞书：%s" % e)
            raise Unsure("连接中断，可能已经发出：%s" % e)
        except Exception as e:
            raise Unsure("没收到飞书的答复，可能已经发出：%s" % e)
        pkt = self.U.P.Packet()
        try:
            pkt.ParseFromString(content)
        except Exception:
            raise Unsure("飞书的答复读不懂，可能已经发出")
        if pkt.status and not pkt.payload:
            raise NotSent("飞书拒收（status %d）" % pkt.status)
        try:
            mid = self.U.builders.decode_put_message_response(content)
        except Exception:
            mid = ""
        if not mid:
            raise Unsure("飞书收下了但没给消息 id，可能已经发出")
        return mid

