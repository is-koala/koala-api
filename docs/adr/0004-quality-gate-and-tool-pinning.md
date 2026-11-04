# ADR-0004: Kalite kapısı ve araç sabitleme

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Kaynak: plan §6.1 (Faz 0 ve Faz 1 çıkış kriterleri), §3.11; N05 A4, A5-13

## Bağlam

Faz 1'in çıkış kriteri şu: "`make verify` yeşil: `golangci-lint` 0 bulgu, `go test -race` yeşil" (§6.1).

core-backend'de lint yalnız `gofmt` ve `go vet`'ten ibaret (N05 A4). Koala'da sıkı bir golangci-lint seti öneriliyor (N05 A5-13). Ayrıca Java repodaki 5.600 satırlık kural korpusunun bayatladığı görüldü (plan §8 madde 13). Kural düzyazıda değil, araçta durmalı.

Kapı yerelde ve CI'da aynı komutla koşmalı. Bu yüzden aracın sürümü makineden makineye kaymamalı.

golangci-lint bu Mac'te kurulu değildi. Sabitleme için üç seçenek vardı:

| Seçenek | Artı | Eksi |
|---|---|---|
| `go.mod` `tool` direktifi (`go get -tool`) | Sürüm `go.mod`'da durur, `go tool golangci-lint` ile çağrılır. | Linter'ın 100+ modülü uygulamanın modül grafiğine girer. MVS, ortak modülleri (ör. `x/text`, `x/sync`) uygulama için de yükseltebilir. `go.sum` şişer ve ADR-0003'ün "go.mod yalnız uygulamanın bağımlılıkları" ilkesi bozulur. |
| `go install …@vX.Y.Z` → `./bin` | Modül grafiği ayrı kalır. Kaynak sum.golang.org ile doğrulanır; platform başına sağlama toplamı tutmak gerekmez. Repo'nun kendi Go sürümüyle derlenir. | İlk kurulum kaynaktan derler (birkaç dakika). golangci-lint belgeleri kaynaktan kurulumu "garantisiz" sayıyor. Bu uyarı, linter'ın desteklemediği bir Go sürümüyle derlenmesine ilişkin. Bunu, linter sürümünü `go` direktifine göre seçerek karşılıyoruz (Karar 2). |
| Yayımlanmış ikiliyi indirmek (resmî kurulum betiği) | En hızlısı; belgelerin önerdiği yol. | İndirme betiği çalıştırılır. Her platform (darwin/arm64, linux/amd64…) için sağlama toplamının ayrıca sabitlenmesi gerekir. |

## Karar

1. **`make verify` kapıdır.** Sırasıyla şunları koşar:
   - `gofmt -l` (boş olmalı),
   - `go mod tidy -diff`,
   - `go vet ./...`,
   - `golangci-lint run`,
   - `go test -race -count=1 ./...`.

   PR yalnız kapı yeşilken açılır.
2. **golangci-lint `go install` ile sabit sürümde kurulur.** Sürüm `Makefile`'daki `GOLANGCI_LINT_VERSION` değişkenindedir.
   - Kurulum `GOTOOLCHAIN=local` ile yapılır, ikili `./bin/golangci-lint-<sürüm>` olarak durur.
   - Sürüm değişince dosya adı değişir, yeni sürüm kendiliğinden kurulur.
   - Seçilen sürüm `go.mod`'daki `go` direktifini kabul eden en yeni sürümdür: Go 1.25 için v2.12.2. v2.13 ve sonrası Go 1.26 istiyor.
   - Go sürümü yükseltildiğinde linter da aynı PR'da yükseltilir.
3. **`.golangci.yml` v2 biçimindedir ve `default: none` + açık liste kullanır.** Yeni sürümün getirdiği linter kapıyı sessizce değiştirmez. Kurallar mümkün olduğunca burada zorlanır:
   - depguard: reddedilen kütüphaneler, `domain.go` ve `platform` katman sınırları.
   - forbidigo: `fmt.Print*`, `os.Getenv` ve alan kodunda `time.Now` yasak.
   - sloglint: global logger yok, context'li çağrı, `snake_case` anahtar.
   - nolintlint: her `//nolint` belirli bir linter adı ve gerekçe taşır.
   - wrapcheck, errorlint, gosec, exhaustive, revive, gocritic, staticcheck, unparam ve diğerleri.
4. **Docker imajı** çok aşamalıdır. Taban imajlar etiket ve digest ile sabittir: `golang:<sürüm>` ve `distroless/static:nonroot`. `CGO_ENABLED=0` ve `-trimpath` ile derlenir. `HEALTHCHECK` ikilinin kendi `healthcheck` alt komutunu çağırır (§3.11).

## Sonuçlar

- `make verify` ilk koşuda linter'ı derler. Sonraki koşular `./bin`'deki ikiliyi kullanır.
- Plan §3.3 sqlc için `go tool` diyor. sqlc ve oapi-codegen'i ilk kullanan PR, aynı ölçüte (modül grafiğine karışma) göre karar verir ve bu ADR'ye atıf yapar.
- Bu Mac'te ağır iş kuralı geçerli: testler `nice -n 15` ile koşar, Docker build yerelde yapılmaz.
