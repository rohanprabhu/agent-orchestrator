# Linear setup in AO Settings

Status: product design, September 18, 2026. The first settings/hosted-service
implementation now exists; see [implementation and deployment notes](linear-settings-implementation.md)
for shipped boundaries, verification, and remaining rollout work. The design below
includes follow-up capabilities beyond that first implementation.

## Entry point and flow

Add **Settings → Integrations → Linear**, independent of the optional Cloud
execution offering. A hosted integration service is needed even when execution
is local; connecting Linear must not enable cloud execution.

1. **Connect Linear** opens Linear authorization in the system browser. AO owns
   the OAuth application, callback, signing secret, and token refresh. Users do
   not create apps, paste tokens, run the coordinator, or configure environment
   variables in the ordinary product path.
2. **Confirm workspace** shows the workspace actually authorized by Linear. A
   workspace is chosen/authorized through Linear; AO cannot switch the token to
   an unrelated workspace with a dropdown. Connecting another workspace starts
   a separate authorization. Show existing connections to avoid duplicate setup.
3. **Choose scope** lists only granted Linear teams. Use Linear's terminology:
   workspace → teams → optional Linear project. Do not use an ambiguous
   "namespace" label. Restricting intake in AO does not widen or replace the team
   permissions granted during installation.
4. **Choose execution** maps a Linear scope to an existing AO project/repository
   and an authorized local runner. Initially default to this machine, with its
   real connectivity status. Harness/model preferences belong to this profile.
5. **Confirm identity and enable** shows the installed agent's actual name,
   avatar, and username/mention handle returned by Linear. Enable intake only
   after authorization, scope validation, routing, and worker registration succeed.
   The agent may already be visible in Linear after OAuth installation while AO
   routing is still incomplete; show "Setup incomplete" rather than "Ready".

The connected screen shows workspace, granted/selected teams, Linear project
filter if present, installed identity, AO profile name, AO project, runner
availability, and intake state. Actions: edit routing, rename the AO profile,
pause/resume intake, reconnect, and disconnect this runner. Workspace-wide app
removal is a separate administrator action with its scope made explicit.

## Names and identity

There are three distinct identities:

- **Linear installed agent:** the app user that appears in Linear's delegation
  and mention menus. Persist workspace ID, OAuth client ID, and app-user ID.
  Linear's documented name/icon source is the OAuth application. Do not show a
  per-installation rename control until provider support for that operation has
  been verified. Renaming the shared OAuth app would affect other installations.
- **AO agent profile:** an editable user-facing name for execution preferences
  and routing, e.g. "Backend helper". Persist a stable profile UUID. Rename changes
  its label, not Linear identity, ownership, credentials, routing keys, or history.
  Label the field "Profile name in AO" when the name is not reflected in Linear.
- **Runner:** a registered machine capable of executing that profile. Its display
  name is not a second Linear agent. Moving work between runners must not create
  another installed app user.

Recommended default: one installed **AO** agent per Linear workspace, with named
AO profiles selected by unambiguous team/project routing. A request for multiple
separately assignable native names (e.g. "AO Backend" and "AO Reviewer") is a
materially different provider-identity requirement. The public documentation does
not establish arbitrary per-installation names or multiple app users from one
OAuth app in a workspace. Validate those capabilities with Linear before
promising them; otherwise separately named OAuth apps are an advanced, explicitly
managed installation model, not a hidden automatic signup step.

## Clashes and reconnects

- **Linear username already used:** Linear documents appending a numeric suffix.
  Read back and show the actual installed identity; never manufacture a mention
  handle from a desired display name. Stable provider IDs route events.
- **AO profile name already used:** enforce uniqueness within the owning AO
  company/connection. Compare trimmed, Unicode-normalized, case-folded names;
  preserve display spelling. Return a field error and an available suggestion.
  Recheck on save in a transaction so simultaneous creates/renames cannot collide.
  Do not silently rename or replace an existing profile.
- **AO already installed:** discover/reuse the existing installation only after
  verifying the current AO principal's permission to bind it. Show "Already
  connected" with the current scope and runner. Do not silently create a second
  connection, take over another person's runner, or rotate shared credentials.
- **Two profiles match an issue:** exact Linear-project routing may override an
  explicitly configured team fallback. Reject equal-priority overlaps at save;
  unresolved ambiguity at dispatch asks the user to choose and launches nothing.
- **Runner already owns an active task:** preserve the existing binding and
  conversation. A profile rename or routing edit applies to future task intake;
  moving active work requires explicit, fenced handoff support.

## Ownership and runtime boundary

The hosted service owns installations, encrypted OAuth access/refresh tokens,
one-time OAuth state/PKCE, tenant-scoped authorization, permission/revocation
handling, resource discovery, profiles and routing, worker registration, durable
commands, and Linear activity delivery. Bind OAuth state to the initiating AO
principal, connection request, redirect target, and expiry; consume it once.
Concurrent reconnect and token-refresh attempts require generation checks.

The local daemon owns the registered project, runner credentials, worker
lifecycle, local execution and permission decisions. The renderer is a thin
settings surface consuming daemon HTTP DTOs; no provider secrets in the renderer,
localStorage, query parameters, or model prompts. Preserve existing loopback and
LAN-listener rules. Desktop reconnect/status polling should work without opening
a new listener. Settings must expose failed/cancelled/expired authorization and
permission loss as real states, not successful connections.

Cloud availability and local runner availability are distinct: a connected
Linear installation can have an offline runner. Show "Waiting for this Mac" and
queue work. Pausing intake stops new dispatch; existing tasks continue unless the
user explicitly chooses to stop them. Disconnecting a runner revokes its own
credential and preserves task history. Removing a workspace installation stops
new dispatch for all its profiles; already-running local work receives a durable
stop request, and is not reported stopped until acknowledged.

The current bridge's env-configured, single-workspace credentials and pinned
SQLite scope are insufficient for this product flow. Extend the authenticated AO
Cloud integration service and the daemon settings boundary rather than putting
OAuth client secrets into a settings form or starting one cloud coordinator per
profile. Existing `docs/integration-foundation.md` provides the company/connection
and project binding model to use.

## Implementation boundaries

- Add an Integrations entry to `GlobalSettingsSection`, `SettingsDialog`, and
  `GlobalSettingsForm`. Follow the existing row/panel components and translations.
- Add daemon controllers/DTOs and service wiring for status, connect attempts,
  authorized resource discovery, profile/routing CRUD, pause, and disconnect.
  Regenerate OpenAPI and the frontend client from the controller contract.
- Use the existing hosted AO identity and organization membership checks for
  installation ownership. OAuth completion must not be an unauthenticated claim
  on a workspace just because its identifier is known.
- Replace manual bridge startup with daemon-managed outbound worker lifecycle;
  leave agent/session execution behind the existing service/API boundaries.
- Persist installation identity separately from editable names, with database
  constraints for routing/name uniqueness and explicit migration of pilot state.

Acceptance checks include cancelled/expired OAuth, reconnect/reinstall,
restricted-team discovery, cross-company denial, concurrent name/routing
conflicts, accurate installed handles, server-side secret handling, offline
runner recovery, and unchanged active-task bindings after a rename. Verify the
settings interface through `ao preview` and normal frontend/backend gates when
implemented.

Sources checked September 18, 2026:
[Linear agent setup](https://linear.app/developers/agents),
[installation/team access and name collisions](https://linear.app/docs/agents-in-linear),
[OAuth](https://linear.app/developers/oauth-2-0-authentication), and
[actor authorization](https://linear.app/developers/oauth-actor-authorization).
