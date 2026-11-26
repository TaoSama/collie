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


if __name__ == "__main__":
    unittest.main()
