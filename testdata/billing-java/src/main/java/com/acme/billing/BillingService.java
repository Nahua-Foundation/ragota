package com.acme.billing;

import org.springframework.stereotype.Service;

// BillingService charges customers for their orders.
@Service
public class BillingService {

    // charge creates an invoice for the given user and amount.
    public Invoice charge(String userId, double amount) {
        Invoice invoice = new Invoice(userId, amount);
        persist(invoice);
        return invoice;
    }

    // persist stores the invoice.
    private void persist(Invoice invoice) {
        // omitted: database write
    }
}
