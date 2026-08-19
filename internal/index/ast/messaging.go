package ast

import (
	"bytes"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// brokerSourceMarkers are the lowercased fragments that show a file depends on
// a message-broker client. Matching the source text rather than each
// language's import syntax keeps this language-neutral, exactly as
// grpcSourceMarkers does: the library's name is the signal and it is spelled
// the same in every import form.
//
// This is the corroboration a call name like send, subscribe or publish needs
// before it is read as messaging. It is deliberately a *library* list and not
// a vocabulary list: "queue", "event" and "message" are words every codebase
// uses, and a file that contains none of the names below has no broker client
// in it, whatever its variables are called.
var brokerSourceMarkers = append(append([]string{}, brokerVendorMarkers...), []string{
	// kafka clients: sarama, segmentio, confluent, franz, kafkajs, aiokafka,
	// kafka-python, spring-kafka — all of them spell "kafka" except sarama.
	"sarama",
	// AMQP / RabbitMQ clients across the languages.
	"pika", "kombu", "amqplib", "amqp091", "streadway", "aio_pika", "easynetq",
	"masstransit", "nservicebus", "rebus",
	// JMS and the brokers that speak it.
	"javax.jms", "jakarta.jms", "activemq", "rocketmq", "org.springframework.jms",
	// cloud and appliance brokers.
	"ibmmq", "ibm.mq", "pymqi", "azure.messaging", "cloud.pubsub", "eventstore",
	"solace", "paho",
	// job queues that carry a destination name the same way.
	"celery", "bullmq", "ioredis", "zeromq", "pyzmq",
}...)

// hasBrokerImport reports whether the file text names a broker client library.
func hasBrokerImport(src []byte) bool {
	lower := bytes.ToLower(src)
	for _, m := range brokerSourceMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return true
		}
	}
	return false
}

// Messaging detection beyond Kafka: AMQP/RabbitMQ, AWS SQS/SNS, NATS, Azure
// Service Bus, Google Pub/Sub and Redis streams, expressed as the same
// declarative rules Kafka uses (see frameworks.go).
//
// Every rule keys its edge on the destination *name* — the topic, the routing
// key, the queue, the subject — because that is the only token both sides of
// the contract spell the same way. Where a broker addresses its destination by
// a queue URL, an ARN or a subscription path, the rule sets topicSpec.Ref and
// the name is taken from the trailing component.
//
// The receiver filters matter as much as the call names: "send", "publish" and
// "consume" are the names of half of every SDK, and a rule that accepts any
// receiver keys contracts on log lines and content types. Each table below
// therefore gates on a receiver that names a broker, except where the call
// name is itself unambiguous (basic_publish, XAdd).

// ---------------------------------------------------------------------------
// Go
// ---------------------------------------------------------------------------

// goAMQPRecv matches an AMQP channel: the vendor name, or the "Chan"/"Channel"
// suffix the amqp libraries' examples give the handle.
var goAMQPRecv = objectMatch{
	Contains: []string{"rabbit", "amqp", "broker"},
	Suffix:   []string{"Chan", "Channel", "chan", "channel"},
	Fold:     false,
}

// goNATSRecv matches a NATS connection. "conn" is deliberately absent: it is
// what every network connection in every Go codebase is called, and Publish is
// a method plenty of them have.
var goNATSRecv = objectMatch{
	Exact:    []string{"nc", "js", "nc1", "nc2"},
	Contains: []string{"nats", "jetstream", "Nats", "JetStream"},
}

// goMessageFieldMethods are the calls whose message argument is a struct
// literal worth scanning for a destination field. Collecting the fields of
// every call would be a second pass over every argument list in the file.
var goMessageFieldMethods = map[string]bool{
	"SendMessage": true, "SendMessageBatch": true, "SendMessageWithContext": true,
	"Publish": true, "PublishWithContext": true, "PublishBatch": true,
	"XAdd": true, "XReadGroup": true, "XRead": true,
	"ReceiveMessage": true, "ReceiveMessageWithContext": true,
}

var goBrokerProduce = []kafkaProduceRule{
	// AMQP: ch.Publish(exchange, routingKey, mandatory, immediate, msg).
	// The routing key is the name a consumer binds its queue with, so it wins
	// over the exchange; a publish to the default exchange names only the
	// exchange, which is why both positions are declared.
	{Object: goAMQPRecv, Methods: []string{"Publish", "PublishMsg"}, Topic: topicSpec{Args: []int{1, 0}}, Conf: contract.ConfHigh},
	// amqp091-go: ch.PublishWithContext(ctx, exchange, routingKey, ...).
	{Object: goAMQPRecv, Methods: []string{"PublishWithContext"}, Topic: topicSpec{Args: []int{2, 1}}, Conf: contract.ConfHigh},
	// NATS: nc.Publish("subject", data), js.Publish(ctx, "subject", data).
	{Object: goNATSRecv, Methods: []string{"Publish", "PublishMsg", "PublishAsync"}, Topic: topicSpec{Args: []int{0, 1}}, Loose: true, Conf: contract.ConfHigh},
	// AWS SDK: sqs.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: url}),
	// sns.Publish(ctx, &sns.PublishInput{TopicArn: arn}).
	{
		Methods: []string{"SendMessage", "SendMessageBatch", "SendMessageWithContext"},
		Topic:   topicSpec{Fields: []string{"QueueUrl", "QueueName"}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHigh,
	},
	{
		Object:  objectMatch{Contains: []string{"sns", "SNS", "Sns"}},
		Methods: []string{"Publish", "PublishWithContext", "PublishBatch"},
		Topic:   topicSpec{Fields: []string{"TopicArn", "TargetArn"}, Ref: true},
		Conf:    contract.ConfHigh,
	},
	// Redis streams: rdb.XAdd(ctx, &redis.XAddArgs{Stream: "events"}).
	{Methods: []string{"XAdd"}, Topic: topicSpec{Fields: []string{"Stream"}}, Loose: true, Conf: contract.ConfHigh},
}

var goBrokerConsume = []kafkaConsumeRule{
	// AMQP: ch.Consume(queue, consumerTag, autoAck, ...).
	{Object: goAMQPRecv, Methods: []string{"Consume", "ConsumeWithContext"}, Topic: topicSpec{Args: []int{0}}, Conf: contract.ConfHigh},
	// NATS: nc.Subscribe("subject", handler), nc.QueueSubscribe(subj, group, h).
	{
		Object:  goNATSRecv,
		Methods: []string{"Subscribe", "SubscribeSync", "QueueSubscribe", "QueueSubscribeSync", "ChanSubscribe"},
		Topic:   topicSpec{Args: []int{0}},
		Loose:   true,
		Conf:    contract.ConfHigh,
	},
	// AWS SDK: sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: url}).
	{
		Methods: []string{"ReceiveMessage", "ReceiveMessageWithContext"},
		Topic:   topicSpec{Fields: []string{"QueueUrl", "QueueName"}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHigh,
	},
	// Redis streams: rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Streams: [...]}).
	{Methods: []string{"XReadGroup", "XRead"}, Topic: topicSpec{Fields: []string{"Stream", "Streams"}, List: true}, Loose: true, Conf: contract.ConfHeuristic},
}

// ---------------------------------------------------------------------------
// Python
// ---------------------------------------------------------------------------

var pyBrokerRecv = objectMatch{
	Contains: []string{"channel", "rabbit", "amqp", "sqs", "sns", "nats", "nc",
		"publisher", "subscriber", "producer", "broker", "bus", "redis", "pubsub"},
	Fold: true,
}

var pyBrokerProduce = []kafkaProduceRule{
	// pika: channel.basic_publish(exchange=..., routing_key=..., body=...),
	// or positionally, basic_publish(exchange, routing_key, body).
	{
		Methods: []string{"basic_publish"},
		Topic:   topicSpec{Kwargs: []string{"routing_key", "exchange"}, Args: []int{1, 0}},
		Conf:    contract.ConfHigh,
	},
	// boto3: sqs.send_message(QueueUrl=..., MessageBody=...). send_message is
	// also what every chat SDK calls its one and only verb — telegram, slack,
	// sendgrid — so the queue client has to be in view.
	{
		Methods:     []string{"send_message", "send_message_batch"},
		Topic:       topicSpec{Kwargs: []string{"QueueUrl", "QueueName"}, Args: []int{0}, Ref: true},
		NeedsBroker: true,
		Conf:        contract.ConfHigh,
	},
	// boto3 SNS / google pub-sub / nats / redis, all named "publish": the
	// receiver is what separates them from every other publish() in python.
	{
		Object:  pyBrokerRecv,
		Methods: []string{"publish", "publish_message", "xadd"},
		Topic:   topicSpec{Kwargs: []string{"TopicArn", "TargetArn", "topic", "channel", "stream"}, Args: []int{0}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
}

var pyBrokerConsume = []kafkaConsumeRule{
	// pika: channel.basic_consume(queue='orders', on_message_callback=cb).
	{
		Methods: []string{"basic_consume", "basic_get"},
		Topic:   topicSpec{Kwargs: []string{"queue"}, Args: []int{0}},
		Conf:    contract.ConfHigh,
	},
	// boto3: sqs.receive_message(QueueUrl=...).
	{
		Methods:     []string{"receive_message", "receive_messages"},
		Topic:       topicSpec{Kwargs: []string{"QueueUrl", "QueueName"}, Args: []int{0}, Ref: true},
		NeedsBroker: true,
		Conf:        contract.ConfHigh,
	},
	// redis streams: r.xreadgroup(group, consumer, {'events': '>'}) reads its
	// streams from a dict, which the list resolver flattens to its keys' values;
	// r.xread({'events': '$'}) is the same shape without the group.
	{
		Object:  pyBrokerRecv,
		Methods: []string{"xreadgroup", "xread"},
		Topic:   topicSpec{Args: []int{2, 0}, List: true},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
}

// ---------------------------------------------------------------------------
// JVM (java, kotlin)
// ---------------------------------------------------------------------------

var jvmAMQPRecv = objectMatch{
	Contains: []string{"channel", "rabbit", "amqp", "jms", "broker", "bus", "template"},
	Fold:     true,
}

var jvmBrokerProduce = []kafkaProduceRule{
	// RabbitMQ java client: channel.basicPublish(exchange, routingKey, props, body).
	{Methods: []string{"basicPublish"}, Topic: topicSpec{Args: []int{1, 0}}, Conf: contract.ConfHigh},
	// Spring AMQP / JMS: rabbitTemplate.convertAndSend(msg) /
	// (routingKey, msg) / (exchange, routingKey, msg). The destination moves
	// with the arity, and the one-argument form names none.
	{
		Object:   jvmAMQPRecv,
		RecvType: jvmAMQPRecv,
		Methods:  []string{"convertAndSend", "convertSendAndReceive", "send"},
		Topic:    topicSpec{ByArity: map[int]int{2: 0, 3: 1, 4: 1}},
		Loose:    true,
		Conf:     contract.ConfHigh,
	},
	// Spring Cloud Stream: streamBridge.send("orders-out-0", payload).
	{
		Object:  objectMatch{Contains: []string{"streambridge"}, Fold: true},
		Methods: []string{"send"},
		Topic:   topicSpec{Args: []int{0}},
		Conf:    contract.ConfHigh,
	},
	// NATS java: connection.publish(subject, data).
	{
		Object:  objectMatch{Contains: []string{"nats", "connection", "publisher"}, Fold: true},
		Methods: []string{"publish"},
		Topic:   topicSpec{Args: []int{0}},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
}

var jvmBrokerConsume = []kafkaConsumeRule{
	// RabbitMQ java client: channel.basicConsume(queue, autoAck, consumer).
	{Methods: []string{"basicConsume", "basicGet"}, Topic: topicSpec{Args: []int{0}}, Conf: contract.ConfHigh},
	// Spring JMS / AMQP polling: jmsTemplate.receive("queue").
	{
		Object:   jvmAMQPRecv,
		RecvType: jvmAMQPRecv,
		Methods:  []string{"receive", "receiveAndConvert"},
		Topic:    topicSpec{Args: []int{0}},
		Loose:    true,
		Conf:     contract.ConfHigh,
	},
	// NATS java: connection.subscribe(subject) / createDispatcher().subscribe(s).
	{
		Object:  objectMatch{Contains: []string{"nats", "connection", "dispatcher"}, Fold: true},
		Methods: []string{"subscribe"},
		Topic:   topicSpec{Args: []int{0}},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
}

// jvmListenerAnnotations are the annotations that bind a method to a
// destination, mapped to the annotation elements that name it. The unnamed
// "value" element is always accepted on top of these; a Rabbit listener names
// its queues with `queues =`, which the Kafka-shaped `topics =` never matched.
var jvmListenerAnnotations = map[string][]string{
	"KafkaListener":    {"topics", "topicPattern"},
	"RabbitListener":   {"queues", "queuesToDeclare"},
	"StreamListener":   nil,
	"JmsListener":      {"destination"},
	"SqsListener":      {"value", "queueNames"},
	"ServiceActivator": {"inputChannel"},
	"NatsListener":     {"subject", "subjects"},
	"PulsarListener":   {"topics"},
}

// ---------------------------------------------------------------------------
// C#
// ---------------------------------------------------------------------------

var csBrokerRecv = objectMatch{
	Contains: []string{"channel", "rabbit", "amqp", "bus", "broker", "sqs", "sns",
		"queue", "topic", "publisher", "producer", "sender", "servicebus"},
	Fold: true,
}

var csBrokerProduce = []kafkaProduceRule{
	// RabbitMQ.Client: channel.BasicPublish(exchange: X, routingKey: Y, ...).
	// The client's own API is named-argument first, which is why the keyword
	// names come before the positions here.
	{
		Methods: []string{"BasicPublish", "BasicPublishAsync"},
		Topic:   topicSpec{Kwargs: []string{"routingKey", "exchange"}, Args: []int{1, 0}},
		Conf:    contract.ConfHigh,
	},
	// AWS SDK: sqs.SendMessageAsync(queueUrl, body) / sns.PublishAsync(topicArn, m).
	{
		Object:  objectMatch{Contains: []string{"sqs", "sns"}, Fold: true},
		Methods: []string{"SendMessageAsync", "SendMessage", "PublishAsync", "Publish"},
		Topic:   topicSpec{Kwargs: []string{"QueueUrl", "TopicArn"}, Args: []int{0}, Ref: true},
		Conf:    contract.ConfHigh,
	},
	// Azure Service Bus: client.CreateSender("orders") declares the queue this
	// service sends to; the SendMessageAsync that follows names none.
	{
		Object:  csBrokerRecv,
		Methods: []string{"CreateSender"},
		Topic:   topicSpec{Args: []int{0}},
		Conf:    contract.ConfHeuristic,
	},
}

var csBrokerConsume = []kafkaConsumeRule{
	// RabbitMQ.Client: channel.BasicConsume(queue: q, ...).
	{
		Methods: []string{"BasicConsume", "BasicConsumeAsync"},
		Topic:   topicSpec{Kwargs: []string{"queue"}, Args: []int{0}},
		Conf:    contract.ConfHigh,
	},
	// AWS SDK: sqs.ReceiveMessageAsync(queueUrl).
	{
		Object:  objectMatch{Contains: []string{"sqs"}, Fold: true},
		Methods: []string{"ReceiveMessageAsync", "ReceiveMessage"},
		Topic:   topicSpec{Kwargs: []string{"QueueUrl"}, Args: []int{0}, Ref: true},
		Conf:    contract.ConfHigh,
	},
	// Azure Service Bus: client.CreateProcessor("orders") / CreateReceiver(q).
	{
		Object:  csBrokerRecv,
		Methods: []string{"CreateProcessor", "CreateReceiver"},
		Topic:   topicSpec{Args: []int{0}},
		Conf:    contract.ConfHeuristic,
	},
}

// ---------------------------------------------------------------------------
// TypeScript / JavaScript
// ---------------------------------------------------------------------------

var tsBrokerRecv = objectMatch{
	Contains: []string{"channel", "rabbit", "amqp", "sqs", "sns", "nats", "redis",
		"pubsub", "broker", "bus", "client", "publisher", "producer"},
	Fold: true,
}

var tsBrokerProduce = []kafkaProduceRule{
	// amqplib: channel.publish(exchange, routingKey, content) and
	// channel.sendToQueue(queue, content).
	{Object: tsBrokerRecv, Methods: []string{"publish"}, Topic: topicSpec{Args: []int{1, 0}}, Loose: true, Conf: contract.ConfHigh},
	{Methods: []string{"sendToQueue"}, Topic: topicSpec{Args: []int{0}}, Conf: contract.ConfHigh},
	// AWS SDK v2/v3: sqs.sendMessage({QueueUrl}), sns.publish({TopicArn}).
	{
		Methods: []string{"sendMessage", "sendMessageBatch"},
		Topic:   topicSpec{Fields: []string{"QueueUrl", "QueueName"}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHigh,
	},
	// google-cloud/pubsub: pubSub.topic('orders').publishMessage({...}). The
	// topic is named by the binding call, not by the publish.
	{
		Object:  objectMatch{Contains: []string{"pubsub"}, Fold: true},
		Methods: []string{"topic"},
		Topic:   topicSpec{Args: []int{0}},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
	// NestJS transport clients: client.emit('order_created', payload).
	{Object: tsBrokerRecv, Methods: []string{"emit"}, Topic: topicSpec{Args: []int{0}}, Loose: true, Conf: contract.ConfHeuristic},
	// ioredis: redis.xadd('events', '*', ...) / redis.publish('channel', m).
	{Object: objectMatch{Contains: []string{"redis"}, Fold: true}, Methods: []string{"xadd"}, Topic: topicSpec{Args: []int{0}}, Conf: contract.ConfHigh},
}

var tsBrokerConsume = []kafkaConsumeRule{
	// amqplib: channel.consume(queue, handler).
	{Object: tsBrokerRecv, Methods: []string{"consume"}, Topic: topicSpec{Args: []int{0}}, Loose: true, Conf: contract.ConfHigh},
	// AWS SDK: sqs.receiveMessage({QueueUrl}).
	{
		Methods: []string{"receiveMessage"},
		Topic:   topicSpec{Fields: []string{"QueueUrl", "QueueName"}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHigh,
	},
	// google-cloud/pubsub: pubSub.subscription('orders-sub').on('message', h).
	{
		Object:  objectMatch{Contains: []string{"pubsub"}, Fold: true},
		Methods: []string{"subscription"},
		Topic:   topicSpec{Args: []int{0}, Ref: true},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
	// ioredis: redis.xreadgroup('GROUP', g, c, 'STREAMS', 'events', '>').
	{
		Object:  objectMatch{Contains: []string{"redis"}, Fold: true},
		Methods: []string{"xreadgroup", "xread"},
		Topic:   topicSpec{Args: []int{4, 0}},
		Loose:   true,
		Conf:    contract.ConfHeuristic,
	},
}

// ---------------------------------------------------------------------------
// Event-typed messaging (C#-style buses)
// ---------------------------------------------------------------------------

// eventPublishVerbs are the calls that hand an event object to a bus. The
// destination is the event's own type: eShop's RabbitMQ transport routes on
// `@event.GetType().Name` and registers subscriptions under `typeof(T).Name`,
// and MassTransit routes Publish<T> on T. So the class name is the topic, and
// it is the only token the publisher and the handler share.
var eventPublishVerbs = map[string]bool{
	"Publish": true, "PublishAsync": true, "PublishThroughEventBusAsync": true,
	"Send": true, "SendAsync": true, "Dispatch": true, "DispatchAsync": true,
	"AddAndSaveEventAsync": true,
}

// eventSendVerbs are the members of that table that .NET also uses for its
// transports: HttpClient.SendAsync(HttpRequestMessage) and
// WebSocket.SendAsync(OutboundWebSocketMessage) end in Message like every
// event does, and were read as ten published contracts across eShop and
// jellyfin. For these two names the receiver has to name a dispatcher.
var eventSendVerbs = map[string]bool{"Send": true, "SendAsync": true}

// eventDispatcherMarkers name an object that hands a message to somebody else
// — a bus, a mediator, a messenger, a queue. A socket and an HTTP client do
// not, whatever the thing they are handed is called.
var eventDispatcherMarkers = []string{
	"mediator", "messenger", "bus", "broker", "publisher", "dispatcher",
	"queue", "eventservice", "integrationevent",
}

// eventPublishReceiverOK reports whether a publish verb may be read as a
// message dispatch on this receiver.
func eventPublishReceiverOK(name, object, recvType string) bool {
	if !eventSendVerbs[name] {
		return true
	}
	return containsAnyFold(object, eventDispatcherMarkers) || containsAnyFold(recvType, eventDispatcherMarkers)
}

// eventHandlerInterfaces are the generic interfaces whose type argument names
// the event a class handles.
var eventHandlerInterfaces = map[string]bool{
	"IIntegrationEventHandler": true, "IEventHandler": true, "IConsumer": true,
	"IMessageHandler": true, "IHandleMessages": true, "IMessageConsumer": true,
}

// eventTypeSuffixes are the type-name endings that mark a value as a message
// rather than as any other argument a publish verb might take.
var eventTypeSuffixes = []string{"IntegrationEvent", "Event", "Message", "Command", "Notification"}

// eventBaseTypes are the abstract bases every message derives from. A publish
// whose argument is statically one of these names no destination — the routing
// key is the runtime type — so keying an edge on the base would join every
// message in the system to every other.
var eventBaseTypes = map[string]bool{
	"IntegrationEvent": true, "Event": true, "Message": true, "Command": true,
	"Notification": true, "DomainEvent": true, "IEvent": true, "IMessage": true,
	"INotification": true, "object": true,
}

// eventTopicName returns the destination a message type maps to, or "" when
// the type is not a message or is the abstract base of one.
func eventTopicName(typ string) string {
	t := lastComponent(trimGenericArgs(typ))
	if t == "" || eventBaseTypes[t] {
		return ""
	}
	for _, suffix := range eventTypeSuffixes {
		if len(t) > len(suffix) && hasSuffixFold(t, suffix) {
			return t
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Contracts named in a type parameter
// ---------------------------------------------------------------------------
//
// A generic registration declares the contract in a type argument and nothing
// else: eShop's `eventBus.AddSubscription<OrderStockConfirmedIntegrationEvent,
// OrderStockConfirmedIntegrationEventHandler>()` names the queue it binds, the
// class that handles it, and no string at all. The runtime agrees with that
// reading — the same transport registers the subscription under
// `typeof(T).Name` and routes an incoming message by `@event.GetType().Name`
// (src/EventBusRabbitMQ/RabbitMQEventBus.cs) — so the type argument *is* the
// destination name, spelled exactly as the publisher spells it.
//
// The idiom is not one repository's: MassTransit registers `AddConsumer<T>()`
// and publishes `Publish<T>()`, NServiceBus and Rebus subscribe `Subscribe<T>()`,
// MediatR sends `Send<T>()`. It is, however, a *.NET* idiom, and for a reason:
// generics that survive to runtime are what let a framework read the type's
// name. Java erases them, so its libraries take a `Class<T>` literal or an
// annotation instead (@KafkaListener, @RabbitListener — see
// jvmListenerAnnotations), and a rule that keyed contracts on JVM type
// arguments would key them on `ActionListener<Void>` and `TransportRequestHandler<T>`.

// genericTopicVerbs are the calls whose type argument names a destination,
// mapped to the direction the message travels. Registration verbs
// (AddSubscription, AddConsumer) are consumption: the call binds a handler to
// a destination even though it moves no message itself.
var genericTopicVerbs = map[string]string{
	"addsubscription": store.EdgeConsumes, "addconsumer": store.EdgeConsumes,
	"addsubscriber": store.EdgeConsumes, "addeventhandler": store.EdgeConsumes,
	"addmessagehandler": store.EdgeConsumes, "addhandler": store.EdgeConsumes,
	"subscribe": store.EdgeConsumes, "subscribeasync": store.EdgeConsumes,
	"consume": store.EdgeConsumes, "consumeasync": store.EdgeConsumes,
	"register": store.EdgeConsumes, "registerhandler": store.EdgeConsumes,
	"registerconsumer": store.EdgeConsumes, "handle": store.EdgeConsumes,

	"publish": store.EdgeProduces, "publishasync": store.EdgeProduces,
	"send": store.EdgeProduces, "sendasync": store.EdgeProduces,
	"dispatch": store.EdgeProduces, "dispatchasync": store.EdgeProduces,
	"enqueue": store.EdgeProduces,
}

// genericHandlerSuffixes name the *other* type argument of a registration: the
// class that receives the message. It is the consumer's identity, never the
// destination, and it is kept so the edge says which class the subscription
// binds — the registration site itself names no handler anywhere else.
var genericHandlerSuffixes = []string{"Handler", "Consumer", "Listener", "Subscriber", "Receiver", "Worker"}

// genericTopicContract reads a call that names its contract in a type
// argument. It returns the edge kind, the destination, and the handler type
// when the call names one.
//
// The destination is the first type argument that is message-shaped
// (eventTopicName), which is what makes one rule cover both argument orders:
// `AddSubscription<TEvent, THandler>` names the event first and
// `Register<TRecipient, TMessage>` names it second. A call whose type
// arguments are a type variable, a primitive or a DI interface names no
// destination and is not messaging — which is most of what a .NET codebase
// writes between angle brackets.
func genericTopicContract(name string, typeArgs []string) (kind, topic, handler string, ok bool) {
	kind, ok = genericTopicVerbs[strings.ToLower(name)]
	if !ok || len(typeArgs) == 0 {
		return "", "", "", false
	}
	for _, arg := range typeArgs {
		if topic == "" {
			if t := eventTopicName(arg); t != "" {
				topic = t
				continue
			}
		}
		if handler == "" && hasAnySuffixFold(lastComponent(trimGenericArgs(arg)), genericHandlerSuffixes) {
			handler = lastComponent(trimGenericArgs(arg))
		}
	}
	if topic == "" {
		return "", "", "", false
	}
	return kind, topic, handler, true
}

func hasAnySuffixFold(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if len(s) > len(suffix) && hasSuffixFold(s, suffix) {
			return true
		}
	}
	return false
}
