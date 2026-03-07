# Krill OpenShift Install (M0 Foundation)

## Prerequisites

- `oc` CLI authenticated to cluster
- Helm 3

## Install

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/install.sh
```

Provide control-plane tokens through OpenShift secrets or deployment env:

- `KRILL_CONTROL_VIEWER_TOKEN`
- `KRILL_CONTROL_OPERATOR_TOKEN`
- `KRILL_CONTROL_ADMIN_TOKEN`

If your project enforces random UIDs, ensure image and volume permissions are compliant with the OpenShift SCC used by the namespace.

PubSub profile overlays:

- use `KRILL_VALUES_OVERLAY=values-prodlike.yaml` for the production-like scaffold.
- `solace` is included as a documented adapter contract/config path in M1; runtime implementation remains staged.

## Remove

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/uninstall.sh
```

Post-install validation:

```bash
oc -n ${KRILL_NAMESPACE:-krill} port-forward deploy/krill 8080:8080
curl -H "Authorization: Bearer $KRILL_CONTROL_VIEWER_TOKEN" http://127.0.0.1:8080/v1/control/readiness
```

Operational runbook: `deploy/docs/runbook.md`
