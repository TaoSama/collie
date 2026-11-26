"""Inbox data for the in-process fake, with message bodies in Feishu's wire shapes."""
import json
import time

import history
import message_view
import richtext

CHAT = "c_inbox"
CALENDAR = "c_calendar"
CARD_ID = "8005"
PIXEL = ("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAF"
         "gAI/ScLbtAAAAABJRU5ErkJggg==")


def card():
    return {
        "schema": "2.0",
        "header": {"template": "blue", "title": {"tag": "plain_text", "content": "数据库告警已恢复"},
                   "subtitle": {"tag": "plain_text", "content": "生产环境 · API 服务"}},
        "body": {"elements": [
            {"tag": "markdown", "content": "**连接池已恢复**\n错误率已回落，值班同学可继续观察。"},
            {"tag": "column_set", "columns": [
                {"tag": "column", "elements": [{"tag": "markdown", "content": "**错误率**\n0.08%"}]},
                {"tag": "column", "elements": [{"tag": "markdown", "content": "**影响范围**\n华东一区"}]},
            ]},
            {"tag": "div", "fields": [
                {"is_short": True, "text": {"tag": "lark_md", "content": "**负责人**\n数据库值班"}},
                {"is_short": True, "text": {"tag": "plain_text", "content": "状态 · 已恢复"}},
            ]},
            {"tag": "hr"},
            {"tag": "action", "actions": [
                {"tag": "button", "text": {"content": "查看监控"}, "type": "primary",
                 "behaviors": [{"type": "open_url", "default_url": "https://example.com/monitor"}]},
                {"tag": "button", "text": {"content": "确认恢复"}, "value": {"action": "ack"}},
            ]},
        ]},
    }


def messages(U):
    now = int(time.time())
    rich = U.decoders.protobuf_to_dict(richtext.build(U.L, "**复盘要点**\n> 恢复时间已更新。\n[记录](https://example.com/review)"))
    calendar = {
        "title": {"element_ids": ["1"], "inner_text": "发布复盘"},
        "start_time": "2026-11-27T15:00:00+08:00", "end_time": "2026-11-27T15:30:00+08:00",
        "location": "线上会议",
    }
    items = [
        (CHAT, "8001", message_view.SYSTEM, {"messageTemplate": "值班同学加入了会话"}),
        (CHAT, "8002", message_view.IMAGE, {"imageV2": {"imageKey": "img_v3_example"}}),
        (CHAT, "8003", message_view.FILE, {"name": "复盘摘要.txt", "size": "18", "key": "file_example"}),
        (CHAT, "8004", message_view.POST, {"title": "复盘记录", "richText": rich}),
        (CHAT, CARD_ID, message_view.CARD, {"jsonCard": json.dumps(card(), ensure_ascii=False)}),
        (CALENDAR, "8006", message_view.CALENDAR, {"calendar": json.dumps(calendar, ensure_ascii=False)}),
    ]
    for i, (chat, mid, kind, data) in enumerate(items):
        view, preview = message_view.describe(U, {"msg_type": kind, "content_data": data})
        yield {
            "id": mid, "chat_id": chat, "chat_kind": "dm", "chat": "值班助手" if chat == CHAT else "飞书日历",
            "sender_name": "" if kind == message_view.SYSTEM else "值班助手" if chat == CHAT else "飞书日历",
            "text": preview, "preview": preview, "view": view,
            "at": history.message_at({"create_time": now - (len(items) - i) * 60}),
        }
