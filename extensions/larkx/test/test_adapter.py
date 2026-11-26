"""The larkx adapter without a Feishu account (extensions/larkx/README.md, 测试).

LARKX_UPSTREAM is a folder holding LarkAgentX's larkx/ at the pinned
commit; the Python running this has requirements.txt installed.
"""
import base64
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ADAPTER = os.path.join(os.path.dirname(HERE), "pkg", "adapter")
UPSTREAM = os.environ.get("LARKX_UPSTREAM", "")
sys.path.insert(0, ADAPTER)

import richtext  # noqa: E402

if not UPSTREAM:
    raise unittest.SkipTest("LARKX_UPSTREAM not set")

import lark  # noqa: E402

U = lark.load_upstream(UPSTREAM, tempfile.mkdtemp(), "feishu")
from protobuf_to_dict import protobuf_to_dict  # noqa: E402

E = U.L.entities.RichTextElement
FOOTER = "🤖 AI 代答，仅供参考 · Claude Code · Opus 5.5 · 12秒"


def tree(rt):
    """The post as nested (tag, text, children) for checking."""
    els = rt.elements.dictionary

    def node(eid):
        el = els[eid]
        text = ""
        if el.tag == richtext.TAG_TEXT:
            p = E.TextProperty()
            p.ParseFromString(el.property)
            text = p.content
        elif el.tag == richtext.TAG_A:
            p = E.AnchorProperty()
            p.ParseFromString(el.property)
            text = p.href
        return (el.tag, text, [node(k) for k in el.childIds])

    return [node(e) for e in rt.elementIds]


class Blocks(unittest.TestCase):
    def test_markdown(self):
        md = "# 标题\n\n第一段 `x = 1` 和 **粗** 与 *斜*\n> 引用一\n> 引用二\n```py\nprint(1)\n\n```\n- 项目\n见 [文档](https://example.com/d) 或 https://example.com/x。"
        b = richtext.blocks(md, FOOTER)
        self.assertEqual(b[0], ("p", [("text", "标题", richtext.BOLD)]))
        self.assertEqual(b[1], ("p", []))
        self.assertEqual(b[2][1][1], ("code", "x = 1"))
        self.assertIn(("text", "粗", richtext.BOLD), b[2][1])
        self.assertIn(("text", "斜", richtext.ITALIC), b[2][1])
        self.assertEqual(b[3][0], "quote")
        self.assertEqual(len(b[3][1]), 2)
        self.assertEqual(b[4], ("code", "py", ["print(1)", ""]))
        self.assertEqual(b[5], ("p", [("text", "• 项目", {})]))
        self.assertIn(("a", "文档", "https://example.com/d"), b[6][1])
        self.assertIn(("a", "https://example.com/x", "https://example.com/x"), b[6][1])
        self.assertEqual(b[-1], ("quote", [[("text", FOOTER, {})]]))

    def test_plain_stays(self):
        for s in ["a * b * c", "snake_case_name", "2*3=6", "| a | b |"]:
            self.assertEqual(richtext.pieces_text(richtext.blocks(s)[0][1]), s)

    def test_unclosed_fence(self):
        b = richtext.blocks("```\nx")
        self.assertEqual(b, [("code", "", ["x"])])


class Build(unittest.TestCase):
    def test_proto(self):
        rt = richtext.build(U.L, "看 `go test` 和 [链接](https://example.com)\n```go\nfmt.Println()\n```", FOOTER)
        t = tree(rt)
        self.assertEqual([n[0] for n in t], [richtext.TAG_P, richtext.TAG_CODE_BLOCK_V2, richtext.TAG_QUOTE])
        p = t[0][2]
        self.assertEqual(p[1][0], richtext.TAG_CODE)
        self.assertEqual(p[1][2][0][1], "go test")
        self.assertEqual(p[3], (richtext.TAG_A, "https://example.com", []))
        self.assertEqual(len(rt.anchorIds), 1)
        cb = E.CodeBlockV2Property()
        cb.ParseFromString(rt.elements.dictionary[rt.elementIds[1]].property)
        self.assertEqual(cb.language, E.CodeBlockV2Property.Language.Value("GO"))
        self.assertEqual(cb.contents[0].contents[0].content, "fmt.Println()")
        quote = t[2][2]
        self.assertEqual(quote[0][2][0][1], FOOTER)
        self.assertTrue(rt.innerText.endswith(FOOTER))

    def test_back_to_text(self):
        rt = richtext.build(U.L, "第一行 `c`\n> 引\n```\nx\n```", FOOTER)
        text = richtext.to_text(U.L, protobuf_to_dict(rt))
        self.assertEqual(text, "第一行 `c`\n> 引\n```\nx\n```\n> " + FOOTER)


class FakeClient:
    def __init__(self, answer):
        self.answer = answer
        self.sent = []
        self.thread = False

    def _gateway_post(self, packet):
        self.sent.append(packet)
        if isinstance(self.answer, Exception):
            raise self.answer
        return self.answer

    def is_thread_chat(self, chat):
        return self.thread


def answer(mid="", status=0):
    resp = U.P.PutMessageResponse()
    if mid:
        resp.message.id = mid
    pkt = U.P.Packet()
    pkt.status = status
    if mid:
        pkt.payload = resp.SerializeToString()
    return pkt.SerializeToString()


class Post(unittest.TestCase):
    def feishu(self, ans):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.client = FakeClient(ans)
        return f

    def sent(self, f):
        pkt = f.client.sent[-1]
        req = U.L.messages.PutMessageRequest()
        req.ParseFromString(pkt.payload)
        return pkt, req

    def test_post(self):
        f = self.feishu(answer("7001"))
        self.assertEqual(f.post("oc_1", "**好**", FOOTER), "7001")
        pkt, req = self.sent(f)
        self.assertEqual((pkt.cmd, req.type, req.chatId), (5, 2, "oc_1"))
        self.assertFalse(req.rootId)
        self.assertEqual(tree(req.content.richText)[-1][0], richtext.TAG_QUOTE)
        self.assertIn(req.cid, f.own_cids)
        self.assertIsNone(f.normalize({"msg_id": "7001", "chat_id": "oc_1", "from_id": "u", "msg_type": 2, "cid": req.cid}))

    def test_react(self):
        f = self.feishu(answer("1"))
        asked = []
        f.client.api = lambda name, data: asked.append((name, data))
        f.react("7001")
        f.react("doc:d1")
        self.assertEqual(asked, [("reactions.PutReactionRequest", {"messageId": "7001", "reactionType": "OnIt"})])

    def test_folded(self):
        f = self.feishu(answer("1"))
        f.client.api = lambda name, data: {"cards": {"c1": {"parentCardId": 6621832799357763847}, "c2": {"parentCardId": 0}}}
        self.assertEqual((f.folded("c1"), f.folded("c2"), f.folded("c3")), (True, False, None))

    def test_is_request(self):
        for text, ok in [("/ai 总结一下", True), ("  /AI：查查", True), ("/ai", True), ("/ai\n第二行", True),
                         ("/aide 不算", False), ("说 /ai", False), ("/a", False)]:
            self.assertEqual(lark.is_request(text, "/ai"), ok, text)
        self.assertFalse(lark.is_request("/ai 你好", ""))

    def test_own_request_marked_at_once(self):
        import threading
        import time

        f = self.feishu(answer("1"))
        f.me, f.trigger = "u_me", "/ai"
        f.user_name = lambda uid, chat: ""
        f.names.get = lambda key, look: ""
        asked, done = [], threading.Event()

        def api(name, data):
            asked.append((time.monotonic(), name, data))
            done.set()

        f.client.api = api
        rt = protobuf_to_dict(richtext.build(U.L, "/ai 今天有什么会"))
        msg = {"msg_id": "9", "chat_id": "c", "from_id": "u_me", "msg_type": 4, "chat_type": 1, "content_data": {"richText": rt}}
        got = time.monotonic()
        f._deliver(dict(msg), lambda m: None, got)
        self.assertTrue(done.wait(2))
        self.assertLess((asked[0][0] - got) * 1000, 20, "the reaction request goes out within 20ms")
        self.assertEqual(asked[0][1:], ("reactions.PutReactionRequest", {"messageId": "9", "reactionType": "OnIt"}))
        f.mark_working("9")  # Collie's own channel.working afterwards
        time.sleep(0.05)
        self.assertEqual(len(asked), 1, "marked once")
        f._deliver(dict(msg, msg_id="10", from_id="u_other"), lambda m: None, got)
        time.sleep(0.05)
        self.assertEqual(len(asked), 1, "someone else's /ai waits for Collie's decision")

    def test_reply_in_topic(self):
        f = self.feishu(answer("7002"))
        f.recent["m_child"] = ("m_root", "问")
        f.post("oc_1", "答", "", "m_child")
        _, req = self.sent(f)
        self.assertEqual((req.rootId, req.parentId, req.isReplyInThread), ("m_root", "m_root", True))
        f.client.thread = True
        f.post("oc_1", "答", "", "m_new")
        _, req = self.sent(f)
        self.assertEqual((req.rootId, req.isReplyInThread), ("m_new", False))

    def test_outcomes(self):
        import requests

        with self.assertRaises(lark.NotSent):
            self.feishu(answer(status=10008)).post("c", "x")
        with self.assertRaises(lark.Unsure):
            self.feishu(answer()).post("c", "x")
        with self.assertRaises(lark.Unsure):
            self.feishu(requests.exceptions.ReadTimeout("slow")).post("c", "x")
        with self.assertRaises(lark.NotSent):
            self.feishu(requests.exceptions.ConnectTimeout("no route")).post("c", "x")


class Normalize(unittest.TestCase):
    def test_text(self):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.me, f.self_chat = "u_me", "c_self"
        f.user_name = lambda uid, chat: {"u_me": "我", "u_a": "甲"}.get(uid, "")
        f.chat_info = lambda chat: {"name": "示例群", "isRemind": chat != "c_g"}
        f.folded = lambda chat: chat == "c_g"
        rt = richtext.build(U.L, "你好 `x`")
        content = {"richText": protobuf_to_dict(rt)}
        m = f.normalize({"msg_id": "1", "chat_id": "c_g", "from_id": "u_a", "msg_type": 4, "chat_type": 2,
                         "content_data": content, "root_id": "0", "create_time": 1764000000})
        self.assertEqual((m["text"], m["chat_kind"], m["chat"], m["owner"], m["sender_name"]), ("你好 `x`", "group", "示例群", False, "甲"))
        self.assertEqual(m["at"], "2025-11-24T16:00:00Z")
        self.assertTrue(m["muted"], "isRemind off is 免打扰")
        self.assertTrue(m["folded"])
        m = f.normalize({"msg_id": "2", "chat_id": "c_self", "from_id": "u_me", "msg_type": 4, "chat_type": 1,
                         "content_data": content, "parent_id": "1"})
        self.assertEqual((m["chat_kind"], m["owner"], m["quote"]), ("self", True, "你好 `x`"))
        self.assertFalse(m["muted"])
        self.assertFalse(m["folded"])
        self.assertIsNone(f.normalize({"msg_id": "3", "chat_id": "c", "from_id": "u", "msg_type": 6}))
        card = {"cardHeader": {"title": "分析中"}, "jsonCard": '{"body":{}}'}
        m = f.normalize({"msg_id": "4", "chat_id": "c_g", "from_id": "u_a", "msg_type": 14, "chat_type": 2, "content_data": card})
        self.assertEqual(m["text"], "[卡片] 分析中", "a card shows its title, not its JSON")


class Pushes(unittest.TestCase):
    def feishu(self):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.me = "u_me"
        f.user_name = lambda uid, chat: {"u_a": "甲"}.get(uid, "")
        return f

    @staticmethod
    def feed(status=1):
        req = U.L.docs.PushDocFeedRequest(status=status)
        req.docFeed.id, req.docFeed.title, req.docFeed.url = "f1", "周报", "https://example.feishu.cn/docx/abc"
        dm = req.docMessages["d1"]
        dm.id, dm.type, dm.fromId, dm.createTime = "d1", 2, "u_a", 1764000000
        dm.richText.CopyFrom(richtext.build(U.L, "这里数字不对 `42`"))
        req.docMessages["d0"].CopyFrom(U.L.entities.DocMessage(id="d0", type=4, fromId="u_a"))  # solved: not news
        return req.SerializeToString()

    def test_doc_comment(self):
        f = self.feishu()
        out = f.doc_comments(self.feed())
        self.assertEqual(len(out), 1)
        m = out[0]
        self.assertEqual((m["id"], m["chat_id"], m["chat"], m["chat_kind"], m["sender_name"], m["owner"]),
                         ("doc:d1", "doc:f1", "周报", "doc", "甲", False))
        self.assertEqual(m["text"], "（评论了文档：https://example.feishu.cn/docx/abc）\n这里数字不对 `42`")
        self.assertEqual(f.doc_comments(self.feed(status=2)), [], "an update repeating it is not news")
        self.assertEqual(self.feishu().doc_comments(self.feed(status=3)), [])
        with self.assertRaises(lark.NotSent):
            f.client = FakeClient(answer("1"))
            f.post("doc:f1", "收到")

    def test_frames(self):
        import gzip

        f = self.feishu()
        pkt = U.P.Packet(sid="s1", cmd=14004, payload=self.feed()).SerializeToString()
        for frame in (U.P.Frame(payload=pkt), U.P.Frame(payloadEncoding="gzip", payload=gzip.compress(pkt))):
            p = f._packet(frame.SerializeToString())
            self.assertEqual((p.sid, p.cmd), ("s1", 14004))
        self.assertIsNone(f._packet(U.P.Frame(payloadEncoding="json", payload=b"{}").SerializeToString()))

    def test_sid_pull(self):
        f = self.feishu()

        class Pull:
            asked = None

            def api(self, name, data):
                Pull.asked = (name, data)
                return {"packets": {"s1": {"cmd": 14004, "payload": Pushes.feed()}}}

        f.client = Pull()
        got = []
        f._push(6000, b"", U.L.improto.Packet(sid="s1", compactedSids=[7, 8]), got.append)
        self.assertEqual(Pull.asked, ("improto.PullPacketsBySidsRequest", {"sids": ["s1", "7", "8"]}))
        self.assertEqual([m["id"] for m in got], ["doc:d1"])


class Session(unittest.TestCase):
    """The receive loop against a local websocket server: compressed frames, acks, document comments."""

    def test_session(self):
        import asyncio
        import gzip

        import websockets

        pkt = U.P.Packet(sid="s9", cmd=14004, payload=Pushes.feed()).SerializeToString()
        frame = U.P.Frame(payloadEncoding="gzip", payload=gzip.compress(pkt)).SerializeToString()
        acks, got = [], []

        async def serve(ws):
            await ws.send(frame)
            await ws.send(b"not a frame")
            await asyncio.sleep(0.3)

        class Client:
            async def heartbeat_loop(self, ws):
                await asyncio.sleep(3600)

            async def send_ack(self, ws, sid):
                acks.append(sid)

        async def main():
            async with websockets.serve(serve, "127.0.0.1", 0) as srv:
                port = srv.sockets[0].getsockname()[1]
                c = Client()
                c.build_ws_url = lambda: "ws://127.0.0.1:%d" % port
                f = Pushes().feishu()
                f.client = c
                await f._session(got.append)
                await asyncio.sleep(0.2)
                return f

        f = asyncio.run(main())
        self.assertEqual(acks, ["s9"])
        self.assertEqual([m["id"] for m in got], ["doc:d1"])
        self.assertEqual(f._bad_frames, 0, "a bad frame is counted, then said and reset")


class Protocol(unittest.TestCase):
    """main.py as the host runs it, with the fake Feishu."""

    def setUp(self):
        self.data = tempfile.mkdtemp()
        # This Python has the packages already: COLLIE_LARKX_DEPS only skips pip.
        env = dict(os.environ, COLLIE_LARKX_FAKE="1", COLLIE_LARKX_DEPS=self.data, COLLIE_EXTENSION_DATA=self.data)
        self.pkg = tempfile.mkdtemp()
        os.symlink(ADAPTER, os.path.join(self.pkg, "adapter"))
        os.symlink(os.path.join(UPSTREAM, "larkx"), os.path.join(self.pkg, "larkx"))
        self.proc = subprocess.Popen([sys.executable, os.path.join(self.pkg, "adapter", "main.py")], env=env,
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        self.secret = None
        self.received = []
        self.events = []
        self.answers = {}
        self.cv = threading.Condition()
        threading.Thread(target=self.read, daemon=True).start()
        self.n = 0

    def tearDown(self):
        self.proc.stdin.close()
        self.proc.wait(10)
        self.proc.stdout.close()

    def read(self):
        for line in self.proc.stdout:
            f = json.loads(line)
            if f.get("method"):
                res = {}
                if f["method"] == "connector.secrets.load":
                    res = {"data": self.secret or ""}
                elif f["method"] == "connector.secrets.save":
                    self.secret = f["params"]["data"]
                elif f["method"] == "connector.secrets.delete":
                    self.secret = None
                elif f["method"] == "channel.receive":
                    self.received.append(f["params"]["message"])
                self.write({"id": f["id"], "result": res})
            with self.cv:
                if f.get("event"):
                    self.events.append(f)
                elif f.get("id") and not f.get("method"):
                    self.answers[f["id"]] = f
                self.cv.notify_all()

    def write(self, f):
        self.proc.stdin.write((json.dumps(f) + "\n").encode())
        self.proc.stdin.flush()

    def call(self, method, params=None):
        self.n += 1
        fid = "h%d" % self.n
        self.write({"id": fid, "method": method, "params": params or {}})
        with self.cv:
            self.cv.wait_for(lambda: fid in self.answers, 20)
        return self.answers[fid]

    def until(self, what, cond):
        with self.cv:
            if not self.cv.wait_for(cond, 20):
                self.fail(what)

    def status(self):
        return self.call("connector.status")["result"]

    def test_flow(self):
        r = self.call("initialize", {"protocol": 1, "extension": {"id": "larkx", "version": "0.4.0", "data_dir": self.data}, "config": {}})
        self.assertEqual(r["result"]["id"], "larkx")
        self.until("loaded", lambda: self.events)
        self.until("not signed in", lambda: self.status()["state"] == "not_signed_in")
        step = self.call("connector.login.step", {"step": ""})["result"]
        self.assertEqual((step["kind"], step["step"]), ("qr", "poll"))
        step = self.call("connector.login.step", {"step": "poll"})["result"]
        self.assertEqual(step["hint"], "已扫码，请在手机上确认")
        step = self.call("connector.login.step", {"step": "poll"})["result"]
        self.assertTrue(step["done"])
        self.assertIn("example", base64.b64decode(self.secret).decode())
        self.until("its messages", lambda: len(self.received) == 3)
        self.assertEqual(self.call("channel.settings", {"trigger": "/ai"})["result"], {})
        self.assertEqual(self.call("channel.working", {"chat": "c_self", "message_id": "m3"})["result"], {})
        with open(os.path.join(self.data, "fake-reactions.jsonl")) as f:
            self.assertEqual(json.loads(f.readline()), {"message_id": "m3", "key": "OnIt"})
        self.assertEqual(self.status()["account"], "示例账号")

        text = "改到 **三点**\n\n" + FOOTER
        send = {"ref": "o1", "chat": "c_dm", "text": text, "footer": FOOTER, "kind": "answer", "reply_to": "m1"}
        self.assertEqual(self.call("channel.send", send)["result"], {"message_id": "m_sent_1"})
        self.assertEqual(self.call("channel.send", send)["result"], {"message_id": "m_sent_1"}, "a ref goes once")
        with open(os.path.join(self.data, "fake-posts.jsonl")) as f:
            posts = [json.loads(x) for x in f]
        self.assertEqual(len(posts), 1)
        self.assertEqual((posts[0]["text"], posts[0]["footer"], posts[0]["reply_to"]), ("改到 **三点**", FOOTER, "m1"))
        err = self.call("channel.send", dict(send, ref="o2", chat="c_unsure"))["error"]
        self.assertEqual(err["code"], "uncertain")
        self.assertEqual(self.call("channel.send", dict(send, ref="o2", chat="c_dm"))["error"]["code"], "uncertain")
        self.assertEqual(self.call("channel.send", dict(send, ref="o3", chat="c_refuse"))["error"]["code"], "cannot_send")
        self.assertEqual(self.call("channel.send", dict(send, ref="o4", images=[{"mime": "image/png", "data": ""}]))["error"]["code"], "cannot_send")

        self.call("connector.logout")
        self.assertIsNone(self.secret)
        self.assertEqual(self.status()["state"], "not_signed_in")


if __name__ == "__main__":
    unittest.main()
