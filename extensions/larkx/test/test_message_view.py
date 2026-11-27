"""Rich inbox behavior with anonymized card/calendar JSON and real message protobufs."""
import json
import unittest

from test_adapter import E, U, lark, protobuf_to_dict, richtext
import inbox_example
import message_view


class MessageViewTest(unittest.TestCase):
    def test_card_body_and_preview(self):
        card = U.L.entities.CardContent(jsonCard=json.dumps(inbox_example.card()))
        wire = {"id": "1003", "type": 14, "content": card.SerializeToString(), "createTime": 1795764600}
        msg = message_view.decode(U, wire, "123")
        view, preview = message_view.describe(U, msg)
        self.assertEqual(preview, "[卡片] 数据库告警已恢复")
        self.assertEqual(view["subtitle"], "生产环境 · API 服务")
        md, columns, fields, rule, actions = view["nodes"]
        self.assertEqual((md["kind"], columns["kind"], fields["kind"], rule["kind"]), ("markdown", "columns", "fields", "rule"))
        self.assertEqual(fields["children"][0]["tone"], "short")
        self.assertEqual(actions["children"][0]["url"], "https://example.com/monitor")
        self.assertNotIn("url", actions["children"][1])

    def test_approval_card_uses_body_if_title_is_missing(self):
        card = {"elements": [
            {"tag": "div", "text": {"tag": "lark_md", "content": "**请假审批**\n申请人：示例员工"}},
            {"tag": "img", "img_key": "img_v3_example", "alt": {"content": "附件截图"}},
        ]}
        view, preview = message_view.describe(U, {"msg_type": 14, "content_data": {"openCardContent": json.dumps(card).encode()}})
        self.assertIn("请假审批", preview)
        self.assertNotIn("**", preview)
        self.assertEqual(view["nodes"][1], {"kind": "image", "key": "img_v3_example", "text": "附件截图"})

    def test_rich_text_marks_links_and_media(self):
        rt = richtext.build(U.L, "**处理结果**\n[记录](https://example.com/ticket)")
        rt.elementIds.append("pic")
        rt.elements.dictionary["pic"].tag = E.IMG
        rt.elements.dictionary["pic"].property = E.ImageProperty(token="img_v3_test").SerializeToString()
        view, preview = message_view.describe(U, {"msg_type": 2, "content_data": {"richText": protobuf_to_dict(rt)}})
        self.assertIn("处理结果", preview)
        self.assertIn("记录", preview)
        self.assertEqual(view["nodes"][0]["children"][0]["tone"], "bold")
        self.assertEqual(view["nodes"][1]["children"][0]["url"], "https://example.com/ticket")
        self.assertEqual(view["nodes"][2]["key"], "img_v3_test")

    def test_legacy_calendar_json_and_system_notice(self):
        messages = list(inbox_example.messages(U))
        cal = messages[-1]
        self.assertEqual(cal["preview"], "[日历] 发布复盘 · 11-27 15:00")
        self.assertEqual(cal["view"]["nodes"][0]["text"], "线上会议")
        self.assertEqual(messages[0]["view"]["kind"], "system")
        self.assertEqual(messages[0]["preview"], "值班同学加入了会话")
        self.assertEqual(messages[2]["preview"], "[文件] 复盘摘要.txt")

    def test_general_calendar_protobuf_and_read_only_history(self):
        event = U.L.entities.GeneralCalendarContent()
        event.eventCard.summary = "团队周会"
        event.eventCard.startTime = 1795764600
        event.eventCard.startTimezone = "Asia/Shanghai"
        body = event.SerializeToString()

        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 23}}}
                return {"messages": {"23": {"id": "85", "chatId": "123", "type": 18, "content": body, "createTime": 1795764600}}}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        rows = f.history.recent("123")
        self.assertEqual(f.client.calls[1][1]["positions"], list(range(12, 24)))
        self.assertEqual(rows[0]["view"]["title"], "团队周会")
        self.assertEqual(f.history.recent("123"), rows)
        self.assertEqual(len(f.client.calls), 2)
        f.history.invalidate("123")
        f.history.recent("123")
        self.assertEqual(len(f.client.calls), 4)


    def test_first_message_is_at_position_0(self):
        """A chat whose only message is a bot's card has it at position 0: the window reads it."""
        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastMessagePosition": 0, "lastVisibleMessagePosition": 0}}}
                return {"messages": {str(p): {"id": "m%d" % p, "chatId": "123", "type": 4,
                                              "content": U.L.entities.TextContent(richText=richtext.build(U.L, "通知")).SerializeToString(),
                                              "createTime": 1795700000} for p in params["positions"]}}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        self.assertEqual([r["id"] for r in f.history.recent("123")], ["m0"])
        self.assertEqual(f.client.calls[1][1]["positions"], [0])

    def test_topic_reply_joins_the_window(self):
        """A reply inside a topic isn't at a position of the chat: the latest one pushed still shows, in time order."""
        class Client:
            def api(self, method, params):
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 1}}}
                return {"messages": {str(p): {"id": "m%d" % p, "chatId": "123", "type": 4, "fromId": "u_a",
                                              "content": U.L.entities.TextContent(richText=richtext.build(U.L, "主消息")).SerializeToString(),
                                              "createTime": 1795700000 + p} for p in params["positions"]}}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client, f.me = Client(), "u_me"
        f.user_name = lambda uid, chat: "甲"
        f.chat_info = lambda chat: {"name": "群"}
        f.folded = lambda chat: None
        f.group_picture = lambda info: ""
        reply = {"msg_id": "r1", "chat_id": "123", "from_id": "u_b", "msg_type": 4, "chat_type": 2, "position": -3,
                 "root_id": "m1", "create_time": 1795710000, "content_data": {"richText": protobuf_to_dict(richtext.build(U.L, "话题里的回复"))}}
        f.normalize(reply)
        rows = f.history.recent("123")
        self.assertEqual([r["id"] for r in rows], ["m0", "m1", "r1"], "the topic reply is the latest")
        self.assertEqual(rows[-1]["text"], "话题里的回复")
        f.normalize(dict(reply, msg_id="m3", position=3, create_time=1795720000))
        self.assertEqual([r["id"] for r in f.history.recent("123")], ["m0", "m1", "r1"], "a message of the chat's own stream comes from its positions")

    def test_senders_have_their_pictures(self):
        """Each sender's avatar comes with the window, the people looked up once in one call."""
        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 1}}}
                if method == "chats.PullChatChattersByIDsRequest":
                    return {"chatters": {7: {"avatar": {"thumbnail": {"urls": ["https://cdn.example/7"]}}}}}
                return {"messages": {str(p): {"id": "m%d" % p, "chatId": "123", "type": 4, "fromId": str(5 + 2 * p),
                                              "content": U.L.entities.TextContent(richText=richtext.build(U.L, "你好")).SerializeToString(),
                                              "createTime": 1795700000 + p} for p in params["positions"]}}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        f.user_name = lambda uid, chat: "甲"
        rows = f.history.recent("123")
        self.assertEqual([r.get("sender_avatar") for r in rows], [None, "https://cdn.example/7"])
        self.assertEqual(f.client.calls[-1][1]["chatterIds"], [5, 7])
        f.history.invalidate("123")
        f.history.recent("123")
        self.assertNotEqual(f.client.calls[-1][0], "chats.PullChatChattersByIDsRequest", "kept an hour")

    def test_inline_emojis_and_stickers(self):
        """Inline emojis resolve to CDN URLs and Chinese labels; stickers become media image nodes."""
        rt = U.L.entities.RichText()
        rt.elementIds.extend(["1", "2", "3", "4"])
        rt.elements.dictionary["1"].tag = E.TEXT
        rt.elements.dictionary["1"].property = E.TextProperty(content="难哦 ").SerializeToString()
        rt.elements.dictionary["2"].tag = E.EMOTION
        rt.elements.dictionary["2"].property = E.EmotionProperty(key="Lark_Emoji_Sob_0").SerializeToString()
        rt.elements.dictionary["3"].tag = E.EMOTION
        rt.elements.dictionary["3"].property = E.EmotionProperty(key="custom_1").SerializeToString()
        rt.elements.dictionary["4"].tag = E.EMOTION
        rt.elements.dictionary["4"].property = E.EmotionProperty(key="UNKNOWN_KEY").SerializeToString()
        text_body = U.L.entities.TextContent(richText=rt).SerializeToString()
        sticker_body = U.L.entities.StickerContent(key="v3_00ng_sticker_key", stickerId="42").SerializeToString()

        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 1}}}
                if method == "reactions.PullEmojiResourcesV2Request":
                    return {"emojiResourceSet": [{"emojiResources": [
                        {"key": "Lark_Emoji_Sob_0", "i18nText": {"i18nTexts": {"zh-CN": "流泪"}},
                         "imageKey": "emoji_sob.png"},
                        {"key": "custom_1", "i18nText": {"i18nTexts": {"zh-CN": "勇攀高峰"}},
                         "i18nImages": {"i18nImage": {"zh-CN": {"key": "v3_00ud_custom"}}}},
                    ]}]}
                if method == "chats.PullChatChattersByIDsRequest":
                    return {"chatters": {}}
                return {"messages": {
                    "0": {"id": "m0", "chatId": "123", "type": 4, "fromId": "5", "content": text_body, "createTime": 1795700000},
                    "1": {"id": "m1", "chatId": "123", "type": 10, "fromId": "5", "content": sticker_body, "createTime": 1795700001},
                }}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        f.user_name = lambda uid, chat: "甲"
        rows = f.history.recent("123")
        self.assertEqual(rows[0]["text"], "难哦 [流泪][勇攀高峰][表情]")
        self.assertEqual(rows[0]["preview"], "难哦 [流泪][勇攀高峰][表情]")
        self.assertEqual(rows[0]["view"]["kind"], "post")
        para = rows[0]["view"]["nodes"][0]
        self.assertEqual(para["kind"], "paragraph")
        self.assertEqual(para["children"], [
            {"kind": "text", "text": "难哦 "},
            {"kind": "emoji", "text": "流泪", "url": "https://sf3-cn.feishucdn.com/obj/lark-reaction-cn/emoji_sob.png"},
            {"kind": "emoji", "text": "勇攀高峰", "url": "https://s1-imfile.feishucdn.com/static-resource/v1/v3_00ud_custom"},
            {"kind": "emoji", "text": "表情"},
        ])
        self.assertEqual(rows[1]["preview"], "[表情包]")
        self.assertEqual(rows[1]["view"], {
            "kind": "media",
            "nodes": [{"kind": "image", "text": "表情包", "key": "v3_00ng_sticker_key", "tone": "sticker"}],
        })
        emoji_calls = [c for c in f.client.calls if c[0] == "reactions.PullEmojiResourcesV2Request"]
        self.assertEqual(len(emoji_calls), 1, "emoji map is cached across lookups")


if __name__ == "__main__":
    unittest.main()
