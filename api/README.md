# api

HTTP sözleşmesi: OpenAPI 3.1, tek doğruluk kaynağı (ADR-0005, ADR-0006). Kurallar: [`docs/api-style-guide.md`](../docs/api-style-guide.md).

| Dosya | Ne |
|---|---|
| `openapi.yaml` | Kök spec: yollar, güvenlik şemaları, ortak hata yanıtları |
| `components/problem.yaml` | RFC 9457 `Problem` ve `ErrorCode` kod listesi |
| `components/money.yaml` | `Money` (`int64` kuruş + para birimi), `BasisPoints` |
| `components/pagination.yaml` | Cursor sayfa zarfı ve `limit`/`cursor`/`includeTotal`/`sort` parametreleri |
| `components/datetime.yaml` | `Timestamp`, `LocalDateTime`, `TimeZone`, `Date`, `Id` |
| `components/headers.yaml` | `Idempotency-Key`, platform başlıkları, yanıt başlıkları |
| `ruleset.yaml`, `ruleset-schemas.yaml` | vacuum kural seti (`make api-lint`) |
| `testdata/lint-probe.yaml` | Her kuralı bir kez çiğneyen prob (`scripts/api-lint-probe.sh`) |
| `spec.go` | Spec'i ikiliye gömer; `api.Load` istek doğrulaması için yükler |

Sunucu arayüzü `make generate` ile `internal/apiv1`'e üretilir; elle yazılmış handler imzası tutulmaz. Yeni bir bileşen dosyası eklenirse `internal/apiv1/generate.go`'ya bir `go:generate` satırı ve `internal/apiv1/oapi-codegen.yaml`'ın `import-mapping`'ine bir satır eklenir.
