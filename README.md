# koala-api

Koala'nın Go backend'i. Koala, İstanbul'da kısa süreli işler için bir pazaryeri. Bu repo, Java/Spring Boot `koala-backend`'in baştan yazımıdır. Hazır olunca Java'nın yerine tek seferde konacak (ADR-0001).

Durum: **Faz 0**. Repo iskeleti, kalite kapısı ve ADR'ler hazır. Sunucu çalışıyor ve yalnız sağlık uçlarını sunuyor. Alan modülleri sonraki fazlarda gelecek.

## Gereksinimler

- Go: `go.mod`'daki sürüm (1.25.x).
- `make`.
- golangci-lint kurulu olmak zorunda değil. `make tools` onu sabit sürümde `./bin`'e derler (ADR-0004).

## Çalıştırma

```sh
make run                                   # KOALA_ENV=local ile :8080'de başlar
curl -i localhost:8080/livez               # 200 ok
curl -i localhost:8080/readyz              # 200 ok; kapanırken 503
```

İkili `make build` ile `./bin/koala`'ya derlenir. Alt komutları:

| Komut | Ne yapar |
|---|---|
| `koala serve` | HTTP sunucusunu başlatır. SIGTERM'de sıralı kapanır: `/readyz` 503 döner → drain beklemesi → istekler boşaltılır → çıkış. |
| `koala healthcheck` | `/readyz`'i loopback üzerinden yoklar; Docker `HEALTHCHECK` için. |
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
| `KOALA_SHUTDOWN_TIMEOUT` | hayır | `20s` | Süren isteklerin bitmesi için verilen süre |

Konteynerin durdurma süresi (Docker `--stop-timeout`, Swarm/Dokploy `stop_grace_period`) şu toplamdan uzun olmalı: `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT`, varsayılanlarla en az 30 sn. Docker'ın varsayılanı 10 sn; bu süre aşılırsa süreç SIGKILL ile kesilir.

Log'lar stdout'a JSON olarak (`log/slog`) yazılır. Her istek satırında `request_id` bulunur. Gelen `X-Request-Id` başlığı iyi biçimliyse kullanılır, değilse yeni bir id üretilir. Id yanıtta da döner.

Hata yanıtları RFC 9457 `application/problem+json` biçimindedir ve stabil bir `code` alanı taşır (ADR-0007).

## Kalite kapısı

```sh
make verify
```

Sırasıyla şunları koşar:
1. `gofmt` kontrolü,
2. `go mod tidy -diff`,
3. `go vet`,
4. `golangci-lint run` (`.golangci.yml`),
5. `go test -race -count=1 ./...`.

PR yalnız kapı yeşilken açılır. Diğer hedefler için `make help`'e bakın.

## Docker

`Dockerfile` çok aşamalıdır: `golang` → `distroless/static:nonroot`. İki taban imaj da etiket ve digest ile sabitlenmiştir. İmaj nonroot kullanıcıyla çalışır; sağlık kontrolü ikilinin kendisidir.

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t koala-api .
```

## Düzen

```
cmd/koala/            ince main: sinyal + app.Run
internal/app/         elle wiring ve alt komutlar
internal/platform/    alan bilgisi taşımayan altyapı: config, log, httpx, apperr
db/                   migration ve sorgular (Faz 1)
api/                  OpenAPI 3.1 sözleşmesi (ayrı PR)
docs/adr/             mimari karar kayıtları
```

Kodlama kuralları `CLAUDE.md`'de, kararların gerekçesi `docs/adr/`'de.
