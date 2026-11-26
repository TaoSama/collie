package wechat

import (
	"errors"
	"sort"
	"strings"
)

// Prefixes of reply plan keys that belong to something else than a message.
const (
	imageReplyPrefix = "image-reply:"
	renewalPrefix    = "renewal:"
)

func (s *StateManager) ReplySentChunks(key string) int {
	if key == "" {
		return 0
	}
	return s.SentReplyProgress[key]
}

// HasUncertainReply: a chunk was handed to WeChat without a receipt. It is
// never sent again.
func (s *StateManager) HasUncertainReply(key string) bool { return s.ReplyDeliveries[key].InFlight }

func (s *StateManager) HasExhaustedReplyRejections(key string) bool {
	return s.ReplyDeliveries[key].RejectedAttempts >= MaxRejectedReplyAttempts
}

func (s *StateManager) HasPermanentReplyFailure(key string) bool {
	item, ok := s.ReplyDeliveries[key]
	return ok && (item.FailureReason != "" || item.RenewalRejected)
}

func (s *StateManager) RecordReplyFailure(key, reason string) error {
	previous, ok := s.ReplyDeliveries[key]
	if !ok || previous.InFlight || previous.RenewalRejected || previous.AcknowledgedChunks == previous.TotalChunks ||
		previous.FailureReason != "" && previous.FailureReason != reason {
		return errors.New("Invalid WeChat permanent reply failure transition")
	}
	if previous.FailureReason == reason {
		return nil
	}
	next := previous
	next.FailureReason = reason
	s.ReplyDeliveries[key] = next
	return s.save(func() { s.ReplyDeliveries[key] = previous })
}

// PrepareReplyDelivery freezes a reply's chunk plan, or returns the plan
// already frozen under key. A different plan under the same key is refused.
func (s *StateManager) PrepareReplyDelivery(key, digest string, totalChunks int) (ReplyDeliveryState, error) {
	if previous, ok := s.ReplyDeliveries[key]; ok {
		if previous.Digest != digest || previous.TotalChunks != totalChunks {
			return ReplyDeliveryState{}, errors.New("WeChat reply content or chunk plan changed; refusing replay")
		}
		if previous.InFlight {
			return ReplyDeliveryState{}, errors.New("WeChat reply delivery uncertain; refusing replay")
		}
		if s.HasPermanentReplyFailure(key) {
			return ReplyDeliveryState{}, errors.New("WeChat reply permanently failed; refusing replay")
		}
		return previous, nil
	}
	if s.ReplySentChunks(key) > 0 {
		return ReplyDeliveryState{}, errors.New("WeChat legacy reply progress requires review before replay")
	}
	removed := ""
	if len(s.ReplyDeliveries) >= MaxReplyDeliveries {
		removed = s.evictableReply()
		if removed == "" {
			return ReplyDeliveryState{}, errors.New("WeChat reply delivery state capacity exhausted")
		}
	}
	item := ReplyDeliveryState{Digest: digest, TotalChunks: totalChunks}
	var removedItem ReplyDeliveryState
	removedProgress, hadProgress := 0, false
	if removed != "" {
		removedItem = s.ReplyDeliveries[removed]
		removedProgress, hadProgress = s.SentReplyProgress[removed]
		delete(s.ReplyDeliveries, removed)
		delete(s.SentReplyProgress, removed)
	}
	s.ReplyDeliveries[key] = item
	s.replyOrder = append(s.replyOrder, key)
	err := s.save(func() {
		delete(s.ReplyDeliveries, key)
		if removed != "" {
			s.ReplyDeliveries[removed] = removedItem
			if hadProgress {
				s.SentReplyProgress[removed] = removedProgress
			}
		}
	})
	return item, err
}

// evictableReply is the oldest completed plan nobody still needs as evidence.
func (s *StateManager) evictableReply() string {
	for _, key := range s.replyKeys() {
		item := s.ReplyDeliveries[key]
		if item.AcknowledgedChunks != item.TotalChunks || item.InFlight || s.replyEvidenceProtection(key) {
			continue
		}
		if _, ok := s.Outbox[key]; ok {
			continue
		}
		if strings.HasPrefix(key, imageReplyPrefix) {
			if _, ok := s.Outbox[strings.TrimPrefix(key, imageReplyPrefix)]; ok {
				continue
			}
		}
		return key
	}
	return ""
}

// PrepareRenewalReplyDelivery lets a renewal reminder change its wording
// between releases. Only an unsent reminder is replaced; acknowledged or
// uncertain evidence stays as it is.
func (s *StateManager) PrepareRenewalReplyDelivery(key, digest string, totalChunks int) (ReplyDeliveryState, error) {
	if !strings.HasPrefix(key, renewalPrefix) {
		return ReplyDeliveryState{}, errors.New("Invalid renewal delivery key")
	}
	previous, ok := s.ReplyDeliveries[key]
	if !ok || previous.Digest == digest && previous.TotalChunks == totalChunks || previous.AcknowledgedChunks > 0 ||
		previous.InFlight || s.HasPermanentReplyFailure(key) {
		return s.PrepareReplyDelivery(key, digest, totalChunks)
	}
	item := ReplyDeliveryState{Digest: digest, TotalChunks: totalChunks}
	s.ReplyDeliveries[key] = item
	return item, s.save(func() { s.ReplyDeliveries[key] = previous })
}

// BeginReplyChunk persists the intent to send chunk index before the
// request: a crash or an ambiguous answer can never cause a replay.
func (s *StateManager) BeginReplyChunk(key string, index int) error {
	return s.updateReplyDelivery(key, index, true, false, false)
}

func (s *StateManager) ConfirmReplyChunk(key string, index int) error {
	return s.updateReplyDelivery(key, index, false, true, false)
}

// RejectReplyChunk records WeChat's explicit refusal: the chunk may be tried
// again, a limited number of times; a refused renewal reminder never is.
func (s *StateManager) RejectReplyChunk(key string, index int, renewalReminder bool) error {
	if renewalReminder && !strings.HasPrefix(key, renewalPrefix) {
		return errors.New("Invalid renewal delivery key")
	}
	return s.updateReplyDelivery(key, index, false, false, renewalReminder)
}

func (s *StateManager) updateReplyDelivery(key string, index int, inFlight, acknowledged, renewalRejected bool) error {
	previous, ok := s.ReplyDeliveries[key]
	if !ok || s.HasPermanentReplyFailure(key) || previous.AcknowledgedChunks != index || index >= previous.TotalChunks ||
		previous.InFlight == inFlight {
		return errors.New("Invalid WeChat reply chunk transition")
	}
	previousProgress, hadProgress := s.SentReplyProgress[key]
	count := index
	if acknowledged {
		count++
	}
	next := previous
	next.AcknowledgedChunks, next.InFlight = count, inFlight
	if renewalRejected {
		next.RenewalRejected = true
	}
	if !inFlight && !acknowledged && !renewalRejected {
		next.RejectedAttempts++
	}
	s.ReplyDeliveries[key] = next
	if count > 0 {
		s.SentReplyProgress[key] = count
	}
	return s.save(func() {
		s.ReplyDeliveries[key] = previous
		if hadProgress {
			s.SentReplyProgress[key] = previousProgress
		} else {
			delete(s.SentReplyProgress, key)
		}
	})
}

func (s *StateManager) RecordReplyChunkProgress(key string, chunksSent int) error {
	if key == "" {
		return nil
	}
	prev, had := s.SentReplyProgress[key]
	s.SentReplyProgress[key] = chunksSent
	return s.save(func() {
		if had {
			s.SentReplyProgress[key] = prev
		} else {
			delete(s.SentReplyProgress, key)
		}
	})
}

// replyKeys lists the reply plans oldest first. The order survives restarts
// in the state file; plans it does not name come last, sorted.
func (s *StateManager) replyKeys() []string {
	seen := make(map[string]bool, len(s.ReplyDeliveries))
	keys := make([]string, 0, len(s.ReplyDeliveries))
	for _, k := range s.replyOrder {
		if _, ok := s.ReplyDeliveries[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range s.ReplyDeliveries {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}
