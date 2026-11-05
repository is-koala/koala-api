# ADR-0019: OpenAPI 3.1 spike sonucu, kod üretimi ve istek doğrulaması

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K9 (spike), K5, K10, K11
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §4.1, §6.1 Faz 0, §9 R4; ADR-0005, ADR-0006, ADR-0007
- Yerine geçtiği: ADR-0005 madde 7'deki 426 kullanımı (Karar 6)

## Bağlam

ADR-0006, oapi-codegen'in OpenAPI 3.1 desteğinin Faz 0 spike'ında doğrulanmasını şart koştu (K9, §9 R4). Spike, ortak bileşenler ve spec lint'iyle aynı PR'da (#6) yapıldı. PR #6'nın iki incelemesi şunları istedi:
- birkaç sözleşme kararının (hata alanları, 415, 426, para sınırı, yüklemeler) yazıya geçirilmesi,
- doğrulayıcının performans ve hata sınıflandırma açıklarının kapatılması.

Kabul edilmiş ADR düzenlenmediği için sonuç ve kararlar burada.

## Karar

### 1. Spike sonucu: oapi-codegen + 3.1 çalışıyor

oapi-codegen v2 + OpenAPI 3.1 + chi strict server ve kin-openapi istek doğrulaması 3.1 spec'le çalışıyor. ADR-0006 madde 2 geçerlidir; ogen'e ya da 3.0.3 alt kümesine geçilmez.

| Bileşen | Sürüm | Nerede |
|---|---|---|
| oapi-codegen (araç) | v2.8.2 | `internal/apiv1/generate.go`, `go run …@v2.8.2` |
| `github.com/oapi-codegen/runtime` | v1.8.0 | `go.mod` |
| `github.com/getkin/kin-openapi` | v0.152.0 | `go.mod` |

**Çalışanlar** (`internal/codegentest`, her biri testle sabit):
- `type: [T, "null"]` pointer olarak üretiliyor; alan zorunluysa `omitempty` yok, yani null yazılıyor.
- `oneOf: [{$ref: X}, {type: "null"}]` `*X` oluyor.
- `type: string` + `const` tek değerli enum oluyor.
- `webhooks` gövde tipini üretiyor.
- `$ref` yanındaki `description`/`examples` koda geçiyor. kin-openapi 3.1'de `$ref` yanındaki kısıtları da uyguluyor.
- Çok dosyalı spec `import-mapping: "-"` ile tek pakete üretiliyor.

**Sınırlar ve önlemler:**

| Bulgu | Önlem |
|---|---|
| `oneOf` + `const` ile yazılan enum düz `string` üretiliyor. | Enum'lar `enum` ile yazılır (stil rehberi §9). |
| `prefixItems` `[]any` üretiyor. | Kullanılmaz. |
| kin-openapi `uuid` biçimini varsayılan olarak doğrulamıyor. | Doğrulayıcının kendi seçeneklerine uuid doğrulayıcısı verildi; kin'in global tablosuna dokunulmadı. |
| kin-openapi'nin JSON Schema 2020-12 doğrulayıcısı şemayı **her istekte yeniden derliyor**; önbellek ya da seçenek yok. Ölçüm (`/v1/meta/ping` gövdesi, yalnız doğrulama): 45 µs ve 992 alloc; kin'in OpenAPI 3.0 doğrulayıcısıyla 2,7 µs ve 53 alloc. | Madde 2. |
| İki doğrulayıcı eksik ve fazla üyeyi farklı biçimde bildiriyor; 2020 doğrulayıcısı üye adını yalnız iletide veriyor. | `httpx` ikisini de `{pointer, code}`'a çeviriyor. İleti biçimi testle sabit; kin yükseltmesi biçimi değiştirirse CI kırmızı olur. |
| Doğrulayıcı iletileri istek değerlerini içeriyor. | İletiler yanıta, log'a ve hata zincirine girmez (Madde 3). |
| Yanıt doğrulaması middleware ile yapılamıyor (oapi-codegen #1038). | Strict server yanıt tiplerini derleme zamanında sınırlar. |

ogen denenmedi: engelleyici bir eksik çıkmadı.

### 2. Doğrulayıcı seçimi operasyon başına, açılışta

`httpx.ValidateRequests` her operasyonun istek şemalarını açılışta tarar:
- **2020'ye özgü anahtar kullanan operasyon** 2020 doğrulayıcısını alır. Bu anahtarlar: `const`, `unevaluatedProperties`, `if`/`then`/`else`, `prefixItems`, `contains`, `dependent*`, `patternProperties`, `propertyNames`, `$defs`, `contentSchema`, `$dynamicRef`, ayrıca `nullable`.
- **Diğer operasyonlar** OpenAPI 3.0 doğrulayıcısını alır. Bu doğrulayıcı derleme yapmaz ve 2020'ye özgü olanlar dışındaki bütün 3.1 anahtarlarını (tip dizileri, sayısal `exclusiveMinimum` dahil) uygular. kin de `$ref`'li şemalarda zaten ona düşüyor.

Gerekçe:
- Derlenmiş şemayı önbelleğe almak için JSON Schema derleyicisini (`santhosh-tekuri/jsonschema/v6`) doğrudan import etmek gerekir. Bu modül bugün yalnız kin'in dolaylı bağımlılığıdır; doğrudan import ADR-0003'e göre ayrı onay ister.
- O onay gelirse 2020 operasyonları için önbellekli sarmalayıcı yazılır.
- Bugün 2020 doğrulayıcısını alan operasyonlar istek başına derleme maliyetini öder ve bu belgelidir.

`BenchmarkPing` (`internal/app`) bütün yığını ölçer: router middleware'i, doğrulama, bağlama ve yanıt. Yerel ölçüm: 5,3 µs, 99 alloc.

### 3. Doğrulama hatalarının sınıflandırılması

- **İstemci hatası** problem belgesi olur ve `cause` taşımaz:
  - `400 REQUEST_INVALID` + alan hataları,
  - `400 REQUEST_BODY_MALFORMED` (gövde yok ya da JSON değil),
  - `413 REQUEST_BODY_TOO_LARGE`,
  - `415 CONTENT_TYPE_UNSUPPORTED`.
- **Sınıflanamayan her şey 500'dür** ve loglanır: derlenemeyen şema, `SecurityRequirementsError`, yeni bir hata tipi. Log satırı yalnız hata tiplerini ve anahtar adlarını yazar, değer yazmaz.
- Tip hatası parametrede de gövdede de aynı kodu alır: `limit=abc` → `FIELD_TYPE_INVALID`.
- Gövdesi olmayan operasyona gelen gövde reddedilir (`RejectWhenRequestBodyNotSpecified`). Bu yalnız `Content-Length > 0` olan istekte işler; chunked gövde bu denetimden geçer.

### 4. Hata alanları: `pointer` ya da `parameter` + `in`

`ProblemField` iki biçimden birini alır:
- Gövdedeki üye için RFC 6901 `pointer` (`""` gövdenin tamamı).
- Query, path, header ve cookie için `parameter` + `in` (`QUERY`, `PATH`, `HEADER`, `COOKIE`; UPPER_SNAKE enum kuralıyla).

Emsal: JSON:API `source.pointer` / `source.parameter`, Microsoft REST Guidelines `target`. Biçim `httpx.TestWriteErrorGoldenBody` ile sabittir; `api.TestProblemSchemaMatchesHTTPX` şema ile Go tipini karşılaştırır.

### 5. `instance` route şablonudur

`Problem.instance` eşleşen route şablonudur (`/v1/jobs/{jobId}`). Yol parametresinin değeri yazılmaz: kimlik, IBAN gibi kişisel veri taşıyabilir. Eşleşmeyen istekte (404) `instance` yoktur. Plan §3.4'teki örnek gövde gerçek yolu gösteriyordu; bu madde onun yerine geçer.

### 6. 426 kullanılmaz: `400 APP_VERSION_UNSUPPORTED`

RFC 9110 §15.5.22, 426 yanıtında `Upgrade` başlığını zorunlu tutar ve bu başlık protokol yükseltmesi içindir; uygulama sürümü için değil. Desteklenmeyen istemci sürümü bu yüzden `400 APP_VERSION_UNSUPPORTED` + `X-Minimum-App-Version` döner. **ADR-0005 madde 7'deki "426 + `X-Minimum-App-Version` korunur" ifadesinin yerine geçer.** ADR-0007'deki "426 için yeni `Kind`" gereksinimi de düşer.

### 7. 415: `KindUnsupportedMediaType`

`apperr`'e `KindUnsupportedMediaType` eklendi; `httpx.StatusOf` onu 415'e eşler. Kodu `CONTENT_TYPE_UNSUPPORTED`'dır.

### 8. Para ve büyük sayılar JavaScript'in güvenli aralığında

- `Money.amount` ve sayfa `total`'ı `minimum`/`maximum` ±9007199254740991 (2^53−1) ile sınırlıdır. Web istemcisi bu değerleri `number` olarak kayıpsız okur. TRY için bu sınır ~90 trilyon TL'dir.
- Yanıt şeması `Money` açıktır (alan eklemek kırıcı olmasın). İstek şeması `MoneyInput` aynı alanlarla kapalıdır (`additionalProperties: false`). `api.TestMoneyVariantsMatch` ikisini karşılaştırır.

### 9. Yüklemeler API'den geçmez

- Dosyalar presigned URL ile doğrudan R2/S3'e yüklenir; S3, GCS ve Stripe Files'ta yerleşik pratik budur.
- API, yüklemeyi başlatan (URL üreten) ve tamamlayan (nesneyi doğrulayan) JSON operasyonlarını sunar.
- Doğrulayıcı `multipart/form-data` işlemez; lint, multipart istek gövdesini reddeder (`koala-no-multipart-upload`).

### 10. `/v1/meta/ping` üretimde bağlanmaz

- `/v1/meta/ping` sözleşme duman testidir. `KOALA_ENV=production`'da route hiç kaydedilmez; her metotta bilinmeyen yol gibi 404 döner.
- İlk gerçek operasyon geldiğinde silinir.

### 10a. PATCH: JSON Merge Patch ve `nullable.Nullable`

- `PATCH` gövdesi yalnız JSON Merge Patch'tir (RFC 7396, `application/merge-patch+json`). Gönderilmeyen üye değişmez, `null` temizler, değer değiştirir. Başka içerik tipi 415 döner.
- Üç durumun Go'ya ulaşması için temizlenebilen üye `github.com/oapi-codegen/nullable` v1.2.0'ın `Nullable[T]` tipiyle üretilir (Apache-2.0; Yusuf, 05.11). Bu tip üye bazında `x-go-type` + `x-go-type-import` + `x-go-type-skip-optional-pointer: true` ile verilir.
- oapi-codegen'in global `nullable-type` seçeneği açılmaz. Gerekçe: o seçenek zorunlu + nullable yanıt alanlarını da `Nullable` yapar ve atanmamış bir `Nullable` `null` değil, sıfır değer olarak yazılır; bu sessiz bir hatadır. Pointer'da atanmamış değer `null`'dır.
- Lint (`koala-patch-is-merge-patch`, `koala-merge-patch-shape`, `koala-closed-request-body`) içerik tipini, zorunlu üye yokluğunu, kapalılığı ve nullable üyelerin `Nullable` olmasını zorlar.
- `internal/codegentest` üreticinin bu biçimi üç durumlu ürettiğini, `internal/app` `PATCH /v1/meta/ping` ile doğrulama + bağlama yolunu test eder. Doğrulayıcı `+json` medya tiplerini JSON olarak çözer; `runtime.BindJSONValue` `null`'u `Nullable`'ın `UnmarshalJSON`'una verir.

### 11. Üretilen kod commit edilir

- `internal/apiv1/*.gen.go` ve `internal/codegentest/*.gen.go` commit edilir. Go'da yerleşik pratik budur: `go generate` derlemenin parçası değildir, üretilen dosya paketle dağıtılır; protobuf, sqlc ve Kubernetes kodu da böyle. Getirisi:
  - PR diff'inde handler imzasının değiştiği görünür,
  - `go build`, IDE ve golangci-lint üretici olmadan çalışır.
- `make gen-check` eski `*.gen.go` dosyalarını silip yeniden üretir. Fark ya da izlenmeyen dosya varsa kapı kırmızıdır. `make verify`'ın parçasıdır.

### 12. Araç sabitleme (ADR-0004'ün ölçütü)

- oapi-codegen `go.mod`'a girmez: `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2`. Bu yol aracın bağımlılıklarını uygulamanın modül grafiğinden uzak tutar; kaynak sum.golang.org ile doğrulanır.
- Sürüm `internal/apiv1/generate.go`'da bir kez yazılıdır; `internal/codegentest` aynısını kullanır (`TestGeneratorVersionMatchesAPIV1`). Runtime aynı PR'da birlikte yükseltilir.
- vacuum `go install` ile `./bin/vacuum-<sürüm>`'e kurulur; sürüm `Makefile`'dadır (`VACUUM_VERSION`).

### 13. Spec lint: vacuum v0.36.2

- vacuum (MIT) Go ile yazılıdır; Node gerektirmez (Spectral npm ister). Spectral kural biçimini okur ve 3.1'i destekler.
- Kural setleri: `api/ruleset.yaml` (Spectral önerilen seti + Koala kuralları) ve `api/ruleset-schemas.yaml` (şema kuralları; her bileşen dosyasında ayrıca koşar).
- vacuum'da iki sınır bulundu:
  - `field: "@key"` desteklenmiyor; anahtar kuralları `…[*]~` ile yazıldı.
  - Bazı `field` aramaları yanlış düğüme bakıyor; bu kurallar `schema` fonksiyonuyla yazıldı.
- Hiçbir şeye eşleşmeyen kural kapıyı sessizce yeşil gösterirdi. Bu yüzden `scripts/api-lint-probe.sh`, her Koala kuralının `api/testdata/lint-probe.yaml`'da tetiklendiğini kanıtlar. Probta tip dizisi (`[T, "null"]`) vakaları da vardır.

### 14. Doğrulama middleware'i kendi kodumuz

- `oapi-codegen/nethttp-middleware` onaylı listede olmadığı için alınmadı. Aynı iş `httpx.ValidateRequests`'te yapılıyor.
- Operasyon chi'nin eşleştirdiği route şablonundan bulunur; kin'in gorilla/mux router'ı kullanılmaz.
- Bunun üstüne şunlar eklendi: operasyon başına `x-body-limit`, doğrulayıcı seçimi, değersiz hata sınıflandırması ve yalnız doğrulanmış değeri veren gövde.
- Toplam ~500 satırdır, yorumlar dahil (`internal/platform/httpx/validate.go`).

## Sonuçlar

- `apperr.FieldError` `Parameter` ve `In` alanlarını kazandı; `httpx.ProblemField` iki biçimi yazar.
- Doğrulamadan sonra `r.Body` yalnız doğrulanmış değeri verir (`ValidatedValue() any`); okumak hata döner. Üretilen handler o değeri bağlar. `runtime.BindJSONBody`'nin `encoding/json`'a geri düşüşü ham baytı okuyamaz, bu yüzden ikinci ve doğrulanmamış bir decode yoktur. Bağlama başarısız olursa yanıt `400 REQUEST_BODY_MALFORMED`'dır.
- kin isteği yerinde değiştirir: gövdeyi okur, şema varsayılanlarını yazar, `r.Body`'yi değiştirir. Faz 1'in idempotency parmak izi bu yüzden ham baytlardan değil, doğrulanmış değerin kanonik JSON'undan (`json.Marshal`, anahtarlar sıralı, varsayılanlar dahil) alınır.
- Authn middleware'i (Faz 1) doğrulamadan önce gelir; kimliksiz istek gövde biçimi hakkında bir şey öğrenmez.
- `httpx.IdempotencyKey` hem çıplak değeri (Stripe) hem taslaktaki tırnaklı structured-field string'i kabul eder ve tırnakları soyar.
- Açık sorular:
  - `santhosh-tekuri/jsonschema/v6`'nın doğrudan bağımlılık olması onay bekliyor (Madde 2).
