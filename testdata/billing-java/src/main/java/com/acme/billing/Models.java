package com.acme.billing;

// OrderCreatedEvent mirrors the orders.created Kafka payload.
class OrderCreatedEvent {
    private String userId;
    private double amount;

    public static OrderCreatedEvent parse(String json) {
        return new OrderCreatedEvent();
    }

    public String getUserId() {
        return userId;
    }

    public double getAmount() {
        return amount;
    }
}

// Invoice is a created invoice.
class Invoice {
    private final String userId;
    private final double amount;

    Invoice(String userId, double amount) {
        this.userId = userId;
        this.amount = amount;
    }

    public String getId() {
        return "inv-" + userId;
    }
}

// NotifyRequest is the payload sent to the notifier service.
class NotifyRequest {
    public String userId;
    public String invoiceId;

    NotifyRequest(String userId, String invoiceId) {
        this.userId = userId;
        this.invoiceId = invoiceId;
    }
}
