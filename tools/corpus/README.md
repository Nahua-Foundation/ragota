# Benchmark corpus

Sixteen real open-source repositories, each here because of the *way* it
declares or calls its contracts. The corpus exists so that "the extractor got
better" is a claim someone can check: clone, index, measure, compare with the
numbers below.

```sh
./clone.sh -d /data/corpus                 # shallow clones, ~15 GB
./bench.py  --corpus /data/corpus --db ~/.ragota/data/ragota.db
./measure.py --corpus /data/corpus --db ~/.ragota/data/ragota.db
```

`bench.py` needs a running server (`--server`, default `http://127.0.0.1:8080`)
and read access to its metadata store (`--db`: a sqlite path or a postgres
DSN — the per-repo counts by unit and edge kind have no API endpoint). Both
scripts write to `corpus-results/`: one JSON per repository, plus
`summary.tsv` and `measure.tsv`. Python 3 standard library, `git`, and `psql`
only if the store is PostgreSQL.

## What each repository is here for

| repository | pattern | what it exercises |
| --- | --- | --- |
| nats-server | messaging-broker | a broker's own subject handling — publish/subscribe that is implementation, not usage |
| apache/kafka | messaging-broker | the same at Java/Scala scale; also the largest repo in the corpus (~6.7k indexed files, ~700k edges) and the reason per-file edge deletes are indexed |
| umbraco | annotation-routing | .NET attribute routing and a genuinely small outbound surface — the control case for "nothing to find" |
| n8n | custom-http-helper | every request goes through the project's own helper; the case where edge counts look plausible and are not |
| GoogleCloudPlatform/microservices-demo | grpc-polyglot | .proto service definitions with clients in Go, Python, Java, C#, Node — cross-repo/cross-language contract linking |
| instana/robot-shop | polyglot-http-queue | five languages, HTTP plus AMQP, small enough to check by hand |
| dotnet/eShop | annotation-routing | minimal APIs and MassTransit-style messaging next to attribute routes |
| spring-petclinic/spring-petclinic-microservices | annotation-routing | Spring `@FeignClient`/`@RestController` — the annotation case with a known-correct answer |
| Netflix/conductor | custom-registry | tasks registered in a registry at runtime; nothing at the call site names the contract |
| elastic/elasticsearch | custom-registry | actions and transport handlers registered through a bespoke layer, at a size where heuristics get expensive |
| grafana | explicit-routing | routes registered by explicit `Get("/api/…", handler)` calls in Go, with a TypeScript frontend calling them |
| hashicorp/consul | explicit-routing | the same in one language, plus its own RPC layer |
| argoproj/argo-cd | grpc-gateway | gRPC with a REST gateway; also the repository that used to fail outright on a dangling symlink in `reposerver/repository/testdata/link-to-nowhere` |
| jellyfin | annotation-routing | large .NET controller surface |
| medusajs/medusa | orm-file-routing | file-system routing plus an ORM — table access with no SQL text anywhere |
| apache/airflow | orm-plugin-registry | SQLAlchemy models and a plugin registry; Python dynamic dispatch |

Three groups matter for reading the results: **annotation-driven** (umbraco,
eShop, petclinic, jellyfin) should have high recall — the contract is written
down; **explicit routing** (grafana, consul) should too, from call sites
instead of annotations; **custom registry / custom helper** (n8n, conductor,
elasticsearch, medusa, airflow) is where an extractor silently returns a
plausible number of edges and misses most of the surface. gRPC
(microservices-demo, argo-cd), brokers (nats-server, kafka, robot-shop) and
ORMs (medusa, airflow) each cover one non-HTTP contract family.

## Current numbers

The measurement run that motivated the coverage work produced three findings
that are worth keeping as fixed points:

| repository | finding |
| --- | --- |
| argo-cd | the pass failed with `read file reposerver/repository/testdata/link-to-nowhere/nowhere.json: no such file or directory`; **1536 files went unindexed because of one dangling symlink**. A file that cannot be read or stat'ed is now skipped, counted and logged, and the run continues. |
| umbraco | 42 outbound HTTP calls — correct. A CMS makes few outbound requests, and coverage should show candidates ≈ edges. |
| n8n | 104 outbound HTTP calls found, thousands missed: every request goes through the project's own helper, so the call sites carry the route literal and produce no edge. |

Umbraco's 42 and n8n's 104 are indistinguishable from the graph alone. That is
what `GET /api/v1/repos/{id}/coverage` exists to answer: it reports, per
contract kind, `candidates` (call sites that look like an outbound contract)
against `edges` (candidates that produced one). Equal counts mean there was
nothing more to find; a large gap means results built on this repository's
graph are incomplete.

A first baseline is recorded below, measured on 2026-08-11 with the AST and
BM25 indexers (no vector index), `indexes.workers: 4`, all twelve repositories
indexed in list order into one SQLite database. Re-run `bench.py` and compare
against it; do not fill a cell from memory or estimation, because the whole
point of the table is comparison against a previous run.

| repository | files | index time | http routes | http calls | rpc calls / impls | produces / consumes | writes / reads |
| --- | --- | --- | --- | --- | --- | --- | --- |
| microservices-demo | 229 | 5s | 1 | 11 | 29 / 133 | 0 / 0 | 0 / 1 |
| robot-shop | 91 | 5s | 34 | 22 | 0 / 0 | 1 / 4 | 0 / 5 |
| eShop | 600 | 5s | 49 | 21 | 9 / 0 | 25 / 18 | 12 / 31 |
| spring-petclinic-microservices | 137 | 5s | 15 | 12 | 8 / 0 | 0 / 0 | 4 / 13 |
| conductor | 918 | 5s | 75 | 11 | 57 / 45 | 0 / 0 | 18 / 453 |
| elasticsearch | 40 001 | 314s | 750 | 56 | 15 / 5 | 0 / 0 | 5 / 853 |
| grafana | 19 151 | 65s | 3 102 | 1 469 | 1 545 / 1 483 | 0 / 0 | 1 415 / 431 |
| consul | 5 087 | 15s | 188 | 911 | 393 / 346 | 0 / 0 | 0 / 0 |
| argo-cd | 4 273 | 15s | 181 | 135 | 484 / 689 | 0 / 0 | 240 / 2 |
| jellyfin | 2 419 | 5s | 422 | 58 | 57 / 0 | 6 / 0 | 89 / 346 |
| medusa | 21 556 | 36s | 4 352 | 439 | 11 / 0 | 0 / 0 | 37 / 49 |
| airflow | 12 167 | 25s | 299 | 1 915 | 35 / 8 | 42 / 1 093 | 110 / 185 |

Across the corpus: 946 services and 1 116 service-to-service links.

Three cells are worth reading as regression guards rather than as counts.
Conductor's 11 http calls were 781 before the Java client rules required an
HTTP receiver — the rest were `Map.put`. Elasticsearch's 750 routes were 0 and
Consul's 188 were 3 before a route could be recognised without a framework
naming it. eShop's messaging and table columns were entirely zero before
RabbitMQ and EF Core were understood, and its 12 writes against 31 reads are
what lets a parameter trace cross a table.

Precision and recall estimates are not in the table: `measure.py` produces them
per repository, and they belong next to a change that claims to move them.

## What the HTTP coverage gap is made of

Measured 2026-08-13 over the nine repositories above, the HTTP column read
3159 edges against 4181 candidates (75.6%). The 1022 call sites in the gap
were classified by re-running the parse and recording, for each site that
produced no edge, the URL expression the parser rejected.

The first thing that measurement settled is what the gap is *not*. Coverage
counts the edges the parser **emits**, not the edges the linker later joins to
a route, so a call to an external API resolves, emits a key and counts as
covered — `http:POST /api/chat.postMessage` is a fine key with nothing to join
it to. None of the 1022 is an external API. Every one of them is a site where
no key could be built at all. "We call somebody else's service" lives in a
different number: of the 3159 emitted edges 1595 linked and 1564 did not, and
those 1564 are 1225 keys no indexed repository serves, 178 that name a route
the index does hold under a different mount prefix (airflow's own tests call
`/dags/{}/…` while its routes are stored `/api/v2/dags/{}/…`), 102 whose path
is a template, and 59 that carry an external host outright.

The 1022, then:

| bucket | sites | what it is |
| --- | --- | --- |
| not an HTTP call | 391 | the rule claimed a call that makes no request: SQLAlchemy `session.get(DagModel, id)` and `session.delete(obj)` (334), a Kubernetes controller-runtime or grpc-gateway `client.Get(ctx, …)` (36), a mapping `.get("key")` on a receiver whose name contains *client* (21). |
| URL not literal | 481 | the target is a variable, a field or a computed expression. |
| literal, rejected | 79 | the path is right there at the call site, written relative to the client's base address — `getForEntity("tasks/queue/size")`, `self.client.get(f"variables/{key}")`. |
| literal, in a template | 71 | consul-ui writes its requests as ``request`GET /v1/kv/${id}` ``: method and path are both literal, inside a tagged template. |

Two of those four are worth acting on and two are not.

The 391 are the same defect as conductor's 781 `Map.put` calls: a denominator
counting sites that were never contracts. They cost nothing to remove and
removing them makes the ratio mean what it says.

The 150 literal ones are a genuine extraction gap with a deterministic fix.

The 481 non-literal ones are not worth chasing. 65 of the 204 outside test
files are in airflow's `providers/` tree — hooks for Databricks, Dataprep,
Weaviate, OpenFaaS, EKS — where resolving the URL perfectly would produce a
key no indexed repository serves. Following a variable to its assignment would
buy a fraction of the remainder at the cost of a guess per site, and the
guesses cannot be checked against anything.

### What closing the first three bought

Same nine repositories, same day, AST indexer only.

| repository | edges/candidates before | after | linked before | after |
| --- | --- | --- | --- | --- |
| microservices-demo | 11/13 | 11/13 | 9 | 9 |
| robot-shop | 22/23 | 22/23 | 6 | 6 |
| eShop | 52/63 | 52/63 | 36 | 36 |
| spring-petclinic-microservices | 12/12 | 12/12 | 10 | 10 |
| conductor | 11/52 | 46/55 | 4 | 23 |
| consul | 911/1085 | 910/1084 | 561 | 560 |
| argo-cd | 135/206 | 135/170 | 99 | 99 |
| jellyfin | 90/113 | 90/113 | 66 | 66 |
| airflow | 1915/2614 | 1962/2329 | 804 | 812 |
| **total** | **3159/4181 (75.6%)** | **3240/3862 (83.9%)** | **1595** | **1621** |

The tagged-template bucket was left alone: 71 sites in one repository, spelled
in that repository's own request DSL.

Precision, `measure.py --sample 0` so the comparison is over every edge rather
than a stride that moves when the edge count does: airflow 0.585 → 0.591,
conductor 0.636 → 0.848, consul 0.865 → 0.866, and unchanged everywhere else.
No kind other than http moved at all.

Conductor's is the number to read carefully. Its 35 new edges were all read by
hand — every one is a `getForEntity("tasks/queue/size", …)` in the project's
own REST client with the route literal at the call site, and 19 of them now
link to conductor's own routes. Against the token list as it stood they scored
*unsupported*, dropping the estimate to 0.13, because the only HTTP evidence
on those lines is the callee name and the list knew `resttemplate` but none of
RestTemplate's operations. The list now knows them; the 0.636 baseline above is
the old edge set re-measured with the same list.

Retrieval was checked too, since the keys these edges carry are what the graph
joins on: `tools/eval/compare.py` over the five repositories the change
touches, 32 queries, returned identical ranks for 31 of them. The one that
moved moves the same way with the *same* binary on both sides — see the
re-indexing noise floor in `tools/eval/README.md`.

## What the messaging coverage number was counting

Measured 2026-08-13 over the same nine repositories, messaging read **107 edges
against 415 candidates (25.8%)** — the worst of the four kinds by a wide
margin. The reason to distrust that number rather than act on it: **consul
reported 22 candidates and 0 edges, and consul has no broker in it.** A
denominator that counts call sites in a repository with nothing to find turns
"we are missing three quarters of the messaging" into a sentence about the
counter.

So this section is the one case where the number moving is a *denominator*
shrinking, and where that is the correct outcome. Nothing below is new
extraction except the 17 edges of the last subsection; messaging coverage goes
from 25.8% to 62.8% because 270 of the 415 candidates were never messaging call
sites.

| repository | edges/candidates before | after |
| --- | --- | --- |
| microservices-demo | 0/1 | 0/0 |
| robot-shop | 2/2 | 2/2 |
| eShop | 43/44 | 56/57 |
| spring-petclinic-microservices | 0/0 | 0/0 |
| conductor | 0/0 | 0/0 |
| consul | 0/22 | 0/0 |
| argo-cd | 0/7 | 2/3 |
| jellyfin | 6/6 | 0/0 |
| airflow | 56/333 | 31/83 |
| **total** | **107/415 (25.8%)** | **91/145 (62.8%)** |

No other contract kind moved by a single site: db stays 2052/2077, http
3240/3862, rpc 1072/1072.

### The rule: a messaging site needs a broker

An earlier pass had already narrowed the shape fallback from 71 candidates to
22 by demanding that a destination be *named* somewhere in the call. The 22
that survived name one — `publisher.Publish([]stream.Event{{Topic: …}})` is
consul's in-process event stream, and its events carry a `Topic` field. What
they never had is a broker.

The rule is now: a messaging site needs evidence that a broker exists, either a
receiver named after a broker product (`sqs_client`, `pubsub_hook`,
`rabbitChan`, a `*redis.Client`) or a file that names a broker client library
(`brokerSourceMarkers`, the messaging counterpart of `grpcSourceMarkers`).
Three places were claiming messaging without it, and each is the same mistake —
a call name the language already uses for something else:

| where | 415 → 145 |
| --- | --- |
| python's kafka rules (`send`, `subscribe`, `send_message`, `receive_message`), which carried no receiver filter at all | `SUPERVISOR_COMMS.send(msg)` (airflow's task-supervisor IPC), `generator.send(x)` (the generator protocol), `session.send(prepared)` (a requests adapter), `manager.subscribe(trigger_id=…)` (airflow's trigger manager — 97 sites in one test file), `hook.send_message({"text": …})` (telegram). 221 sites in airflow. |
| go's unresolved-kafka fallback, which counted `Publish`/`Produce`/`Consume`/`ReadMessage` on any receiver | consul's `stream.Publisher.Publish` (14), argo-cd's `wsConn.ReadMessage()` (5) |
| the shape fallback's "the call names a destination field" branch | consul's event stream again (8), on the strength of the `Topic` field of the event it publishes |

What is left in the denominator is a genuine gap: airflow's remaining 52
unresolved sites are Azure Service Bus, SQS, IBM MQ, kafka and redis pub/sub
calls whose queue is `self.queue_name`, and eShop's one is
`_consumerChannel.BasicConsumeAsync(queue: …)`.

The same evidence rule then removed three groups of *edges* — the numerator,
not the denominator. Corpus-wide messaging edges go 1 189 → 127:

| what | edges | why it was wrong |
| --- | --- | --- |
| `@task`-decorated functions read as queue consumers | 1 093 | celery spells it `@shared_task` / `@app.task`; airflow spells its DAG nodes `@task` and locust spells its load-generator steps the same way. Keyed on the function's own name, joined to no producer anywhere in the corpus. |
| `HttpClient.SendAsync(HttpRequestMessage)` and `WebSocket.SendAsync(OutboundWebSocketMessage)` read as event publishes | 10 | a .NET transport takes an argument whose type ends in `Message`, exactly like every integration event does. `Send`/`SendAsync` now need a receiver that dispatches — a mediator, a messenger, a bus. All six of jellyfin's messaging edges were this. |
| `pool.apply_async(…)` and `self.delay(30)` read as celery dispatches | 26 | the celery rule takes the task's name from the receiver, so these were keyed `topic:pool` and `topic:self`. |

The `produces / consumes` column of the baseline table above therefore reads
differently now: airflow 42 / 1 093 → 17 / 50, eShop 25 / 18 → 21 / 35,
jellyfin 6 / 0 → 0 / 0, robot-shop 1 / 4 → 1 / 1, argo-cd 0 / 0 → 1 / 1.

### The estimator could not see a bus that routes on a type

Precision over every messaging edge, `measure.py --sample 0`: **0.086 → 0.740**
(102 supported of 1 189 → 94 of 127). No other kind moved: db 0.377, http
0.670, rpc 0.138 on both sides.

The baseline in that sentence is not the number the tool would have printed
yesterday. It scored eShop's entire RabbitMQ integration layer at **0.000** —
43 correct edges, not one supported — for two reasons that are the same defect
as the RestTemplate one above:

- the messaging tokens were anchored on both sides, and messaging APIs put
  theirs inside a compound name: `BasicPublishAsync`, `PublishThroughEventBusAsync`,
  `AddSubscription`, `xreadgroup`, and `topics` plural. The messaging fragments
  now match anywhere in an identifier; each is a word that appears in code about
  moving messages and nowhere else, which is what licenses dropping the
  boundaries. The other three sets keep theirs — "from", "get" and "channel"
  are not that kind of word.
- a bus that routes on the message's own type names no verb at all. The line
  that receives eShop's most-published contract is `public async Task
  Handle(OrderStatusChangedToPaidIntegrationEvent @event)`, and the evidence in
  it is the type: `IntegrationEvent`, `EventHandler`, `eventBus`, `Mediator`,
  `Messenger`.

With the list fixed, the *old* edge set scores eShop 0.907, jellyfin 0.000,
robot-shop 0.400, airflow 0.054 — and the four unsupported eShop edges are
exactly the four `HttpClient.SendAsync` false positives above. Both sides of the
comparison are measured with the fixed list.

### A contract named in a type parameter

eShop's integration layer declares its subscriptions as
`eventBus.AddSubscription<OrderStockConfirmedIntegrationEvent,
OrderStockConfirmedIntegrationEventHandler>()` — the event, the handler, and no
string anywhere. The runtime agrees with reading the type as the destination:
the same transport registers under `typeof(T).Name` and routes an incoming
message by `@event.GetType().Name` (`src/EventBusRabbitMQ/RabbitMQEventBus.cs`).

The rule is therefore: for a call whose name is a messaging registration verb,
the first type argument that is message-shaped is the destination, and the
other one is the handler. Reading the *first message-shaped* argument rather
than the first argument is what makes one rule cover both orders —
`AddSubscription<TEvent, THandler>` and MAUI's `Register<TRecipient, TMessage>`
— and requiring it to be message-shaped is what keeps `AddSingleton<IFoo,
Foo>()` and every other DI registration out. 17 edges in eShop, 12 distinct
topics, precision 1.000; eShop's `kafka_flow` joins go 14 → 27.

How universal it is, from the corpus rather than from the docs: the idiom is
.NET's, and the reason is that .NET generics survive to runtime. MassTransit
registers `AddConsumer<T>()`, NServiceBus and Rebus subscribe `Subscribe<T>()`,
MediatR sends `Send<T>()` — the rule covers all of them. Java erases its
generics, so its frameworks take a `Class<T>` literal or an annotation instead;
the corpus has no generic messaging registration in Java at all, and a rule
that keyed contracts on JVM type arguments would key them on elasticsearch's
`ActionListener<Void>` and `TransportRequestHandler<T>`. The gate that makes it
safe is the broker test: eShop's own app host writes
`eventing.Subscribe<BeforeStartEvent>(handler)` over in-process application
eventing, and that names no contract.

**Retrieval did not move**, on either selection, on `/search` or `/context`,
with no query changing rank — `tools/eval/compare.py --scope cross-service
--repo eshop --repo robotshop` (9 questions) and `--shape topic` (8), each run
twice. That is not a surprise once the graph is read instead of the ranking:
the four `topic` questions that fail are eShop's, all four describe the
contract in prose ("which service subscribes to catalog product price
changes"), and `contractKeys` builds a topic key only from "the *orders*
queue"-shaped phrasing. Both sides of those contracts are in the graph and
correctly keyed — `topic:ProductPriceChangedIntegrationEvent` has its producer
at `CatalogApi.cs:356` and its consumer at the handler's `:5`, which is the
expected answer line — and no lookup asks for them. That is the same finding
the eval README records for `callers` and `topic`: the `rpc` shape was fixed by
matching from the edges to the question, and these two have not had that
treatment. One extraction gap does remain on that side:
`topic:OrderStockConfirmedIntegrationEvent` still has no publisher, because the
event is assigned by a ternary whose static type is the abstract base — which
is a local-type-inference problem, not a type-parameter one.

## How precision and recall are estimated

Both are heuristics computed from evidence the extractor did not produce, so a
change to the extractor cannot move them by construction:

- **precision** — for a sample of contract edges (300 per kind by default),
  read the source line the edge points at and look for an independent token
  that a call of that kind happens there: `http`/`fetch`/`axios`/`url` for
  http, `grpc`/`stub`/`channel` for rpc, `publish`/`consume`/`topic` for
  messaging, a SQL verb or ORM call for db. An edge whose own line shows none
  is counted as unsupported and sampled into
  `<repo>.measure.json:unsupported_edge_examples`.
- **recall** — scan the indexed sources for lines that advertise an outbound
  contract at a call site (a URL literal, a route template passed to
  something being called, a topic literal next to a publish/subscribe, SQL
  text) and ask how many carry a contract edge within ±2 lines. Uncovered
  lines are sampled into `uncovered_line_examples`.

Known biases, in both directions: comments are skipped but string constants in
tests are not; a route template handed to any call counts, so file-serving
code inflates the http denominator; a `client` variable that is not a client
inflates precision. The numbers are only meaningful compared with another run
of the same scripts.

One bias is structural rather than statistical, and it is the one to watch when
a precision estimate looks impossible: the check reads the edge's **own line**,
so a contract declared on a *definition* — a `@shared_task` function, a
`@KafkaListener` method — is judged by the `def` or the signature, and the
decorator that declares it sits one line above. Those edges score unsupported
however correct they are. When the token list has been extended twice for the
same kind (RestTemplate for http, the event-typed vocabulary for messaging), it
is worth asking whether the next low number is this instead.
