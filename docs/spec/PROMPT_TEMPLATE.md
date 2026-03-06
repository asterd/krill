# Universal Milestone Kickoff Prompt

Usa questo prompt sostituendo `<MILESTONE_ID>` con `M0..M7` o `M4.5` quando richiesto.

```text
Implementa la milestone <MILESTONE_ID> usando come fonte unica:
- docs/spec/SPEC.md
- docs/spec/milestones/<MILESTONE_ID>.md

Obiettivi operativi:
1) Rispetta rigidamente lo scope della milestone.
2) Mantieni retrocompatibilità e non introdurre breaking changes non richieste.
3) Se la milestone include runtime/deploy, allinea:
   - path locale: docker + docker-compose + docker-sandbox con startup script unico
   - path cluster: helm + mini-kube script + install docs per k8s/openshift
4) Mantieni le configurazioni clear e versionate (default + overlay env specifici).

Quality gates obbligatori:
1) Aggiungi test unitari, integrazione e non-regressione.
2) Coverage sui file modificati >=85% (target >=90%).
3) Esegui e riporta:
   - go test ./... -race -count=1
   - go test ./... -covermode=atomic -coverprofile=coverage.out
4) Aggiungi benchmark/failure-injection se la milestone tocca performance o sistemi esterni.
5) Se un gate fallisce, non chiudere la milestone.

Output finale richiesto:
1) breve design note (decisioni principali e tradeoff)
2) elenco file modificati
3) elenco test aggiunti/aggiornati
4) risultati test e coverage (sintesi)
5) verifica exit criteria milestone (checklist pass/fail)
6) rischi residui e prossimi step
```

## Example

Per avviare M4:

```text
Implementa la milestone M4 usando come fonte unica:
- docs/spec/SPEC.md
- docs/spec/milestones/M4.md
...
```
