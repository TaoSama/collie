package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxImageBytes        = 8 * 1024 * 1024
	MaxReplyImages       = 3
	DefaultMaxFileBytes  = 50 * 1024 * 1024
	maxFileNameBytes     = 255
	maxImageIDChars      = 128
	imagesDigestVersion  = "images-v1"
	fileDigestVersion    = "file-v1"
	imageClientIDPrefix  = clientIDPrefix + "image:"
	fileClientIDPrefix   = clientIDPrefix + "file:"
	errImageInterrupted  = "WeChat image reply interrupted before completion"
	errImageBeforeUpload = "WeChat image reply interrupted before upload"
	errFileInterrupted   = "WeChat file reply interrupted before completion"
)

var (
	imageMimes       = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
	sha256Pattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imageIDForbidden = regexp.MustCompile(`[\x00-\x20\x7f]`)
	fileNameBad      = regexp.MustCompile(`[\x00-\x1f\x7f/\\]`)
)

func validateImageBytes(b []byte) (string, error) {
	if len(b) == 0 || len(b) > MaxImageBytes {
		return "", errors.New("WeChat image must be a Buffer between 1 byte and 8 MiB")
	}
	mime := sniffImageMime(b)
	if mime == "" {
		return "", errors.New("WeChat image has an unsupported file signature")
	}
	return mime, nil
}

// ImageAttachment describes a reply picture; its bytes are read when sent.
type ImageAttachment struct {
	ID     string `json:"id"`
	Mime   string `json:"mime"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

func validateImageAttachments(images []ImageAttachment) error {
	if len(images) > MaxReplyImages {
		return errors.New("Invalid WeChat image attachment list")
	}
	ids := map[string]bool{}
	for _, img := range images {
		if img.ID == "" || utf16Len(img.ID) > maxImageIDChars || img.ID == "." || img.ID == ".." ||
			imageIDForbidden.MatchString(img.ID) || ids[img.ID] || !imageMimes[img.Mime] ||
			img.Size <= 0 || img.Size > MaxImageBytes || !sha256Pattern.MatchString(img.SHA256) {
			return errors.New("Invalid WeChat image attachment metadata")
		}
		ids[img.ID] = true
	}
	return nil
}

// ImageUploader puts reply pictures on the CDN.
type ImageUploader struct{ uploader *cdnUploader }

func newImageUploader(c *ILinkClient, request uploadRequest) (*ImageUploader, error) {
	return &ImageUploader{&cdnUploader{client: c, request: request, label: KindImage}}, nil
}

func (u *ImageUploader) upload(ctx context.Context, chatID string, data []byte) (map[string]any, error) {
	if _, err := validateImageBytes(data); err != nil {
		return nil, err
	}
	up, err := u.uploader.upload(ctx, chatID, data, mediaTypeImage)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": ItemTypeImage, "image_item": map[string]any{"media": cdnMediaJSON(up), "mid_size": up.CiphertextBytes}}, nil
}

func cdnMediaJSON(up UploadedCDNMedia) map[string]any {
	return map[string]any{"encrypt_query_param": up.Media.EncryptQueryParam, "aes_key": up.Media.AESKey, "encrypt_type": up.Media.EncryptType}
}

// FileMetadata is a reply file's integrity facts.
type FileMetadata struct {
	FileName   string
	ByteLength int
	SHA256     string
}

// FileUploader puts reply files on the CDN after checking their integrity.
type FileUploader struct {
	uploader     *cdnUploader
	maxFileBytes int64
}

func newFileUploader(c *ILinkClient, request uploadRequest, maxFileBytes int64) (*FileUploader, error) {
	if maxFileBytes == 0 {
		maxFileBytes = DefaultMaxFileBytes
	}
	if maxFileBytes < 0 || maxFileBytes > maxSafeInteger {
		return nil, errors.New("Invalid WeChat file byte limit")
	}
	return &FileUploader{&cdnUploader{client: c, request: request, label: KindFile}, maxFileBytes}, nil
}

func (u *FileUploader) upload(ctx context.Context, chatID string, data []byte, meta FileMetadata) (map[string]any, error) {
	if len(data) == 0 || int64(len(data)) > u.maxFileBytes {
		return nil, errors.New("WeChat file exceeds the configured byte limit or is empty")
	}
	if strings.TrimSpace(meta.FileName) == "" || meta.FileName == "." || meta.FileName == ".." ||
		fileNameBad.MatchString(meta.FileName) || len(meta.FileName) > maxFileNameBytes || !utf8.ValidString(meta.FileName) {
		return nil, errors.New("WeChat file requires a valid file name without a path")
	}
	sum := sha256.Sum256(data)
	if meta.ByteLength != len(data) || !sha256Pattern.MatchString(meta.SHA256) || hex.EncodeToString(sum[:]) != meta.SHA256 {
		return nil, errors.New("WeChat file bytes do not match artifact integrity metadata")
	}
	up, err := u.uploader.upload(ctx, chatID, data, mediaTypeFile)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": ItemTypeFile, "file_item": map[string]any{"media": cdnMediaJSON(up), "file_name": meta.FileName,
		"len": strconv.Itoa(up.PlaintextBytes)}}, nil
}

// imageReplyOptions is one picture reply's delivery.
type imageReplyOptions struct {
	deliveryKey    string
	images         []ImageAttachment
	getImageBytes  func(ctx context.Context, image ImageAttachment) ([]byte, error)
	uploadImage    func(ctx context.Context, data []byte) (map[string]any, error)
	sendImage      func(ctx context.Context, item map[string]any, clientID string) (string, error)
	beforeBegin    func() error
	shouldContinue func() bool
}

// deliverImages sends each picture once: read, check, upload, then the
// intent-send-confirm sequence. Only sendmessage has an uncertain outcome.
func deliverImages(ctx context.Context, s *StateManager, chatID string, o imageReplyOptions) (int, error) {
	if err := validateImageAttachments(o.images); err != nil {
		return 0, err
	}
	if len(o.images) == 0 {
		return 0, nil
	}
	if strings.TrimSpace(chatID) == "" || strings.TrimSpace(o.deliveryKey) == "" {
		return 0, errors.New("WeChat image delivery requires stable identity")
	}
	if err := abortCause(ctx); err != nil {
		return 0, err
	}
	digest := hashJSON([]any{s.LoginFingerprint(), chatID, imagesDigestVersion, o.images})
	delivery, err := s.PrepareReplyDelivery(o.deliveryKey, digest, len(o.images))
	if err != nil {
		return 0, err
	}
	stop := func(msg string) error {
		if o.shouldContinue != nil && !o.shouldContinue() {
			return deferred(DeferContextChanged, msg)
		}
		return nil
	}
	for index := delivery.AcknowledgedChunks; index < len(o.images); index++ {
		if err := abortCause(ctx); err != nil {
			return 0, err
		}
		if err := stop(errImageInterrupted); err != nil {
			return 0, err
		}
		image := o.images[index]
		data, err := o.getImageBytes(ctx, image)
		if err != nil {
			return 0, err
		}
		if err := abortCause(ctx); err != nil {
			return 0, err
		}
		mime, mimeErr := validateImageBytes(data)
		sum := sha256.Sum256(data)
		if len(data) != image.Size || mimeErr != nil || mime != image.Mime || hex.EncodeToString(sum[:]) != image.SHA256 {
			return 0, errors.New("WeChat image bytes do not match the attachment metadata")
		}
		if err := stop(errImageBeforeUpload); err != nil {
			return 0, err
		}
		item, err := o.uploadImage(ctx, bytes.Clone(data))
		if err != nil {
			return 0, err
		}
		if err := abortCause(ctx); err != nil {
			return 0, err
		}
		if err := stop(errImageInterrupted); err != nil {
			return 0, err
		}
		clientID := imageClientIDPrefix + hashJSON([]any{s.LoginFingerprint(), chatID, o.deliveryKey, index})[:deterministicIDHexChars]
		if o.beforeBegin != nil {
			if err := o.beforeBegin(); err != nil {
				return 0, err
			}
		}
		if err := s.BeginReplyChunk(o.deliveryKey, index); err != nil {
			return 0, err
		}
		if _, err := o.sendImage(ctx, item, clientID); err != nil {
			if IsRejectedSendReceipt(err) {
				if rerr := s.RejectReplyChunk(o.deliveryKey, index, false); rerr != nil {
					return 0, rerr
				}
			}
			return 0, err
		}
		if err := s.ConfirmReplyChunk(o.deliveryKey, index); err != nil {
			return 0, err
		}
	}
	return len(o.images), nil
}

// imageAttachmentFor describes picture bytes for a frozen reply.
func imageAttachmentFor(id string, data []byte) (ImageAttachment, error) {
	mime, err := validateImageBytes(data)
	if err != nil {
		return ImageAttachment{}, err
	}
	sum := sha256.Sum256(data)
	return ImageAttachment{ID: id, Mime: mime, Size: len(data), SHA256: hex.EncodeToString(sum[:])}, nil
}
