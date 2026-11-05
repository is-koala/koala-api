# API stil rehberi

Bu rehber Koala HTTP API'sinin kurallarını toplar. Sözleşme `api/openapi.yaml` ve `api/components/*.yaml`'dadır (ADR-0005); dosyalar bu kurallara göre yazılır.

**Etiketler:**
- **[lint: kural-adı]**: kural `make api-lint`'te zorlanır (vacuum, `api/ruleset.yaml` ve `api/ruleset-schemas.yaml`).
- **[test]**: kuralı bir Go testi zorlar.
- Etiketsiz kural incelemede bakılır.
- Her kuralın dayanağı parantez içinde verilir: plan (`docs/plan/go-gecis-degerlendirme.md`), ADR ya da karar kaydı (K*n*).
- **(öneri)**: kuralın plan, ADR ya da karar kaydında dayanağı yoktur. Bu PR'da önerildi ve incelemeye açıktır.

**Sektör kaynakları.** Kurallar çoğunlukla aşağıdaki kaynaklarla örtüşür. Ayrıldığımız yer ilgili maddede yazılıdır.
- Google API Improvement Proposals (AIP):
  - kaynak adları (AIP-122),
  - standart ve özel metotlar (AIP-131–136),
  - sayfalama (AIP-158),
  - hatalar (AIP-193),
  - geriye uyumluluk (AIP-180).
- Microsoft REST API Guidelines: sürümleme, hata gövdesi (`target`), kullanımdan kaldırma.
- Zalando RESTful API Guidelines: kebab-case yol, sayfalama, `x-extensible-enum`.
- JSON:API: hata kaynağı (`source.pointer`, `source.parameter`).
- Stripe API: Idempotency-Key, en küçük birimde para, durum geçişi için alt eylem.
- IETF belgeleri:
  - RFC 9110 (HTTP semantiği),
  - RFC 9457 (problem details),
  - RFC 3339 (zaman),
  - RFC 6901 (JSON pointer),
  - RFC 7396 (JSON Merge Patch),
  - RFC 9745 (`Deprecation`),
  - RFC 8594 (`Sunset`),
  - `draft-ietf-httpapi-idempotency-key-header`.

## 1. Adlandırma

- **Yol:** kaynak adları **çoğul isimdir** ve **kebab-case** yazılır: `/v1/job-assignments/{assignmentId}` (ADR-0005 madde 2, plan §4.1-2; AIP-122, Zalando) **[lint: koala-paths-kebab-case]**.
  - Yol parametreleri camelCase'dir: `{jobId}`. Sonda eğik çizgi yoktur **[lint: koala-paths-kebab-case]**.
  - Kendi verin `/v1/me/...` altındadır: `/v1/me/job-applications` (ADR-0005 madde 2).
- **JSON alan adları camelCase'dir** (ADR-0005 madde 2; Google ve Microsoft ile aynı, Zalando'nun snake_case'inden ayrılır) **[lint: koala-properties-camel-case]**.
- **Enum değerleri UPPER_SNAKE'tir:** `IN_TRANSIT` (ADR-0005 madde 2) **[lint: koala-enum-upper-snake]**. Kural `ProblemField.in` gibi teknik enum'lara da uygulanır: `QUERY`, `PATH`.
- **`operationId`** camelCase bir fiil + isimdir: `listJobs`, `getJob`, `completeJobAssignment` (ADR-0005 madde 1; fiil + isim biçimi öneri). Üretilen Go metodunun adı budur **[lint: koala-operation-id-camel-case]**.
- **Şema adları** PascalCase'dir: `JobAssignment`, `JobPage`, `CreateJobRequest` (öneri).
- **Kısaltmalar** alan adında kelime gibi yazılır: `iban`, `tckn`, `userId` (`userID` değil) (öneri).

## 2. Sürümleme

- Taban yol `/v1`'dir; her yol onunla başlar (ADR-0005 madde 2) **[lint: koala-paths-v1]**. Sağlık uçları (`/livez`, `/readyz`) sözleşmede değildir.
- `/v1` içinde yalnız **geriye uyumlu** değişiklik yapılır (AIP-180):
  - Uyumlu: yeni operasyon, yanıta yeni alan, isteğe yeni **isteğe bağlı** alan, yeni hata kodu, yeni enum değeri.
  - Uyumsuz: alan silmek ya da yeniden adlandırmak, tipini ya da anlamını değiştirmek, isteğe zorunlu alan eklemek, durum kodunu değiştirmek.
- İstemciler bilmedikleri yanıt alanını yok sayar. Bu yüzden **yanıt şemalarında `additionalProperties: false` kullanılmaz**; istekte kullanılır (§4). İstekte ve yanıtta ortak bir nesne gerekiyorsa istek için kapalı bir varyant yazılır: `Money` (yanıt) ve `MoneyInput` (istek) **[test: api TestMoneyVariantsMatch]**.
- Uyumsuz değişiklik önce kullanımdan kaldırma (§12) ile yapılır. `/v2` ancak sözleşmenin büyük bölümü değişirse açılır ve o gün ayrı bir ADR yazılır (öneri).
- `info.version` sözleşmenin sürümüdür (semver) ve yol sürümünden bağımsızdır (öneri).

## 3. Metot semantiği

Metotlar RFC 9110'a göre kullanılır. Tablo öneridir; dayanağı yalnız alt eylem satırıdır.

| Metot | Kullanım | Başarı |
|---|---|---|
| `GET` | Okuma; yan etkisi yok, gövdesi yok. | 200 |
| `POST /koleksiyon` | Oluşturma. | 201 + `Location` + oluşan kaynak |
| `POST /kaynak/{id}/<eylem>` | Durum geçişi ve CRUD'a sığmayan eylem: `…/complete`, `…/cancel` (ADR-0005 madde 2, plan §4.1-2; Stripe `…/confirm` kalıbı). Google'ın `:verb` biçimi yerine alt yol kullanılır. | 200 + güncel kaynak; gövde yoksa 204 |
| `PATCH` | Kısmi güncelleme, **JSON Merge Patch** (RFC 7396, `application/merge-patch+json`): gönderilmeyen alan değişmez, `null` alanı temizler. | 200 + güncel kaynak |
| `PUT` | Yalnız istemcinin tamamını bildiği alt kaynakta tam yer değiştirme (ör. tercihler). | 200 |
| `DELETE` | Silme (soft-delete, ADR-0013). | 204 |

- Durum alanı (`status`) `PATCH` ile değiştirilmez; geçiş her zaman alt eylemdir. FSM `domain.go`'da tablo olarak durur (CLAUDE.md).
- **PATCH için açık konu:** Merge Patch'te "gönderilmedi" ile "null" farklıdır. Go'da bu ayrım oapi-codegen'in `nullable-type` seçeneğini, o da `github.com/oapi-codegen/nullable` modülünü gerektirir. **Bu bağımlılık onay bekliyor**; onaylanana kadar spec'e PATCH operasyonu yazılmaz.
- Uzun iş isteğe bağlanmaz: 202 + iş kaynağı döner; sonuç iş kaynağından okunur (CLAUDE.md "Transaction").

## 4. İstek gövdesi

- **Gövde biçimi:**
  - Gövde `application/json`'dur; PATCH'te `application/merge-patch+json` kullanılır (§3).
  - Desteklenmeyen `Content-Type` `415 CONTENT_TYPE_UNSUPPORTED` döner (ADR-0019) **[test]**.
- **Bilinmeyen alan reddedilir** (plan §3.3, "bilinmeyen alan reddi istekte açık"):
  - JSON istek gövdesi ve içindeki **her nesne** bilinmeyen alanı reddeder. Düz nesne `additionalProperties: false`, `allOf` ile kurulan nesne `unevaluatedProperties: false` yazar **[lint: koala-closed-request-body]**.
  - Bilinmeyen alan 400 `REQUEST_INVALID` + `FIELD_UNKNOWN` döner. Gövdesi olmayan operasyona gelen gövde de reddedilir **[test]**.
- **Doğrulama:**
  - Biçim spec'ten kin-openapi ile doğrulanır; Go'da etiketle ikinci kopya tutulmaz (ADR-0006 madde 2).
  - Handler yalnız doğrulanmış değeri alır: doğrulamadan sonra ham gövde okunamaz, ikinci ve doğrulanmamış bir decode olmaz (ADR-0019) **[test: httpx TestValidateRequestsHandsOnlyTheValidatedValue]**.
  - kin gövdeye şema varsayılanlarını yazar. Gövdenin parmak izi (idempotency) bu doğrulanmış değerden alınır.
- **Gövde sınırı:**
  - Varsayılan sınır 1 MiB'tır; aşımı 413 `REQUEST_BODY_TOO_LARGE` döner (CLAUDE.md "Hatalar").
  - Daha büyük gövde gereken operasyon `x-body-limit: <bayt>` yazar. Doğrulayıcı bu sınırı gövdeyi okumadan önce uygular **[lint: koala-body-limit]** **[test]** (uzantı öneri).
- **Dosya yüklemeleri API'den geçmez.** İstemci, API'nin ürettiği presigned URL ile dosyayı doğrudan nesne deposuna (R2/S3) yükler. API yalnız yüklemeyi başlatan ve tamamlayan JSON operasyonlarını sunar. Doğrulayıcı `multipart/form-data` işlemez (ADR-0019; S3, GCS ve Stripe pratiği) **[lint: koala-no-multipart-upload]**.

## 5. Yanıtlar ve durum kodları

- **Zarf yoktur.** Başarılı yanıt çıplak kaynaktır ya da sayfa nesnesidir; `success`, `message`, `httpStatus` alanları yoktur (ADR-0005 madde 3) **[lint: koala-no-envelope]**.
- **Hata yanıtı RFC 9457 problem belgesidir** (`application/problem+json`, `api/components/problem.yaml#/components/schemas/Problem`). Her 4xx, 5xx ve `default` yanıtı içeriklidir ve yalnız problem belgesi döner (ADR-0007) **[lint: koala-error-responses-are-problems]**.
  - Gövde `httpx.Problem` ile birebir aynıdır **[test: httpx TestWriteErrorGoldenBody, api TestProblemSchemaMatchesHTTPX]**.
  - `instance` eşleşen route şablonudur; yol parametresinin değeri yazılmaz (ADR-0019) **[test]**.
- Her operasyon `default` yanıtını tanımlar **[lint: koala-default-problem-response]** (öneri). Ortak hata yanıtları kök spec'in `components/responses`'ındadır (`BadRequest`, `NotFound`, …).
- Durum kodları (tablo öneri; 422 ve sürüm satırının dayanağı ADR-0005 ve ADR-0019):

| Kod | Ne zaman |
|---|---|
| 200 | Okuma, güncelleme, gövdeli eylem |
| 201 | Oluşturma; `Location` başlığı yeni kaynağı gösterir |
| 202 | Kabul edildi, iş arka planda (River) |
| 204 | Gövdesiz başarı (`DELETE`, gövdesiz eylem) |
| 400 | Sözleşmeye uymayan istek: `REQUEST_INVALID`, `REQUEST_BODY_MALFORMED`, `SORT_FIELD_NOT_ALLOWED`, `CURSOR_INVALID`, `IDEMPOTENCY_KEY_REQUIRED`. Alan kuralı ihlali alanın kendi kodunu taşır. Desteklenmeyen istemci sürümü `APP_VERSION_UNSUPPORTED` + `X-Minimum-App-Version` döner (426 değil: RFC 9110 426'da `Upgrade` başlığını zorunlu tutar; ADR-0019) |
| 401 | Kimlik yok ya da geçersiz |
| 403 | Kimlik var, eylem yasak. **Okuma reddi 403 değil, kaynağın 404'üdür**; gövde bayt bayt aynıdır (ADR-0014) |
| 404 | Kaynak yok ya da görme yetkisi yok; `ROUTE_NOT_FOUND` yalnız bilinmeyen yolda döner |
| 405 | Yol var, metot yok; `Allow` başlığı döner |
| 409 | Kaynağın durumu isteğe izin vermiyor ya da aynı Idempotency-Key'li istek sürüyor |
| 413 | Gövde sınırı aşıldı |
| 415 | Gövdenin `Content-Type`'ı desteklenmiyor |
| 422 | **Yalnız** aynı Idempotency-Key farklı gövdeyle geldiğinde (ADR-0005 madde 6). Alan doğrulaması 422 değil, 400'dür |
| 429 | Oran sınırı aşıldı; `Retry-After` döner |
| 500 | Beklenmeyen hata (`INTERNAL`); sebep yalnız log'da. Doğrulayıcının sınıflayamadığı hata da 500'dür (ADR-0019) |
| 502 | Üst sistem (Iyzico, FCM…) hatası |
| 503 | Sunucu kapanıyor ya da hazır değil |

## 6. Sayfalama, süzme ve sıralama

- **Sayfalama her yerde cursor'ladır** (K10, ADR-0005 madde 4; AIP-158, Stripe, GitHub, Slack). Sayı ya da offset tabanlı sayfa yoktur **[lint: koala-cursor-pagination]**.
  - İstek `?limit=&cursor=` biçimindedir (`pagination.yaml#/components/parameters/Limit`, `Cursor`). `limit` 1–100 arasıdır, varsayılan 20'dir (aralık öneri).
  - Yanıt `{items, nextCursor, prevCursor}` biçimindedir. Liste şeması `allOf` ile `CursorPage`'i alır ve `items`'ı ekler. `nextCursor: null` son sayfa demektir.
  - Cursor opaktır: istemci onu çözmez, kurmaz, saklamaz. Cursor sıralama anahtarını ve süzgeçleri kodlar. Bozuk ya da başka bir sorguya ait cursor 400 `CURSOR_INVALID` döner (kod adı öneri).
- **`total` yalnız admin listelerinde ve SEO'lu aramada sunulur** (ADR-0005 madde 4, plan §4.1-4). O operasyon `IncludeTotal` parametresini ve `CountedCursorPage`'i kullanır **[lint: koala-include-total-admin-or-seo]**. SEO'lu arama `x-seo-search: true` ile işaretlenir (uzantı öneri). `total` JavaScript'in güvenli tam sayı aralığındadır (ADR-0019).
- **Sıralama** dışındaki alan 400 `SORT_FIELD_NOT_ALLOWED` döner (ADR-0005 madde 4).
  - Sözdizimi `?sort=-startsAt,id` biçimindedir: virgülle ayrılmış alanlar, `-` öneki azalan sıra (sözdizimi öneri; JSON:API ve Zalando ile aynı).
  - Operasyon kabul ettiği alanları `x-sort-fields` ile listeler **[lint: koala-sort-fields]** (uzantı öneri).
  - Sıralama her zaman tekil bir alanla (`id`) biter ki cursor kararlı olsun (öneri).
- **Süzme** alanın adını taşıyan sade query parametresidir (öneri):
  - Çoklu değer dizidir (`form` + `explode`): `?status=OPEN&status=FILLED`.
  - Aralıklar `<alan>From` (dahil) ve `<alan>To` (hariç) çiftidir: `?startsAtFrom=…&startsAtTo=…`.
  - Serbest süzgeç dili (OData `$filter`, AIP-160) kullanılmaz.

## 7. Idempotency

- Para ya da yan etki doğuran her `POST` **`Idempotency-Key` başlığını zorunlu** alır: ödeme oturumu, iade, çıkış (personel), başvuru, kayıt, şikayet (ADR-0005 madde 6, plan §4.1-6; Stripe; IETF taslağı). Parametre `headers.yaml#/components/parameters/IdempotencyKey`'dir.
- Yan etkisi olmayan `POST` bunu `x-no-idempotency-key: <gerekçe>` ile açıkça yazar **[lint: koala-idempotency-key]** (uzantı öneri).
- **Anahtarın biçimi** (öneri):
  - 8–255 karakterdir ve `[A-Za-z0-9_.:-]` kümesinden oluşur. İstemci her mantıksal işlem için yeni bir UUID üretir ve yeniden denemede aynısını gönderir.
  - Çıplak değer (Stripe) ve taslaktaki tırnaklı structured-field string (`"…"`) ikisi de kabul edilir; tırnaklar soyulur ve ikisi aynı anahtardır (ADR-0019) **[test: httpx TestIdempotencyKey]**.
- Aynı anahtar farklı gövdeyle gelirse 422 `IDEMPOTENCY_KEY_REUSED`, ilk istek sürerken gelirse 409 `IDEMPOTENCY_KEY_IN_USE` döner (ADR-0005 madde 6; kod adları öneri).
- Anahtar aktör ve operasyonla saklanır (`idempotency_keys`, plan §4.1-6). Gövdenin parmak izi doğrulanmış değerin kanonik JSON'undan alınır (§4). Uygulaması Faz 1'dedir.

## 8. Hata kodları

- Kod **UPPER_SNAKE**'tir, **anlamı hiç değişmez** ve `problem.yaml#/components/schemas/ErrorCode` listesindedir. Servis yeni bir kod döndürmeden önce kod oraya eklenir (CLAUDE.md "Hatalar", ADR-0007 madde 5). İstemcilerin kod listesi buradan üretilir.
- HTTP platformunun döndürdüğü her kod listededir **[test: internal/app TestPlatformCodesAreInTheContract]**.
- **Kod adlandırması** (öneri):
  - Ad durumu değil, sebebi söyler: `PAYMENT_ATTEMPT_ACTIVE`, `IBAN_INVALID`.
  - Önek alan adıdır (`PAYMENT_`, `JOB_`); platform kodları öneksizdir.
  - HTTP durum adı tekrar edilmez (`CONFLICT_…` yok).
  - Biçim hatalarının kodları `FIELD_*`'dır (`FIELD_REQUIRED`, `FIELD_UNKNOWN`, `FIELD_TYPE_INVALID`, `FIELD_FORMAT_INVALID`, `FIELD_VALUE_INVALID`, `FIELD_OUT_OF_RANGE`). Alan kuralının ihlali kendi kodunu taşır (`IBAN_INVALID`).
- **Alan hataları** `errors[]`'tadır (ADR-0019; JSON:API `source`, Microsoft `target`):
  - Gövdedeki üye: `{pointer, code}`. `pointer` RFC 6901'dir: `/amount/currency`; gövdenin tamamı için `""`.
  - Parametre: `{parameter, in, code}`. `in` değeri `QUERY`, `PATH`, `HEADER` ya da `COOKIE`'dir.
  - Tip hatası parametrede ve gövdede aynı kodu alır: `limit=abc` → `FIELD_TYPE_INVALID` **[test]**.
- **Metin yok:**
  - `detail` alanı yoktur; kullanıcı metni istemcidedir (ADR-0005 madde 3).
  - Doğrulayıcının iletileri istek değerleri içerebilir; hiçbiri yanıta, log'a ya da hata zincirine girmez **[test: httpx TestValidateRequestsNeverEchoesValues]**.
- **Kapalı enum** (öneri): `ErrorCode` ve diğer enum'lar `enum` ile kapalı yazılır. Zalando'nun `x-extensible-enum` önerisinden bu noktada ayrılır. Listeye değer eklemek uyumludur (§2), bu yüzden istemci üreteci bilinmeyen değeri reddetmeyecek biçimde ayarlanır:
  - openapi-generator (Dart, TypeScript): `enumUnknownDefaultCase=true`. Üreteç bilinmeyen değer için ek bir üye (`unknown_default_open_api`) üretir.
  - openapi-typescript: enum union tipi kalır; istemci kodu `switch`'te `default` dalını zorunlu tutar.

## 9. Veri tipleri

- **Para:**
  - Para nesnesi yanıtta `Money`, istekte `MoneyInput`'tur: `{"amount": 15000, "currency": "TRY"}` (K5, ADR-0008; Stripe, Adyen).
  - `amount` en küçük birimde (kuruş) `int64`'tür ve JavaScript'in güvenli tam sayı aralığındadır (±9007199254740991, ADR-0019).
  - Para alanı (`…amount`, `…price`, `…fee`, `…cost`, `…balance`, `…commission`, `…payout`, `…refund`, `…wage`, `…deposit`, `…Total`) `Money`/`MoneyInput`'a `$ref` verir; sayı ya da string ondalık olamaz **[lint: koala-money-is-money]**.
  - `"150.00"` biçimi yalnız Iyzico sınırında üretilir.
- **Oran** baz puandır (`BasisPoints`, 1250 = %12,5; K5).
  - Alanın adı `Bps` ile biter: `commissionBps` (sonek öneri).
  - Sayısal `…Rate`, `…Percent`, `…Ratio` alanı yazılmaz **[lint: koala-rate-in-basis-points, koala-bps-integer]**.
- **Anlık zaman** RFC 3339 `date-time`'dır (`Timestamp`, K11, ADR-0005 madde 5).
  - Değer UTC'dir ve `Z` ile biter; ofsetli değer reddedilir (ADR-0019) **[test: api TestTimestampRequiresZ]**.
  - Alan adı `At` ile biter: `createdAt` **[lint: koala-instant-is-date-time]**.
- **Yerel anlamı olan zaman** (ilan başlangıcı) `OffsetDateTime`'dır. Değer ofsetli yazılır; saat dilimi aynı nesnede `timeZone` alanında taşınır (K11, plan §3.8): `{"startsAt": "2026-08-27T09:30:00+03:00", "timeZone": "Europe/Istanbul"}`.
- **Takvim günü** `format: date`'tir (`Date`). Adı `On` ya da `Date` ile biter: `birthDate` (sonek öneri) **[lint: koala-calendar-date-is-date]**.
- **Kimlik** `format: uuid`'dir (UUIDv7, plan §3.6-4; `Id`). Biçimi doğrulanır **[test]**.
- **Tam sayı** `format: int32` ya da `int64` yazar (öneri) **[lint: koala-integer-format]**. Koordinat gibi gerçek ondalıklar `number` + `format: double`'dır.
- **Null** OpenAPI 3.1 biçiminde yazılır **[lint: koala-no-nullable-keyword]**:
  - Düz alanda `type: [string, "null"]`; `$ref`'li şemada `oneOf: [{$ref: …}, {type: "null"}]`. 3.0'ın `nullable` anahtarı kullanılmaz.
  - Yanıtta "değer yok" null ile yazılır, alan atlanmaz. Zorunlu + nullable alan Go'da `omitempty`'siz pointer olur **[test: internal/codegentest]**.
  - Tip denetleyen her lint kuralı tip dizilerini de tanır.
- **Enum** `enum` ile yazılır. 3.1'in `oneOf` + `const` kalıbı oapi-codegen'de enum sabiti değil düz string üretir (ADR-0019) **[test: internal/codegentest]**.
- **Örnekler** (`examples`) kendi şemalarına uyar **[test: api TestExamplesMatchTheirSchemas]**.

## 10. Başlıklar

- **Platform başlıkları operasyonlara yazılmaz.** Tanımları `headers.yaml`'dadır ki istemciler tip ve sabit üretsin (ADR-0005 madde 7). Her başlığın zorunluluğunu kendi middleware testi kanıtlar:
  - `X-Request-Id` isteğe bağlı gelir ve her yanıtta döner **[test: httpx middleware testleri]**.
  - `X-Device-Id` `/v1/admin` dışındaki her istekte zorunludur. Middleware'i ve testi Faz 1'de gelir.
  - `X-App-Version` semver'dir. Desteklenmeyen sürüm `400 APP_VERSION_UNSUPPORTED` + `X-Minimum-App-Version` döner (ADR-0019). Middleware'i ve testi Faz 1'de gelir; kapı `/v1` dışında da çalışır.
  - `X-Client-Platform` değerleri `IOS`, `ANDROID`, `WEB`, `ADMIN`'dir (değerler öneri).
- Operasyona özgü başlık (`Idempotency-Key`) operasyonun `parameters`'ında referanslanır.
- `Accept-Language` ile mesaj çözümü yoktur (ADR-0005 madde 7).

## 11. Yetki uzantıları

- **Her operasyon** ya `x-authz` taşır ya da `security: []` ile açıkça public'tir; ikisi birden olmaz (ADR-0005 madde 1, plan §4.1-1) **[lint: koala-operation-authz]**.
  - Biçim: `x-authz: {action: can_read, object: job}`. `action`, authz modelindeki (`model.fga`, ADR-0014, plan §5.4) `can_*` relation'ıdır; `object` aynı modeldeki tiptir.
  - Varsayılan güvenlik `marketplaceBearer`'dır (kök `security`).
- **`/v1/admin` altındaki her operasyon** `security: [{adminBearer: []}]` ve `x-authz-permission: payout.release` taşır. Eşleme eksikse sunucu açılmaz (Faz 1) **[lint: koala-admin-permission]**. `adminBearer` ve `x-authz-permission` başka yerde kullanılmaz **[lint: koala-admin-only-under-admin]**.
- **Sıra:** authn middleware'i (Faz 1) istek doğrulamasından önce gelir. Kimliksiz istek gövde biçimi hakkında bir şey öğrenmez (ADR-0019).

## 12. Kullanımdan kaldırma (öneri)

Bu bölümün planda ya da bir ADR'de dayanağı yoktur.

- Kaldırılacak operasyon ya da alan `deprecated: true` olur. Operasyon kaldırılma gününü `x-sunset: YYYY-MM-DD` ile yazar **[lint: koala-deprecation-has-sunset]**.
- Sunucu kaldırılmış operasyonun yanıtına `Deprecation` (RFC 9745) ve `Sunset` (RFC 8594) başlıklarını ekler. Tanımları `headers.yaml`'dadır.
- Kaldırma süresi en az bir zorunlu mobil güncelleme döngüsüdür: `X-Minimum-App-Version` yeni yolu kullanan sürüme çıkmadan eski yol silinmez. Yayımlanmamış (henüz istemcisi olmayan) operasyon doğrudan değiştirilebilir.

## 13. Akış ve araçlar

- Akış: spec PR'ı → `make api-lint` → `make generate` → uygulama (ADR-0005 madde 1).
- `make api-lint` vacuum'la iki şey denetler:
  - kök spec (`api/ruleset.yaml`: Spectral'ın önerilen seti + Koala kuralları),
  - her bileşen dosyası (`api/ruleset-schemas.yaml`).

  Ardından `scripts/api-lint-probe.sh`, her Koala kuralının `api/testdata/lint-probe.yaml`'da gerçekten tetiklendiğini kanıtlar. Hata kapıyı kırmızı yapar; uyarılar yalnız raporlanır.
- `make generate` `internal/apiv1`'i ve `internal/codegentest`'i oapi-codegen ile üretir. Üretilen kod commit edilir. `make gen-check` eski dosyaları silip yeniden üretir ve fark varsa kırmızı olur. İkisi de `make verify`'ın parçasıdır (ADR-0019).
- Yeni bir bileşen dosyası `api/components/`'a eklenirse iki satır eklenir:
  - `internal/apiv1/generate.go`'ya bir `go:generate` satırı,
  - `internal/apiv1/oapi-codegen.yaml`'ın `import-mapping`'ine bir satır.
- `/v1/meta/ping` sözleşme duman testidir. Üretimde bağlanmaz ve ilk gerçek operasyonla silinir (ADR-0019) **[test]**.
