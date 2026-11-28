"""Native inbox operations on the logged-in Feishu Web Gateway."""

from collections import OrderedDict
import logging
import threading
import time
import unicodedata

import peer

log = logging.getLogger("larkx")

RECENT_EMOJI_TTL = 60
REACTION_NAMES_MAX = 2
REACTION_BATCH = 50
INBOX_READ_TTL = 10
INBOX_READ_MAX = 64
TOPIC_WINDOW = 50
TOPIC_PREVIEW_REPLIES = 3
UNCERTAIN_FORWARD = "没有收到转发回执，消息可能已经转发，请到飞书确认"
EMOJI_KEY_MAX = 256


def raw_id(value):
    value = str(value or "")
    return value if value.isascii() and value.isdecimal() and value != "0" and len(value) <= 20 else ""


def _count(value):
    try:
        return max(0, int(value or 0))
    except (TypeError, ValueError):
        return 0


def require_client(adapter):
    client = adapter.client
    if client is None:
        raise peer.Error("not_signed_in", "飞书尚未登录，请在接入页检查连接状态")
    return client


def _request(adapter, method, data, forwarding=False):
    """Decode the status before the payload so a gateway refusal can't look like success."""
    import requests
    from google.protobuf.message import DecodeError
    client = require_client(adapter)
    U = adapter.U
    gateway = U.gateway
    cmd, req, response = gateway.resolve_cmd(method)
    pkt = gateway.build_packet(cmd, U.ids.generate_long_request_id(), req_msg=gateway.request_from_dict(req, data))
    uncertain = (peer.Error("uncertain", UNCERTAIN_FORWARD) if forwarding
                 else ValueError("飞书网关没有确认操作"))
    try:
        raw = client._gateway_post(pkt)
    except requests.exceptions.HTTPError as e:
        status = e.response.status_code if e.response is not None else 0
        if 400 <= status < 500:
            if forwarding and status == 403:
                raise ValueError("飞书拒绝转发，请检查这条消息和目标会话的转发权限（HTTP 403）") from e
            raise ValueError("飞书拒绝操作（HTTP %s）" % status) from e
        raise uncertain from e
    except requests.exceptions.RequestException as e:
        raise uncertain from e
    if not raw:
        raise uncertain
    try:
        reply = U.P.Packet.FromString(raw)
        if reply.status:
            if forwarding and reply.status == 403:
                raise ValueError("飞书拒绝转发，请检查这条消息和目标会话的转发权限（状态 403）")
            raise ValueError("飞书拒绝操作（状态 %s）" % reply.status)
        return gateway.response_to_dict(response, reply.payload) if reply.payload else {}
    except DecodeError as e:
        raise uncertain from e


def _matches_chat(raw, chat):
    ids = [str(raw[key]) for key in ("chatId", "channelId") if raw.get(key) not in (None, "", 0, "0")]
    return bool(ids) and all(value == chat for value in ids)


class Inbox:
    def __init__(self, adapter):
        self.adapter = adapter
        self.lock = threading.Lock()
        self.mru = (0, [])
        self.reads = OrderedDict()
        self.read_generation = 0

    def invalidate(self, chat):
        with self.lock:
            self.read_generation += 1
            for key in list(self.reads):
                if key[1] == chat:
                    del self.reads[key]

    def _cached(self, key, read):
        with self.lock:
            hit = self.reads.get(key)
            if hit and hit[0] > time.monotonic():
                self.reads.move_to_end(key)
                return hit[1]
            generation = self.read_generation
        found = read()
        with self.lock:
            # A write that occurred during this read may have already invalidated it.
            if generation == self.read_generation:
                self.reads[key] = (time.monotonic() + INBOX_READ_TTL, found)
                self.reads.move_to_end(key)
                while len(self.reads) > INBOX_READ_MAX:
                    self.reads.popitem(last=False)
        return found

    def recent(self, chat):
        adapter = self.adapter
        require_client(adapter)
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
            out = require_client(self.adapter).api("reactions.PullUserMRUReactionsRequest", {}) or {}
            keys = list(dict.fromkeys(key for key in out.get("userMruReactions", []) if key))
        except peer.Error:
            raise
        except Exception as e:
            log.warning("recent emoji: %s", e)
            keys = previous
        with self.lock:
            self.mru = (time.monotonic() + RECENT_EMOJI_TTL, keys)
        return list(keys)

    def enrich_reactions(self, chat, messages):
        by_id = {row["id"]: row for row in messages if raw_id(row["id"])
                 and (row.get("view") or {}).get("kind") not in ("system", "recalled")}
        ids = sorted(by_id)
        for i in range(0, len(ids), REACTION_BATCH):
            batch = ids[i:i + REACTION_BATCH]
            try:
                rows = self._cached(("reactions", chat, tuple(batch)), lambda: self._reactions(chat, batch))
                for mid, reactions in rows.items():
                    by_id[mid]["reactions"] = reactions
            except peer.Error:
                raise
            except Exception as e:
                log.warning("reactions in %s: %s", chat, e)

    def _reactions(self, chat, ids):
        adapter = self.adapter
        response = require_client(adapter).api("reactions.PullReactionsRequest", {
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
        reactions = {mid: [] for mid in ids}
        with_names = []
        for entry in response.get("messageReactions") or []:
            mid = str(entry.get("messageId"))
            if mid not in reactions:
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
                    with_names.append((row, ids_seen))
                reactions[mid].append(row)
        names = self._reaction_names(chat, {uid for _, uids in with_names for uid in uids})
        for row, ids_seen in with_names:
            display = [names.get(uid, "") for uid in ids_seen]
            if all(display):
                row["names"] = display
        return reactions

    def _reaction_names(self, chat, users):
        adapter = self.adapter
        names = {uid: ("你" if uid == adapter.me else adapter.names.peek("user:" + uid)) for uid in users}
        want = sorted(uid for uid, name in names.items() if not name and raw_id(uid))
        for i in range(0, len(want), REACTION_BATCH):
            batch = want[i:i + REACTION_BATCH]
            try:
                response = require_client(adapter).api("chats.PullChatChattersByIDsRequest", {
                    "chatId": chat, "chatterIds": [int(uid) for uid in batch], "needChatters": True,
                }) or {}
            except peer.Error:
                raise
            except Exception as e:
                log.warning("reaction names in %s: %s", chat, e)
                continue
            for uid, person in (response.get("chatters") or {}).items():
                uid = str(uid)
                name = person.get("localizedName") or person.get("name")
                if uid in batch and name:
                    names[uid] = adapter.names.get("user:" + uid, lambda: name)
        return names

    def message(self, chat, mid):
        if not raw_id(chat) or not raw_id(mid):
            raise ValueError("无效的飞书消息")
        reply = require_client(self.adapter).api("messages.PullMessagesByIdsRequest", {"messageIds": [mid]}) or {}
        raw = (reply.get("messages") or {}).get(mid)
        M = self.adapter.U.L.entities.Message
        if (not raw or str(raw.get("id") or "") != mid or not _matches_chat(raw, chat)
                or raw.get("status") == M.DELETED or raw.get("type") in (None, M.SYSTEM)):
            raise ValueError("该消息不可操作或不属于当前会话")
        return raw

    def topic(self, chat, tid, mid=""):
        if not raw_id(chat) or not raw_id(tid):
            raise ValueError("无效的飞书话题")
        reply = require_client(self.adapter).api("thread.PullThreadsByIDsRequest", {
            "threadIds": [tid], "lastMessageCount": TOPIC_PREVIEW_REPLIES,
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
            response = require_client(self.adapter).api("thread.PullThreadMessagesByPositionsRequest", {
                "threadId": tid, "positions": positions, "channelId": chat, "channelType": 1,
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
        require_client(self.adapter)
        chat, mid, tid = (params.get(key) or "" for key in ("chat_id", "message_id", "thread_id"))
        action = params.get("action")
        if action == "topic":
            return {"messages": self._cached(("comments", chat, tid, mid), lambda: self.comments(chat, tid, mid))}
        if action == "reaction":
            self.message(chat, mid)
            try:
                self.reaction(mid, params.get("key"), params.get("active"))
            finally:
                self.invalidate(chat)
        elif action == "subscribe":
            self.topic(chat, tid, mid)
            try:
                self.subscribe(tid, params.get("active"))
            finally:
                self.adapter.history.invalidate(chat)
                self.invalidate(chat)
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
                self.invalidate(target)
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
        topic_mode = require_client(adapter).is_thread_chat(chat)
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
        if (not isinstance(key, str) or not 1 <= len(key) <= EMOJI_KEY_MAX or not isinstance(active, bool)
                or any(c.isspace() or unicodedata.category(c) in ("Cc", "Cs") for c in key)):
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
        adapter = self.adapter
        chats = adapter.chats_now([chat, target])
        if chat not in chats or target not in chats:
            raise ValueError("飞书无法读取原会话或目标会话，请检查会话权限")
        is_topic_group = adapter.U.decoders.is_thread_chat
        if not is_topic_group(chats[chat]) and is_topic_group(chats[target]):
            raise ValueError("普通群的话题暂时无法转发到话题群，请选择普通群或私聊")
        reply = _request(self.adapter, "thread.PutThreadShareRequest", {
            "channelId": chat, "threadId": tid, "targets": [{"toChat": {"chatId": target}}],
        }, forwarding=True)
        self._forward_receipt(reply, target)

    def forward_message(self, chat, mid, raw, target):
        adapter = self.adapter
        cid = adapter.U.ids.generate_request_cid()
        adapter._record_own_cid(cid)
        try:
            reply = _request(adapter, "messages.PutTransmitMessageRequest", {
                "messageId": mid, "chatIds": [target], "cids": {target: cid},
                "immutableMessage": {"messageId": mid, "chatId": chat, "messageType": raw["type"]},
            }, forwarding=True)
            self._forward_receipt(reply, target)
        except ValueError as e:
            if raw["type"] == adapter.U.L.entities.Message.IMAGE:
                raise ValueError("图片转发失败。%s" % e) from e
            raise

    @staticmethod
    def _forward_receipt(reply, target):
        denied = reply.get("filePermCheck") or {}
        if denied:
            raise ValueError(denied.get("toast") or "飞书限制了这条消息的转发")
        for key, raw in (reply.get("messages") or {}).items():
            if raw_id(raw.get("id")) and (str(key) == target or _matches_chat(raw, target)):
                return
        for info in (reply.get("threadMessages") or {}).values():
            raw = info.get("message") or {}
            if str(info.get("chatId") or "") != target and not _matches_chat(raw, target):
                continue
            if info.get("failed"):
                raise ValueError("飞书拒绝了向这个会话转发消息")
            if raw_id(raw.get("id")):
                return
        for thread in (reply.get("threads") or {}).values():
            if (str(thread.get("channelId") or "") == target
                    and raw_id(thread.get("id")) and raw_id(thread.get("rootMessageId"))):
                return
        raise peer.Error("uncertain", UNCERTAIN_FORWARD)
