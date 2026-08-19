package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	pb "example.com/microservices/proto/orders"
	"google.golang.org/grpc"
)

var ordersConn *grpc.ClientConn

func main() {
	http.HandleFunc("POST /api/v1/orders", CreateOrderHandler)
	http.HandleFunc("GET /api/v1/orders/{id}", GetOrderHandler)
	http.HandleFunc("GET /health", HealthHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// createOrderBody is the JSON body of POST /api/v1/orders.
type createOrderBody struct {
	UserID string  `json:"user_id"`
	Amount float64 `json:"amount"`
}

// CreateOrderHandler accepts an order creation request and forwards it
// to the orders service over gRPC.
func CreateOrderHandler(w http.ResponseWriter, r *http.Request) {
	var body createOrderBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := createOrder(r.Context(), body.UserID, body.Amount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// createOrder calls the orders service via the generated gRPC client.
func createOrder(ctx context.Context, userID string, amount float64) (*pb.CreateOrderResponse, error) {
	client := pb.NewOrderServiceClient(ordersConn)
	return client.CreateOrder(ctx, &pb.CreateOrderRequest{
		UserId: userID,
		Amount: amount,
	})
}

// GetOrderHandler proxies order lookup to the orders app.
func GetOrderHandler(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	client := pb.NewOrderServiceClient(ordersConn)
	order, err := client.GetOrder(r.Context(), &pb.GetOrderRequest{OrderId: orderID})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(order)
}

// HealthHandler reports liveness.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
