# ADR-0023: İmaj kapısı, yayın hattı ve test sahteleri

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.8, §3.10, §3.11, §6.1 (Faz 1 ve Faz 9); [ADR-0003](0003-dependency-policy.md), [ADR-0004](0004-quality-gate-and-tool-pinning.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md); koala-backend `.github/workflows/release.yml`, `scripts/publish-ghcr-image.sh`, `docs/runbooks/release.md`, CLAUDE.md §6
- Kod: `.github/workflows/{verify,release,release-tag-guard}.yml`, `scripts/{image,release}.sh`, `internal/platform/{clock,fakes}`, [runbook](../runbook/yayin.md)

## Bağlam

Faz 0'dan kalan Dockerfile hiç build edilmedi. Plan §6.1 Faz 1 çıkış kriteri aynen şöyle: "İmaj distroless nonroot, açılış <1 sn, `/readyz` drain davranışı test edildi." Drain davranışının birim testi `internal/platform/httpx` altında `TestServerShutdownOrder`'dır: iptalde readiness 503'e döner, dinleyici kabul etmeye devam eder, süren istekler biter. Faz 1 kapsamında ayrıca "Docker, yayın betiği" ile test altyapısı olarak sahte saat, Iyzico ve FCM sahteleri var.

Kısıtlar:
- Yerelde ağır iş yok, kapı GitHub'ın kendi runner'larında koşar (ADR-0004). Self-hosted runner kullanılmaz.
- **Yusuf'un yayın kuralı: imaj GHCR'de yokken tag basılmaz.** Java bunu `workflow_dispatch` ile uyguluyor: yeşil verify → imaj → digest denetimi → annotated tag'i workflow'un kendisi atar → Dokploy webhook'u. Elle atılan `v*` tag'i imaj build etmez; imaj yoksa koşu kırmızıdır.
- Java imajı tek bir linux/amd64 manifest olarak basıyor (`--provenance=false --sbom=false`). koala-backend CLAUDE.md §6'nın gerekçesi: indeks Dokploy'a çekilemez. Dokploy Docker Swarm kullanıyor; attestation manifest'i taşıyan indeks (platformu `unknown/unknown`) Swarm'ın platform eşleştirmesinde sorun çıkarıyor.
- `is-koala` organizasyonu GitHub Free planında ve repo private. Bunun iki sonucu var:
  - GitHub'ın yerleşik artifact attestation'ı (`actions/attest-build-provenance`, `actions/attest-sbom`) private repoda yalnız Enterprise Cloud'da var.
  - Branch protection, tag ruleset'i ve environment koruma kuralları private repoda Pro/Team ister. API 05.11'de 403 döndü.

## Karar

### 1. Her PR'da ve `main` push'unda imaj

İmaj `verify.yml`'deki ayrı `image` job'ında, `scripts/image.sh` ile build edilir. GHCR'ye basılmaz; yalnız job'ın kendi geçici registry'sine (`registry:3` service container'ı) basılır. Yayının basma yolu orada aynen koşar.

- **`build`:**
  - Platform linux/amd64, `VERSION=ci-<sha12>`.
  - `SOURCE_DATE_EPOCH` commit zamanıdır ve dosya zaman damgaları yeniden yazılır (`rewrite-timestamp=true`). Böylece aynı commit'in build'leri katman katman aynıdır.
  - Her `FROM` satırının digest'le sabitli olduğu denetlenir. Denetim büyük/küçük harfe ve girintiye duyarsızdır.
- **`smoke`:** Job'ın Postgres service container'ına karşı koşar. Aşağıdakilerden biri sağlanmazsa kırmızıdır:
  - imaj kullanıcısı `nonroot:nonroot`, çalışan sürecin uid'i 65532, imajda shell yok;
  - `koala version` build sürümünü yazıyor;
  - ilk `koala bootstrap` geçiyor; operatörün açtığı girişlerden sonra `scripts/release-db.sh` (bootstrap'ın ikinci koşusu ve `migrate up`) geçiyor;
  - açılışta süreç başlangıcından ilk `/livez` 200'üne kadar geçen süre **1 sn'den kısa**; `/readyz` 10 sn içinde 200;
  - `koala healthcheck` çalışan sunucuya karşı 0 dönüyor; sunucusuz koşuda 0 dışında dönüyor, nedeni `connection refused` (yapılandırma hatası değil);
  - Docker `HEALTHCHECK` 40 sn içinde `healthy`;
  - **drain:** SIGTERM'den sonra 3 sn içinde `/readyz` 503 dönerken `/livez` hâlâ 200; süreç 45 sn içinde 0 ile çıkıyor. Bu, `TestServerShutdownOrder`'ın imajdaki karşılığıdır.
- Açılış iki saatle ölçülür: `docker start`'tan itibaren ve konteynerin `StartedAt` anından itibaren. Kapı ikincisidir, çünkü plan uygulamanın açılışını ölçüyor, Docker'ın ek yükünü değil. İkisi de raporlanır.
- İmaj boyutu (sıkıştırılmamış ve gzip) ve ölçümler job özetine ve bir `::notice` notuna yazılır. Job'a `pull-requests: write` verilmez.
- `scripts/release.sh self-test`, yayın kapısının sürüm sıralama kurallarını her PR'da sınar.

### 2. Yayın `workflow_dispatch` ile başlar; tag'i en son workflow atar

`release.yml` `main`'den dispatch edilir, girdisi `version`'dır (ör. `v0.1.0`).

- **`gate`** şu durumlarda kırmızıdır:
  - dispatch `main`'den değil;
  - sürüm `vMAJOR.MINOR.PATCH[-PRERELEASE]` değil (SemVer 2.0.0, build metadata'sız);
  - sürüm var olan bir tag;
  - sürüm var olan en büyük SemVer tag'inden büyük değil (SemVer §11 önceliği, `scripts/release.sh`);
  - aynı SHA'da `verify.yml`'nin yeşil bir `push: main` koşusu yok. Bu koşu `make verify`, entegrasyon testleri ve imaj smoke'unu içerir.
- **`publish`** GHCR'de `:<version>` ya da `:<version>-attest` varsa durur; basılmış sürüm değişmez. Sonra sırasıyla:
  1. build ve smoke;
  2. SBOM ve provenance attestation'larıyla **`push-by-digest=true` ile etiketsiz basma**;
  3. denetimler (Karar 3);
  4. ancak bunlardan sonra `imagetools create` ile etiketler.
- **`tag`** imajın `:<version>`'da beklenen digest'le durduğunu bir kez daha denetler. Sonra API ile annotated `v*` tag'ini bu koşunun SHA'sına atar ve GitHub release'ini açar. Ekler: `image.json`, `sbom.spdx.json`, `provenance.json`, `SHA256SUMS`. İmajsız tag oluşmaz. GITHUB_TOKEN ile atılan tag başka workflow tetiklemez.
- **Elle atılan `v*` tag'i** hiçbir şey build etmez. `release-tag-guard.yml` şu durumlarda kırmızı olur:
  - tag annotated değil;
  - tag'in commit'i `main`'de değil;
  - `:<tag>` imajı yok;
  - imajın `org.opencontainers.image.revision` etiketi tag'in commit'i değil.

  Guard push'u engelleyemez; kırmızı koşu "tag'i sil" demektir.
- **`:latest` basılmaz.** Dağıtım değişmez `ghcr.io/is-koala/koala-api:<version>@<digest>` referansıyla yapılır. Hareketli etiket olmadığı için "`:latest` geriye kayar" durumu da yoktur. Plan döneminde açık bırakılan "Dokploy `:latest` mı, tag mı çeker" sorusu böylece kapanır.

### 3. Basılan imaj, smoke'tan geçen imajdır; attestation'lar ayrı etikette

Etiketsiz basılan nesne bir OCI indeksidir. İçinde linux/amd64 imaj manifest'i ve BuildKit'in attestation manifest'i bulunur:
- SPDX SBOM. Üreteç `docker/buildkit-syft-scanner` digest'le sabittir.
- SLSA provenance, `mode=max`.

Etiketlemeden önce şunlar denetlenir:
- indekste tam bir linux/amd64 imaj manifest'i var; geri kalan her manifest `vnd.docker.reference.digest` ile o imaja bağlı bir attestation manifest'i. Bu bağ, attestation'ların in-toto subject'idir.
- İmaj manifest'i tek manifest'tir, indeks değildir.
- **Basılan imaj smoke'tan geçen imajdır:**
  - `rootfs.diff_ids` ve `config` (`User`, `Entrypoint`, `Cmd`, `Env`, `Labels`, `ExposedPorts`) aynı;
  - iki imajdan çıkarılan `/koala`'nın sha256'sı aynı. Bu değer `image.json`'a `binary_sha256` olarak yazılır.
- SBOM binary'deki Go modüllerini görüyor (`github.com/jackc/pgx/v5`).
- Provenance, Dockerfile'daki iki taban imajın digest'ini içeriyor.

Sonra iki etiket atılır:
- `:<version>` → imaj manifest'inin kendisi (`imagetools create --prefer-index=false`). Swarm bunu çeker.
- `:<version>-attest` → indeks. Attestation'lar registry'de imajın yanında kalır.

Değerlendirilen seçenekler:

| Seçenek | Karar | Neden |
|---|---|---|
| A. Attestation'lar sürüm etiketinin indeksinde (`--provenance`/`--sbom` + `-t :<version>`) | Alınmadı | Etiket indekse döner; Swarm/Dokploy çekemez (Java'nın kaydı). |
| B. **Attestation'lı indeks ayrı etikette (`:<version>-attest`), sürüm etiketi indeksin içindeki imaj manifest'inde** | **Seçildi** | Tek build: attestation'ın subject'i, dağıtılan digest'in kendisi. Standart araçlar (`imagetools inspect --format '{{json .Provenance}}'`) okur. Bedeli: imzasız; paket listesinde fazladan bir etiket, yanlışlıkla dağıtılırsa Swarm reddeder. |
| C. Dosyalar ayrı bir yerel exporter build'inden, yalnız GitHub release'inde (bu PR'ın ilk hâli) | Alınmadı | İkinci build'in dağıtılan digest'e bağı yalnız bir metin dosyasındaydı. |
| D. GitHub attestation (`actions/attest-*`) | Şimdilik yok | Private repoda Free planda çalışmaz. Repo public olur ya da Enterprise'a geçilirse eklenir; tag'in manifest'ini değiştirmez ve imzalıdır. |
| E. cosign ile imzalı attestation | Onay gerekir | Yeni araç (Karar 6); bu PR'da sorulmadı. |

### 4. Dokploy dağıtımı yazılıdır ama kapalıdır

`deploy` job'ı yalnız iki koşul birlikte sağlanınca koşar: depo değişkeni `DOKPLOY_DEPLOY_ENABLED` `true` ve sürüm kararlı (pre-release değil). Sırrı `DOKPLOY_DEPLOY_WEBHOOK` adıyla okur.

Webhook yalnız Dokploy'da yapılandırılmış imajı yeniden dağıtır. Faz 9'da deploy açılmadan önce şunlar yazılır:
- Dokploy'a `<version>@<digest>` referansını veren adım;
- DB adımının (`scripts/release-db.sh`) yeri;
- webhook'un HTTPS bir alan adına taşınması. Java'nın webhook'u ham IP ve HTTP üzerindeydi ve GitHub runner'larından erişilemedi.

### 5. Yetkiler en azdır; kapı hataya karşı korur, kötü niyete karşı değil

`release.yml` ve `release-tag-guard.yml`'nin üst düzeyi `permissions: {}`'dir. Job'ların yetkileri:

| Job | Yetki |
|---|---|
| `gate` | `contents: read`, `actions: read` |
| `publish` | `contents: read`, `packages: write` |
| `tag` | `contents: write`, `packages: read` |
| `deploy` | yok |
| guard | `contents: read`, `packages: read` |
| `verify.yml` `image` | `contents: read` |

Free plan ve private repo olduğu için şunların hiçbiri yok: branch protection, tag ruleset'i, environment onayı. Bu yüzden:
- Yazma yetkisi olan herkes şunları yapabilir: `main`'e doğrudan push, elle `v*` tag'i, workflow dosyasını değiştiren bir dalda workflow koşturmak. Böyle bir koşu depo düzeyindeki sırları (`DOKPLOY_DEPLOY_WEBHOOK`) da okuyabilir.
- `gate` ve guard **kazaya karşı korur** (yanlış commit, verify'sız SHA, imajsız tag), **kötü niyete karşı korumaz**.

Gerçek koruma için Team planı ya da public repo gerekir. Planlanan önlemler: branch protection, `v*` ruleset'i, `production` environment'ı ve sırrın oraya taşınması (runbook'taki insan adımları).

### 6. Yeni karar: action ve CI imajı politikası

ADR-0003 Go bağımlılıkları içindir; CI'ın action'ları ve imajları için ayrı bir kural yoktu. Bu ADR şunu kaydeder:

- **GitHub Action'ları** yalnız resmî `actions/*` ve `docker/*` olabilir. Commit SHA'sıyla sabitlenir, yanına sürüm yorumu yazılır. Başka bir action ADR-0003 madde 3'teki gibi ayrı onay ister.
- **CI'da çalışan imajlar** (service container'lar, BuildKit, SBOM üreteci, taban imajlar) etiket ve digest'le sabitlenir. Yeni bir imaj ya da araç (ör. cosign, skopeo, oras) ayrı onay ister.
- Sürüm seçiminde **ADR-0017'nin 14 gün kuralı** uygulanır: en az 14 gün önce yayımlanmış en yeni sürüm alınır. İstisna, güvenlik düzeltmesidir.

Bu PR'da kural şöyle uygulandı:

| Ne | Sürüm | Yayın |
|---|---|---|
| `docker/setup-buildx-action` | v4.4.1 | 16.09.2026 |
| `docker/login-action` | v4.6.0 (v4.7.0 23.10'da çıktı, 14 günü doldurmadı) | 29.07.2026 |
| `actions/upload-artifact` | v7.0.2 | 07.10.2026 |
| `actions/download-artifact` | v8.0.2 | 07.10.2026 |
| `moby/buildkit` (builder) | v0.34.1, digest'le (v0.34.2 03.11'de çıktı) | 20.10.2026 |
| `docker/buildkit-syft-scanner` | 1.12.0, digest'le (1.12.1 26.10'da çıktı) | 13.08.2026 |
| `registry` (job registry'si) | 3.1.1, digest'le (3.1.2 26.10'da çıktı) | 21.09.2026 |

### 7. `internal/platform/clock`

- `Clock` arayüzü `Now`, `NewTimer` ve `NewTicker`'dan oluşur. `Real` sistem saatidir.
- `*Fake` yalnız `Advance` ile ilerler ve geçtiği zamanlayıcıları ve tick'leri sırayla tetikler.
- Go 1.23+ semantiği uygulanır: zamanlayıcı kanalı eşzamanlıdır, değeri alınmamış zamanlayıcı henüz ateşlenmemiş sayılır. Bu yüzden `Stop` ve `Reset` böyle bir zamanlayıcı için `true` döner ve bayat değeri düşürür. `TestRealTimerStopSemanticsMatchTheFake`, `time.Timer`'ın aynı davrandığını sabitler.
- `BlockUntil(ctx, n)` bekleyen zamanlayıcı sayısı n'e ulaşana kadar bekler. Sınırları paket belgesinde:
  - saatteki bütün zamanlayıcıları sayar;
  - durdurulmamış ticker sayımda kalır;
  - uzun `Advance` her ticker periyodu için bir tur döner.
- `realtime.Clock` (`Now()`) tüketici tarafındaki arayüz olarak kalır; `Real` ve `*Fake` onu karşılar. realtime testlerindeki yerel `fakeClock`'un bu pakete geçmesi o paketin işidir.

### 8. `internal/platform/fakes`

httptest tabanlıdır; gerçek istemcinin konuşacağı HTTP sunucusudur.

- **Ortak `Server`:**
  - istek kaydı, yol başına sıralı senaryo (`Enqueue`);
  - `Hang`: istemci vazgeçene kadar yanıtsız kalır, belirsiz sonuçlu zaman aşımını canlandırır;
  - `Drop`: bağlantıyı yanıtsız kapatır.
  - Programlanmamış istek 501 alır ve testi cleanup'ta kırmızı yapar. 1 MiB'ı aşan gövde 413 alır ve aynı şekilde kırmızı yapar.
- **`Iyzico`:**
  - Her isteğin IYZWSv2 imzasını doğrular. imza büyük/küçük harfe duyarlı karşılaştırılır, apiKey sabit zamanlı karşılaştırılır (`crypto/subtle`).
  - Kötü imza 401 alır, programlanmış yanıtı tüketmez ve **varsayılan olarak testi cleanup'ta kırmızı yapar**. İmza hatasını sınayan test bunu `ExpectAuthFailures()` ile bilinçli açar.
  - Initialize ve retrieve yanıtlarını Java'nın alan sırasıyla imzalar; ondalıkları `stripTrailingZeros().toPlainString()` gibi yazar.
  - `testdata/iyzico/vectors.json`'daki IR-01, IR-02, IV-01–IV-04 ve normalizasyon satırlarıyla test edilir.
- **`FCM`:**
  - OAuth 2.0 token ucu ve `messages:send` sunar; yalnız kendi verdiği bearer'ı kabul eder.
  - Tek hedef ve string `data` denetlenir.
  - `FailToken` cihaz başına kalıcı hata verir. Hata gövdesi Google'ınkidir, `FcmError` ayrıntısıyla.
- Protokol yardımcıları dışa açılmaz, böylece Faz 5–6 istemcisi bu paketi import edemez. Kural tek bir vektör dosyasında durur.
- `TestOnlyTestsImportFakes`, fakes ağacı dışında test olmayan hiçbir dosyanın bu paketi ya da alt paketlerini import etmediğini denetler.

## Sonuçlar

- Faz 1'in imaj kriterinin üç parçası her PR'da ölçülür: distroless nonroot, açılış <1 sn, `/readyz` drain.
- `scripts/release-db.sh` gerçek imajla her PR'da koşar. ADR-0017 Sonuçlar'daki "yayın hattı kurulduğunda bağlanır" maddesinin CI yarısı tamamlandı; production yarısı Faz 9'da.
- Basma yolu (etiketsiz basma, denetimler, iki etiket) her PR'da job registry'sine karşı sınanır. İlk gerçek yayına kadar yalnız GitHub'a özgü parçalar uçtan uca sınanmamış olacak: `gate`, GHCR girişi, `tag`, guard ve `deploy`.
- Yarıda kalan yayının durumu runbook'tadır: imaj ve etiketler var ama git tag'i yok. Bu durumda yeni dispatch "zaten var" ile durur; yalnız başarısız `tag` job'ı yeniden koşturulur.
- **Açık sorular (Faz 9):**
  - `release-db.sh` production DB'ye karşı nerede koşacak? GitHub runner DB'ye erişemez.
  - Dokploy'a `<version>@<digest>` hangi yoldan verilecek?
  - Webhook HTTPS bir alan adına taşınacak mı?
- **Kanıtlanmadı:** GITHUB_TOKEN'ın `ghcr.io/is-koala/koala-api` paketini ilk kez yaratması. Bu, organizasyonun paket oluşturma ayarına bağlı; runbook'taki insan adımı.
