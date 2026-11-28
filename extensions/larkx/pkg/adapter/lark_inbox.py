"""Native inbox operations on the logged-in Feishu Web Gateway."""

import logging
import threading
import time

log = logging.getLogger("larkx")

RECENT_EMOJI_TTL = 60
REACTION_NAMES_MAX = 2
REACTION_BATCH = 50
TOPIC_WINDOW = 50
UNCERTAIN_FORWARD = "飞书未确认转发结果，请先到目标会话查看"


def raw_id(value):
    value = str(value or "")
    return value if value.isascii() and value.isdecimal() and value != "0" and len(value) <= 20 else ""


def _count(value):
    try:
        return max(0, int(value or 0))
    except (TypeError, ValueError):
        return 0


def _request(adapter, method, data, uncertain="飞书网关没有确认操作"):
    """Decode the status before the payload so a gateway refusal can't look like success."""
    import requests
    from google.protobuf.message import DecodeError
    U = adapter.U
    gateway = U.gateway
    cmd, req, response = gateway.resolve_cmd(method)
    pkt = gateway.build_packet(cmd, U.ids.generate_long_request_id(), req_msg=gateway.request_from_dict(req, data))
    try:
        raw = adapter.client._gateway_post(pkt)
    except requests.exceptions.HTTPError as e:
        status = e.response.status_code if e.response is not None else 0
        if 400 <= status < 500:
            raise ValueError("飞书拒绝操作（HTTP %s）" % status) from e
        raise ValueError(uncertain) from e
    except requests.exceptions.RequestException as e:
        raise ValueError(uncertain) from e
    if not raw:
        raise ValueError(uncertain)
    try:
        reply = U.P.Packet.FromString(raw)
        if reply.status:
            raise ValueError("飞书拒绝操作（状态 %s）" % reply.status)
        return gateway.response_to_dict(response, reply.payload) if reply.payload else {}
    except DecodeError as e:
        raise ValueError(uncertain) from e


def _matches_chat(raw, chat):
    ids = [str(raw[key]) for key in ("chatId", "channelId") if raw.get(key) not in (None, "", 0, "0")]
    return bool(ids) and all(value == chat for value in ids)


class Inbox:
    def __init__(self, adapter):
        self.adapter = adapter
        self.lock = threading.Lock()
        self.mru = (0, [])

    def recent(self, chat):
        adapter = self.adapter
        messages = adapter.history.recent(chat)
        result = {"messages": messages}
        if not raw_id(chat):
            return result
        resources = adapter.emojis()
        result["lark"] = {
            "topic_mode": adapter.history.topic_mode(chat),
            "emoji": [{"key": key, "label": label, "url": url} for key, (label, url) in resources.items()],
            "recent_emoji": self._recent_emoji(),
        }
        self.enrich_reactions(chat, messages)
        return result

    def _recent_emoji(self):
        with self.lock:
            expires, previous = self.mru
            if expires > time.monotonic():
                return list(previous)
        try:
            out = self.adapter.client.api("reactions.PullUserMRUReactionsRequest", {}) or {}
            keys = list(dict.fromkeys(key for key in out.get("userMruReactions", []) if key))
        except Exception as e:
            log.warning("recent emoji: %s", e)
            keys = previous
        with self.lock:
            self.mru = (time.monotonic() + RECENT_EMOJI_TTL, keys)
        return list(keys)

    def enrich_reactions(self, chat, messages):
        by_id = {row["id"]: row for row in messages if raw_id(row["id"])
                 and (row.get("view") or {}).get("kind") not in ("system", "recalled")}
        ids = list(by_id)
        for i in range(0, len(ids), REACTION_BATCH):
            try:
                self._reactions(chat, by_id, ids[i:i + REACTION_BATCH])
            except Exception as e:
                log.warning("reactions in %s: %s", chat, e)

    def _reactions(self, chat, by_id, ids):
        adapter = self.adapter
        response = adapter.client.api("reactions.PullReactionsRequest", {
            "messageIds": ids, "fetchUsers": True, "fetchChatters": True,
        }) or {}
        if response.get("status"):
            raise ValueError("飞书未能返回表情回应")
        own = {}
        for item in response.get("selfReactionStatus") or []:
            if not item.get("userId") or str(item["userId"]) == adapter.me:
                identity = str(item.get("messageId")), item.get("reactionType")
                stamp = _count(item.get("nanoUpdateTime"))
                if identity not in own or stamp >= own[identity][0]:
                    own[identity] = (stamp, not item.get("isCancelled", False))
        for mid in ids:
            by_id[mid]["reactions"] = []
        for entry in response.get("messageReactions") or []:
            mid = str(entry.get("messageId"))
            if mid not in ids:
                continue
            for item in entry.get("reactions") or []:
                key, count = item.get("type"), _count(item.get("count"))
                if not key or not count:
                    continue
                ids_seen = list(dict.fromkeys(str(uid) for users in (item.get("userIds"), item.get("chatterIds"))
                                              for uid in users or [] if uid))
                status = own.get((mid, key))
                mine = status[1] if status else bool(adapter.me) and adapter.me in ids_seen
                if status and not mine:
                    ids_seen = [uid for uid in ids_seen if uid != adapter.me]
                row = {"key": key, "count": count, "mine": mine}
                if count <= REACTION_NAMES_MAX and len(ids_seen) == count:
                    names = [("你" if uid == adapter.me else adapter.user_name(uid, chat)) for uid in ids_seen]
                    if all(names):
                        row["names"] = names
                by_id[mid]["reactions"].append(row)

    def message(self, chat, mid):
        if not raw_id(chat) or not raw_id(mid):
            raise ValueError("无效的飞书消息")
        reply = self.adapter.client.api("messages.PullMessagesByIdsRequest", {"messageIds": [mid]}) or {}
        raw = (reply.get("messages") or {}).get(mid)
        M = self.adapter.U.L.entities.Message
        if (not raw or str(raw.get("id") or "") != mid or not _matches_chat(raw, chat)
                or raw.get("status") == M.DELETED or raw.get("type") in (None, M.SYSTEM)):
            raise ValueError("该消息不可操作或不属于当前会话")
        return raw

    def topic(self, chat, tid, mid=""):
        if not raw_id(chat) or not raw_id(tid):
            raise ValueError("无效的飞书话题")
        reply = self.adapter.client.api("thread.PullThreadsByIDsRequest", {
            "threadIds": [tid], "lastMessageCount": 0,
        }) or {}
        topic = (reply.get("threads") or {}).get(tid)
        if not topic or str(topic.get("id") or "") != tid or str(topic.get("channelId") or "") != chat:
            raise ValueError("该话题不可操作或不属于当前会话")
        root = raw_id(topic.get("rootMessageId"))
        if not root or (mid and mid != root):
            raise ValueError("飞书话题与所选消息不一致")
        return root, topic, reply

    def comments(self, chat, tid, mid):
        root, topic, reply = self.topic(chat, tid, mid)
        history = self.adapter.history
        rows = history.thread_rows(chat, reply)
        start = _count(topic.get("messageStartPosition"))
        last = _count(topic.get("lastVisibleMessagePosition", topic.get("lastMessagePosition")))
        positions = list(range(max(start, last - TOPIC_WINDOW + 1), last + 1))
        if positions and (_count(topic.get("replyCount")) or last > start):
            response = self.adapter.client.api("thread.PullThreadMessagesByPositionsRequest", {
                "threadId": tid, "positions": positions, "channelId": chat, "channelType": 1,
                "onlyVisibleMessages": True,
            }) or {}
            if "status" in response and response["status"]:
                raise ValueError("飞书未能读取话题评论")
            for raw in (response.get("messages") or {}).values():
                if _matches_chat(raw, chat) and (str(raw.get("threadId") or "") == tid or
                                                str(raw.get("rootId") or "") == root):
                    rid = str(raw.get("id") or "")
                    if rid != root:
                        rows[rid] = {**raw, "_root": root, "_thread": tid}
        if root not in rows:
            raw = self.message(chat, root)
            setting = (reply.get("settings") or {}).get(tid) or {}
            rows[root] = history.topic_root(raw, tid, root, topic, setting)
        messages = history.render_rows(chat, rows.values())
        self.enrich_reactions(chat, messages)
        return messages

    def action(self, params):
        chat, mid, tid = (params.get(key) or "" for key in ("chat_id", "message_id", "thread_id"))
        action = params.get("action")
        if action == "topic":
            return {"messages": self.comments(chat, tid, mid)}
        if action == "reaction":
            self.message(chat, mid)
            # Reactions are enriched from Feishu separately on every history read.
            self.reaction(mid, params.get("key"), params.get("active"))
        elif action == "subscribe":
            self.topic(chat, tid, mid)
            try:
                self.subscribe(tid, params.get("active"))
            finally:
                self.adapter.history.invalidate(chat)
        elif action == "forward":
            target = params.get("target") or ""
            if not raw_id(target):
                raise ValueError("无效的转发会话")
            try:
                if tid:
                    self.topic(chat, tid, mid)
                    self.forward_topic(chat, tid, target)
                else:
                    raw = self.message(chat, mid)
                    self.forward_message(chat, mid, raw, target)
            finally:
                self.adapter.history.invalidate(target)
        else:
            raise ValueError("未知的飞书收件箱操作")
        return {}

    def prepare_reply(self, req, chat, mid, mode):
        adapter = self.adapter
        if not raw_id(chat):
            raise ValueError("无效的飞书会话")
        if mode == "publish":
            info = adapter.chats_now([chat]).get(chat)
            if mid or not info or not adapter.U.decoders.is_thread_chat(info):
                raise ValueError("只能在话题群发布话题")
            req.isThreadGroupTopic = True
            return
        if mode not in ("quote", "create_topic", "comment"):
            raise ValueError("无效的飞书回复方式")
        raw = self.message(chat, mid)
        topic_mode = adapter.client.is_thread_chat(chat)
        root = raw_id(raw.get("rootId"))
        if mode == "create_topic":
            M = adapter.U.L.entities.Message
            if topic_mode or raw.get("chatType") == M.P2P:
                raise ValueError("请在普通群消息上创建话题")
            if root or raw.get("threadMessageType") in (M.THREAD_ROOT_MESSAGE, M.THREAD_REPLY_MESSAGE):
                raise ValueError("该消息已属于话题，请进入话题发表评论")
        # PutMessage accepts message ids rather than the separately assigned thread id.
        req.rootId = root or mid
        req.parentId = mid
        if not topic_mode:
            req.isReplyInThread = mode != "quote"

    def reaction(self, mid, key, active):
        if not isinstance(key, str) or not key or len(key) > 256 or not isinstance(active, bool):
            raise ValueError("无效的表情回应")
        method = "reactions.PutReactionRequest" if active else "reactions.DeleteReactionRequest"
        _request(self.adapter, method, {"messageId": mid, "reactionType": key})

    def subscribe(self, tid, active):
        if not isinstance(active, bool):
            raise ValueError("无效的订阅状态")
        _request(self.adapter, "thread.PatchThreadSettingRequest", {
            "setting": {"threadId": tid, "isFollow": active, "isFollowV2": active},
            "updateFields": ["UPDATE_FIELD_IS_FOLLOW"],
        })

    def forward_topic(self, chat, tid, target):
        reply = _request(self.adapter, "thread.PutThreadShareRequest", {
            "channelId": chat, "threadId": tid, "targets": [{"toChat": {"chatId": target}}],
        }, UNCERTAIN_FORWARD)
        self._forward_receipt(reply, target)

    def forward_message(self, chat, mid, raw, target):
        adapter = self.adapter
        cid = adapter.U.ids.generate_request_cid()
        adapter._record_own_cid(cid)
        reply = _request(adapter, "messages.PutTransmitMessageRequest", {
            "messageId": mid, "chatIds": [target], "cids": {target: cid},
            "immutableMessage": {"messageId": mid, "chatId": chat, "messageType": raw["type"]},
        }, UNCERTAIN_FORWARD)
        self._forward_receipt(reply, target)

    @staticmethod
    def _forward_receipt(reply, target):
        denied = reply.get("filePermCheck") or {}
        if denied:
            raise ValueError(denied.get("toast") or "飞书限制了这条消息的转发")
        if not any(raw_id(raw.get("id")) and (str(key) == target or _matches_chat(raw, target))
                   for key, raw in (reply.get("messages") or {}).items()):
            raise ValueError(UNCERTAIN_FORWARD)
