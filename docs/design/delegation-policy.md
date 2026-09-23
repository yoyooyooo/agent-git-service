# Legacy Delegation Policy Inventory

The `delegation` YAML section is historical migration input only. It cannot issue a Session,
select an Access Grant executor, grant a repository operation, or authorize a provider effect.

## Runtime status

- `GET /api/v3/integrations/delegation-policies` is retired and returns `404`.
- `POST /api/v3/agent-sessions/exchange` is retired and returns `404`.
- No production service path invokes the legacy delegation resolver.
- Access Grant issue/use-time logic reads canonical Principal Session authority and live native
  repository grants, not role/Task mappings.

The parser and migration planner remain only so operators can inspect or migrate old configuration
without silently reinterpreting it as current authority. Existing rows/config cannot widen the
Access Grant legal envelope.

## Target replacement

Current authority uses:

1. provider-neutral execution-context snapshots;
2. canonical `(source_instance_id, external_agent_id)` actors;
3. source/workspace default and optional actor-bound elevated Principal Session bindings;
4. normalized resource-operation policy;
5. separately managed live native repository grants;
6. Access Grants and exact use-time revalidation.

Agent names, roles, labels, Task IDs, prompts, environment hints, and caller-provided principal
selectors are provenance or requests only.

## Cleanup boundary

Do not add new fields, routes, or behavior to the legacy delegation policy. New authority changes
belong to Principal Session desired state, Access Grant evaluation, native repository grants, and
their source-controlled transition evidence.

Source anchors:

- `internal/delegationpolicy/`
- `internal/sessionauthority/migration.go`
- `internal/service/access_grant.go`
- `internal/router/router.go`
