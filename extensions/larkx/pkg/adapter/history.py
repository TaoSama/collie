"""Read a small inbox window and its attachments through the signed-in client."""
import base64
import collections
import threading
import time

import actions
from lark_inbox import require_client
import message_view

RECENT_COUNT = 12
RECENT_TTL = 30
RECENT_CHATS = 16
MEDIA_MOST = 20 << 20
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
                window, topic_mode = self._read(chat)
                with self.lock:
                    if self.active.get(chat) is fetch:
                        self.cache[chat] = (time.monotonic() + RECENT_TTL, window, topic_mode)
                        self.cache.move_to_end(chat)
                        while len(self.cache) > RECENT_CHATS:
                            self.cache.popitem(last=False)
            finally:
                with self.lock:
                    if self.active.get(chat) is fetch:
                        del self.active[chat]

        return self._shown(window) if self is self.adapter.history else []

    def topic_mode(self, chat):
        with self.lock:
            cached = self.cache.get(chat)
            return cached[2] if cached else False

    def _shown(self, window):
        # Cache the unadorned window so each poll can pick up newly resolved previews.
        messages, pairs = [], []
        for raw, entry in window:
            entry = dict(entry)
            if raw is not None:
                if entry.get("view") is not None:
                    entry["view"] = dict(entry["view"])
                pairs.append((raw, entry))
            messages.append(entry)
        self.previews.enrich(pairs, include_fallback=True)
        return messages

    @staticmethod
    def topic_root(raw, tid, root, topic, setting):
        return {**raw, "_thread": tid, "_root": root,
                "_replies": int(topic.get("replyCount", raw.get("replyCount")) or 0),
                "_subscribed": bool(setting.get("isFollowV2", setting.get("isFollow", False)))}

    @classmethod
    def thread_rows(cls, chat, result):
        """Keep each root and its metadata while retaining the recent replies in the same window."""
        rows, by_root = {}, {}
        settings = result.get("settings") or {}
        threads = result.get("threads") or {}

        def put(raw, hint=""):
            mid = str((raw or {}).get("id") or "")
            if not mid or str(raw.get("chatId") or raw.get("channelId") or chat) != chat:
                return
            root = str(raw.get("rootId") or "")
            root = root if root != "0" else ""
            tid = str(raw.get("threadId") or "")
            rows[mid] = {**raw, "_thread": (tid if tid != "0" else "") or hint, "_root": root}

        for mid, raw in (result.get("rootMessages") or {}).items():
            put(raw)
            if (mid := str((raw or {}).get("id") or mid)) in rows:
                by_root[mid] = rows[mid].get("_thread") or mid
        for raw in (result.get("lastReplyMessages") or {}).values():
            put(raw)
        for tid, bundle in (result.get("threadMessages") or {}).items():
            if not isinstance(bundle, dict):
                continue
            raw = bundle.get("rootMessage")
            put(raw, str(tid))
            if (mid := str((raw or {}).get("id") or "")) in rows:
                by_root[mid] = str(tid)
            for raw in bundle.get("lastMessages") or []:
                put(raw, str(tid))
                mid = str((raw or {}).get("id") or "")
                if mid and mid in rows and not rows[mid].get("_root"):
                    original = bundle.get("rootMessage") or {}
                    rows[mid]["_root"] = str(original.get("id") or "")
        meta = {}
        for position, topic in threads.items():
            tid = str(topic.get("id") or position)
            mid = str(topic.get("rootMessageId") or "")
            if mid and str(topic.get("channelId") or chat) == chat:
                by_root[mid] = tid
                meta[tid] = topic
        for mid, tid in by_root.items():
            if mid in rows:
                topic = meta.get(tid) or {}
                setting = settings.get(tid) or {}
                rows[mid] = cls.topic_root(rows[mid], tid, mid, topic, setting)
        for mid, raw in rows.items():
            root = raw.get("_root")
            if root and root != mid:
                raw["_thread"] = by_root.get(root) or raw.get("_thread") or root
        return rows

    def _pull_raw(self, chat, info, topic_mode):
        client = require_client(self.adapter)
        if topic_mode:
            last = info.get("lastVisibleThreadPosition")
            if last is None:
                last = info.get("lastThreadPosition")
            if last is not None and int(last) >= 0:
                last = int(last)
                positions = list(range(max(0, last - RECENT_COUNT + 1), last + 1))
                result = client.api("thread.PullThreadsByPositionsRequest", {
                    "channelId": chat, "type": CHANNEL_CHAT, "positions": positions, "lastMessageCount": TOPIC_REPLIES,
                }) or {}
                rows = self.thread_rows(chat, result)
                if rows:
                    return sorted(rows.values(), key=_raw_time_key)
        last = int(info.get("lastVisibleMessagePosition") or info.get("lastMessagePosition") or 0)
        # Positions count from 0: a chat with one message (a bot's card, say) has it at 0.
        positions = list(range(max(0, last - RECENT_COUNT + 1), last + 1))
        result = client.api("messages.PullMessagesByPositionsRequest", {"chatId": chat, "positions": positions}) or {}
        return [raw for _, raw in sorted((result.get("messages") or {}).items(), key=lambda kv: int(kv[0]))]

    def _read(self, chat):
        adapter = self.adapter
        info = adapter.chats_now([chat]).get(chat) or {}
        topic_mode = adapter.U.decoders.is_thread_chat(info)
        window = self._render(chat, self._pull_raw(chat, info, topic_mode))
        if topic_mode:
            return window, True
        with self.lock:
            replies = list(self.topic_replies.get(chat, ()))
        ids = {m["id"] for _, m in window}
        window += [(msg, m) for msg, m in replies if m["id"] not in ids]
        window.sort(key=lambda item: item[1]["at"])
        return window[-RECENT_COUNT:], False

    def render_rows(self, chat, raws):
        return self._shown(self._render(chat, sorted(raws, key=_raw_time_key)))

    def _render(self, chat, raws):
        adapter = self.adapter
        messages = []
        cal_pairs = []
        for raw in raws:
            msg = message_view.decode(adapter.U, raw, chat)
            if not msg["msg_id"] or str(msg["chat_id"]) != chat:
                continue
            sender = str(msg["from_id"] or "")
            recalled = raw.get("status") == adapter.U.L.entities.Message.DELETED
            if recalled:
                text = preview = "撤回了一条消息"
                view = dict(kind="recalled", nodes=[message_view.node("text", text)])
            else:
                view, preview = message_view.describe(adapter.U, msg, emojis=adapter.emojis, me=adapter.me)
                text = preview
                if msg["msg_type"] in (message_view.TEXT, message_view.POST):
                    text = adapter._written(sender, msg["content_data"] or {})[0]
            is_system = msg["msg_type"] == message_view.SYSTEM
            entry = dict(id=str(msg["msg_id"]), chat_id=chat, sender=sender, text=text, preview=preview,
                         sender_name="" if is_system else adapter.user_name(sender, chat),
                         owner=not is_system and bool(adapter.me) and sender == adapter.me,
                         at=message_at(msg), view=view)
            root = str(raw.get("_root") or raw.get("rootId") or "")
            thread = raw.get("_thread") or (str(raw.get("threadId") or "") if root and root != "0" else "")
            is_thread_root = raw.get("threadMessageType") == adapter.U.L.entities.Message.THREAD_ROOT_MESSAGE
            if is_thread_root and root in ("", "0"):
                root = entry["id"]
                thread = str(raw.get("threadId") or "")
                thread = thread if thread and thread != "0" else root
                entry["reply_count"] = int(raw.get("replyCount") or 0)
            if root and root != "0":
                entry["root_id"] = root
            if thread and thread != "0":
                entry["thread_id"] = thread
            if "_replies" in raw:
                entry["reply_count"] = raw["_replies"]
                entry["subscribed"] = raw["_subscribed"]
            link_msg = msg if not recalled and msg["msg_type"] in (message_view.TEXT, message_view.POST) else None
            messages.append((link_msg, entry))
            if view and view.get("kind") == "calendar":
                cal_pairs.append((msg, entry))
        if cal_pairs:
            actions.enrich_calendars(adapter, cal_pairs)
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
