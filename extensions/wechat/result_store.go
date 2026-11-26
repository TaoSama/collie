package wechat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	resultStoreBytes   = 8 * 1024 * 1024
	maxResumeRecords   = 1000
	maxStoredResults   = ResultOutboxLimit
	reasonSuperseded   = "superseded"
	reasonExpired      = "business_expired"
	reasonSendUnknown  = "send_unknown"
	resultsFileName    = "results.json"
	artifactsDirectory = "artifacts"
)

var (
	errResultNotFound = errors.New("Result delivery not found")
	errResultConflict = errors.New("Result idempotency key already belongs to different immutable content")
	errResultsFull    = errors.New("Result store capacity exhausted")
)

// storedResult is a plan with what the store alone needs: which outcome
// Collie was told, so each is reported once.
type storedResult struct {
	ResultDelivery
	Reported string `json:"reported,omitempty"`
}

type resumeRecord struct {
	ChatID     string `json:"chatId"`
	Generation string `json:"generation"`
	Resumed    int    `json:"resumed"`
}

type resultStoreFile struct {
	Fingerprint string          `json:"fingerprint"`
	Plans       []*storedResult `json:"plans"`
	Resumes     []resumeRecord  `json:"resumes,omitempty"`
}

// resultStore holds the results Collie pushed and moves them through
// ready, claimed, waiting_for_user and a final receipt, the way a result
// service would; the dispatcher drives it. Artifacts are files beside it.
type resultStore struct {
	dir         string
	fingerprint func() string
	current     string
	plans       map[string]*storedResult
	resumes     []resumeRecord
	// savePending covers in-memory changes and failed checkpoints, which may
	// already have replaced results.json before reporting an error.
	savePending bool
	// report tells Collie a result's outcome: sent, deferred, uncertain or expired.
	report func(ref, state, reason string)
	// attempted reports whether any step of a plan reached WeChat; an
	// attempt that ran out without one is retried, never made uncertain.
	attempted func(ResultDelivery) bool
}

func newResultStore(dir string, fingerprint func() string, report func(ref, state, reason string)) (*resultStore, error) {
	s := &resultStore{dir: dir, fingerprint: fingerprint, current: fingerprint(), plans: map[string]*storedResult{}, report: report}
	var st resultStoreFile
	found, err := readPrivateJSON(filepath.Join(dir, resultsFileName), resultStoreBytes, &st)
	if err != nil {
		return nil, errors.New("Failed to read result store state")
	}
	if !found || st.Fingerprint != s.current {
		s.savePending = found
		return s, nil
	}
	for _, p := range st.Plans {
		if p == nil || ValidateResultDelivery(p.ResultDelivery, false) != nil {
			return nil, errors.New("Invalid result store state")
		}
		s.plans[p.ID] = p
	}
	s.resumes = st.Resumes
	return s, nil
}

// account empties the store when the login changes.
func (s *resultStore) account() error {
	current := s.fingerprint()
	if current == s.current {
		return nil
	}
	s.current, s.plans, s.resumes = current, map[string]*storedResult{}, nil
	s.savePending = true
	if err := os.RemoveAll(filepath.Join(s.dir, artifactsDirectory)); err != nil {
		return err
	}
	return s.save()
}

func (s *resultStore) save() error {
	plans := make([]*storedResult, 0, len(s.plans))
	for _, p := range s.sorted() {
		plans = append(plans, p)
	}
	err := writePrivateJSON(filepath.Join(s.dir, resultsFileName), resultStoreFile{Fingerprint: s.current, Plans: plans, Resumes: s.resumes})
	s.savePending = err != nil
	return err
}

func (s *resultStore) sorted() []*storedResult {
	out := make([]*storedResult, 0, len(s.plans))
	for _, p := range s.plans {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *resultStore) artifactPath(id string) string {
	return filepath.Join(s.dir, artifactsDirectory, id)
}

// commit saves a change, restoring before when the write fails.
func (s *resultStore) commit(before map[string]storedResult) error {
	if err := s.save(); err != nil {
		for id, p := range before {
			p := p
			s.plans[id] = &p
		}
		return err
	}
	return nil
}

func (s *resultStore) byKey(key string) *storedResult {
	for _, p := range s.plans {
		if p.Key == key {
			return p
		}
	}
	return nil
}

// create stores a plan once per key; the same key with other content is
// refused. Older plans it supersedes stop waiting.
func (s *resultStore) create(plan ResultDelivery, artifact []byte) (ResultDelivery, error) {
	if err := s.account(); err != nil {
		return ResultDelivery{}, err
	}
	if existing := s.byKey(plan.Key); existing != nil {
		if existing.ChatID != plan.ChatID || existing.Kind != plan.Kind || existing.Source != plan.Source ||
			existing.Artifact.SHA256 != plan.Artifact.SHA256 || existing.Mode != plan.Mode {
			return ResultDelivery{}, errResultConflict
		}
		return existing.ResultDelivery, nil
	}
	if err := ValidateResultDelivery(plan, false); err != nil {
		return ResultDelivery{}, err
	}
	if len(s.plans) >= maxStoredResults {
		if err := s.prune(nowMs()); err != nil {
			return ResultDelivery{}, err
		}
		if len(s.plans) >= maxStoredResults {
			return ResultDelivery{}, errResultsFull
		}
	}
	if err := os.MkdirAll(filepath.Join(s.dir, artifactsDirectory), privateDirMode); err != nil {
		return ResultDelivery{}, err
	}
	if err := writePrivateFile(s.artifactPath(plan.Artifact.ID), artifact); err != nil {
		return ResultDelivery{}, err
	}
	before := map[string]storedResult{}
	var superseded []string
	for id, older := range s.plans {
		if canSupersede(older.ResultDelivery, plan) {
			before[id] = *older
			older.Status = ResultSuperseded
			superseded = append(superseded, older.Key)
		} else if canSupersede(plan, older.ResultDelivery) {
			plan.Status = ResultSuperseded
		}
	}
	s.plans[plan.ID] = &storedResult{ResultDelivery: plan}
	if err := s.commit(before); err != nil {
		delete(s.plans, plan.ID)
		return ResultDelivery{}, err
	}
	for _, key := range superseded {
		s.reportOnce(key, DeliveryExpired, reasonSuperseded)
	}
	if plan.Status == ResultSuperseded {
		s.reportOnce(plan.Key, DeliveryExpired, reasonSuperseded)
	}
	return plan, nil
}

func writePrivateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, privateFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// reportOnce tells Collie a plan's outcome the first time it is reached.
func (s *resultStore) reportOnce(key, state, reason string) {
	p := s.byKey(key)
	if p == nil || p.Reported == state && state != DeliveryDeferred {
		return
	}
	p.Reported = state
	if err := s.save(); err != nil {
		p.Reported = ""
		return
	}
	if s.report != nil {
		s.report(key, state, reason)
	}
}

// get also turns an attempt that outlived its deadline into uncertain.
func (s *resultStore) get(id string, now int64) (ResultDelivery, error) {
	if err := s.account(); err != nil {
		return ResultDelivery{}, err
	}
	p, ok := s.plans[id]
	if !ok {
		return ResultDelivery{}, errResultNotFound
	}
	if p.Status == ResultClaimed && p.AttemptExpiresAt <= now {
		before := map[string]storedResult{id: *p}
		unsent := s.attempted != nil && !s.attempted(p.ResultDelivery)
		if unsent {
			p.Status, p.AttemptID, p.AttemptedAt, p.AttemptExpiresAt, p.DeferReason = ResultReady, "", 0, 0, "attempt_expired"
		} else {
			p.Status = ResultUncertain
		}
		if err := s.commit(before); err != nil {
			return ResultDelivery{}, err
		}
		if !unsent {
			s.reportOnce(p.Key, DeliveryUncertain, reasonSendUnknown)
		}
	}
	return p.ResultDelivery, nil
}

// listPending is every ready plan for chats, terminal kinds first, oldest
// first.
func (s *resultStore) listPending(chats []string, now int64) ([]ResultDelivery, error) {
	if err := s.account(); err != nil {
		return nil, err
	}
	var out []ResultDelivery
	for _, p := range s.sorted() {
		if p.Status == ResultClaimed && p.AttemptExpiresAt <= now {
			if _, err := s.get(p.ID, now); err != nil {
				return nil, err
			}
		}
		if containsString(chats, p.ChatID) && p.Status == ResultReady && p.ExpiresAt > now && p.RetainedUntil > now && p.CreatedAt <= now {
			out = append(out, p.ResultDelivery)
		}
	}
	sortStable(out, func(a, b ResultDelivery) bool {
		return a.Kind != KindProgressResult && b.Kind == KindProgressResult
	})
	if len(out) > ResultPendingLimit {
		out = out[:ResultPendingLimit]
	}
	return out, nil
}

// transition applies next to plan id; nil from next is a mismatch.
func (s *resultStore) transition(id string, next func(ResultDelivery) *ResultDelivery) (ResultDelivery, error) {
	if err := s.account(); err != nil {
		return ResultDelivery{}, err
	}
	p, ok := s.plans[id]
	if !ok {
		return ResultDelivery{}, errResultNotFound
	}
	updated := next(p.ResultDelivery)
	if updated == nil {
		return ResultDelivery{}, errors.New("Result delivery transition refused")
	}
	before := map[string]storedResult{id: *p}
	p.ResultDelivery = *updated
	if err := s.commit(before); err != nil {
		return ResultDelivery{}, err
	}
	return p.ResultDelivery, nil
}

func (s *resultStore) claim(id, attemptID string, now int64) (ResultDelivery, error) {
	if _, err := s.get(id, now); err != nil {
		return ResultDelivery{}, err
	}
	return s.transition(id, func(p ResultDelivery) *ResultDelivery { return claimDelivery(p, attemptID, now) })
}

func (s *resultStore) deferResult(id, attemptID, reason string, now int64, submitted int) (ResultDelivery, error) {
	p, err := s.transition(id, func(p ResultDelivery) *ResultDelivery { return deferDelivery(p, attemptID, reason, now, submitted) })
	if err == nil && p.Status == ResultWaitingForUser {
		s.reportOnce(p.Key, DeliveryDeferred, reason)
	}
	return p, err
}

func (s *resultStore) receipt(id, status, attemptID string) (ResultDelivery, error) {
	p, err := s.transition(id, func(p ResultDelivery) *ResultDelivery { return receiptDelivery(p, status, attemptID) })
	if err != nil {
		return p, err
	}
	if p.Status == ResultAccepted {
		s.reportOnce(p.Key, DeliverySent, "")
	} else {
		s.reportOnce(p.Key, DeliveryUncertain, reasonSendUnknown)
	}
	return p, nil
}

// resume releases a chat's waiting plans once per inbound generation.
func (s *resultStore) resume(chatID, generation string, now int64) (int, error) {
	if err := s.account(); err != nil {
		return 0, err
	}
	for _, r := range s.resumes {
		if generation != "" && r.ChatID == chatID && r.Generation == generation {
			return r.Resumed, nil
		}
	}
	before := map[string]storedResult{}
	resumed := 0
	for id, p := range s.plans {
		if p.ChatID == chatID && p.Status == ResultWaitingForUser && p.ExpiresAt > now && p.RetainedUntil > now {
			before[id] = *p
			p.Status, p.DeferReason = ResultReady, ""
			resumed++
		}
	}
	previous := s.resumes
	if generation != "" {
		s.resumes = append(append([]resumeRecord(nil), s.resumes...), resumeRecord{chatID, generation, resumed})
		if len(s.resumes) > maxResumeRecords {
			s.resumes = s.resumes[len(s.resumes)-maxResumeRecords:]
		}
	}
	if err := s.commit(before); err != nil {
		s.resumes = previous
		return 0, err
	}
	return resumed, nil
}

// markChatWaiting parks a chat's untouched ready plans until the person
// writes again.
func (s *resultStore) markChatWaiting(chatID, reason string, now int64) (int, error) {
	if err := s.account(); err != nil {
		return 0, err
	}
	before := map[string]storedResult{}
	var keys []string
	for id, p := range s.plans {
		if p.ChatID == chatID && p.Status == ResultReady && p.SubmittedMessages == 0 && p.CreatedAt <= now && p.ExpiresAt > now && p.RetainedUntil > now {
			before[id] = *p
			p.Status, p.DeferReason = ResultWaitingForUser, reason
			keys = append(keys, p.Key)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	if err := s.commit(before); err != nil {
		return 0, err
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.reportOnce(k, DeliveryDeferred, reason)
	}
	return len(keys), nil
}

// materialize is the plan as sent: download plans gain their link message.
func (s *resultStore) materialize(id string, now int64) (ResultDelivery, error) {
	p, err := s.get(id, now)
	if err != nil {
		return ResultDelivery{}, err
	}
	if p.Status == ResultPreparing || p.RetainedUntil <= now {
		return ResultDelivery{}, errResultNotFound
	}
	if p.Mode == ModeDownload {
		p.Messages = append(append([]string(nil), p.Messages...), downloadLinkPrefix+p.Artifact.DownloadURL)
	}
	return p, nil
}

func (s *resultStore) artifact(id string) ([]byte, error) {
	data, err := os.ReadFile(s.artifactPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("Result artifact unavailable")
	}
	return data, err
}

// prune forgets plans past retention and reports those never delivered.
func (s *resultStore) prune(now int64) error {
	var expired []string
	for id, p := range s.plans {
		final := p.Status == ResultAccepted || p.Status == ResultUncertain || p.Status == ResultSuperseded
		if !final && p.ExpiresAt <= now && p.Reported != DeliveryExpired {
			expired = append(expired, p.Key)
			p.Reported = DeliveryExpired
			s.savePending = true
		}
		if p.RetainedUntil <= now {
			delete(s.plans, id)
			s.savePending = true
			if err := os.Remove(s.artifactPath(p.Artifact.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	if !s.savePending {
		return nil
	}
	if err := s.save(); err != nil {
		return err
	}
	sort.Strings(expired)
	for _, key := range expired {
		if s.report != nil {
			s.report(key, DeliveryExpired, reasonExpired)
		}
	}
	return nil
}

// chats lists chats that still have a result to deliver.
func (s *resultStore) chats(now int64) []string {
	var out []string
	for _, p := range s.sorted() {
		if (p.Status == ResultReady || p.Status == ResultWaitingForUser || p.Status == ResultClaimed) && p.ExpiresAt > now && !containsString(out, p.ChatID) {
			out = append(out, p.ChatID)
		}
	}
	return out
}
