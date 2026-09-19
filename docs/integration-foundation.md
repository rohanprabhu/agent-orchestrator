# AO integration foundation — proposed design

Status: researched design proposal, September 5, 2026. AO was inspected at `/Users/rohan/projects/custom-ao`. Names and examples below describe proposed APIs; no runtime implementation is claimed.

## Boundary

Put an organization-scoped integration service above AO. AO continues to run work against resolved repositories. Product features and agents consume versioned capability contracts through this service; only adapters know provider APIs, tokens, and provider-specific fields.

Flow: company configuration → scoped capability resolution → provider adapter → normalized result → AO job context or application consumer.

## Configuration and ownership

- Company owns named integration connections, a repository catalog, capability defaults, and policy.
- A connection identifies a provider installation/account, its external workspace identity, credential reference, and allowed resources. Support multiple connections to the same provider.
- Repository records identify a source-control connection, immutable provider repository ID, clone URL, and default branch. Machine-specific checkout paths belong to execution environments, not the company catalog.
- Projects select repositories and bind capabilities to company connections plus resource selectors (for example a Linear team and optional project).
- Projects inherit company defaults and may override bindings inside company policy. Overrides never widen resource or operation permissions.
- Resolve an explicit binding first, then a company default. Reject ambiguity instead of picking an arbitrary connection. A selected connection without the requested operation produces an explicit error; do not silently redirect writes.
- Store secret references only in configuration. Resolve credentials on the server for the selected company and connection. Never put tokens in agent context.

Illustrative configuration, not an AO configuration format:

```yaml
schemaVersion: 1
company:
  id: acme
  connections:
    engineering:
      provider: linear
      workspaceId: linear-workspace-id
      credentialRef: secrets/acme/linear
      resources:
        teamIds: [linear-team-id]
      allowedOperations: [issues.get, issues.search, issues.create, issues.transition]
    code:
      provider: github
      credentialRef: secrets/acme/github
  repositories:
    backend:
      connection: code
      externalId: repository-id
      cloneUrl: https://github.com/acme/backend.git
      defaultBranch: main
  defaults:
    issueTracking:
      connection: engineering
      scope:
        teamId: linear-team-id
projects:
  backend:
    repositories: [backend]
    bindings:
      issueTracking:
        connection: engineering
        scope:
          teamId: linear-team-id
          projectId: linear-project-id
```

## Capability model

Use capability families plus independently supported operations and semantic traits, not a single supported boolean or coarse levels such as basic/full.

Initial contract families:

| Family | Example operations | Traits to describe explicitly |
| --- | --- | --- |
| issueTracking/v1 | get, search, create, update, listTransitions, transition, comments.list, comments.create | supported filters, writable fields, hierarchy, pagination, events |
| sourceControl/v1 | repositories.get, repositories.list, pullRequests.get | clone protocol, provider identity, branch/ref support |
| documentManagement/v1 | get, search, create, update | content formats, revisions, rich text, comments |

These are a contract vocabulary, not promises that the first adapters implement every operation. An integration can implement multiple families and any declared subset of operations.

Discovery returns implemented operations, currently allowed operations, resource restrictions, version, and reasons for unavailable operations. Effective availability intersects adapter implementation, credential grants, company policy, caller policy, and resource binding. Where provider permissions cannot be precomputed, indicate that availability is conditional and enforce it on execution. Provider authorization remains authoritative.

Each operation has a validated input/output schema, side-effect classification, and permission requirements. Generate agent tool exposure from these descriptors. Do not advertise stubs. Use typed family interfaces and runtime validation; avoid an unrestricted execute(string, any) as the public consumer interface.

Conceptual consumer example:

```ts
const issues = await integrations.resolve({
  companyId,
  projectId,
  capability: 'issueTracking/v1',
  require: ['get', 'transition'],
  principal,
});
const issue = await issues.get({ ref });
const transitions = await issues.listTransitions({ ref: issue.ref });
await issues.transition({ ref: issue.ref, transitionId: selectedTransition.id });
```

The final code should align the operation naming and types with AO's actual language and conventions.

## Normalization and runtime guarantees

- Resource references include company, connection, resource kind, and immutable external ID. Human issue identifiers and URLs are display/search attributes. Validate every incoming reference against the resolved context.
- Normalize a small useful issue core: ref, identifier, title, body, URL, assignee references, timestamps, and state. Keep native state ID/name plus a broad category; query permitted transitions rather than assuming a universal workflow.
- Preserve provider-specific detail through namespaced, schema-validated extensions. Consumers opt into extensions explicitly.
- Search and list are paginated. Treat cursors as opaque and bound to connection, query, and scope.
- Normalize authentication, permission, unsupported-operation, not-found, validation, conflict, rate-limit, and transient errors without leaking secrets.
- Enforce deadlines and bounded retries for safe reads. Do not retry uncertain writes blindly. Describe idempotency support per operation and reconcile ambiguous outcomes.
- Audit writes with actor, company, connection, resource, operation, and correlation ID. Redact credentials and avoid logging full document/issue content by default.
- Resolve bindings before AO execution and attach repository and issue references to the job context. Recheck authorization at invocation; cached discovery is not authorization.

## First Linear slice

1. Implement the registry, configuration validation, scoped resolver, and one real read path before adding broad interfaces.
2. Add Linear connection validation and workspace/team selection. Start with a server-side credential reference; support API keys for development and OAuth for shared installations.
3. Implement issue get and scoped paginated search, normalizing references and workflow states.
4. Demonstrate one consumer reading an issue through issueTracking/v1 and resolving its project repository into AO's existing execution path.
5. Add create/update/transitions and comments as separate operations with runtime permission checks.
6. Add webhook ingestion later using the same adapter boundary: authenticate events, resolve the connection/company, deduplicate delivery, and emit normalized capability events. Do not make webhook setup a prerequisite for reading issues.
7. Add a second adapter or a deliberately smaller fixture adapter to validate partial capability behavior. Keep document management as a contract until a real consumer needs it.

Linear's API is GraphQL and supports API keys and OAuth. Its OAuth documentation recommends OAuth for integrations and currently documents refresh-token rotation. Token refresh must be serialized per connection and replacement credentials stored atomically. Webhook provisioning has separate permission requirements; normal issue access should not require admin access merely to prepare for future events.

Sources checked September 5, 2026:
- https://linear.app/developers/graphql
- https://linear.app/developers/oauth-2-0-authentication
- https://linear.app/developers/webhooks

## Acceptance checks

- A project inherits the intended company connection and an explicit override resolves deterministically.
- Multiple companies and multiple Linear workspaces remain isolated, including guessed resource references and reused cursors.
- A read-only provider/grant exposes read operations and cannot execute writes.
- A provider supporting issues and documents exposes those families independently.
- Ambiguous, missing, disabled, unsupported, and out-of-scope bindings fail explicitly.
- Provider state IDs survive normalization and transition selection round trips.
- Pagination, partial GraphQL errors, expired credentials, rate limits, and uncertain write outcomes are handled.
- Existing AO configuration remains valid through an explicit backward-compatible migration after inspecting its current shape.

## Repository-dependent decisions

Locate existing configuration loading, plugin/provider interfaces, persistence and secret storage, job context construction, API/tool registration, and tests. Reuse those mechanisms where appropriate. Do not introduce a second competing registry or configuration system before inspecting AO.

## Findings from the actual checkout

The active repository is `/Users/rohan/projects/custom-ao`; the task's original `/Users/rohan/Documents/ChatGPT/custom-ao` path is stale.

- `backend/internal/ports/tracker.go` already defines `Tracker.Get`, repository-scoped `List`, and `Preflight`. Keep it as a legacy compatibility port while introducing capability interfaces with explicit scope and pagination. Forcing a Linear team into `TrackerRepo` would hide incompatible semantics.
- `backend/internal/domain/tracker.go` defines GitHub/GitLab-only provider validation and `TrackerID{Provider, Native, Host}`. Provider plus host does not distinguish two installations on the same host. New resource references need a connection ID; preserve old persisted IDs through a compatibility path.
- `backend/internal/domain/project.go` already models single-repo, workspace, and scratch projects, with child repositories in `WorkspaceRepoRecord`. Reuse the execution model. Add a separate company catalog whose entries existing local project and workspace-repo records can reference. A company is not another workspace project.
- `backend/internal/domain/projectconfig.go` carries per-project `TrackerIntake`. Add optional company ownership and integration bindings without changing the meaning of existing configuration.
- `backend/internal/daemon/tracker_wiring.go` constructs a daemon-wide multi-tracker with environment/CLI credential fallbacks. New company connections must resolve their own credentials; leave global credentials only on the legacy path. An explicit company connection must never silently borrow a logged-in user's CLI account.
- `backend/internal/service/session/issue_context.go` recognizes GitHub/GitLab URLs and numeric issue IDs and derives the tracker from the code origin. Bound projects should resolve Linear identifiers through their issue capability before this legacy parser. Existing unbound projects retain the current path. Explicitly requested issue-driven execution should surface resolution errors, not launch an apparently hydrated task after a failed lookup.
- `backend/internal/observe/trackerintake/observer.go` already owns polling and requires an assignee rule in project config. Migrate its resolver to company/project bindings and scoped issue search; preserve eligibility and duplicate-session protection. Include connection identity in deduplication keys so matching issue identifiers across workspaces cannot collide.
- `docs/STATUS.md` describes a single-user local daemon. Company configuration is an ownership/routing boundary inside that model; it does not make the unauthenticated loopback API a secure multi-user service. Preserve current listener rules and existing approval/policy boundaries.

## Concrete Go package placement and consumer contract

Stay within the existing Go architecture:

| Location | Proposed responsibility |
| --- | --- |
| `backend/internal/domain/integration.go` | Company, connection, binding, resource reference, capability descriptors |
| `backend/internal/ports/integration.go` | Small operation interfaces, resolver and credential ports |
| `backend/internal/service/integration/` | Configuration validation, binding resolution, discovery, guarded invocation |
| `backend/internal/adapters/tracker/linear/` | Linear transport and normalized issue operations |
| `backend/internal/storage/sqlite/` | New company/connection/binding/catalog tables, queries, CDC triggers |
| `backend/internal/daemon/` | Construct and inject the integration service into consumers |
| `backend/internal/httpd/controllers/` | Configuration/discovery and typed operation endpoints |

Use narrow Go interfaces for optional behavior, for example (illustrative, domain DTO definitions omitted):

```go
type IssueGetter interface {
    GetIssue(context.Context, ResourceRef) (IssueSnapshot, error)
}

type IssueSearcher interface {
    SearchIssues(context.Context, IssueQuery) (IssuePage, error)
}

type IssueTransitioner interface {
    ListTransitions(context.Context, ResourceRef) ([]IssueTransition, error)
    TransitionIssue(context.Context, TransitionRequest) (IssueSnapshot, error)
}
```

Resolve a connection into guarded, scope-bound operation handles. Consumers request the interfaces they need; registration validates that advertised descriptors have implementations. Optional interfaces must not make a read-only adapter implement dummy writes. The service supplies discovery metadata for UI/agent tools and enforces the same constraints when invoking the handle. A typed operation handler, its schemas, and its descriptor should be registered together to prevent drift.

For the first slice, expose company/connection/binding management and effective capability discovery over the existing daemon HTTP API. The desktop and CLI remain thin clients. Regenerate OpenAPI and frontend schema from controller DTOs and the operation registry using `npm run api`; do not hand-maintain a parallel JSON tool schema. Agent-specific exposure can project this contract once an agent consumer is selected.

## Persistence and delivery order

1. Add company and connection records, explicit capability defaults, project bindings, and repository catalog records via new SQLite migrations and sqlc queries. Add nullable company/catalog references to existing project/repository records. Preserve unowned projects as legacy local projects. Company-scoped composite references must prevent cross-company bindings. Connection removal is disabled/detached explicitly while references exist; never cascade-delete AO projects or sessions.
2. Build and test the resolver with two fixture connections and unequal operation sets. Validate inheritance, overrides, disabled bindings (distinct from inherited absence), resource narrowing, unknown versions, cross-company references, and operation availability.
3. Implement Linear read operations against an injected HTTP client and credential source. Use `httptest` for success, GraphQL errors including HTTP-200 errors/partial data, pagination, scope mismatch, rate limits, and token failures. A partial issue response must not silently become a valid full snapshot.
4. Wire explicit issue lookup through the session service, then migrate intake. Confirm a GitHub code repository can consume Linear issues without changing SCM/PR routing. Confirm legacy GitHub/GitLab config still works.
5. Add management/discovery UI: company connection list with account/workspace identity and health; per-connection supported/available operations with reasons; project bindings showing inherited versus overridden values; repository mappings. A capability is enabled by effective policy, not merely by an installed provider logo.
6. Add writes and events when their consumers are introduced. Contract tests must cover unavailable operations before exposing those tools.

No runtime code or schema was modified during this design pass. Git inspection was blocked by the machine's unaccepted Xcode license; no license was accepted and no commit or branch operation was attempted successfully.
