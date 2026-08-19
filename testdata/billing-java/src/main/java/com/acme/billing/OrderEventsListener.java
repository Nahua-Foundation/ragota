package com.acme.billing;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;

// OrderEventsListener consumes order events and charges customers.
@Service
public class OrderEventsListener {

    @Autowired
    private RestTemplate restTemplate;

    @Autowired
    private BillingService billingService;

    // onOrderCreated handles the orders.created Kafka event.
    @KafkaListener(topics = "orders.created", groupId = "billing")
    public void onOrderCreated(String message) {
        OrderCreatedEvent event = OrderCreatedEvent.parse(message);
        chargeUser(event.getUserId(), event.getAmount());
    }

    // chargeUser bills the customer and notifies them about the invoice.
    private void chargeUser(String userId, double amount) {
        Invoice invoice = billingService.charge(userId, amount);
        restTemplate.postForObject(
            "http://notifier:5000/api/notify/send",
            new NotifyRequest(userId, invoice.getId()),
            Void.class);
    }
}
