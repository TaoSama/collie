"""Read a small inbox window and its attachments through the signed-in client."""
import base64
import collections
import threading
import time

import actions
import message_view

RECENT_COUNT = 12
RECENT_TTL = 30
RECENT_CHATS = 16
MEDIA_MOST = 20 << 20
CHAT_TOPICS = 3
CHANNEL_CHAT = 1
TOPIC_REPLIES = 3


def message_at(msg):
    at = int(msg.get("create_time") or 0)
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at / 1000 if at > 10 ** 12 else at)) if at else ""


def _raw_time_key(raw):
    ms = int(raw.get("createTimeMs") or 0)
    if not ms:
        sec = int(raw.get("createTime") or 0)
        ms = sec if sec > 10 ** 12 else sec * 1000
    mid = raw.get("id") or ""
    return (ms, int(mid) if str(mid).isdigit() else 0)


class History:
    message_at = staticmethod(message_at)

    def __init__(self, adapter, previews):
        self.adapter = adapter
        self.previews = previews
        self.lock = threading.Lock()
        self.cache = collections.OrderedDict()
        self.active = {}
        # Replies inside a topic (position < 0) live in the topic, not in the chat's
        # own positions: the ones pushed since this run, by chat, newest last.
        self.topic_replies = {}

    def saw_topic_reply(self, chat, raw, entry):
        with self.lock:
            self.topic_replies.setdefault(chat, collections.deque(maxlen=RECENT_COUNT)).append((raw, entry))

    def invalidate(self, chat):
        with self.lock:
            self.cache.pop(chat, None)
            self.active.pop(chat, None)

    def invalidate_calendars(self):
        with self.lock:
            for chat in list(self.cache):
                msgs = self.cache[chat][1]
                if any((m.get("view") or {}).get("kind") == "calendar" for _, m in msgs):
                    self.cache.pop(chat, None)
                    self.active.pop(chat, None)

    def recent(self, chat):
        if not chat.isdigit():
            return []
        with self.lock:
            cached = self.cache.get(chat)
            if (cached and cached[0] > time.monotonic()) or chat in self.active:
                # A poll or second browser can reuse the last window while one request refreshes it.
                window = cached[1] if cached else []
                fetch = None
            else:
                fetch = self.active[chat] = object()
        if fetch is not None:
            try:
                window = self._read(chat)
                with self.lock:
                    if self.active.get(chat) is fetch:
                        self.cache[chat] = (time.monotonic() + RECENT_TTL, window)
                        self.cache.move_to_end(chat)
                        while len(self.cache) > RECENT_CHATS:
                            self.cache.popitem(last=False)
            finally:
                with self.lock:
                    if self.active.get(chat) is fetch:
                        del self.active[chat]

        # Cache the unadorned window so each poll can pick up newly resolved previews.
        messages, pairs = [], []
        for raw, entry in window:
            if raw is not None:
                entry = dict(entry)
                if entry.get("view") is not None:
                    entry["view"] = dict(entry["view"])
                pairs.append((raw, entry))
            messages.append(entry)
        self.previews.enrich(pairs, include_fallback=True)
        return messages if self is self.adapter.history else []

    def _pull_raw(self, chat, info):
        client = self.adapter.client
        if info.get("chatMode") == CHAT_TOPICS:
            last = info.get("lastVisibleThreadPosition")
            if last is None:
                last = info.get("lastThreadPosition")
            if last is not None and int(last) >= 0:
                last = int(last)
                positions = list(range(max(0, last - RECENT_COUNT + 1), last + 1))
                result = client.api("thread.PullThreadsByPositionsRequest", {
                    "channelId": chat, "type": CHANNEL_CHAT, "positions": positions, "lastMessageCount": TOPIC_REPLIES,
                }) or {}
                by_id = {}
                for bucket in ((result.get("rootMessages") or {}).values(),
                               (result.get("lastReplyMessages") or {}).values()):
                    for raw in bucket:
                        mid = str((raw or {}).get("id") or "")
                        if mid and str(raw.get("chatId") or raw.get("channelId") or chat) == chat:
                            by_id[mid] = raw
                for td in (result.get("threadMessages") or {}).values():
                    if not isinstance(td, dict):
                        continue
                    for raw in [td.get("rootMessage")] + list(td.get("lastMessages") or []):
                        mid = str((raw or {}).get("id") or "")
                        if mid and str(raw.get("chatId") or raw.get("channelId") or chat) == chat:
                            by_id[mid] = raw
                if by_id:
                    return sorted(by_id.values(), key=_raw_time_key)[-RECENT_COUNT:]
        last = int(info.get("lastVisibleMessagePosition") or info.get("lastMessagePosition") or 0)
        # Positions count from 0: a chat with one message (a bot's card, say) has it at 0.
        positions = list(range(max(0, last - RECENT_COUNT + 1), last + 1))
        result = client.api("messages.PullMessagesByPositionsRequest", {"chatId": chat, "positions": positions}) or {}
        return [raw for _, raw in sorted((result.get("messages") or {}).items(), key=lambda kv: int(kv[0]))]

    def _read(self, chat):
        adapter = self.adapter
        info = adapter.chats_now([chat]).get(chat) or {}
        messages = []
        cal_pairs = []
        for raw in self._pull_raw(chat, info):
            msg = message_view.decode(adapter.U, raw, chat)
            if not msg["msg_id"] or str(msg["chat_id"]) != chat:
                continue
            sender = str(msg["from_id"] or "")
            view, preview = message_view.describe(adapter.U, msg, emojis=adapter.emojis, me=adapter.me)
            data = msg["content_data"] or {}
            text = preview
            if msg["msg_type"] in (message_view.TEXT, message_view.POST):
                text = adapter._written(sender, data)[0]
            is_system = msg["msg_type"] == message_view.SYSTEM
            entry = dict(id=str(msg["msg_id"]), chat_id=chat, sender=sender, text=text, preview=preview,
                         sender_name="" if is_system else adapter.user_name(sender, chat),
                         owner=not is_system and bool(adapter.me) and sender == adapter.me,
                         at=message_at(msg), view=view)
            link_msg = msg if msg["msg_type"] in (message_view.TEXT, message_view.POST) else None
            messages.append((link_msg, entry))
            if view and view.get("kind") == "calendar":
                cal_pairs.append((msg, entry))
        if cal_pairs:
            actions.enrich_calendars(adapter, cal_pairs)
        with self.lock:
            replies = list(self.topic_replies.get(chat, ()))
        ids = {m["id"] for _, m in messages}
        messages += [(msg, m) for msg, m in replies if m["id"] not in ids]
        messages.sort(key=lambda item: item[1]["at"])
        messages = messages[-RECENT_COUNT:]
        pictures = adapter.user_pictures(chat, {m["sender"] for _, m in messages if m["sender_name"]})
        for _, m in messages:
            if pictures.get(m["sender"]):
                m["sender_avatar"] = pictures[m["sender"]]
        return messages

    def media(self, chat, mid, key):
        if not chat.isdigit() or not mid.isdigit() or not message_view.media_key(key):
            raise ValueError("无效的飞书附件")
        adapter = self.adapter
        auth, media = adapter.client.auth, adapter.U.media
        url = media.message_resource_url(mid, key, chat, host=auth.region.file_host)
        buf, mime = media.download(auth, url, max_bytes=MEDIA_MOST)
        return {"mime": (mime or "application/octet-stream").split(";")[0], "data": base64.b64encode(buf).decode()}
