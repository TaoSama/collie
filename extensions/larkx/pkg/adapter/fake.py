"""A Feishu with made-up people and no network, for tests (COLLIE_LARKX_FAKE=1).

It builds each post with the real proto, so a test sees what would be sent.
"""
import copy
import json
import os
import threading
import time

import inbox_example
import lark
import peer
import richtext

ME = "u_me"
ACCOUNT = "示例账号"
TOPICS = "c_topics"
CHAT_NAMES = {"c_group": "示例群", TOPICS: "示例话题群"}
EMOJI = dict(THUMBSUP="👍", HEART="❤️", LAUGH="😂", THANKS="🙏", EYES="👀", PARTY="🎉", OK="👌",
             CLAP="👏", MUSCLE="💪", FIRE="🔥", SMILE="🙂", CRY="😭", STAR="⭐", ROCKET="🚀")



class Fake:
    def __init__(self, U, region, on_save):
        self.U = U
        self.on_save = on_save
        self.polls = 0
        self.signed_in = False
        self.posts = []
        self.wake = threading.Event()
        self.trigger = ""
        self.ignored = False
        self.marked = set()
        self.history = self
        self.lock = threading.RLock()
        self.inbox = list(inbox_example.messages(U))
        self.inbox.extend([
            self._message("c_group", "m2", "@示例账号 大家好"),
            self._message("c_group", "9001", "今天优先回归弱网切换、识别纠错和蓝牙连接。请把复测结果补到同一条记录里。",
                          reactions=[{"key": "THUMBSUP", "count": 2, "names": ["林同学", "陈同学"], "mine": False},
                                     {"key": "HEART", "count": 3, "mine": False}]),
            self._message(TOPICS, "9101", "已汇总本轮联测问题。弱网切换和蓝牙连接优先复测。",
                          view={"kind": "post", "title": "周末联测进展", "nodes": [{"kind": "text", "text": "已汇总本轮联测问题。弱网切换和蓝牙连接优先复测。"}]},
                          thread_id="9101", root_id="9101", reply_count=1, subscribed=False,
                          reactions=[{"key": "THUMBSUP", "count": 3, "mine": False}]),
            self._message(TOPICS, "9102", "下周补齐远程设备回归用例。",
                          view={"kind": "post", "title": "周一回归安排", "nodes": [{"kind": "text", "text": "下周补齐远程设备回归用例。"}]},
                          thread_id="9102", root_id="9102", subscribed=False),
            self._message(TOPICS, "9111", "我来补充车载设备的记录。", thread_id="9101", root_id="9101"),
        ])

    def _message(self, chat, mid, text, **fields):
        return {"id": mid, "chat_id": chat, "chat_kind": "group", "chat": CHAT_NAMES.get(chat, chat),
                "sender": "u_b", "sender_name": "示例同事", "owner": False,
                "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "text": text, **fields}

    def _find(self, chat, mid):
        msg = next((m for m in self.inbox if m["chat_id"] == chat and m["id"] == mid), None)
        if msg is None:
            raise lark.NotSent("找不到这个会话里的消息")
        return msg


    def restore(self, saved):
        if saved and saved.get("unreachable_once") and not getattr(self, "_tried", False):
            self._tried = True  # Feishu slow to answer the first time, as after a restart
            raise lark.Unreachable("Read timed out")
        if not saved or saved.get("cookies", {}).get("session") != "example":
            raise lark.Expired("会话已被服务端注销")
        self.signed_in = True
        return ACCOUNT

    def login_begin(self):
        self.polls = 0
        return json.dumps({"qrlogin": {"token": "example"}})

    def login_poll(self):
        self.polls += 1
        return lark.QR_DONE if self.polls >= 2 else lark.QR_SCANNED

    def login_finish(self):
        saved = {"region": "feishu", "cookies": {"session": "example"}, "user_id": ME}
        self.on_save(saved)
        return self.restore(saved)

    def logout(self):
        self.signed_in = False
        self.wake.set()

    def stop(self):
        self.wake.set()

    def listen(self, on_message, on_link, stop):
        on_link("up")
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        for m in (
            {"id": "m1", "chat_id": "c_dm", "chat": "示例联系人", "chat_kind": "dm", "sender": "u_a",
             "sender_name": "示例联系人", "owner": False, "text": "周五的会改到几点？", "at": now},
            {"id": "m2", "chat_id": "c_group", "chat": "示例群", "chat_kind": "group", "sender": "u_b",
             "sender_name": "示例同事", "owner": False, "text": "@示例账号 大家好", "at_me": True, "at": now},
            {"id": "m3", "chat_id": "c_self", "chat": ACCOUNT, "chat_kind": "self", "sender": ME,
             "sender_name": ACCOUNT, "owner": True, "text": "/ai 今天有什么会", "at": now},
        ):
            if m["owner"] and not self.ignored and lark.is_request(m["text"], self.trigger):
                self.mark_working(m["id"])
            on_message(m)
        for m in self.inbox:
            if m["id"] in (inbox_example.CARD_ID, "8006", "9101"):
                on_message(m)
        while not stop.is_set() and self.signed_in:
            self.wake.wait(1)

    def recent(self, chat):
        with self.lock:
            return copy.deepcopy([m for m in self.inbox if m["chat_id"] == chat])

    def inbox_recent(self, chat):
        return {"messages": self.recent(chat),
                "lark": {"topic_mode": chat == TOPICS,
                         "emoji": [{"key": k, "label": v} for k, v in EMOJI.items()],
                         "recent_emoji": list(EMOJI)[:6]}}

    def inbox_lark_action(self, params):
        chat, mid = params["chat_id"], params.get("message_id")
        action, thread = params["action"], params.get("thread_id")
        with self.lock:
            msg = self._find(chat, mid) if mid else None
            root = self._find(chat, thread) if thread else None
            if root is not None and (root.get("thread_id") != thread or
                                     (msg is not None and msg.get("thread_id") != thread)):
                raise lark.NotSent("话题和消息不匹配")
            if action == "topic":
                return {"messages": [m for m in self.recent(chat) if m.get("thread_id") == thread]}
            if action == "subscribe":
                root["subscribed"] = params["active"]
            elif action == "reaction":
                key, active = params["key"], params["active"]
                if key not in EMOJI:
                    raise lark.NotSent("飞书表情不存在")
                reactions = msg.setdefault("reactions", [])
                item = next((r for r in reactions if r["key"] == key), None)
                if item is None and active:
                    reactions.append({"key": key, "count": 1, "mine": True})
                elif item is not None and item["mine"] != active:
                    item["count"] += 1 if active else -1
                    item["mine"] = active
                    if item["count"] == 0:
                        reactions.remove(item)
            elif action == "forward":
                target = params["target"]
                sent = self._message(target, "fwd_%d" % len(self.inbox), msg["text"])
                sent.update(owner=True, sender=ME, sender_name=ACCOUNT)
                if target == TOPICS:
                    sent.update(thread_id=sent["id"], root_id=sent["id"], subscribed=False)
                self.inbox.append(sent)
                if target == "c_self":
                    # The write arrived but its acknowledgement did not.
                    raise peer.Error("uncertain", "飞书未确认转发结果")
            else:
                raise lark.NotSent("未知的飞书操作")
            return {}


    def media(self, chat, mid, key):
        if (chat, mid, key) == (inbox_example.CHAT, "8002", "img_v3_example"):
            return {"mime": "image/png", "data": inbox_example.PIXEL}
        if (chat, mid, key) == (inbox_example.CHAT, "8003", "file_example"):
            return {"mime": "text/plain", "data": "ZXhhbXBsZSBpbmJveCBmaWxl"}
        raise ValueError("示例附件不存在")

    # Unread counts as the fake Feishu has them: more than came while Collie ran.
    UNREAD = {"c_dm": 3, "c_group": 2}

    def unread(self, chats):
        return {c: {"unread": self.UNREAD[c], "at_me": c == "c_group"} for c in chats if c in self.UNREAD}

    def mark_read(self, chats):
        self.UNREAD = dict(self.UNREAD)  # this Feishu's own, not the class's
        for c in chats:
            if c in self.UNREAD:
                self.UNREAD[c] = 0
        with open(os.path.join(os.environ.get("COLLIE_EXTENSION_DATA", "."), "fake-read.jsonl"), "a") as f:
            f.write(json.dumps({"chats": chats}) + "\n")
        return [c for c in chats if c in self.UNREAD], {c: "飞书里没有这个会话" for c in chats if c not in self.UNREAD}

    def mark_working(self, message_id, got=None):
        if message_id not in self.marked:
            self.marked.add(message_id)
            self.react(message_id)

    def react(self, message_id, key=lark.WORKING_REACTION):
        with open(os.path.join(os.environ.get("COLLIE_EXTENSION_DATA", "."), "fake-reactions.jsonl"), "a") as f:
            f.write(json.dumps({"message_id": message_id, "key": key}) + "\n")

    def post(self, chat, markdown, footer="", reply_to="", reply_mode=""):
        if chat == "c_refuse":
            raise lark.NotSent("飞书拒收（status 1）")
        if chat == "c_unsure":
            raise lark.Unsure("没收到飞书的答复，可能已经发出")
        rt = richtext.build(self.U.L, markdown, footer)
        with self.lock:
            target = self._find(chat, reply_to) if reply_mode and reply_to else None
            if reply_mode == "publish" and chat != TOPICS:
                raise lark.NotSent("只有话题群才能发布话题")
            self.posts.append(rt)
            mid = "m_sent_%d" % len(self.posts)
            if reply_mode:
                msg = self._message(chat, mid, markdown)
                msg.update(owner=True, sender=ME, sender_name=ACCOUNT)
                if reply_mode == "publish":
                    msg.update(thread_id=mid, root_id=mid, subscribed=False)
                elif reply_mode in ("create_topic", "comment"):
                    root_id = target.get("root_id") or target["id"]
                    root = self._find(chat, root_id)
                    root.update(root_id=root_id, thread_id=root_id, reply_count=root.get("reply_count", 0) + 1)
                    msg.update(thread_id=root_id, root_id=root_id)
                self.inbox.append(msg)
            with open(os.path.join(os.environ.get("COLLIE_EXTENSION_DATA", "."), "fake-posts.jsonl"), "a") as f:
                f.write(json.dumps({"chat": chat, "text": markdown, "footer": footer, "reply_to": reply_to,
                                    "reply_mode": reply_mode, "inner": rt.innerText}, ensure_ascii=False) + "\n")
            return mid
