# Krill Kubernetes Install (M0 Foundation)

## Prerequisites

- Docker
- Helm 3
- Kubernetes cluster access (`kubectl`)

## Install

```bash
./deploy/scripts/k8s/install.sh
```

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

Local compose bootstrap with PubSub profile parity:

```bash
KRILL_PUBSUB_PROFILE=nats ./deploy/scripts/dev/up.sh
KRILL_PUBSUB_PROFILE=redis ./deploy/scripts/dev/up.sh
```

## Uninstall

```bash
./deploy/scripts/k8s/uninstall.sh
```
