package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/telemetry"
)

// Mode identifies whether a session is ephemeral or persistent.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

// Status is the lifecycle state of a session.
type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

// EventType identifies the kind of audit event stored in a session timeline.
type EventType string

const (
	EventOpen       EventType = "open"
	EventResume     EventType = "resume"
	EventCheckpoint EventType = "checkpoint"
	EventClose      EventType = "close"
	EventMessage    EventType = "message"
	EventSummary    EventType = "summary"
	EventBranch     EventType = "branch"
	EventCommit     EventType = "commit"
	EventMerge      EventType = "merge"
)

// MergePolicy controls how branch conflicts are handled during merge.
type MergePolicy string

const (
	MergeFail   MergePolicy = "fail"
	MergeLWW    MergePolicy = "last-write-wins"
	MergeManual MergePolicy = "manual"
)

// Provenance records who or what produced a session mutation.
type Provenance struct {
	Actor  string            `json:"actor,omitempty"`
	Source string            `json:"source,omitempty"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// HistoryPolicy configures retention and summarization behavior for a session.
type HistoryPolicy struct {
	RetentionMaxMessages    int `json:"retention_max_messages"`
	SummarizationThreshold  int `json:"summarization_threshold"`
	SummarizationKeepRecent int `json:"summarization_keep_recent"`
}

// Event is an auditable lifecycle or context mutation in a session timeline.
type Event struct {
	Seq         int64             `json:"seq"`
	Type        EventType         `json:"type"`
	OccurredAt  time.Time         `json:"occurred_at"`
	SessionID   string            `json:"session_id"`
	Ref         string            `json:"ref,omitempty"`
	Message     *llm.Message      `json:"message,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Changes     map[string]string `json:"changes,omitempty"`
	Conflicts   []string          `json:"conflicts,omitempty"`
	Provenance  Provenance        `json:"provenance,omitempty"`
	Description string            `json:"description,omitempty"`
}

// Session is the durable representation of a long-running backend session.
type Session struct {
	ID             string            `json:"id"`
	Tenant         string            `json:"tenant"`
	ClientID       string            `json:"client_id"`
	ThreadID       string            `json:"thread_id"`
	Project        string            `json:"project,omitempty"`
	Mode           Mode              `json:"mode"`
	Status         Status            `json:"status"`
	HistoryPolicy  HistoryPolicy     `json:"history_policy"`
	CheckpointRef  string            `json:"checkpoint_ref,omitempty"`
	BranchRef      string            `json:"branch_ref,omitempty"`
	CommitRef      string            `json:"commit_ref,omitempty"`
	MergeRef       string            `json:"merge_ref,omitempty"`
	BaseSessionID  string            `json:"base_session_id,omitempty"`
	BaseContext    map[string]string `json:"base_context,omitempty"`
	Context        map[string]string `json:"context,omitempty"`
	Summary        string            `json:"summary,omitempty"`
	Messages       []llm.Message     `json:"messages,omitempty"`
	Events         []Event           `json:"events,omitempty"`
	OpenedAt       time.Time         `json:"opened_at"`
	ClosedAt       time.Time         `json:"closed_at,omitempty"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	NextSeq        int64             `json:"next_seq"`
}

// OpenRequest describes the inputs required to create or reuse a session.
type OpenRequest struct {
	SessionID string
	Tenant    string
	ClientID  string
	ThreadID  string
	Project   string
	Mode      Mode
}

// MergeResult contains the merged session snapshot and any conflict keys.
type MergeResult struct {
	Session   Session  `json:"session"`
	Conflicts []string `json:"conflicts,omitempty"`
}

type diskState struct {
	Sessions map[string]*Session `json:"sessions"`
}

// Service manages durable session lifecycle, replay, and versioned context operations.
type Service struct {
	mu           sync.Mutex
	path         string
	defaults     HistoryPolicy
	defaultMerge MergePolicy
	sessions     map[string]*Session
}

// NewService creates a session service backed by a JSON persistence file.
func NewService(cfg config.SessionConfig) (*Service, error) {
	path := cfg.Path
	if path == "" {
		path = "./.krill/sessions.json"
	}
	svc := &Service{
		path: path,
		defaults: HistoryPolicy{
			RetentionMaxMessages:    cfg.RetentionMaxMessages,
			SummarizationThreshold:  cfg.SummarizationThreshold,
			SummarizationKeepRecent: cfg.SummarizationKeepRecent,
		},
		defaultMerge: MergePolicy(strings.TrimSpace(cfg.DefaultMergeConflictMode)),
		sessions:     map[string]*Session{},
	}
	if svc.defaultMerge == "" {
		svc.defaultMerge = MergeLWW
	}
	if err := svc.load(); err != nil {
		return nil, err
	}
	return svc, nil
}

func (s *Service) Open(req OpenRequest, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.ClientID) == "" || strings.TrimSpace(req.ThreadID) == "" {
		return Session{}, fmt.Errorf("client_id and thread_id are required")
	}
	if existing := s.findOpenLocked(req.ClientID, req.ThreadID); existing != nil {
		return cloneSession(*existing), nil
	}
	now := time.Now().UTC()
	mode := req.Mode
	if mode == "" {
		mode = ModePersistent
	}
	id := strings.TrimSpace(req.SessionID)
	if id == "" {
		id = uuid.NewString()
	}
	sess := &Session{
		ID:             id,
		Tenant:         strings.TrimSpace(req.Tenant),
		ClientID:       strings.TrimSpace(req.ClientID),
		ThreadID:       strings.TrimSpace(req.ThreadID),
		Project:        strings.TrimSpace(req.Project),
		Mode:           mode,
		Status:         StatusOpen,
		HistoryPolicy:  s.defaults,
		Context:        map[string]string{},
		OpenedAt:       now,
		LastActivityAt: now,
		NextSeq:        1,
	}
	s.appendEventLocked(sess, Event{Type: EventOpen, OccurredAt: now, Provenance: prov})
	s.sessions[sess.ID] = sess
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(*sess), nil
}

func (s *Service) Resume(sessionID string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	now := time.Now().UTC()
	sess.LastActivityAt = now
	s.appendEventLocked(sess, Event{Type: EventResume, OccurredAt: now, Provenance: prov})
	telemetry.IncCounter(telemetry.MetricSessionResumeTotal, 1, map[string]string{"mode": string(sess.Mode)})
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(*sess), nil
}

func (s *Service) ResumeByThread(clientID, threadID string, prov Provenance) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.findOpenLocked(clientID, threadID)
	if sess == nil {
		return Session{}, false, nil
	}
	now := time.Now().UTC()
	sess.LastActivityAt = now
	s.appendEventLocked(sess, Event{Type: EventResume, OccurredAt: now, Provenance: prov})
	telemetry.IncCounter(telemetry.MetricSessionResumeTotal, 1, map[string]string{"mode": string(sess.Mode)})
	if err := s.persistLocked(); err != nil {
		return Session{}, false, err
	}
	return cloneSession(*sess), true, nil
}

func (s *Service) Checkpoint(sessionID, description string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	ref := "chk-" + uuid.NewString()
	sess.CheckpointRef = ref
	sess.LastActivityAt = time.Now().UTC()
	s.appendEventLocked(sess, Event{
		Type:        EventCheckpoint,
		OccurredAt:  sess.LastActivityAt,
		Ref:         ref,
		Description: strings.TrimSpace(description),
		Provenance:  prov,
	})
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(*sess), nil
}

func (s *Service) Close(sessionID string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	now := time.Now().UTC()
	sess.Status = StatusClosed
	sess.ClosedAt = now
	sess.LastActivityAt = now
	s.appendEventLocked(sess, Event{Type: EventClose, OccurredAt: now, Provenance: prov})
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(*sess), nil
}

func (s *Service) RecordMessage(clientID, threadID string, msg llm.Message, prov Provenance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.findOpenLocked(clientID, threadID)
	if sess == nil {
		return nil
	}
	cp := msg
	sess.Messages = append(sess.Messages, cp)
	sess.LastActivityAt = time.Now().UTC()
	s.appendEventLocked(sess, Event{Type: EventMessage, OccurredAt: sess.LastActivityAt, Message: &cp, Provenance: prov})
	s.enforcePolicyLocked(sess, prov)
	return s.persistLocked()
}

func (s *Service) Branch(sessionID string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	now := time.Now().UTC()
	branchRef := "branch-" + uuid.NewString()
	child := cloneSession(*parent)
	child.ID = uuid.NewString()
	child.Status = StatusOpen
	child.OpenedAt = now
	child.ClosedAt = time.Time{}
	child.LastActivityAt = now
	child.BaseSessionID = parent.ID
	child.BaseContext = cloneStringMap(parent.Context)
	child.BranchRef = branchRef
	child.ThreadID = parent.ThreadID + "#" + branchRef
	child.NextSeq = 1
	child.Events = nil
	s.appendEventLocked(&child, Event{Type: EventBranch, OccurredAt: now, Ref: branchRef, Provenance: prov})
	parent.LastActivityAt = now
	s.appendEventLocked(parent, Event{Type: EventBranch, OccurredAt: now, Ref: branchRef, Description: child.ID, Provenance: prov})
	s.sessions[child.ID] = &child
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(child), nil
}

func (s *Service) Commit(sessionID string, changes map[string]string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	if sess.Context == nil {
		sess.Context = map[string]string{}
	}
	for k, v := range changes {
		sess.Context[k] = v
	}
	ref := "commit-" + uuid.NewString()
	sess.CommitRef = ref
	sess.LastActivityAt = time.Now().UTC()
	s.appendEventLocked(sess, Event{Type: EventCommit, OccurredAt: sess.LastActivityAt, Ref: ref, Changes: cloneStringMap(changes), Provenance: prov})
	if err := s.persistLocked(); err != nil {
		return Session{}, err
	}
	return cloneSession(*sess), nil
}

func (s *Service) Merge(baseSessionID, branchSessionID string, policy MergePolicy, prov Provenance) (MergeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, ok := s.sessions[strings.TrimSpace(baseSessionID)]
	if !ok {
		return MergeResult{}, fmt.Errorf("session %q not found", baseSessionID)
	}
	branch, ok := s.sessions[strings.TrimSpace(branchSessionID)]
	if !ok {
		return MergeResult{}, fmt.Errorf("session %q not found", branchSessionID)
	}
	if policy == "" {
		policy = s.defaultMerge
	}
	conflicts := findConflicts(base.Context, branch.BaseContext, branch.Context)
	if len(conflicts) > 0 && policy == MergeFail {
		return MergeResult{}, fmt.Errorf("merge conflict: %s", strings.Join(conflicts, ", "))
	}
	now := time.Now().UTC()
	ref := "merge-" + uuid.NewString()
	if len(conflicts) == 0 || policy == MergeLWW {
		if base.Context == nil {
			base.Context = map[string]string{}
		}
		for key, branchValue := range branch.Context {
			if branch.BaseContext != nil && branch.BaseContext[key] == branchValue {
				continue
			}
			base.Context[key] = branchValue
		}
		base.Messages = append(base.Messages, branch.Messages...)
	}
	base.MergeRef = ref
	base.LastActivityAt = now
	s.appendEventLocked(base, Event{Type: EventMerge, OccurredAt: now, Ref: ref, Conflicts: append([]string(nil), conflicts...), Provenance: prov})
	branch.LastActivityAt = now
	s.appendEventLocked(branch, Event{Type: EventMerge, OccurredAt: now, Ref: ref, Conflicts: append([]string(nil), conflicts...), Provenance: prov})
	if err := s.persistLocked(); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{Session: cloneSession(*base), Conflicts: conflicts}, nil
}

func (s *Service) RestoreMessages(sessionID string) ([]llm.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	return buildRestoreMessages(sess), nil
}

func (s *Service) RestoreMessagesByThread(clientID, threadID string) ([]llm.Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.findOpenLocked(clientID, threadID)
	if sess == nil {
		return nil, false, nil
	}
	return buildRestoreMessages(sess), true, nil
}

func (s *Service) Replay(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	out := append([]Event(nil), sess.Events...)
	slices.SortFunc(out, func(a, b Event) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		default:
			return 0
		}
	})
	return out, nil
}

func (s *Service) findOpenLocked(clientID, threadID string) *Session {
	for _, sess := range s.sessions {
		if sess.ClientID == strings.TrimSpace(clientID) && sess.ThreadID == strings.TrimSpace(threadID) && sess.Status == StatusOpen {
			return sess
		}
	}
	return nil
}

func (s *Service) appendEventLocked(sess *Session, evt Event) {
	evt.Seq = sess.NextSeq
	evt.SessionID = sess.ID
	sess.NextSeq++
	sess.Events = append(sess.Events, evt)
}

func (s *Service) enforcePolicyLocked(sess *Session, prov Provenance) {
	maxMessages := sess.HistoryPolicy.RetentionMaxMessages
	if maxMessages > 0 && len(sess.Messages) > maxMessages {
		drop := len(sess.Messages) - maxMessages
		sess.Summary = mergeSummary(sess.Summary, summarizeMessages(sess.Messages[:drop]))
		sess.Messages = append([]llm.Message(nil), sess.Messages[drop:]...)
		s.appendEventLocked(sess, Event{
			Type:       EventSummary,
			OccurredAt: time.Now().UTC(),
			Summary:    sess.Summary,
			Provenance: prov,
		})
	}
	threshold := sess.HistoryPolicy.SummarizationThreshold
	keep := sess.HistoryPolicy.SummarizationKeepRecent
	if threshold > 0 && len(sess.Messages) >= threshold && keep >= 0 && len(sess.Messages) > keep {
		cut := len(sess.Messages) - keep
		sess.Summary = mergeSummary(sess.Summary, summarizeMessages(sess.Messages[:cut]))
		sess.Messages = append([]llm.Message(nil), sess.Messages[cut:]...)
		s.appendEventLocked(sess, Event{
			Type:       EventSummary,
			OccurredAt: time.Now().UTC(),
			Summary:    sess.Summary,
			Provenance: prov,
		})
	}
}

func buildRestoreMessages(sess *Session) []llm.Message {
	out := make([]llm.Message, 0, len(sess.Messages)+1)
	if strings.TrimSpace(sess.Summary) != "" {
		out = append(out, llm.Message{Role: "system", Content: sess.Summary})
	}
	out = append(out, sess.Messages...)
	return out
}

func summarizeMessages(msgs []llm.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	parts := make([]string, 0, min(len(msgs), 6))
	for i, msg := range msgs {
		if i == 6 {
			break
		}
		content := strings.TrimSpace(msg.Content)
		if len(content) > 48 {
			content = content[:48] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s:%s", msg.Role, content))
	}
	return strings.Join(parts, " | ")
}

func mergeSummary(current, next string) string {
	switch {
	case strings.TrimSpace(current) == "":
		return next
	case strings.TrimSpace(next) == "":
		return current
	default:
		return current + " || " + next
	}
}

func findConflicts(baseCurrent, baseSnapshot, branchCurrent map[string]string) []string {
	keys := map[string]struct{}{}
	for k := range branchCurrent {
		keys[k] = struct{}{}
	}
	conflicts := make([]string, 0)
	for key := range keys {
		snap := baseSnapshot[key]
		base := baseCurrent[key]
		branch := branchCurrent[key]
		if branch == snap {
			continue
		}
		if base != snap && base != branch {
			conflicts = append(conflicts, key)
		}
	}
	slices.Sort(conflicts)
	return conflicts
}

func (s *Service) load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir session dir: %w", err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sessions file: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode sessions file: %w", err)
	}
	if state.Sessions != nil {
		s.sessions = state.Sessions
	}
	return nil
}

func (s *Service) persistLocked() error {
	state := diskState{Sessions: s.sessions}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sessions file: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write temp sessions file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace sessions file: %w", err)
	}
	return nil
}

func cloneSession(in Session) Session {
	out := in
	out.Context = cloneStringMap(in.Context)
	out.BaseContext = cloneStringMap(in.BaseContext)
	out.Messages = append([]llm.Message(nil), in.Messages...)
	out.Events = append([]Event(nil), in.Events...)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
