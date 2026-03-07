package session

import (
	"bufio"
	"encoding/json"
	"errors"
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

type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

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

type MergePolicy string

const (
	MergeFail   MergePolicy = "fail"
	MergeLWW    MergePolicy = "last-write-wins"
	MergeManual MergePolicy = "manual"
)

var ErrAsyncQueueFull = errors.New("session async queue full")

type Provenance struct {
	Actor  string            `json:"actor,omitempty"`
	Source string            `json:"source,omitempty"`
	Meta   map[string]string `json:"meta,omitempty"`
}

type HistoryPolicy struct {
	RetentionMaxMessages    int `json:"retention_max_messages"`
	SummarizationThreshold  int `json:"summarization_threshold"`
	SummarizationKeepRecent int `json:"summarization_keep_recent"`
}

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
	State       *Session          `json:"state,omitempty"`
}

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

type OpenRequest struct {
	SessionID string
	Tenant    string
	ClientID  string
	ThreadID  string
	Project   string
	Mode      Mode
}

type MergeResult struct {
	Session   Session  `json:"session"`
	Conflicts []string `json:"conflicts,omitempty"`
}

type queuedMessage struct {
	clientID string
	threadID string
	msg      llm.Message
	prov     Provenance
}

type Service struct {
	mu           sync.Mutex
	defaults     HistoryPolicy
	defaultMerge MergePolicy

	eventsPath    string
	snapshotsPath string

	sessions map[string]*Session
	events   map[string][]Event
	active   map[string]string

	asyncQueue chan queuedMessage
	asyncWG    sync.WaitGroup
}

func NewService(cfg config.SessionConfig) (*Service, error) {
	eventsPath, snapshotsPath := sessionPaths(cfg.Path)
	svc := &Service{
		defaults: HistoryPolicy{
			RetentionMaxMessages:    cfg.RetentionMaxMessages,
			SummarizationThreshold:  cfg.SummarizationThreshold,
			SummarizationKeepRecent: cfg.SummarizationKeepRecent,
		},
		defaultMerge:  MergePolicy(strings.TrimSpace(cfg.DefaultMergeConflictMode)),
		eventsPath:    eventsPath,
		snapshotsPath: snapshotsPath,
		sessions:      map[string]*Session{},
		events:        map[string][]Event{},
		active:        map[string]string{},
		asyncQueue:    make(chan queuedMessage, 256),
	}
	if svc.defaultMerge == "" {
		svc.defaultMerge = MergeLWW
	}
	if err := svc.load(); err != nil {
		return nil, err
	}
	go svc.runAsyncWriter()
	return svc, nil
}

func (s *Service) Shutdown() error {
	if err := s.Flush(); err != nil {
		return err
	}
	close(s.asyncQueue)
	return nil
}

func (s *Service) Flush() error {
	s.asyncWG.Wait()
	return nil
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
	if err := s.commitEventLocked(sess, Event{Type: EventOpen, OccurredAt: now, Provenance: prov}); err != nil {
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
	sess.LastActivityAt = time.Now().UTC()
	if err := s.commitEventLocked(sess, Event{Type: EventResume, OccurredAt: sess.LastActivityAt, Provenance: prov}); err != nil {
		return Session{}, err
	}
	telemetry.IncCounter(telemetry.MetricSessionResumeTotal, 1, map[string]string{"mode": string(sess.Mode)})
	return cloneSession(*sess), nil
}

func (s *Service) ResumeByThread(clientID, threadID string, prov Provenance) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.findOpenLocked(clientID, threadID)
	if sess == nil {
		return Session{}, false, nil
	}
	sess.LastActivityAt = time.Now().UTC()
	if err := s.commitEventLocked(sess, Event{Type: EventResume, OccurredAt: sess.LastActivityAt, Provenance: prov}); err != nil {
		return Session{}, false, err
	}
	telemetry.IncCounter(telemetry.MetricSessionResumeTotal, 1, map[string]string{"mode": string(sess.Mode)})
	return cloneSession(*sess), true, nil
}

func (s *Service) Checkpoint(sessionID, description string, prov Provenance) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	sess.CheckpointRef = "chk-" + uuid.NewString()
	sess.LastActivityAt = time.Now().UTC()
	if err := s.commitEventLocked(sess, Event{
		Type:        EventCheckpoint,
		OccurredAt:  sess.LastActivityAt,
		Ref:         sess.CheckpointRef,
		Description: strings.TrimSpace(description),
		Provenance:  prov,
	}); err != nil {
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
	if err := s.commitEventLocked(sess, Event{Type: EventClose, OccurredAt: now, Provenance: prov}); err != nil {
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
	events := []Event{{Type: EventMessage, OccurredAt: sess.LastActivityAt, Message: &cp, Provenance: prov}}
	events = append(events, s.enforcePolicyLocked(sess, prov)...)
	return s.commitEventsLocked(sess, events...)
}

func (s *Service) RecordMessageAsync(clientID, threadID string, msg llm.Message, prov Provenance) error {
	s.asyncWG.Add(1)
	select {
	case s.asyncQueue <- queuedMessage{clientID: clientID, threadID: threadID, msg: msg, prov: prov}:
		return nil
	default:
		s.asyncWG.Done()
		return ErrAsyncQueueFull
	}
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
	if err := s.commitEventLocked(&child, Event{Type: EventBranch, OccurredAt: now, Ref: branchRef, Provenance: prov}); err != nil {
		return Session{}, err
	}
	parent.LastActivityAt = now
	if err := s.commitEventLocked(parent, Event{Type: EventBranch, OccurredAt: now, Ref: branchRef, Description: child.ID, Provenance: prov}); err != nil {
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
	sess.CommitRef = "commit-" + uuid.NewString()
	sess.LastActivityAt = time.Now().UTC()
	if err := s.commitEventLocked(sess, Event{
		Type:       EventCommit,
		OccurredAt: sess.LastActivityAt,
		Ref:        sess.CommitRef,
		Changes:    cloneStringMap(changes),
		Provenance: prov,
	}); err != nil {
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
	base.MergeRef = "merge-" + uuid.NewString()
	base.LastActivityAt = time.Now().UTC()
	if err := s.commitEventLocked(base, Event{
		Type:       EventMerge,
		OccurredAt: base.LastActivityAt,
		Ref:        base.MergeRef,
		Conflicts:  append([]string(nil), conflicts...),
		Provenance: prov,
	}); err != nil {
		return MergeResult{}, err
	}
	branch.LastActivityAt = base.LastActivityAt
	if err := s.commitEventLocked(branch, Event{
		Type:       EventMerge,
		OccurredAt: branch.LastActivityAt,
		Ref:        base.MergeRef,
		Conflicts:  append([]string(nil), conflicts...),
		Provenance: prov,
	}); err != nil {
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
	events, ok := s.events[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	out := append([]Event(nil), events...)
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

func (s *Service) Snapshot(sessionID string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return Session{}, false
	}
	return cloneSession(*sess), true
}

func (s *Service) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, cloneSession(*sess))
	}
	return out
}

func (s *Service) runAsyncWriter() {
	for item := range s.asyncQueue {
		_ = s.RecordMessage(item.clientID, item.threadID, item.msg, item.prov)
		s.asyncWG.Done()
	}
}

func (s *Service) findOpenLocked(clientID, threadID string) *Session {
	if id := s.active[threadKey(clientID, threadID)]; id != "" {
		return s.sessions[id]
	}
	for _, sess := range s.sessions {
		if sess.ClientID == strings.TrimSpace(clientID) && sess.ThreadID == strings.TrimSpace(threadID) && sess.Status == StatusOpen {
			s.active[threadKey(clientID, threadID)] = sess.ID
			return sess
		}
	}
	return nil
}

func (s *Service) commitEventLocked(sess *Session, evt Event) error {
	return s.commitEventsLocked(sess, evt)
}

func (s *Service) commitEventsLocked(sess *Session, events ...Event) error {
	for i := range events {
		events[i].Seq = sess.NextSeq
		events[i].SessionID = sess.ID
		events[i].State = cloneSessionState(sess)
		sess.NextSeq++
		s.events[sess.ID] = append(s.events[sess.ID], events[i])
	}
	sess.Events = append([]Event(nil), s.events[sess.ID]...)
	s.sessions[sess.ID] = sess
	if sess.Status == StatusOpen {
		s.active[threadKey(sess.ClientID, sess.ThreadID)] = sess.ID
	} else {
		delete(s.active, threadKey(sess.ClientID, sess.ThreadID))
	}
	if err := appendEventsFile(s.eventsPath, events); err != nil {
		return err
	}
	if shouldSnapshot(events) {
		if err := saveSnapshotsFile(s.snapshotsPath, s.snapshotMapLocked()); err != nil {
			return err
		}
	}
	return nil
}

func shouldSnapshot(events []Event) bool {
	for _, evt := range events {
		switch evt.Type {
		case EventOpen, EventCheckpoint, EventClose, EventBranch, EventCommit, EventMerge, EventSummary:
			return true
		}
	}
	return false
}

func (s *Service) snapshotMapLocked() map[string]Session {
	out := make(map[string]Session, len(s.sessions))
	for id, sess := range s.sessions {
		out[id] = cloneSession(*sess)
	}
	return out
}

func (s *Service) enforcePolicyLocked(sess *Session, prov Provenance) []Event {
	var events []Event
	maxMessages := sess.HistoryPolicy.RetentionMaxMessages
	if maxMessages > 0 && len(sess.Messages) > maxMessages {
		drop := len(sess.Messages) - maxMessages
		sess.Summary = mergeSummary(sess.Summary, summarizeMessages(sess.Messages[:drop]))
		sess.Messages = append([]llm.Message(nil), sess.Messages[drop:]...)
		events = append(events, Event{Type: EventSummary, OccurredAt: time.Now().UTC(), Summary: sess.Summary, Provenance: prov})
	}
	threshold := sess.HistoryPolicy.SummarizationThreshold
	keep := sess.HistoryPolicy.SummarizationKeepRecent
	if threshold > 0 && len(sess.Messages) >= threshold && keep >= 0 && len(sess.Messages) > keep {
		cut := len(sess.Messages) - keep
		sess.Summary = mergeSummary(sess.Summary, summarizeMessages(sess.Messages[:cut]))
		sess.Messages = append([]llm.Message(nil), sess.Messages[cut:]...)
		events = append(events, Event{Type: EventSummary, OccurredAt: time.Now().UTC(), Summary: sess.Summary, Provenance: prov})
	}
	return events
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
	if err := os.MkdirAll(filepath.Dir(s.eventsPath), 0o755); err != nil {
		return fmt.Errorf("mkdir session dir: %w", err)
	}
	snapshots, err := loadSnapshotsFile(s.snapshotsPath)
	if err != nil {
		return err
	}
	for id, sess := range snapshots {
		sess.Events = nil
		cp := cloneSession(sess)
		s.sessions[id] = &cp
		if cp.Status == StatusOpen {
			s.active[threadKey(cp.ClientID, cp.ThreadID)] = cp.ID
		}
	}
	events, err := loadEventsFile(s.eventsPath)
	if err != nil {
		return err
	}
	for _, evt := range events {
		cp := cloneEvent(evt)
		s.events[evt.SessionID] = append(s.events[evt.SessionID], cp)
		if cp.State != nil {
			sess := cloneSession(*cp.State)
			sess.Events = append([]Event(nil), s.events[evt.SessionID]...)
			s.sessions[evt.SessionID] = &sess
			if sess.Status == StatusOpen {
				s.active[threadKey(sess.ClientID, sess.ThreadID)] = sess.ID
			} else {
				delete(s.active, threadKey(sess.ClientID, sess.ThreadID))
			}
		}
	}
	return nil
}

func sessionPaths(path string) (string, string) {
	if strings.TrimSpace(path) == "" {
		path = "./.krill/sessions.json"
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	if base == "" {
		base = path
	}
	return base + ".events.jsonl", base + ".snapshots.json"
}

func appendEventsFile(path string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open session events file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, evt := range events {
		if err := enc.Encode(evt); err != nil {
			return fmt.Errorf("append session event: %w", err)
		}
	}
	return nil
}

func loadEventsFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open session events file: %w", err)
	}
	defer f.Close()
	var events []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt Event
		if err := json.Unmarshal(line, &evt); err != nil {
			return nil, fmt.Errorf("decode session event: %w", err)
		}
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan session events: %w", err)
	}
	return events, nil
}

func saveSnapshotsFile(path string, snapshots map[string]Session) error {
	payload, err := json.MarshalIndent(snapshots, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session snapshots: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write temp session snapshots: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace session snapshots: %w", err)
	}
	return nil
}

func loadSnapshotsFile(path string) (map[string]Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Session{}, nil
		}
		return nil, fmt.Errorf("read session snapshots: %w", err)
	}
	if len(data) == 0 {
		return map[string]Session{}, nil
	}
	var snapshots map[string]Session
	if err := json.Unmarshal(data, &snapshots); err != nil {
		return nil, fmt.Errorf("decode session snapshots: %w", err)
	}
	if snapshots == nil {
		snapshots = map[string]Session{}
	}
	return snapshots, nil
}

func cloneSessionState(in *Session) *Session {
	if in == nil {
		return nil
	}
	out := cloneSession(*in)
	out.Events = nil
	return &out
}

func cloneEvent(in Event) Event {
	out := in
	if in.Message != nil {
		cp := *in.Message
		out.Message = &cp
	}
	out.Changes = cloneStringMap(in.Changes)
	out.Conflicts = append([]string(nil), in.Conflicts...)
	out.State = cloneSessionState(in.State)
	return out
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

func threadKey(clientID, threadID string) string {
	return strings.TrimSpace(clientID) + ":" + strings.TrimSpace(threadID)
}
