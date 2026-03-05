#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART_DIR="$ROOT_DIR/deploy/charts/krill"
RELEASE="${KRILL_HELM_RELEASE:-krill}"
NAMESPACE="${KRILL_NAMESPACE:-krill-dev}"

minikube start
eval "$(minikube docker-env)"
docker build -t krill:latest "$ROOT_DIR"
helm upgrade --install "$RELEASE" "$CHART_DIR" \
  --namespace "$NAMESPACE" --create-namespace \
  -f "$CHART_DIR/values.yaml" \
  -f "$CHART_DIR/values-minikube.yaml"
