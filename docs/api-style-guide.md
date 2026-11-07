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
| `PATCH` | Kısmi güncelleme, **JSON Merge Patch** (RFC 7396, `application/merge-patch+json`; ADR-0019): gönderilmeyen alan değişmez, `null` alanı temizler. | 200 + güncel kaynak |
| `PUT` | Yalnız istemcinin tamamını bildiği alt kaynakta tam yer değiştirme (ör. tercihler). | 200 |
| `DELETE` | Silme (soft-delete, ADR-0013). | 204 |

- Durum alanı (`status`) `PATCH` ile değiştirilmez; geçiş her zaman alt eylemdir. FSM `domain.go`'da tablo olarak durur (CLAUDE.md).
- **PATCH yalnız JSON Merge Patch'tir** (RFC 7396; ADR-0019). Gövdenin `Content-Type`'ı `application/merge-patch+json` olmalıdır; `application/json` `415 CONTENT_TYPE_UNSUPPORTED` döner **[lint: koala-patch-is-merge-patch]** **[test: internal/codegentest]**. Uçtan uca test (doğrulama + bağlama) ilk gerçek PATCH operasyonuyla `internal/app`'e gelir; `PATCH /v1/meta/ping` ADR-0025 ile kaldırıldı. Patch şeması şu kurallara uyar:
  - Kökte hiçbir üye zorunlu değildir. Şema ve içindeki her nesne bilinmeyen üyeyi reddeder (§4) **[lint: koala-merge-patch-shape, koala-closed-request-body]**. İç içe değer nesneleri (`MoneyInput` gibi) atomiktir: tamamı gönderilir ve kendi zorunlu üyelerini korur.
  - Temizlenebilen üye nullable'dır: düz alanda `type: [string, "null"]`, `$ref`'li alanda `oneOf: [{$ref: …}, {type: "null"}]`. Go'da `nullable.Nullable[T]` olur, böylece "gönderilmedi", "null" ve "değer" üç ayrı durum kalır. Bunun için üye `x-go-type: nullable.Nullable[…]`, `x-go-type-import: {path: github.com/oapi-codegen/nullable}` ve `x-go-type-skip-optional-pointer: true` taşır. Kural iç içe nesnelerdeki üyelere de uygulanır. Bu üç uzantı yoksa üye pointer olur ve `null` ile temizleme isteği "gönderilmedi"den ayırt edilemez, sessizce kaybolur **[lint: koala-merge-patch-nullable]** **[test: internal/codegentest]**.
  - Temizlenemeyen üye nullable değildir; ona `null` göndermek `400 FIELD_TYPE_INVALID` döner.
  - `nullable.Nullable` yalnız merge patch üyelerinde kullanılır; oapi-codegen'in global `nullable-type` seçeneği açılmaz. Gerekçe: zorunlu + nullable bir yanıt alanı `Nullable` olursa handler'ın atamayı unuttuğu değer `null` değil, sıfır değer (`""`, `0`) olarak yazılır. Pointer'da unutulan değer `null`'dır.
  - Örnek: `internal/codegentest/testdata/openapi31.yaml`'daki merge patch şeması.
- Uzun iş isteğe bağlanmaz: 202 + iş kaynağı döner; sonuç iş kaynağından okunur (CLAUDE.md "Transaction").

## 4. İstek gövdesi

- **Gövde biçimi:**
  - Gövde `application/json`'dur; PATCH'te `application/merge-patch+json` kullanılır (§3).
  - Desteklenmeyen `Content-Type` `415 CONTENT_TYPE_UNSUPPORTED` döner (ADR-0019) **[test]**.
- **Bilinmeyen alan reddedilir** (plan §3.3, "bilinmeyen alan reddi istekte açık"):
  - JSON istek gövdesi ve içindeki **her nesne** bilinmeyen alanı reddeder. Düz nesne `additionalProperties: false`, `allOf` ile kurulan nesne `unevaluatedProperties: false` yazar; `allOf` üyesi tek başına kapatılmaz, onu bileşik nesne kapatır **[lint: koala-closed-request-body]** **[test: api TestRequestBodiesAreClosed]**. vacuum yalnız kendi `properties`'i olan nesneye ulaşır; `allOf` ile kurulan özellikleri Go testi denetler.
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
  - İki biçim birbirini dışlar; şema bunu zorlar (`dependentRequired`, `not`, `if`/`then`) **[test: api TestProblemFieldIsPointerXorParameter]**.
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
  - Yanıtta `Z` tek yerden zorlanır: `Timestamp` Go'da `jsontime.UTC`'dir ve başka dilimdeki zamanı da UTC yazar; handler `.UTC()` çağırmak zorunda değildir (ADR-0021) **[test: jsontime TestMarshalWritesUTC]**.
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
  - `X-Client-Platform` değerleri `IOS`, `ANDROID`, `WEB`, `ADMIN`'dir ve hangi kuralın uygulanacağını belirler. Kabulde harf duyarsızdır (`web` = `WEB`); kanonik biçim büyük harftir. Değer istemcinin beyanıdır, yetki girdisi değildir. `/v1/admin` dışında zorunludur; `ADMIN` yalnız `/v1/admin` altında geçerlidir (ADR-0021) **[test: httpx TestClientGate]**.
  - `X-Device-Id` (`[A-Za-z0-9._:-]{8,128}`) `/v1/admin` dışındaki her istekte zorunludur. Tek istisna tarayıcının WebSocket handshake'idir **[test]**.
  - `X-App-Version` semver'dir; yalnız `IOS` ve `ANDROID` için okunur ve zorunludur. Desteklenmeyen, eksik ya da okunamayan sürüm `400 APP_VERSION_UNSUPPORTED` + `X-Minimum-App-Version` döner (ADR-0019). Kapı yönlendirmeden önce, WebSocket dahil her yolda çalışır **[test]**.
  - Başlık hatası `400 REQUEST_INVALID` + `{parameter, in: HEADER, code}` döner.
  - Koala uygulaması olmayan çağıranın (ödeme callback'i, takip pikseli) route'u `internal/app`'te `ClientPolicy.ExemptRoutes`'a, testiyle eklenir.
- Operasyona özgü başlık (`Idempotency-Key`) operasyonun `parameters`'ında referanslanır.
- `Accept-Language` ile mesaj çözümü yoktur (ADR-0005 madde 7).

## 11. Yetki uzantıları

- **Her operasyon** ya `x-authz` taşır, ya `security: []` ile açıkça public'tir, ya da `x-authz-exempt: <gerekçe>` ile belgeli istisnadır; yalnız biri bulunur (ADR-0005 madde 1, plan §4.1-1; `x-authz-exempt` ADR-0021 ile ADR-0005 madde 1'i ve plan §4.1-1/§6.3'ü genişletir) **[lint: koala-operation-authz]**.
  - `x-authz-exempt` kimlik isteyen ama yetkilendirilecek nesnesi olmayan uç içindir (kendi oturumu için bilet gibi). Gerekçe en az 20 karakterdir; `security: []` ile birlikte kullanılmaz.
  - Biçim: `x-authz: {action: can_read, object: job}`. `action`, authz modelindeki (`model.fga`, ADR-0014, plan §5.4) `can_*` relation'ıdır; `object` aynı modeldeki tiptir.
  - Varsayılan güvenlik `marketplaceBearer`'dır (kök `security`).
- **`/v1/admin` altındaki her operasyon** `security: [{adminBearer: []}]` ve `x-authz-permission: payout.release` taşır; izin, `x-authz`'ın yerine geçen dördüncü biçimdir (nesnesi platformdur, ADR-0026) **[lint: koala-operation-authz]**. `x-authz` ile `x-authz-permission` hiçbir yolda birlikte yazılmaz; ikisi birden ikinci bir karar kaynağı demektir **[lint: koala-operation-authz, koala-admin-permission]**. Eşleme eksikse ya da izin katalogda (`internal/authz/permissions.go`) yoksa sunucu açılmaz **[lint: koala-admin-permission]** **[test: staff TestGateRefusesAnUnmappedStaffOperation]**. İzni olmayan personel `403 STAFF_PERMISSION_DENIED` alır; sözleşmede olmayan yol `404 ROUTE_NOT_FOUND`'dur (ADR-0026). `adminBearer` ve `x-authz-permission` başka yerde kullanılmaz **[lint: koala-admin-only-under-admin]**. İki dar istisna vardır (ADR-0025 §9; ADR-0005 madde 1'i ve plan §4.1-1, §5.5'i genişletir):
  - **Kimliksiz personel uçları** yalnız `/v1/admin/auth/login` ve `/v1/admin/auth/refresh`'tir: `security: []`, izin ve muafiyet yok **[lint: koala-admin-public]**.
  - **Personelin kendi hesabına dokunan uçlar** yalnız açık izin listesindedir: `/v1/admin/me`, `/v1/admin/me/capabilities` (ADR-0026), `/v1/admin/auth/logout` ve `/v1/admin/auth/password`. Bunlar adminBearer + `x-authz-exempt` taşır, `x-authz-permission` taşımaz **[lint: koala-admin-self-service]**. Liste dışındaki her yol, `/v1/admin/auth/` altındaki yeni bir uç da, izin ister; orada ne `x-authz-exempt` ne `security: []` izin yerine geçer **[lint: koala-admin-permission]**.
  - Kurallar `head`, `options` ve `trace` dahil her HTTP metoduna uygulanır.
- **Sıra:** authn middleware'i (Faz 1) istek doğrulamasından önce gelir. Kimliksiz istek gövde biçimi hakkında bir şey öğrenmez (ADR-0019).
- **Her GET operasyonu** okuduğu tabloyu `x-resource: <tablo>` ile adlandırır (`db/softdelete.yaml`'daki ad) **[lint: koala-get-resource]**. Üretilmiş okuma testi (`softdeletetest.ReadTest`) bu operasyonlarda silinmiş nesnenin gizlendiğini sınar (ADR-0020); gerçek sözleşmede `internal/app.TestReadTestOnTheContract` koşar **[test]**.
  - Tablo okumayan GET `x-resource: none` yazar ve gerekçesini `x-resource-reason` ile verir (en az 20 karakter); gerekçesiz `none` lint'te kırmızıdır.
  - Çağıranın kendi kaydını döndüren uç (`/v1/me`, `/v1/admin/me`) ayrıca `x-resource-self: true` taşır: okuma testi onu kimliğiyle çağırır ve silinmeden sonra 200 değil 401 bekler (ADR-0026). Yol parametresi olan operasyon `x-resource-self` taşımaz.
  - Soft-delete'li olmayan tabloyu (M1–M6) adlandıran operasyon okuma testinde atlanır: gizlenecek bir şey yoktur.

- **Oran sınırı grubu:** Operasyon varsayılan grubundan (GET/HEAD/OPTIONS → `read`, diğerleri → `write`) başka bir gruba `x-rate-limit: <grup>` ile girer (`auth`, `upload`, `message`, `sitemap`, `boarding`). Grup tablosu `internal/app/edge.go`'dadır; bilinmeyen grup açılışı durdurur (ADR-0021) **[test]**. `auth_account` grubunun route'u yoktur: giriş, hedef başına (adres fark etmeksizin) kovayı buradan alır (ADR-0025 §8).

## 11.1 Dondurulmuş ama uygulanmamış operasyon (ADR-0029 §1)

Bir alanın sözleşmesi bir kerede dondurulur ki istemci işi (plan §4.2) uygulamayı beklemeden başlasın. Handler'ı henüz gelmemiş operasyon şunu taşır:

- `x-not-implemented: <gerekçe>` (en az 20 karakter). Operasyon `501 NOT_IMPLEMENTED` döner ve bu kod **operasyonun `default` yanıtının altındadır**; `501` ayrı bir yanıt olarak yazılmaz. Gerekçe: `501`'i yazıp handler gelince silmek operasyonun durum kümesini değiştirir, §2 bunu uyumsuz değişiklik sayar.
- İşaret üç kapıyı birden gevşetir ve üçü de işaret kalkınca kendiliğinden silahlanır:
  - `internal/app.TestNotImplementedOperationsAreDeclared` işaretli operasyonların kümesini `pending.Operations()`'a **iki yönlü** bağlar: unutulan handler da unutulan işaret de kırmızıdır **[test]**.
  - `softdeletetest.ReadTest` işaretli GET'i kataloğa bakmadan atlar, böylece `x-resource` henüz var olmayan bir tabloyu dürüstçe adlandırabilir **[test]**.
  - `test/scenarios.TestFrozenDomainsDeferTheirScenarios` alanın P0 senaryolarının `deferred` olmasını ister; işaretli operasyon kalmayınca bağlanmış olmalarını ister **[test]**.
- `501` `404 ROUTE_NOT_FOUND` ile karıştırılmaz: yol **vardır** ve sözleşmesi bağlayıcıdır, yalnız uygulaması yoktur.

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

  Ardından `scripts/api-lint-probe.sh` iki şeyi kanıtlar. `api/testdata/lint-probe.yaml`'daki bağımsız vakaların Koala sonuçları, (kural, JSON yolu) çiftleriyle `lint-probe.expect`'e birebir eşittir; eksik ya da fazla sonuç kapıyı kırmızı yapar. `lint-pass.yaml`'daki doğru biçimler ise hiçbir Koala sonucu üretmez. Bir kural değişince iki dosya ve beklenti listesi birlikte güncellenir. Lint hatası kapıyı kırmızı yapar; uyarılar yalnız raporlanır.
- `make generate` `internal/apiv1`'i ve `internal/codegentest`'i oapi-codegen ile üretir. Üretilen kod commit edilir. `make gen-check` eski dosyaları silip yeniden üretir ve fark varsa kırmızı olur. İkisi de `make verify`'ın parçasıdır (ADR-0019).
- Yeni bir bileşen dosyası `api/components/`'a eklenirse iki satır eklenir:
  - `internal/apiv1/generate.go`'ya bir `go:generate` satırı,
  - `internal/apiv1/oapi-codegen.yaml`'ın `import-mapping`'ine bir satır.
- `/v1/meta/ping` ilk gerçek operasyonlarla (kimlik doğrulama, ADR-0025) kaldırıldı; doğrulama zincirinin uçtan uca testi `POST /v1/auth/login` üzerindedir (`internal/app TestLoginRequestValidation`) **[test]**.
