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

Faz 1'de kalanlar: pgx/`InTx`, goose ve sqlc, metrik, 426 sürüm kapısı, rate limit, path normalizasyonu, River, authn/authz ve soft-delete korumaları.

Plan belgesi: [`docs/plan/go-gecis-degerlendirme.md`](docs/plan/go-gecis-degerlendirme.md). Kararlar: [`docs/adr/`](docs/adr/README.md). Kodlama kuralları: [`CLAUDE.md`](CLAUDE.md).

## Gereksinimler

- Go: `go.mod`'daki `toolchain` sürümü (go1.27.3). Eski bir Go kuruluysa `GOTOOLCHAIN=auto` ile doğru sürüm kendiliğinden iner.
- `make`.

## Kalite kapısı: CI'da

`make verify` GitHub Actions'ta her PR'da ve `main`'e her push'ta koşar (`.github/workflows/verify.yml`). Sırası:
1. `gofmt` kontrolü,
2. `go mod tidy -diff`,
3. `go vet`,
4. golangci-lint (`.golangci.yml`),
5. lint probu (`scripts/lint-probe.sh`): katman ve bağımlılık kurallarının gerçekten tetiklendiğini geçici prob paketleriyle kanıtlar,
6. `govulncheck`,
7. `go test -race -shuffle=on -count=1 ./...`.

Araçlar (`golangci-lint`, `govulncheck`) `Makefile`'daki sabit sürümlerle `./bin`'e kaynaktan derlenir (ADR-0004).

**Paylaşılan geliştirme Mac'inde ağır iş yapılmaz.** Yerelde yalnız `make fmt-check`, `make mod-check`, `gofmt` ve `go mod tidy` çalıştırılır. Test, vet, lint ve build CI'da koşar.

## Çalıştırma

Sunucu bir CI ya da sunucu ortamında şöyle çalışır:

```sh
make run                                   # KOALA_ENV=local ile :8080'de başlar
curl -i localhost:8080/livez               # 200 ok (HEAD de kabul edilir)
curl -i localhost:8080/readyz              # 200 ok; kapanırken 503
```

İkili `make build` ile `./bin/koala`'ya derlenir. Alt komutları:

| Komut | Ne yapar |
|---|---|
| `koala serve` | HTTP sunucusunu başlatır. SIGTERM'de sıralı kapanır: `/readyz` 503 döner → drain beklemesi → istekler boşaltılır → çıkış. |
| `koala healthcheck` | `/livez`'i loopback üzerinden yoklar; Docker `HEALTHCHECK` için. DB kesintisi konteyneri yeniden başlatmasın diye readiness değil liveness kullanılır. |
| `koala migrate` | Yer tutucu; Faz 1'de goose ile gelecek. |
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

Konteynerin durdurma süresi (Docker `--stop-timeout`, Swarm/Dokploy `stop_grace_period`) şu toplamdan uzun olmalı: `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT`. Varsayılanlarla bu toplam 5 + 35 = 40 sn'dir; `stop_grace_period` en az 45 sn verilmeli. Docker'ın varsayılanı 10 sn; bu süre aşılırsa süreç SIGKILL ile kesilir.

## HTTP platformu

- Log'lar stdout'a JSON olarak (`log/slog`) yazılır. Her satırda `request_id` bulunur.
- Gelen `X-Request-Id` başlığı iyi biçimliyse kullanılır, değilse yeni bir id üretilir. Id yanıtta da döner.
- Hata yanıtları RFC 9457 `application/problem+json` biçimindedir ve stabil bir `code` alanı taşır (ADR-0007).
- Her isteğin context'inde 25 sn'lik süre sınırı var (sunucunun 30 sn'lik `WriteTimeout`'undan kısa).
- İstek gövdesi varsayılan olarak 1 MiB ile sınırlı. Aşılırsa `413 REQUEST_BODY_TOO_LARGE` döner; route bazında `httpx.BodyLimit(n)` ile yükseltilir.
- Her yanıtta şu başlıklar var: `X-Content-Type-Options: nosniff` ve `Content-Security-Policy: frame-ancestors 'none'`.

## Docker

`Dockerfile` çok aşamalıdır: `golang:1.27.3` → `distroless/static:nonroot`. İki taban imaj da etiket ve digest ile sabitlenmiştir. İmaj nonroot kullanıcıyla çalışır; sağlık kontrolü ikilinin kendisidir. İmaj paylaşılan Mac'te build edilmez.

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t koala-api .
```

## Düzen

```
cmd/koala/            ince main: sinyal + app.Run
internal/app/         elle wiring ve alt komutlar
internal/platform/    alan bilgisi taşımayan altyapı: config, log, httpx, apperr
internal/archtest/    lint'in ifade edemediği mimari testler
db/                   migration ve sorgular (Faz 1)
api/                  OpenAPI 3.1 sözleşmesi: api/openapi.yaml, api/components/ (ayrı PR)
docs/adr/             mimari karar kayıtları
docs/plan/            Go geçiş planı
scripts/              CI yardımcıları (lint probu)
```
