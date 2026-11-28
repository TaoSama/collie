"""Verify native inbox operations with the pinned Web Gateway message types."""

import os
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "pkg", "adapter"))
UPSTREAM = os.environ.get("LARKX_UPSTREAM", "")
if not UPSTREAM:
    raise unittest.SkipTest("LARKX_UPSTREAM not set")

import lark  # noqa: E402
import lark_inbox  # noqa: E402
import main  # noqa: E402
import peer  # noqa: E402


class NativeInbox(unittest.TestCase):
    def setUp(self):
        work = tempfile.TemporaryDirectory()
        self.addCleanup(work.cleanup)
        self.U = lark.load_upstream(UPSTREAM, work.name, "feishu")
        self.adapter = lark.Feishu(self.U, "feishu", lambda _: None)
        self.adapter.client = self
        self.adapter.me = "12"
        self.adapter.user_name = lambda uid, chat: {"12": "我", "19": "小张"}.get(uid, "")
        self.adapter.user_pictures = lambda chat, uids: {}
        self.adapter.emojis = lambda: {"THUMBSUP": ("赞", "https://example.com/like")}
        self.inbox = self.adapter.native_inbox
        self.calls = []
        self.posts = []
        self.status = 0
        self.forward_receipt = None

    def api(self, method, params):
        self.calls.append((method, params))
        if method == "chats.PullChatsByIdsRequest":
            return {"chats": {chat: {"chatMode": 3 if chat == "32" else 1} for chat in params["chatIds"]}}
        if method == "messages.PullMessagesByIdsRequest":
            mid = params["messageIds"][0]
            return {"messages": {mid: {"id": mid, "chatId": "31" if mid in ("90", "96") else "32", "chatType": 2,
                                       "type": 4, "rootId": "91" if mid == "90" else "0"}}}
        if method == "thread.PullThreadsByIDsRequest":
            tid = params["threadIds"][0]
            return {"threads": {tid: {"id": tid, "rootMessageId": "90", "channelId": "31",
                                      "replyCount": 2, "lastMessagePosition": 2}},
                    "rootMessages": {"90": self.raw_message("90", chat="31")},
                    "settings": {tid: {"threadId": tid, "isFollowV2": True}}}
        if method == "thread.PullThreadMessagesByPositionsRequest":
            return {"messages": {
                "1": self.raw_message("93", chat="31", thread="92", root="90"),
                "2": self.raw_message("94", chat="32", thread="92", root="90"),
            }}
        if method == "reactions.PullUserMRUReactionsRequest":
            return {"userMruReactions": ["THUMBSUP"]}
        if method == "chats.PullChatChattersByIDsRequest":
            return {"chatters": {str(uid): {"localizedName": "成员 " + str(uid)} for uid in params["chatterIds"]}}
        if method == "reactions.PullReactionsRequest":
            return {"messageReactions": [
                {"messageId": "90", "reactions": [
                    {"type": "THUMBSUP", "count": 2, "userIds": ["12"], "chatterIds": ["12", "19"]},
                    {"type": "HEART", "count": 5, "chatterIds": ["19"]},
                    {"type": "SMILE", "count": 2, "chatterIds": ["19", "20"]},
                ]},
                {"messageId": "92", "reactions": [{"type": "HEART", "count": 6}]},
            ], "selfReactionStatus": [
                {"userId": "12", "messageId": "90", "reactionType": "THUMBSUP", "isCancelled": True},
                {"userId": "12", "messageId": "90", "reactionType": "HEART"},
            ]}
        raise AssertionError(method)

    def _gateway_post(self, pkt):
        calls = {
            5: ("messages.PutMessageRequest", dict(message=dict(id="95"))),
            25: ("reactions.PutReactionRequest", dict(count=2)),
            26: ("reactions.DeleteReactionRequest", dict(count=1)),
            51: ("messages.PutTransmitMessageRequest", dict(messages={"32": dict(id="95", chatId="32")})),
            91: ("thread.PatchThreadSettingRequest", {}),
            8125: ("thread.PutThreadShareRequest", dict(messages={"32": dict(id="95", chatId="32")})),
        }
        method, body = calls[pkt.cmd]
        if pkt.cmd == 51 and self.forward_receipt is not None:
            body = self.forward_receipt
        _, req_type, resp_type = self.U.gateway.resolve_cmd(method)
        self.posts.append(req_type.FromString(pkt.payload))
        resp = self.U.gateway.request_from_dict(resp_type, body)
        return self.U.P.Packet(cmd=pkt.cmd, status=self.status, payload=resp.SerializeToString()).SerializeToString()

    @staticmethod
    def is_thread_chat(chat):
        return chat == "32"

    def raw_message(self, mid, chat, **metadata):
        raw = {"id": mid, "chatId": chat, "type": 4,
               "content": self.U.L.entities.TextContent(text="测试话题").SerializeToString()}
        if "root" in metadata:
            raw["rootId"] = metadata["root"]
        if "thread" in metadata:
            raw["threadId"] = metadata["thread"]
        return raw

    def action(self, action, **params):
        return self.inbox.action({"chat_id": "31", "action": action, "message_id": "90", **params})

    def test_batch_reactions_and_self_status(self):
        messages = [{"id": "90"}, {"id": "91", "view": {"kind": "recalled"}}]
        self.inbox.enrich_reactions("31", messages)
        self.assertEqual(messages[0]["reactions"], [
            {"key": "THUMBSUP", "count": 2, "mine": False},
            {"key": "HEART", "count": 5, "mine": True},
            {"key": "SMILE", "count": 2, "mine": False, "names": ["成员 19", "成员 20"]},
        ])
        self.assertNotIn("reactions", messages[1])
        self.assertEqual(self.calls[0][1]["messageIds"], ["90"])
        self.assertEqual(self.calls[1][0], "chats.PullChatChattersByIDsRequest")
        self.assertEqual(self.calls[1][1]["chatterIds"], [19, 20])

    def test_inbox_reads_expire_and_writes_invalidate(self):
        def calls(method):
            return sum(name == method for name, _ in self.calls)

        reactions = "reactions.PullReactionsRequest"
        comments = "thread.PullThreadMessagesByPositionsRequest"
        with patch.object(lark_inbox.time, "monotonic", return_value=100) as clock:
            for _ in range(2):
                self.inbox.enrich_reactions("31", [{"id": "90"}])
                self.action("topic", thread_id="92")
            self.assertEqual((calls(reactions), calls(comments)), (2, 1))
            self.action("reaction", key="THUMBSUP", active=True)
            self.inbox.enrich_reactions("31", [{"id": "90"}])
            self.action("topic", thread_id="92")
            self.assertEqual((calls(reactions), calls(comments)), (4, 2))
            clock.return_value += lark_inbox.INBOX_READ_TTL
            self.inbox.enrich_reactions("31", [{"id": "90"}])
            self.action("topic", thread_id="92")
            self.assertEqual((calls(reactions), calls(comments)), (6, 3))

    def test_reaction_success_and_refused_gateway_packet(self):
        self.action("reaction", key="THUMBSUP", active=True)
        self.action("reaction", key="THUMBSUP", active=False)
        self.assertEqual([r.DESCRIPTOR.name for r in self.posts], ["PutReactionRequest", "DeleteReactionRequest"])
        self.assertTrue(all(r.messageId == "90" and r.reactionType == "THUMBSUP" for r in self.posts))
        self.status = 403
        with self.assertRaisesRegex(ValueError, "403"):
            self.action("reaction", key="THUMBSUP", active=False)

    def test_native_forward_and_follow(self):
        self.action("forward", target="32")
        self.action("forward", thread_id="92", target="32")
        self.action("subscribe", thread_id="92", active=True)
        self.action("subscribe", thread_id="92", active=False)
        send, share, follow, unfollow = self.posts
        self.assertEqual((send.messageId, list(send.chatIds), send.immutableMessage.chatId), ("90", ["32"], 31))
        self.assertIn(send.cids["32"], self.adapter.own_cids)
        self.assertEqual((share.channelId, share.threadId, share.targets[0].toChat.chatId), ("31", "92", "32"))
        self.assertEqual(list(follow.updateFields), [follow.UPDATE_FIELD_IS_FOLLOW])
        self.assertEqual(follow.setting.threadId, "92")
        self.assertTrue(follow.setting.isFollowV2)
        self.assertFalse(unfollow.setting.isFollowV2)
        self.assertTrue(unfollow.setting.HasField("isFollowV2"))

    def test_forward_uncertainty_reaches_extension_caller(self):
        from requests.exceptions import ReadTimeout

        extension = main.Extension()
        extension.backend, extension.account = self.adapter, "用户"
        params = {"chat_id": "31", "action": "forward", "message_id": "90", "target": "32"}
        method = "connector.inbox.lark_action"
        for error in (None, ReadTimeout()):
            with self.subTest(error=error), patch.object(self, "_gateway_post", return_value=b"", side_effect=error):
                with self.assertRaises(peer.Error) as result:
                    extension.handle(method, params)
                self.assertEqual(result.exception.code, "uncertain")
                self.assertEqual(str(result.exception), "没有收到转发回执，消息可能已经转发，请到飞书确认")

        self.status = 403
        with self.assertRaises(peer.Error) as result:
            extension.handle(method, params)
        self.assertNotEqual(result.exception.code, "uncertain")
        self.assertIn("403", str(result.exception))

    def test_forward_receipt_in_topic_group(self):
        info = {"chatId": "32", "message": {"id": "95", "chatId": "32"}}
        self.forward_receipt = {"threadMessages": {"32": info}}
        self.assertEqual(self.action("forward", target="32"), {})
        info["failed"] = True
        with self.assertRaisesRegex(ValueError, "拒绝"):
            self.action("forward", target="32")

        self.forward_receipt = {}
        with self.assertRaises(peer.Error) as result:
            self.action("forward", target="32")
        self.assertEqual(result.exception.code, "uncertain")

    def test_offline_inbox_returns_sign_in_error(self):
        extension = main.Extension()
        extension.backend, extension.account = self.adapter, "用户"
        self.adapter.client = None
        calls = [
            lambda: extension.handle("connector.inbox.recent", {"chat_id": "31"}),
            lambda: extension.handle("connector.inbox.lark_action", {"chat_id": "31", "action": "topic"}),
            lambda: self.inbox.reaction("90", "THUMBSUP", True),
        ]
        for call in calls:
            with self.assertRaises(peer.Error) as result:
                call()
            self.assertEqual(result.exception.code, "not_signed_in")
            self.assertIn("飞书尚未登录", str(result.exception))

    def test_emoji_key_accepts_tenant_keys_without_whitespace(self):
        self.action("reaction", key="tenant.custom-😀", active=True)
        self.assertEqual(self.posts[0].reactionType, "tenant.custom-😀")
        for key in ("", "a" * 257, "a b", "a\x80b", "a\u00a0b", "a\ud800"):
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "表情"):
                self.action("reaction", key=key, active=True)
        self.assertEqual(len(self.posts), 1)

    def test_foreign_message_and_topic_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "不属于"):
            self.inbox.action(dict(chat_id="32", message_id="90", action="reaction", key="THUMBSUP", active=True))
        with self.assertRaisesRegex(ValueError, "不属于"):
            self.inbox.action(dict(chat_id="32", thread_id="92", action="topic"))
        with self.assertRaisesRegex(ValueError, "不一致"):
            self.inbox.action(dict(chat_id="31", thread_id="92", message_id="95", action="subscribe", active=True))
        self.assertEqual(self.posts, [])

    def test_topic_comments_preserve_root_and_exclude_other_chats(self):
        rows = self.action("topic", thread_id="92")["messages"]
        self.assertEqual([r["id"] for r in rows], ["90", "93"])
        self.assertTrue(all(r["thread_id"] == "92" and r["root_id"] == "90" for r in rows))
        self.assertEqual(rows[0]["reply_count"], 2)
        self.assertTrue(rows[0]["subscribed"])

    def test_explicit_send_modes_keep_legacy_thread_reply(self):
        self.adapter._remember("90", "91", "父消息")
        self.adapter.post("31", "测试内容", reply_to="90")
        self.adapter.post("31", "测试内容", reply_to="90", reply_mode="quote")
        self.adapter.post("31", "测试内容", reply_to="96", reply_mode="create_topic")
        self.adapter.post("31", "测试内容", reply_to="90", reply_mode="comment")
        self.adapter.post("32", "测试内容", reply_mode="publish")
        self.assertEqual([(r.rootId, r.parentId, r.isReplyInThread, r.isThreadGroupTopic) for r in self.posts], [
            ("91", "91", True, False), ("91", "90", False, False), ("96", "96", True, False),
            ("91", "90", True, False), ("", "", False, True),
        ])
        with self.assertRaises(lark.NotSent):
            self.adapter.post("31", "测试内容", reply_mode="publish")

    def test_normal_group_topics_preserve_native_id_without_guessing_standalone(self):
        root = self.raw_message("90", chat="31", thread="92")
        root["threadMessageType"] = self.U.L.entities.Message.THREAD_ROOT_MESSAGE
        standalone = self.raw_message("96", chat="31", thread="96")
        rows = self.adapter.history.render_rows("31", [root, standalone])
        self.assertEqual([(r["id"], r.get("thread_id"), r.get("root_id")) for r in rows],
                         [("90", "92", "90"), ("96", None, None)])
        with self.assertRaisesRegex(lark.NotSent, "已属于话题"):
            self.adapter.post("31", "测试内容", reply_to="90", reply_mode="create_topic")
