# Linear → local AO pilot

This opt-in pilot provides a small durable coordinator and an outbound local
worker. It is separate from AO Cloud's sandbox provisioning and does not change
AO's daemon, listeners, session schema, or frontend. All execution goes through
the existing loopback daemon API.

The initial routing scope is **one Linear workspace, one team, one registered
local AO project, and one worker machine**. This is a trial implementation,
not a multi-tenant hosted product.

## What works

- Linear `AgentSessionEvent.created` persists a task and an initial command.
- The cloud coordinator acknowledges the webhook after committing SQLite and
  separately publishes a queued activity to Linear, even with the runner offline.
- The worker creates a Chat session through the daemon, then submits the issue
  context using Chat's durable `clientMessageId` support.
- `prompted` events deliver follow-up messages to that same local session. Mid-turn
  messages use AO's normal queued-message behavior.
- Linear's native `stop` signal invalidates queued/claimed commands and asks the
  worker to terminate the session. It confirms after AO acknowledges termination.
  A stop is permanent for that Linear session; start a new delegation to work again.
- Completed turn text and failures are copied back as Linear activities. Approval
  and structured-input requests direct the user to AO to answer locally.
- Session IDs, commands, output reports and retries survive process restarts.

The human remains the issue owner. The bridge does not change issue workflow
states, mark an issue Done, merge a PR, or automatically approve agent requests.
PR links included in the agent's final response appear in that response; native
Linear external-URL/PR registration is not implemented yet.

## Build and verify

From the repository root, with the repository's Go toolchain:

```sh
cd backend
GOWORK=off go test ./internal/linearbridge ./cmd/ao-linear-bridge
GOWORK=off go test -race ./internal/linearbridge ./cmd/ao-linear-bridge
GOWORK=off go vet ./internal/linearbridge ./cmd/ao-linear-bridge
GOWORK=off go build -o /tmp/ao-linear-bridge ./cmd/ao-linear-bridge
```

`GOWORK=off` keeps this standalone binary independent of the cloud module.
The tests use local HTTP servers and temporary databases; they do not contact
Linear, start agents, or use the user's AO database.

## Coordinator setup

Create/install a Linear OAuth app with `actor=app`, the read/write scopes needed
for the agent activities, and `app:assignable` / `app:mentionable`. Enable **Agent
session events**. Obtain its installed app access token using Linear's OAuth
flow. This pilot accepts an already-issued access token; it does not yet host an
OAuth callback or refresh tokens. Rotate the token through your service's secret
environment and restart the coordinator when needed. Do not use a personal API
key as a substitute for an agent installation.

Supply these values through your host's secret/environment configuration:

| Variable | Location | Meaning |
| --- | --- | --- |
| `LINEAR_ACCESS_TOKEN` | Coordinator only | Installed app OAuth access token |
| `LINEAR_WEBHOOK_SECRET` | Coordinator only | Linear webhook signing secret |
| `LINEAR_ORGANIZATION_ID` | Coordinator only | Allowed workspace UUID |
| `LINEAR_TEAM_ID` | Coordinator only | Allowed team UUID |
| `AO_BRIDGE_WORKER_TOKEN` | Both | Same randomly generated token, at least 32 characters |
| `AO_DATA_DIR` | Each process | State directory; defaults to `~/.ao` |
| `AO_BRIDGE_URL` | Worker only | Coordinator HTTPS origin |
| `AO_BRIDGE_PROJECT_ID` | Worker only | Existing local AO project ID |
| `AO_RUN_FILE` | Worker, optional | Daemon discovery file; defaults to `$AO_DATA_DIR/running.json` |

Run the coordinator on an always-on host:

```sh
/tmp/ao-linear-bridge coordinator --listen 127.0.0.1:8787
```

Place an HTTPS reverse proxy in front of it and set Linear's webhook URL to
`https://YOUR_HOST/linear/webhook`. The proxy must forward the raw body and the
`Linear-Signature` header unchanged. `/worker/*` uses bearer authentication;
`GET /healthz` is a minimal liveness probe.

Keep `$AO_DATA_DIR/linear-bridge/coordinator.db` on a persistent local disk and
back it up using a SQLite-aware backup method. Run exactly **one** coordinator
and outbox publisher for this database. Do not put SQLite on a network filesystem
or scale the service horizontally. A process-level file lock prevents accidental
second instances sharing the same database. For a container, mount `AO_DATA_DIR`
on a persistent volume and explicitly select `--listen 0.0.0.0:8787` behind the
HTTPS ingress. This bind belongs to the standalone coordinator, never AO's daemon.

## Local worker setup

Start AO normally and register/select the local project to use. Run:

```sh
/tmp/ao-linear-bridge worker --project LOCAL_AO_PROJECT_ID \
  --coordinator https://YOUR_HOST
```

The worker uses the project's agent default; `--harness` can select an installed
Chat-capable harness. The worker reads the daemon's current port from
`running.json` for each request, so daemon restarts do not require changing its
configuration. Its requests to AO are always addressed to `127.0.0.1`.

Keep the worker journal at `$AO_DATA_DIR/linear-bridge/worker.db`. Do not delete,
copy to another machine, or share it with a second worker. The journal is pinned
to its coordinator/project and the coordinator database to its workspace/team;
a routing change fails rather than silently sending old work into the new scope.
There is no worker migration/failover protocol in this pilot.

The worker polls every two seconds. A command has a five-minute lease; transient
failures may therefore take up to five minutes to retry. Stops bypass queued
commands, but cannot reach a disconnected or sleeping machine. A request already
in progress may finish before the worker receives the stop. Linear displays
"Stop requested" until the runner confirms; an offline runner is never reported
as having stopped execution.

## Trial checklist

1. Delegate a small issue in the configured team to the installed AO agent.
2. Confirm Linear receives the queued activity and AO creates one Chat session.
3. Send a follow-up in the Linear agent session; confirm it reaches that same AO
   session and its response returns to Linear.
4. Stop the worker process, delegate another issue, then restart the worker.
   Confirm the queued request survives and executes once.
5. Use Linear's **Send stop request** action and confirm AO terminates the session.
6. Restart the coordinator and verify accepted requests/reports remain durable.

## Uncertain session creation

AO's create-session endpoint does not yet accept an idempotency key. Before
calling it, the worker durably reserves the Linear session. If it loses the
response or crashes, it does **not** repeat that create: the session may already
exist and be running. Linear receives an error asking for local reconciliation.

Stop the worker, inspect AO, and attach the existing live Chat session:

```sh
/tmp/ao-linear-bridge reconcile --project LOCAL_AO_PROJECT_ID \
  --coordinator https://YOUR_HOST \
  --task LINEAR_AGENT_SESSION_ID --session EXISTING_AO_SESSION_ID
```

Restart the worker and resend the desired instruction in Linear. Reconciliation
validates that the local session is live, in Chat mode, and belongs to the selected
project. It does not replay already-acknowledged messages. If no session exists,
create an empty Chat session in AO and reconcile that one. If an uncertain task
was stopped, inspect/terminate any orphan in AO manually before delegating again.

## Guarantees and remaining work

- Webhook HMAC-SHA256 verification uses the original bytes; timestamps outside a
  one-minute window are rejected. Workspace/team routing is explicit.
- Creates and prompts deduplicate by signed session/activity identity, including
  webhook retries with new delivery IDs. Command acknowledgements are fenced by
  lease token and expiry. AO Chat deduplicates injected messages.
- A stop invalidates earlier leases. The worker checks the lease again after
  creation before injecting the initial prompt. There is still a network race
  between this check and delivery; immediate distributed cancellation is not claimed.
- Reports are durable locally and deduplicated when received by the coordinator.
  Linear publication is **at least once**: losing the API response or crashing
  after publication may duplicate a visible activity. It never duplicates execution.
- Reports currently read AO's bounded conversation snapshot. This is not full
  historical transcript synchronization; turns outside that page may not be
  reported after a long worker outage. Full paginated catch-up is follow-up work.
- Outbox delivery retries once per second. Provider outages/token expiry can
  delay the first activity beyond Linear's ten-second responsiveness target and
  block later activities. There is no background OAuth refresh or alerting yet.
- Only agent-session events are handled. Removing delegation, changing issue
  status, and revoking app access are not yet cancellation triggers. Use the
  explicit Linear stop action during the pilot.
- Cloud storage contains issue prompts and published assistant summaries, which
  may include code quoted by the agent. It does not synchronize repositories,
  local credentials, terminal streams, or raw reasoning/tool logs. Treat the
  databases and their backups as private task data.

Before wider deployment: add OAuth installation/refresh and revocation handling,
organization/project/worker registration, session-create idempotency in AO,
worker liveness and recovery, paginated report catch-up, Linear publication
reconciliation/backoff, and native issue/PR state synchronization. The existing
AO Cloud service can host those durable control-plane responsibilities once this
local execution path has been validated.

API references checked during implementation:
[Linear agents](https://linear.app/developers/agents),
[agent interaction](https://linear.app/developers/agent-interaction),
[stop signals](https://linear.app/developers/agent-signals),
[webhook authentication](https://linear.app/developers/webhooks), and the
[official SDK schema](https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql).
The schema uses `agentActivity.content.body`; the bridge also accepts the older
`agentActivity.body` shape described in the interaction guide.
