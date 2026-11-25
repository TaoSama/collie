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
        f.names.get = lambda key, look: "示例群"
        rt = richtext.build(U.L, "你好 `x`")
        content = {"richText": protobuf_to_dict(rt)}
        m = f.normalize({"msg_id": "1", "chat_id": "c_g", "from_id": "u_a", "msg_type": 4, "chat_type": 2,
                         "content_data": content, "root_id": "0", "create_time": 1764000000})
        self.assertEqual((m["text"], m["chat_kind"], m["chat"], m["owner"], m["sender_name"]), ("你好 `x`", "group", "示例群", False, "甲"))
        self.assertEqual(m["at"], "2025-11-24T16:00:00Z")
        m = f.normalize({"msg_id": "2", "chat_id": "c_self", "from_id": "u_me", "msg_type": 4, "chat_type": 1,
                         "content_data": content, "parent_id": "1"})
        self.assertEqual((m["chat_kind"], m["owner"], m["quote"]), ("self", True, "你好 `x`"))
        self.assertIsNone(f.normalize({"msg_id": "3", "chat_id": "c", "from_id": "u", "msg_type": 6}))


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
        self.until("its messages", lambda: len(self.received) == 2)
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
