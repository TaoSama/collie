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
import types
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

    def test_docs_container_and_list(self):
        """Feishu's rich editor wraps posts in a DOCS (22) root element; it must not prepend [文档] or glue list items."""
        rt = U.L.entities.RichText()
        els = rt.elements.dictionary
        els["1"].tag = E.DOCS
        els["1"].childIds.extend(["2", "4"])
        els["2"].tag = E.P
        els["2"].childIds.append("3")
        els["3"].tag, els["3"].property = E.TEXT, E.TextProperty(content="/ai 整理文档").SerializeToString()
        els["4"].tag = E.UL
        els["4"].childIds.extend(["5", "7"])
        els["5"].tag = E.LI
        els["5"].childIds.append("6")
        els["6"].tag, els["6"].property = E.A, E.AnchorProperty(href="https://example.com/1", textContent="https://example.com/1").SerializeToString()
        els["7"].tag = E.LI
        els["7"].childIds.append("8")
        els["8"].tag, els["8"].property = E.A, E.AnchorProperty(href="https://example.com/2", textContent="https://example.com/2").SerializeToString()
        rt.elementIds.append("1")

        text = richtext.to_text(U.L, protobuf_to_dict(rt))
        self.assertEqual(text, "/ai 整理文档\n- https://example.com/1\n- https://example.com/2")
        self.assertTrue(lark.is_request(text, "/ai"))


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
        msg = {"msg_id": "9", "chat_id": "c", "from_id": "u_me", "msg_type": 4, "chat_type": 1, "content_data": {"richText": rt},
               "create_time": int(time.time())}
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

    def test_marked_only_when_taken(self):
        """Marked 在做了 only where Collie will take the request: as strict as the hub."""
        import time

        f = self.feishu(answer("1"))
        f.me, f.trigger = "u_me", "/ai"
        asked = []
        f.client.api = lambda name, data: asked.append(data["messageId"])
        f.react = lambda mid: asked.append(mid)
        now = int(time.time())

        def msg(mid, text, **kw):
            rt = protobuf_to_dict(richtext.build(U.L, text))
            m = {"msg_id": mid, "chat_id": "c", "from_id": "u_me", "msg_type": 4, "chat_type": 1,
                 "content_data": {"richText": rt}, "create_time": now}
            m.update(kw)
            return m

        f._quick(msg("1", "/ai 好"), None)
        f._quick(msg("2", "/ai 旧的", create_time=now - 3600), None)  # history
        f.own_cids["c-ours"] = True
        f._quick(msg("3", "/ai 回声", cid="c-ours"), None)  # this adapter's own post
        post = msg("4", "/ai 帖子", msg_type=2)
        post["content_data"]["title"] = "标题"  # Collie reads "**标题**\n/ai …": no request
        f._quick(post, None)
        f.ignored = True
        f._quick(msg("5", "/ai 被忽略"), None)
        time.sleep(0.1)
        self.assertEqual(asked, ["1"])

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
        self.assertNotIn("chat_tags", m, "a plain group has no tags")
        f.chat_info = lambda chat: {"name": "[N1][Main][PVT][低概率] Wi-Fi 条件任务端侧执行成功后任务卡片错误显示「当前任务服务超时」 - [缺...",
                                    "description": "https://meego.larkoffice.com/new_ocean/issue/detail/7399029097?tabKey=detail"}
        m = f.normalize({"msg_id": "5", "chat_id": "c_m", "from_id": "u_a", "msg_type": 14, "chat_type": 2, "content_data": card})
        self.assertEqual(m["chat_tags"], ["meego-issue"], "a Meego group whose cut name lost its tag")


class ChatTags(unittest.TestCase):
    def test_meego(self):
        story = "https://meego.larkoffice.com/new_ocean/story/detail/7398829062?tabKey=detail"
        for info, want in [
            ({"name": "Harness 2.5 交互体验优化 - [需求同步群]", "description": story}, ["meego-story"]),
            ({"name": "Harness 2.5 交互体验优化", "description": story}, ["meego-story"]),
            ({"name": "闪退 - [缺陷同步群]"}, ["meego-issue"]),
            ({"name": "[Genesis-M][beta][2115872568] 让...", "description": " https://project.feishu.cn/space/issue/detail/42 "}, ["meego-issue"]),
            # A link someone wrote into a description among other words is not Meego's.
            ({"name": "Nerida 业务内部UT反馈-新", "description": "需求见 " + story}, []),
            ({"name": "M项目独立端反馈群", "description": "https://meego.larkoffice.com/new_ocean/issue/homepage"}, []),
            ({"name": "OMNI CR", "description": ""}, []),
        ]:
            self.assertEqual(lark.chat_tags(info), want, info["name"])


def at_rich(uid, name, text):
    """A received rich text: an @ (a real AT element) and then text."""
    rt = U.L.entities.RichText()
    para = rt.elements.dictionary["1"]
    para.tag, para.property = richtext.TAG_P, E.ParagraphProperty().SerializeToString()
    at = rt.elements.dictionary["2"]
    at.tag, at.property = richtext.TAG_AT, E.AtProperty(userId=uid, content="@" + name).SerializeToString()
    tx = rt.elements.dictionary["3"]
    tx.tag, tx.property = richtext.TAG_TEXT, E.TextProperty(content=text).SerializeToString()
    para.childIds.extend(["2", "3"])
    rt.elementIds.append("1")
    return protobuf_to_dict(rt)


class Methods(unittest.TestCase):
    def test_used_methods_exist(self):
        """Every method the adapter calls on itself is there: tests that stub some must not hide a lost one."""
        import ast
        import inspect

        tree = ast.parse(inspect.getsource(lark))
        defined = {n.name for c in tree.body if isinstance(c, ast.ClassDef) and c.name == "Feishu" for n in c.body if isinstance(n, ast.FunctionDef)}
        called = {n.func.attr for c in tree.body if isinstance(c, ast.ClassDef) and c.name == "Feishu" for n in ast.walk(c)
                  if isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute) and isinstance(n.func.value, ast.Name) and n.func.value.id == "self"}
        self.assertEqual(called - defined - {"names", "client"}, set())

    def test_real_names_unstubbed(self):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.me = "u_me"

        class Client:
            def get_user_name(self, uid, chat):
                return {"u_me": "陆文韬"}.get(uid)

            def api(self, name, data):
                return {"chatters": {"u_me": {"name": "陆文韬"}}}

        f.client = Client()
        self.assertEqual(f.user_name("u_me", "c"), "陆文韬")
        self.assertEqual(f.dm_name("c", f.peer("c")), "陆文韬（我）")


class Names(unittest.TestCase):
    """A direct chat is named as in Feishu: the other person, or you （我）; never 还不知道对方是谁."""

    def feishu(self, chatters):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.me, f.self_chat = "u_me", ""
        f.client = FakeClient(answer("1"))
        f.client.api = lambda name, data: {"chatters": chatters}
        f.user_name = lambda uid, chat: {"u_me": "陆文韬", "u_b": "乙"}.get(uid, "")
        f.chat_info = lambda chat: {}
        f.folded = lambda chat: None
        return f

    def msg(self, mid, sender, chat="d1"):
        rt = protobuf_to_dict(richtext.build(U.L, "嗨"))
        return {"msg_id": mid, "chat_id": chat, "from_id": sender, "msg_type": 4, "chat_type": 1, "content_data": {"richText": rt}}

    def test_names(self):
        f = self.feishu({"u_me": {"name": "陆文韬"}, "u_b": {"localizedName": "乙"}})
        m = f.normalize(self.msg("1", "u_me"))
        self.assertEqual((m["chat"], m["chat_kind"]), ("乙", "dm"), "your own message: the other person from the members")
        self.assertEqual(f.normalize(self.msg("2", "u_b", "d2"))["chat"], "乙", "theirs: the sender")
        m = self.feishu({"u_me": {"name": "陆文韬"}}).normalize(self.msg("3", "u_me", "d3"))
        self.assertEqual((m["chat"], m["chat_kind"]), ("陆文韬（我）", "self"), "only you in it: your chat with yourself")
        m = self.feishu({}).normalize(self.msg("4", "u_me", "6617623665812963591"))
        self.assertEqual(m["chat"], "私聊 963591", "nothing known: a short id, never 还不知道对方是谁")

    def test_bot_chat_tagged(self):
        """A direct chat with a bot or app (chatter type 2) is tagged bot, one with a person not."""
        m = self.feishu({"u_me": {"name": "陆文韬", "type": 1}, "u_b": {"name": "智能纪要助手", "type": 2}}).normalize(self.msg("5", "u_b", "d5"))
        self.assertEqual((m["chat_kind"], m["chat_tags"]), ("dm", ["bot"]))
        m = self.feishu({"u_me": {"name": "陆文韬", "type": 1}, "u_b": {"name": "乙", "type": 1}}).normalize(self.msg("6", "u_b", "d6"))
        self.assertEqual(m["chat_tags"], [])


class Mentions(unittest.TestCase):
    """@自己唤起 and 群话题各开对话."""

    def feishu(self):
        f = lark.Feishu(U, "feishu", lambda d: None)
        f.me, f.self_chat = "u_me", "c_self"
        f.user_name = lambda uid, chat: ""
        f.chat_info = lambda chat: {"name": "群", "isRemind": True}
        f.folded = lambda chat: False
        return f

    def msg(self, mid, sender, rich, chat_type=2, root="0"):
        return {"msg_id": mid, "chat_id": "c_g", "from_id": sender, "msg_type": 4, "chat_type": chat_type,
                "content_data": {"richText": rich}, "root_id": root}

    def test_self_mention(self):
        f = self.feishu()
        self.assertEqual(richtext.at_users(U.L, at_rich("u_me", "我", " 总结一下")), {"u_me"})
        m = f.normalize(self.msg("1", "u_me", at_rich("u_me", "我", " 总结一下")))
        self.assertEqual((m.get("mention"), m["text"]), (True, "总结一下"), "the @ taken out")
        m = f.normalize(self.msg("2", "u_a", at_rich("u_me", "我", " 看看")))
        self.assertFalse(m.get("mention"), "someone else @-ing you is no request")
        self.assertEqual(m["text"], "@我 看看")
        m = f.normalize(self.msg("3", "u_me", at_rich("u_b", "乙", " 你看")))
        self.assertFalse(m.get("mention"), "@-ing someone else")
        f.self_mention = False
        self.assertFalse(f.normalize(self.msg("4", "u_me", at_rich("u_me", "我", " x"))).get("mention"))

    def test_at_me(self):
        f = self.feishu()
        self.assertTrue(f.normalize(self.msg("1", "u_a", at_rich("u_me", "我", " 看看")))["at_me"])
        self.assertTrue(f.normalize(self.msg("2", "u_a", at_rich("all", "所有人", " 开会")))["at_me"], "@所有人 too")
        self.assertNotIn("at_me", f.normalize(self.msg("3", "u_a", at_rich("u_b", "乙", " 你看"))))
        self.assertNotIn("at_me", f.normalize(self.msg("4", "u_me", at_rich("u_me", "我", " x"))), "your own @ is a request, not news")

    def test_self_mention_marked_at_once(self):
        import threading
        import time

        f = self.feishu()
        f.client, f.trigger = FakeClient(answer("1")), "/ai"
        asked, done = [], threading.Event()
        f.client.api = lambda name, data: (asked.append(data["messageId"]), done.set())
        f._deliver(dict(self.msg("9", "u_me", at_rich("u_me", "我", " 看下")), create_time=int(time.time())), lambda m: None)
        self.assertTrue(done.wait(2))
        self.assertEqual(asked, ["9"])

    def test_topics(self):
        f = self.feishu()
        rich = protobuf_to_dict(richtext.build(U.L, "/ai 你好"))
        self.assertEqual(f.normalize(self.msg("1", "u_me", rich))["thread"], "1", "outside a topic: its own")
        self.assertEqual(f.normalize(self.msg("2", "u_me", rich, root="77"))["thread"], "77", "in a topic: the topic")
        self.assertNotIn("thread", f.normalize(self.msg("3", "u_me", rich, chat_type=1)), "a direct chat is one conversation")
        f.group_topics = False
        self.assertNotIn("thread", f.normalize(self.msg("4", "u_me", rich)))


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
        out = []

        f._deliver_doc_comments(self.feed(), out.append)
        self.assertEqual(len(out), 1)
        m = out[0]
        self.assertEqual((m["id"], m["chat_id"], m["chat"], m["chat_kind"], m["sender_name"], m["owner"]),
                         ("doc:d1", "doc:f1", "周报", "doc", "甲", False))
        self.assertEqual(m["text"], "（评论了文档：https://example.feishu.cn/docx/abc）\n这里数字不对 `42`")
        f._deliver_doc_comments(self.feed(status=2), out.append)
        self.feishu()._deliver_doc_comments(self.feed(status=3), out.append)
        self.assertEqual(len(out), 1, "repeated comments and other feed updates are not news")
        with self.assertRaises(lark.NotSent):
            f.client = FakeClient(answer("1"))
            f.post("doc:f1", "收到")

    def test_doc_comment_survives_hub_unavailable(self):
        from main import Extension
        from peer import Error

        f = self.feishu()
        ext = Extension()
        ext.data = tempfile.mkdtemp()
        attempts, offline = [], [True]

        def call(method, params):
            self.assertEqual(method, "channel.receive")
            attempts.append(params["message"]["id"])
            if offline[0]:
                raise Error("hub_unavailable", "offline")

        ext.peer = types.SimpleNamespace(call=call, closed=threading.Event())

        def listen(on_message, on_link, stop):
            f._pushes._push(14004, self.feed(), None, on_message)
            self.assertIn("doc:d1", ext.pending)
            # Replayed feed update while offline does not double-queue.
            f._pushes._push(14004, self.feed(status=2), None, on_message)
            offline[0] = False
            self.assertTrue(ext._flush_pending())

        ext.backend = types.SimpleNamespace(listen=listen)
        ext._listen(types.SimpleNamespace(wait=lambda _: False))
        self.assertEqual(attempts, ["doc:d1", "doc:d1"], "queued once, replayed on flush")
        self.assertEqual(ext.pending, {})

    def test_messages_queued_to_disk_and_flushed_on_hub_recovery(self):
        from main import Extension
        from peer import Error

        for code in ("hub_unavailable", "timeout"):
            with self.subTest(code=code):
                data = tempfile.mkdtemp()
                ext = Extension()
                ext.data = data
                offline = [True]
                delivered = []

                def call(method, params):
                    self.assertEqual(method, "channel.receive")
                    if offline[0]:
                        raise Error(code, "offline")
                    delivered.append(params["message"])

                ext.peer = types.SimpleNamespace(call=call, closed=threading.Event())
                m1 = {"id": "m1", "chat_id": "c1", "chat_kind": "group", "text": "hello"}
                m2 = {"id": "m2", "chat_id": "c_self", "chat_kind": "self", "owner": True, "text": "/ai 查一下"}
                m1_upd = {"id": "m1", "chat_id": "c1", "chat_kind": "group", "text": "hello updated"}

                self.assertTrue(ext._received(m1))
                self.assertTrue(ext._received(m2))
                self.assertTrue(ext._received(m1_upd))  # same id deduplicated in pending
                with open(os.path.join(data, "pending.json")) as fp:
                    disk = json.load(fp)
                self.assertEqual(list(disk.keys()), ["m1", "m2"])
                self.assertEqual(disk["m1"]["message"]["text"], "hello updated")

                # Simulate an extension restart loading pending.json from disk.
                ext2 = Extension()
                ext2.data = data
                ext2.peer = types.SimpleNamespace(call=call, closed=threading.Event())
                ext2.pending = ext2._read_pending()
                self.assertEqual(list(ext2.pending.keys()), ["m1", "m2"])

                offline[0] = False
                self.assertTrue(ext2._flush_pending())
                self.assertEqual([m["id"] for m in delivered], ["m1", "m2"])
                self.assertEqual(delivered[0]["text"], "hello updated")
                self.assertEqual(delivered[1]["text"], "/ai 查一下")
                with open(os.path.join(data, "pending.json")) as fp:
                    self.assertEqual(json.load(fp), {})

    def test_doc_comment_replay_while_handoff_is_pending(self):
        f = self.feishu()
        started, release = threading.Event(), threading.Event()
        attempts = []

        def blocked(m):
            attempts.append(m["id"])
            started.set()
            release.wait(2)

        thread = threading.Thread(target=f._deliver_doc_comments, args=(self.feed(), blocked))
        thread.start()
        try:
            self.assertTrue(started.wait(2))
            f._deliver_doc_comments(self.feed(status=2), lambda m: attempts.append(m["id"]))
        finally:
            release.set()
            thread.join()
        self.assertEqual(attempts, ["doc:d1"])

    def test_frames(self):
        import gzip

        stream = self.feishu()._pushes
        pkt = U.P.Packet(sid="s1", cmd=14004, payload=self.feed()).SerializeToString()
        for frame in (U.P.Frame(payload=pkt), U.P.Frame(payloadEncoding="gzip", payload=gzip.compress(pkt))):
            p = stream._packet(frame.SerializeToString())
            self.assertEqual((p.sid, p.cmd), ("s1", 14004))
        self.assertIsNone(stream._packet(U.P.Frame(payloadEncoding="json", payload=b"{}").SerializeToString()))

    def test_sid_pull(self):
        f = self.feishu()

        class Pull:
            asked = None

            def api(self, name, data):
                Pull.asked = (name, data)
                return {"packets": {"s1": {"cmd": 14004, "payload": Pushes.feed()}}}

        f.client = Pull()
        got = []
        f._pushes._push(6000, b"", U.L.improto.Packet(sid="s1", compactedSids=[7, 8]), got.append)
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
        chat = U.P.Frame(payload=U.P.Packet(sid="s10", cmd=6).SerializeToString()).SerializeToString()
        acks, got, chat_progress = [], [], []
        doc_started, release_doc, chat_delivered = threading.Event(), threading.Event(), threading.Event()

        async def serve(ws):
            await ws.send(frame)
            await asyncio.to_thread(doc_started.wait, 2)
            await ws.send(chat)
            chat_progress.append(await asyncio.to_thread(chat_delivered.wait, 1))
            release_doc.set()
            await ws.send(b"not a frame")
            await asyncio.sleep(0.3)

        def received(m):
            if m["id"] == "doc:d1":
                doc_started.set()
                release_doc.wait(3)
            else:
                chat_delivered.set()
            got.append(m)

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
                f._pushes._messages = lambda _: [{"id": "m1"}]
                f._pushes._deliver = lambda m, callback, _: callback(m)
                await f._pushes._session(received)
                await asyncio.sleep(0.2)
                return f

        f = asyncio.run(main())
        self.assertEqual(acks, ["s9", "s10"])
        self.assertEqual(chat_progress, [True], "a blocked comment must not stall normal pushes")
        self.assertCountEqual([m["id"] for m in got], ["doc:d1", "m1"])
        self.assertEqual(f._pushes._bad_frames, 0, "a bad frame is counted, then said and reset")


class Restore(unittest.TestCase):
    """A network failure while signing in again is not an expired session."""

    def feishu(self, err, recheck):
        f = lark.Feishu(U, "feishu", lambda d: None)

        def boom(auth):
            raise U.auth.AuthExpired(err)

        f.U = type("U", (), {"client": type("C", (), {"LarkClient": staticmethod(boom)}), "auth": U.auth, "region": U.region})
        f.signed_out = lambda auth=None: recheck
        return f

    def test_restore(self):
        saved = {"region": "feishu", "cookies": {"session": "s"}}
        net = "凭证已过期或无效: ticket 探针网络异常(先检查网络,不一定是凭证问题): Read timed out.\n请从浏览器重新复制 cookie 后执行: lark auth import"
        with self.assertRaises(lark.Unreachable) as e:
            self.feishu(net, True).restore(saved)
        self.assertNotIn("cookie", str(e.exception), "no advice to import cookies again for a timeout")
        gone = "凭证已过期或无效: 会话已被服务端注销(凭证保存于 3.0h 前): 未登录"
        with self.assertRaises(lark.Expired):
            self.feishu(gone, True).restore(saved)
        with self.assertRaises(lark.Unreachable):
            self.feishu(gone, False).restore(saved)  # asked again, Feishu didn't say so: not expired

    def test_websocket_expiry_confirmed(self):
        from lark_ws import PushStream
        import threading

        links, stop = [], threading.Event()
        calls = {"n": 0}

        class Client:
            def build_ws_url(self):
                calls["n"] += 1
                if calls["n"] > 1:
                    stop.set()
                raise U.auth.AuthExpired("获取 ticket 失败")

        ps = PushStream(U, threading.Lock(), lambda: Client(), None, None, signed_out=lambda: False)
        ps.listen(lambda m: None, lambda state, why="": links.append(state), stop)
        self.assertNotIn("expired", links, "unconfirmed: tried again")
        stop.clear()
        calls["n"] = 0
        ps = PushStream(U, threading.Lock(), lambda: Client(), None, None, signed_out=lambda: True)
        ps.listen(lambda m: None, lambda state, why="": links.append(state), stop)
        self.assertEqual(links[-1], "expired")


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
        self.secret_gate = None
        self.secret_loaded = threading.Event()
        self.received = []
        self.fail_receive = False
        self.events = []
        self.answers = {}
        self.cv = threading.Condition()
        threading.Thread(target=self.read, daemon=True).start()
        self.n = 0

    def tearDown(self):
        if self.secret_gate is not None:
            self.secret_gate.set()
        self.proc.stdin.close()
        self.proc.wait(10)
        self.proc.stdout.close()

    def read(self):
        for line in self.proc.stdout:
            f = json.loads(line)
            if f.get("method"):
                if f["method"] == "connector.secrets.load" and self.secret_gate is not None:
                    gate = self.secret_gate

                    def finish(req=f, ev=gate):
                        self.secret_loaded.set()
                        ev.wait(20)
                        self.write({"id": req["id"], "result": {"data": self.secret or ""}})

                    threading.Thread(target=finish, daemon=True).start()
                    continue
                res = {}
                if f["method"] == "connector.secrets.load":
                    self.secret_loaded.set()
                    res = {"data": self.secret or ""}
                elif f["method"] == "connector.secrets.save":
                    self.secret = f["params"]["data"]
                elif f["method"] == "connector.secrets.delete":
                    self.secret = None
                elif f["method"] == "channel.receive":
                    if self.fail_receive:
                        self.write({"id": f["id"], "error": {"code": "hub_unavailable", "message": "restarting"}})
                        continue
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


    def test_restore_retries(self):
        """Signed in before, Feishu slow at first: 正在连接, never 未登录, then signed in."""
        import time

        saved = {"region": "feishu", "cookies": {"session": "example"}, "user_id": "u_me", "unreachable_once": True}
        self.secret = base64.b64encode(json.dumps(saved).encode()).decode()
        self.call("initialize", {"protocol": 1, "extension": {"id": "larkx", "version": "0.4.8", "data_dir": self.data}, "config": {}})
        seen = []
        for _ in range(80):
            st = self.status()
            seen.append(st.get("detail", {}).get("phase") or st["state"])
            if st.get("account"):
                break
            time.sleep(0.1)
        self.assertEqual(self.status()["account"], "示例账号")
        self.assertIn("正在连接飞书", seen)
        self.assertNotIn("not_signed_in", seen)

    def test_pending_flush_after_hub_unavailable(self):
        """Messages that arrive while the hub returns hub_unavailable are persisted and flushed on reconnect."""
        import time

        self.fail_receive = True
        saved = {"region": "feishu", "cookies": {"session": "example"}, "user_id": "u_me"}
        self.secret = base64.b64encode(json.dumps(saved).encode()).decode()
        self.call("initialize", {"protocol": 1, "extension": {"id": "larkx", "version": "0.4.22", "data_dir": self.data}, "config": {}})
        pfile = os.path.join(self.data, "pending.json")
        for _ in range(100):
            if os.path.exists(pfile):
                with open(pfile) as f:
                    try:
                        if {"m1", "m2", "m3"} <= set(json.load(f)):
                            break
                    except ValueError:
                        pass
            time.sleep(0.05)
        else:
            self.fail("pending.json did not record offline messages")
        self.assertEqual(self.received, [])

        # Hub comes back and asks for status: the adapter wakes and replays all queued messages.
        self.fail_receive = False
        self.status()
        self.until("replayed messages", lambda: {"m1", "m2", "m3"} <= {m["id"] for m in self.received})
        self.assertIn("/ai 今天有什么会", [m["text"] for m in self.received if m["id"] == "m3"])
        for _ in range(40):
            with open(pfile) as f:
                if json.load(f) == {}:
                    break
            time.sleep(0.05)
        with open(pfile) as f:
            self.assertEqual(json.load(f), {})

    def test_send_not_ready_while_restoring_then_wakes_outbox(self):
        """A send arriving while larkx is restoring returns not_ready (not not_signed_in), then outbox wakes on sign-in."""
        saved = {"region": "feishu", "cookies": {"session": "example"}, "user_id": "u_me", "unreachable_once": True}
        self.secret = base64.b64encode(json.dumps(saved).encode()).decode()
        self.call("initialize", {"protocol": 1, "extension": {"id": "larkx", "version": "0.4.23", "data_dir": self.data}, "config": {}})

        send = {"ref": "orestore1", "chat": "c_dm", "text": "恢复后再发\n\n" + FOOTER, "footer": FOOTER, "kind": "answer"}
        err = self.call("channel.send", send)["error"]
        self.assertEqual(err["code"], "not_ready")
        self.assertFalse(os.path.exists(os.path.join(self.data, "fake-posts.jsonl")))

        self.until("outbox woken after sign-in", lambda: any(
            e.get("event") == "connector.changed" and (e.get("params") or {}).get("scope") == "outbox"
            for e in self.events
        ) and self.status().get("account") == "示例账号")

        res = self.call("channel.send", send)
        self.assertEqual(res["result"], {"message_id": "m_sent_1"})

        self.call("connector.logout")
        err_after = self.call("channel.send", dict(send, ref="orestore2"))["error"]
        self.assertEqual(err_after["code"], "not_signed_in")

    def test_send_not_ready_while_loading_secrets_then_not_signed_in(self):
        """While secrets.load is in flight, send returns not_ready; once startup finishes without credentials, it returns not_signed_in."""
        self.secret_gate = threading.Event()
        self.call("initialize", {"protocol": 1, "extension": {"id": "larkx", "version": "0.4.23", "data_dir": self.data}, "config": {}})
        self.assertTrue(self.secret_loaded.wait(10))

        send = {"ref": "oload1", "chat": "c_dm", "text": "测试", "kind": "answer"}
        self.assertEqual(self.call("channel.send", send)["error"]["code"], "not_ready")
        self.assertNotEqual(self.status()["state"], "not_signed_in")

        self.secret_gate.set()
        self.until("outbox woken after startup", lambda: any(
            e.get("event") == "connector.changed" and (e.get("params") or {}).get("scope") == "outbox"
            for e in self.events
        ))
        self.assertEqual(self.status()["state"], "not_signed_in")
        self.assertEqual(self.call("channel.send", send)["error"]["code"], "not_signed_in")

if __name__ == "__main__":
    unittest.main()


class Read(unittest.TestCase):
    """Unread counts as Feishu shows them, and marking chats read."""

    def test_unread_of(self):
        self.assertEqual(lark.unread_of({"chatMode": 1, "lastMessagePositionBadgeCount": 50, "readPositionBadgeCount": 42}), 8)
        self.assertEqual(lark.unread_of({"chatMode": 1, "lastMessagePositionBadgeCount": 6, "readPositionBadgeCount": 6}), 0)
        topics = {"chatMode": 3, "lastThreadPositionBadgeCount": 120, "readThreadPositionBadgeCount": 107,
                  "lastMessagePositionBadgeCount": 9, "readPositionBadgeCount": 0}
        self.assertEqual(lark.unread_of(topics), 13, "a topic group counts new topics")
        self.assertEqual(lark.unread_of({}), 0)

    def feishu(self, chats):
        f = lark.Feishu(U, "feishu", lambda d: None)
        calls = []

        def api(name, data):
            calls.append((name, data))
            if name == "chats.PullChatsByIdsRequest":
                return {"chats": {k: v for k, v in chats.items() if k in data["chatIds"]}}
            if name == "chats.PullChatChattersRequest":
                return {"chatters": {"u_me": {"avatar": {"thumbnail": {"urls": ["https://cdn/me.png"]}}},
                                     "u_b": {"avatar": {"thumbnail": {"urls": ["http://plain/b.png", "https://cdn/b.png"]}}}}}
            if name == "messages.PullUnreadAtMessageIdsRequest":
                return {"messageIds": ["m1", "m2"]}
            if name == "messages.PullMessagesByIdsRequest":
                return {"messages": {m: {"chatId": {"m1": "2", "m2": "9"}[m]} for m in data["messageIds"]}}
            return {}
        f.client = types.SimpleNamespace(api=api)
        return f, calls

    def test_unread_and_mark(self):
        f, calls = self.feishu({
            "1": {"chatMode": 1, "lastMessagePosition": 30, "lastMessagePositionBadgeCount": 9, "readPositionBadgeCount": 4},
            "2": {"chatMode": 3, "lastThreadPositionBadgeCount": 2, "readThreadPositionBadgeCount": 0},
            "3": {"chatMode": 1, "lastMessagePositionBadgeCount": 1, "readPositionBadgeCount": 1},
        })
        self.assertEqual(f.unread(["1", "2", "3", "4", "doc:x"]),
                         {"1": {"unread": 5, "at_me": False}, "2": {"unread": 2, "at_me": True}, "3": {"unread": 0, "at_me": False}})
        self.assertEqual([c[1]["chatIds"] for c in calls if c[0] == "chats.PullChatsByIdsRequest"], [["1", "2", "3", "4"]], "one call for them all, documents left out")
        calls.clear()
        f.unread(["1"])
        self.assertNotIn("messages.PullMessagesByIdsRequest", [c[0] for c in calls], "a message's chat is looked up once")
        calls.clear()
        marked, failed = f.mark_read(["1", "2", "3", "4"])
        self.assertEqual(marked, ["1", "3"])
        self.assertEqual(set(failed), {"2", "4"}, "a topic group, and a chat Feishu doesn't have")
        self.assertEqual([c for c in calls if c[0] == "messages.PutReadMessagesRequest"],
                         [("messages.PutReadMessagesRequest", {"chatId": "1", "maxPosition": 30})], "only the unread one, up to its last message")

    def test_pictures(self):
        f, calls = self.feishu({
            "10": {"type": 2, "chatMode": 1, "avatar": {"key": "v3_abc", "urls": ["http://p4.pstatp.com/origin/v3_abc"]}},
            "11": {"type": 1, "chatMode": 1},
        })
        f.me = "u_me"
        got = f.unread(["10", "11"])
        self.assertTrue(got["10"]["avatar"].startswith("https://s1-imfile.feishucdn.com/static-resource/v1/v3_abc~"), "a group: its key on the CDN, never plain http")
        self.assertEqual(got["11"]["avatar"], "https://cdn/b.png", "a direct chat: the other person, https only")
        calls.clear()
        f.unread(["11"])
        self.assertNotIn("chats.PullChatChattersRequest", [c[0] for c in calls], "kept an hour")
        self.assertNotIn("name", got["10"], "no name known, none said")
        f.region = "lark"
        self.assertEqual(f.group_picture({"avatar": {"key": "v3_abc"}}), "", "Lark's CDN isn't known")
        self.assertEqual(lark.picture_url({"origin": {"urls": ["https://o"]}}), "https://o")

    def test_names_put_right(self):
        """A name given before (私聊（还不知道对方是谁）) is put right with the read state."""
        f, calls = self.feishu({"12": {"type": 1, "chatMode": 1}, "13": {"type": 1, "chatMode": 1}, "14": {"type": 2, "chatMode": 1, "name": "群"}})
        f.me, f.self_chat = "u_me", "13"
        f.user_name = lambda uid, chat: {"u_me": "陆文韬", "u_b": "乙"}.get(uid, "")
        got = f.unread(["12", "13", "14"])
        self.assertEqual((got["12"]["name"], got["13"]["name"], got["14"]["name"]), ("乙", "陆文韬（我）", "群"))
        f2, _ = self.feishu({str(20 + i): {"type": 1, "chatMode": 1} for i in range(lark.DM_LOOKUPS_A_SYNC + 2)})
        f2.me = "u_me"
        named = [k for k, v in f2.unread([str(20 + i) for i in range(lark.DM_LOOKUPS_A_SYNC + 2)]).items() if "name" in v]
        self.assertEqual(len(named), lark.DM_LOOKUPS_A_SYNC, "a few direct chats looked up an answer, the rest the next time")
