// Package pixapi is a typed client for the "API Pix", the REST API that
// Brazilian payment service providers (PSPs) expose to merchants under the
// Banco Central do Brasil standard, for charges and received payments.
//
// It covers:
//
//   - cob: immediate charges (PUT/POST/GET/PATCH /cob, GET /cob list);
//   - cobv: charges with due date, fines, interest and discounts;
//   - lotecobv: batches of charges with due date;
//   - pix: received payments and refunds (devoluções);
//   - webhook: registration of the URL the PSP notifies;
//   - loc: payload locations served to the payer's app.
//
// # Authentication
//
// The API Pix requires both mutual TLS and OAuth2. Give NewClient the
// client certificate through WithTLSConfig (see LoadClientTLS) and the
// client credentials through WithOAuth2; the token endpoint is called over
// the same mTLS connection. Tokens are cached, refreshed before they expire
// and fetched once for any number of concurrent callers.
//
// # Validation
//
// Requests are validated before being sent: txid format, CPF and CNPJ
// check digits and Pix keys (through github.com/victorcondackdev/brpay),
// amounts, field lengths. A *ValidationError lists every violation found,
// using the same vocabulary ("violacoes") as the PSP's problem documents,
// so a request rejected locally looks like one rejected remotely.
//
// # Money
//
// Amounts are Amount values, an int64 number of centavos that marshals to
// the API's decimal strings ("123.45"). No float is ever involved.
//
// # Errors
//
// Non-2xx responses become *Error, which embeds the RFC 7807 Problem and
// matches the Err* sentinels with errors.Is (ErrNotFound, ErrConflict...).
//
// # Retries
//
// Idempotent calls (GET, PUT, DELETE) are retried on transport errors and
// on 429/502/503/504 with jittered exponential backoff; POST and PATCH are
// never retried. Every logical call carries one request ID across retries.
//
// The model follows the public API Pix specification. Where a field, limit
// or problem type is uncertain the documentation says so: verify against
// the current BCB spec version and your PSP's documentation.
package pixapi
