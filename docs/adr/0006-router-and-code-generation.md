# ADR-0006: Router ve sözleşmeden kod üretimi: chi + oapi-codegen

- Durum: Kabul edildi; oapi-codegen + 3.1 spike'ı: çalışıyor (05.11.2026, "Spike sonucu")
- Tarih: 04.11.2026
- Karar kaydı: K8, K9
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K8–K9, §3.3, §9 R4; N05 A5-7

## Bağlam

§0.1'deki ilk öneri stdlib `ServeMux`'tu. core-backend fiber kullanıyor; fiber fasthttp tabanlı ve `net/http` ekosisteminden (otel, standart middleware, `httptest`) kopuk (N05 A5-7).

Koala'nın iki ayrı middleware zinciri var: marketplace ve `/v1/admin`. Bu zincirleri route gruplarıyla ayırmak gerekiyor.

Sözleşme önce yazılacağı için (ADR-0005) handler imzaları spec'ten üretilmeli. Ancak oapi-codegen'in OpenAPI 3.1 desteği doğrulanmadı (§9 R4).

## Karar

1. **Router: `github.com/go-chi/chi/v5` (K8).** Gerekçe:
   - `net/http` ile uyumlu.
   - Go üretim servislerinde en yerleşik router.
   - Route grupları `/v1/admin` zincirini ayırmaya yetiyor.

   §0.1'deki stdlib önerisinin yerine geçer.
2. **Kod üretimi: oapi-codegen, `chi-server` hedefi, strict server (K9).** §3.3'teki `std-http` ifadesi K8 ile geçersizdir. Biçim doğrulaması spec'ten kin-openapi middleware'iyle yapılır. Etiketlerle ikinci bir kopya tutulmaz.
3. **3.1 desteği Faz 0 spike'ında doğrulanır.** Eksikse sırayla şu alternatiflere bakılır: ogen; spec'i 3.0.3 uyumlu alt kümede tutmak; istemci tarafında 3.1 araçları (§9 R4).

## Sonuçlar

- `internal/platform/httpx.NewRouter` bir `*chi.Mux` döner (somut tip; arayüzü tüketen taraf tanımlar). Platform middleware zinciri ve sağlık uçları üzerinde kuruludur. Modüller route'larını buna bağlar. Gövde sınırını yükseltmesi gereken route `r.With(httpx.BodyLimit(n))` kullanır.
- chi'nin özel 405 işleyicisi `Allow` başlığını yazmıyor. `httpx` bunu `Routes.Match` ile hesaplıyor.
- Erişim log'unda `route` olarak chi'nin route şablonu (`/v1/jobs/{id}`) kullanılır. HTTP metriklerinin `http_route` etiketi de aynı değeri alacak (§3.10).
- Spike sonucu bu ADR'ye "çalışıyor" ya da "alternatif seçildi: ADR-NNNN" notuyla işlenir (§6.1 Faz 0 çıkış kriteri).

## Spike sonucu (05.11.2026)

**Sonuç: çalışıyor.** oapi-codegen v2 + OpenAPI 3.1 + chi strict server ve kin-openapi istek doğrulaması 3.1 spec'le çalışıyor. Karar 2 geçerlidir; ogen'e ya da 3.0.3 alt kümesine geçilmez.

### Sürümler

| Bileşen | Sürüm | Nerede |
|---|---|---|
| oapi-codegen (araç) | v2.8.2 (31.10.2026) | `internal/apiv1/generate.go`, `go run …@v2.8.2` |
| `github.com/oapi-codegen/runtime` | v1.8.0 | `go.mod` |
| `github.com/getkin/kin-openapi` | v0.152.0 | `go.mod` |
| chi | v5.5.0 | `go.mod` (değişmedi) |
| Go | 1.27.3 | `go.mod` `toolchain` |

### Ne denendi

1. **Kalıcı:** `api/openapi.yaml` (`openapi: 3.1.1`) + beş bileşen dosyası (`api/components/`) ve tek operasyon: `POST /v1/meta/ping`. Gövdesi ortak `Money`'yi kullanıyor, hataları ortak `Problem`'i dönüyor. Üretilen strict handler `internal/app/api.go`'da `httpx` router'ına bağlandı; testleri `internal/app/api_test.go` ve `internal/platform/httpx/validate_test.go`'da.
2. **Geçici (repo'ya girmedi):** ayrı bir deneme spec'i: `webhooks`, `oneOf` + `const`, `prefixItems`, `oneOf: [{$ref}, {type: "null"}]`, `type: [array, "null"]`, header ve dizi query parametreleri, `$ref` yanında kısıt. Üretilen kod derlendi ve JSON gidiş-dönüşü denendi.

### Ne çalıştı

- 3.1 belge hatasız yükleniyor ve üretiliyor. `type: [T, "null"]` pointer olarak üretiliyor; alan zorunluysa `omitempty` yok, yani null yazılıyor.
- `$ref` yanındaki `description` ve `examples` üretilen koda yorum olarak geçiyor. kin-openapi 3.1'de `$ref` yanındaki kısıtları da (ör. `maximum`) uyguluyor.
- `oneOf: [{$ref: X}, {type: "null"}]` `*X` oluyor. `type: string` + `const` tek değerli enum oluyor. `webhooks` gövde tipini üretiyor.
- Çok dosyalı spec: her bileşen dosyası kendi `go:generate` satırıyla aynı pakete üretiliyor; kök spec `import-mapping: {"./components/x.yaml": "-"}` ile onlara bağlanıyor.
- Strict server + chi: üretilen route'lar `httpx.NewRouter`'ın middleware zincirinin içinde; 404/405 yine problem+json.
- kin-openapi `openapi3filter` 3.1 belgede JSON Schema 2020-12 doğrulayıcısını kullanıyor. `$ref` içeren ve 2020'ye özgü anahtar kullanmayan şemada OpenAPI 3.0 doğrulayıcısına düşüyor; iki yol aynı sonucu farklı hata biçimiyle veriyor. `httpx.ValidateRequests` ikisini de `{pointer, code}` alan hatasına çeviriyor; iki biçim de testle sabit.
- `KeepValidatedValue` + `runtime.BindJSONBody`: handler, doğrulanan değeri bağlıyor; gövde ikinci kez çözülmüyor. `encoding/json`'ın büyük-küçük harf duyarsız eşlemesiyle şemayı atlatma (`"Note"` → `Note`) kapandı (test: `TestPing`).

### Ne çalışmadı ya da sınırlı

| Bulgu | Etkisi | Ne yapıldı |
|---|---|---|
| `oneOf` + `const` ile yazılmış enum düz `string` üretiliyor (README destekliyor diyor, sabit üretmiyor). | Go'da enum sabiti yok. | Enum'lar `enum` ile yazılır (stil rehberi §9). |
| `prefixItems` `[]interface{}` üretiyor. | Tipsiz dizi. | Kullanılmıyor. |
| kin-openapi `uuid` biçimini varsayılan olarak doğrulamıyor. | Bozuk kimlik doğrulamadan geçerdi. | `ValidateRequests` yalnız kendi seçeneklerine bir uuid doğrulayıcısı veriyor; kin'in global tablosuna dokunulmuyor. |
| `format: int64` sınırı float karşılaştırmasıyla denetleniyor: 2^63 geçiyor. | Bağlama hata veriyor. | 400 `REQUEST_BODY_MALFORMED` döner (test). |
| 2020 doğrulayıcısı eksik ve fazla üyeyi alanın yolunda değil, iletisinde adlandırıyor. | Ad iletiden çıkarılıyor; kırılgan. | Testler ileti biçimini sabitliyor; kin yükseltmesi biçimi değiştirirse CI kırmızı olur. |
| Doğrulayıcı iletileri istek değerlerini içeriyor (kin-openapi belgeli). | PII sızıntısı riski. | İletiler yanıta ve log'a yazılmaz; yalnız pointer ve kod çıkar (test: `TestValidateRequestsNeverEchoesValues`). |
| Yanıt doğrulaması middleware ile yapılamıyor (oapi-codegen #1038). | Sunucu sözleşmeye aykırı yanıt dönebilir. | Strict server yanıt tiplerini derleme zamanında sınırlıyor; gerekirse testte `openapi3filter.ValidateResponse`. |
| İçerik tipi yanlışsa 415 değil 400 dönüyor (`header:Content-Type`). | — | `apperr`'de 415 `Kind`'ı yok; açık soru. |

**ogen** denenmedi: engelleyici bir eksik çıkmadı (Karar 3'teki alternatife gerek kalmadı).

### Üretilen kod commit edilir

- `internal/apiv1/*.gen.go` commit edilir. Go'da yerleşik pratik budur: `go generate` "üretilen dosya paketle birlikte dağıtılır, `go build` üreticiyi gerektirmez" diye tasarlandı; protobuf, sqlc ve Kubernetes kodu da böyle. Getirisi:
  - PR diff'inde handler imzasının değiştiği görünür,
  - `go build`, IDE ve golangci-lint üretici olmadan çalışır,
  - üretici sürümü değişince fark tek commit'te incelenir.
- CI'da güncellik `make gen-check` ile denetlenir: `go generate` koşar, `internal/apiv1`'de fark ya da izlenmeyen dosya varsa kapı kırmızı olur. `make verify`'ın parçasıdır.

### Araç sabitleme (ADR-0004'ün ölçütü)

- oapi-codegen `go.mod`'a girmez. `internal/apiv1/generate.go`'daki `//go:generate -command oapi-codegen go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2` satırı sürümü tek yerde tutar. `go run pkg@version` aracın bağımlılıklarını (speakeasy-api/openapi, x/tools, kin-openapi'nin eski sürümü) uygulamanın modül grafiğine karıştırmaz; kaynak sum.golang.org ile doğrulanır. Runtime (`oapi-codegen/runtime`) ve kin-openapi uygulamanın bağımlılığıdır ve `go.mod`'dadır; oapi-codegen yükseltilirken runtime da aynı PR'da yükseltilir.
- vacuum golangci-lint gibi `go install` ile `./bin/vacuum-<sürüm>`'e kurulur; sürüm `Makefile`'da (`VACUUM_VERSION`).

### Spec lint: vacuum

- **Seçilen: vacuum v0.36.2** (`github.com/daveshanley/vacuum`, MIT). Gerekçe:
  - Go ile yazılı; Node gerektirmez ve ADR-0004'ün `go install` akışına uyar. Spectral Node (npm) gerektirir.
  - Spectral kural biçimini okur: `extends: [[spectral:oas, recommended]]` ve çekirdek fonksiyonlar (`pattern`, `casing`, `schema`, `defined`, …). Kural seti ileride Spectral'a taşınabilir.
  - OpenAPI 3.1'i destekler.
- Kurallar: `api/ruleset.yaml` (kök spec: Spectral önerilen seti + Koala kuralları) ve `api/ruleset-schemas.yaml` (şema kuralları; her bileşen dosyasında ayrıca koşar). Her kuralın açıklaması stil rehberindeki maddeye atıf yapar.
- vacuum'da bulunan iki sınır: `field: "@key"` desteklenmiyor (anahtar kuralları `…[*]~` seçicisiyle yazıldı); bazı `field` aramaları yanlış düğüme bakıyor (o kurallar `schema` fonksiyonuyla yazıldı). Sessizce hiçbir şeye eşleşmeyen kural kapıyı yeşil gösterirdi. Bu yüzden `scripts/api-lint-probe.sh`, her Koala kuralının `api/testdata/lint-probe.yaml`'da tetiklendiğini kanıtlar (golangci-lint probunun karşılığı).
- `make api-lint` ve `make gen-check` `make verify`'a eklendi; CI'da `make verify` adımında koşarlar (`verify.yml` değişmedi).

### Eklenen bağımlılıklar

Onaylı listeden (plan §3.3, ADR-0003): `github.com/getkin/kin-openapi` v0.152.0 (MIT), `github.com/oapi-codegen/runtime` v1.8.0 (Apache-2.0). Dolaylı: `apapsch/go-jsonmerge/v2` (MIT), `go-openapi/jsonpointer` ve `go-openapi/swag/jsonname` (Apache-2.0), `google/uuid` (BSD-3, listede), `oasdiff/yaml` ve `oasdiff/yaml3` (MIT), `santhosh-tekuri/jsonschema/v6` (Apache-2.0), `kr/pretty` (MIT), `golang.org/x/text` (BSD-3, listede). kin-openapi'nin `routers/gorillamux` paketi kullanılmadığı için gorilla/mux grafiğe girmedi: operasyon chi'nin eşleştirdiği route şablonundan bulunur. `oapi-codegen/nethttp-middleware` listede olmadığı için alınmadı; aynı iş `httpx.ValidateRequests`'te ~80 satır.
