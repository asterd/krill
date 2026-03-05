# Krill 🦀

> Ultra-light · Security-first · Multi-protocol · Multi-agent orchestrator  
> ~2700 lines of Go. Zero frameworks. One binary. Plug-n-play protocols.

---

## Quickstart

```bash
# 1. Clone and enter
git clone <your-repo> krill && cd krill

# 2. Install dependencies (only uuid + yaml)
make tidy

# 3. Copy and edit config
cp krill.yaml.example krill.yaml
# → set OPENAI_API_KEY or edit base_url for local LLM
# → alternatively create .env (same folder as krill.yaml), e.g. GROQ_API_KEY=...
# → choose log format with core.log_format: json|text

# 4. Build and run
make run

# 5. Send a message
curl -X POST http://localhost:8080/v1/chat \
  -H "Content-Type: application/json" \
  -d '{"client_id":"test","message":"What is 2+2?"}'
```

---

## Architecture

```
Protocols (HTTP/Telegram/Webhook)
        │  normalize to bus.Envelope
        ▼
   Internal Bus  (__inbound__ / __reply__:<proto>)
        │
        ▼
   Orchestrator  (per-client fan-out, semaphore)
        │  creates one Loop per client
        ▼
   Agent Loop    (ReAct: think → tool → observe)
        │  uses skill.View (progressive loading)
        ▼
   Skill Registry  (builtin / exec / wasm sandboxes)
```

**Lazy skill loading (DeerFlow pattern):**  
Each agent loop has a `skill.View` — only `eager_skills` are sent to the LLM.  
When the LLM calls a skill not yet active, it auto-promotes on first use.  
This keeps context lean with 50+ registered skills.

---

## Project Structure

```
krill/
├── cmd/krill/main.go          ← entry point (40 lines)
├── config/config.go              ← typed YAML schema
├── internal/
│   ├── bus/bus.go                ← pub/sub backbone (interface + local impl)
│   ├── core/
│   │   ├── engine.go             ← wires everything together
│   │   └── registry.go           ← plugin factory registry (no circular imports)
│   ├── orchestrator/orchestrator.go  ← per-client loop manager
│   ├── agent/loop.go             ← ReAct loop with lazy skill loading
│   ├── skill/registry.go         ← marketplace + View (progressive loading)
│   ├── memory/memory.go          ← per-thread conversation history
│   ├── llm/llm.go                ← OpenAI-compatible pool
│   └── sandbox/sandbox.go        ← exec (ephemeral tmpfs) + wasm stub + noop
└── plugins/
    ├── protocol/
    │   ├── http/http.go          ← REST + SSE
    │   ├── telegram/telegram.go  ← long-poll bot
    │   └── webhook/webhook.go    ← generic (Slack/Discord/WhatsApp)
    └── skill/builtins/builtins.go ← http_fetch, json_format, json_extract
```

---

## Adding a Protocol Plugin

```go
// plugins/protocol/myproto/myproto.go
package myproto

import "github.com/krill/krill/internal/core"

func init() {
    core.Global().RegisterProtocol("myproto", func(cfg map[string]interface{}) (core.Protocol, error) {
        return &Plugin{addr: cfg["addr"].(string)}, nil
    })
}

type Plugin struct{ addr string }
func (p *Plugin) Name() string { return "myproto" }
func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error { ... }
func (p *Plugin) Stop(ctx context.Context) error { ... }
```

Then add `_ "github.com/krill/krill/plugins/protocol/myproto"` to `cmd/krill/main.go`.

---

## Adding a Skill

**Option A — Go builtin** (in `plugins/skill/builtins/builtins.go`):
```go
sr.RegisterBuiltin("my_skill", "Does X", &myExecutor{}, jsonSchemaRaw)
```

**Option B — Any executable** (stdin JSON → stdout result):
```yaml
skills:
  - name: my_script
    description: "Does Y"
    runtime: exec
    path: ./skills/my_script.py
    input_schema: '{"type":"object","required":["input"],"properties":{"input":{"type":"string"}}}'
    tags: [nlp]
```

**Option C — WASM** (build with `-tags wasm` + add `wazero` dep):
```yaml
skills:
  - name: my_wasm
    runtime: wasm
    path: ./skills/my_skill.wasm
```

---

## Lazy Loading: Eager vs Lazy Skills

```yaml
agents:
  - name: default
    eager_skills:           # always in LLM context
      - http_fetch
      - json_format
      - code_exec
    # All other registered skills are lazy:
    # they activate automatically on first use,
    # or manually via view.ActivateByTag("research")
```

---

## Running Tests

```bash
make test
# or with verbose output:
go test ./... -race -v -count=1
```

---

## Coding Sandbox Skill (`code_exec`)

`code_exec` is a built-in skill for lightweight "opencode"-style execution:
- creates an ephemeral workspace per invocation
- writes one or more files
- executes `python3`, `node`, or `bash` entrypoint
- returns structured JSON (`ok`, `exit_code`, `stdout`, `stderr`, `duration_ms`)

Example call through the agent:

```bash
curl -N -X POST http://localhost:8080/v1/chat \
  -H "Content-Type: application/json" \
  -d '{"client_id":"test","message":"Use code_exec to run python code that prints the first 10 fibonacci numbers, then return only the final output."}'
```

Observability fields now included in logs/SSE payload:
- `trace_id`, `request_id`
- span logs (`span start` / `span end`) for HTTP ingress, agent turn, tool call
- token usage (`tokens_prompt`, `tokens_completion`, `tokens_total`) aggregated per request
- optional generated code logging: `core.log_generated_code: true`

---

## Multi-node Scaling

The `bus.Bus` interface is the only coupling point. Replace `bus.NewLocal()` in `engine.go` with a NATS or Redis Streams implementation — zero other changes needed.

```go
// Future: one line change in internal/core/engine.go
b := natsbus.New(cfg.NATS.URL, cfg.Core.BusBuffer)
```

---

## Security Model

| Layer | Mechanism |
|-------|-----------|
| HTTP auth | Optional Bearer token |
| Webhook auth | HMAC-SHA256 signature verification |
| Telegram | Bot token |
| exec skills | Ephemeral tmpdir + minimal env (HOME/TMPDIR/PATH only) |
| WASM skills | CPU fuel limit, no syscalls (wazero WASI restrictions) |
| HTTP input | 1MB body limit, 512KB response limit |
| Container | Distroless non-root, no shell |
