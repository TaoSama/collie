package wechat

import (
	"path/filepath"
)

const (
	stashFileName = "pending-media.json"
	stashBlobDir  = "inbound-blobs"
	// maxStashedMedia bounds what one chat holds for its next text.
	maxStashedMedia = MaxInboundMedia * 3
	stashFileBytes  = 1024 * 1024
)

// StashedMedia is an attachment sent alone, held as context for the chat's
// next text. Its bytes are kept by hash.
type StashedMedia struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Mime     string `json:"mime"`
	SHA256   string `json:"sha256"`
	StoredAt int64  `json:"storedAt"`
}

type stashFile struct {
	Fingerprint string                    `json:"fingerprint"`
	Chats       map[string][]StashedMedia `json:"chats"`
}

// mediaStash keeps attachments that arrived without text until the next
// text of the same chat goes to Collie with them.
type mediaStash struct {
	dir         string
	fingerprint func() string
	current     string
	chats       map[string][]StashedMedia
	blobs       blobDir
	windowMs    int64
}

func newMediaStash(dir string, fingerprint func() string, windowMs int64) (*mediaStash, error) {
	s := &mediaStash{dir: dir, fingerprint: fingerprint, current: fingerprint(), chats: map[string][]StashedMedia{},
		blobs: blobDir{filepath.Join(dir, stashBlobDir)}, windowMs: windowMs}
	var st stashFile
	found, err := readPrivateJSON(filepath.Join(dir, stashFileName), stashFileBytes, &st)
	if err != nil {
		return nil, err
	}
	if found && st.Fingerprint == s.current && st.Chats != nil {
		s.chats = st.Chats
	}
	return s, nil
}

func (s *mediaStash) account() error {
	if current := s.fingerprint(); current != s.current {
		s.current, s.chats = current, map[string][]StashedMedia{}
		return s.save()
	}
	return nil
}

func (s *mediaStash) save() error {
	if err := writePrivateJSON(filepath.Join(s.dir, stashFileName), stashFile{Fingerprint: s.current, Chats: s.chats}); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, list := range s.chats {
		for _, m := range list {
			keep[m.SHA256] = true
		}
	}
	return s.blobs.sweep(keep)
}

// hold adds media to chat's context, keeping the newest.
func (s *mediaStash) hold(chatID string, media []DownloadedMedia) error {
	if err := s.account(); err != nil {
		return err
	}
	previous := s.chats[chatID]
	list := append([]StashedMedia(nil), previous...)
	for _, m := range media {
		if _, err := s.blobs.put(m.Bytes); err != nil {
			return err
		}
		list = append(list, StashedMedia{Kind: m.Kind, Name: m.Name, Mime: m.Mime, SHA256: m.SHA256, StoredAt: nowMs()})
	}
	if len(list) > maxStashedMedia {
		list = list[len(list)-maxStashedMedia:]
	}
	s.chats[chatID] = list
	if err := s.save(); err != nil {
		s.chats[chatID] = previous
		return err
	}
	return nil
}

// held is chat's context still within the reply window, with its bytes.
func (s *mediaStash) held(chatID string) ([]DownloadedMedia, error) {
	if err := s.account(); err != nil {
		return nil, err
	}
	var out []DownloadedMedia
	now := nowMs()
	for _, m := range s.chats[chatID] {
		if now-m.StoredAt >= s.windowMs {
			continue
		}
		data, err := s.blobs.get(m.SHA256)
		if err != nil {
			return nil, err
		}
		out = append(out, DownloadedMedia{Kind: m.Kind, Name: m.Name, Mime: m.Mime, Bytes: data, SHA256: m.SHA256})
	}
	return out, nil
}

// release forgets chat's context once Collie has it.
func (s *mediaStash) release(chatID string) error {
	previous, ok := s.chats[chatID]
	if !ok {
		return nil
	}
	delete(s.chats, chatID)
	if err := s.save(); err != nil {
		s.chats[chatID] = previous
		return err
	}
	return nil
}
