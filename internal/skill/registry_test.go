package skill

import (
	"context"
	"log/slog"
	"testing"

	"github.com/krill/krill/config"
)

type testExec struct{}

func (e *testExec) Execute(_ context.Context, argsJSON string) (string, error) {
	return "ok:" + argsJSON, nil
}

func TestRegistry_CoversCorePaths(t *testing.T) {
	skills := []config.SkillConfig{
		{Name: "s1", Description: "S1", Runtime: "builtin", Tags: []string{"tag-a"}},
		{Name: "s2", Description: "S2", Runtime: "builtin", Tags: []string{"tag-a"}},
	}
	r, err := NewRegistry(skills, config.CoreConfig{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterBuiltin("s1", "S1", &testExec{}, nil)
	r.RegisterBuiltin("s2", "S2", &testExec{}, nil)

	if !r.Exists("s1") || r.Count() != 2 {
		t.Fatalf("unexpected registry state: exists=%v count=%d", r.Exists("s1"), r.Count())
	}
	if _, ok := r.GetDef("s1"); !ok {
		t.Fatal("expected tool definition")
	}
	if len(r.AllNames()) != 2 {
		t.Fatal("expected 2 registered names")
	}

	v := NewView(r, []string{"s1"}, slog.Default())
	if !v.IsActive("s1") {
		t.Fatal("expected eager skill active")
	}
	if len(v.ActiveToolDefs()) != 1 {
		t.Fatal("expected one active tool definition")
	}
	if !v.Activate("s2") {
		t.Fatal("expected manual activate to succeed")
	}
	v.ActivateByTag("tag-a")
	if v.ActiveCount() != 2 {
		t.Fatalf("expected 2 active skills, got %d", v.ActiveCount())
	}

	out, lazy, err := v.Execute(context.Background(), "s2", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" || (lazy != AlreadyActive && lazy != LazyLoaded) {
		t.Fatalf("unexpected execute result: out=%q lazy=%d", out, lazy)
	}
	if _, _, err := v.Execute(context.Background(), "missing", `{}`); err == nil {
		t.Fatal("expected execute error for missing skill")
	}
}

func TestNewRegistry_UnknownRuntimeFails(t *testing.T) {
	_, err := NewRegistry([]config.SkillConfig{{Name: "x", Runtime: "bad"}}, config.CoreConfig{}, slog.Default())
	if err == nil {
		t.Fatal("expected unknown runtime error")
	}
}

func TestViewActivateMissingAndRegistryMisconfiguredEntry(t *testing.T) {
	r, err := NewRegistry(nil, config.CoreConfig{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	v := NewView(r, nil, slog.Default())
	if v.Activate("ghost") {
		t.Fatal("activate should fail for missing skill")
	}
	r.mu.Lock()
	r.entries["broken"] = &entry{cfg: config.SkillConfig{Name: "broken"}}
	r.mu.Unlock()
	if _, _, err := v.Execute(context.Background(), "broken", `{}`); err == nil {
		t.Fatal("expected error for skill without executor")
	}
}
