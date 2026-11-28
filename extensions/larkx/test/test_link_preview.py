"""Preview caching and delivery with a local stand-in for the Feishu gateway."""
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
from unittest.mock import patch

from test_adapter import U, lark, richtext
import link_preview
import message_view

URL = "https://example.feishu.cn/docx/sample"
OTHER = "https://example.feishu.cn/docx/other"
CHAT = "123"


def wire_message(url, mid):
    body = U.L.entities.TextContent(richText=richtext.build(U.L, url)).SerializeToString()
    return {"id": mid, "chatId": CHAT, "type": 4, "fromId": "5", "chatType": 2,
            "content": body, "createTime": 1795700000}


class Client:
    def __init__(self, title, raw, blocked_url=None):
        self.title = title
        self.raw = raw
        self.blocked_url = blocked_url
        self.started, self.release = threading.Event(), threading.Event()
        self.lookups = []

    def create_chat(self, uid):
        return "456"

    def get_chat_info(self, chat):
        return {}

    def api(self, method, params):
        if method == "chats.PullChatsByIdsRequest":
            return {"chats": {CHAT: {"lastVisibleMessagePosition": 0}}}
        if method == "messages.PullMessagesByPositionsRequest":
            return {"messages": {"0": self.raw}}
        if method == "docs.PullDocByURLsRequest":
            urls = params["urls"]
            self.lookups.extend(urls)
            if self.blocked_url in urls:
                self.started.set()
                self.release.wait(5)
            return {"docs": {u: {"name": self.title, "docType": 8} for u in urls}}
        return {}


def sign_in(adapter, client, uid):
    with patch.object(U.client, "LarkClient", return_value=client):
        adapter.restore({"cookies": {"session": "fixture"}, "region": "feishu", "user_id": uid})


def wait_for_previews(inbox):
    inbox.previews._pool.submit(lambda: None).result(timeout=3)


class LinkPreviewTest(unittest.TestCase):
    def setUp(self):
        self.feishu = lark.Feishu(U, "feishu", lambda _: None)
        self.feishu.user_name = lambda *_: "示例用户"
        self.addCleanup(self.feishu.logout)

    def test_logout_splits_warm_and_inflight_previews_from_new_account(self):
        raw = wire_message(URL, "200")
        old = Client("旧账号标题", raw, blocked_url=OTHER)
        new = Client("新账号标题", raw)
        sign_in(self.feishu, old, "1")
        old_inbox = self.feishu.history
        old_pool = old_inbox.previews._pool
        old_inbox.recent(CHAT)
        wait_for_previews(old_inbox)
        self.assertIn("旧账号标题", old_inbox.recent(CHAT)[0]["preview"])

        pushed = message_view.decode(U, wire_message(OTHER, "201"))
        self.feishu._deliver(pushed, lambda _: None)
        self.assertTrue(old.started.wait(3))

        try:
            self.feishu.logout()
            sign_in(self.feishu, new, "2")
            inbox = self.feishu.history
            first = inbox.recent(CHAT)[0]
            self.assertNotIn("旧账号标题", first["preview"])
            self.assertNotIn("旧账号标题", str(first["view"]))
            wait_for_previews(inbox)
            self.assertEqual(new.lookups, [URL])
            self.assertIn("新账号标题", inbox.recent(CHAT)[0]["preview"])
        finally:
            old.release.set()
            old_pool.shutdown(wait=True)

        # The old request completed after re-login. This login still looks up that URL itself.
        self.feishu._deliver(pushed, lambda _: None)
        wait_for_previews(self.feishu.history)
        self.assertEqual(new.lookups, [URL, OTHER])
        self.assertEqual(old_inbox.recent(CHAT), [])
        entry = {"text": OTHER}
        old_inbox.previews.enrich([(pushed, entry)])
        self.assertEqual(entry, {"text": OTHER})

    def test_slow_previews_leave_live_delivery_and_history_available(self):
        live = wire_message(URL, "200")
        history = wire_message(OTHER, "201")
        client = Client("补齐的标题", history, blocked_url=URL)
        sign_in(self.feishu, client, "1")
        results = []

        with ThreadPoolExecutor(max_workers=2) as pool:
            try:
                push = pool.submit(self.feishu._deliver, message_view.decode(U, live), results.append)
                self.assertTrue(client.started.wait(3))
                push.result(timeout=2)
                self.assertEqual(len(results), 1)
                self.assertIn(URL, results[0]["text"])

                recent = pool.submit(self.feishu.history.recent, CHAT).result(timeout=2)
                self.assertEqual(len(recent), 1)
                self.assertIn(OTHER, recent[0]["text"])
                self.assertNotIn("补齐的标题", recent[0]["preview"])

                self.feishu.history.recent(CHAT)
                self.assertEqual(client.lookups, [URL])
            finally:
                client.release.set()

        wait_for_previews(self.feishu.history)
        self.assertEqual(client.lookups, [URL, OTHER])
        self.assertIn("补齐的标题", self.feishu.history.recent(CHAT)[0]["preview"])
        self.assertIn(OTHER, self.feishu.history.recent(CHAT)[0]["text"])

    def test_preview_misses_cool_down_across_history_polls(self):
        for response in ({}, OSError("gateway unavailable")):
            with self.subTest(response=response):
                client = Client("补齐的标题", wire_message(URL, "200"))
                api = client.api
                lookups = []

                def preview_api(method, params):
                    if method == "docs.PullDocByURLsRequest":
                        lookups.extend(params["urls"])
                        if len(lookups) == 1:
                            if isinstance(response, Exception):
                                raise response
                            return response
                    return api(method, params)

                client.api = preview_api
                sign_in(self.feishu, client, "1")
                inbox = self.feishu.history
                now = 0
                with patch.object(link_preview, "time", SimpleNamespace(monotonic=lambda: now)):
                    first = inbox.recent(CHAT)
                    wait_for_previews(inbox)
                    for now in (4, 8, 12):
                        self.assertEqual(inbox.recent(CHAT), first)
                        wait_for_previews(inbox)
                        self.assertEqual(lookups, [URL])

                    now = 31
                    inbox.recent(CHAT)
                    wait_for_previews(inbox)
                    self.assertEqual(lookups, [URL, URL])
                    self.assertIn("补齐的标题", inbox.recent(CHAT)[0]["preview"])
