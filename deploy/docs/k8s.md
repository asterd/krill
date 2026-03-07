# Krill Kubernetes Install (M0 Foundation)

## Prerequisites

- Docker
- Helm 3
- Kubernetes cluster access (`kubectl`)

## Install

```bash
./deploy/scripts/k8s/install.sh
```

Control-plane tokens should be provided through your values overlay or cluster secret/env injection:

- `KRILL_CONTROL_VIEWER_TOKEN`
- `KRILL_CONTROL_OPERATOR_TOKEN`
- `KRILL_CONTROL_ADMIN_TOKEN`

Use overlays via `KRILL_VALUES_OVERLAY`:

```bash
KRILL_VALUES_OVERLAY=values-prodlike.yaml ./deploy/scripts/k8s/install.sh
```

PubSub profiles (M1):

- `values-dev.yaml`: `nats` profile scaffold.
- `values-minikube.yaml`: `redis_streams` profile scaffold.
- `values-prodlike.yaml`: `solace` roadmap scaffold (contract/config only).

## Minikube path

```bash
./deploy/scripts/k8s/up-minikube.sh
```

After install, validate parity and control-plane readiness:

```bash
kubectl port-forward deploy/krill 8080:8080 -n ${KRILL_NAMESPACE:-krill-dev}
curl -H "Authorization: Bearer $KRILL_CONTROL_VIEWER_TOKEN" http://127.0.0.1:8080/v1/control/readiness
curl -H "Authorization: Bearer $KRILL_CONTROL_VIEWER_TOKEN" http://127.0.0.1:8080/v1/control/diagnostics
```

Local compose bootstrap with PubSub profile parity:

```bash
KRILL_PUBSUB_PROFILE=nats ./deploy/scripts/dev/up.sh
KRILL_PUBSUB_PROFILE=redis ./deploy/scripts/dev/up.sh
```

## Uninstall

```bash
./deploy/scripts/k8s/uninstall.sh
```

Operational runbook: `deploy/docs/runbook.md`
