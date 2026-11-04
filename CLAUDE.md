# koala-api: çalışma kuralları

Koala'nın Go backend'i. Java `is-koala/koala-backend`'in baştan yazımıdır (ADR-0001).

Kararların gerekçesi `docs/adr/`'dedir. Bir kurala itiraz edeceksen önce ilgili ADR'yi oku. Kararı değiştirmek için yeni ADR yazılır; eski ADR düzenlenmez.

Bu dosya kısa tutulur. Bir kural lint ya da testle zorlanabiliyorsa oraya yazılır, buraya değil. Aşağıda **[lint]** ya da **[test]** ile işaretli kurallar makine tarafından zorlanır.

## Kapı

```sh
nice -n 15 make verify   # gofmt + go mod tidy -diff + go vet + golangci-lint + go test -race
```

- PR yalnız `make verify` yeşilken açılır. Kırmızı kapıyla "sonra düzeltirim" yok.
- `//nolint` yalnız linter adı ve gerekçeyle yazılır: `//nolint:wrapcheck // neden` **[lint: nolintlint]**. Gerekçesiz susturma yerine kodu düzelt.
- golangci-lint `make tools` ile `./bin`'e sabit sürümde kurulur (ADR-0004). Sistemdeki başka bir sürümü kullanma.

## Paket düzeni ve katmanlar (ADR-0002, plan §3.1–3.2)

- `cmd/koala/main.go` incedir: sinyal yakalama ve `app.Run`. Wiring `internal/app/`'te elle yapılır, modül başına bir `wire<Modül>()`. DI kütüphanesi kullanılmaz.
- `internal/platform/*` alan bilgisi taşımaz ve alan paketlerini import etmez **[lint: depguard platform]**.
- Her alan şu dosyalardan oluşur: `domain.go` (tip, kural, FSM tablosu), `service.go`, `store.go` (sqlc sarmalayıcısı), `http.go` (üretilmiş strict arayüzün uygulaması).
- Bağımlılık yönü `http → service → store`. `domain.go` içinde `net/http`, `encoding/json`, `pgx` ya da chi bulunmaz **[lint: depguard domain]**.
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
  - Bilinmeyen `KOALA_*` değişkeni açılışı durdurur **[test]**.

## Hatalar (ADR-0007)

- Servisler istemciye dönecek hatayı `apperr.New(kind, "STABLE_CODE")` ya da `apperr.Wrap(cause, kind, code)` ile üretir. Kod UPPER_SNAKE'tir ve anlamı hiç değişmez; OpenAPI'deki kod listesine eklenir.
- Alan paketinde `ErrNotFound`/`ErrForbidden` gibi sentinel tanımlanmaz. HTTP durumu yalnız `httpx.StatusOf` ile belirlenir.
- Dış paketten gelen hata bağlam eklenerek sarılır: `fmt.Errorf("load job %s: %w", id, err)` **[lint: wrapcheck, errorlint]**.
- `*apperr.Error` olmayan hata istemciye `500 INTERNAL` olarak gider, sebebi yalnız log'a yazılır **[test]**. "Bu durumda 409 dönsün" gibi örtük eşleme yazılmaz.
- Yetkisiz okuma, kaynağın kendi 404'ünü döner. Gövde bayt bayt aynıdır (ADR-0014).

## Transaction (ADR-0012, plan §3.5)

- Tx servis metodu sınırında `db.InTx(ctx, func(q Querier) error)` ile açılır. Store `Querier` alır, kendi tx'ini açmaz.
- **Dış çağrı (Iyzico, FCM, SMTP, S3) sırasında tx açık kalmaz.** Akış şöyledir: rezerve et ve commit et → dış çağrıyı yap → sonucu kısa bir tx'te yaz.
- Garantili yan etki iş verisiyle aynı tx'te River `InsertTx` ile kuyruğa girer. Best-effort işler commit sonrası hook'ta çalışır.
- Kilit açık yazılır (`SELECT … FOR UPDATE`). Para yolunda kilit sırası "önce atama, sonra detay"dır.

## Log (plan §3.10)

- Logger yapıcıyla enjekte edilir ve `*Context` metotları kullanılır; `request_id` context'ten gelir **[lint: sloglint]**.
- Anahtarlar `snake_case`, mesajlar sabit metindir **[lint: sloglint]**.
- PII (TCKN, IBAN, telefon, adres, hassas konum), sır, token ve query string log'a yazılmaz. Erişim log'u yalnız path'i yazar **[test]**.

## Test düzeni

- Yalnız stdlib `testing` kullanılır; testify yok **[lint: depguard]**.
- Kara kutu testi tercih edilir (`package x_test`). İç fonksiyon testi gerekiyorsa aynı pakette yazılır.
- Testler tablo güdümlüdür ve `t.Parallel()` çağırır. Yardımcılar `t.Helper()` çağırır **[lint: thelper]**.
- Kablo biçimleri (problem+json, para JSON'u, Iyzico gövdeleri) altın testle bayt bayt sabitlenir.
- Servis testleri gerçek PG'de koşar, test başına ayrı DB kullanılır (Faz 1). **Bellek içi store yazılmaz.** Sahte implementasyon yalnız dış entegrasyonlar için yazılır.
- Testler sıraya bağımlı olmaz. Bir testin geçmesi kardeş testin ısıttığı duruma dayanamaz.
- Zamana bağlı testler sahte saat kullanır. `time.Sleep` ile yarış beklenmez; koşul döngüyle ve süre sınırıyla beklenir.
- Para, authz ve KVKK kuralları için senaryo kataloğundaki P0 satırının bir Go testi olur (plan §6.2).

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
  - `make verify` çıktısının özeti,
  - planın ilgili bölümleri,
  - açık sorular.
- PR incelemeden merge edilmez. Para, kripto (NFC) ve authz PR'larında ikinci göz zorunludur.
- Sır ya da env **değeri** hiçbir dosyaya, commit'e ya da PR'a yazılmaz; yalnız değişken adları yazılır.

## Paylaşılan Mac

- Bu Mac'te birden çok oturum çalışıyor. Ağır işten önce `uptime`'a bakılır. Testler `nice -n 15` ile koşar.
- Docker build ve run yerelde yapılmaz.
- İş bitince `df -h /`'ye bakılır. `~/Library/Caches/go-build` 10 GB'ı geçtiyse `go clean -cache` çalıştırılır.
