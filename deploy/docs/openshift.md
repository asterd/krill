# Krill OpenShift Install (M0 Foundation)

## Prerequisites

- `oc` CLI authenticated to cluster
- Helm 3

## Install

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/install.sh
```

If your project enforces random UIDs, ensure image and volume permissions are compliant with the OpenShift SCC used by the namespace.

PubSub profile overlays:

- use `KRILL_VALUES_OVERLAY=values-prodlike.yaml` for the production-like scaffold.
- `solace` is included as a documented adapter contract/config path in M1; runtime implementation remains staged.

## Remove

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/uninstall.sh
```
