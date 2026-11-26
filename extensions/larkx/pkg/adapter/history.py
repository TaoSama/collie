"""Read a small inbox window and its attachments through the signed-in client."""
import base64
import collections
import threading
import time

import message_view

RECENT_COUNT = 12
RECENT_TTL = 30
RECENT_CHATS = 16
MEDIA_MOST = 20 << 20


def message_at(msg):
    at = int(msg.get("create_time") or 0)
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at / 1000 if at > 10 ** 12 else at)) if at else ""


class History:
    def __init__(self, adapter):
        self.adapter = adapter
        self.lock = threading.Lock()
        self.cache = collections.OrderedDict()
        self.active = {}

    def invalidate(self, chat):
        with self.lock:
            self.cache.pop(chat, None)
            self.active.pop(chat, None)

    def recent(self, chat):
        if not chat.isdigit():
            return []
        with self.lock:
            cached = self.cache.get(chat)
            if cached and cached[0] > time.monotonic():
                return cached[1]
            # A poll or second browser can reuse the last window while one request refreshes it.
            if chat in self.active:
                return cached[1] if cached else []
            fetch = self.active[chat] = object()
        try:
            messages = self._read(chat)
            with self.lock:
                if self.active.get(chat) is fetch:
                    self.cache[chat] = (time.monotonic() + RECENT_TTL, messages)
                    self.cache.move_to_end(chat)
                    while len(self.cache) > RECENT_CHATS:
                        self.cache.popitem(last=False)
            return messages
        finally:
            with self.lock:
                if self.active.get(chat) is fetch:
                    del self.active[chat]

    def _read(self, chat):
        adapter = self.adapter
        info = adapter.chats_now([chat]).get(chat) or {}
        last = int(info.get("lastVisibleMessagePosition") or info.get("lastMessagePosition") or 0)
        positions = list(range(max(1, last - RECENT_COUNT + 1), last + 1))
        if not positions:
            return []
        result = adapter.client.api("messages.PullMessagesByPositionsRequest", {"chatId": chat, "positions": positions}) or {}
        messages = []
        for _, raw in sorted((result.get("messages") or {}).items(), key=lambda kv: int(kv[0])):
            msg = message_view.decode(adapter.U, raw, chat)
            if not msg["msg_id"] or str(msg["chat_id"]) != chat:
                continue
            sender = str(msg["from_id"] or "")
            view, preview = message_view.describe(adapter.U, msg)
            data = msg["content_data"] or {}
            text = preview
            if msg["msg_type"] in (message_view.TEXT, message_view.POST):
                text = adapter._written(sender, data)[0]
            is_system = msg["msg_type"] == message_view.SYSTEM
            messages.append(dict(id=str(msg["msg_id"]), chat_id=chat, sender=sender, text=text, preview=preview,
                                 sender_name="" if is_system else adapter.user_name(sender, chat),
                                 owner=not is_system and bool(adapter.me) and sender == adapter.me,
                                 at=message_at(msg), view=view))
        return messages[-RECENT_COUNT:]

    def media(self, chat, mid, key):
        if not chat.isdigit() or not mid.isdigit() or not message_view.media_key(key):
            raise ValueError("无效的飞书附件")
        adapter = self.adapter
        auth, media = adapter.client.auth, adapter.U.media
        url = media.message_resource_url(mid, key, chat, host=auth.region.file_host)
        buf, mime = media.download(auth, url, max_bytes=MEDIA_MOST)
        return {"mime": (mime or "application/octet-stream").split(";")[0], "data": base64.b64encode(buf).decode()}
