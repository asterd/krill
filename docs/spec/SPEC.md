# Krill Enterprise Implementation Spec

## Status

- Version: `0.1-draft`
- Owners: `Krill Core Team`
- Last Updated: `2026-03-05`
- Audience: LLM coding agents and human maintainers

## Goal

Define an implementation contract for evolving Krill into an enterprise-grade, protocol-agnostic agent orchestration runtime with:

- multi-protocol ingress (`http`, `telegram`, `webhook`, `pubsub`, `a2a`)
- composable agent orchestration (single-agent and cooperative multi-agent)
- persistent long-running development sessions with resumable history
- low-overhead deep observability (OTEL traces/metrics/log correlation)
- policy-driven skill/capability governance and hardened code sandboxes

## External Pattern Alignment (Gap Snapshot)

This spec intentionally aligns with two external patterns:

1. Lightweight, extensible agent core (as seen in projects like nanobot).
2. Declarative + versioned context systems (as discussed in Wasteland/Dolt pattern).

Current Krill gaps to close incrementally:

1. Declarative orchestration schema is not yet first-class.
2. Versioned context/state (branch/merge/audit on structured agent context) is missing.
3. Skill management is lifecycle-oriented but not yet intelligence-oriented (ranking, trust, compatibility, policy-based auto-selection).
4. Federation-grade audit and provenance need stronger primitives.
5. Runtime kernel contracts are still too coarse-grained for the target performance and resilience envelope.

## Non-Goals (for this spec pack)

- building a full UI control plane in the first milestones
- replacing current APIs with breaking changes by default
- forcing a single PubSub backend vendor

## Product Constraints

1. Backward compatibility by default for existing config and protocol behavior.
2. Incremental delivery: each milestone must be deployable independently.
3. Test-first quality gates: high coverage and explicit non-regression suites.
4. Security-by-default for code execution and capability enforcement.
5. Observability overhead must remain bounded and measurable.
6. Local development and cluster runtime parity must be maintained.

## Guiding Principles

1. Stable Contracts First: version envelopes/config before new behavior.
2. Pluggability Over Forking: new protocols and runtimes via interfaces/adapters.
3. Data Durability for Long Sessions: persistent memory for backend workflows.
4. Deterministic Operations: idempotency, retries, bounded handoffs, bounded queues.
5. Explicit Policy Decisions: every allow/deny decision must be auditable.
6. DevEx by Default: one-command startup for local and mini-kube environments.

## Architecture (Target)

```text
Ingress Plugins (http/telegram/webhook/pubsub/a2a)
      -> Envelope Normalizer (v2 schema)
      -> Bus (local|external adapter)
      -> Orchestrator (single, planned, or cooperative multi-agent)
      -> Intent Resolver
      -> Planner
      -> Capability Selector + Policy Gate
      -> Plan Executor
      -> Agent Loop(s) + Skill Runtime(s)
      -> Reply Router (protocol-aware)
      -> Egress plugins

Cross-cutting:
- Session Store (persistent history/checkpoints/summaries)
- Capability Registry / Metadata Graph
- Policy Engine (capabilities, budgets, allow/deny)
- OTEL (traces, metrics, logs correlation)
- Audit/Event Stream
```

Planner-oriented execution requirements:

1. The system MUST support an explicit execution plan for non-trivial requests.
2. A plan MUST be auditable, replayable, and versionable inside session/provenance state.
3. Planning and execution MUST remain lightweight:
   - keep single-step conversational flows on the existing direct agent loop path
   - use the planner path only when the request requires multi-step tool orchestration, policy-sensitive execution, or retries/fallbacks
4. The planner MUST operate on a generic capability abstraction rather than hard-coding only skills.
5. Capability selection MUST occur before execution and MUST be policy-aware.

## Runtime Packaging and Environment Parity

Krill MUST provide two operational bootstrap paths with aligned behavior:

1. Local path (Docker-based):
   - `docker` + `docker-compose`
   - sandbox runtime enabled (`docker-sandbox` profile)
   - single startup script for full dependency graph
2. Cluster path (Kubernetes-based):
   - mini-kube startup path for local cluster validation
   - Helm chart as canonical packaging unit
   - install instructions for both Kubernetes and OpenShift

Required repository structure:

- `deploy/compose/docker-compose.yml`
- `deploy/compose/docker-compose.sandbox.yml`
- `deploy/scripts/dev/up.sh`
- `deploy/scripts/dev/down.sh`
- `deploy/scripts/dev/reset.sh`
- `deploy/charts/krill/` (Helm chart)
- `deploy/scripts/k8s/up-minikube.sh`
- `deploy/scripts/k8s/install.sh`
- `deploy/scripts/k8s/uninstall.sh`
- `deploy/docs/k8s.md`
- `deploy/docs/openshift.md`

Required behavior:

1. One-command local bootstrap starts all required services.
2. One-command mini-kube bootstrap deploys same logical stack via Helm.
3. Config overlays exist for dev, mini-kube, and production-like modes.
4. Sandbox mode parity is testable in both local and cluster paths.

## Domain Model

### EnvelopeV2

Required fields:

- `schema_version`
- `id`
- `client_id`
- `thread_id`
- `tenant`
- `workflow_id`
- `hop`
- `source_protocol`
- `role`
- `text`
- `meta`
- `capabilities`
- `created_at`

Compatibility:

- `v1 <-> v2` mapper is mandatory.
- New fields are optional-at-ingress and defaulted in normalizer.

### Session

- `session_id`
- `tenant`
- `client_id`
- `thread_id`
- `mode` (`ephemeral|persistent`)
- `history_policy` (window, retention, summarization thresholds)
- `checkpoint_ref` (optional)

### Workflow

- `workflow_id`
- `orchestration_mode` (`single|cooperative`)
- `participants` (agents)
- `budget` (tokens/time/tool calls/hops)

### OrgSchema (Declarative Agent Topology)

- `schema_id`
- `version`
- `roles` (router/specialist/synthesizer/custom)
- `responsibilities`
- `handoff_rules`
- `escalation_rules`
- `policy_bindings`

The system should execute against declared structures rather than imperative prompt wiring.

### VersionedContext

- `context_id`
- `base_ref`
- `branch_ref`
- `commit_ref`
- `merge_ref`
- `provenance`

Required semantics:

1. branch context for experiments/parallel agent work
2. merge context with conflict policy
3. immutable history for audit/replay

## Protocol Requirements

### PubSub (Generic Adapter Model)

PubSub support MUST be adapter-driven:

- `nats`
- `redis_streams`
- `kafka`
- `solace` (mandatory adapter target in roadmap)

Shared interface requirements:

- `Connect(ctx)`
- `Subscribe(ctx, topic, group)`
- `Publish(ctx, topic, envelope)`
- `Ack(msg)`
- `Nack(msg, retryPolicy)`
- `Close()`

Delivery semantics:

- at-least-once by default
- dedup key handling for idempotency
- dead-letter support and replay path

### A2A Ingress

Requirements:

- dedicated protocol plugin `a2a`
- strict schema validation
- support agent handoff metadata (`origin_agent`, `target_agent`, `handoff_reason`)
- preserve trace context across handoffs

## Long-Running Development Sessions

Krill MUST support backend-driven continuous coding sessions:

1. Persisted conversation history beyond in-memory windows.
2. Session resume by `session_id` after process restarts.
3. Configurable retention by tenant/project.
4. Optional summarization checkpoints to bound prompt size.
5. Thread-aware and branch/workspace metadata in `meta`.

Minimum APIs:

- `session.open`
- `session.resume`
- `session.checkpoint`
- `session.close`

Versioned session context requirements:

1. Optional branch-per-task behavior for long coding sessions.
2. Merge/checkpoint primitives for consolidating agent outputs.
3. Replayable history with deterministic ordering and provenance metadata.

## Cron Scheduling of Agents

Scheduler requirements:

1. Cron-triggered workflow start (per tenant and per agent profile).
2. Retry policy and missed-run behavior.
3. Concurrency policy (`allow`, `forbid`, `replace`).
4. Tracing and audit for every schedule trigger.
5. Dry-run mode for validation.

Minimum schedule object:

- `schedule_id`
- `cron_expr`
- `timezone`
- `target_agent_or_workflow`
- `payload_template`
- `concurrency_policy`
- `enabled`

## OTEL Requirements (Low Overhead)

Profiles:

- `off`
- `minimal`
- `standard`
- `debug`

Must-trace spans:

- ingress receive/validate
- bus publish/consume
- orchestrator route/select-agent
- agent turn
- llm call
- skill execute
- memory ops (append/get/trim/checkpoint)
- sandbox lifecycle
- scheduler trigger
- inter-agent handoff

Core metrics:

- `krill.active_loops`
- `krill.inbound_queue_depth`
- `krill.skill.activations_total`
- `krill.memory.ops_total`
- `krill.memory.bytes`
- `krill.sandbox.exec_duration_ms`
- `krill.agent.handoff_total`
- `krill.scheduler.trigger_total`
- `krill.session.resume_total`

Performance budgets:

- `minimal`: <= 3% avg CPU overhead
- `standard`: <= 8% avg CPU overhead

## Security and Sandbox Requirements

1. Capability model by tenant/protocol/agent/skill.
2. Sandbox profiles: `strict`, `balanced`, `extended`.
3. Network default-deny with allowlist override.
4. Filesystem scope isolation with explicit read/write roots.
5. CPU/memory/time quotas enforced per run.
6. Execution attestation (input/artifact hash, metadata signature).
7. Optional stronger isolation path (container/microVM).
8. OpenCode-compatible coding machine profile in roadmap scope.

## Intelligent Skill Management Requirements

Skill system must evolve from static registry to intelligence engine with:

1. Skill metadata graph:
   - capability tags
   - compatibility constraints
   - trust/security level
   - cost/latency profile
2. Policy-aware skill selection:
   - allow/deny/budget checks before selection
   - tenant/agent/protocol restrictions
3. Versioned skill lifecycle:
   - release channels (`stable`, `candidate`, `deprecated`)
   - compatibility checks against agent/org schema
4. Execution feedback loop:
   - success/failure rates
   - runtime performance signals
   - policy violation counters
5. Optional marketplace/federation import with signature verification.

## Planning and Capability-Oriented Execution Requirements

The runtime must evolve from implicit ReAct-only execution to a lightweight planning model.

Required primitives:

1. `IntentResolver`
   - normalize user request into goal, constraints, tenant/security context, and requested modality
2. `Plan`
   - explicit structure with steps, candidate capabilities, selected capability, budgets, timeouts, fallbacks, and status
3. `Capability`
   - generic abstraction covering skill, MCP endpoint, code execution, backend action, or delegated agent action
4. `CapabilitySelector`
   - deterministic selection using policy, budget, compatibility, trust, and latency/cost metadata
5. `PlanExecutor`
   - step execution with pre-execution policy checks, retries/fallbacks, result normalization, and audit emission

Execution rules:

1. Simple conversational turns MAY remain on the direct loop path.
2. Multi-step, high-risk, or policy-sensitive requests MUST go through explicit planning.
3. Every executed step MUST record rationale, selected capability, and policy decision inputs.
4. Planner output MUST be inspectable through session/audit records and future control-plane APIs.

## Test and Quality Contract (Mandatory)

For every milestone implementation:

1. Add or update:
   - unit tests
   - integration tests
   - non-regression tests
2. Coverage target:
   - modified files >= 85% (preferred >= 90%)
3. Mandatory commands:
   - `go test ./... -race -count=1`
   - `go test ./... -covermode=atomic -coverprofile=coverage.out`
4. If introducing adapters/external dependencies:
   - add deterministic integration test harness
   - add failure-injection tests (timeouts, network partition, duplicate delivery)
5. Deliver a short verification report in PR/summary:
   - files changed
   - tests added
   - coverage delta
   - risks and follow-ups

No milestone is complete if any quality gate fails.

## Milestone Pack

Detailed instructions per milestone:

- [M0 - Compatibility Foundation](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M0.md)
- [M1 - PubSub + External State](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M1.md)
- [M2 - OTEL Deep Observability](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M2.md)
- [M3 - A2A + Cooperative Orchestration](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M3.md)
- [M4 - Cron + Long Sessions](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M4.md)
- [M4.5 - Runtime Kernel Refactor](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M4.5.md)
- [M5 - Capability Governance + Lightweight Planning Engine](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M5.md)
- [M6 - Enterprise Control Plane + Planner Operations](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M6.md)
- [M7 - Product Completion + Enterprise Skill and Asset Runtime](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M7.md)
- [M8 - Graph Planner + Capability Retrieval + Composable Skill Runtime](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M8.md)
- [M8.5 - Performance Hardening + Latency Budgets + Runtime Tightening](/Users/ddurzo/Development/python/krill/docs/spec/milestones/M8.5.md)

Planning companion:

- [Priority Matrix](/Users/ddurzo/Development/python/krill/docs/spec/PRIORITY_MATRIX.md)

Runtime/deployment packaging requirements are distributed in:

- M0: local docker-compose + sandbox bootstrap foundation
- M1: pubsub adapter runtime profiles in local stack
- M6: Helm packaging + mini-kube + Kubernetes/OpenShift install flow

Declarative/versioned evolution distribution:

- M3: declarative org schema + cooperative execution compiler
- M4: versioned context/session primitives (branch/merge/checkpoint/audit)
- M4.5: runtime kernel refactor (event backbone + state plane + execution model)
- M5: lightweight planning engine + intelligent capability selection + trust/policy-aware execution
- M7: baseline product completion across channels, skills, schedules, and governed operational flows
- M8: advanced execution intelligence (graph planning + capability retrieval + composable nested runtime)
- M8.5: performance hardening and latency-budget enforcement for advanced planner/runtime paths

## Implementation Prompt Template (Reusable)

Use this exact prompt structure for any milestone:

1. "Implement milestone `<ID>` from the Krill spec pack (`docs/spec/SPEC.md` + `docs/spec/milestones/<ID>.md`) with backward compatibility."
2. "Follow the milestone scope strictly; do not add out-of-scope features."
3. "Apply test contract: unit + integration + non-regression, high coverage on touched files."
4. "Run and report:
   - `go test ./... -race -count=1`
   - `go test ./... -covermode=atomic -coverprofile=coverage.out`"
5. "Provide final report: architecture decisions, changed files, tests, coverage, residual risks."

## Milestone Kickoff Prompt (Universal)

Reusable prompt is provided in:

- [PROMPT_TEMPLATE.md](/Users/ddurzo/Development/python/krill/docs/spec/PROMPT_TEMPLATE.md)
