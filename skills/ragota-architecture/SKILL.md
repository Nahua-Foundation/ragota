---
name: ragota-architecture
description: Answer architecture and cross-repository questions with the ragota graph tools — which services exist, who talks to whom, who consumes an event, where a value ends up, what breaks if an endpoint changes. Use when a question spans more than one file or one repository; file search cannot see across that boundary.
---

# Architecture questions: use the graph, not your eyes

ragota joins repositories by their contracts — `http:POST /charges`,
`grpc:Cart/AddItem`, `topic:order-events`, `db:orders` — so "who calls this
endpoint" has an answer even when the caller lives in another repository,
another language, another team. Reading files cannot get there: the caller of
an HTTP route shares no text with its handler beyond the path string.

## Which tool answers which question

| Question | Tool |
|---|---|
| What services exist, who talks to whom | `ragota_services` |
| Who publishes / consumes this event | `ragota_topics` |
| Who uses the thing at this file+line | `ragota_references` |
| What does this one unit call, what points at it | `ragota_neighbors` (needs a `unit_id` from an earlier answer) |
| Does A reach B, and through what | `ragota_path` (two `unit_id`s) |
| Where does this parameter's value end up, across services | `ragota_trace` |
| The flow around a search answer, multi-file | `ragota_context` |

**Start wide, then narrow.** For "how does checkout work here":
`ragota_services` first — learn the service names and their links — then one
`ragota_search` inside the repository that owns the service, then ranges.
Do not start by listing files.

## Worked examples

Impact analysis — "can we delete the legacy /v1/orders endpoint?":

```
ragota_services {}
  → links include: storefront → orders-api  http:GET /v1/orders  x4
ragota_search {"query": "where is GET /v1/orders called in the storefront", "repos": ["storefront"]}
  → web/src/api/orders.ts:31 fetchOrders ...
Read that range. The four call sites aggregate to one wrapper — the delete
needs one client change, and you can name it.
```

Value flow — "where does the order id actually end up?":

```
ragota_trace {"symbol": "PlaceOrder", "param": "order_id"}
  → PlaceOrder → http POST /payments/charge (charge.order_ref)
    → ChargeHandler → kafka topic payments.settled → db:settlements
```

`param` matching ignores case and underscores and aligns on word boundaries
(`user_id` follows `userId` and `req.GetUserId()`, not `user_agent`). A
misspelled symbol or param comes back **empty, not as an error** — check the
spelling against a `ragota_symbol` answer before concluding the value goes
nowhere.

## Rules that keep the graph honest

- **`unit_id` is currency you receive, never mint.** Take it from
  `ragota_context`, `ragota_services`, `ragota_neighbors` or `ragota_path`
  answers. Ids do not survive a reindex — when one is rejected, fetch a fresh
  one instead of retrying.
- **Confidence is a grade, not a decoration.** Static call edges are fact;
  contract joins are strong; name-similarity and runtime-observed edges are
  leads. A long `ragota_trace` chain multiplies confidences down — treat the
  tail as a hypothesis to confirm in source (one range read), not as a fact
  to report.
- **`unresolved` edges are leads.** A reference marked unresolved names the
  symbol but was never tied to it. Say "likely" when you relay one.
- **The map ignores your repository scope on purpose.** `ragota_services`
  shows the whole estate even when other tools are scoped, because the far
  side of a cross-service call is by definition somewhere else. Two
  repositories may both have a service named `api` — always carry the
  repository qualifier when you report one.
- **`ragota_context` is the expensive door.** A default call has measured
  over ten thousand tokens' worth of answer on a small corpus. Reach for it
  when one answer genuinely spans files; keep `hops: 1`; never use it as a
  default search.

## What this cannot do

- Edges into code that is not indexed (third-party libraries, SaaS) end at
  the boundary: a call with no far side is normal, not a defect.
- Coverage is honest but partial. `ragota_status` with a `repo` reports how
  much of that repository's outbound call surface resolved into edges; a
  ratio well below 1 means "no caller found" may be the indexer's limit.
  Quote that ratio when an empty graph answer matters to a decision.
