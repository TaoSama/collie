"""A Feishu with made-up people and no network, for tests (COLLIE_LARKX_FAKE=1).

It builds each post with the real proto, so a test sees what would be sent.
"""
import json
import os
import threading
import time

import lark
import richtext

ME = "u_me"
ACCOUNT = "示例账号"


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
        while not stop.is_set() and self.signed_in:
            self.wake.wait(1)

    # Unread counts as the fake Feishu has them: more than came while Collie ran.
    UNREAD = {"c_dm": 3, "c_group": 2}

    def unread(self, chats):
        return {c: {"unread": self.UNREAD[c]} for c in chats if c in self.UNREAD}

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

    def post(self, chat, markdown, footer="", reply_to=""):
        if chat == "c_refuse":
            raise lark.NotSent("飞书拒收（status 1）")
        if chat == "c_unsure":
            raise lark.Unsure("没收到飞书的答复，可能已经发出")
        rt = richtext.build(self.U.L, markdown, footer)
        self.posts.append(rt)
        with open(os.path.join(os.environ.get("COLLIE_EXTENSION_DATA", "."), "fake-posts.jsonl"), "a") as f:
            f.write(json.dumps({"chat": chat, "text": markdown, "footer": footer, "reply_to": reply_to,
                                "inner": rt.innerText}, ensure_ascii=False) + "\n")
        return "m_sent_%d" % len(self.posts)
