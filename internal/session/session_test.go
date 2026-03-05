package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     filepath.Join(t.TempDir(), "sessions.json"),
		RetentionMaxMessages:     6,
		SummarizationThreshold:   4,
		SummarizationKeepRecent:  2,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestLifecycleResumeCheckpointAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	svc, err := NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     path,
		RetentionMaxMessages:     10,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}

	sess, err := svc.Open(OpenRequest{
		Tenant:   "tenant-a",
		ClientID: "client-a",
		ThreadID: "thread-a",
		Mode:     ModePersistent,
	}, Provenance{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordMessage("client-a", "thread-a", llm.Message{Role: "user", Content: "hello"}, Provenance{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Checkpoint(sess.ID, "before restart", Provenance{Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     path,
		RetentionMaxMessages:     10,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed, ok, err := restarted.ResumeByThread("client-a", "thread-a", Provenance{Actor: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected session resume after restart")
	}
	if resumed.CheckpointRef == "" {
		t.Fatal("expected checkpoint ref to persist")
	}
	msgs, ok, err := restarted.RestoreMessagesByThread("client-a", "thread-a")
	if err != nil || !ok {
		t.Fatalf("restore failed ok=%v err=%v", ok, err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("unexpected restored messages: %+v", msgs)
	}
	if _, err := restarted.Close(resumed.ID, Provenance{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizationAndRetentionPolicies(t *testing.T) {
	svc := newTestService(t)
	sess, err := svc.Open(OpenRequest{ClientID: "c1", ThreadID: "t1", Mode: ModePersistent}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"one", "two", "three", "four", "five"} {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := svc.RecordMessage("c1", "t1", llm.Message{Role: role, Content: text}, Provenance{}); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := svc.Resume(sess.ID, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Summary == "" {
		t.Fatal("expected summary checkpoint to be generated")
	}
	if len(reloaded.Messages) > 3 {
		t.Fatalf("expected bounded recent window, got %d messages", len(reloaded.Messages))
	}
	msgs, err := svc.RestoreMessages(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("expected summary system message first, got %+v", msgs)
	}
}

func TestBranchMergeCommitReplayDeterministic(t *testing.T) {
	svc := newTestService(t)
	base, err := svc.Open(OpenRequest{ClientID: "c2", ThreadID: "t2", Mode: ModePersistent}, Provenance{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	base, err = svc.Commit(base.ID, map[string]string{"theme": "blue", "mode": "draft"}, Provenance{Actor: "base"})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := svc.Branch(base.ID, Provenance{Actor: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if branch.ThreadID == base.ThreadID {
		t.Fatal("expected branch thread id to diverge from parent for deterministic routing")
	}
	if _, err := svc.Commit(branch.ID, map[string]string{"theme": "green", "extra": "yes"}, Provenance{Actor: "branch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(base.ID, map[string]string{"theme": "red"}, Provenance{Actor: "base"}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Merge(base.ID, branch.ID, MergeFail, Provenance{Actor: "merge"}); err == nil {
		t.Fatal("expected conflict on fail policy")
	}
	merged, err := svc.Merge(base.ID, branch.ID, MergeLWW, Provenance{Actor: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Conflicts) != 1 || merged.Conflicts[0] != "theme" {
		t.Fatalf("expected theme conflict, got %+v", merged.Conflicts)
	}
	if got := merged.Session.Context["theme"]; got != "green" {
		t.Fatalf("expected lww branch value, got %s", got)
	}
	if got := merged.Session.Context["extra"]; got != "yes" {
		t.Fatalf("expected merged branch key, got %s", got)
	}

	events, err := svc.Replay(base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 {
		t.Fatalf("expected replay events, got %d", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].Seq >= events[i].Seq {
			t.Fatalf("expected deterministic ascending replay, got %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
}

func TestManualMergeAndUtilityBranches(t *testing.T) {
	svc := newTestService(t)
	base, err := svc.Open(OpenRequest{ClientID: "c3", ThreadID: "t3", Mode: ModePersistent}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(base.ID, map[string]string{"k": "base"}, Provenance{}); err != nil {
		t.Fatal(err)
	}
	branch, err := svc.Branch(base.ID, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(branch.ID, map[string]string{"k": "branch"}, Provenance{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(base.ID, map[string]string{"k": "updated"}, Provenance{}); err != nil {
		t.Fatal(err)
	}
	merged, err := svc.Merge(base.ID, branch.ID, MergeManual, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Conflicts) != 1 {
		t.Fatalf("expected manual merge conflict, got %+v", merged.Conflicts)
	}
	if merged.Session.Context["k"] != "updated" {
		t.Fatalf("manual merge should keep base value, got %+v", merged.Session.Context)
	}

	if summary := summarizeMessages(nil); summary != "" {
		t.Fatalf("expected empty summary for nil messages, got %q", summary)
	}
	if got := mergeSummary("", "next"); got != "next" {
		t.Fatalf("unexpected mergeSummary empty current: %q", got)
	}
	if got := mergeSummary("current", ""); got != "current" {
		t.Fatalf("unexpected mergeSummary empty next: %q", got)
	}
}

func TestNewServiceLoadCorruptedAndOpenValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(config.SessionConfig{Enabled: true, Path: path}); err == nil {
		t.Fatal("expected corrupted session file error")
	}

	svc := newTestService(t)
	if _, err := svc.Open(OpenRequest{}, Provenance{}); err == nil {
		t.Fatal("expected open validation error")
	}
	if _, err := svc.Resume("missing", Provenance{}); err == nil {
		t.Fatal("expected resume missing error")
	}
	if _, err := svc.RestoreMessages("missing"); err == nil {
		t.Fatal("expected restore missing error")
	}
	if _, err := svc.Replay("missing"); err == nil {
		t.Fatal("expected replay missing error")
	}
}
