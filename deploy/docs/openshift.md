# Krill OpenShift Install (M0 Foundation)

## Prerequisites

- `oc` CLI authenticated to cluster
- Helm 3

## Install

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/install.sh
```

If your project enforces random UIDs, ensure image and volume permissions are compliant with the OpenShift SCC used by the namespace.

## Remove

```bash
KRILL_NAMESPACE=krill ./deploy/scripts/k8s/uninstall.sh
```
