#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
COMPOSE_BASE="$ROOT_DIR/deploy/compose/docker-compose.yml"
COMPOSE_SANDBOX="$ROOT_DIR/deploy/compose/docker-compose.sandbox.yml"
COMPOSE_PUBSUB="$ROOT_DIR/deploy/compose/docker-compose.pubsub.yml"
DOCKER_BIN="${DOCKER_BIN:-docker}"

compose_args=(-f "$COMPOSE_BASE")
if [[ "${KRILL_ENABLE_DOCKER_SANDBOX:-0}" == "1" ]]; then
  compose_args+=(-f "$COMPOSE_SANDBOX" --profile docker-sandbox)
fi
case "${KRILL_PUBSUB_PROFILE:-}" in
  nats)
    compose_args+=(-f "$COMPOSE_PUBSUB" --profile pubsub-nats)
    ;;
  redis)
    compose_args+=(-f "$COMPOSE_PUBSUB" --profile pubsub-redis)
    ;;
  both)
    compose_args+=(-f "$COMPOSE_PUBSUB" --profile pubsub-nats --profile pubsub-redis)
    ;;
esac

"$DOCKER_BIN" compose "${compose_args[@]}" down -v --remove-orphans
"$DOCKER_BIN" compose "${compose_args[@]}" up -d --build
"$DOCKER_BIN" compose "${compose_args[@]}" ps

echo "krill stack reset completed"
