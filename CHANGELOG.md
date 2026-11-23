# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-11-22

### Added

- `pixapi`: typed client for the API Pix: `cob` (PUT/POST/GET/PATCH, list),
  `cobv` (PUT/GET/PATCH, list), `lotecobv` (PUT/PATCH/GET, list), `pix`
  (GET, list, devolução PUT/GET), `webhook` (PUT/GET/DELETE, list) and `loc`
  (POST/GET, unlink, list).
- mTLS through `WithTLSConfig` / `LoadClientTLS`; OAuth2 client credentials
  with token caching, early background refresh and single-flight fetches;
  automatic re-authentication on 401.
- Retries with jittered exponential backoff for GET/PUT/DELETE only,
  honoring `Retry-After`; per-attempt timeouts; one request ID per call.
- Local validation (txid, CPF/CNPJ, Pix keys via brpay, amounts, lengths)
  reported as `violacoes`, and RFC 7807 problem mapping to `*pixapi.Error`
  with `errors.Is` sentinels.
- `Amount`: int64 centavos with strict decimal-string JSON encoding.
- `Iterator[T]` over paginated list endpoints.
- `webhook`: mTLS-verifying receiver with deduplication by EndToEndId and
  refund state, plus a hardened `http.Server` helper.
- `mockpsp` and `cmd/mockpsp`: in-memory reference server with token
  endpoint, certificate-bound tokens, development PKI generation,
  `pixCopiaECola` generation, `/simulate/pay/{txid}` with webhook delivery
  over mTLS and retries, refunds and batches.
- `pixapitest`: reusable conformance suite, run against mockpsp in CI and
  optionally against a PSP sandbox.
- `examples/merchant`: end-to-end merchant flow.

[0.1.0]: https://github.com/victorcondackdev/pix-cobranca-go/releases/tag/v0.1.0
