# Krill Priority Matrix (Feature vs Milestone vs Risk)

## Scopo

Matrice decisionale rapida per ordinare l'esecuzione reale delle feature evolutive in base a valore, rischio e dipendenze.

Legenda:

- Impatto: `H` alto, `M` medio, `L` basso
- Rischio: `H` alto, `M` medio, `L` basso
- Effort: `H` alto, `M` medio, `L` basso

## Matrice

| Feature | Milestone Target | Impatto | Rischio | Effort | Dipendenze | Gap che chiude | Note Operative |
|---|---|---|---|---|---|---|---|
| Envelope v2 + compat mapper | M0 | H | M | M | nessuna | Contratti stabili | Prerequisito per tutto il resto |
| Bootstrap locale one-command (docker/compose/sandbox) | M0 | H | L | M | nessuna | DevEx/parity | Riduce tempo ciclo sviluppo/test |
| PubSub generic adapter | M1 | H | M | H | M0 | Ingress enterprise | Inizia con 2 adapter runnable |
| Solace adapter scaffold + config contract | M1 | M | L | L | M1 base | Vendor extensibility | Implementazione piena può essere iterativa |
| External state (memory/session durable) | M1 | H | H | H | M0 | Sessioni lunghe/resilience | Fondamentale per backend coding continuativo |
| OTEL deep tracing + metrics profiles | M2 | H | M | M | M0-M1 | Operabilità enterprise | Tenere overhead sotto budget |
| Declarative OrgSchema | M3 | H | M | H | M0-M2 | Architettura dichiarativa | Evitare hardcoding imperativo handoff |
| A2A ingress + cooperative handoff | M3 | H | M | M | M0-M2 | Multi-agent interoperabile | Policy hop/budget obbligatorie |
| Cron scheduler | M4 | M | M | M | M0-M3 | Automazione workload | Aggiungere deterministic clock nei test |
| Versioned context (branch/merge/commit) | M4 | H | H | H | M1-M3 | Contesto versionato/audit | Cuore “organizational intelligence” |
| Runtime kernel refactor (bus/state/execution) | M4.5 | H | H | H | M0-M4 | Fondamenta runtime corrette | Consentita rottura compat per kernel più semplice |
| Intelligent capability engine (metadata/ranking/channels) | M5 | H | H | H | M3-M4.5 | Capability intelligence | Abilitare rollout `monitor -> enforce` |
| Capability policy + sandbox hardening | M5 | H | H | H | M2-M4.5 | Sicurezza runtime | Test evasione obbligatori |
| OpenCode-compatible coding profile | M5 | M | M | M | M5 sandbox | Continuità coding backend | Vincolare capabilities per tenant |
| Helm chart + minikube bootstrap | M6 | H | M | M | M0-M5 | Parità cluster | Script one-command anche per mini-kube |
| Kubernetes/OpenShift install docs + validation | M6 | M | L | M | M6 Helm | Enterprise adoption | Validare SCC/security context OpenShift |
| Control Plane API + RBAC + audit | M6 | H | H | H | M2-M5 | Governance enterprise | Preferire rollout progressivo |

## Ordine Consigliato Reale (Execution Wave)

1. Wave 1 (stabilità base): `M0` completo.
2. Wave 2 (durabilità + ingress): `M1` completo.
3. Wave 3 (osservabilità): `M2` completo.
4. Wave 4 (orchestrazione dichiarativa): `M3` completo.
5. Wave 5 (cron + contesto versionato): `M4` completo.
6. Wave 6 (kernel runtime): `M4.5` completo.
7. Wave 7 (planning, capability intelligence, sicurezza hard): `M5` completo.
8. Wave 8 (productization cluster/governance): `M6` completo.

## Risk Hotspots (da monitorare)

1. `M1` external state migration: rischio regressione latenza e consistency.
2. `M2` OTEL overhead: rischio di superare budget CPU in carico.
3. `M4` merge semantics: rischio conflitti non deterministici in sessioni lunghe.
4. `M4.5` kernel refactor: rischio di introdurre regressioni nel path base se il nuovo contract non è testato bene.
5. `M5` policy + intelligent selector: rischio blocchi falsi positivi o selection bias.
6. `M6` control plane coupling: rischio impatti sul data plane in failure mode.

## KPI Gate per Go/No-Go tra milestone

1. Nessuna regressione funzionale sui protocolli legacy.
2. Coverage file modificati >= 85% (target >= 90%).
3. Test non-regressione verdi.
4. Benchmark/overhead in target per milestone (quando richiesto).
5. Evidenza documentata di exit criteria raggiunti.
