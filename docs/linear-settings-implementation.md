# Linear settings implementation

The desktop now includes **Settings → Integrations → Linear**, independent of
cloud execution. The UI uses the Linear mark and the actual installed app-user
name/display name. Editable Lenticular profile names do not rename the shared Linear app.

## Desktop flow

1. Sign in to Lenticular and select a Lenticular organization. An owner/admin manages integrations.
2. Connect Linear. The system browser authorizes an app installation using OAuth
   state and PKCE. Return to settings; connections refresh every five seconds.
3. Select the authorized workspace, granted team, optional Linear project, and
   existing local Lenticular project. Choose a profile name and enable it.
4. Edit the profile name or team/project filter, pause/resume new intake, or
   disconnect the runner. Existing task bindings do not move on a rename or route
   edit. Changing the local Lenticular project requires a new profile; it is deliberately
   immutable for a runner with an existing journal.

Names use NFKC normalization and Unicode case folding. Saves reject duplicate
names and equal-priority routing scopes transactionally. Exact project scopes win
against team fallbacks, including when the exact profile is paused. A save retry
with the same runner registration reuses the profile instead of duplicating it.

The daemon creates a random worker credential, stores it mode 0600 beneath
`AO_DATA_DIR/integrations/linear`, and sends only its SHA-256 challenge to the
renderer. Cloud profile registration binds the challenge to an organization,
owner, profile, and local project. The daemon verifies that binding before
starting its outbound worker. OAuth credentials never reach the desktop;
worker credentials never reach the renderer. A runner credential is pinned to
its original control-plane origin. Local runner control routes are blocked on
the optional LAN listener.

## Local experience development (Lenticular)

Deployment is deferred until the local experience is settled. Start Podman and
its machine, then run `bash cloud/scripts/linear-local.sh`. This starts a separate
PostgreSQL container on loopback port 54339, applies migrations using the owner
role, and runs the integration service on loopback port 8080 using the restricted
runtime role. State and the encryption key persist under
`${AO_DATA_DIR:-~/.ao}/lenticular-linear`. It does not start remote agent workers.

Launch the desktop from `frontend/`:

```bash
AO_CLOUD_OFFERING=off AO_CLOUD_CONTROL_PLANE_URL=http://127.0.0.1:8080 npm run dev
```

Settings → Integrations → Linear offers the existing local development sign-in
and registration dialog, even with cloud execution disabled. Register a local
account and workspace there. OAuth configuration is still required to connect a
real Linear workspace; an unconfigured service honestly shows unavailable.

The private Linear application draft is named **Lenticular**; it is not registered yet. Its local callback is
`http://localhost:8080/api/cloud/v1/linear/callback`. Receiving real Linear
webhooks additionally needs an HTTPS relay/tunnel to the signed webhook endpoint;
localhost cannot receive calls from Linear. Do not expose the desktop daemon or
the local-auth endpoints. A relay should forward only the exact webhook path.
No tunnel or cloud deployment is started by this script.

Stop the service with Ctrl-C and PostgreSQL with
`podman stop lenticular-linear-postgres`. Both retain local state. The original
upstream deployment scripts and domains are not used by this local workflow.

## Hosted deployment configuration

Deploy the updated `ao-cloud` binary and PostgreSQL migration
`00040_linear_integrations.sql`. The service must already have Lenticular authentication,
organization membership, and `AO_CLOUD_PROVIDER_SECRET_KEY` configured (the
existing provider encryption key). Configure a Lenticular-owned Linear OAuth app:

- App actor installation, with `read`, `write`, `app:assignable`, and
  `app:mentionable` scopes.
- Callback: `https://<control-plane>/api/cloud/v1/linear/callback`.
- Webhook: `https://<control-plane>/api/cloud/v1/linear/webhook`, subscribed to
  agent-session events. OAuth revocation events are also handled.
- Set `AO_CLOUD_LINEAR_CLIENT_ID`, `AO_CLOUD_LINEAR_CLIENT_SECRET`,
  `AO_CLOUD_LINEAR_REDIRECT_URL`, and `AO_CLOUD_LINEAR_WEBHOOK_SECRET` on the
  service. These are operator configuration, not user-entered desktop settings.
- Configure the desktop daemon's existing `AO_CLOUD_CONTROL_PLANE_URL`. This does
  not require enabling the cloud execution offering.

An unconfigured deployment reports `available: false`; it cannot appear connected.
This change does not register an OAuth application or deploy a cloud service.
The OAuth app's name/icon govern the native agent identity; Linear may disambiguate
its display name. The app reads that identity back instead of inventing a handle.

## Execution and persistence

Signed webhooks are durably accepted before provider resource lookup. The service
resolves the issue's actual team/project using Linear, binds a profile once, and
queues commands for that runner. Workers use the existing daemon API to create
chat sessions and send idempotent conversation messages. Follow-ups stay in the
same chat. Native stop signals invalidate previous leases and queue a kill.
Uninstall events pause all installation profiles, erase the provider credentials,
and queue durable stops; workers remain authorized to receive and acknowledge
those stops. A stop is not reported as completed before local acknowledgement.

OAuth attempts expire after ten minutes and are single-use. Callback completion
rechecks the initiating principal's current organization permission. Access and
refresh tokens use the existing AEAD cipher with organization/connection AAD.
Token refresh and routing changes serialize under a PostgreSQL tenant row lock.
Tenant state and global opaque routing indexes commit together; row-level security
isolates tenant state. The route index prevents a workspace or worker challenge
being claimed by a different Lenticular organization.

This first implementation uses one JSON state/mailbox row per organization for
atomic low-volume routing and lease updates. It is not designed for high-volume
mailboxes: large histories increase row rewrite cost. Split commands/outbox into
indexed normalized tables before high-volume rollout. Provider calls are bounded
but can hold the organization lock up to the HTTP timeout, so webhook latency
under provider slowness remains a rollout concern. Delivery retries use stable
activity UUIDs; exactly-once provider delivery is not claimed.

The existing bridge's uncertain-session safeguard is retained: a lost session
creation reply is never blindly retried. Inspect Lenticular and use the pilot's reconcile
command with the daemon-managed runner journal if reconciliation is needed. A
settings-based reconciliation UI, arbitrary native agent renaming, model/harness
selection per profile, and moving active work between machines are not included.
The worker uses Lenticular's default chat harness and the normal local permission flow.

## Pilot transition

The env-configured `ao-linear-bridge` pilot remains available. Its single-scope
journal is not silently imported or reused as a desktop runner. Drain/stop the
pilot before switching the Linear app's webhook to the hosted service. Preserve
its journal and finish/reconcile existing pilot tasks there; use newly delegated
sessions after connecting the desktop profile. This avoids duplicate ownership
of existing Linear agent sessions.

## Verification

- Full cloud Go suite and affected daemon HTTP/API, daemon wiring, worker and
  runner-manager suites pass. OAuth/callback replay, encrypted persistence,
  membership recheck, scope/name conflicts, retries, lease fencing, revocation,
  route auth/isolation, and secret redaction are covered by local fixtures.
- The pinned golangci-lint v2.12.2 check passes for all changed backend packages.
- Race tests cover the new service, cloud handlers, runner manager, and pilot
  bridge. Frontend typecheck and production renderer build pass.
- 91 settings, API-client, and localization tests pass. The separate renderer
  localization coverage gate still flags pre-existing Sidebar literals
  (`Lenticular`, `AO fork`); those user edits are unchanged.
- The real settings UI and logo were inspected in a browser against an isolated
  daemon with cloud execution disabled. `ao preview` was attempted, but this
  Codex task has no `AO_SESSION_ID`, so the Lenticular session-owned preview command is
  unavailable here.
- No live Linear installation was authorized. PostgreSQL migration/RLS behavior
  still needs a live-database test: Podman is not available yet. No deployment, packaging/publishing, or remote CI run is claimed.
