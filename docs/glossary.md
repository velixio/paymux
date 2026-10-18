# Paymux

Language for integrating and operating a payment hub. These terms distinguish the customer payment, the provider's processing, and the delivery of information about it.

## Ownership

**Organization**:
The administrative boundary that owns applications, provider accounts, users, and shared policies.
_Avoid_: Workspace when referring to ownership.

**Application**:
A product integrated with the payment hub, with its own payment records, API keys, and webhook endpoints.
_Avoid_: Provider app, dashboard user.

**Provider account**:
One configured payment-provider relationship, belonging to an organization and used by eligible applications.
_Avoid_: Account without identifying whether it means a provider account or dashboard user.

**Connector**:
The integration that translates between Paymux's payment concepts and a particular provider's capabilities.
_Avoid_: Provider account; several accounts may use the same connector.

**Mode**:
The test or live boundary for a payment operation and the credentials that authorize it.
_Avoid_: Environment when referring to test versus live payments.

## Money and billing

**Payment**:
The application's request to collect a particular amount in a particular currency.
_Avoid_: Attempt, provider transaction.

**Attempt**:
One effort to collect a payment through a provider account; a payment may have multiple attempts.
_Avoid_: Payment when referring to an individual provider interaction.

**Refund**:
A return of some or all captured funds associated with an attempt.
_Avoid_: Cancellation; canceling an unpaid payment does not return funds.

**Dispute**:
A challenge to a payment through the provider's dispute or chargeback process.
_Avoid_: Refund request.

**Subscription**:
A recurring billing agreement, collected by the provider or through a customer-authorized mandate.
_Avoid_: Mandate; authorization to debit is distinct from the recurring agreement.

**Mandate**:
The customer's authorization for charges under an agreed limit and payment method.
_Avoid_: Subscription.

**Invoice**:
An issued tax document recording a supply and its amounts, with the supplier and recipient information at issue time.
_Avoid_: Payment link; an invoice is not a new request to collect funds.

**Credit note**:
A corrective document associated with an issued invoice.
_Avoid_: Edited invoice.

**Payout**:
A transfer out through an eligible provider account to a beneficiary.
_Avoid_: Refund, which returns a customer's captured funds.

## Events and operations

**Provider webhook**:
A notification sent by a payment provider to Paymux about a provider-side change.
_Avoid_: App webhook when describing this direction.

**App webhook**:
An event Paymux sends to an application's registered endpoint.
_Avoid_: Provider webhook.

**Delivery**:
The sending of one event to one application endpoint, including retries.
_Avoid_: Payment status.

**Reconciliation**:
Comparison of the hub's payment records with provider-side records to find and resolve differences.
_Avoid_: Webhook replay, which resends events to applications.
