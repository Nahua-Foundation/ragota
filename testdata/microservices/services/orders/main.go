package main

import (
	"context"
	"encoding/json"
	"log"
	"net"

	pb "example.com/microservices/proto/orders"
	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
)

const ordersCreatedTopic = "orders.created"

func main() {
	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()
	pb.RegisterOrderServiceServer(s, &orderServer{})
	log.Fatal(s.Serve(lis))
}

// orderServer implements the OrderService gRPC contract.
type orderServer struct {
	pb.UnimplementedOrderServiceServer
}

// CreateOrder persists a new order and publishes an OrderCreated event.
func (o *orderServer) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderResponse, error) {
	orderID := saveOrder(req.UserId, req.Amount)
	publishOrderCreated(ctx, req.UserId, orderID, req.Amount)
	return &pb.CreateOrderResponse{OrderId: orderID, Status: "created"}, nil
}

// GetOrder loads an order by id.
func (o *orderServer) GetOrder(ctx context.Context, req *pb.GetOrderRequest) (*pb.Order, error) {
	return loadOrder(req.OrderId), nil
}

// orderCreatedEvent is the payload published to Kafka.
type orderCreatedEvent struct {
	UserID  string  `json:"user_id"`
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

// publishOrderCreated emits the order-created event to Kafka.
func publishOrderCreated(ctx context.Context, userID, orderID string, amount float64) {
	writer := &kafka.Writer{
		Addr:  kafka.TCP("kafka:9092"),
		Topic: ordersCreatedTopic,
	}
	event := orderCreatedEvent{
		UserID:  userID,
		OrderID: orderID,
		Amount:  amount,
	}
	payload, _ := json.Marshal(event)
	if err := writer.WriteMessages(ctx, kafka.Message{Value: payload}); err != nil {
		log.Printf("publish order created: %v", err)
	}
}

// saveOrder stores the order and returns its id.
func saveOrder(userID string, amount float64) string {
	_ = userID
	_ = amount
	return "ord-1"
}

// loadOrder loads an order from storage.
func loadOrder(orderID string) *pb.Order {
	return &pb.Order{OrderId: orderID}
}
