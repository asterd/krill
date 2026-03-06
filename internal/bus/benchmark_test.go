package bus

import (
	"context"
	"testing"
	"time"
)

func BenchmarkLocalBackboneThroughput(b *testing.B) {
	backbone := NewLocal(1024)
	sub := backbone.Subscribe("bench")

	env := &Envelope{
		ID:             "bench",
		ClientID:       "client",
		ThreadID:       "thread",
		Role:           RoleUser,
		Text:           "payload",
		SourceProtocol: "bench",
		CreatedAt:      time.Now().UTC(),
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := backbone.Publish(context.Background(), "bench", env); err != nil {
			b.Fatal(err)
		}
		<-sub
	}
	b.StopTimer()
	backbone.Unsubscribe("bench")
}
