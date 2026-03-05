#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART_DIR="$ROOT_DIR/deploy/charts/krill"
RELEASE="${KRILL_HELM_RELEASE:-krill}"
NAMESPACE="${KRILL_NAMESPACE:-krill}"
OVERLAY="${KRILL_VALUES_OVERLAY:-values-dev.yaml}"

helm upgrade --install "$RELEASE" "$CHART_DIR" \
  --namespace "$NAMESPACE" --create-namespace \
  -f "$CHART_DIR/values.yaml" \
  -f "$CHART_DIR/$OVERLAY"
