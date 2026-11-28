"""Rich inbox behavior with anonymized card/calendar JSON and real message protobufs."""
import json
import unittest
from unittest.mock import patch

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

    def test_card_v2_history_requests_supported_command_version(self):
        with patch("requests.post") as post:
            for cmd in (8, 43, 8108, 5):
                with self.subTest(cmd=cmd):
                    headers = {"x-command": str(cmd), "x-command-version": "5.7.0", "x-web-version": "3.9.32"}
                    U.client.requests.post("https://example.com/im/gateway/", headers=headers, timeout=15)
                    self.assertEqual(post.call_args.kwargs["headers"], {
                        **headers, "x-command-version": "5.7.0" if cmd == 5 else "7.20.0",
                    })

    def test_packed_card_v2_uses_new_body(self):
        def packed(tag, **props):
            return {"tag": tag, "property": props}

        card = {
            "schema": "2.0",
            "header": packed("card_header", title=packed("plain_text", content="巡检")),
            "body": {"elements": [
                packed("img", imageID="24"),
                packed("markdown", elements=[
                    packed("plain_text", content="Upgrade to the latest app version to view the content"),
                ]),
            ]},
            "newBody": packed("body", elements=[
                packed("column_set", columns=[packed("column", elements=[
                    packed("markdown", elements=[
                        packed("plain_text", content="检查结果", textStyle={"attributes": ["bold"]}),
                        packed("br"),
                        packed("plain_text", content="任务已完成"),
                    ]),
                ])]),
            ]),
        }
        wire = U.L.entities.CardContent(jsonCard=json.dumps(card)).SerializeToString()
        view, preview = message_view.describe(U, message_view.decode(U, {"type": 14, "content": wire}))
        self.assertEqual(preview, "[卡片] 巡检")
        self.assertEqual(view["nodes"], [message_view.node("columns", children=[
            message_view.node("group", children=[
                message_view.node("paragraph", children=[message_view.node("text", "检查结果", tone="bold")]),
                message_view.node("paragraph", children=[message_view.node("text", "任务已完成")]),
            ]),
        ])])

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


    def test_recalled_messages_in_history(self):
        message = U.L.entities.Message
        body = U.L.entities.TextContent(text="编辑后的消息").SerializeToString()

        class Client:
            def api(self, method, params):
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 3}}}
                return {"messages": {
                    "0": {"id": "1", "type": 4, "status": message.MODIFIED, "content": body, "createTime": 1795700000},
                    "1": {"id": "2", "type": 4, "status": message.DELETED, "createTime": 1795700001},
                    "2": {"id": "3", "type": 2, "status": message.DELETED, "createTime": 1795700002},
                    "3": {"id": "4", "type": 14, "status": message.DELETED, "createTime": 1795700003},
                }}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        edited, *recalled = f.history.recent("123")
        self.assertEqual(edited["text"], "编辑后的消息")
        self.assertIsNone(edited["view"])
        for row in recalled:
            with self.subTest(id=row["id"]):
                self.assertEqual(row["text"], "撤回了一条消息")
                self.assertEqual(row["preview"], row["text"])
                self.assertEqual(row["view"], {"kind": "recalled", "nodes": [{"kind": "text", "text": row["text"]}]})

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

    def test_topic_group_reads_threads_and_replies(self):
        """A topic group (chatMode 3) has no chat message positions: its window pulls recent topics and replies."""
        def text_wire(mid, text, at, root=""):
            w = {"id": mid, "chatId": "750", "type": 4, "fromId": "u1",
                 "content": U.L.entities.TextContent(richText=richtext.build(U.L, text)).SerializeToString(),
                 "createTime": at}
            if root:
                w["rootId"] = root
            return w

        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"750": {"chatMode": 3, "lastVisibleThreadPosition": 757, "lastThreadPosition": 757}}}
                if method == "thread.PullThreadsByPositionsRequest":
                    root = text_wire("101", "话题根消息", 1795795757)
                    r1 = text_wire("102", "第一条回复", 1795797024, root="101")
                    r2 = text_wire("103", "最后一条回复", 1795797174, root="101")
                    return {
                        "rootMessages": {"101": root},
                        "lastReplyMessages": {"101": r2},
                        "threadMessages": {"101": {"rootMessage": root, "lastMessages": [r1]}},
                    }
                if method == "chats.PullChatChattersByIDsRequest":
                    return {"chatters": {}}
                raise AssertionError("unexpected call: %s" % method)

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client, f.me = Client(), "u_me"
        f.user_name = lambda uid, chat: "甲"
        rows = f.history.recent("750")
        self.assertEqual([r["id"] for r in rows], ["101", "102", "103"])
        self.assertEqual([r["text"] for r in rows], ["话题根消息", "第一条回复", "最后一条回复"])
        pull = [c for c in f.client.calls if c[0] == "thread.PullThreadsByPositionsRequest"][0][1]
        self.assertEqual((pull["channelId"], pull["type"], pull["positions"]), ("750", 1, list(range(746, 758))))

    def test_topic_group_without_threads_falls_back_to_chat_messages(self):
        """A brand-new topic group with no topics still reads chat position 0."""
        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"751": {"chatMode": 3, "lastThreadPosition": -1}}}
                if method == "messages.PullMessagesByPositionsRequest":
                    return {"messages": {"0": {"id": "1", "chatId": "751", "type": 6, "createTime": 1795700000, "content": b""}}}
                return {}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client = Client()
        rows = f.history.recent("751")
        self.assertEqual([r["id"] for r in rows], ["1"])
        self.assertNotIn("thread.PullThreadsByPositionsRequest", [c[0] for c in f.client.calls])

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

    def test_richtext_card_buttons_select_overflow_picker_and_action_exec(self):
        """RichText cards expose POST callbacks, OPEN_URL buttons, select menus, pickers, and execute actionv2."""
        from unittest import mock
        import actions

        card = U.L.entities.CardContent(type=2, cardVersion=2)
        rt = card.richtext
        rt.elementIds.extend(["p1", "sel1", "sel2", "ov1", "dp1"])
        p1 = rt.elements.dictionary["p1"]
        p1.tag = E.P
        p1.childIds.extend(["b1", "b2"])
        b1 = rt.elements.dictionary["b1"]
        b1.tag = E.BUTTON
        b1.property = E.ButtonProperty(actionId="act_ok", mode="primary").SerializeToString()
        b1.childIds.append("t1")
        rt.elements.dictionary["t1"].tag = E.TEXT
        rt.elements.dictionary["t1"].property = E.TextProperty(content="Approve").SerializeToString()
        b2 = rt.elements.dictionary["b2"]
        b2.tag = E.BUTTON
        b2.property = E.ButtonProperty(actionId="act_url", mode="default").SerializeToString()
        b2.childIds.append("t2")
        rt.elements.dictionary["t2"].tag = E.TEXT
        rt.elements.dictionary["t2"].property = E.TextProperty(content="View Doc").SerializeToString()

        sel1 = rt.elements.dictionary["sel1"]
        sel1.tag = E.SELECTMENU
        sp1 = E.SelectMenuProperty(type=1, actionId="act_mute", placeHolder="Ack and Mute")
        sp1.options.add(text="Mute 30m", value="30")
        sel1.property = sp1.SerializeToString()

        sel2 = rt.elements.dictionary["sel2"]
        sel2.tag = E.SELECTMENU
        sel2.property = E.SelectMenuProperty(type=2, actionId="act_person", placeHolder="选人").SerializeToString()

        ov1 = rt.elements.dictionary["ov1"]
        ov1.tag = E.OVERFLOWMENU
        op = E.OverflowMenuProperty(actionId="act_ov")
        op.options.add(text="更多", value="m1", optionActionId="act_ok")
        ov1.property = op.SerializeToString()

        dp1 = rt.elements.dictionary["dp1"]
        dp1.tag = E.DATEPICKER
        dp1.property = E.DatePickerProperty(actionId="act_date", placeHolder="选日期", initialDate="2026-11-30").SerializeToString()

        a_ok = card.actions["act_ok"]
        a_ok.url = "https://internal-api.feishu.cn/lark/card/api/actionv2"
        a_ok.method = 2
        a_ok.requiredParameters.needUserId = True
        a_ok.requiredParameters.needMessageId = True
        a_ok.parameters.parameters["action_id"] = "act_ok"
        a_ok.parameters.parameters["refresh_token"] = "tok1"
        a_url = card.actions["act_url"]
        a_url.method = 4
        a_url.openUrl.url = "https://example.com/doc"
        for k in ("act_mute", "act_date"):
            a = card.actions[k]
            a.url = "https://internal-api.feishu.cn/lark/card/api/actionv2"
            a.method = 2
            a.parameters.parameters["action_id"] = k

        wire = {"id": "9001", "chatId": "123", "type": 14, "content": card.SerializeToString(), "createTime": 1795700000}
        msg = message_view.decode(U, wire, "123")
        view, _ = message_view.describe(U, msg)
        act_row, s_static, s_person, ov, dp = view["nodes"]
        self.assertEqual(act_row["kind"], "actions")
        self.assertEqual(act_row["children"][0], {"kind": "button", "text": "Approve", "action": "act_ok", "tone": "primary"})
        self.assertEqual(act_row["children"][1], {"kind": "button", "text": "View Doc", "url": "https://example.com/doc"})
        self.assertEqual((s_static["kind"], s_static["action"], s_static["options"]),
                         ("select", "act_mute", [{"text": "Mute 30m", "value": "30"}]))
        self.assertTrue(s_person["disabled"])
        self.assertEqual(s_person["tone"], "person")
        self.assertEqual(ov["options"][0]["action"], "act_ok")
        self.assertEqual((dp["kind"], dp["action"], dp["value"]), ("date", "act_date", "2026-11-30"))

        # Execute POST actionv2 and verify refreshed card
        done_card = U.L.entities.CardContent(type=2, cardVersion=2)
        done_card.richtext.CopyFrom(richtext.build(U.L, "You have approved the request."))
        done_wire = dict(wire, content=done_card.SerializeToString(), updateTime=1795700010)

        class Client:
            auth = type("A", (), {
                "cookies": {"session": "s"}, "csrf_token": "csrf1",
                "region": type("R", (), {"locale": "zh_CN", "web_origin": "https://open-dev.feishu.cn"})(),
            })()
            pulls = 0

            def api(self, method, params):
                if method == "messages.PullMessagesByIdsRequest":
                    self.pulls += 1
                    return {"messages": {"9001": wire if self.pulls == 1 else done_wire}}
                return {}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client, f.me = Client(), "u_me"
        f.user_name = lambda uid, chat: "云文档助手"
        posted = {}

        class FakeResp:
            def raise_for_status(self): pass
            def json(self): return {"code": 0, "toast": {"content": "已批准"}}

        def fake_post(url, **kw):
            posted["url"] = url
            posted["json"] = kw["json"]
            return FakeResp()

        with mock.patch("requests.post", side_effect=fake_post), mock.patch.object(actions, "CARD_REFRESH_WAITS", (0,)):
            res = f.inbox_action({"chat_id": "123", "message_id": "9001", "action_id": "act_mute", "kind": "card", "value": "30"})
        self.assertEqual(posted["url"], "https://internal-api.feishu.cn/lark/card/api/actionv2")
        self.assertEqual(posted["json"]["common"], {"uid": "u_me", "mid": "9001"})
        self.assertEqual(posted["json"]["action"], {"action_id": "act_mute", "selected_option": "30"})
        self.assertEqual(res["toast"], "已批准")
        self.assertIn("approved", res["message"]["preview"])

    def test_calendar_conflict_rsvp_and_external_sync(self):
        """Calendar cards enrich live RSVP + conflict, reply via API, and invalidate on calendar WS push."""
        ev = U.L.entities.ShareCalendarEventContent()
        ev.calendarEvent.uniqueKey = "uk-1"
        ev.calendarEvent.summary = "数据回流评审"
        ev.calendarEvent.start = 1796036400
        ev.calendarEvent.end = 1796040000
        ev.calendarEvent.startTimezone = "Asia/Shanghai"
        ev.calendarEvent.selfAttendeeStatus = 1
        wire = {"id": "7001", "chatId": "123", "type": 16, "fromId": "u1", "content": ev.SerializeToString(), "createTime": 1795700000}

        rsvp = U.L.entities.GeneralCalendarContent(calendarType=4)
        rsvp.rsvpCardInfo.basicEventInfo.summary = "背屏需求评审"
        rsvp.rsvpCardInfo.basicEventInfo.uid = "uk-2"
        rsvp.rsvpCardInfo.basicEventInfo.startTime = 1796043600
        rsvp.rsvpCardInfo.basicEventInfo.endTime = 1796047200
        rsvp_wire = {"id": "7002", "chatId": "123", "type": 18, "fromId": "u1", "content": rsvp.SerializeToString(), "createTime": 1795700001}

        class Client:
            status = 2
            replied = []

            def api(self, method, params):
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 1}}}
                if method == "messages.PullMessagesByPositionsRequest":
                    return {"messages": {"0": wire, "1": rsvp_wire}}
                if method == "messages.PullMessagesByIdsRequest":
                    return {"messages": {"7001": wire}}
                if method == "calendars.GetUserPrimaryCalendarRequest":
                    return {"calendar": {"id": "my_cal"}}
                if method == "calendarevents.MGetCalendarEventsWithKeyRequest":
                    return {"calendarEvents": [
                        {"uniqueKey": "uk-1", "originalTime": 0, "selfAttendeeStatus": self.status, "start": 1796036400, "end": 1796040000},
                        {"uniqueKey": "uk-2", "originalTime": 0, "selfAttendeeStatus": 1, "start": 1796043600, "end": 1796047200},
                    ]}
                if method == "calendarevents.QueryInstanceRequest":
                    return {
                        "calendarEventInstance": [
                            {"eventRefId": "e_other", "beginTime": 1796036400, "endTime": 1796040000},
                            {"eventRefId": "e_self", "beginTime": 1796036400, "endTime": 1796040000},
                        ],
                        "events": {
                            "e_other": {"uniqueKey": "uk-conflict", "selfAttendeeStatus": 2, "isFree": False},
                            "e_self": {"uniqueKey": "uk-1", "selfAttendeeStatus": self.status, "isFree": False},
                        },
                    }
                if method == "calendarevents.ReplyCalendarEventInvitationRequest":
                    self.replied.append(params)
                    self.status = 4  # DECLINE
                    return {}
                return {}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f.client, f.me = Client(), "u_me"
        f.user_name = lambda uid, chat: "赵雨彤"
        rows = f.history.recent("123")
        self.assertEqual(rows[0]["view"]["status"], "accept")
        self.assertTrue(rows[0]["view"]["conflict"])
        self.assertEqual(rows[0]["view"]["action"], "cal:uk-1:0")
        self.assertEqual(rows[1]["view"]["title"], "背屏需求评审")
        self.assertEqual(rows[1]["view"]["status"], "needs_action")
        self.assertNotIn("conflict", rows[1]["view"])

        # Reply via inbox_action
        res = f.inbox_action({"chat_id": "123", "message_id": "7001", "action_id": "cal:uk-1:0", "kind": "calendar", "value": "decline"})
        self.assertEqual(f.client.replied[0]["replyType"], 2)
        self.assertEqual(f.client.replied[0]["calendarEventKey"]["calendarId"], "my_cal")
        self.assertEqual(res["message"]["view"]["status"], "decline")

        # Calendar WS sync push invalidates cached chat
        f.history.recent("123")
        self.assertIn("123", f.history.cache)
        f._pushes._push(1020003, b"", None, lambda _: None)
        self.assertNotIn("123", f.history.cache)

        # Card update push marks updated=True
        f.chat_info = lambda c: {"name": "云文档助手"}
        f.folded = lambda c: None
        f.dm_picture = lambda c: ""
        f.peer = lambda c, k="": ("u1", "云文档助手")
        f.dm_name = lambda c, p, s="": "云文档助手"
        card_update = {"msg_id": "m_card", "chat_id": "123", "from_id": "u1", "msg_type": 14, "chat_type": 1,
                       "create_time": 1000, "update_time": 1020, "content_data": {"cardDesc": "已通过"}}
        out = f.normalize(card_update)
        self.assertTrue(out.get("updated"))

    def test_link_preview_embedded_doc_and_meego_cards_and_inline_list(self):
        """Feishu doc & Meego links prefer embedded data, batch-fetch read-only previews, cache, and preserve text."""
        def text_msg_with_links(items, mid, hang_points=None, doc_entity=None):
            rt = U.L.entities.RichText()
            for idx, (kind, val) in enumerate(items):
                eid = str(idx + 1)
                rt.elementIds.append(eid)
                el = rt.elements.dictionary[eid]
                if kind == "text":
                    el.tag = E.TEXT
                    el.property = E.TextProperty(content=val).SerializeToString()
                else:
                    el.tag = E.A
                    el.property = E.AnchorProperty(href=val, textContent=val, content=val).SerializeToString()
            tc = U.L.entities.TextContent(richText=rt)
            if doc_entity:
                for k, (title, dtype) in doc_entity.items():
                    ref = tc.docEntity.elementEntityRef[k]
                    ref.title = title
                    ref.docType = dtype
            wire = {"id": mid, "chatId": "123", "type": 4, "fromId": "5",
                    "content": tc.SerializeToString(), "createTime": 1795700000}
            if hang_points:
                wire["urlPreviewHangPointMap"] = hang_points
            return wire

        sheet_url = "https://bytedance.larkoffice.com/sheets/NvwJsg4qlhK3uct1h4Kc13dHn8b?sheet=y2SnN9"
        wiki_url = "https://bytedance.larkoffice.com/wiki/En30wusn3ipAnlkQSPAckay6nGh"
        emb_url = "https://bytedance.larkoffice.com/docx/EmbDocToken123"
        meego_issue = "https://meego.larkoffice.com/flowco/issue/detail/7402919043?parentUrl=%2Fflowco"
        meego_view = "https://meego.larkoffice.com/flowco/issueView/3rIEbvROR?scope=workspaces"
        meego_list_urls = [f"https://meego.larkoffice.com/flowco/issue/detail/740000000{i}" for i in range(1, 5)]

        # m0: embedded DocEntity (should NOT call PullDocByURLsRequest for emb_url)
        m0 = text_msg_with_links([("link", emb_url)], "900", doc_entity={"1": ("消息自带标题", 8)})
        # m1: standalone Feishu sheet + wiki via PullDocByURLsRequest
        m1 = text_msg_with_links([("text", "请看进度 "), ("link", sheet_url), ("text", " 和 "), ("link", wiki_url)], "901")
        # m2: standalone Meego issueView via urlPreviewHangPointMap
        m2 = text_msg_with_links([("link", meego_view)], "902",
                                 hang_points={"1": {"url": meego_view, "previewId": "pid_view"}})
        # m3: 4 Meego issues in one message (> MAX_CARDS -> inline rich links, no card explosion)
        list_items = [("text", "待修复：\n")]
        list_hp = {}
        for i, u in enumerate(meego_list_urls):
            eid = str(len(list_items) + 1)
            list_items.append(("link", u))
            list_hp[eid] = {"url": u, "previewId": f"pid_i{i + 1}"}
            list_items.append(("text", "\n"))
        m3 = text_msg_with_links(list_items, "903", hang_points=list_hp)
        # m4: single Meego issue (card mode)
        m4 = text_msg_with_links([("link", meego_issue)], "904",
                                 hang_points={"1": {"url": meego_issue, "previewId": "pid_issue"}})

        class Client:
            calls = []

            def api(self, method, params):
                self.calls.append((method, params))
                if method == "chats.PullChatsByIdsRequest":
                    return {"chats": {"123": {"lastVisibleMessagePosition": 4}}}
                if method == "messages.PullMessagesByPositionsRequest":
                    return {"messages": {"0": m0, "1": m1, "2": m2, "3": m3, "4": m4}}
                if method == "chats.PullChatChattersByIDsRequest":
                    return {"chatters": {}}
                if method == "docs.PullDocByURLsRequest":
                    return {"docs": {
                        sheet_url: {"docType": 2, "name": "冷启动降级进度表", "ownerName": "皇凤伟", "updateTime": 1795797244},
                        wiki_url: {"docType": 8, "name": "启动入口保障双日会", "ownerName": "崔一鸣", "updateTime": 1795753128},
                    }}
                if method == "url_preview.GetPreviewRequest":
                    entities = {
                        "pid_view": {
                            "i18nTitleKey": "title",
                            "localeI18nTemplates": {"title": {"i18nTemplates": {"en_us": " [View] A项目缺陷视图"}}},
                            "previewBody": {"components": {"elements": {
                                "common-meego-header-0-0": {"header": {"title": "IssuesView · Flowco"}},
                                "common-text-width-lineheight-text-custom-0-0": {"text": {"text": "项1"}},
                                "common-text-width-lineheight-text-custom-1-0": {"text": {"text": "项2"}},
                            }}},
                        },
                        "pid_issue": {
                            "i18nTitleKey": "title",
                            "localeI18nTemplates": {"title": {"i18nTemplates": {"en_us": " [Issues] 【iOS】未染色"}}},
                            "previewBody": {"components": {"elements": {
                                "common-meego-header-0": {"header": {"title": "Issues · Flowco"}},
                                "common-tag-tags-0-0-0-1": {"tagList": {"tags": ["P1"]}},
                                "common-tag-tags-0-1-0-2": {"tagList": {"tags": ["OPEN"]}},
                            }}},
                        },
                    }
                    for i in range(1, 5):
                        entities[f"pid_i{i}"] = {
                            "i18nTitleKey": "title",
                            "localeI18nTemplates": {"title": {"i18nTemplates": {"en_us": f" [Issues] 缺陷 {i}"}}},
                            "previewBody": {"components": {"elements": {
                                "common-meego-header-0": {"header": {"title": "Issues · Flowco"}},
                                "common-tag-tags-0-0-0-1": {"tagList": {"tags": ["P2"]}},
                                "common-tag-tags-0-1-0-2": {"tagList": {"tags": ["OPEN"]}},
                            }}},
                        }
                    return {"previewEntityMap": {k: entities[k] for k in params.get("previewIds") or [] if k in entities}}
                return {}

        f = lark.Feishu(U, "feishu", lambda _: None)
        f._set_client(Client())
        self.addCleanup(f.logout)
        f.user_name = lambda uid, chat: "甲"
        f.history.recent("123")
        f.history.previews._pool.submit(lambda: None).result(timeout=3)
        rows = f.history.recent("123")

        # Embedded doc skipped docs.PullDocByURLsRequest
        doc_calls = [c[1]["urls"] for c in f.client.calls if c[0] == "docs.PullDocByURLsRequest"]
        self.assertEqual(len(doc_calls), 1)
        self.assertNotIn(emb_url, doc_calls[0])
        self.assertEqual(rows[0]["view"]["nodes"], [{
            "kind": "link-card", "url": emb_url, "text": "消息自带标题",
            "tone": "docx", "name": "飞书文档",
        }])

        # m1: prose inline links + 2 preview cards with owner & updated_at; raw text preserved for agents
        self.assertIn(sheet_url, rows[1]["text"])
        self.assertIn("[表格] 冷启动降级进度表", rows[1]["preview"])
        cards1 = [n for n in rows[1]["view"]["nodes"] if n["kind"] == "link-card"]
        self.assertEqual([c["text"] for c in cards1], ["冷启动降级进度表", "启动入口保障双日会"])
        self.assertEqual(cards1[0]["tone"], "sheet")
        self.assertEqual(cards1[0]["meta"], "皇凤伟")
        self.assertTrue(cards1[0]["updated_at"].endswith("Z"))
        self.assertEqual(cards1[1]["tone"], "wiki")

        # m2: standalone Meego view promoted to single link-card
        self.assertEqual(len(rows[2]["view"]["nodes"]), 1)
        vc = rows[2]["view"]["nodes"][0]
        self.assertEqual((vc["kind"], vc["tone"], vc["text"], vc["meta"]),
                         ("link-card", "meego-view", "A项目缺陷视图", "Flowco · 2 项预览"))

        # m3: 4 Meego issues stay inline rich links with badges (no link-card explosion)
        self.assertFalse(any(n["kind"] == "link-card" for n in rows[3]["view"]["nodes"]))
        inline_links = [n for n in rows[3]["view"]["nodes"][0]["children"] if n["kind"] == "link"]
        self.assertEqual([l["text"] for l in inline_links], ["缺陷 1", "缺陷 2", "缺陷 3", "缺陷 4"])
        self.assertEqual(inline_links[0]["tone"], "meego-issue")
        self.assertEqual(inline_links[0]["options"][0]["text"], "P2")

        # m4: single Meego issue promoted to link-card with priority & status badges
        ic = rows[4]["view"]["nodes"][0]
        self.assertEqual((ic["kind"], ic["tone"], ic["text"], ic["meta"]),
                         ("link-card", "meego-issue", "【iOS】未染色", "Flowco"))
        self.assertEqual(ic["options"], [{"text": "P1", "value": "attn"}, {"text": "OPEN", "value": "info"}])

        # Second fetch after invalidation hits TTL cache -> 0 new doc/preview RPCs
        prev_calls = len(f.client.calls)
        f.history.invalidate("123")
        f.history.recent("123")
        new_methods = [c[0] for c in f.client.calls[prev_calls:]]
        self.assertNotIn("docs.PullDocByURLsRequest", new_methods)
        self.assertNotIn("url_preview.GetPreviewRequest", new_methods)


if __name__ == "__main__":
    unittest.main()
