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

- Kapı `make verify`'dır ve **yalnız GitHub Actions'ta** koşar (`.github/workflows/verify.yml`, ADR-0004). Sırası: gofmt → `go mod tidy -diff` → `go vet` → golangci-lint → lint probu → govulncheck → `go test -race -shuffle=on`.
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

- Uygulama sorguları yalnız `active_<t>` view'larından okur ve yazar; temel tabloya dokunmaz. Faz 1'de `koala_app` rolünün temel tablolarda yetkisi olmayacak ve `sqlc vet` bunu zorlayacak.
- `softdelete.yaml`'da `detach` politikalı FK kenarında yalnız `LEFT JOIN active_*` kullanılır; INNER JOIN yasak. Faz 1'de lint'le zorlanacak.
- Temel tablolara yalnız `koala_maint` rolü erişir: geri alma, temizlik, anonimleştirme. M1 tabloları (defter, kanıt) silinmez.

## Hatalar (ADR-0007)

- Servisler istemciye dönecek hatayı `apperr.New(kind, "STABLE_CODE")` ya da `apperr.Wrap(cause, kind, code)` ile üretir. Kod UPPER_SNAKE'tir ve anlamı hiç değişmez; OpenAPI'deki kod listesine eklenir.
- Alan paketinde `ErrNotFound`/`ErrForbidden` gibi sentinel tanımlanmaz. HTTP durumu yalnız `httpx.StatusOf` ile belirlenir.
- Dış paketten gelen hata bağlam eklenerek sarılır: `fmt.Errorf("load job %s: %w", id, err)` **[lint: wrapcheck, errorlint]**.
- `*apperr.Error` olmayan hata istemciye `500 INTERNAL` olarak gider, sebebi yalnız log'a yazılır **[test]**. "Bu durumda 409 dönsün" gibi örtük eşleme yazılmaz.
- Gövde decode hatası olduğu gibi döndürülür. `httpx.BodyLimit` aşımı (varsayılan 1 MiB) `413 REQUEST_BODY_TOO_LARGE` olur **[test]**. Daha büyük gövde gereken route `r.With(httpx.BodyLimit(n))` kullanır.
- Yetkisiz okuma, kaynağın kendi 404'ünü döner. Gövde bayt bayt aynıdır (ADR-0014).

## Transaction (ADR-0012, plan §3.5)

- Tx servis metodu sınırında `db.InTx(ctx, func(q Querier) error)` ile açılır. Store `Querier` alır, kendi tx'ini açmaz.
- **Dış çağrı (Iyzico, FCM, SMTP, S3) sırasında tx açık kalmaz.** Akış şöyledir: rezerve et ve commit et → dış çağrıyı yap → sonucu kısa bir tx'te yaz.
- Garantili yan etki iş verisiyle aynı tx'te River `InsertTx` ile kuyruğa girer. Best-effort işler commit sonrası hook'ta çalışır.
- Kilit açık yazılır (`SELECT … FOR UPDATE`). Para yolunda kilit sırası "önce atama, sonra detay"dır.
- Her isteğin context'inde `httpx.RequestTimeout` süre sınırı var. Uzun işler isteğe değil, River işine taşınır.

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
- Servis testleri gerçek PG'de koşar, test başına ayrı DB kullanılır (Faz 1). **Bellek içi store yazılmaz.** Sahte implementasyon yalnız dış entegrasyonlar için yazılır.
- Zamana bağlı testler sahte saat kullanır. `time.Sleep` ile yarış beklenmez; koşul döngüyle ve süre sınırıyla beklenir. Sunucu ya da goroutine başlatan test kendini `t.Cleanup` ile kapatır.
- Para, authz ve KVKK kuralları için senaryo kataloğundaki (`test/scenarios/`) P0 satırının bir Go testi olur (plan §6.2).

## Bağımlılıklar (ADR-0003)

- Yalnız plan §3.3'teki onaylı listeden alınır ve yalnız onu gerçekten kullanan PR'da eklenir **[make verify: go mod tidy -diff]**.
- Liste dışı her bağımlılık ayrı onay ister. PR gövdesine lisans, gerekçe ve alternatif yazılır.
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
