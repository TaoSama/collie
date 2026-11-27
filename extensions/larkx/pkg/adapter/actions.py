"""Interactive Feishu card actions and live calendar RSVP/conflict helpers."""
import json
import logging
import time
from urllib.parse import urlencode, urlparse

import message_view

log = logging.getLogger("larkx")

ACTION_POST, ACTION_GET = 2, 1
CAL_REPLY = {"accept": 1, "decline": 2, "tentative": 3}
CAL_STATUS = {1: "needs_action", 2: "accept", 3: "tentative", 4: "decline", 5: "removed"}
ALLOWED_ACTION_HOSTS = (".feishu.cn", ".larksuite.com", ".larkoffice.com")
CARD_REFRESH_WAITS = (0.35, 0.65, 0.9)


def allowed_action_url(raw):
    try:
        u = urlparse(raw or "")
    except ValueError:
        return False
    host = (u.hostname or "").lower()
    return u.scheme == "https" and any(host.endswith(s) for s in ALLOWED_ACTION_HOSTS)


def primary_calendar_id(adapter):
    """Cached primary calendar id of the signed-in user; "" when unavailable."""
    if adapter.client is None:
        return ""
    with adapter._mu:
        if adapter._primary_cal is not None:
            return adapter._primary_cal
    cal_id = ""
    try:
        resp = adapter.client.api("calendars.GetUserPrimaryCalendarRequest", {}) or {}
        cal_id = str((resp.get("calendar") or {}).get("id") or "")
    except Exception as e:
        log.warning("primary calendar: %s", e)
    if cal_id:
        with adapter._mu:
            adapter._primary_cal = cal_id
    return cal_id


def enrich_calendars(adapter, pairs):
    """Fill live selfAttendeeStatus and conflict flags on calendar MessageViews.

    pairs: list of (raw_decoded_msg, out_message_dict)
    """
    targets = []
    for raw_msg, out in pairs:
        view = out.get("view")
        if not view or view.get("kind") != "calendar":
            continue
        meta = message_view.calendar_meta(raw_msg.get("content_data") or {})
        if meta.get("uid"):
            targets.append((meta, view))
    if not targets or adapter.client is None:
        return
    cal_id = primary_calendar_id(adapter)
    if not cal_id:
        return

    keys = []
    seen = set()
    for meta, _ in targets:
        k = (meta["uid"], meta["original_time"])
        if k not in seen:
            seen.add(k)
            keys.append({"calendarId": cal_id, "uniqueKey": meta["uid"], "originalTime": meta["original_time"]})
    live = {}
    try:
        r = adapter.client.api("calendarevents.MGetCalendarEventsWithKeyRequest", {"keys": keys}) or {}
        for ev in r.get("calendarEvents") or []:
            uk = ev.get("uniqueKey") or ""
            ot = int(ev.get("originalTime") or 0)
            if uk:
                live[(uk, ot)] = ev
                live.setdefault((uk, 0), ev)
    except Exception as e:
        log.warning("calendar status lookup: %s", e)

    spans = []
    for meta, view in targets:
        ev = live.get((meta["uid"], meta["original_time"])) or live.get((meta["uid"], 0))
        if ev:
            cancelled = bool(ev.get("isDeleted")) or ev.get("status") == 3
            st = "cancelled" if cancelled else CAL_STATUS.get(ev.get("selfAttendeeStatus"), view.get("status") or "needs_action")
            view["status"] = st
            if st in ("cancelled", "removed"):
                view.pop("action", None)
            if ev.get("start") and ev.get("end"):
                meta["start_ts"], meta["end_ts"] = int(ev["start"]), int(ev["end"])
        if view.get("status") not in ("cancelled", "removed") and not view.get("all_day"):
            s_ts, e_ts = meta.get("start_ts", 0), meta.get("end_ts", 0)
            if s_ts and e_ts > s_ts:
                spans.append((meta, view, s_ts, e_ts))

    if not spans:
        return
    lo = min(s for _, _, s, _ in spans)
    hi = max(e for _, _, _, e in spans)
    try:
        qr = adapter.client.api("calendarevents.QueryInstanceRequest",
                                {"calendarIds": [cal_id], "startTime": lo, "endTime": hi}) or {}
    except Exception as e:
        log.warning("calendar conflict lookup: %s", e)
        return
    insts = qr.get("calendarEventInstance") or []
    evts = qr.get("events") or {}
    busy = []
    for inst in insts:
        ev = evts.get(inst.get("eventRefId")) or {}
        if ev.get("isFree") or ev.get("isDeleted") or ev.get("status") == 3:
            continue
        if ev.get("selfAttendeeStatus") not in (2, 3):  # ACCEPT or TENTATIVE
            continue
        b, e = int(inst.get("beginTime") or 0), int(inst.get("endTime") or 0)
        if b and e > b:
            busy.append((ev.get("uniqueKey") or "", b, e))
    for meta, view, s_ts, e_ts in spans:
        uid = meta["uid"]
        if any(uk != uid and b < e_ts and e > s_ts for uk, b, e in busy):
            view["conflict"] = True
        else:
            view.pop("conflict", None)


def _pull_message(adapter, chat_id, message_id):
    res = adapter.client.api("messages.PullMessagesByIdsRequest", {"messageIds": [str(message_id)]}) or {}
    raw = (res.get("messages") or {}).get(str(message_id))
    if not raw:
        raise ValueError("找不到对应的飞书消息")
    if chat_id and str(raw.get("chatId") or raw.get("channelId") or "") not in ("", str(chat_id)):
        raise ValueError("消息不属于当前会话")
    return raw


def _render_message(adapter, raw, chat_id):
    msg = message_view.decode(adapter.U, raw, chat_id)
    sender = str(msg.get("from_id") or "")
    view, preview = message_view.describe(adapter.U, msg, emojis=adapter.emojis, me=adapter.me)
    text = preview
    if msg["msg_type"] in (message_view.TEXT, message_view.POST):
        text = adapter._written(sender, msg.get("content_data") or {})[0]
    out = dict(
        id=str(msg["msg_id"]), chat_id=str(msg["chat_id"] or chat_id), sender=sender,
        text=text, preview=preview, sender_name=adapter.user_name(sender, chat_id),
        owner=bool(adapter.me) and sender == adapter.me,
        at=adapter.history.message_at(msg), view=view,
    )
    enrich_calendars(adapter, [(msg, out)])
    return out


def execute(adapter, chat_id, message_id, action_id, kind="", value="", params=None):
    """Execute a card callback action or calendar RSVP as the signed-in user."""
    chat_id, message_id, action_id = str(chat_id or ""), str(message_id or ""), str(action_id or "")
    if not chat_id or not message_id or not action_id:
        raise ValueError("缺少操作参数")
    if kind == "calendar" or action_id.startswith("cal:"):
        return _reply_calendar(adapter, chat_id, message_id, value)
    return _trigger_card(adapter, chat_id, message_id, action_id, value, params or {})


def _reply_calendar(adapter, chat_id, message_id, value):
    reply_type = CAL_REPLY.get(str(value or "").lower())
    if not reply_type:
        raise ValueError("无效的日程回复状态")
    raw = _pull_message(adapter, chat_id, message_id)
    msg = message_view.decode(adapter.U, raw, chat_id)
    meta = message_view.calendar_meta(msg.get("content_data") or {})
    uid = meta.get("uid") or ""
    if not uid:
        raise ValueError("日程卡片缺少日程标识")
    cal_id = primary_calendar_id(adapter)
    if not cal_id:
        raise ValueError("无法读取本人主日历")
    adapter.client.api("calendarevents.ReplyCalendarEventInvitationRequest", {
        "calendarEventKey": {
            "calendarId": cal_id,
            "uniqueKey": uid,
            "originalTime": int(meta.get("original_time") or 0),
        },
        "replyType": reply_type,
    })
    adapter.history.invalidate(chat_id)
    updated = _render_message(adapter, raw, chat_id)
    if updated.get("view") and updated["view"].get("kind") == "calendar":
        updated["view"]["status"] = str(value).lower()
    return {"toast": "", "message": updated}


def _trigger_card(adapter, chat_id, message_id, action_id, value, extra_params):
    import requests

    raw = _pull_message(adapter, chat_id, message_id)
    before_content = raw.get("content") or b""
    msg = message_view.decode(adapter.U, raw, chat_id)
    if msg["msg_type"] != message_view.CARD:
        raise ValueError("该消息不是互动卡片")
    data = msg.get("content_data") or {}
    act = (data.get("actions") or {}).get(action_id)
    if not isinstance(act, dict):
        raise ValueError("该操作已失效或已处理")
    method = act.get("method")
    url = str(act.get("url") or "")
    if method not in (ACTION_POST, ACTION_GET) or not allowed_action_url(url):
        raise ValueError("不支持的卡片回调地址")

    req_params = act.get("requiredParameters") or {}
    base_params = act.get("parameters") or {}
    common = {}
    if req_params.get("needUserId", True) and adapter.me:
        common["uid"] = str(adapter.me)
    if req_params.get("needMessageId", True):
        common["mid"] = message_id
    uinfo = base_params.get("userInfo") or {}
    if uinfo.get("userOpenId"):
        common["ouid"] = str(uinfo["userOpenId"])
    minfo = base_params.get("messageInfo") or {}
    if minfo.get("messageOpenId"):
        common["omid"] = str(minfo["messageOpenId"])
    if data.get("cardId"):
        common["card_id"] = str(data["cardId"])

    action_body = dict(base_params.get("parameters") or {})
    if value:
        action_body["selected_option"] = str(value)
    for k, v in (extra_params or {}).items():
        if isinstance(k, str) and k:
            action_body[k] = str(v)

    auth = adapter.client.auth
    headers = {
        "content-type": "application/json",
        "locale": auth.region.locale,
        "origin": auth.region.web_origin,
        "referer": auth.region.web_origin + "/",
        "x-csrf-token": auth.csrf_token or auth.cookies.get("swp_csrf_token", ""),
        "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
    }
    if method == ACTION_POST:
        resp = requests.post(url, headers=headers, cookies=auth.cookies,
                             json={"common": common, "action": action_body}, timeout=12)
    else:
        qs = urlencode({**common, **action_body})
        sep = "&" if "?" in url else "?"
        resp = requests.get(url + (sep + qs if qs else ""), headers=headers, cookies=auth.cookies, timeout=12)
    resp.raise_for_status()

    toast = ""
    try:
        j = resp.json()
    except ValueError:
        j = {}
    if isinstance(j, dict):
        t = j.get("toast") or (j.get("data") or {}).get("toast") if isinstance(j.get("data"), dict) else j.get("toast")
        if isinstance(t, dict) and t.get("content"):
            toast = str(t["content"])
        code = j.get("code")
        if code not in (None, 0):
            if toast:
                pass
            elif code == 100 or code >= 200000:
                msg_text = str(j.get("message") or j.get("msg") or "")
                try:
                    inner = json.loads(msg_text)
                    if isinstance(inner, dict) and inner.get("error_msg"):
                        msg_text = str(inner["error_msg"])
                except ValueError:
                    pass
                raise ValueError(msg_text or ("飞书卡片操作失败（code %s）" % code))

    adapter.history.invalidate(chat_id)
    latest = raw
    for wait in CARD_REFRESH_WAITS:
        time.sleep(wait)
        try:
            pulled = _pull_message(adapter, chat_id, message_id)
        except Exception:
            break
        latest = pulled
        if (pulled.get("content") or b"") != before_content:
            break
    adapter.history.invalidate(chat_id)
    return {"toast": toast, "message": _render_message(adapter, latest, chat_id)}
