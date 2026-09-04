# Chat Routing Service — How It's Exposed

## Current state

`chat-routing-service` is a standalone Go HTTP server (`net/http`, listens on `:9096` by default). It is **not** SDK-only — the HTTP API is the real, primary interface. The Go SDK (`apps/chat-routing-service/sdk-go`) still exists alongside it, but only as an optional convenience wrapper for `csm-portal/backend`, its one real caller today. Nothing about the server requires the SDK to be reachable.

## What's exposed

Every route below requires the header `X-Routing-Service-Token: <shared secret>`, except `/health`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/route/escalate` | Route a new case to an engineer, or queue it |
| POST | `/route/presence` | Set AVAILABLE/OFFLINE |
| POST | `/route/completed` | End a session |
| POST | `/route/decline` | Release/reassign a case |
| POST | `/route/accept` | Confirm accepting an assigned case |
| GET | `/route/presence/{email}` | Look up an engineer's status |
| POST | `/route/workitem`, `/route/comment` | Temporary stand-in persistence (see below) |
| GET | `/route/debug/state`, `/route/debug/workitem/{caseId}` | Verification only |
| GET | `/health` | Liveness, unauthenticated |

## Where the contract is documented

- `apps/chat-routing-service/backend/openapi.yaml` — full OpenAPI 3.0.1 spec (every request/response shape), lint-clean.
- `apps/chat-routing-service/backend/README.md` — has a new **"Integrating from another service"** section stating this plainly, plus the full state machine and data model.

## SDK status

Kept as-is, not deprecated, not required. It's a thin client — no routing logic of its own, just request marshaling + the token header. A non-Go consumer, or a Go service that doesn't want the dependency, builds straight against `openapi.yaml` instead.

## For a new consumer

1. Get the base URL and a copy of `ROUTING_SERVICE_TOKEN` from whoever owns this service.
2. Build against `openapi.yaml`.
3. Never put that token in browser-facing code — every route here assumes a trusted backend caller, not an end user.
4. Treat `/route/workitem`, `/route/comment`, and `/route/debug/*` as unstable — confirm with the service owner before depending on them (they're a temporary stand-in for a schema that isn't merged yet).

*For the full project architecture, data model, design decisions, and defense-sheet Q&A, see the earlier, longer explainer doc already delivered in this conversation.*
