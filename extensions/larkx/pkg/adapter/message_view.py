"""Feishu message content as a bounded tree of display elements, without HTML."""
import datetime as dt
import json
import logging
import re

import richtext

log = logging.getLogger("larkx")
POST, FILE, TEXT, IMAGE, SYSTEM, STICKER, CALENDAR, CARD, SHARED_EVENT, GENERAL_EVENT = 2, 3, 4, 5, 6, 10, 12, 14, 16, 18
VIEW_NODES = 320
VIEW_DEPTH = 16
BODY_MOST = 36000
TEXT_MOST = 12000
MEDIA_KEY = re.compile(r"[A-Za-z0-9_-]{1,256}")
INLINE_KINDS = ("text", "emoji", "mention", "link", "code")
CONTROL_KINDS = ("button", "select", "overflow", "date", "time", "datetime")
CAL_STATUS = {1: "needs_action", 2: "accept", 3: "tentative", 4: "decline", 5: "removed"}
LABELS = {POST: "富文本", FILE: "文件", IMAGE: "图片", SYSTEM: "系统消息", STICKER: "表情包", CARD: "卡片",
          CALENDAR: "日历", SHARED_EVENT: "日历", GENERAL_EVENT: "日历"}


def obj(value):
    return value if isinstance(value, dict) else {}


def seq(value):
    return value if isinstance(value, list) else []


def content(value):
    for _ in range(6):
        if isinstance(value, str):
            return value.strip()[:TEXT_MOST]
        data = obj(value)
        value = (data.get("content") or data.get("innerText") or data.get("inner_text") or data.get("text")
                 or data.get("zh_cn") or data.get("zh-CN") or data.get("en_us") or data.get("en-US"))
        if value is None:
            break
    return ""


def json_obj(value):
    if isinstance(value, (str, bytes)) and len(value) <= 1 << 20:
        try:
            return obj(json.loads(value))
        except (ValueError, RecursionError):
            log.debug("message content is not a JSON object")
    return obj(value)


def media_key(value):
    return value if isinstance(value, str) and MEDIA_KEY.fullmatch(value) else ""


def node(kind, text="", children=None, url="", key="", tone="", action="", value="", placeholder="", name="",
         disabled=False, options=None):
    return {k: v for k, v in dict(
        kind=kind, text=text, children=children, url=url, key=key, tone=tone,
        action=action, value=value, placeholder=placeholder, name=name, disabled=disabled, options=options,
    ).items() if v}


def words(nodes):
    """The first readable paragraph, excluding decorative controls."""
    for n in nodes:
        if n["kind"] in ("image", "button", "actions", "rule"):
            continue
        value = n.get("text") or words(n.get("children") or [])
        if value:
            return value
    return ""


def preview_text(text):
    text = re.sub(r"!\[([^\]]*)\]\([^)]+\)|\[([^\]]+)\]\([^)]+\)",
                  lambda m: m.group(1) or m.group(2) or "", text)
    text = re.sub(r"<[^<>]*>", "", text)
    text = re.sub(r"(?m)^\s*[#>]+\s*|\*\*|__|`", "", text)
    return " ".join(text.split())[:500]


def link_of(data):
    multi = obj(data.get("multi_url") or data.get("multiUrl"))
    for behavior in seq(data.get("behaviors")):
        if obj(behavior).get("type") == "open_url":
            multi = behavior
            break
    return content(data.get("url") or data.get("href") or multi.get("pc_url") or multi.get("url")
                   or multi.get("default_url") or multi.get("defaultUrl"))


class Tree:
    def __init__(self, U, emojis=None, actions=None, sender_cannot_click=False):
        self.E = U.L.entities.RichTextElement
        self.left = VIEW_NODES
        self.characters = BODY_MOST
        self._emojis = emojis
        self._emoji_map = None
        self.actions = obj(actions)
        self.sender_cannot_click = bool(sender_cannot_click)

    def _action_target(self, action_id):
        if not action_id or self.sender_cannot_click:
            return "", ""
        act = obj(self.actions.get(action_id))
        if not act:
            return "", ""
        method = act.get("method")
        if method == 4:  # OPEN_URL
            ou = obj(act.get("openUrl"))
            return "url", content(ou.get("pcUrl") or ou.get("url") or act.get("url"))
        if method in (1, 2) and act.get("url"):  # GET / POST callback
            return "action", action_id
        return "", ""

    def emoji(self, key):
        if self._emoji_map is None:
            self._emoji_map = (self._emojis() if callable(self._emojis) else self._emojis) or {}
        hit = self._emoji_map.get(key)
        if isinstance(hit, (tuple, list)) and len(hit) >= 2:
            return hit[0], hit[1]
        return "", ""

    def take(self, text):
        text = text[:self.characters]
        self.characters -= len(text)
        return text

    def markdown(self, text, images=None):
        # Markdown images in cards may name a message resource rather than a URL.
        text = self.take(text)
        parts, start = [], 0
        for m in re.finditer(r"!\[([^\]\n]*)\]\(([^()\s]+)\)", text):
            ref = m.group(2)
            resource = obj(obj(images).get(ref))
            key = media_key(resource.get("token") or (ref if ref.startswith("img_") else ""))
            if not key:
                continue
            if start < m.start():
                parts.append(node("markdown", text[start:m.start()]))
            parts.append(node("image", m.group(1), key=key))
            start = m.end()
            if len(parts) >= VIEW_NODES // 4:
                break
        if start < len(text):
            parts.append(node("markdown", text[start:]))
        return parts

    def card(self, values, depth=0):
        out = []
        for value in seq(values):
            if self.left <= 0 or depth > VIEW_DEPTH:
                break
            self.left -= 1
            n = obj(value)
            tag = n.get("tag")
            text_obj = n.get("text")
            text = content(n.get("content") or text_obj)
            children = self.card(n.get("elements") or n.get("columns") or n.get("actions"), depth + 1)
            if tag == "markdown" or tag == "lark_md":
                out.extend(self.markdown(text))
            elif tag in ("img", "image"):
                out.append(node("image", content(n.get("alt")), key=media_key(n.get("img_key") or n.get("image_key"))))
            elif tag in ("button", "overflow"):
                if tag == "overflow":
                    opts = []
                    for o in seq(n.get("options"))[:20]:
                        oo = obj(o)
                        oaid = content(oo.get("option_action_id") or oo.get("action_id") or n.get("action_id") or n.get("element_id"))
                        kind_t, target = self._action_target(oaid)
                        ourl = link_of(oo) or (target if kind_t == "url" else "")
                        oact = target if kind_t == "action" and not ourl else ""
                        opts.append({k: v for k, v in dict(
                            text=content(oo.get("text")) or content(oo.get("value")),
                            value=content(oo.get("value")), url=ourl, action=oact,
                        ).items() if v})
                    out.append(node("overflow", options=opts))
                else:
                    disabled = bool(n.get("disabled")) or self.sender_cannot_click
                    url = "" if disabled else link_of(n)
                    aid = content(n.get("action_id") or n.get("element_id"))
                    if not aid and not url:
                        for b in seq(n.get("behaviors")):
                            if obj(b).get("type") == "callback":
                                aid = content(obj(b).get("action_id") or n.get("name"))
                                break
                    kind_t, target = ("", "") if disabled or url else self._action_target(aid)
                    if kind_t == "url":
                        url = target
                    action = target if kind_t == "action" else ""
                    fname = "submit" if n.get("form_action_type") == "submit" else ""
                    out.append(node("button", text, url=url, action=action, name=fname, tone=content(n.get("type")),
                                    disabled=disabled or (not url and not action)))
            elif tag in ("select_static", "select_person"):
                ph = content(n.get("placeholder")) or ("选择成员" if tag == "select_person" else "请选择")
                if tag == "select_person":
                    out.append(node("select", placeholder=ph, tone="person", disabled=True))
                else:
                    aid = content(n.get("action_id") or n.get("element_id"))
                    kind_t, target = self._action_target(aid)
                    action = target if kind_t == "action" and not n.get("disabled") else ""
                    opts = [{"text": content(obj(o).get("text")) or content(obj(o).get("value")),
                             "value": content(obj(o).get("value"))} for o in seq(n.get("options"))[:50]]
                    out.append(node("select", placeholder=ph, value=content(n.get("initial_option")),
                                    name=content(n.get("name")), action=action, options=opts,
                                    disabled=bool(n.get("disabled")) or not action))
            elif tag in ("date_picker", "picker_time", "picker_datetime"):
                pkind = {"date_picker": "date", "picker_time": "time", "picker_datetime": "datetime"}[tag]
                aid = content(n.get("action_id") or n.get("element_id"))
                kind_t, target = self._action_target(aid)
                action = target if kind_t == "action" and not n.get("disabled") else ""
                init = content(n.get("initial_date") or n.get("initial_time") or n.get("initial_datetime"))
                out.append(node(pkind, placeholder=content(n.get("placeholder")), value=init,
                                name=content(n.get("name")), action=action, disabled=bool(n.get("disabled")) or not action))
            elif tag == "input":
                out.append(node("input", text=content(n.get("label")), placeholder=content(n.get("placeholder")),
                                value=content(n.get("default_value")), name=content(n.get("name")),
                                disabled=bool(n.get("disabled"))))
            elif tag == "form":
                out.append(node("form", name=content(n.get("name")), children=children))
            elif tag in ("hr", "divider"):
                out.append(node("rule"))
            elif tag in ("column_set", "column", "action", "note"):
                out.append(node({"column_set": "columns", "column": "group", "action": "actions", "note": "note"}[tag],
                                children=children))
            elif tag == "div":
                if text:
                    out.extend(self.card([obj(text_obj)], depth + 1) if obj(text_obj).get("tag") else [node("text", self.take(text))])
                fields = seq(n.get("fields"))
                if fields:
                    out.append(node("fields", children=self.card_fields(fields, depth)))
                if obj(n.get("extra")):
                    out.extend(self.card([n["extra"]], depth + 1))
                out.extend(children)
            else:
                if text:
                    out.append(node("text", self.take(text)))
                out.extend(children)
        return out

    def card_fields(self, fields, depth):
        result = []
        for value in fields[:VIEW_NODES]:
            field = obj(value)
            text = field.get("text")
            elements = self.card([text] if isinstance(text, dict) else [{"content": text}], depth + 1)
            if elements:
                result.append(node("field", children=elements, tone="short" if field.get("is_short") else ""))
        return result

    def rich(self, data):
        elements = obj(obj(data.get("elements")).get("dictionary"))
        roots = data.get("elementIds") or data.get("element_ids")
        if not roots:
            descendants = {k for e in elements.values() for k in seq(obj(e).get("childIds")) if isinstance(k, str)}
            roots = [key for key in elements if key not in descendants]

        def walk(ids, depth=0):
            result = []
            for eid in seq(ids):
                if self.left <= 0 or depth > VIEW_DEPTH:
                    break
                self.left -= 1
                el = obj(elements.get(eid)) if isinstance(eid, str) else {}
                kids = walk(el.get("childIds"), depth + 1)
                result.extend(self.rich_element(el, kids))
            return result

        out = walk(roots) or ([node("text", content(data))] if content(data) else [])
        return _group_inline(out)

    def rich_element(self, el, children):
        from google.protobuf.message import DecodeError

        E = self.E
        raw = el.get("property") or b""

        def prop(cls):
            value = cls()
            if isinstance(raw, bytes):
                try:
                    value.ParseFromString(raw)
                except DecodeError:
                    log.debug("could not decode %s", cls.__name__)
            return value

        tag = el.get("tag")
        if isinstance(tag, str):
            tag = E.Tag.Value(tag) if tag in E.Tag.keys() else 0
        if tag in (E.TEXT, E.B, E.I, E.U):
            text = self.take(prop(E.TextProperty).content[:TEXT_MOST])
            style = obj(el.get("style"))
            marks = (tag == E.B or style.get("fontWeight") == "bold",
                     tag == E.I or style.get("fontStyle") == "italic",
                     tag == E.U or style.get("textDecoration") == "underline",
                     style.get("textDecoration") == "line-through")
            tone = " ".join(mark for mark, enabled in zip(("bold", "italic", "underline", "strike"), marks) if enabled)
            return [node("text", text, children, tone=tone)] if text or children else []
        if tag == E.IMG:
            image = prop(E.ImageProperty)
            return [node("image", image.alt[:500], key=media_key(image.originKey or image.token))]
        if tag == E.A:
            a = prop(E.AnchorProperty)
            return [node("link", content(a.textContent or a.content or a.href), children, url=content(a.pcHref or a.href))]
        if tag == E.LINK:
            lp = prop(E.LinkProperty)
            return [node("link", children=children, url=content(lp.pcUrl or lp.url))]
        if tag == E.AT:
            return [node("mention", content(prop(E.AtProperty).content) or "@")]
        if tag == E.EMOTION:
            label, url = self.emoji(prop(E.EmotionProperty).key)
            return [node("emoji", label or "表情", url=url)]
        if tag == E.MARKDOWN:
            md = prop(E.MarkdownProperty)
            images = {k: {"token": v.originKey or v.token} for k, v in md.images.items()}
            return self.markdown(md.content[:TEXT_MOST], images)
        if tag == E.CODE_BLOCK_V2:
            cb = prop(E.CodeBlockV2Property)
            return [node("code-block", self.take("\n".join("".join(c.content for c in line.contents) for line in cb.contents)[:TEXT_MOST]))]
        if tag == E.CODE_BLOCK:
            return [node("code-block", self.take(prop(E.CodeBlockProperty).content[:TEXT_MOST]))]
        if tag == E.HR:
            return [node("rule")]
        if tag == E.BUTTON:
            bp = prop(E.ButtonProperty)
            tone = bp.mode if bp.mode in ("primary", "danger") else ""
            disabled = bool(bp.disable) or self.sender_cannot_click
            url = next((n["url"] for n in children if n.get("url")), "")
            action = ""
            if not disabled and not url:
                kind_t, target = self._action_target(bp.actionId)
                if kind_t == "url":
                    url = target
                elif kind_t == "action":
                    action = target
            return [node("button", words(children), url=url, action=action, tone=tone,
                         disabled=disabled or (not url and not action))]
        if tag == E.SELECTMENU:
            sp = prop(E.SelectMenuProperty)
            ph = sp.placeHolder or ("选择成员" if sp.type == 2 else "请选择")
            if sp.type == 2:  # PERSON
                return [node("select", placeholder=ph, tone="person", disabled=True)]
            kind_t, target = self._action_target(sp.actionId)
            action = target if kind_t == "action" else ""
            opts = [{"text": o.text or o.value, "value": o.value} for o in sp.options[:50]]
            return [node("select", placeholder=ph, value=sp.initialOption, action=action,
                         options=opts, disabled=not action)]
        if tag == E.OVERFLOWMENU:
            op = prop(E.OverflowMenuProperty)
            opts = []
            for o in op.options[:20]:
                oaid = o.optionActionId or op.actionId
                ourl = content(o.url.pcUrl or o.url.url) if o.HasField("url") else ""
                oact = ""
                if not ourl:
                    kind_t, target = self._action_target(oaid)
                    if kind_t == "url":
                        ourl = target
                    elif kind_t == "action":
                        oact = target
                if o.text or o.value:
                    opts.append({k: v for k, v in dict(
                        text=o.text or o.value, value=o.value, url=ourl, action=oact,
                    ).items() if v})
            return [node("overflow", options=opts)] if opts else []
        if tag in (E.DATEPICKER, E.TIMEPICKER, E.DATETIMEPICKER):
            pkind, cls, init_field = {
                E.DATEPICKER: ("date", E.DatePickerProperty, "initialDate"),
                E.TIMEPICKER: ("time", E.TimePickerProperty, "initialTime"),
                E.DATETIMEPICKER: ("datetime", E.DatetimePickerProperty, "initialDatetime"),
            }[tag]
            dp = prop(cls)
            kind_t, target = self._action_target(dp.actionId)
            action = target if kind_t == "action" else ""
            return [node(pkind, placeholder=dp.placeHolder, value=getattr(dp, init_field, ""),
                         action=action, disabled=not action)]
        if tag == E.PROGRESS_SELECT_OPTION:
            pp = prop(E.ProgressSelectOptionProperty)
            kind_t, target = self._action_target(pp.actionId)
            action = target if kind_t == "action" and not pp.disable else ""
            label = pp.content or pp.optionCase or pp.actionParamValue
            if pp.numberOfTotal > 0:
                label = "%s (%d/%d)" % (label, pp.numberOfSelected, pp.numberOfTotal)
            return [node("button", label, action=action, name=pp.actionParamName, value=pp.actionParamValue,
                         tone="primary" if pp.selected else "", disabled=bool(pp.disable) or not action)]
        if tag == E.P and children and all(n.get("kind") in CONTROL_KINDS for n in children):
            return [node("actions", children=children)]
        container = {E.P: "paragraph", E.QUOTE: "quote", E.CODE: "code", E.H1: "heading", E.H2: "heading",
                     E.H3: "heading", E.UL: "list", E.OL: "ordered", E.LI: "item", E.COLUMNS: "columns",
                     E.COLUMN: "group", E.TABLE: "table", E.TABLE_ROW: "row", E.TABLE_CELL: "cell"}.get(tag)
        if container and children:
            return [node(container, children=children)]
        return children


def localized_card(data):
    # Web clients can receive either the JSON card or the equivalent open-card bytes.
    card = json_obj(data.get("jsonCard")) or json_obj(data.get("openCardContent"))
    if isinstance(card.get("card"), dict):
        card = card["card"]
    return card


def event_time(value):
    if isinstance(value, dict):
        value = value.get("timestamp") or value.get("date")
    if not value:
        return ""
    try:
        if str(value).isdigit():
            stamp = int(value)
            return dt.datetime.fromtimestamp(stamp / 1000 if stamp >= 10 ** 12 else stamp, dt.timezone.utc).isoformat()
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).isoformat()
    except (ValueError, AttributeError, TypeError, OverflowError, OSError):
        return ""


def _ts_int(value):
    if isinstance(value, dict):
        value = value.get("timestamp")
    if isinstance(value, (int, float)) and value:
        return int(value / 1000) if value >= 10 ** 12 else int(value)
    if isinstance(value, str) and value.isdigit():
        n = int(value)
        return int(n / 1000) if n >= 10 ** 12 else n
    return 0


def calendar_meta(data):
    """Extract normalized fields and RSVP lookup identity across type 12/16/18 calendar messages."""
    cal_json = json_obj(data.get("calendar"))
    rsvp_info = obj(data.get("rsvpCardInfo"))
    event = cal_json or obj(data.get("eventCard") or rsvp_info.get("basicEventInfo") or data.get("calendarEvent"))
    invite_title = ""
    if cal_json:
        t_els = obj(obj(cal_json.get("title")).get("elements"))
        invite_title = content(obj(t_els.get("title")).get("property")) or content(cal_json.get("title"))
    summary = content(event.get("summary") or (event.get("title") if isinstance(event.get("title"), str) else ""))
    title = summary or invite_title or "日程"
    subtitle = invite_title if invite_title and invite_title != title else ""

    raw_start = event.get("startTime") or event.get("start_time") or event.get("start")
    raw_end = event.get("endTime") or event.get("end_time") or event.get("end")
    location = content(event.get("location") or obj(next(iter(seq(event.get("locations"))), {})).get("name"))
    if not location and cal_json:
        c_els = obj(obj(cal_json.get("contents")).get("elements"))
        location = content(obj(c_els.get("meeting_room_content")).get("property"))
    desc = content(event.get("description") or event.get("desc"))
    uid = content(event.get("uniqueKey") or event.get("uid") or event.get("key"))
    original_time = _ts_int(event.get("originalTime") or event.get("original_time"))
    self_status = event.get("selfAttendeeStatus")
    if self_status is None:
        self_status = event.get("self_attendee_status")
    cancelled = (bool(event.get("isDeleted")) or event.get("status") == 3
                 or (bool(cal_json) and event.get("type") == 5)
                 or rsvp_info.get("cardStatus") == 2)
    status = "cancelled" if cancelled else CAL_STATUS.get(self_status, "")
    return dict(
        title=title, subtitle=subtitle, start=event_time(raw_start), end=event_time(raw_end),
        start_ts=_ts_int(raw_start), end_ts=_ts_int(raw_end),
        all_day=bool(event.get("isAllDay") or event.get("is_all_day")),
        zone=event.get("startTimezone") or event.get("start_timezone") or "Asia/Shanghai",
        url=content(event.get("url")), location=location, desc=desc,
        uid=uid, original_time=original_time, status=status, cancelled=cancelled,
    )


def calendar(data):
    meta = calendar_meta(data)
    view = dict(kind="calendar", title=meta["title"], start=meta["start"], end=meta["end"],
                all_day=meta["all_day"], url=meta["url"],
                nodes=[node("note", s) for s in (meta["location"], meta["desc"]) if s])
    if meta["subtitle"]:
        view["subtitle"] = meta["subtitle"]
    if meta["status"]:
        view["status"] = meta["status"]
    if meta["uid"] and not meta["cancelled"] and meta["status"] != "removed":
        view["action"] = "cal:%s:%d" % (meta["uid"], meta["original_time"])
    # A stable fallback for stored previews. The detail displays in the browser's timezone.
    day = ""
    if meta["start"]:
        when = dt.datetime.fromisoformat(meta["start"])
        try:
            from zoneinfo import ZoneInfo, ZoneInfoNotFoundError
            when = when.astimezone(ZoneInfo(meta["zone"])) if when.tzinfo else when
        except (ZoneInfoNotFoundError, TypeError, ValueError):
            log.debug("calendar timezone unavailable")
        day = when.strftime("%m-%d" if view["all_day"] else "%m-%d %H:%M")
    return view, " · ".join(s for s in (meta["title"], day) if s)


def system_text(data):
    values = {str(k): "、".join(content(obj(i).get("text")) for i in seq(obj(v).get("items")))
              for k, v in obj(data.get("sysContentValues")).items()}
    values.update({str(k): content(v) for k, v in obj(data.get("contents")).items() if str(k) not in values})
    template = content(data.get("messageTemplate"))
    if template:
        # Some messages provide only a localization key, with no user-facing sentence.
        rendered = re.sub(r"(?:\{|%)(\w+)\}?", lambda m: values.get(m.group(1), m.group(0)), template)
        if not re.fullmatch(r"[\w.:-]+", rendered, re.ASCII):
            return rendered
    return " · ".join(v for v in values.values() if v) or "系统消息"


def _group_inline(nodes):
    """Wrap runs of top-level inline nodes in a paragraph so they flow horizontally."""
    out, run = [], []
    for n in nodes:
        if n.get("kind") in INLINE_KINDS:
            run.append(n)
        else:
            if run:
                out.append(node("paragraph", children=run))
                run = []
            out.append(n)
    if run:
        out.append(node("paragraph", children=run))
    return out


def _has_emotion(E, rich):
    els = obj(obj(rich.get("elements")).get("dictionary"))
    return any(obj(el).get("tag") in (E.EMOTION, "EMOTION") for el in els.values())


def describe(U, msg, emojis=None, me=""):
    """Return (view, preview) from the same content. Plain messages can use their existing text."""
    kind, data = msg.get("msg_type"), obj(msg.get("content_data"))
    sender_locked = kind == CARD and data.get("extra") == 1 and bool(me) and str(msg.get("from_id") or "") == str(me)
    tree = Tree(U, emojis=emojis, actions=data.get("actions") if kind == CARD else None,
                sender_cannot_click=sender_locked)
    if kind == CARD:
        card = localized_card(data)
        header, old_header = obj(card.get("header")), obj(data.get("cardHeader"))
        title = content(header.get("title") or old_header.get("mainTitle") or old_header.get("title"))
        subtitle = content(header.get("subtitle") or old_header.get("subtitle"))
        i18n = obj(card.get("i18n_elements"))
        elements = obj(card.get("body")).get("elements") or card.get("elements") or i18n.get("zh_cn") or i18n.get("en_us")
        nodes = tree.card(elements) or tree.rich(obj(data.get("richtext") or data.get("richText")))
        summary = content(data.get("cardDesc")) or content(obj(card.get("summary")).get("content"))
        title = title or content(card.get("title"))
        if not nodes and summary and summary != title:
            nodes = [node("text", summary)]
        first = title or words(nodes) or summary or subtitle
        return dict(kind="card", title=title, subtitle=subtitle, tone=content(header.get("template") or old_header.get("theme")),
                    url=link_of(obj(card.get("card_link"))) or content(data.get("actionUrl")), nodes=nodes), (
                        "[卡片] " + preview_text(first) if first else "[卡片]")
    if kind in (CALENDAR, SHARED_EVENT, GENERAL_EVENT):
        view, summary = calendar(data)
        return view, "[日历] " + summary
    if kind == SYSTEM:
        text = system_text(data)
        return dict(kind="system", nodes=[node("text", tree.take(text))]), text
    if kind == STICKER:
        key = media_key(U.media.extract_resource_key(kind, data))
        return dict(kind="media", nodes=[node("image", "表情包", key=key, tone="sticker")]), "[表情包]"
    if kind in (IMAGE, FILE):
        key = media_key(U.media.extract_resource_key(kind, data))
        name = content(data.get("name"))
        size = data.get("size")
        return dict(kind="media", nodes=[node("image" if kind == IMAGE else "file", name, key=key,
                                              tone=str(size) if isinstance(size, (str, int)) else "")]), (
            "[图片]" if kind == IMAGE else ("[文件] " + name).strip())
    if kind in (POST, TEXT):
        rich = obj(data.get("richText"))
        has_em = _has_emotion(tree.E, rich)
        if kind == TEXT and not has_em and not any(rich.get(k) for k in ("imageIds", "markdownIds")):
            return None, ""
        title = content(data.get("title"))
        nodes = tree.rich(rich)
        plain = content(data.get("text"))
        if not nodes and plain:
            nodes = [node("text", plain)]
        summary = title or (richtext.to_text(U.L, rich, emojis=emojis) if has_em else content(rich)) or words(nodes) or plain
        return dict(kind="post", title=title, nodes=nodes), preview_text(summary) or "[富文本]"
    raw = content(msg.get("content"))
    cleaned = re.sub(r",?\s*(key|imageKey|stickerId)=\S+", "", raw).strip()
    structure = re.search(r'(?:^|\s)([{\[])\s*[{"\[]', cleaned)
    if structure:
        cleaned = cleaned[:structure.start()].strip()
    return None, cleaned or "[" + LABELS.get(kind, "飞书消息") + "]"


def decode(U, raw, chat=""):
    """Decode both a push and a message pulled by position into one input shape."""
    from google.protobuf.message import DecodeError

    D = U.decoders
    kind = D.enum_to_int(raw.get("type", 0))
    body = raw.get("content") or b""
    summary, data = D.decode_message_content(kind, body)
    # The published decoder omits this type and exposes only the bytes for a shared event.
    full = {GENERAL_EVENT: U.L.entities.GeneralCalendarContent, SHARED_EVENT: U.L.entities.ShareCalendarEventContent,
            CARD: U.L.entities.CardContent}.get(kind)
    if full is not None:
        parsed = full()
        try:
            parsed.ParseFromString(body)
            data = D.protobuf_to_dict(parsed) or data
        except DecodeError:
            log.debug("could not decode the extended fields of message %s", raw.get("id"))
    return dict(msg_id=raw.get("id"), msg_type=kind, chat_type=D.enum_to_int(raw.get("chatType", 0)),
                from_id=raw.get("fromId"), chat_id=raw.get("chatId") or raw.get("channelId") or chat,
                root_id=raw.get("rootId"), parent_id=raw.get("parentId"), cid=raw.get("cid"),
                create_time=raw.get("createTime"), update_time=raw.get("updateTime"),
                content=summary, content_data=data)
