package wechat

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

// iLink item types.
const (
	ItemTypeText  = 1
	ItemTypeImage = 2
	ItemTypeVoice = 3
	ItemTypeFile  = 4
	ItemTypeVideo = 5
)

const (
	QuoteMaxChars = 200
	// MaxInboundMedia: attachments beyond this count in one message are ignored.
	MaxInboundMedia        = 3
	MaxMediaParamChars     = 8192
	MaxMediaFileNameChars  = 255
	aesKeyBytes            = 16
	quoteEllipsis          = "…"
	quotePrefixFormatLeft  = "引用：「"
	quotePrefixFormatRight = "」\n"
)

// Kinds of content the gateway cannot pass on.
const (
	KindImage   = "image"
	KindFile    = "file"
	KindVideo   = "video"
	KindVoice   = "voice"
	KindUnknown = "unknown"
)

var (
	aesKeyHexPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	base64Pattern    = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	controlChars     = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	// JavaScript's \s, Unicode spaces included.
	whitespaceRun = regexp.MustCompile(`[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)
	mediaKinds    = map[int]string{ItemTypeImage: KindImage, ItemTypeVoice: KindVoice, ItemTypeFile: KindFile, ItemTypeVideo: KindVideo}
)

// InboundMediaRef is a CDN reference to an attachment. Only these strings
// are persisted; the bytes are fetched when the message is processed.
type InboundMediaRef struct {
	Kind              string `json:"kind"` // image | file
	FullURL           string `json:"fullUrl,omitempty"`
	EncryptQueryParam string `json:"encryptQueryParam,omitempty"`
	// AESKeyHex is the raw AES-128 key as 32 lowercase hex characters;
	// absent means the CDN object is plaintext.
	AESKeyHex string `json:"aesKeyHex,omitempty"`
	FileName  string `json:"fileName,omitempty"`
}

// InboundMessage is one polled message.
type InboundMessage struct {
	ChatID string `json:"chatId"`
	// Text is empty only when UnsupportedKinds or Media is present.
	Text         string `json:"text"`
	MessageID    string `json:"messageId"`
	DeliveryKey  string `json:"deliveryKey"`
	ContextToken string `json:"contextToken,omitempty"`
	CreatedAtMs  *int64 `json:"createdAtMs,omitempty"`
	// UnsupportedKinds: content that cannot be passed on; the sender gets a fixed notice.
	UnsupportedKinds []string          `json:"unsupportedKinds,omitempty"`
	Media            []InboundMediaRef `json:"media,omitempty"`
	// Quote is the quoted message as it appears in Text's prefix.
	Quote string `json:"quote,omitempty"`
	// IgnoredKinds are unsupported items sent along with usable content.
	IgnoredKinds []string `json:"ignoredKinds,omitempty"`
}

// InboundContent is what a message's items carry.
type InboundContent struct {
	// Text goes to Collie, with a delimited quote prefix when present.
	Text string
	// OwnText is the sender's own words without the quote; it keeps
	// deterministic identities stable.
	OwnText string
	// UnsupportedKinds is set only when there is neither usable text nor
	// supported media.
	UnsupportedKinds []string
	Media            []InboundMediaRef
	Quote            string
	IgnoredKinds     []string
}

func record(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func itemType(item map[string]any) int {
	n, ok := safeInt(item["type"])
	if !ok || !isJSONNumber(item["type"]) {
		return -1
	}
	return int(n)
}

// itemText is a text item's content, or the platform's transcript of a
// voice item.
func itemText(item map[string]any) string {
	var field any
	switch itemType(item) {
	case ItemTypeText:
		field = record(item["text_item"])["text"]
	case ItemTypeVoice:
		field = record(item["voice_item"])["text"]
	}
	s, _ := field.(string)
	return s
}

// quoted is the first quote's text, cut to QuoteMaxChars without splitting
// a character.
func quoted(items []map[string]any) string {
	for _, item := range items {
		ref := record(item["ref_msg"])
		if ref == nil {
			continue
		}
		text := ""
		if quotedItem := record(ref["message_item"]); quotedItem != nil {
			text = itemText(quotedItem)
		}
		if text == "" {
			text, _ = ref["title"].(string)
		}
		text = strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
		if text == "" {
			return ""
		}
		cut := utf16Prefix(text, QuoteMaxChars)
		if cut != text {
			cut += quoteEllipsis
		}
		return cut
	}
	return ""
}

func boundedString(v any, maxChars int) string {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" || utf16Len(s) > maxChars || controlChars.MatchString(s) {
		return ""
	}
	return s
}

// NormalizeAESKey reads image_item.aeskey (raw hex) or CDNMedia.aes_key
// (base64 of either the raw key or its hex text). ok false means a key was
// given but is unusable; "" with ok means there is none.
func NormalizeAESKey(hexKey, base64Key any) (key string, ok bool) {
	if h, isString := hexKey.(string); isString && h != "" {
		if aesKeyHexPattern.MatchString(h) {
			return strings.ToLower(h), true
		}
		return "", false
	}
	b, isString := base64Key.(string)
	if !isString || b == "" {
		return "", true
	}
	if len(b) > 64 || !base64Pattern.MatchString(b) {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(b)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(b, "="))
		if err != nil {
			return "", false
		}
	}
	if len(decoded) == aesKeyBytes {
		return hex.EncodeToString(decoded), true
	}
	if len(decoded) == aesKeyBytes*2 && aesKeyHexPattern.Match(decoded) {
		return strings.ToLower(string(decoded)), true
	}
	return "", false
}

// mediaRef is an item's attachment; nil when the item is not one or its
// reference is unusable.
func mediaRef(item map[string]any) *InboundMediaRef {
	kind := ""
	var body map[string]any
	switch itemType(item) {
	case ItemTypeImage:
		kind, body = KindImage, record(item["image_item"])
	case ItemTypeFile:
		kind, body = KindFile, record(item["file_item"])
	default:
		return nil
	}
	media := record(body["media"])
	if body == nil || media == nil {
		return nil
	}
	ref := InboundMediaRef{Kind: kind, FullURL: boundedString(media["full_url"], MaxMediaParamChars),
		EncryptQueryParam: boundedString(media["encrypt_query_param"], MaxMediaParamChars)}
	if ref.FullURL == "" && ref.EncryptQueryParam == "" {
		return nil
	}
	var hexKey any
	if kind == KindImage {
		hexKey = body["aeskey"]
	}
	key, ok := NormalizeAESKey(hexKey, media["aes_key"])
	// Only images are seen as plaintext CDN objects; files must be encrypted.
	if !ok || kind == KindFile && key == "" {
		return nil
	}
	ref.AESKeyHex = key
	if kind == KindFile {
		ref.FileName = boundedString(body["file_name"], MaxMediaFileNameChars)
	}
	return &ref
}

// ParseInboundContent reads an item list; nil when it carries neither text
// nor a recognizable attachment.
func ParseInboundContent(itemList []any) *InboundContent {
	var items []map[string]any
	for _, raw := range itemList {
		if m := record(raw); m != nil {
			items = append(items, m)
		}
	}
	var own strings.Builder
	for _, item := range items {
		own.WriteString(itemText(item))
	}
	ownText := own.String()
	var media []InboundMediaRef
	var kinds []string
	for _, item := range items {
		if ref := mediaRef(item); ref != nil {
			if len(media) < MaxInboundMedia {
				media = append(media, *ref)
			}
			continue
		}
		t := itemType(item)
		if t == ItemTypeText || t == ItemTypeVoice && itemText(item) != "" {
			continue
		}
		kind, known := mediaKinds[t]
		if !known {
			kind = KindUnknown
		}
		if !containsString(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	if ownText != "" {
		q := quoted(items)
		text := ownText
		if q != "" {
			text = quotePrefixFormatLeft + q + quotePrefixFormatRight + ownText
		}
		return &InboundContent{Text: text, OwnText: ownText, Media: media, Quote: q, IgnoredKinds: kinds}
	}
	// Supported media alone becomes pending chat context; unsupported siblings are not announced.
	if len(media) > 0 {
		return &InboundContent{OwnText: ownText, Media: media, IgnoredKinds: kinds}
	}
	if len(kinds) > 0 {
		return &InboundContent{OwnText: ownText, UnsupportedKinds: kinds}
	}
	return nil
}
