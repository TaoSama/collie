"""Feishu through LarkAgentX at its pinned commit: its sign-in, websocket and
proto are used as they are; only how credentials are kept (with the host,
not in a file) and how a post is put together are this adapter's own.
"""
import asyncio
import base64
import collections
import importlib
import logging
import os
import re
import sys
import threading
import time
import types
from pathlib import Path

import richtext

log = logging.getLogger("larkx")

QR_WAITING, QR_SCANNED, QR_DONE, QR_EXPIRED, QR_FAILED = "waiting", "scanned", "done", "expired", "failed"
QR_STATUS = {1: QR_WAITING, 2: QR_SCANNED, 3: QR_FAILED, 4: QR_FAILED, 5: QR_EXPIRED}

MSG_FILE, MSG_TEXT, MSG_IMAGE, MSG_SYSTEM = 3, 4, 5, 6
CHAT_P2P = 1
CMD_PUT_MESSAGE = 5
MEDIA_MOST = 20 << 20
UP_AFTER = 5  # seconds a websocket stays open before it counts as up
NAME_TTL = 3600
RECENT_MOST = 2000
CREDENTIAL_FIELDS = ("cookies", "saved_at", "device_id", "app_key", "user_id", "csrf_token", "web_app_id")


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
        builders=mod("larkx.proto.builders"), ids=mod("larkx.proto.ids"),
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
        async def handle(msg):  # on upstream's own loop
            await asyncio.get_running_loop().run_in_executor(None, self._deliver, msg, on_message)

        wait = 2
        while not stop.is_set() and self.client is not None:
            loop = asyncio.new_event_loop()
            started = time.time()
            try:
                task = loop.create_task(self.client.connect_websocket(handle))
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

    def _deliver(self, msg, on_message):
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
        else:
            text = re.sub(r",?\s*(key|imageKey|stickerId)=\S+", "", msg.get("content") or "").strip()
        out["text"] = text
        with self._mu:
            quoted = self.recent.get(str(msg.get("parent_id") or ""))
        if quoted:
            out["quote"] = quoted[1]
        if msg.get("anchor"):
            out["thread"] = str(msg["anchor"])
        with self._mu:
            self.recent[mid] = (root, text)
            while len(self.recent) > RECENT_MOST:
                self.recent.popitem(last=False)
        p2p = msg.get("chat_type") == CHAT_P2P
        out["chat_kind"] = "self" if p2p and chat == self.self_chat else "dm" if p2p else "group"
        out["sender_name"] = self.user_name(sender, chat)
        if p2p and not out["owner"]:
            out["chat"] = out["sender_name"]
        else:
            out["chat"] = self.names.get("chat:" + chat, lambda: (self.client.get_chat_info(chat) or {}).get("name"))
        at = msg.get("create_time")
        if at:
            at = int(at) / (1000 if int(at) > 10 ** 12 else 1)
            out["at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at))
        return out

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

