# ADR-0023: İmaj kapısı, yayın hattı ve test sahteleri

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.8, §3.11, §6.1 (Faz 1 ve Faz 9); [ADR-0004](0004-quality-gate-and-tool-pinning.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md); koala-backend `release.yml`, `scripts/publish-ghcr-image.sh` ve CLAUDE.md §6
- Kod: `.github/workflows/{verify,release}.yml`, `scripts/image.sh`, `internal/platform/{clock,fakes}`, [runbook](../runbook/yayin.md)

## Bağlam

Faz 0'dan kalan Dockerfile hiç build edilmedi. Plan Faz 1'in çıkış kriteri şunu istiyor: imaj distroless nonroot, açılış 1 sn'nin altında (§6.1). Faz 1 kapsamında ayrıca "Docker, yayın betiği" ve test altyapısı olarak sahte saat ile Iyzico ve FCM sahteleri var.

Kısıtlar:
- Yerelde ağır iş yok, kapı GitHub'ın kendi runner'larında koşar (ADR-0004). Self-hosted runner kullanılmaz.
- Java'nın yayını şu sırayla ilerliyor: aynı SHA'da yeşil `push: main` koşusu → GHCR imajı → `v*` tag'i → Dokploy webhook'u. Java imajı **tek bir linux/amd64 manifest** olarak basıyor (`--provenance=false --sbom=false`); koala-backend CLAUDE.md §6 bunun gerekçesini "indeks Dokploy'a çekilemez" diye yazıyor. Dokploy Docker Swarm kullanıyor. Attestation manifest'i taşıyan bir indeks (platformu `unknown/unknown`), Swarm'ın platform eşleştirmesinde sorun çıkarıyor.
- `is-koala` organizasyonu GitHub Free planında ve repo private. Bu yüzden:
  - GitHub'ın yerleşik artifact attestation'ı (`actions/attest-build-provenance`, `actions/attest-sbom`) private repoda yalnız Enterprise Cloud'da var;
  - branch protection ve ruleset'ler private repoda Pro/Team ister. API 05.11'de 403 döndü.
- Yeni araç ya da üçüncü taraf action onaysız eklenmez. Resmî `actions/*` ve `docker/*` action'ları SHA ile sabitlenince kabul.

## Karar

1. **Her PR'da ve `main` push'unda imaj build edilir ve CI içinde çalıştırılır.** Bu iş `verify.yml`'deki ayrı `image` job'ıdır ve `scripts/image.sh` ile şu sırayla yürür:
   - `build`: linux/amd64, `VERSION=ci-<sha12>`. Her `FROM` satırının digest'le sabitli olduğu denetlenir.
   - `smoke`: job'ın Postgres service container'ına karşı çalışır. Kırmızı olduğu durumlar:
     - imaj kullanıcısı `nonroot:nonroot` değil, süreç uid'i 65532 değil ya da imajda shell var;
     - `koala version`, build'e verilen sürümü yazmıyor;
     - ilk `koala bootstrap` başarısız; operatörün açtığı girişlerden sonra `scripts/release-db.sh` (bootstrap'ın ikinci kez koşması ve `migrate up`) başarısız;
     - `serve` açılışında süreç başlangıcından ilk `/livez` 200'üne kadar geçen süre **1 sn ya da üstü**;
     - `/readyz` 10 sn içinde 200 olmuyor;
     - `koala healthcheck` çalışan sunucuya karşı 0, sunucusuz 0 dışında bir kodla çıkmıyor; Docker `HEALTHCHECK` 40 sn içinde `healthy` olmuyor;
     - SIGTERM'den sonra çıkış kodu 0 değil.
   - Açılış iki saatle ölçülür: `docker start`'tan itibaren ve konteynerin `StartedAt` anından itibaren. Kapı ikincisidir, çünkü plan uygulamanın açılışını ölçüyor, Docker'ın kendi ek yükünü değil. İkisi de raporlanır.
   - İmaj boyutu (sıkıştırılmamış ve gzip) ve ölçümler job özetine ve bir `::notice` notuna yazılır. PR'a yorum yazılmaz, çünkü job'a `pull-requests: write` verilmez.
   - `publish`: yayının yolu job'a özel registry'ye (`registry:3`, digest'le sabitli service container) karşı aynen koşar. Bu sayede tag atmadan da sınanır.
2. **Yayın `v*` tag'iyle başlar** (`release.yml`). Java'dan farkı şudur: Java'da tag'i workflow basıyor, burada insan basar ve workflow tag'i denetler.
   - `gate` job'ı şu durumlarda kırmızıdır:
     - tag `vMAJOR.MINOR.PATCH[-PRERELEASE]` biçiminde değil;
     - tag'in commit'i `main`'de değil;
     - aynı SHA'da `verify.yml`'nin yeşil bir `push: main` koşusu yok. Bu koşu `make verify`, entegrasyon testleri ve imaj smoke'unu içerir.
   - Erken basılan tag'de workflow, verify yeşile döndükten sonra yeniden koşturulur.
   - `publish` job'ı:
     - GHCR'de aynı tag varsa durur. Basılmış tag değişmez.
     - İmajı build edip smoke'tan geçirir.
     - `ghcr.io/is-koala/koala-api:<tag>` adresine tek manifest olarak basar. Pre-release olmayan tag'de `:latest` de basılır. Dokploy'un Java'daki gibi sabit bir etiketi çekmesi için `:latest` gerekir.
     - Basılan nesnenin indeks olmadığını ve iki etiketin aynı digest'i gösterdiğini denetler.
   - Smoke basmadan önce koşar. Bozuk imaj GHCR'ye hiç gitmez.
3. **SBOM ve provenance BuildKit'in kendi üretimidir, registry'ye değil GitHub release'ine konur.**
   - Aynı build, cache'ten ve aynı argümanlarla, `--attest type=sbom` (SPDX; üreteç `docker/buildkit-syft-scanner` digest'le sabit) ve `--attest type=provenance,mode=max` (SLSA) ile yerel exporter'a bir kez daha koşar. Ortaya çıkan dosyalar şunlardır: `sbom.spdx.json`, `provenance.json`, `image.json` (basılan digest, revizyon) ve `SHA256SUMS`.
   - `github-release` job'ı bu dosyalarla tag için bir GitHub release'i açar.
   - SBOM'un binary'deki Go modüllerini görmesi denetlenir (`github.com/jackc/pgx/v5`).
   - Değerlendirilen seçenekler:

     | Seçenek | Neden alınmadı |
     |---|---|
     | `--provenance`/`--sbom` ile registry'de attestation | Tag indekse döner; Swarm/Dokploy çekemez (Java'nın kaydı). |
     | GitHub attestation (`actions/attest-*`) | Private repoda Free planda yok. |
     | cosign ile imzalı attestation | Yeni araç; onay gerekir, bu PR'da sorulmadı. |
   - Bedeli şu: attestation'lar imzalı değil ve imaj digest'ine registry'de bağlı değil. Bağ yalnız release'teki `image.json` ve `SHA256SUMS`'ta. Repo public olur ya da Enterprise'a geçilirse `actions/attest-build-provenance` ve `actions/attest-sbom` eklenir. Bunlar tag'in manifest'ini değiştirmez.
4. **Dokploy webhook'u yazılıdır ama kapalıdır.** `deploy` job'ı yalnız depo değişkeni `DOKPLOY_DEPLOY_ENABLED` `true` ise koşar ve sırrı `DOKPLOY_DEPLOY_WEBHOOK` adıyla okur. İkisi de Faz 9'da tanımlanır. Webhook'tan önce `scripts/release-db.sh`'ın production DB'ye karşı nerede koşacağı açık sorudur (Sonuçlar).
5. **Yetkiler en azdır.**
   - `release.yml`'nin üst düzeyi `permissions: {}`'dir. Job'ların yetkileri:
     - `gate`: `contents: read`, `actions: read`;
     - `publish`: `contents: read`, `packages: write`;
     - `github-release`: `contents: write`;
     - `deploy`: hiçbiri.
   - `verify.yml`'deki `image` job'ı yalnız `contents: read` alır.
   - Yeni action'lar şunlardır, hepsi SHA ile sabit ve en az 14 günlük sürümler:
     - `docker/setup-buildx-action` v4.4.1,
     - `docker/login-action` v4.6.0 (v4.7.0 14 günü doldurmadı),
     - `actions/upload-artifact` v7.0.2,
     - `actions/download-artifact` v8.0.2.
6. **`internal/platform/clock`.**
   - `Clock` arayüzü `Now`, `NewTimer` ve `NewTicker`'dan oluşur.
   - `Real` sistem saatidir.
   - `*Fake` yalnız `Advance` ile ilerler. Geçilen zamanlayıcıları ve tick'leri sırayla tetikler. Go 1.23 zamanlayıcıları gibi, `Stop`/`Reset`'ten sonra bayat değer bırakmaz.
   - `BlockUntil(ctx, n)` test edilen kod zamanlayıcısını kurana kadar bekler; böylece `Advance` yarışsız olur.
   - `realtime.Clock` (`Now()`) tüketici tarafındaki arayüz olarak kalır; `Real` ve `*Fake` onu karşılar. realtime testlerindeki yerel `fakeClock`'un bu pakete geçmesi o paketin işidir.
7. **`internal/platform/fakes`.** httptest tabanlıdır, gerçek istemcinin konuşacağı HTTP sunucusudur.
   - Ortak `Server` şunları sağlar: istek kaydı, yol başına sıralı senaryo (`Enqueue`), `Hang` (istemci vazgeçene kadar yanıtsız) ve `Drop` (bağlantı yanıtsız kapanır). Hiçbir şeyin yanıtlamadığı istek 501 alır ve testi cleanup'ta kırmızı yapar.
   - `Iyzico`:
     - Her isteğin IYZWSv2 imzasını doğrular. Kötü imza 401 alır ve programlanmış yanıtı tüketmez.
     - Initialize ve retrieve yanıtlarını Java'nın alan sırasıyla imzalar. Ondalıkları `stripTrailingZeros().toPlainString()` gibi yazar.
     - `testdata/iyzico/vectors.json`'daki IR-01, IR-02, IV-01–IV-04 ve normalizasyon satırlarıyla test edilir.
   - `FCM`:
     - OAuth 2.0 token ucu ve `messages:send` sunar.
     - Bearer token'ı yalnız kendi verdiği token'lardan kabul eder.
     - Mesajın tek hedefi olmasını ve `data`'nın string değerli olmasını denetler.
     - Cihaz başına kalıcı hata verebilir (`FailToken`). Google'ın hata gövdesini `FcmError` ayrıntısıyla üretir.
   - Protokol yardımcıları dışa açılmaz, böylece Faz 5–6 istemcisi bu paketi import edemez. Kural tek bir vektör dosyasında tutulur, iki uygulama da o dosyaya bağlıdır.
   - `TestOnlyTestsImportFakes`, test dışı hiçbir dosyanın paketi import etmediğini denetler.

## Sonuçlar

- Faz 1'in "imaj distroless nonroot, açılış <1 sn" kriteri her PR'da ölçülür.
- `scripts/release-db.sh` artık gerçek imajla, CI'da her PR'da koşuyor (ADR-0017 Sonuçlar'daki "yayın hattı kurulduğunda bağlanır" maddesinin ilk yarısı).
- Yayın hattı ilk `v*` tag'ine kadar uçtan uca sınanmamış olacak. `gate`, GHCR'ye giriş, `github-release` ve `deploy` yalnız tag'de koşar. Build, smoke, tek manifest denetimi ve SBOM/provenance her PR'da job registry'sine karşı koşar.
- `:latest` her kararlı tag'de ilerler. İmaj smoke'tan sonra basılır; smoke kırmızıysa GHCR'de hiçbir etiket değişmez.
- Branch protection olmadığı için `v*` tag'ini yazma yetkisi olan herkes atabilir. `gate` bunu yalnız "main'de ve yeşil" koşuluyla sınırlar.
- **Açık sorular (Faz 9):**
  - `release-db.sh` production DB'ye karşı nerede koşacak? GitHub runner DB'ye erişemez. Seçenekler: Dokploy'un ön-dağıtım komutu ya da zamanlanmış işi, ya da sunucuda SSH ile çalışan ayrı bir adım.
  - Webhook DB adımından sonra mı tetiklenecek?
  - Dokploy `:latest` mı, tag mı çekecek?
- **Kanıtlanmadı:** GITHUB_TOKEN'ın `ghcr.io/is-koala/koala-api` paketini ilk kez yaratması. Bu organizasyonun paket oluşturma ayarına bağlı; runbook'taki insan adımı.
