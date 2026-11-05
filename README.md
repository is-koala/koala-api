# koala-api

Koala'nın Go backend'i. Koala, İstanbul'da kısa süreli işler için bir pazaryeri. Bu repo, Java/Spring Boot `koala-backend`'in baştan yazımıdır. Hazır olunca Java'nın yerine tek seferde konacak (ADR-0001).

Durum: **Faz 0**. Repo iskeleti, CI'daki kalite kapısı ve ADR'ler hazır. Sunucu yalnız sağlık uçlarını sunuyor; alan modülleri sonraki fazlarda gelecek.

**Faz 1'den öne çekilenler.** Plan §6.1'de Faz 1'e yazılı olan bazı platform kalemleri iskeletin çalışması için bu fazda geldi:
- `config` (fail-closed),
- slog JSON log,
- `apperr` ve problem+json (altın testiyle),
- middleware zinciri: request id, erişim log'u, panic kurtarma, istek süre sınırı, gövde boyutu sınırı, güvenlik başlıkları,
- `/livez` ve `/readyz`,
- sıralı graceful shutdown,
- Dockerfile.

Faz 0'daki River + goose spike'ıyla gelenler (ADR-0017):
- pgx havuzu ve `InTx`,
- goose migration'ları ve `koala migrate`,
- River şeması ve client'ı,
- `koala_owner`/`koala_app`/`koala_maint` rol modeli,
- test başına template DB.

Faz 1'de kalanlar: sqlc, metrik, sürüm kapısı (`400 APP_VERSION_UNSUPPORTED`, ADR-0019), rate limit, path normalizasyonu, River worker'larının sunucuya bağlanması, authn/authz ve soft-delete korumalarının geri kalanı.

Plan belgesi: [`docs/plan/go-gecis-degerlendirme.md`](docs/plan/go-gecis-degerlendirme.md). Kararlar: [`docs/adr/`](docs/adr/README.md). Kodlama kuralları: [`CLAUDE.md`](CLAUDE.md).

## Gereksinimler

- Go: `go.mod`'daki `toolchain` sürümü (go1.27.3). Eski bir Go kuruluysa `GOTOOLCHAIN=auto` ile doğru sürüm kendiliğinden iner.
- `make`.

## Kalite kapısı: CI'da

Kapı iki adımdır ve GitHub Actions'ta her PR'da ve `main`'e her push'ta koşar (`.github/workflows/verify.yml`, ADR-0004, ADR-0017). İki adım da tek `verify` işindedir.

**1. `make verify`:** DB'siz kontroller ve birim testleri. Sırası:
1. `gofmt` kontrolü,
2. `go mod tidy -diff`,
3. `go vet`,
4. golangci-lint (`.golangci.yml`),
5. lint probu (`scripts/lint-probe.sh`): katman ve bağımlılık kurallarının gerçekten tetiklendiğini geçici prob paketleriyle kanıtlar,
6. `govulncheck`,
7. `go test -race -shuffle=on -count=1 ./...`,
8. spec lint (`make api-lint`): vacuum ile `api/` ve her kuralın tetiklendiğini kanıtlayan prob,
9. `make gen-check`: `internal/apiv1` spec'ten yeniden üretilenle aynı mı.

**2. `make test-integration`:** iş için başlatılan Postgres 17 service container'ına karşı yalnız `//go:build integration` etiketli testleri koşar (`scripts/test-integration.sh`).
- Önce etiket korumaları çalışır: yanlış yazılmış ya da eksik build tag'i olan test dosyası kırmızıdır.
- Her test, template DB'den kendi DB'sini klonlar (`internal/platform/dbtest`).

Araçlar (`golangci-lint`, `govulncheck`, `vacuum`) `Makefile`'daki sabit sürümlerle `./bin`'e kaynaktan derlenir (ADR-0004).

**Paylaşılan geliştirme Mac'inde ağır iş yapılmaz.** Yerelde yalnız `make fmt-check`, `make mod-check`, `gofmt` ve `go mod tidy` çalıştırılır. Test, vet, lint ve build CI'da koşar.

## Çalıştırma

Sunucu bir CI ya da sunucu ortamında şöyle çalışır. `make run` bir Postgres ister: `KOALA_DATABASE_URL` tanımlı olmalı, giriş yalnız `koala_app`'e üye olmalı, roller ve migration'lar önceden kurulmuş olmalı (`koala bootstrap`, `koala migrate up`; runbook: [`docs/runbook/veritabani.md`](docs/runbook/veritabani.md)). DB'ye ulaşılamazsa sunucu açılır ama `/readyz` 503 döner.

```sh
make run                                   # KOALA_ENV=local ile :8080'de başlar
curl -i localhost:8080/livez               # 200 ok (HEAD de kabul edilir)
curl -i localhost:8080/readyz              # 200 ok; kapanırken 503
```

İkili `make build` ile `./bin/koala`'ya derlenir. Alt komutları:

| Komut | Ne yapar |
|---|---|
| `koala serve` | HTTP sunucusunu başlatır. SIGTERM'de sıralı kapanır: `/readyz` 503 döner → drain beklemesi → istekler boşaltılır → çıkış. `KOALA_DATABASE_URL` ister. Giriş `koala_app`'ten fazlasını yapabiliyorsa açılışta DB'ye ulaşınca çıkar (ADR-0017). |
| `koala healthcheck` | `/livez`'i loopback üzerinden yoklar; Docker `HEALTHCHECK` için. DB kesintisi konteyneri yeniden başlatmasın diye readiness değil liveness kullanılır. |
| `koala bootstrap` | `koala_owner`, `koala_app` ve `koala_maint` grup rollerini yoksa kurar; `LOGIN`'i olanı `NOLOGIN`'e çeker. İdempotenttir, her yayında `migrate`'ten önce koşar (`scripts/release-db.sh`). `KOALA_BOOTSTRAP_DATABASE_URL` ile `postgres` DB'sine bağlanır; giriş `CREATEROLE` yetkili olmalı, süper kullanıcı olamaz. |
| `koala migrate up\|down\|status` | Gömülü goose migration'larını (`db/migrations`) `koala_owner` olarak uygular, tek adım geri alır ya da durumu listeler. `KOALA_OWNER_DATABASE_URL` kullanır. `down` yalnız geliştirme içindir, production'da reddedilir. |
| `koala maint reindex` | River'ın iş tablosu indekslerini `REINDEX CONCURRENTLY` ile yeniden kurar. Önce yarıda kalmış kurulumların geçersiz artıklarını düşürür. Haftalık zamanlanmış ops işi olarak koşar, `KOALA_OWNER_DATABASE_URL` kullanır. |
| `koala version` | Derleme sürümünü yazar. |

## Yapılandırma

Yapılandırma yalnız ortam değişkenlerinden okunur ve fail-closed doğrulanır. Aşağıdaki durumların her biri açılışı durdurur:
- eksik zorunlu değişken,
- geçersiz değer,
- bilinmeyen bir `KOALA_*` değişkeni.

Hata mesajı değişkenin adını verir, değerini yazmaz.

| Değişken | Zorunlu | Varsayılan | Açıklama |
|---|---|---|---|
| `KOALA_ENV` | evet | — | `local`, `test`, `staging`, `production` |
| `KOALA_HTTP_ADDR` | hayır | `:8080` | `host:port` |
| `KOALA_LOG_LEVEL` | hayır | `info` | `debug`, `info`, `warn`, `error` |
| `KOALA_SHUTDOWN_DRAIN_DELAY` | hayır | `5s` | Kapanışta `/readyz` 503 döndükten sonra dinleyicinin kapanmasına kadar geçen süre |
| `KOALA_SHUTDOWN_TIMEOUT` | hayır | `35s` | Süren isteklerin bitmesi için verilen süre; aşılırsa bağlantılar zorla kapatılır. En az `35s`: başlık okuma (5 sn) + istek süre sınırı (25 sn) + yazma payı (5 sn). Daha kısası açılışı durdurur |
| `KOALA_DATABASE_URL` | `serve` için | — | Sunucu girişi; yalnız `koala_app` üyesi. `postgres://` URL |
| `KOALA_OWNER_DATABASE_URL` | `migrate`, `maint` için | — | `koala_owner` üyesi giriş; süper kullanıcı olamaz |
| `KOALA_BOOTSTRAP_DATABASE_URL` | `bootstrap` için | — | `CREATEROLE` yetkili giriş, `postgres` DB'sine; süper kullanıcı olamaz |

Üç DB URL'si de sırdır; değerleri log'a ve hata mesajına yazılmaz. Production'da `sslmode=require`, `verify-ca` ya da `verify-full` zorunludur. Bir bağlantı parametresi URL'de iki kez geçemez. Havuz ayarları (`pool_max_conns`, `statement_timeout` ve diğerleri) URL'de verilirse varsayılanların yerine geçer (ADR-0017 §6).

Konteynerin durdurma süresi (Docker `--stop-timeout`, Swarm/Dokploy `stop_grace_period`) şu toplamdan uzun olmalı: `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT`. Varsayılanlarla bu toplam 5 + 35 = 40 sn'dir; `stop_grace_period` en az 45 sn verilmeli. Docker'ın varsayılanı 10 sn; bu süre aşılırsa süreç SIGKILL ile kesilir.

## HTTP platformu

- Log'lar stdout'a JSON olarak (`log/slog`) yazılır. Her satırda `request_id` bulunur.
- Gelen `X-Request-Id` başlığı iyi biçimliyse kullanılır, değilse yeni bir id üretilir. Id yanıtta da döner.
- Hata yanıtları RFC 9457 `application/problem+json` biçimindedir ve stabil bir `code` alanı taşır (ADR-0007).
- Her isteğin context'inde 25 sn'lik süre sınırı var (sunucunun 30 sn'lik `WriteTimeout`'undan kısa).
- İstek gövdesi varsayılan olarak 1 MiB ile sınırlı. Aşılırsa `413 REQUEST_BODY_TOO_LARGE` döner; route bazında `httpx.BodyLimit(n)` ile yükseltilir.
- Her yanıtta şu başlıklar var: `X-Content-Type-Options: nosniff` ve `Content-Security-Policy: frame-ancestors 'none'`.

## Docker ve yayın

`Dockerfile` çok aşamalıdır: `golang:1.27.3` → `distroless/static:nonroot`. İki taban imaj da etiket ve digest ile sabitlenmiştir. İmaj nonroot kullanıcıyla çalışır; sağlık kontrolü ikilinin kendisidir. İmaj paylaşılan Mac'te build edilmez.

**Her PR'da** `verify.yml`'nin `image` job'ı `scripts/image.sh` ile imajı build eder ve Postgres'e karşı çalıştırır. İmaj GHCR'ye basılmaz; yalnız job'ın kendi geçici registry'sine basılır ve yayının basma yolu orada aynen koşar. Job şunlardan biri sağlanmazsa kırmızıdır (ADR-0023):
- imaj nonroot ve shell'siz;
- `koala bootstrap` ve `scripts/release-db.sh` geçiyor;
- açılış: süreç başlangıcından `/livez` 200'e **1 sn'nin altında**;
- `koala healthcheck` ve Docker `HEALTHCHECK` çalışıyor;
- SIGTERM'de `/readyz` 503 dönerken `/livez` 200 kalıyor ve süreç 0 ile çıkıyor.

İmaj boyutu ve ölçülen açılış süresi job özetindedir.

**Yayın** `main`'den dispatch edilir: `gh workflow run "koala-api release" --ref main -f version=vX.Y.Z` (`release.yml`; adım adım: [`docs/runbook/yayin.md`](docs/runbook/yayin.md)). **İmaj GHCR'de yokken `v*` tag'i olmaz.** Akış:
1. Sürüm SemVer olmalı ve var olan en büyük sürümden büyük olmalı.
2. Aynı SHA'da `push: main` verify koşusu yeşil olmalı: `make verify`, entegrasyon testleri ve imaj smoke'u.
3. İmaj build edilir ve smoke'tan geçer.
4. İmaj, SBOM (SPDX) ve SLSA provenance attestation'larıyla etiketsiz basılır.
5. Denetimler yapılır: tek imaj manifest'i; smoke edilen imajla aynı katmanlar, config ve `/koala`; SBOM; provenance.
6. Ancak denetimlerden sonra etiketler atılır:
   - `ghcr.io/is-koala/koala-api:<sürüm>`: imaj manifest'inin kendisi. Dokploy/Swarm indeks çekemediği için indeks değildir.
   - `:<sürüm>-attest`: attestation'lı indeks.

   `:latest` basılmaz; dağıtım `:<sürüm>@<digest>` ile yapılır.
7. Workflow annotated `v*` tag'ini en son kendisi atar ve GitHub release'ini açar.
8. Dokploy webhook'u (`DOKPLOY_DEPLOY_WEBHOOK` sırrı) yalnız kararlı sürümde ve `DOKPLOY_DEPLOY_ENABLED` depo değişkeni `true` iken koşar. Faz 9'a kadar kapalıdır.

Elle atılan `v*` tag'i hiçbir şey build etmez. `release-tag-guard.yml` şu durumlarda kırmızı olur: tag annotated değil, `main`'de değil ya da imajı yok.

Konteynerin durdurma süresi (`stop_grace_period`) `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` toplamından uzun olmalı; varsayılanlarla **en az 45 sn** (Yapılandırma bölümü).

**Yapılacak insan adımları** (ayrıntı runbook'ta):
- [ ] GHCR paketinin görünürlüğü ve erişimi: private kalır, repoya bağlanır, Dokploy için `read:packages` token'ı verilir.
- [ ] Branch protection, `v*` tag ruleset'i ve `production` environment'ı. Free planda private repo için yok; Team planı ya da public repo gerekir.
- [ ] Dokploy `stop_grace_period` ≥ 45 sn.
- [ ] Faz 9'da `DOKPLOY_DEPLOY_WEBHOOK` sırrı (HTTPS alan adı) ve `DOKPLOY_DEPLOY_ENABLED` değişkeni.

Elle build gerekirse (CI ya da sunucu ortamında):

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t koala-api .
```

## Düzen

```
cmd/koala/            ince main: sinyal + app.Run
internal/app/         elle wiring ve alt komutlar
internal/apiv1/       spec'ten üretilmiş sunucu arayüzü (make generate; elle düzenlenmez)
internal/platform/    alan bilgisi taşımayan altyapı: config, log, httpx, apperr, db, jobs (River), dbtest, clock, fakes (yalnız testler)
internal/archtest/    lint'in ifade edemediği mimari testler
db/                   goose migration'ları (db/migrations), grup rolleri (db/bootstrap); sorgular Faz 1'de
api/                  OpenAPI 3.1 sözleşmesi, ortak bileşenler ve spec lint kuralları
docs/adr/             mimari karar kayıtları
docs/plan/            Go geçiş planı
docs/runbook/         işletim notları
scripts/              CI ve yayın yardımcıları: lint probları, entegrasyon testleri, release-db, image (build, smoke, publish), release (sürüm kuralları)
```
