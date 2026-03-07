# Krill M6 Operational Runbook

## Control Plane

Enable the HTTP control plane in `krill.yaml`:

```yaml
protocols:
  - name: http
    enabled: true
    config:
      addr: ":8080"
      control_plane:
        enabled: true
        tokens:
          viewer: $KRILL_CONTROL_VIEWER_TOKEN
          operator: $KRILL_CONTROL_OPERATOR_TOKEN
          admin: $KRILL_CONTROL_ADMIN_TOKEN
```

Role model:

- `viewer`: diagnostics, plans, workflow traces, schedule/session visibility, audit visibility
- `operator`: viewer rights plus planner/capability/schedule mutations and approval updates
- `admin`: operator rights plus secret rotation and rollout rollback

## Core endpoints

- `GET /v1/control/health`
- `GET /v1/control/readiness`
- `GET /v1/control/diagnostics`
- `GET|PUT /v1/control/planner/config`
- `GET|POST /v1/control/capabilities`
- `GET|POST /v1/control/schedules`
- `POST /v1/control/schedules/{id}/pause|resume|cancel|trigger`
- `GET /v1/control/schedules/{id}`
- `GET /v1/control/schedules/{id}/history`
- `GET /v1/control/plans/{request_id}`
- `GET /v1/control/workflows/{request_id}`
- `GET /v1/control/artifacts/{request_id}`
- `GET|POST /v1/control/external-actions/{request_id}`
- `GET /v1/control/sessions`
- `GET /v1/control/sessions/{session_id}`
- `GET|POST /v1/control/secrets`
- `GET /v1/control/audit`
- `GET /v1/control/rollouts`
- `POST /v1/control/rollouts/{rollout_id}/rollback`

## Incident drill

1. Check readiness and diagnostics.
2. Inspect the affected `request_id` plan and workflow handoff chain.
3. Inspect schedule history if the incident is scheduler-driven.
4. Inspect external action state and approval/audit trail.
5. If needed, perform a `dry_run` on planner/capability/schedule change.
6. Apply the rollout, validate diagnostics, and rollback with the rollout id if needed.

## Rotation semantics

- Secret rotation uses control-plane API and updates runtime process env without restart.
- Capability and planner changes are applied in-process and affect subsequent planner decisions.
- Schedule mutations are applied in-process and preserve scheduler runtime parity with existing local/cluster packaging.
