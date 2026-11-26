"""larkx: your own Feishu account in Collie's 收件箱 (extensions/larkx/README.md).

The protocol side needs only the standard library, so the host hears back at
once; LarkAgentX and its packages load in the background on first start.
"""
import base64
import json
import logging
import os
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PKG = os.path.dirname(HERE)
sys.path.insert(0, HERE)

import deps  # noqa: E402
import peer  # noqa: E402

ID = "larkx"
PROTOCOL = 1
RESTORE_RETRY = 60
QR_RENEWALS = 3
LOAD_WAIT = 40  # a sign-in started while the packages install waits this long (the host's is 50)
SENT_KEEP = 7 * 86400
QR_HINTS = {"waiting": "用飞书 App 扫码登录", "scanned": "已扫码，请在手机上确认"}

log = logging.getLogger("larkx")


class Extension:
    def __init__(self):
        self.peer = peer.Peer(self.handle)
        self.mu = threading.Lock()
        self.write_mu = threading.Lock()
        self.version = ""
        self.data = ""
        self.region = "feishu"
        self.backend = None
        self.lark = None  # the lark module, once loaded
        self.loading = "正在准备运行环境（第一次要装依赖，约一分钟）"
        self.account = ""
        self.link = ""
        self.expired = ""
        self.renewals = 0
        self.qr = ""  # the QR code being scanned
        self.stop = threading.Event()
        self.loaded = threading.Event()
        self.sent = {}
        self.trigger = ""

    # Host -> extension.

    def handle(self, method, params):
        if method == "initialize":
            return self.initialize(params)
        if method == "ping":
            return {}
        if method == "connector.status":
            return self.status()
        if method == "connector.login.step":
            return self.login_step(params.get("step") or "")
        if method == "connector.logout":
            return self.logout()
        if method == "channel.send":
            return self.send(params)
        if method == "channel.interest":
            return {}
        if method == "channel.working":
            return self.working(params)
        if method == "channel.settings":
            self.trigger = params.get("trigger") or ""
            if self.backend is not None:
                self.backend.trigger = self.trigger
            return {}
        raise peer.Error("unknown_method", method)

    def initialize(self, p):
        if p.get("protocol") != PROTOCOL:
            raise peer.Error("wrong_protocol", "this extension speaks protocol %d" % PROTOCOL)
        ext = p.get("extension") or {}
        self.version = ext.get("version", "")
        self.data = ext.get("data_dir") or os.environ.get("COLLIE_EXTENSION_DATA", ".")
        self.region = (p.get("config") or {}).get("region") or "feishu"
        self.sent = self._read_sent()
        threading.Thread(target=self.start, daemon=True).start()
        return {"protocol": PROTOCOL, "id": ID, "version": self.version}

    def status(self):
        with self.mu:
            if self.backend is None:
                return {"state": "error", "error": self.loading}
            if not self.account:
                st = {"state": "not_signed_in", "guide": ["在下面扫码，用你本人的飞书账号登录"]}
                if self.expired:
                    st["error"] = "登录已失效，请重新扫码"
                return st
            phase = {"up": "已连上", "down": "正在重连", "": "正在连接"}.get(self.link, self.link)
            return {"state": "ok", "account": self.account, "detail": {"phase": phase}}

    def login_step(self, step):
        self.loaded.wait(LOAD_WAIT)
        b = self.backend
        if b is None:
            raise peer.Error("internal", self.loading)
        lark = self.lark
        try:
            if step != "poll" or not self.qr:
                self.renewals = 0
                self.qr = b.login_begin()
                return self._qr(self.qr)
            state = b.login_poll()
            if state == lark.QR_DONE:
                self.qr = ""
                account = b.login_finish()
                self._signed_in(account)
                return {"done": True, "account": account}
            if state == lark.QR_EXPIRED and self.renewals < QR_RENEWALS:
                self.renewals += 1
                self.qr = b.login_begin()
                return self._qr(self.qr, "二维码过期了，换了一张新的")
            if state in (lark.QR_EXPIRED, lark.QR_FAILED):
                self.qr = ""
                raise peer.Error("internal", "登录已取消或二维码过期，请重新开始")
            return self._qr(self.qr, QR_HINTS.get(state, ""))
        except peer.Error:
            raise
        except Exception as e:
            raise peer.Error("internal", "登录失败：%s" % e)

    @staticmethod
    def _qr(content, hint=""):
        return {"step": "poll", "kind": "qr", "url": content, "interval": 2, "hint": hint or QR_HINTS["waiting"]}

    def logout(self):
        self.stop.set()
        if self.backend is not None:
            self.backend.logout()
        with self.mu:
            self.account, self.link, self.expired = "", "", ""
        self.changed()
        self.peer.call("connector.secrets.delete", {"contribution": ID})
        return {}

    def send(self, p):
        ref, chat = p.get("ref") or "", p.get("chat") or ""
        if not ref or not chat:
            raise peer.Error("invalid_params", "a send needs ref and chat")
        if p.get("images") or p.get("files"):
            raise peer.Error("cannot_send", "飞书扩展目前只发文字")
        if self.backend is None or not self.account:
            raise peer.Error("not_signed_in", "飞书没有登录")
        with self.mu:  # checked and claimed at once: two calls with one ref post once
            done = self.sent.get(ref)
            if done is None:
                self.sent[ref] = {"at": time.time()}
        if done and done.get("id"):
            return {"message_id": done["id"]}
        if done:
            raise peer.Error("uncertain", "这条上次发到一半，可能已经发出，不再重发")
        self._mark(ref, self.sent[ref])
        text, footer = p.get("text") or "", (p.get("footer") or "").strip()
        if footer and text.rstrip().endswith(footer):
            text = text.rstrip()[:-len(footer)].rstrip()
        lark = self.lark
        try:
            mid = self.backend.post(chat, text, footer, p.get("reply_to") or "")
        except lark.NotSent as e:
            self._mark(ref, None)
            raise peer.Error("cannot_send", str(e))
        except lark.Unsure as e:
            raise peer.Error("uncertain", str(e))
        except Exception as e:
            raise peer.Error("uncertain", "发送时出错，可能已经发出：%s" % e)
        try:
            self._mark(ref, {"at": time.time(), "id": mid})
        except OSError as e:  # it went out: say so even if the record didn't stick
            log.error("recording send %s: %s", ref, e)
        return {"message_id": mid}

    def working(self, p):
        if self.backend is None or not self.account:
            raise peer.Error("not_signed_in", "飞书没有登录")
        self.backend.mark_working(p.get("message_id") or "")  # once: your own request may be marked already
        return {}

    # Sends already made, by ref: a ref goes out once, across restarts.

    def _sent_file(self):
        return os.path.join(self.data, "sent.json")

    def _read_sent(self):
        try:
            with open(self._sent_file()) as f:
                return json.load(f)
        except FileNotFoundError:
            return {}
        except ValueError:
            log.warning("sent.json is damaged; starting it over")
            return {}

    def _mark(self, ref, rec):
        with self.mu:
            if rec is None:
                self.sent.pop(ref, None)
            else:
                self.sent[ref] = rec
            old = time.time() - SENT_KEEP
            self.sent = {k: v for k, v in self.sent.items() if v.get("at", 0) > old}
            snapshot = dict(self.sent)
        with self.write_mu:  # outside mu: a slow disk holds up no status call
            tmp = self._sent_file() + ".tmp"
            with open(tmp, "w") as f:
                json.dump(snapshot, f)
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, self._sent_file())

    # Starting, signing in, listening.

    def start(self):
        try:
            backend = self._load()
        except Exception as e:
            log.error("%s", e)
            with self.mu:
                self.loading = str(e) if isinstance(e, deps.DepsError) else "启动失败：%s" % e
            self.loaded.set()
            self.changed()
            return
        backend.trigger = self.trigger
        with self.mu:
            self.backend = backend
        self.loaded.set()
        saved = self._saved()
        while saved:
            try:
                self._signed_in(backend.restore(saved))
                break
            except self.lark.Expired as e:
                log.warning("kept sign-in no longer valid: %s", e)
                with self.mu:
                    self.expired = "expired"
                break
            except Exception as e:  # the network: try again later
                log.warning("signing in again: %s", e)
                if self.stop.wait(RESTORE_RETRY):
                    break
        self.changed()

    def _load(self):
        fake = os.environ.get("COLLIE_LARKX_FAKE") == "1"
        path = os.environ.get("COLLIE_LARKX_DEPS") or deps.ensure(os.path.join(PKG, "requirements.txt"), self.data)
        sys.path.insert(1, path)
        import lark

        self.lark = lark
        U = lark.load_upstream(PKG, self.data, self.region)
        if fake:
            import fake as fake_backend

            return fake_backend.Fake(U, self.region, self._save)
        return lark.Feishu(U, self.region, self._save)

    def _saved(self):
        try:
            res = self.peer.call("connector.secrets.load", {"contribution": ID}) or {}
        except peer.Error as e:
            log.warning("loading the kept sign-in: %s", e.message)
            return None
        if not res.get("data"):
            return None
        try:
            return json.loads(base64.b64decode(res["data"]))
        except ValueError:
            log.warning("the kept sign-in is damaged")
            return None

    def _save(self, saved):
        data = base64.b64encode(json.dumps(saved).encode()).decode()
        self.peer.call("connector.secrets.save", {"contribution": ID, "data": data})

    def _signed_in(self, account):
        self.stop.set()  # a listener of an earlier sign-in, and its websocket
        self.backend.stop()
        with self.mu:
            self.account, self.expired, self.link = account, "", ""
        self.stop = threading.Event()
        threading.Thread(target=self._listen, args=(self.stop,), daemon=True).start()
        self.changed()

    def _listen(self, stop):
        try:
            self.backend.listen(self._received, self._link, stop)
        except Exception as e:
            log.error("listening stopped: %s", e)
            self._link("down")

    def _link(self, state, why=""):
        with self.mu:
            if state == "expired":
                self.account, self.expired, self.link = "", why or "expired", ""
            else:
                self.link = state
        self.changed()

    def _received(self, m):
        try:
            self.peer.call("channel.receive", {"contribution": ID, "message": m})
        except peer.Error as e:
            log.warning("handing over message %s: %s", m.get("id"), e.message)

    def changed(self):
        self.peer.notify("connector.changed", {"contribution": ID, "scope": "status"})


def main():
    logging.basicConfig(stream=sys.stderr, level=logging.INFO, format="%(levelname)s %(message)s")
    Extension().peer.serve()


if __name__ == "__main__":
    main()
