# ADR-0007: Hata modeli: `apperr` + RFC 9457

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Kaynak: plan §3.4, §4.1-3, §5.1; N05 A3, A5-3

## Bağlam

core-backend'de 14 pakette aynı sentinel hata kümesi tekrarlanıyor ve 16 ayrı handler hata eşleyicisi var (N05 A5-3).

Java'da `IllegalStateException → 409` gibi örtük eşlemeler var. Bunlar bir hatanın hangi koda dönüştüğünü belirsizleştiriyor (§3.4).

İstemcilerin hata kodu listesi bugün Java kaynağı taranarak üretiliyor (§3.4).

## Karar

1. **Tek `apperr` paketi** (`internal/platform/apperr`). Bir hatanın parçaları:
   - `Kind`: {Internal, Invalid, Unauthenticated, Forbidden, NotFound, Conflict, RateLimited, Upstream},
   - stabil UPPER_SNAKE `Code`,
   - isteğe bağlı alan hataları (`pointer` + `code`).

   Sıfır `Kind` `Internal`'dır; sınıflandırılmamış bir hata asla 4xx olmaz.
2. **Tek eşleyici** (`httpx.WriteError`, `httpx.StatusOf`). RFC 9457 `application/problem+json` üretir. Gövdenin alanları: `type`, `title`, `status`, `code`, `instance`, `requestId`, `errors`. `type` değeri `https://api.iskoala.com/problems/<code-kebab>` biçimindedir. `detail` alanı yoktur; metin istemcidedir.
3. **Örtük eşleme yoktur.**
   - `*apperr.Error` olmayan her hata `500 INTERNAL` döner.
   - `Kind=Internal` ya da boş `Code`, istemciye her zaman `INTERNAL` olarak gider.
   - Sebep (`cause`) yalnız log'a yazılır. 5xx'ler `request_id` ile loglanır, 4xx'ler loglanmaz (erişim log'u zaten durumu yazar).
4. **Var olmayı gizleme** authz kararının çıktısıdır. Okuma reddi, kaynağın kendi 404 gövdesiyle bayt bayt aynı döner (§3.4, ADR-0014).
5. **Kod kaydı:** Kodlar OpenAPI'de enum ya da `x-error-codes` olarak listelenir. İstemcilerin listesi buradan üretilir.

## Sonuçlar

- Problem gövdesi için altın test var (`httpx/problem_test.go`, `TestWriteErrorGoldenBody`).
- Bilinmeyen route `404 ROUTE_NOT_FOUND`, yanlış metot `405 METHOD_NOT_ALLOWED` döner (+ `Allow`). Panic `500 INTERNAL` döner ve stack log'a gider.
- 422 (Idempotency-Key gövde uyuşmazlığı, ADR-0005) ve 426 (minimum uygulama sürümü) için yeni `Kind` gerekecek. Bunlar ilgili PR'da eşlemesi ve testiyle eklenir.
- Kısıt ihlallerini koda çevirmek için tek yer kullanılır: `pgconn.PgError.ConstraintName` → `Code` eşlemesi Faz 1'de `platform/db`'de yazılır (§6.3).
