package session

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
)

func BenchmarkSessionRestoreLatency(b *testing.B) {
	dir := b.TempDir()
	cfg := config.SessionConfig{
		Enabled:                  true,
		Path:                     filepath.Join(dir, "sessions.json"),
		RetentionMaxMessages:     32,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	}
	svc, err := NewService(cfg)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		sess, err := svc.Open(OpenRequest{
			ClientID: "bench-client-" + strconv.Itoa(i),
			ThreadID: "bench-thread-" + strconv.Itoa(i),
			Mode:     ModePersistent,
		}, Provenance{Actor: "bench"})
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 8; j++ {
			if err := svc.RecordMessage(sess.ClientID, sess.ThreadID, llm.Message{Role: "user", Content: "message"}, Provenance{Actor: "bench"}); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := svc.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		restarted, err := NewService(cfg)
		if err != nil {
			b.Fatal(err)
		}
		if err := restarted.Shutdown(); err != nil {
			b.Fatal(err)
		}
	}
}
