package wechat

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

const (
	resultOutboxBytes = 4 * 1024 * 1024
	maxChatIDChars    = 512
)

// Result reference statuses: transition intents that survive a crash.
const (
	RefPending  = "pending"
	RefClaiming = "claiming"
	RefClaimed  = "claimed"
	RefDefer    = "defer"
	RefWaiting  = "waiting"
	RefReceipt  = "receipt"
)

var (
	refStatuses       = map[string]bool{RefPending: true, RefClaiming: true, RefClaimed: true, RefDefer: true, RefWaiting: true, RefReceipt: true}
	resultStepPattern = regexp.MustCompile(`^result:([^:]+):step:`)
)

// ResultReference is the dispatcher's durable intent for one result.
type ResultReference struct {
	ID                string `json:"id"`
	ChatID            string `json:"chatId"`
	Identity          string `json:"identity"`
	Status            string `json:"status"`
	AttemptID         string `json:"attemptId,omitempty"`
	ReceiptStatus     string `json:"receiptStatus,omitempty"`
	DeferReason       string `json:"deferReason,omitempty"`
	SubmittedMessages int    `json:"submittedMessages,omitempty"`
}

func validReference(r ResultReference) error {
	if !uuidPattern.MatchString(r.ID) || strings.TrimSpace(r.ChatID) == "" || len(r.ChatID) > maxChatIDChars ||
		!digestPattern.MatchString(r.Identity) || !refStatuses[r.Status] || r.SubmittedMessages < 0 || r.SubmittedMessages > ResultTextMessages {
		return errors.New("Invalid result outbox reference")
	}
	if r.Status != RefPending && !uuidPattern.MatchString(r.AttemptID) {
		return errors.New("Invalid result outbox reference")
	}
	if r.Status == RefReceipt && r.ReceiptStatus != ResultAccepted && r.ReceiptStatus != ResultUncertain ||
		r.Status == RefDefer && (strings.TrimSpace(r.DeferReason) == "" || len(r.DeferReason) > 128) {
		return errors.New("Invalid result outbox transition intent")
	}
	return nil
}

type resultOutboxFile struct {
	Fingerprint   string            `json:"fingerprint"`
	Items         []ResultReference `json:"items"`
	DispatchTimes map[string]int64  `json:"dispatchTimes"`
	Resumes       map[string]string `json:"resumes,omitempty"`
}

// FileResultOutbox keeps bounded transition references only: never a
// second message budget or send ledger.
type FileResultOutbox struct {
	path          string
	fingerprint   func() string
	current       string
	items         map[string]ResultReference
	order         []string
	dispatchTimes map[string]int64
	resumes       map[string]string
	// failUpdate, when set, can fail an update before it is saved (tests).
	failUpdate func(next ResultReference) error
}

func NewFileResultOutbox(path string, fingerprint func() string) (*FileResultOutbox, error) {
	o := &FileResultOutbox{path: path, fingerprint: fingerprint, current: fingerprint(),
		items: map[string]ResultReference{}, dispatchTimes: map[string]int64{}, resumes: map[string]string{}}
	if o.current == "" {
		return nil, errors.New("Result outbox requires an active account")
	}
	var st resultOutboxFile
	found, err := readPrivateJSON(path, resultOutboxBytes, &st)
	if errors.Is(err, errStateTooLarge) {
		return nil, errors.New("Result outbox exceeds byte budget")
	}
	if err != nil {
		return nil, errors.New("Failed to read result outbox state")
	}
	if !found || st.Fingerprint != o.current {
		return o, nil
	}
	if len(st.Items) > ResultOutboxLimit || st.DispatchTimes == nil || len(st.DispatchTimes) > ResultOutboxLimit {
		return nil, errors.New("Invalid result outbox state")
	}
	for _, item := range st.Items {
		if err := validReference(item); err != nil {
			return nil, err
		}
		if _, dup := o.items[item.ID]; dup {
			return nil, errors.New("Duplicate result outbox reference")
		}
		o.items[item.ID] = item
		o.order = append(o.order, item.ID)
	}
	for chat, at := range st.DispatchTimes {
		if strings.TrimSpace(chat) == "" || len(chat) > maxChatIDChars || at < 0 {
			return nil, errors.New("Invalid result outbox dispatch time")
		}
		o.dispatchTimes[chat] = at
	}
	if len(st.Resumes) > ResultOutboxLimit {
		return nil, errors.New("Invalid result resume state")
	}
	for chat, gen := range st.Resumes {
		if strings.TrimSpace(chat) == "" || len(chat) > maxChatIDChars || gen == "" || len(gen) > maxChatIDChars {
			return nil, errors.New("Invalid result resume reference")
		}
		o.resumes[chat] = gen
	}
	return o, nil
}

func (o *FileResultOutbox) account() error {
	current := o.fingerprint()
	if current == "" {
		return errors.New("Result outbox account unavailable")
	}
	if current != o.current {
		o.current = current
		o.items, o.order, o.dispatchTimes, o.resumes = map[string]ResultReference{}, nil, map[string]int64{}, map[string]string{}
		return o.save()
	}
	return nil
}

func (o *FileResultOutbox) save() error {
	return writePrivateJSON(o.path, resultOutboxFile{Fingerprint: o.current, Items: o.list(), DispatchTimes: o.dispatchTimes, Resumes: o.resumes})
}

func (o *FileResultOutbox) list() []ResultReference {
	out := []ResultReference{}
	for _, id := range o.order {
		if r, ok := o.items[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (o *FileResultOutbox) References() ([]ResultReference, error) {
	if err := o.account(); err != nil {
		return nil, err
	}
	return o.list(), nil
}

func (o *FileResultOutbox) Put(r ResultReference) error {
	if err := o.account(); err != nil {
		return err
	}
	if err := validReference(r); err != nil {
		return err
	}
	if previous, ok := o.items[r.ID]; ok {
		if previous.ChatID != r.ChatID || previous.Identity != r.Identity {
			return errors.New("Result outbox identity changed")
		}
		return nil
	}
	if len(o.items) >= ResultOutboxLimit {
		return errors.New("Result outbox capacity exhausted")
	}
	o.items[r.ID] = r
	o.order = append(o.order, r.ID)
	if err := o.save(); err != nil {
		delete(o.items, r.ID)
		o.order = o.order[:len(o.order)-1]
		return err
	}
	return nil
}

func (o *FileResultOutbox) Update(id string, patch func(*ResultReference)) error {
	if err := o.account(); err != nil {
		return err
	}
	previous, ok := o.items[id]
	if !ok {
		return errors.New("Missing result outbox reference")
	}
	next := previous
	patch(&next)
	if err := validReference(next); err != nil {
		return err
	}
	if o.failUpdate != nil {
		if err := o.failUpdate(next); err != nil {
			return err
		}
	}
	if next.ID != id || next.ChatID != previous.ChatID || next.Identity != previous.Identity {
		return errors.New("Result outbox identity changed")
	}
	o.items[id] = next
	if err := o.save(); err != nil {
		o.items[id] = previous
		return err
	}
	return nil
}

func (o *FileResultOutbox) Remove(id string) error {
	if err := o.account(); err != nil {
		return err
	}
	previous, ok := o.items[id]
	if !ok {
		return nil
	}
	delete(o.items, id)
	if err := o.save(); err != nil {
		o.items[id] = previous
		return err
	}
	return nil
}

func (o *FileResultOutbox) LastDispatchAt(chatID string) (int64, error) {
	if err := o.account(); err != nil {
		return 0, err
	}
	return o.dispatchTimes[chatID], nil
}

// RecordDispatch is saved before a send, so a crash cannot skip the
// per-chat interval.
func (o *FileResultOutbox) RecordDispatch(chatID string, at int64) error {
	if err := o.account(); err != nil {
		return err
	}
	if strings.TrimSpace(chatID) == "" || len(chatID) > maxChatIDChars || at < 0 {
		return errors.New("Invalid result dispatch timestamp")
	}
	previous := make(map[string]int64, len(o.dispatchTimes))
	for k, v := range o.dispatchTimes {
		previous[k] = v
	}
	o.dispatchTimes[chatID] = at
	if len(o.dispatchTimes) > ResultOutboxLimit {
		chats := make([]string, 0, len(o.dispatchTimes))
		for k := range o.dispatchTimes {
			chats = append(chats, k)
		}
		sort.SliceStable(chats, func(i, j int) bool { return o.dispatchTimes[chats[i]] < o.dispatchTimes[chats[j]] })
		delete(o.dispatchTimes, chats[0])
	}
	if err := o.save(); err != nil {
		o.dispatchTimes = previous
		return err
	}
	return nil
}

// ResumeIntent is a chat whose waiting results should be released by a new
// inbound generation.
type ResumeIntent struct{ ChatID, Generation string }

func (o *FileResultOutbox) PendingResumes() ([]ResumeIntent, error) {
	if err := o.account(); err != nil {
		return nil, err
	}
	chats := make([]string, 0, len(o.resumes))
	for c := range o.resumes {
		chats = append(chats, c)
	}
	sort.Strings(chats)
	out := make([]ResumeIntent, 0, len(chats))
	for _, c := range chats {
		out = append(out, ResumeIntent{c, o.resumes[c]})
	}
	return out, nil
}

func (o *FileResultOutbox) QueueResume(chatID, generation string) error {
	if err := o.account(); err != nil {
		return err
	}
	if strings.TrimSpace(chatID) == "" || len(chatID) > maxChatIDChars || generation == "" || len(generation) > maxChatIDChars {
		return errors.New("Invalid result resume reference")
	}
	previous, had := o.resumes[chatID]
	if had && previous == generation {
		return nil
	}
	if !had && len(o.resumes) >= ResultOutboxLimit {
		return errors.New("Result resume outbox capacity exhausted")
	}
	o.resumes[chatID] = generation
	if err := o.save(); err != nil {
		if had {
			o.resumes[chatID] = previous
		} else {
			delete(o.resumes, chatID)
		}
		return err
	}
	return nil
}

func (o *FileResultOutbox) CompleteResume(chatID, generation string) error {
	if err := o.account(); err != nil {
		return err
	}
	if o.resumes[chatID] != generation {
		return nil
	}
	delete(o.resumes, chatID)
	if err := o.save(); err != nil {
		o.resumes[chatID] = generation
		return err
	}
	return nil
}

// ProtectsDeliveryKey keeps a result step's evidence while its reference
// still exists.
func (o *FileResultOutbox) ProtectsDeliveryKey(key string) bool {
	if o.account() != nil {
		return false
	}
	m := resultStepPattern.FindStringSubmatch(key)
	if m == nil {
		return false
	}
	_, ok := o.items[m[1]]
	return ok
}
