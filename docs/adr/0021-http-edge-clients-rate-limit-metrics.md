# ADR-0021: HTTP kenarı: istemci başlıkları, oran sınırı, yol normalizasyonu ve metrikler

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K12 (hız sınırı), §0.1 (426 → ADR-0019)
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §3.9, §3.10, §3.11, §4.1-7, §6.1 (Faz 1 ve Faz 2 çıkış kriteri), §9 R9; N01 §1.6, §4.5, §6, §8.2; [ADR-0005](0005-api-contract-principles.md) madde 7, [ADR-0018](0018-realtime-ticket-and-ws-protocol.md) §8–9, [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) Karar 6
- Kod: `internal/platform/httpx` (`router.go`, `clients.go`, `path.go`, `clientip.go`, `ratelimit.go`, `metrics.go`), `internal/platform/metrics`, `internal/platform/appversion`, `internal/platform/jsontime`, `internal/app/edge.go`, `ops/monitoring/prometheus-alerts.yml`

## Bağlam

Faz 1'in middleware zincirinde (§6.1) sürüm kapısı, oran sınırı, yol normalizasyonu ve metrik eksikti. Java'da bunlar şöyle çalışıyor:
- **Sürüm kapısı** (`MinimumAppVersionFilter`): `X-Client-Platform`'a göre platform eşiği. Başlıksız istek kapıdan geçiyor; eşik `Settings` tablosundan da okunuyor.
- **Oran sınırı** (`RateLimitFilter`, N01 §4.5): 8 kova, hepsi IP anahtarlı, çünkü filtre kimlikten önce koşuyor. AUTH kovasının anahtarı yol + IP + gövdedeki hedefin SHA-256'sıdır. Bellek içi, tek örnek; 10 dakikada bir süpürme.
- **İstemci adresi** (`ClientIpResolver`): `app.trusted-proxies`'ten gelen proxy'ler için önce RFC 7239 `Forwarded`, sonra `X-Forwarded-For` okunuyor.
- **Yol**: Spring'in `StrictHttpFirewall`'ı kodlanmış `/`, `.` ve `%` içeren yolu reddediyor. `/uploads/**` güvenliği buna dayanıyor (N01 §8.2).
- **Metrik**: Micrometer, `/actuator/prometheus` marketplace ADMIN token'ı arkasında. `ops/monitoring/prometheus-alerts.yml`'de 19 kural var (plan 18 diyor; origin/main'de 19).

ADR-0018 §8 bir tuzağı kayda geçirdi: chi'de bir route ebeveyninin middleware'inden çıkamaz. `Router.Stream` ile bağlanan WebSocket ucu yalnız kök zinciri görür. Kök zincire konmayan bir kapıyı WS atlar.

## Karar

### 1. Zincir

```
kök:     RequestID → route araması → SecurityHeaders → AccessLog → metrik → Recover
         → CanonicalPath → istemci adresi → istemci başlıkları → adres başına oran sınırı
sınırlı: kök + Timeout → BodyLimit → (API grubu: authn [Faz 1] → kimlik başına oran sınırı → doğrulama)
stream:  kök
```

- İstemci başlıkları kapısı ve adres başına oran sınırı **kök zincirdedir**: WebSocket handshake'i, bilinmeyen yollar ve 405'ler de onlardan geçer.
- İkisi yönlendirmeden önce koştuğu için hangi route'a gidildiğini bilmeleri gerekir. Router isteğin route şablonunu en başta bir kez arar (`chi.Mux.Find`, chi'nin yönlendirdiği yolun aynısıyla). Erişim log'u, metrik etiketi ve problem `instance`'ı da reddedilen istekte bu şablonu kullanır. Bunun dışında yolun ham hâli hiçbir yere yazılmaz.
- `NewRouter` artık `RouterOptions` ister; `ClientIPs`, `RateLimit` ve `Metrics` zorunludur. Sıfır değerle korumasız bir router kurulamaz.

### 2. İstemci başlıkları ve sürüm kapısı

`X-Client-Platform` hangi kuralın uygulanacağını belirler. Değerler sözleşmenin enum'udur (`IOS`, `ANDROID`, `WEB`, `ADMIN`); büyük-küçük harf duyarlıdır.

| Route | `X-Client-Platform` | `X-Device-Id` | `X-App-Version` |
|---|---|---|---|
| Eşleşmeyen yol, `/livez`, `/readyz`, `ClientPolicy.ExemptRoutes` | okunmaz | okunmaz | okunmaz |
| `/v1/admin…` | yok ya da `ADMIN` | okunmaz | okunmaz |
| Diğer her şey | `IOS`, `ANDROID` | zorunlu | zorunlu, ≥ `KOALA_MIN_APP_VERSION` |
| | `WEB` | zorunlu | okunmaz |
| Stream route'unda `Origin`'li WebSocket upgrade'i, platform başlığı yok | `WEB` sayılır | isteğe bağlı | okunmaz |

- **Admin.** Personel API'si `/v1/admin` altında ayrı zincirdir (§3.3). Panel sunucu tarafından çağırır ve bilerek cihaz ya da sürüm göndermez (koala-admin `docs/server-only-guard.md`). `ADMIN` değeri yalnız orada geçerlidir. `/v1/admin` dışında `ADMIN` 400 alır; aksi hâlde başlık, cihaz ve sürüm kuralını atlatmanın yolu olurdu.
- **Web.** Web istemcisi BFF'tir; API ile birlikte yayımlanır ve tarayıcı profiline ait `X-Device-Id`'yi zaten gönderir (koala-web `AGENTS.md`). Sürüm kapısı mağazada kalan eski derleme içindir; web'e uygulanırsa yalnız kademeli yayın sırasındaki eski BFF örneğini kilitler. Java'da web için ayrı bir eşik vardı; bu karar onu bırakır.
- **Tarayıcıdan WebSocket.** Tarayıcının WebSocket API'si başlık gönderemez. Stream route'una `Origin` ve `Upgrade: websocket` ile gelen, platform başlığı taşımayan istek web istemcisi sayılır. Cihaz bağı biletten gelir (ADR-0018 §2, ADR-0022). Yerel istemcinin handshake'i REST'teki kuralın aynısına uyar; eski mobil derleme WS'ye de bağlanamaz.
- **Sürüm.** Semver karşılaştırılır (`internal/platform/appversion`). Pre-release kendi sürümünden önce gelir; `+build` sırayı etkilemez. `IOS`/`ANDROID` için sürüm yoksa ya da okunamıyorsa da `400 APP_VERSION_UNSUPPORTED` + `X-Minimum-App-Version` döner (ADR-0019 Karar 6). Eski derleme önce güncellenmesi gerektiğini öğrenir; bu yüzden sürüm, cihaz başlığından önce denetlenir.
- **Başlık hataları** yeni kod üretmez: `400 REQUEST_INVALID` + `{parameter, in: HEADER, code}` (ADR-0019 Karar 4). Eksik başlık `FIELD_REQUIRED`, biçimi tutmayan `FIELD_VALUE_INVALID` alır.
- **Cihaz kimliği** `[A-Za-z0-9._:-]{8,128}` biçimindedir. Bu Java'nın kuralıdır; mobil (32 karakter) ve web (32 hex) zaten buna uyuyor. Başlık yalnız biçim olarak denetlenir; değer `httpx.DeviceIDFrom(ctx)` ile okunur. Oturumun cihazıyla eşleşmesini Faz 1'in authn PR'ı kurar.
- **Minimum sürüm** `KOALA_MIN_APP_VERSION`'dan gelir. `serve` production'da onsuz açılmaz; ayarsız bir kapı her derlemeyi sessizce geçirirdi. Diğer ortamlarda varsayılan `0.0.0`'dır. Java'daki `Settings` tablosundan okuma yoktur: eşik yayınla birlikte değişir.
- **Muaf route'lar.** Koala uygulaması olmayan çağıranlar (ödeme sağlayıcısının callback'i, e-posta takip pikseli) `ClientPolicy.ExemptRoutes`'a route şablonuyla, kendi testiyle eklenir. Şimdilik liste boştur. Sağlık uçları her zaman muaftır.

### 3. Yol: yeniden yazılmaz, reddedilir

`CanonicalPath` yönlendirmeden önce şunları `400 PATH_INVALID` ile reddeder:
- `/` ile başlamayan yol (`OPTIONS *`),
- boş parça (`//`); sondaki tek `/` yönlendirmeye kalır ve 404 alır,
- `.` ya da `..` parçası, kodlanmış hâlleri dahil (`%2e`, `.%2E`),
- kodlanmış `/`, `\` ya da `%` (`%2f`, `%5c`, `%25`): çözülünce bir parçayı ikiye bölerler ya da sonraki bir katman bir kez daha çözer,
- çözülmüş yolda `\`, kontrol karakteri ya da geçersiz UTF-8.

Gerekçe:
- Hiçbir Koala istemcisi böyle yol üretmez.
- Yeniden yazma (Go `ServeMux`'un temizleyip 301'lemesi gibi), istemci yolu seçtikten sonra hangi route'un ve hangi yetki kuralının uygulanacağını değiştirir. POST'u 301'lemek de istemcide metodu GET'e çevirir.
- Spring `StrictHttpFirewall`'ın reddettiği küme budur. Java bugün `/uploads/**` güvenliği için ona dayanıyor.

Fuzz testi (`FuzzCanonicalPath`) şu özelliği sabitler: geçen her istek hedefinin çözülmüş yolu kanoniktir ve bir depolama önekine eklendiğinde önekin dışına çıkamaz. CI tohum korpusunu `go test` ile koşar. Bu, Faz 2'nin "yol geçişi fuzz testi yeşil" çıkış kriterinin temelidir.

### 4. İstemci adresi

- `X-Forwarded-For` yalnız TCP eşi `KOALA_TRUSTED_PROXIES` içindeyse okunur.
- Okunurken sağdan sola yürünür. Her güvenilir proxy kendisine bağlanan adresi sona ekler; güvenilir olmayan ilk girdi istemcidir. Onun solundakileri istemci yazmıştır ve hiç okunmaz.
- Bütün girdiler güvenilirse en soldaki istemcidir (iç çağıran). Bozuk girdi yürüyüşü durdurur ve son güvenilir atlama kullanılır. Bozuk bir başlık istemciyi en fazla proxy gibi gösterebilir, istediği bir adres gibi gösteremez.
- **RFC 7239 `Forwarded` okunmaz.** Java onu `X-Forwarded-For`'dan önce okuyordu. Traefik `X-Forwarded-For`'u kendisi yazar, `Forwarded`'ı ise olduğu gibi geçirir; bu durumda başlık istemcinin elindedir (plan: doğrulanmadı, Java için ayrı bulgu).
- `KOALA_TRUSTED_PROXIES` production'da `serve` için zorunludur. Ayarsız kalırsa bütün istemciler proxy'nin tek adres kovasını paylaşır.

### 5. Oran sınırı: kova modeli

**Anahtar** `(grup, tür, değer)` üçlüsüdür. Üç tür vardır:

| Tür | Değer | Nerede |
|---|---|---|
| `ip` | İstemci adresi; IPv6 için /64 (bir ev ya da cihaz genelde koca bir /64 alır; tek adres anahtarı sınırsız kova üretirdi) | Kök zincir, yönlendirmeden önce; her istek |
| `identity` | Kimliği doğrulanmış kullanıcı ya da personel (`WithRateLimitIdentity`, authn PR'ı koyar) | API grubu, authn'den sonra (`ByIdentity`) |
| `custom` | Modülün seçtiği anahtar, ör. giriş hedefinin hash'i | Modül `RateLimiter.Allow(grup, KeyCustom, anahtar)` çağırır |

**Grup** route'tan gelir. Operasyon `x-rate-limit: <grup>` uzantısıyla bir gruba girer. Uzantı yoksa GET, HEAD ve OPTIONS `read`, diğer her metot `write` grubundadır. Hiçbir route unutularak sınırsız kalmaz. Bilinmeyen grup adı açılışı durdurur.

**Sınırlar.** Kimlik başına sınırlar Java'nın değerleridir. Adres başına sınırlar 5 katıdır, çünkü mobil operatörün NAT'ı birçok kullanıcıyı tek adresin arkasına koyar.

| Grup | Adres başına (dk / burst) | Kimlik başına | Özel anahtar | Java karşılığı |
|---|---|---|---|---|
| `read` | 600 / 120 | 300 / 60 | — | PUBLIC 120/dk |
| `write` | 300 / 60 | 60 / 20 | — | WRITE 60/dk |
| `auth` | 30 / 10 | — | 10 / 5 | AUTH 10/dk (yol + IP + hedef) |
| `upload` | 100 / 20 | 20 / 5 | — | UPLOAD 20/dk |
| `message` | 150 / 30 | 30 / 10 | — | MESSAGE 30/dk |
| `sitemap` | 30 / 10 | — | — | SITEMAP 30/dk |
| `boarding` | 600 / 60 | 120 / 20 | — | BOARDING 120/dk |

- Realtime bileti kendi kullanıcı başına sınırını korur (ADR-0018 §2).
- `TestRateLimitGroups` tabloyu sabitler.
- Aşımda `429 RATE_LIMITED` döner. `Retry-After`, kovanın bir sonraki jetonuna kalan süredir; saniyeye yukarı yuvarlanır, en az 1'dir. Reddedilen istek jeton harcamaz.

**Bellek.**
- Kovalar bellekte tutulur (`golang.org/x/time/rate`). Üst sınır `MaxKeys` = 100 000'dir; kova başına ~250 bayttan en kötü durumda ~25 MB eder.
- Yeni anahtar eklenirken kova sayısı eşiği geçtiyse dolu kovalar (yani boşta kalmış, yeni açılsa aynı davranacak olanlar) silinir. Eşik amortize edilir.
- Silmeden sonra hâlâ üst sınırdaysa kovalar rastgele atılarak %90'a inilir ve `koala_http_rate_limit_evicted_total` artar. Map sırası rastgele olduğundan, bir anahtar selinde atılanların çoğu selin kendisidir. Atılan kova sıfırlanmış olur (o anahtar için fail-open).
- Ayrı bir süpürme goroutine'i yoktur; trafik yoksa bellek de büyümez.

**Çok örnek: bilinçli sınır.**
- Kovalar süreç içidir. N örnek round-robin'le çalışırsa etkin sınır N katına çıkar. Tek örnek varsayımı plan §3.9 ve §9 R9'dadır.
- İkinci örnek açılmadan önce iki yol vardır: proxy'de adrese göre yapışkan dağıtım (adres kovası doğru kalır, kimlik kovası kalmaz) ya da paylaşılan sayaç (Postgres'te kova tablosu; onaylı listede Redis yok).
- Karar o gün verilir. Realtime'ın fanout sınırıyla (ADR-0018 §9) aynı anda ele alınır.

### 6. Metrikler

- `/metrics` ayrı bir iç dinleyicidedir (`KOALA_METRICS_ADDR`, varsayılan `:9090`; API portuyla aynı olamaz) ve proxy'ye açılmaz (§3.10).
- Kayıt defteri `metrics.NewRegistry` ile kurulur ve aşağı geçirilir. `prometheus` paketinin global varsayılan defterine hiçbir şey yazılmaz.
- Metrik dinleyicisi API'nin drain'i boyunca açık kalır ve en son kapanır; drain'in kendisi de gözlenebilir.

| Metrik | Tür | Etiketler | Not |
|---|---|---|---|
| `http_server_request_duration_seconds` | histogram | `http_request_method`, `http_route`, `http_response_status_code` | OpenTelemetry HTTP semconv adı ve kovaları. `http_route` route şablonudur, ham yol değildir (kardinalite); eşleşmeyen istekte boştur. Bilinmeyen metot `_OTHER`'dır. Panic'le kesilen yanıt 500 sayılır. |
| `koala_http_rate_limited_total` | counter | `group`, `key` | Bilinen grup/tür çiftleri sıfırdan başlar |
| `koala_http_rate_limit_keys` | gauge | — | Bellekteki kova sayısı |
| `koala_http_rate_limit_evicted_total` | counter | — | Üst sınırdan atılan kovalar |
| `koala_http_app_version_rejected_total` | counter | `platform` | Sürüm kapısının reddettiği istekler |
| `koala_build_info` | gauge | `version` | Her zaman 1 |
| `go_*`, `process_*` | — | — | Go runtime ve süreç collector'ları |

Plan §3.10'daki diğer adlar kendi PR'larında gelir:
- `koala_job_failures_total{kind}`: River worker'larının sunucuya bağlandığı Faz 1 PR'ında,
- `koala_payment_operations_total{operation,outcome}` ve payout gauge'ları: Faz 6'da, adları ve etiketleri Java'daki gibi.

**Uyarılar.** `ops/monitoring/prometheus-alerts.yml`, koala-backend'deki dosyanın yerine geçer. `TestAlertRulesUseRegisteredMetrics`, bir kuralın okuduğu her metriğin servis tarafından kaydedildiğini denetler. Servis dışı metrikler (yedek betiği, blackbox) testte ayrıca listelenir. Böylece hiç var olmayan bir seri üzerindeki kural sessizce yeşil kalamaz; metriği olmayan kural dosyaya giremez.

| Java kuralı | Karşılığı |
|---|---|
| `KoalaHighHttp5xxRate` | Aynı adla, `http_server_request_duration_seconds_count{http_response_status_code=~"5.."}` üzerinde |
| `KoalaPaymentIntegrationFailure`, `KoalaPaymentInitiationAbandoned`, `KoalaPaymentFailureSpike`, `KoalaPayoutSettlementEvidenceMismatch` | Faz 6: `koala_payment_operations_total`, aynı ifade |
| `KoalaPayoutSettlementOverdue`, `KoalaPayoutSettlementStalled` | Faz 6: aynı adlı gauge'lar |
| `KoalaScheduledJobFailure` | River worker PR'ı: `koala_job_failures_total{kind}` |
| `KoalaNotificationDispatchBacklog`, `…OldestAge`, `KoalaUserProfileSyncBacklog`, `…OldestAge` | Outbox River `InsertTx` ile kalkar (K3); karşılığı River kuyruk birikimi ve en eski işin yaşı, River worker PR'ında adlandırılır |
| `KoalaNotificationDispatchTerminalBrokerFailure`, `KoalaUserProfileSyncTerminalBrokerFailure`, `KoalaRabbitDeadLetterQueue` | Rabbit yok; terminal hata River'da işin `discarded` olmasıdır: `koala_job_failures_total` |
| `KoalaRabbitConsumerLag`, `KoalaNotificationDeliveryParkingQueue` | Kalkar: Rabbit ve park kuyruğu yok (K3) |
| `KoalaBackupStale`, `KoalaCertificateExpiring` | Aynen taşındı (servis dışı metrikler) |

Yeni kurallar:
- `KoalaRateLimitBucketsEvicted`: kova üst sınırı aşıldı.
- `KoalaRateLimitedSpike`: bir grupta saniyede 1'den fazla 429.

### 7. Anlık zaman tek yerde UTC yazılır

Sözleşmedeki `Timestamp` şeması `x-go-type: jsontime.UTC` ile üretilir. Bu tip her zaman UTC ve `Z` ile yazılır, okurken de `Z` ister. Handler başka dilimde bir zaman verse de kablo biçimi değişmez; kural her çağıranın `.UTC()` çağırmasına bağlı kalmaz (issue #7). Realtime olay zarfının `occurredAt`'i ve biletin `expiresAt`'i de aynı tiptir. `OffsetDateTime` (ilan saatleri) `time.Time` kalır; ofsetin anlamı vardır.

### 8. Nesnesiz kimlikli uçlar: `x-authz-exempt`

`koala-operation-authz` kuralı üçüncü bir biçim kabul eder: `x-authz-exempt: <en az 20 karakter gerekçe>`. Bu biçim kimlik isteyen (`security` boş değil) ama yetkilendirilecek nesnesi olmayan uç içindir; ör. kendi oturumu için bilet. `x-authz` ve `security: []` ile birlikte kullanılamaz. Lint probu üç yanlış biçimi ve bir doğru biçimi sabitler. Realtime uçları bu biçimle sözleşmeye girdi (ADR-0022).

## Sonuçlar

- Yeni kodlar `ErrorCode` listesindedir: `PATH_INVALID`, `RATE_LIMITED`, realtime kodları ve `CONVERSATION_NOT_FOUND`. `TestPlatformCodesAreInTheContract` httpx ve realtime kodlarını birlikte denetler.
- Yeni değişkenler: `KOALA_MIN_APP_VERSION`, `KOALA_TRUSTED_PROXIES` (ikisi production'da `serve` için zorunlu), `KOALA_METRICS_ADDR`. Yayın hattı (Dokploy) bu adları tanımlamalıdır; değerleri burada yazılmaz.
- `prometheus/client_golang` v1.25.0 eklendi (onaylı liste, Apache-2.0; yayımlanalı 28 gün). Dolaylı bağımlılıkları da beraber geldi: `client_model`, `common`, `procfs`, `beorn7/perks`, `cespare/xxhash`, `munnerz/goautoneg`, `google.golang.org/protobuf`, `x/sys`.
- Authn PR'ının yapacakları:
  - `WithRateLimitIdentity` çağırmak,
  - `DeviceIDFrom`'u oturumun cihazıyla eşlemek,
  - auth uçlarına `x-rate-limit: auth` yazmak ve hedef anahtarını `KeyCustom` ile denetlemek.
- **Plandan sapmalar:**
  - Plan §4.1-7 ve §3.9 426 diyor; bu kayıt ADR-0019'u izler (400).
  - Web için sürüm eşiği kalktı (Madde 2).
  - `Forwarded` başlığı okunmuyor (Madde 4).
