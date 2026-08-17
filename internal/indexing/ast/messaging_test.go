package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Broker detection beyond Kafka. Every case is a shape taken from a real
// service; the assertion is always on the join key, since a producer and a
// consumer only meet when both sides key on the same destination name.

func TestBrokerProduceConsumeShapes(t *testing.T) {
	tests := []struct {
		name     string
		lang     string
		file     string
		src      string
		produces []string
		consumes []string
		absent   []string // topic keys that must not be emitted at all
	}{
		{
			name: "pika publish with keyword arguments and class-attribute literals",
			lang: "python", file: "rabbitmq.py",
			src: `import pika

class Publisher:
    EXCHANGE = 'robot-shop'
    ROUTING_KEY = 'orders'

    def _publish(self, msg, headers):
        self._channel.basic_publish(exchange=self.EXCHANGE,
                                    routing_key=self.ROUTING_KEY,
                                    body=msg)
`,
			produces: []string{TopicKey("orders")},
		},
		{
			name: "pika publish to the default exchange falls back to the exchange name",
			lang: "python", file: "pub.py",
			src: `def send(channel, body):
    channel.basic_publish(exchange='events', routing_key='', body=body)
`,
			produces: []string{TopicKey("events")},
		},
		{
			name: "pika consume names its queue with a keyword",
			lang: "python", file: "worker.py",
			src: `def run(channel):
    channel.basic_consume(queue='orders', on_message_callback=handle)
`,
			consumes: []string{TopicKey("orders")},
		},
		{
			name: "boto3 sqs send and receive key on the queue name behind the url",
			lang: "python", file: "queue.py",
			src: `def send(sqs, body):
    sqs.send_message(QueueUrl='https://sqs.eu-west-1.amazonaws.com/123456789012/order-events', MessageBody=body)

def read(sqs):
    return sqs.receive_message(QueueUrl='https://sqs.eu-west-1.amazonaws.com/123456789012/order-events')
`,
			produces: []string{TopicKey("order-events")},
			consumes: []string{TopicKey("order-events")},
		},
		{
			name: "boto3 sns publish keys on the arn's topic name",
			lang: "python", file: "notify.py",
			src: `def notify(sns_client, msg):
    sns_client.publish(TopicArn='arn:aws:sns:us-east-1:123456789012:order-shipped', Message=msg)
`,
			produces: []string{TopicKey("order-shipped")},
		},
		{
			name: "go amqp publish takes the routing key, consume the queue",
			lang: "go", file: "main.go",
			src: `package main

func publish(rabbitChan *amqp.Channel, body []byte) {
	rabbitChan.Publish("robot-shop", "orders", false, false, amqp.Publishing{Body: body})
}

func consume(rabbitChan *amqp.Channel) {
	msgs, _ := rabbitChan.Consume("orders", "", true, false, false, false, nil)
	_ = msgs
}
`,
			produces: []string{TopicKey("orders")},
			consumes: []string{TopicKey("orders")},
		},
		{
			name: "go amqp091 publish shifts its arguments for the context",
			lang: "go", file: "main.go",
			src: `package main

func publish(amqpChan *amqp.Channel) {
	amqpChan.PublishWithContext(ctx, "events", "orders.created", false, false, msg)
}
`,
			produces: []string{TopicKey("orders.created")},
		},
		{
			name: "go nats publish and subscribe on a connection named nc",
			lang: "go", file: "bus.go",
			src: `package bus

func run(nc *nats.Conn) {
	nc.Publish("orders.created", data)
	nc.Subscribe("orders.shipped", handle)
}
`,
			produces: []string{TopicKey("orders.created")},
			consumes: []string{TopicKey("orders.shipped")},
		},
		{
			name: "a plain network connection is not a nats connection",
			lang: "go", file: "net.go",
			src: `package net

func run(conn *websocket.Conn) {
	conn.Publish("hello there", data)
}
`,
			absent: []string{TopicKey("hello there")},
		},
		{
			name: "go sqs input struct carries the queue url",
			lang: "go", file: "sqs.go",
			src: `package q

func send(client *sqs.Client) {
	client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    "https://sqs.us-east-1.amazonaws.com/123/payments",
		MessageBody: body,
	})
}
`,
			produces: []string{TopicKey("payments")},
		},
		{
			name: "go redis stream add",
			lang: "go", file: "stream.go",
			src: `package stream

func add(rdb *redis.Client) {
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: "order-events", Values: v})
}
`,
			produces: []string{TopicKey("order-events")},
		},
		{
			name: "java rabbit client publish and consume",
			lang: "java", file: "Broker.java",
			src: `package app;

public class Broker {
  void publish(Channel channel, byte[] body) throws Exception {
    channel.basicPublish("robot-shop", "orders", null, body);
  }

  void consume(Channel channel) throws Exception {
    channel.basicConsume("orders", true, consumer);
  }
}
`,
			produces: []string{TopicKey("orders")},
			consumes: []string{TopicKey("orders")},
		},
		{
			name: "spring amqp convertAndSend picks its destination by arity",
			lang: "java", file: "Publisher.java",
			src: `package app;

public class Publisher {
  private RabbitTemplate rabbitTemplate;

  void two(Object msg) {
    rabbitTemplate.convertAndSend("orders.created", msg);
  }

  void three(Object msg) {
    rabbitTemplate.convertAndSend("events", "orders.paid", msg);
  }
}
`,
			produces: []string{TopicKey("orders.created"), TopicKey("orders.paid")},
		},
		{
			name: "rabbit listener names its queues, not its topics",
			lang: "java", file: "Listener.java",
			src: `package app;

public class Listener {
  @RabbitListener(queues = "orders")
  public void handle(String message) {
  }
}
`,
			consumes: []string{TopicKey("orders")},
		},
		{
			name: "jms listener destination",
			lang: "java", file: "JmsListener.java",
			src: `package app;

public class Handler {
  @JmsListener(destination = "shipping.requests")
  public void handle(String message) {
  }
}
`,
			consumes: []string{TopicKey("shipping.requests")},
		},
		{
			name: "csharp rabbit client with named arguments",
			lang: "csharp", file: "Bus.cs",
			src: `namespace App;

public class Bus
{
    public async Task Publish(IChannel channel, byte[] body)
    {
        await channel.BasicPublishAsync(exchange: "events", routingKey: "orders.created", body: body);
    }

    public async Task Consume(IChannel channel)
    {
        await channel.BasicConsumeAsync(queue: "orders-queue", autoAck: false, consumer: consumer);
    }
}
`,
			produces: []string{TopicKey("orders.created")},
			consumes: []string{TopicKey("orders-queue")},
		},
		{
			name: "azure service bus sender and processor name their queue",
			lang: "csharp", file: "Sb.cs",
			src: `namespace App;

public class Sb
{
    public void Wire(ServiceBusClient busClient)
    {
        var sender = busClient.CreateSender("orders");
        var processor = busClient.CreateProcessor("shipments");
    }
}
`,
			produces: []string{TopicKey("orders")},
			consumes: []string{TopicKey("shipments")},
		},
		{
			name: "amqplib publish, sendToQueue and consume",
			lang: "typescript", file: "broker.ts",
			src: `export async function wire(channel: Channel) {
  channel.publish('events', 'orders.created', Buffer.from('x'));
  channel.sendToQueue('orders', Buffer.from('x'));
  await channel.consume('orders', handle);
}
`,
			produces: []string{TopicKey("orders.created"), TopicKey("orders")},
			consumes: []string{TopicKey("orders")},
		},
		{
			name: "google pubsub topic and subscription bindings",
			lang: "typescript", file: "pubsub.ts",
			src: `export async function wire(pubSubClient: PubSub) {
  await pubSubClient.topic('order-events').publishMessage({ data });
  pubSubClient.subscription('projects/p/subscriptions/order-events-sub').on('message', handle);
}
`,
			produces: []string{TopicKey("order-events")},
			consumes: []string{TopicKey("order-events-sub")},
		},
		{
			name: "aws sdk v2 style sqs options object",
			lang: "typescript", file: "sqs.ts",
			src: `export async function send(sqs: any) {
  await sqs.sendMessage({ QueueUrl: 'https://sqs.us-east-1.amazonaws.com/1/jobs', MessageBody: 'x' });
}
`,
			produces: []string{TopicKey("jobs")},
		},
		{
			name: "express res.send is not a publish",
			lang: "javascript", file: "server.js",
			src: `app.get('/health', (req, res) => {
  res.status(500).send('database not available');
});
`,
			absent: []string{TopicKey("database not available")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			for _, want := range tt.produces {
				if findEdge(facts.Edges, storage.EdgeProduces, want) == nil {
					t.Errorf("produces %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeProduces))
				}
			}
			for _, want := range tt.consumes {
				if findEdge(facts.Edges, storage.EdgeConsumes, want) == nil {
					t.Errorf("consumes %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeConsumes))
				}
			}
			for _, not := range tt.absent {
				for _, kind := range []string{storage.EdgeProduces, storage.EdgeConsumes} {
					if findEdge(facts.Edges, kind, not) != nil {
						t.Errorf("%s %q was emitted and should not be", kind, not)
					}
				}
			}
		})
	}
}

// A bus whose publish API takes no destination routes on the message's own
// type, and the handler declares the same type. Both sides must land on the
// same key or eShop's nine integration events join nothing.
func TestEventTypedMessaging(t *testing.T) {
	publisher := `namespace App;

public class GracePeriodManagerService(IEventBus eventBus)
{
    public async Task Check(int orderId)
    {
        var confirmGracePeriodEvent = new GracePeriodConfirmedIntegrationEvent(orderId);
        await eventBus.PublishAsync(confirmGracePeriodEvent);
    }

    public async Task Untyped(IntegrationEvent evt)
    {
        await eventBus.PublishAsync(evt);
    }
}
`
	handler := `namespace App;

public class GracePeriodConfirmedIntegrationEventHandler(ILogger logger)
    : IIntegrationEventHandler<GracePeriodConfirmedIntegrationEvent>
{
    public async Task Handle(GracePeriodConfirmedIntegrationEvent @event)
    {
    }
}
`
	key := TopicKey("GracePeriodConfirmedIntegrationEvent")

	pub := parseFactsOrFail(t, "csharp", "GracePeriodManagerService.cs", publisher)
	if findEdge(pub.Edges, storage.EdgeProduces, key) == nil {
		t.Errorf("produces %q missing from %v", key, edgeNamesOfKind(pub.Edges, storage.EdgeProduces))
	}
	// The abstract base names no destination — the routing key is the runtime
	// type — so it must not become a topic of its own.
	if findEdge(pub.Edges, storage.EdgeProduces, TopicKey("IntegrationEvent")) != nil {
		t.Error("the abstract event base was keyed as a topic")
	}

	sub := parseFactsOrFail(t, "csharp", "GracePeriodConfirmedIntegrationEventHandler.cs", handler)
	if findEdge(sub.Edges, storage.EdgeConsumes, key) == nil {
		t.Errorf("consumes %q missing from %v", key, edgeNamesOfKind(sub.Edges, storage.EdgeConsumes))
	}
}

func TestEventTopicName(t *testing.T) {
	tests := []struct{ typ, want string }{
		{"OrderStartedIntegrationEvent", "OrderStartedIntegrationEvent"},
		{"eShop.Events.OrderShippedEvent", "OrderShippedEvent"},
		{"SubmitOrderCommand", "SubmitOrderCommand"},
		{"IntegrationEvent", ""},
		{"Event", ""},
		{"object", ""},
		{"CancellationToken", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := eventTopicName(tt.typ); got != tt.want {
			t.Errorf("eventTopicName(%q) = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

// The shape fallback is what covers the SDK no rule names yet: a broker-ish
// receiver, a verb that moves a message, and a destination-shaped literal.
func TestBrokerShapeFallback(t *testing.T) {
	tests := []struct {
		name     string
		lang     string
		src      string
		produces []string
		consumes []string
		absent   []string
	}{
		{
			name: "an unrecognized publish on a receiver that names a broker",
			lang: "go",
			src: `package app

func send(pulsarProducer *pulsar.Producer) {
	pulsarProducer.Emit("orders.created", payload)
}
`,
			produces: []string{TopicKey("orders.created")},
		},
		{
			name: "an unrecognized receive on a broker receiver",
			lang: "python",
			src: `def run(message_broker):
    message_broker.receive_messages("orders.shipped")
`,
			consumes: []string{TopicKey("orders.shipped")},
		},
		{
			name: "a sentence in the argument is not a queue name",
			lang: "go",
			src: `package app

func send(eventBus *bus.Bus) {
	eventBus.Emit("failed to reach the broker", err)
}
`,
			absent: []string{TopicKey("failed to reach the broker")},
		},
		{
			name: "a receiver that names nothing broker-like is left alone",
			lang: "go",
			src: `package app

func send(w *widget) {
	w.Emit("orders.created", payload)
}
`,
			absent: []string{TopicKey("orders.created")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, "fallback."+tt.lang, tt.src)
			for _, want := range tt.produces {
				e := findEdge(facts.Edges, storage.EdgeProduces, want)
				if e == nil {
					t.Fatalf("produces %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeProduces))
				}
				if e.Confidence >= 0.7 {
					t.Errorf("shape fallback emitted at confidence %v, want the weakest tier", e.Confidence)
				}
			}
			for _, want := range tt.consumes {
				if findEdge(facts.Edges, storage.EdgeConsumes, want) == nil {
					t.Errorf("consumes %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeConsumes))
				}
			}
			for _, not := range tt.absent {
				for _, kind := range []string{storage.EdgeProduces, storage.EdgeConsumes} {
					if findEdge(facts.Edges, kind, not) != nil {
						t.Errorf("%s %q was emitted and should not be", kind, not)
					}
				}
			}
		})
	}
}

func TestQueueNameLike(t *testing.T) {
	yes := []string{"orders", "orders.created", "order-events", "order_events",
		"$JS.ADVISORY.TEST", "projects/p/topics/t", "svc.*.events"}
	no := []string{"", "ab", "database not available", "https://example.com/x",
		"/api/orders", "SELECT * FROM x", `{"a":1}`, "a1"}
	for _, s := range yes {
		if !queueNameLike(s) {
			t.Errorf("queueNameLike(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if queueNameLike(s) {
			t.Errorf("queueNameLike(%q) = true, want false", s)
		}
	}
}

func TestBrokerName(t *testing.T) {
	tests := []struct {
		v    string
		ref  bool
		want string
	}{
		{v: "orders", ref: true, want: "orders"},
		{v: "arn:aws:sns:us-east-1:123:order-events", ref: true, want: "order-events"},
		{v: "https://sqs.us-east-1.amazonaws.com/123/jobs", ref: true, want: "jobs"},
		{v: "projects/p/subscriptions/sub", ref: true, want: "sub"},
		{v: "orders.created", ref: false, want: "orders.created"},
		{v: "arn:aws:sns:us-east-1:123:x", ref: false, want: "arn:aws:sns:us-east-1:123:x"},
		{v: "${ORDERS_TOPIC}", ref: true, want: "${ORDERS_TOPIC}"},
	}
	for _, tt := range tests {
		if got := brokerName(tt.v, tt.ref); got != tt.want {
			t.Errorf("brokerName(%q, %v) = %q, want %q", tt.v, tt.ref, got, tt.want)
		}
	}
}

func TestTopicSpecSources(t *testing.T) {
	res := func(expr string) (string, bool) { return unquote(expr) }
	cs := &callSite{
		Args:   []string{"'exchange'", "'routing-key'", "body"},
		Kwargs: map[string]string{"queue": "'q-name'"},
		Fields: map[string]string{"QueueUrl": "'https://sqs/1/from-field'"},
	}
	tests := []struct {
		name string
		spec topicSpec
		want string
	}{
		{name: "positional preference order", spec: topicSpec{Args: []int{1, 0}}, want: "routing-key"},
		{name: "falls through an unresolvable position", spec: topicSpec{Args: []int{2, 0}}, want: "exchange"},
		{name: "keyword before position", spec: topicSpec{Kwargs: []string{"queue"}, Args: []int{0}}, want: "q-name"},
		{name: "options field with a reference", spec: topicSpec{Fields: []string{"QueueUrl"}, Ref: true}, want: "from-field"},
		{name: "by arity", spec: topicSpec{ByArity: map[int]int{3: 1}}, want: "routing-key"},
		{name: "arity with no entry finds nothing", spec: topicSpec{ByArity: map[int]int{2: 0}}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := tt.spec.resolve(cs, res); got != tt.want {
				t.Errorf("resolve = %q, want %q", got, tt.want)
			}
		})
	}
}

// A messaging candidate has to have a broker behind it. These are the shapes
// that produced 251 of the corpus's 415 messaging candidates while naming no
// broker anywhere — an in-process event stream, a websocket, a task
// supervisor's IPC channel, a chat SDK — each taken from the repository that
// wrote it.
func TestMessagingNeedsBrokerEvidence(t *testing.T) {
	tests := []struct {
		name       string
		lang       string
		file       string
		src        string
		candidates int
		edges      int
	}{
		{
			// consul: agent/consul/stream. A Publisher, a Publish, and events
			// carrying a Topic field — all of it in one process.
			name: "an in-process event stream is not a broker",
			lang: "go", file: "stream/publisher.go",
			src: `package stream

func emit(publisher *stream.EventPublisher, idx uint64) {
	publisher.Publish([]stream.Event{{Topic: topicServiceHealth, Index: idx}})
}
`,
		},
		{
			name: "the same publish in a file that has a broker is a candidate",
			lang: "go", file: "stream/publisher.go",
			src: `package stream

import "github.com/segmentio/kafka-go"

func emit(publisher *stream.EventPublisher, idx uint64) {
	publisher.Publish([]stream.Event{{Topic: topicServiceHealth, Index: idx}})
}
`,
			candidates: 1,
		},
		{
			// argo-cd: server/application/websocket.go.
			name: "a websocket read is not a consume",
			lang: "go", file: "server/websocket.go",
			src: `package server

func read(wsConn *websocket.Conn) {
	_, message, err := wsConn.ReadMessage()
	_ = message
	_ = err
}
`,
		},
		{
			// airflow: task-sdk's supervisor protocol, and the generator
			// protocol that shares its name with kafka-python's producer.
			name: "an IPC channel and a generator are not producers",
			lang: "python", file: "task_runner.py",
			src: `def run(msg, gen):
    response = SUPERVISOR_COMMS.send(msg)
    gen.send(response)
`,
		},
		{
			name: "a chat client's send_message is not a queue",
			lang: "python", file: "telegram.py",
			src: `def notify(hook):
    hook.send_message({"text": "build finished"})
`,
		},
		{
			// The kafka-python spelling the rule exists for: the receiver
			// names the part it plays and that is corroboration enough for a
			// call name this specific.
			name: "a producer receiver still counts",
			lang: "python", file: "producer.py",
			src: `def emit(producer, topic, payload):
    producer.send("orders.created", payload)
    producer.send(topic, payload)
`,
			candidates: 2, edges: 1,
		},
		{
			// The same file with no producer in sight: only the import says
			// there is a broker, and that is enough.
			name: "a broker import corroborates on its own",
			lang: "python", file: "emit.py",
			src: `from kafka import KafkaProducer

def emit(client, topic, payload):
    client.send(topic, payload)
`,
			candidates: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			got := facts.Coverage[storage.ContractKindMessaging]
			want := storage.CoverageCounts{Candidates: tt.candidates, Edges: tt.edges}
			if got != want {
				t.Errorf("messaging coverage = %+v, want %+v", got, want)
			}
		})
	}
}

// A contract named in a type parameter, which is how the .NET ecosystem
// registers most of its messaging: eShop's AddSubscription<TEvent, THandler>,
// MassTransit's AddConsumer<T> and Publish<T>, Rebus's Subscribe<T>.
func TestGenericTypeArgumentContracts(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		produces []string
		consumes []string
		absent   []string
		handler  string
	}{
		{
			name: "a subscription names the event and the handler",
			src: `namespace App;

public static class Extensions
{
    public static void AddSubscriptions(this IEventBusBuilder eventBus)
    {
        eventBus.AddSubscription<OrderStockConfirmedIntegrationEvent, OrderStockConfirmedIntegrationEventHandler>();
    }
}
`,
			consumes: []string{TopicKey("OrderStockConfirmedIntegrationEvent")},
			handler:  "OrderStockConfirmedIntegrationEventHandler",
		},
		{
			name: "a generic publish is the producing side of the same key",
			src: `namespace App;

public class Sender(IEventBus bus)
{
    public Task Emit() => bus.Publish<OrderStockConfirmedIntegrationEvent>(new (1));
}
`,
			produces: []string{TopicKey("OrderStockConfirmedIntegrationEvent")},
		},
		{
			// MassTransit registers the consumer class, not the message; the
			// message is what the class is named after.
			name: "a consumer registration names the message second",
			src: `namespace App;

public static class Bus
{
    public static void Configure(IRabbitMqBusFactoryConfigurator cfg)
    {
        cfg.AddConsumer<SubmitOrderConsumer, SubmitOrderCommand>();
    }
}
`,
			consumes: []string{TopicKey("SubmitOrderCommand")},
			handler:  "SubmitOrderConsumer",
		},
		{
			name: "dependency injection is not a subscription",
			src: `namespace App;

public static class Extensions
{
    public static void Add(IServiceCollection services)
    {
        services.AddSingleton<IOrderRepository, OrderRepository>();
        services.AddKeyedTransient<IIntegrationEventHandler, OrderPaidIntegrationEventHandler>(typeof(OrderPaidIntegrationEvent));
    }
}
`,
			absent: []string{TopicKey("OrderPaidIntegrationEvent")},
		},
		{
			// eShop's own app host, two projects away from the event bus:
			// the same call shape over in-process application eventing.
			name: "in-process eventing with no broker names no topic",
			src: `namespace App;

public class Subscriber
{
    public Task SubscribeAsync(IDistributedApplicationEventing eventing)
    {
        eventing.Subscribe<BeforeStartEvent>((@event, ct) => Task.CompletedTask);
        return Task.CompletedTask;
    }
}
`,
			absent: []string{TopicKey("BeforeStartEvent")},
		},
		{
			name: "the declaration of a generic registration names no contract",
			src: `namespace App;

public static class EventBusBuilderExtensions
{
    public static IEventBusBuilder AddSubscription<T, TH>(this IEventBusBuilder eventBus)
        where T : IntegrationEvent
        where TH : class, IIntegrationEventHandler<T>
    {
        eventBus.Services.AddKeyedTransient<IIntegrationEventHandler, TH>(typeof(T));
        return eventBus;
    }
}
`,
			absent: []string{TopicKey("T"), TopicKey("TH")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, "csharp", "Extensions.cs", tt.src)
			for _, want := range tt.produces {
				if findEdge(facts.Edges, storage.EdgeProduces, want) == nil {
					t.Errorf("produces %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeProduces))
				}
			}
			for _, want := range tt.consumes {
				e := findEdge(facts.Edges, storage.EdgeConsumes, want)
				if e == nil {
					t.Fatalf("consumes %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeConsumes))
				}
				if got := storage.DecodeEdgeMeta(e.Meta).Fields["handler"]; got != tt.handler {
					t.Errorf("handler recorded as %q, want %q", got, tt.handler)
				}
			}
			for _, not := range tt.absent {
				for _, kind := range []string{storage.EdgeProduces, storage.EdgeConsumes} {
					if findEdge(facts.Edges, kind, not) != nil {
						t.Errorf("%s %q was emitted and should not be", kind, not)
					}
				}
			}
		})
	}
}

func TestGenericTopicContract(t *testing.T) {
	tests := []struct {
		name     string
		call     string
		typeArgs []string
		kind     string
		topic    string
		handler  string
	}{
		{name: "subscription", call: "AddSubscription", typeArgs: []string{"OrderPaidIntegrationEvent", "OrderPaidIntegrationEventHandler"},
			kind: storage.EdgeConsumes, topic: "OrderPaidIntegrationEvent", handler: "OrderPaidIntegrationEventHandler"},
		{name: "message second", call: "Register", typeArgs: []string{"CatalogViewModel", "ProductCountChangedMessage"},
			kind: storage.EdgeConsumes, topic: "ProductCountChangedMessage"},
		{name: "publish", call: "PublishAsync", typeArgs: []string{"eShop.Events.OrderShippedEvent"},
			kind: storage.EdgeProduces, topic: "OrderShippedEvent"},
		{name: "a type variable names nothing", call: "AddSubscription", typeArgs: []string{"T", "TH"}},
		{name: "the abstract base names nothing", call: "Publish", typeArgs: []string{"IntegrationEvent"}},
		{name: "not a messaging verb", call: "AddSingleton", typeArgs: []string{"OrderPaidIntegrationEvent", "Handler"}},
		{name: "no type arguments", call: "Publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, topic, handler, ok := genericTopicContract(tt.call, tt.typeArgs)
			if ok != (tt.topic != "") {
				t.Fatalf("ok = %v, want %v", ok, tt.topic != "")
			}
			if !ok {
				return
			}
			if kind != tt.kind || topic != tt.topic || handler != tt.handler {
				t.Errorf("= (%s, %s, %s), want (%s, %s, %s)", kind, topic, handler, tt.kind, tt.topic, tt.handler)
			}
		})
	}
}

// The two publish verbs .NET also gives its transports. Ten of the corpus's
// messaging edges were an HTTP request and a websocket frame, both of them
// types that end in Message.
func TestTransportSendIsNotAPublish(t *testing.T) {
	src := `namespace App;

public class Svc(HttpClient httpClient, IMediator mediator)
{
    public Task Forward(HttpRequestMessage requestMessage) => httpClient.SendAsync(requestMessage);

    public Task Frame(IWebSocket socket) => socket.SendAsync(new OutboundKeepAliveMessage());

    public Task Cancel(CancelOrderCommand command) => mediator.Send(command);
}
`
	facts := parseFactsOrFail(t, "csharp", "Svc.cs", src)
	for _, not := range []string{TopicKey("HttpRequestMessage"), TopicKey("OutboundKeepAliveMessage")} {
		if findEdge(facts.Edges, storage.EdgeProduces, not) != nil {
			t.Errorf("produces %q was emitted and should not be", not)
		}
	}
	if findEdge(facts.Edges, storage.EdgeProduces, TopicKey("CancelOrderCommand")) == nil {
		t.Errorf("a mediator send is still a dispatch: %v", edgeNamesOfKind(facts.Edges, storage.EdgeProduces))
	}
}

// A bare task decorator names a queue only where there is a broker to queue
// on: celery spells it @shared_task, and airflow spells its DAG nodes @task.
func TestBareTaskDecoratorNeedsABroker(t *testing.T) {
	celery := `from celery import shared_task

@shared_task
def process_order(order_id):
    pass
`
	dag := `from airflow.sdk import dag, task

@task
def produce_asset_events():
    pass

@task(retries=3)
def consume_asset_event():
    pass
`
	facts := parseFactsOrFail(t, "python", "tasks.py", celery)
	if findEdge(facts.Edges, storage.EdgeConsumes, TopicKey("process_order")) == nil {
		t.Errorf("celery task consumer missing from %v", edgeNamesOfKind(facts.Edges, storage.EdgeConsumes))
	}
	facts = parseFactsOrFail(t, "python", "example_dag.py", dag)
	for _, not := range []string{TopicKey("produce_asset_events"), TopicKey("consume_asset_event")} {
		if findEdge(facts.Edges, storage.EdgeConsumes, not) != nil {
			t.Errorf("a dag node was read as a queue consumer: %s", not)
		}
	}
}

// The celery dispatch takes its destination from the receiver's name, so a
// receiver that cannot be a task names nothing.
func TestCeleryDispatchReceiver(t *testing.T) {
	src := `from myapp.tasks import process_order

def run(pool, image_params):
    process_order.delay(1)
    pool.apply_async(build_image, kwds=image_params)
    self.delay(30)
`
	facts := parseFactsOrFail(t, "python", "run.py", src)
	if findEdge(facts.Edges, storage.EdgeProduces, TopicKey("process_order")) == nil {
		t.Errorf("celery dispatch missing from %v", edgeNamesOfKind(facts.Edges, storage.EdgeProduces))
	}
	for _, not := range []string{TopicKey("pool"), TopicKey("self")} {
		if findEdge(facts.Edges, storage.EdgeProduces, not) != nil {
			t.Errorf("%q was emitted and should not be", not)
		}
	}
	if got := facts.Coverage[storage.ContractKindMessaging]; got != (storage.CoverageCounts{Candidates: 1, Edges: 1}) {
		t.Errorf("messaging coverage = %+v, want one site", got)
	}
}
