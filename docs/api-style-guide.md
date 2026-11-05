# API stil rehberi

Koala HTTP API'sinin kuralları. Sözleşme `api/openapi.yaml` ve `api/components/*.yaml`'dadır (ADR-0005). Bu rehber o dosyaları yazarken uyulacak kuralları toplar.

**Etiketler.** **[lint: kural-adı]** ile işaretli kural `make api-lint`'te (vacuum, `api/ruleset.yaml` ve `api/ruleset-schemas.yaml`) zorlanır. **[test]** bir Go testiyle zorlanır. Etiketsiz kural incelemede bakılır. Her kuralın dayanağı parantez içinde verilir: plan (`docs/plan/go-gecis-degerlendirme.md`), ADR ya da karar kaydı (K*n*).

**Sektör kaynakları.** Kurallar çoğunlukla şu rehberlerle örtüşür; ayrıldığımız yer ilgili maddede yazılı:
- Google API Improvement Proposals (AIP): kaynak adları (AIP-122), standart ve özel metotlar (AIP-131–136), sayfalama (AIP-158), hatalar (AIP-193), geriye uyumluluk (AIP-180).
- Microsoft REST API Guidelines: sürümleme, hata gövdesi, kullanımdan kaldırma.
- Zalando RESTful API Guidelines: kebab-case yol, sayfalama, `x-extensible-enum` tartışması.
- Stripe API: Idempotency-Key, en küçük birimde para, durum geçişi için alt eylem.
- IETF: RFC 9110 (HTTP semantiği), RFC 9457 (problem details), RFC 3339 (zaman), RFC 6901 (JSON pointer), RFC 9745 (`Deprecation`), RFC 8594 (`Sunset`), `draft-ietf-httpapi-idempotency-key-header`.

## 1. Adlandırma

- Kaynak adları **çoğul isimdir** ve yolda **kebab-case** yazılır: `/v1/job-assignments/{assignmentId}` (ADR-0005 madde 2, plan §4.1-2; AIP-122, Zalando) **[lint: koala-paths-kebab-case]**.
- Yol parametreleri camelCase'dir: `{jobId}`. Sonda eğik çizgi yoktur **[lint: koala-paths-kebab-case]**.
- Kendi verin `/v1/me/...` altındadır: `/v1/me/job-applications` (ADR-0005 madde 2).
- JSON alan adları **camelCase**'dir (ADR-0005 madde 2; Google ve Microsoft ile aynı, Zalando'nun snake_case'inden ayrılır) **[lint: koala-properties-camel-case]**.
- Enum değerleri **UPPER_SNAKE**'tir: `IN_TRANSIT` (ADR-0005 madde 2; istemci değişikliği azalsın diye bugünkü biçim) **[lint: koala-enum-upper-snake]**.
- `operationId` camelCase bir fiil + isimdir: `listJobs`, `getJob`, `completeJobAssignment`. Üretilen Go metodunun adı budur (ADR-0005 madde 1) **[lint: koala-operation-id-camel-case]**.
- Şema adları PascalCase'dir: `JobAssignment`, `JobPage`, `CreateJobRequest`.
- Kısaltmalar alan adında kelime gibi yazılır: `iban`, `tckn`, `userId` (`userID` değil).

## 2. Sürümleme

- Taban yol `/v1`'dir; her yol onunla başlar (ADR-0005 madde 2) **[lint: koala-paths-v1]**. Sağlık uçları (`/livez`, `/readyz`) sözleşmede değildir.
- `/v1` içinde yalnız **geriye uyumlu** değişiklik yapılır (AIP-180):
  - Uyumlu: yeni operasyon, yanıta yeni alan, isteğe yeni **isteğe bağlı** alan, yeni hata kodu, yeni enum değeri.
  - Uyumsuz: alan silmek ya da yeniden adlandırmak, tipini ya da anlamını değiştirmek, isteğe zorunlu alan eklemek, durum kodunu değiştirmek.
- İstemciler bilmedikleri yanıt alanını yok sayar; bilmedikleri enum değerini ve hata kodunu "bilinmeyen" olarak ele alır. Bu yüzden yanıt şemalarında `additionalProperties: false` kullanılmaz.
- Uyumsuz değişiklik önce kullanımdan kaldırma (§12) ile yapılır. `/v2` ancak sözleşmenin büyük bölümü değişirse açılır; o gün ayrı bir ADR yazılır.
- `info.version` sözleşmenin sürümüdür (semver), yol sürümünden bağımsızdır.

## 3. Metot semantiği

RFC 9110'a göre:

| Metot | Kullanım | Başarı |
|---|---|---|
| `GET` | Okuma; yan etkisi yok, gövdesi yok. | 200 |
| `POST /koleksiyon` | Oluşturma. | 201 + `Location` + oluşan kaynak |
| `POST /kaynak/{id}/<eylem>` | Durum geçişi ve CRUD'a sığmayan eylem: `…/complete`, `…/cancel` (ADR-0005 madde 2, plan §4.1-2; Stripe `…/confirm` kalıbı). Google'ın `:verb` biçimi yerine alt yol kullanılır. | 200 + güncel kaynak; gövde yoksa 204 |
| `PATCH` | Kısmi güncelleme; gönderilmeyen alan değişmez. | 200 + güncel kaynak |
| `PUT` | Yalnız istemcinin tamamını bildiği alt kaynakta tam yer değiştirme (ör. tercihler). | 200 |
| `DELETE` | Silme (soft-delete, ADR-0013). | 204 |

- Durum alanı (`status`) `PATCH` ile değiştirilmez; geçiş her zaman alt eylemdir. FSM `domain.go`'da tablo olarak durur (CLAUDE.md).
- Uzun iş isteğe bağlanmaz: 202 + iş kaynağı döner; sonuç iş kaynağından okunur (CLAUDE.md "Transaction").

## 4. İstek gövdesi

- Gövde `application/json`'dur. Dosya yükleme `multipart/form-data`'dır.
- JSON istek şeması **bilinmeyen alanı reddeder**: `additionalProperties: false` (plan §3.3 "bilinmeyen alan reddi istekte açık") **[lint: koala-closed-request-body]**. Biçim doğrulaması spec'ten kin-openapi ile yapılır; Go'da etiketle ikinci kopya tutulmaz (ADR-0006 madde 2). Bilinmeyen alan 400 `REQUEST_INVALID` + `FIELD_UNKNOWN` döner **[test]**.
- Doğrulayıcı gövdeyi bir kez çözer; handler aynı değeri bağlar. `encoding/json`'ın büyük-küçük harf duyarsız eşlemesi (`"Note"` → `Note`) bu yüzden devreye girmez **[test: internal/app TestPing]**.
- Gövde varsayılan 1 MiB ile sınırlıdır; aşımı 413 `REQUEST_BODY_TOO_LARGE`'dır (CLAUDE.md "Hatalar").

## 5. Yanıtlar ve durum kodları

- **Zarf yoktur.** Başarılı yanıt çıplak kaynaktır ya da sayfa nesnesidir; `success`, `message`, `httpStatus` yoktur (ADR-0005 madde 3) **[lint: koala-no-envelope]**.
- **Hata yanıtı RFC 9457 problem belgesidir** (`application/problem+json`, `api/components/problem.yaml#/components/schemas/Problem`). Her 4xx, 5xx ve `default` yanıtı yalnız onu döner (ADR-0007) **[lint: koala-error-responses-are-problems]**. Gövde `httpx.Problem` ile birebir aynıdır **[test: httpx TestWriteErrorGoldenBody]**.
- Her operasyon `default` yanıtını tanımlar **[lint: koala-default-problem-response]**. Ortak hata yanıtları kök spec'in `components/responses`'ındadır (`BadRequest`, `NotFound`, …).
- Durum kodları:

| Kod | Ne zaman |
|---|---|
| 200 | Okuma, güncelleme, gövdeli eylem |
| 201 | Oluşturma; `Location` başlığı yeni kaynağı gösterir |
| 202 | Kabul edildi, iş arka planda (River) |
| 204 | Gövdesiz başarı (`DELETE`, gövdesiz eylem) |
| 400 | Sözleşmeye uymayan istek (`REQUEST_INVALID`, `REQUEST_BODY_MALFORMED`, `SORT_FIELD_NOT_ALLOWED`, `CURSOR_INVALID`, `IDEMPOTENCY_KEY_REQUIRED`) ya da alan kuralı ihlali (alanın kendi kodu) |
| 401 | Kimlik yok ya da geçersiz |
| 403 | Kimlik var, eylem yasak. **Okuma reddi 403 değil, kaynağın 404'üdür**; gövde bayt bayt aynıdır (ADR-0014) |
| 404 | Kaynak yok ya da görme yetkisi yok; `ROUTE_NOT_FOUND` yalnız bilinmeyen yol |
| 405 | Yol var, metot yok; `Allow` başlığı döner |
| 409 | Kaynağın durumu isteğe izin vermiyor; aynı Idempotency-Key'li istek sürüyor |
| 413 | Gövde sınırı aşıldı |
| 422 | **Yalnız** aynı Idempotency-Key farklı gövdeyle (ADR-0005 madde 6). Alan doğrulaması 422 değil, 400'dür |
| 426 | `X-App-Version` desteklenmiyor; `X-Minimum-App-Version` döner (ADR-0005 madde 7) |
| 429 | Oran sınırı; `Retry-After` döner |
| 500 | Beklenmeyen hata (`INTERNAL`); sebep yalnız log'da |
| 502 | Üst sistem (Iyzico, FCM…) hatası |
| 503 | Sunucu kapanıyor ya da hazır değil |

## 6. Sayfalama, süzme ve sıralama

- **Sayfalama her yerde cursor'ladır** (K10, ADR-0005 madde 4; AIP-158, Stripe, GitHub, Slack). Sayı ya da offset tabanlı sayfa yoktur **[lint: koala-cursor-pagination]**.
  - İstek: `?limit=&cursor=` (`pagination.yaml#/components/parameters/Limit`, `Cursor`). `limit` 1–100, varsayılan 20.
  - Yanıt: `{items, nextCursor, prevCursor}`; liste şeması `allOf` ile `CursorPage`'i alır ve `items`'ı ekler. `nextCursor: null` son sayfadır.
  - Cursor opaktır: istemci onu çözmez, kurmaz, saklamaz. Cursor sıralama anahtarını ve süzgeçleri kodlar; bozuk ya da başka bir sorguya ait cursor 400 `CURSOR_INVALID`'dir.
- **`total` yalnız admin listelerinde ve SEO'lu aramada** sunulur: o operasyon `IncludeTotal` parametresini ve `CountedCursorPage`'i kullanır; SEO'lu arama `x-seo-search: true` taşır (ADR-0005 madde 4, plan §4.1-4) **[lint: koala-include-total-admin-or-seo]**.
- **Sıralama** `?sort=-startsAt,id` biçimindedir: virgülle ayrılmış alanlar, `-` azalan sıra. Operasyon kabul ettiği alanları `x-sort-fields` ile listeler; dışındaki alan 400 `SORT_FIELD_NOT_ALLOWED`'dır (ADR-0005 madde 4) **[lint: koala-sort-fields]**. Sıralama her zaman tekil bir alanla (`id`) biter ki cursor kararlı olsun.
- **Süzme** alanın adını taşıyan sade query parametresidir: `?status=OPEN&status=FILLED` (dizi, `form` + `explode`). Aralıklar `<alan>From` (dahil) ve `<alan>To` (hariç) çiftidir: `?startsAtFrom=…&startsAtTo=…`. Serbest süzgeç dili (OData `$filter`, AIP-160) kullanılmaz.

## 7. Idempotency

- Para ya da yan etki doğuran her `POST` **`Idempotency-Key` başlığını zorunlu** alır: ödeme oturumu, iade, çıkış (personel), başvuru, kayıt, şikayet (ADR-0005 madde 6, plan §4.1-6; Stripe; IETF taslağı). Parametre `headers.yaml#/components/parameters/IdempotencyKey`'dir.
- Yan etkisi olmayan `POST` bunu `x-no-idempotency-key: <gerekçe>` ile açıkça yazar **[lint: koala-idempotency-key]**.
- İstemci her mantıksal işlem için yeni bir UUID üretir ve yeniden denemede aynısını gönderir.
- Aynı anahtar farklı gövdeyle gelirse 422 `IDEMPOTENCY_KEY_REUSED`, ilk istek sürerken gelirse 409 `IDEMPOTENCY_KEY_IN_USE` döner. Anahtar aktör ve operasyonla saklanır (`idempotency_keys`, plan §4.1-6). Uygulaması Faz 1'dedir.

## 8. Hata kodları

- Kod **UPPER_SNAKE**'tir, **anlamı hiç değişmez** ve `problem.yaml#/components/schemas/ErrorCode` listesindedir. Servis yeni bir kod döndürmeden önce kod oraya eklenir (CLAUDE.md "Hatalar", ADR-0007 madde 5). İstemcilerin kod listesi buradan üretilir.
- HTTP platformunun döndürdüğü her kod listededir **[test: internal/app TestPlatformCodesAreInTheContract]**.
- Ad, durumu değil sebebi söyler: `PAYMENT_ATTEMPT_ACTIVE`, `IBAN_INVALID`. Önek alan adıdır (`PAYMENT_`, `JOB_`), platform kodları öneksizdir. HTTP durum adı tekrar edilmez (`CONFLICT_…` yok).
- Alan hataları `errors[]`'ta `{pointer, code}` olarak gelir. `pointer` gövdede RFC 6901 JSON pointer'dır (`/amount/currency`, gövdenin tamamı için `""`); parametrede `<in>:<ad>`'dır (`query:limit`, `header:Idempotency-Key`). Biçim hatalarının kodları `FIELD_*`'dır; alan kuralının ihlali kendi kodunu taşır (`IBAN_INVALID`).
- `detail` yoktur; kullanıcı metni istemcidedir (ADR-0005 madde 3). Doğrulayıcının iletileri istek değerleri içerebilir; hiçbiri yanıta ya da log'a yazılmaz **[test: httpx TestValidateRequestsNeverEchoesValues]**.
- Bir enum'u kapalı tutmak (`enum`) Zalando'nun `x-extensible-enum` önerisinden ayrılır: listeye yeni değer eklemek uyumlu sayılır (§2) ve istemci bilinmeyen değeri genel hata olarak gösterir.

## 9. Veri tipleri

- **Para** `Money`'dir: `{"amount": 15000, "currency": "TRY"}`. `amount` en küçük birimde (kuruş) `int64`'tür (K5, ADR-0008; Stripe, Adyen). Para alanı `number` ya da string ondalık olamaz **[lint: koala-money-not-decimal]**. `"150.00"` biçimi yalnız Iyzico sınırında üretilir.
- **Oran** baz puandır (`BasisPoints`, 1250 = %12,5) ve alanın adı `Bps` ile biter: `commissionBps` (K5). `…Rate`, `…Percent`, `…Ratio` adı kullanılmaz **[lint: koala-rate-in-basis-points, koala-bps-integer]**.
- **Anlık zaman** RFC 3339 `date-time`'dır; sunucu UTC ve `Z` yazar (`Timestamp`). Alan adı `At` ile biter: `createdAt` (K11, ADR-0005 madde 5) **[lint: koala-instant-is-date-time]**.
- **Yerel anlamı olan zaman** (ilan başlangıcı) ofsetli yazılır ve saat dilimi aynı nesnede `timeZone` alanındadır: `{"startsAt": "2026-08-27T09:30:00+03:00", "timeZone": "Europe/Istanbul"}` (`LocalDateTime`, K11, plan §3.8).
- **Takvim günü** `format: date`'tir (`Date`) ve adı `On` ya da `Date` ile biter: `birthDate` **[lint: koala-calendar-date-is-date]**.
- **Kimlik** `format: uuid`'dir (UUIDv7, plan §3.6-4; `Id`). Biçimi doğrulanır **[test]**.
- **Tam sayı** `format: int32` ya da `int64` yazar **[lint: koala-integer-format]**. Koordinat gibi gerçek ondalıklar `number` + `format: double`'dır.
- **Null** OpenAPI 3.1 biçiminde yazılır: `type: [string, "null"]`; `$ref`'li bir şema için `oneOf: [{$ref: …}, {type: "null"}]`. 3.0'ın `nullable` anahtarı kullanılmaz **[lint: koala-no-nullable-keyword]**. Yanıtta "değer yok" null ile yazılır, alan atlanmaz; zorunlu + nullable alan Go'da omitempty'siz pointer olur.
- **Enum** `enum` ile yazılır. 3.1'in `oneOf` + `const` kalıbı oapi-codegen'de enum sabiti değil düz string üretir (ADR-0006 "Spike sonucu"), kullanılmaz.

## 10. Başlıklar

- Platform başlıkları middleware'de zorlanır ve operasyonlara tek tek yazılmaz; tanımları `headers.yaml`'dadır (ADR-0005 madde 7):
  - `X-Request-Id`: isteğe bağlı gelir, her yanıtta döner.
  - `X-Device-Id`: `/v1/admin` dışındaki her istekte zorunlu (Faz 1).
  - `X-App-Version`: semver; desteklenmeyen sürüm 426 + `X-Minimum-App-Version` (Faz 1). Kapı `/v1` dışında da çalışır.
  - `X-Client-Platform`: `IOS`, `ANDROID`, `WEB`, `ADMIN`.
- Operasyona özgü başlık (`Idempotency-Key`) operasyonun `parameters`'ında referanslanır.
- `Accept-Language` ile mesaj çözümü yoktur (ADR-0005 madde 7).

## 11. Yetki uzantıları

- Her operasyon ya `x-authz` taşır ya da `security: []` ile açıkça public'tir; ikisi birden olmaz (ADR-0005 madde 1, plan §4.1-1) **[lint: koala-operation-authz]**.
  - `x-authz: {action: read, object: job}`. `action` ve `object` authz modelindeki (ADR-0014) eylem ve nesne tipidir, snake_case.
  - Varsayılan güvenlik `marketplaceBearer`'dır (kök `security`).
- `/v1/admin` altındaki her operasyon `security: [{adminBearer: []}]` ve `x-authz-permission: payout.release` taşır. Eşleme eksikse sunucu açılmaz (Faz 1) **[lint: koala-admin-permission]**. `adminBearer` ve `x-authz-permission` başka yerde kullanılmaz **[lint: koala-admin-only-under-admin]**.

## 12. Kullanımdan kaldırma

- Kaldırılacak operasyon ya da alan `deprecated: true` olur; operasyon kaldırılma gününü `x-sunset: YYYY-MM-DD` ile yazar **[lint: koala-deprecation-has-sunset]**.
- Sunucu kaldırılmış operasyonun yanıtına `Deprecation` (RFC 9745) ve `Sunset` (RFC 8594) başlıklarını ekler; tanımları `headers.yaml`'dadır.
- Kaldırma süresi en az bir zorunlu mobil güncelleme döngüsüdür: `X-Minimum-App-Version` yeni yolu kullanan sürüme çıkmadan eski yol silinmez. Yayımlanmamış (henüz istemcisi olmayan) operasyon doğrudan değiştirilebilir.

## 13. Akış ve araçlar

- Akış: spec PR'ı → `make api-lint` → `make generate` → uygulama (ADR-0005 madde 1).
- `make api-lint`: vacuum ile kök spec (`api/ruleset.yaml`: Spectral'ın önerilen seti + Koala kuralları) ve her bileşen dosyası (`api/ruleset-schemas.yaml`). Ardından `scripts/api-lint-probe.sh` her Koala kuralının `api/testdata/lint-probe.yaml`'da gerçekten tetiklendiğini kanıtlar. Hata kapıyı kırmızı yapar; uyarılar yalnız raporlanır.
- `make generate`: `internal/apiv1`'i oapi-codegen ile üretir. Üretilen kod commit edilir; `make gen-check` spec'ten yeniden üretilenle aynı olduğunu denetler. İkisi `make verify`'ın parçasıdır.
- Yeni bir bileşen dosyası `api/components/`'a eklenirse `internal/apiv1/generate.go`'ya bir `go:generate` satırı ve `oapi-codegen.yaml`'ın `import-mapping`'ine bir satır eklenir.
