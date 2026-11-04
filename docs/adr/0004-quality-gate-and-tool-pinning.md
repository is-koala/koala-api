# ADR-0004: Kalite kapısı, Go sürümü ve araç sabitleme

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §6.1 (Faz 0 ve Faz 1 çıkış kriterleri), §3.11, §9 R10; N05 A4, A5-13

## Bağlam

Faz 1'in çıkış kriteri şu: "`make verify` yeşil: `golangci-lint` 0 bulgu, `go test -race` yeşil" (§6.1).

core-backend'de lint yalnız `gofmt` ve `go vet`'ten ibaret (N05 A4). Koala'da sıkı bir golangci-lint seti öneriliyor (N05 A5-13). Java repodaki kural korpusunun bayatladığı görüldü (§8 madde 13). Bu yüzden kural düzyazıda değil, araçta durmalı.

Bu Mac'i birden çok oturum paylaşıyor ve yük testleri sahte kırmızıya çeviriyor (§9 R10). Yusuf'un 04.11 kuralı: koala'da yerelde ağır iş yapılmaz, kapı GitHub'ın kendi runner'larında koşar.

Go yalnız son iki ana sürüme güvenlik yaması veriyor. İskelet önce Mac'teki 1.25.5 ile açılmıştı; o sürüm destek dışı.

golangci-lint'i sabitlemek için üç seçenek vardı:

| Seçenek | Artı | Eksi |
|---|---|---|
| `go.mod` `tool` direktifi (`go get -tool`) | Sürüm `go.mod`'da durur, `go tool golangci-lint` ile çağrılır. | Linter'ın 100+ modülü uygulamanın modül grafiğine girer. MVS, ortak modülleri (ör. `x/text`, `x/sync`) uygulama için de yükseltebilir. `go.sum` şişer ve ADR-0003'ün "go.mod yalnız uygulamanın bağımlılıkları" ilkesi bozulur. |
| `go install …@vX.Y.Z` → `./bin` | Modül grafiği ayrı kalır. Kaynak sum.golang.org ile doğrulanır; platform başına sağlama toplamı tutmak gerekmez. Repo'nun Go araç zinciriyle derlenir. | Kaynaktan derleme CI süresine birkaç dakika ekler. golangci-lint belgeleri kaynaktan kurulumu "garantisiz" sayıyor; bu uyarı linter'ın desteklemediği bir Go sürümüyle derlenmesine ilişkin. Bunu, linter sürümünü Go sürümüne göre seçerek karşılıyoruz (Karar 3). |
| Yayımlanmış ikiliyi indirmek (resmî kurulum betiği) | En hızlısı; belgelerin önerdiği yol. | İndirme betiği çalıştırılır. Her platform için sağlama toplamının ayrıca sabitlenmesi gerekir. |

## Karar

1. **Kapı CI'da koşar.** `.github/workflows/verify.yml` şunlarla çalışır:
   - `ubuntu-latest`,
   - `pull_request` ve yalnız `main`'e `push`,
   - `permissions: contents: read`,
   - PR'da yeni push eski koşuyu iptal eder.

   Action'lar commit SHA'sıyla sabitlenir. `actions/setup-go` sürümü `go.mod`'daki `toolchain` satırından okur. Tek adım `make verify`'dır.

   **Yerelde ağır iş yoktur.** `go test`, `go vet`, `go build`, golangci-lint ve docker yerelde çalıştırılmaz. Yalnız `gofmt` ve `go mod tidy` gibi hafif işler serbesttir. PR, CI'daki `verify` yeşil olmadan merge edilmez.
2. **`make verify` sırası:**
   1. `gofmt -l` (boş olmalı),
   2. `go mod tidy -diff`,
   3. `go vet ./...`,
   4. `golangci-lint run`,
   5. `scripts/lint-probe.sh`: depguard, forbidigo ve sloglint kurallarının gerçekten tetiklendiğini, geçici olarak üretilen ve commit'e girmeyen prob paketleriyle kanıtlar. depguard'ın `platform` kuralındaki allow/deny çakışmasının doğru çözüldüğünü de denetler.
   6. `govulncheck ./...`,
   7. `go test -race -shuffle=on -count=1 ./...`.
3. **Go sürümü: desteklenen en yeni Go.**
   - `go.mod`: `go 1.27` + `toolchain go1.27.3`.
   - `GOTOOLCHAIN` kısıtlanmaz; eksikse Go doğru araç zincirini kendisi indirir. Sistemdeki Go'ya dokunulmaz.
   - Dockerfile'daki `golang` imajı `toolchain` satırıyla aynı sürümdür.
   - Yeni bir Go ana sürümü çıkınca, ya da yama sürümü güvenlik düzeltmesi getirince, `go.mod`, Dockerfile ve linter aynı PR'da yükseltilir.
4. **Araçlar `go install` ile sabit sürümde kurulur** ve `./bin/<araç>-<sürüm>` olarak durur. Sürümler `Makefile`'dadır; sürüm değişince dosya adı değişir, yeni sürüm kendiliğinden kurulur.
   - golangci-lint `v2.15.0`: Go 1.27'yi destekleyen sürüm. golangci-lint'in `go.mod`'u "`go` direktifi her zaman en yeni − 1" kuralını yazıyor. v2.13'ten itibaren bu direktif 1.26; buradan Go 1.27'nin desteklendiği çıkıyor.
   - govulncheck `v1.9.0` (`golang.org/x/vuln`).
5. **`.golangci.yml` v2 biçimindedir ve `default: none` + açık liste kullanır.** Yeni bir sürümün getirdiği linter kapıyı sessizce değiştirmez. Kurallar mümkün olduğunca burada zorlanır:
   - depguard: reddedilen kütüphaneler, `domain.go` ve `platform` katman sınırları.
   - forbidigo: `fmt.Print*`, `os.Getenv` ve alan kodunda `time.Now` yasak.
   - sloglint: global logger yok, context'li çağrı, `snake_case` anahtar.
   - paralleltest ve tparallel: testler paralel koşar; `-shuffle=on` sıra bağımlılığını açığa çıkarır.
   - nolintlint: her `//nolint` belirli bir linter adı ve gerekçe taşır.
   - wrapcheck, errorlint, gosec, exhaustive, revive, gocritic, staticcheck, unparam ve diğerleri.

   Lint'in ifade edemediği kurallar `internal/archtest` altında testtir (ör. `domain.go`'da JSON struct tag'i yok).
6. **Docker imajı** çok aşamalıdır. Taban imajlar etiket ve digest ile sabittir: `golang:<toolchain>` ve `distroless/static:nonroot`. `CGO_ENABLED=0` ve `-trimpath` ile derlenir. `HEALTHCHECK` ikilinin kendi `healthcheck` alt komutunu çağırır; bu komut `/livez`'i yoklar (§3.11). İmaj yerelde build edilmez.

## Sonuçlar

- Kapının sonucu PR'daki `verify` kontrolünden okunur. Yerelde koşulmadığı için PR gövdesine yerel kapı çıktısı yazılmaz.
- İlk CI koşusu araçları kaynaktan derler; sonraki koşular setup-go önbelleğinden yararlanır.
- Plan §3.3 sqlc için `go tool` diyor. sqlc ve oapi-codegen'i ilk kullanan PR, aynı ölçüte (araç modül grafiğine karışıyor mu?) göre karar verir ve bu ADR'ye atıf yapar.
- Yerelde, iş bitince `go clean -cache` çalıştırılır.
