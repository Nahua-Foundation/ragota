package com.acme.billing;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

// BillingController exposes invoice lookup endpoints.
@RestController
@RequestMapping("/api/billing")
public class BillingController {

    @Autowired
    private BillingService billingService;

    // getInvoice returns an invoice by id.
    @GetMapping("/invoices/{id}")
    public Invoice getInvoice(@PathVariable String id) {
        return billingService.charge(id, 0);
    }
}
