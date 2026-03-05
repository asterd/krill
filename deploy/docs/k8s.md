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

## Minikube path

```bash
./deploy/scripts/k8s/up-minikube.sh
```

## Uninstall

```bash
./deploy/scripts/k8s/uninstall.sh
```
