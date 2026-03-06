// Package core_test — integration tests covering all core paths.
// Run: go test ./... -race -count=1 -v
package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
)

// ─── Bus ──────────────────────────────────────────────────────────────────────

func TestBus_PublishSubscribe(t *testing.T) {
	bus.SetReplyPrefix("__reply__")
	b := bus.NewLocal(16)
	ch := b.Subscribe("test-key")
	env := &bus.Envelope{ID: "1", ClientID: "c1", Role: bus.RoleUser, CreatedAt: time.Now()}
	b.Publish(context.Background(), "test-key", env)
	select {
	case got := <-ch:
		if got.ID != "1" {
			t.Fatalf("wrong id: %s", got.ID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestBus_ReplyKeyIsolation(t *testing.T) {
	bus.SetReplyPrefix("__reply__")
	b := bus.NewLocal(8)
	httpCh := b.Subscribe(bus.ReplyKey("http"))
	tgCh := b.Subscribe(bus.ReplyKey("telegram"))

	reply := &bus.Envelope{ID: "r1", ClientID: "c1", Role: bus.RoleAssistant, Text: "hello", CreatedAt: time.Now()}
	b.Publish(context.Background(), bus.ReplyKey("http"), reply)

	select {
	case got := <-httpCh:
		if got.ID != "r1" {
			t.Fatalf("wrong id")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("http did not receive reply")
	}

	select {
	case msg := <-tgCh:
		t.Fatalf("telegram received wrong reply: %v", msg.ID)
	case <-time.After(20 * time.Millisecond):
		// correct
	}
}

func TestBus_SlowConsumerDropsOldest(t *testing.T) {
	b := bus.NewLocal(2)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		b.Publish(ctx, "flood", &bus.Envelope{ID: uuid.NewString(), CreatedAt: time.Now()})
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	b := bus.NewLocal(4)
	b.Subscribe("k")
	b.Unsubscribe("k")
	b.Unsubscribe("k") // double unsubscribe must not panic
}

func TestBus_InboundKey(t *testing.T) {
	bus.SetReplyPrefix("__reply__")
	if bus.InboundKey == "" {
		t.Fatal("InboundKey must not be empty")
	}
	if bus.ReplyKey("http") == bus.ReplyKey("telegram") {
		t.Fatal("reply keys must differ per protocol")
	}
}

func TestBus_ReplyKeyCustomPrefix(t *testing.T) {
	bus.SetReplyPrefix("krill-reply")
	defer bus.SetReplyPrefix("__reply__")
	if got := bus.ReplyKey("http"); got != "krill-reply:http" {
		t.Fatalf("unexpected custom reply key %q", got)
	}
}

// ─── Memory ───────────────────────────────────────────────────────────────────

func TestMemory_AppendAndGet(t *testing.T) {
	m := memory.NewRAM()
	_ = m.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"})
	_ = m.Append("c1", "t1", llm.Message{Role: "assistant", Content: "hi"})
	msgs, err := m.Get("c1", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestMemory_WindowTruncation(t *testing.T) {
	m := memory.NewRAM()
	for i := 0; i < 50; i++ {
		_ = m.Append("c1", "t1", llm.Message{Role: "user", Content: "msg"})
	}
	msgs, err := m.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 10 {
		t.Fatalf("expected 10 windowed messages, got %d", len(msgs))
	}
}

func TestMemory_ThreadIsolation(t *testing.T) {
	m := memory.NewRAM()
	_ = m.Append("alice", "t1", llm.Message{Role: "user", Content: "alice msg"})
	_ = m.Append("bob", "t1", llm.Message{Role: "user", Content: "bob msg"})
	alice, err := m.Get("alice", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := m.Get("bob", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 1 || alice[0].Content != "alice msg" {
		t.Fatal("alice's history wrong")
	}
	if len(bob) != 1 || bob[0].Content != "bob msg" {
		t.Fatal("bob's history wrong")
	}
}

func TestMemory_Clear(t *testing.T) {
	m := memory.NewRAM()
	_ = m.Append("c1", "t1", llm.Message{Role: "user", Content: "hi"})
	_ = m.Clear("c1", "t1")
	msgs, err := m.Get("c1", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatal("expected empty after clear")
	}
}

// ─── Skill registry ───────────────────────────────────────────────────────────

type echoExec struct{}

func (e *echoExec) Execute(_ context.Context, args string) (string, error) {
	return "echo:" + args, nil
}

func TestSkill_RegisterAndExecute(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("echo", "Echo test", &echoExec{}, nil)

	result, err := sr.Execute(context.Background(), "echo", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result, "echo:") {
		t.Fatalf("unexpected: %s", result)
	}
}

func TestSkill_UnknownReturnsError(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	_, err := sr.Execute(context.Background(), "ghost", "{}")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSkill_ToolDef_JSONSchema(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	sr.RegisterBuiltin("search", "web search", &echoExec{}, schema)

	td, ok := sr.GetDef("search")
	if !ok {
		t.Fatal("GetDef returned false")
	}
	if td.Function.Name != "search" {
		t.Fatalf("wrong name: %s", td.Function.Name)
	}
	if string(td.Function.Parameters) != string(schema) {
		t.Fatalf("schema mismatch")
	}
}

// ─── Skill View (lazy loading) ────────────────────────────────────────────────

func TestSkillView_EagerActivation(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("a", "A", &echoExec{}, nil)
	sr.RegisterBuiltin("b", "B", &echoExec{}, nil)
	sr.RegisterBuiltin("c", "C", &echoExec{}, nil)

	view := skill.NewView(sr, []string{"a", "b"}, slog.Default())
	if view.ActiveCount() != 2 {
		t.Fatalf("expected 2 active, got %d", view.ActiveCount())
	}
	if !view.IsActive("a") || !view.IsActive("b") {
		t.Fatal("a and b should be active")
	}
	if view.IsActive("c") {
		t.Fatal("c should not be active")
	}
}

func TestSkillView_LazyAutoPromotion(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("lazy_skill", "Lazy", &echoExec{}, nil)

	view := skill.NewView(sr, nil, slog.Default())
	if view.ActiveCount() != 0 {
		t.Fatal("expected 0 active initially")
	}

	result, lazyResult, err := view.Execute(context.Background(), "lazy_skill", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if lazyResult != skill.LazyLoaded {
		t.Fatalf("expected LazyLoaded, got %d", lazyResult)
	}
	if !strings.HasPrefix(result, "echo:") {
		t.Fatalf("unexpected result: %s", result)
	}
	if !view.IsActive("lazy_skill") {
		t.Fatal("skill should be active after lazy load")
	}
	if view.ActiveCount() != 1 {
		t.Fatalf("expected 1 active after lazy load, got %d", view.ActiveCount())
	}
}

func TestSkillView_ActiveToolDefs_OnlyActive(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("visible", "Visible", &echoExec{}, nil)
	sr.RegisterBuiltin("hidden", "Hidden", &echoExec{}, nil)

	view := skill.NewView(sr, []string{"visible"}, slog.Default())
	defs := view.ActiveToolDefs()
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool def, got %d", len(defs))
	}
	if defs[0].Function.Name != "visible" {
		t.Fatalf("wrong tool exposed: %s", defs[0].Function.Name)
	}
}

func TestSkillView_ActivateByTag(t *testing.T) {
	skills := []config.SkillConfig{
		{Name: "s1", Description: "S1", Runtime: "builtin", Tags: []string{"research"}},
		{Name: "s2", Description: "S2", Runtime: "builtin", Tags: []string{"research"}},
		{Name: "s3", Description: "S3", Runtime: "builtin", Tags: []string{"coding"}},
	}
	sr, _ := skill.NewRegistry(skills, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("s1", "S1", &echoExec{}, nil)
	sr.RegisterBuiltin("s2", "S2", &echoExec{}, nil)
	sr.RegisterBuiltin("s3", "S3", &echoExec{}, nil)

	view := skill.NewView(sr, nil, slog.Default())
	view.ActivateByTag("research")

	if view.ActiveCount() != 2 {
		t.Fatalf("expected 2 active after tag activation, got %d", view.ActiveCount())
	}
	if view.IsActive("s3") {
		t.Fatal("s3 (coding tag) should not be active")
	}
}

// ─── Concurrency safety ───────────────────────────────────────────────────────

func TestBus_RaceConcurrentPublish(t *testing.T) {
	b := bus.NewLocal(512)
	ctx := context.Background()
	done := make(chan struct{}, 200)
	for i := 0; i < 100; i++ {
		go func() {
			b.Publish(ctx, "k1", &bus.Envelope{ID: uuid.NewString(), CreatedAt: time.Now()})
			done <- struct{}{}
		}()
		go func() {
			b.Subscribe("k1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 200; i++ {
		<-done
	}
}

func TestSkillRegistry_ConcurrentExecute(t *testing.T) {
	sr, _ := skill.NewRegistry(nil, config.CoreConfig{}, slog.Default())
	sr.RegisterBuiltin("par", "Parallel", &echoExec{}, nil)

	done := make(chan error, 100)
	for i := 0; i < 100; i++ {
		go func() {
			_, err := sr.Execute(context.Background(), "par", `{}`)
			done <- err
		}()
	}
	for i := 0; i < 100; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent execute error: %v", err)
		}
	}
}

func TestMemory_ConcurrentAppend(t *testing.T) {
	m := memory.NewRAM()
	done := make(chan struct{}, 100)
	for i := 0; i < 100; i++ {
		go func() {
			_ = m.Append("c1", "t1", llm.Message{Role: "user", Content: "msg"})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
}

// keep fmt used
var _ = fmt.Sprintf
