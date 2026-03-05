#!/usr/bin/env bash
set -euo pipefail

RELEASE="${KRILL_HELM_RELEASE:-krill}"
NAMESPACE="${KRILL_NAMESPACE:-krill}"

helm uninstall "$RELEASE" --namespace "$NAMESPACE"
