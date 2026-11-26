package wechat

import (
	"errors"
	"path/filepath"
)

const (
	answersFileName  = "answers.json"
	answersBlobDir   = "answer-blobs"
	answersFileBytes = 16 * 1024 * 1024
	maxFrozenReplies = MaxOutboxLimit
)

// FrozenFile is a file of a reply, kept by its hash.
type FrozenFile struct {
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

// FrozenReply is Collie's answer as it arrived: recovery sends exactly
// this, never asks for it again. State is set once its delivery ended; the
// body is dropped then and only the outcome kept, so a repeated ref is
// answered from it.
type FrozenReply struct {
	Ref       string            `json:"ref"`
	ChatID    string            `json:"chatId"`
	ReplyTo   string            `json:"replyTo,omitempty"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text,omitempty"`
	Images    []ImageAttachment `json:"images,omitempty"`
	Files     []FrozenFile      `json:"files,omitempty"`
	Identity  string            `json:"identity"`
	CreatedAt int64             `json:"createdAt"`
	State     string            `json:"state,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	MessageID string            `json:"messageId,omitempty"`
}

type answersFile struct {
	Fingerprint string         `json:"fingerprint"`
	Replies     []*FrozenReply `json:"replies"`
}

var (
	errAnswersFull     = errors.New("Frozen reply store capacity exhausted")
	errAnswerIdentity  = errors.New("Reply content changed for the same ref; refusing replay")
	errAnswerMissing   = errors.New("Frozen reply is unavailable")
	errAnswerReadError = errors.New("Failed to read frozen replies")
)

// answerStore keeps frozen replies in the data directory.
type answerStore struct {
	dir         string
	fingerprint func() string
	current     string
	replies     []*FrozenReply
	blobs       blobDir
}

func newAnswerStore(dir string, fingerprint func() string) (*answerStore, error) {
	s := &answerStore{dir: dir, fingerprint: fingerprint, current: fingerprint(), blobs: blobDir{filepath.Join(dir, answersBlobDir)}}
	var st answersFile
	found, err := readPrivateJSON(filepath.Join(dir, answersFileName), answersFileBytes, &st)
	if err != nil {
		return nil, errAnswerReadError
	}
	if found && st.Fingerprint == s.current {
		s.replies = st.Replies
	}
	return s, nil
}

func (s *answerStore) account() error {
	if current := s.fingerprint(); current != s.current {
		s.current, s.replies = current, nil
		return s.save()
	}
	return nil
}

func (s *answerStore) save() error {
	replies := s.replies
	if replies == nil {
		replies = []*FrozenReply{}
	}
	if err := writePrivateJSON(filepath.Join(s.dir, answersFileName), answersFile{Fingerprint: s.current, Replies: replies}); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range s.replies {
		for _, img := range r.Images {
			keep[img.SHA256] = true
		}
		for _, f := range r.Files {
			keep[f.SHA256] = true
		}
	}
	return s.blobs.sweep(keep)
}

func (s *answerStore) get(ref string) *FrozenReply {
	if s.account() != nil {
		return nil
	}
	for _, r := range s.replies {
		if r.Ref == ref {
			return r
		}
	}
	return nil
}

// put freezes a reply with its bytes once; the same ref with other content
// is refused.
func (s *answerStore) put(r FrozenReply, images, files [][]byte) (*FrozenReply, error) {
	if err := s.account(); err != nil {
		return nil, err
	}
	if existing := s.get(r.Ref); existing != nil {
		if existing.Identity != r.Identity {
			return nil, errAnswerIdentity
		}
		return existing, nil
	}
	if len(s.replies) >= maxFrozenReplies {
		evicted := -1
		for i, old := range s.replies {
			if old.State != "" {
				evicted = i
				break
			}
		}
		if evicted < 0 {
			return nil, errAnswersFull
		}
		s.replies = append(s.replies[:evicted:evicted], s.replies[evicted+1:]...)
	}
	for _, data := range images {
		if _, err := s.blobs.put(data); err != nil {
			return nil, err
		}
	}
	for _, data := range files {
		if _, err := s.blobs.put(data); err != nil {
			return nil, err
		}
	}
	stored := r
	s.replies = append(s.replies, &stored)
	if err := s.save(); err != nil {
		s.replies = s.replies[:len(s.replies)-1]
		return nil, err
	}
	return &stored, nil
}

// finish records a reply's outcome and drops its body.
func (s *answerStore) finish(ref, state, reason, messageID string) error {
	r := s.get(ref)
	if r == nil {
		return nil
	}
	previous := *r
	r.State, r.Reason, r.MessageID = state, reason, messageID
	r.Text, r.Images, r.Files = "", nil, nil
	if err := s.save(); err != nil {
		*r = previous
		return err
	}
	return nil
}

func (s *answerStore) blob(sum string) ([]byte, error) {
	data, err := s.blobs.get(sum)
	if err != nil {
		return nil, errAnswerMissing
	}
	return data, nil
}
