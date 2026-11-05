# koala-api: çalışma kuralları

Koala'nın Go backend'i. Java `is-koala/koala-backend`'in baştan yazımıdır (ADR-0001).

Kararların gerekçesi `docs/adr/`'de, plan belgesi `docs/plan/go-gecis-degerlendirme.md`'dedir. Bir kurala itiraz edeceksen önce ilgili ADR'yi oku. Kararı değiştirmek için yeni ADR yazılır; kabul edilmiş ADR düzenlenmez.

Bu dosya kısa tutulur. Bir kural lint ya da testle zorlanabiliyorsa oraya yazılır. Etiketlerin anlamı:
- **[lint: …]**: golangci-lint zorlar.
- **[test]**: bir test zorlar.
- **[make verify: …]**: `make verify` kapısının lint ve test dışındaki bir adımı zorlar (ör. `go mod tidy -diff`).
- Etiketsiz kural: makine zorlamıyor, incelemede bakılır.
- "(öneri)": plan belgesinde yok, iskelet PR'ında önerildi.

## Kapı: CI'da koşar, yerelde ağır iş yok

- Kapı üç adımdır ve **yalnız GitHub Actions'ta** koşar (`.github/workflows/verify.yml`, ADR-0004, ADR-0017):
  1. `make verify`: gofmt → `go mod tidy -diff` → `go vet` → golangci-lint → lint probu → govulncheck → `go test -race -shuffle=on` → spec lint (`api-lint`) → üretilen kod güncel mi (`gen-check`) → `sqlc vet` ve probu (`sqlc-vet`). Bu adımdaki testler DB'siz birim testleridir.
  2. `make test-integration`: service container'daki Postgres'e karşı yalnız `integration` etiketli testler. Önce etiket korumaları koşar.
  3. `make test-conformance` (ayrı `authz-conformance` job'u): `conformance` etiketli uygunluk koşucusu, authz matrisini Go değerlendiricisine ve service container'daki OpenFGA'ya koşturur; fark kırmızıdır (ADR-0024).
- **Bu Mac'te yerelde ağır iş yapılmaz** (Yusuf, 04.11). `go test`, `go vet`, `go build`, golangci-lint, `make verify` ve docker çalıştırılmaz. Serbest olanlar: `gofmt`, `go mod tidy`, `make fmt-check`, `make mod-check`. İş bitince `go clean -cache`.
- PR, CI'daki `verify` yeşil olmadan merge edilmez. Kırmızı kapıyla "sonra düzeltirim" yok.
- `//nolint` yalnız linter adı ve gerekçeyle yazılır: `//nolint:wrapcheck // neden` **[lint: nolintlint]**. Gerekçesiz susturma yerine kodu düzelt.
- Araç sürümleri `Makefile`'da sabittir. Go sürümü desteklenen en yeni Go'dur (`go.mod` `toolchain`); Go, Dockerfile imajı ve golangci-lint birlikte yükseltilir.

## Paket düzeni ve katmanlar (ADR-0002, plan §3.1–3.2)

- `cmd/koala/main.go` incedir: sinyal yakalama ve `app.Run`. Wiring `internal/app/`'te elle yapılır, modül başına bir `wire<Modül>()`. DI kütüphanesi yok (öneri) **[lint: depguard rejected]**.
- `internal/platform/*` alan bilgisi taşımaz ve alan paketlerini import etmez **[lint: depguard platform]**.
- Her alan şu dosyalardan oluşur: `domain.go` (tip, kural, FSM tablosu), `service.go`, `store.go` (sqlc sarmalayıcısı), `http.go` (üretilmiş strict arayüzün uygulaması).
- Bağımlılık yönü `http → service → store`.
  - `domain.go` `net/http`, `encoding/json`, `pgx` ya da chi import etmez **[lint: depguard domain]**.
  - `domain.go`'da JSON struct tag'i bulunmaz **[test: internal/archtest]**.
- Alanlar arası çağrı arayüz üzerinden yapılır. Atomiklik gerekiyorsa tx'e bağlı `db.Querier` parametre olarak geçirilir.

## Tasarım: SOLID/DRY'ın Go karşılığı

- **Arayüz kullanan tarafta tanımlanır** ve küçük tutulur (1–3 metot). Sağlayıcı paket somut tip döner. "Her store'a bir `Store` arayüzü" yazılmaz.
- **Kompozisyon:** Davranış küçük tiplerin birleşiminden çıkar. Struct gömme yalnız gerçekten tüm metot kümesi devralınacaksa kullanılır.
- **Bağımlılıklar açık parametredir:** Yapıcıya (`NewX(dep1, dep2)`) verilir. Paket düzeyinde değişken durum yoktur **[lint: gochecknoglobals, gochecknoinits]**. Varyadik "opsiyonel parametre" ya da setter ile enjeksiyon yapılmaz.
- **İş kuralı tek yerde durur:**
  - para hesabı `money` ve `payment/domain.go`'da,
  - FSM'ler `domain.go`'da tablo olarak,
  - yetki kararı yalnız `authz`'da.

  Aynı kural ikinci bir yerde yazılmaz. Teknik kodda (handler, eşleyici) biraz tekrar, yanlış soyutlamadan iyidir.
- **Zaman** alan kodunda `clock.Clock`'tan alınır, `time.Now()` çağrılmaz **[lint: forbidigo]**.
- **Yapılandırma** yalnız `internal/platform/config`'ten okunur. Ortam değişkeni başka yerde okunmaz **[lint: forbidigo]**.
  - Yeni değişken `KOALA_` önekiyle `known` listesine ve testine eklenir.
  - Bilinmeyen `KOALA_*` değişkeni açılışı durdurur (öneri) **[test]**.

## Para (ADR-0008, K5)

- Tutar `int64` kuruştur; DB'de `bigint` kuruş olarak saklanır, `NUMERIC` kullanılmaz. Para hesabında `float` kullanılmaz.
- API biçimi `{"amount": 15000, "currency": "TRY"}`. Oranlar baz puandır (1250 = %12,5).
- Yuvarlama tek fonksiyondadır (`money.MulRateHalfUp`). `"150.00"` biçimi yalnız Iyzico sınırında üretilir.

## Veri erişimi ve soft-delete (ADR-0012, ADR-0013)

- Uygulama sorguları yalnız `active_<t>` view'larından okur ve yazar; temel tabloya dokunmaz. `koala_app` rolünün temel tablolarda yetkisi yok **[test: TestRoleGrants]**. Uygulama sorgusunda soft-delete'li temel tablo adı ve `deleted_at`/`deletion_id`'ye yazım `sqlc vet`'te kırmızıdır **[make verify: sqlc-vet]**.
- Sorgular sqlc ile yazılır (ADR-0020): uygulama sorguları `db/queries/<alan>/`, `koala_maint` sorguları `db/queries/maint/` → `maintdb`. `maintdb`'yi yalnız soft-delete platformu, `privacy` ve `staff` import eder **[lint: depguard maint]**. Üretilen kod commit edilir **[make verify: gen-check]**.
- Yeni tablo varsayılan olarak soft-delete'lidir: `CREATE TABLE`'ın altına `-- +koala soft-delete v1 <t>` yazılır, `make generate` view'ı ve GRANT'leri üretir. Tablo ve soft-delete'li tabloya giden her FK `db/softdelete.yaml`'da sınıflanır (muafiyet gerekçeli) **[test: TestCatalogMatchesDatabase]**. Silme yalnız `softdelete.Delete` ile, geri alma `softdelete.Restore` ile yapılır; kaskad trigger'la yapılmaz.
- Soft-delete'li bir ebeveyne çocuk satırı ekleyen ya da çocuğun FK'sini başka ebeveyne çeviren kod, aynı tx'te önce `softdelete.LockActiveParent(ctx, q, tablo, id)` çağırır; `ErrNotActive` ebeveynin kaynağına göre 404 ya da 409'a çevrilir. Yoksa eşzamanlı bir silme ile "silinmiş ebeveynin aktif çocuğu" oluşur (ADR-0020).
- Sunucu girişi izin listesiyle denetlenir: üyelikleri tam olarak `{koala_app}`, güçlü özniteliği yok, kendi tablo yetkisi yok. Uymayan girişle havuz bağlantı vermez **[test]**. Sahip, bakım ve bootstrap girişleri süper kullanıcı olamaz **[test]**. `SET ROLE` güvenlik sınırı değildir (ADR-0017).
- **Oturum düzeyinde `SET` yasaktır; yalnız `InTx` içinde `SET LOCAL` kullanılır.** Havuz her edinimde `current_user`'ı denetler, değişmişse bağlantıyı atar **[test]**. Bu yüzden PgBouncer transaction modu kullanılmaz.
- Yeni tablo, view, dizi ya da fonksiyon kendi GRANT'iyle gelir. Fonksiyonlarda `PUBLIC` `EXECUTE` yoktur. KVKK verisi süzen view `WITH (security_barrier)` ile kurulur (ADR-0017); katalogda `pii: true` ⇔ view'da `security_barrier` **[test]**.
- `softdelete.yaml`'da `detach` politikalı FK kenarında ebeveyne yalnız çocuktan `LEFT JOIN active_*` ile ulaşılır; INNER/RIGHT/CROSS/NATURAL join, FROM'da ebeveyn, iç derived tablo, LATERAL, CTE ve IN/EXISTS yarı-join'i yasak **[test: TestQueriesJoinLint]**. Elle yazılmış pgx sorguları taranmaz, incelemede bakılır.
- GET operasyonu okuduğu tabloyu `x-resource` ile adlandırır; soft-delete'li her tablo için fikstür yazılır ve üretilmiş okuma testi (`softdeletetest.ReadTest`) koşar (ADR-0020).
- Temel tablolara yalnız `koala_maint` rolü erişir: geri alma, temizlik, anonimleştirme. M1 tabloları (defter, kanıt) silinmez.

## API (ADR-0005, ADR-0006, ADR-0019, `docs/api-style-guide.md`)

- Sözleşme önce: `api/openapi.yaml` (OpenAPI 3.1) ve `api/components/*.yaml` tek doğruluk kaynağıdır. Akış: spec → `make api-lint` → `make generate` → uygulama.
- Spec kuralları stil rehberindedir ve vacuum'la zorlanır **[make verify: api-lint]**. `scripts/api-lint-probe.sh` her kuralın bağımsız vakasını (kural, JSON yolu) beklentisiyle ve doğru biçimlerin geçtiğini kanıtlar.
- `internal/apiv1/*.gen.go` üretilir ve commit edilir; elle düzenlenmez **[make verify: gen-check]**. `internal/codegentest` üreticinin 3.1 davranışını sabitler. Alan `http.go`'su üretilmiş `StrictServerInterface`'in kendi metotlarını uygular.
- İstek biçimi spec'ten `httpx.ValidateRequests` ile doğrulanır; handler'da biçim kontrolü tekrar yazılmaz. Handler yalnız doğrulanmış değeri alır **[test]**. Authn doğrulamadan önce gelir.
- `make generate` yerelde serbesttir; derleme gerektirdiği için önce yüke bakılır ve `nice -n 15` ile koşulur.
- Servisin döndürdüğü her hata kodu `api/components/problem.yaml`'daki `ErrorCode` listesindedir (platform ve realtime kodları için **[test]**).
- İstemci başlıkları (`X-Client-Platform`, `X-Device-Id`, `X-App-Version`), yol normalizasyonu ve adres başına oran sınırı router'ın kök zincirindedir; WebSocket dahil her istek onlardan geçer. Yeni kapı kök zincire konur, route grubuna değil (ADR-0018 §8, ADR-0021) **[test]**.
- Her route bir oran sınırı grubundadır: operasyonun `x-rate-limit`'i ya da metodun varsayılanı. Grup tablosu `internal/app/edge.go`'dadır (ADR-0021) **[test]**.

## Hatalar (ADR-0007)

- Servisler istemciye dönecek hatayı `apperr.New(kind, "STABLE_CODE")` ya da `apperr.Wrap(cause, kind, code)` ile üretir. Kod UPPER_SNAKE'tir ve anlamı hiç değişmez; OpenAPI'deki kod listesine eklenir.
- Alan paketinde `ErrNotFound`/`ErrForbidden` gibi sentinel tanımlanmaz. HTTP durumu yalnız `httpx.StatusOf` ile belirlenir.
- Dış paketten gelen hata bağlam eklenerek sarılır: `fmt.Errorf("load job %s: %w", id, err)` **[lint: wrapcheck, errorlint]**.
- `*apperr.Error` olmayan hata istemciye `500 INTERNAL` olarak gider, sebebi yalnız log'a yazılır **[test]**. "Bu durumda 409 dönsün" gibi örtük eşleme yazılmaz.
- Gövde decode hatası olduğu gibi döndürülür. `httpx.BodyLimit` aşımı (varsayılan 1 MiB) `413 REQUEST_BODY_TOO_LARGE` olur **[test]**. Daha büyük gövde gereken operasyon spec'te `x-body-limit` yazar; spec dışı route `r.With(httpx.BodyLimit(n))` kullanır.
- Yetkisiz okuma, kaynağın kendi 404'ünü döner. Gövde bayt bayt aynıdır (ADR-0014).

## Transaction (ADR-0012, plan §3.5)

- Tx servis metodu sınırında `db.InTx(ctx, func(ctx context.Context, q db.Querier) error)` ile açılır. Store `Querier` alır, kendi tx'ini açmaz. İç içe `InTx` `ErrNestedTx` döner **[test]**.
- Commit hatası belirsizdir: tx işlenmiş olabilir. Tekrarı güvenli olmayan yazım idempotent yapılır (ADR-0017).
- **Dış çağrı (Iyzico, FCM, SMTP, S3) sırasında tx açık kalmaz.** Akış şöyledir: rezerve et ve commit et → dış çağrıyı yap → sonucu kısa bir tx'te yaz.
- Garantili yan etki iş verisiyle aynı tx'te River `InsertTx` ile kuyruğa girer. Best-effort işler commit sonrası hook'ta çalışır.
- Kilit açık yazılır (`SELECT … FOR UPDATE`). Para yolunda kilit sırası "önce atama, sonra detay"dır.
- Her isteğin context'inde `httpx.RequestTimeout` süre sınırı var. Uzun işler isteğe değil, River işine taşınır.

## Metrik (plan §3.10, ADR-0021)

- Metrikler enjekte edilen `prometheus.Registerer`'a kaydedilir; global varsayılan deftere yazılmaz. `/metrics` iç porttadır (`KOALA_METRICS_ADDR`).
- HTTP etiketi route şablonudur, ham yol değildir. Etiket değeri sonlu bir kümeden gelir; id, e-posta ya da yol etikete yazılmaz.
- Uyarı kuralı `ops/monitoring/prometheus-alerts.yml`'e yazılır; okuduğu metriğin kayıtlı olduğunu test denetler **[test: TestAlertRulesUseRegisteredMetrics]**.

## Log (plan §3.10)

- Logger yapıcıyla enjekte edilir. `*Context` metotları kullanılır; `request_id` context'ten gelir **[lint: sloglint]**.
- Anahtarlar `snake_case`, mesajlar sabit metindir **[lint: sloglint]**.
- PII (TCKN, IBAN, telefon, adres, hassas konum), sır ve token log'a yazılmaz.
- Erişim log'u query string'i hiç yazmaz; path'i yalnız eşleşmeyen istekte (404) yazar **[test]**.

## Test düzeni

- Yalnız stdlib `testing` kullanılır; testify yok (öneri) **[lint: depguard rejected]**.
- Kara kutu testi tercih edilir (`package x_test`). İç fonksiyon testi gerekiyorsa aynı pakette yazılır.
- Testler `t.Parallel()` çağırır **[lint: paralleltest, tparallel]**. Yardımcılar `t.Helper()` çağırır **[lint: thelper]**.
- Testler sıraya bağımlı olmaz. Bir testin geçmesi kardeş testin ısıttığı duruma dayanamaz. CI testleri `-shuffle=on` ile rastgele sırada koşar.
- Tablo güdümlü test tercih edilir.
- Kablo biçimleri (problem+json, para JSON'u, Iyzico gövdeleri) altın testle bayt bayt sabitlenir.
- Servis testleri gerçek PG'de koşar, test başına ayrı DB kullanılır. **Bellek içi store yazılmaz.** Sahte implementasyon yalnız dış entegrasyonlar için yazılır.
  - DB'li test `//go:build integration` etiketi taşır ve DB'yi `dbtest.New(t)` ile alır: template'ten klonlanmış, rol başına girişli. CI'da `make test-integration` koşar (ADR-0017).
- Zamana bağlı testler sahte saat kullanır. `time.Sleep` ile yarış beklenmez; koşul döngüyle ve süre sınırıyla beklenir. Sunucu ya da goroutine başlatan test kendini `t.Cleanup` ile kapatır.
- Para, authz ve KVKK kuralları için senaryo kataloğundaki (`test/scenarios/`) P0 satırının bir Go testi olur (plan §6.2).

## Bağımlılıklar (ADR-0003)

- Yalnız plan §3.3'teki onaylı listeden alınır ve yalnız onu gerçekten kullanan PR'da eklenir **[make verify: go mod tidy -diff]**.
- Liste dışı her bağımlılık ayrı onay ister. PR gövdesine lisans, gerekçe ve alternatif yazılır.
- Eklenen ya da yükseltilen sürüm, yayımlanalı en az 14 gün olmuş en yeni sürümdür; istisna govulncheck bulgusunu kapatan yamadır (ADR-0017).
- Reddedilen kütüphaneler depguard'ın `rejected` listesindedir **[lint]**.

## Git ve PR

- `main`'e doğrudan commit yapılmaz. Her iş `origin/main` tabanlı ayrı bir worktree ve dalda yapılır. Önce `git fetch origin --prune`; worktree `~/Projects/koala/.worktrees/api-<konu>` altında açılır, dal `faz<n>/<konu>` biçimindedir.
- Dosyalar tek tek stage edilir. `git add .` ya da `-A` kullanılmaz. `--no-verify` ve `reset --hard` kullanılmaz.
- Commit mesajı İngilizce ve conventional commit biçimindedir (`feat(httpx): …`, `docs(adr): …`).
- PR gövdesi Türkçedir ve şunları içerir:
  - ne yapıldığı,
  - kapının CI'da nerede okunacağı,
  - planın ilgili bölümleri,
  - açık sorular.
- PR incelemeden merge edilmez. Para, kripto (NFC) ve authz PR'larında ikinci göz zorunludur.
- Sır ya da env **değeri** hiçbir dosyaya, commit'e ya da PR'a yazılmaz; yalnız değişken adları yazılır.
