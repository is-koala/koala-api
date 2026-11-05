# Yayın: imaj, tag ve dağıtım

Bu sayfa açıklamadır. Kararlar ve gerekçeleri ADR-0023'te; DB adımı ADR-0017'de ve [veritabanı runbook'unda](veritabani.md). İmaj bu Mac'te build edilmez; her adım GitHub Actions'ta koşar.

**Kural: imaj GHCR'de yokken `v*` tag'i olmaz.** Tag'i yayın workflow'u, imaj basılıp denetlendikten sonra kendisi atar. Elle tag atılmaz.

## Akış

1. PR açılır. `koala-api verify` dört job koşar; merge için dördü de yeşil olmalı (CLAUDE.md):
   - `verify`: `make verify`;
   - `integration`: Postgres'e karşı entegrasyon testleri (`make test-integration`);
   - `image`: build, smoke ve yayın yolunun job'a özel geçici registry'de provası. Bu job GHCR'ye basmaz. İmaj boyutu ve açılış süresi job özetindedir;
   - `authz-conformance`: authz matrisi Go değerlendiricisinde ve gerçek OpenFGA'da (`make test-conformance`, ADR-0024).
2. PR squash merge edilir. `main` push'u aynı workflow'u yeni SHA için koşar; yayının kanıtı bu koşudur. Bitmesini bekle.
3. Yayın `main`'den dispatch edilir:

   ```sh
   gh workflow run "koala-api release" -R is-koala/koala-api --ref main -f version=v0.1.0
   ```

   Ya da Actions → koala-api release → Run workflow (branch `main`, version).
4. `koala-api release` sırayla şu job'ları koşar:
   - `gate`:
     - dispatch `main`'den mi?
     - sürüm SemVer mi, yeni mi, en büyük sürümden büyük mü?
     - aynı SHA'da yeşil `push: main` verify koşusu var mı?
   - `publish`:
     - `:<sürüm>` ya da `:<sürüm>-attest` varsa durur;
     - build → smoke → etiketsiz basma → denetimler: tek imaj manifest'i, smoke edilen imajla aynı katman/config/`/koala`, SBOM, provenance;
     - ancak sonra `ghcr.io/is-koala/koala-api:<sürüm>` (imaj) ve `:<sürüm>-attest` (attestation indeksi) etiketlerini atar. `:latest` yok.
   - `tag`: imajın beklenen digest'le durduğunu denetler, annotated `v*` tag'ini atar, GitHub release'ini açar. Ekler: `image.json`, `sbom.spdx.json`, `provenance.json`, `SHA256SUMS`.
   - `deploy`: yalnız kararlı sürümde (pre-release'te asla) ve yalnız `DOKPLOY_DEPLOY_ENABLED=true` iken koşar. **Faz 9'a kadar kapalı** (aşağıda).
5. Dağıtılan referans her zaman değişmez `ghcr.io/is-koala/koala-api:<sürüm>@<digest>`'tir; `image.json`'daki `reference` alanı.

## Bekleyen koşular iptal olabilir

GitHub, bir concurrency grubunda en fazla bir çalışan ve bir bekleyen koşu tutar.
- **Verify:** `main`'e merge'ler verify'dan hızlı gelirse yeni bekleyen koşu eskisinin yerini alır. Eskisi "cancelled" biter ve o SHA'nın yeşil `push: main` koşusu olmaz. O SHA yayınlanacaksa koşuyu Actions'ta **Re-run all jobs** ile yeniden koştur. Yayın her zaman `main`'in o anki HEAD'inden dispatch edildiği için bu genelde sorun çıkarmaz.
- **Yayın:** Bir yayın sürerken iki dispatch daha gelirse bekleyen ilk dispatch iptal olur. Yeniden dispatch et.

## Yayın başarısız olursa

| Nerede | Durum | Ne yapılır |
|---|---|---|
| `gate`: "Dispatch from main" | Başka daldan dispatch edildi. | `--ref main` ile yeniden dispatch et. |
| `gate`: "not greater than" / "already exists" | Sürüm eski ya da kullanılmış. | Daha büyük bir sürümle dispatch et. |
| `gate`: "No green verify run" | `main` verify'ı bitmedi, kırmızı ya da iptal oldu. | Bitmesini bekle; iptal olduysa Re-run et; kırmızıysa düzelt ve merge et. Sonra yeniden dispatch et. |
| `publish`: "already exists" | Bu sürümün imajı basılmış. Ya yayın tamamlanmış, ya da önceki koşu `tag`'de kaldı. | Releases'ta tag varsa yeni sürüm seç. Yoksa önceki koşunun **yalnız başarısız job'ını** (Re-run failed jobs) koştur; o koşunun artifact'ı ve digest'i yerinde. Önceki koşu yoksa ya da silindiyse: GHCR'deki digest'i `image.json`'daki revizyonla karşılaştır, sonra elle annotated tag at (guard denetler) ya da GHCR'deki o sürümü sil ve yeniden dispatch et. |
| `publish`: smoke kırmızı | GHCR'ye hiçbir şey basılmadı. | Düzelt, merge et, aynı sürümle yeniden dispatch et. |
| `publish`: basma sonrası denetim kırmızı ("differs from the smoke-tested", "not a single image manifest", SBOM/provenance) | Etiketsiz bir digest GHCR'de kaldı; etiket ve tag atılmadı. | Dağıtma. Nedeni bul ve düzelt. GHCR'deki etiketsiz sürüm paket ayarlarından silinebilir. |
| `tag` kırmızı | İmaj ve etiketler var, git tag'i ya da release yok. | Aynı koşuda **Re-run failed jobs**. Tag atıldıysa ama release açılmadıysa tag'i elle silme; `gh release create <sürüm> --verify-tag` ile ekleri koşunun artifact'ından yükle. |
| `deploy` kırmızı | Yayın tamam, dağıtım olmadı. | Faz 9 notlarına bak. |

Bir release'in eklerini doğrulamak için:

```sh
gh release download vX.Y.Z -R is-koala/koala-api -D /tmp/rel && (cd /tmp/rel && sha256sum -c SHA256SUMS)
jq -r .reference /tmp/rel/image.json        # ghcr.io/is-koala/koala-api:vX.Y.Z@sha256:...
docker buildx imagetools inspect ghcr.io/is-koala/koala-api:vX.Y.Z-attest --format '{{json .Provenance}}'
```

## Elle atılan tag

Elle atılan `v*` tag'i hiçbir şey build etmez. `koala-api release tag guard` şu durumlarda kırmızı olur:
- tag **annotated değil**;
- tag'in commit'i `main`'de değil;
- `:<tag>` imajı yok;
- imajın `org.opencontainers.image.revision` etiketi tag'in commit'i değil.

Guard tag'i engelleyemez. Kırmızıysa tag'i sil (`git push --delete origin vX.Y.Z`) ve yayını dispatch et. Elle tag yalnız yukarıdaki "`tag`'de kalmış koşu" durumunda, imaj doğrulandıktan sonra atılır:

```sh
git tag -a vX.Y.Z -m vX.Y.Z <commit>   # annotated; -a şart
git push origin vX.Y.Z
```

## Kapının sınırı

Repo private ve organizasyon Free planında. Bu yüzden branch protection, tag ruleset'i ve environment onayı yok. Yazma yetkisi olan biri şunları yapabilir: `main`'e doğrudan push, elle tag, ya da workflow'u değiştirip bir dalda koşturmak. Sonuncusu depo sırlarını da okuyabilir. `gate` ve guard kazaya karşı korur, kötü niyete karşı korumaz (ADR-0023 Karar 5).

**Sırlar environment'sız.** `DOKPLOY_DEPLOY_WEBHOOK` depo düzeyinde bir sırdır. Herhangi bir daldaki workflow onu okuyabilir; onay ya da dal kısıtı yoktur. Team planına geçilince sır bir `production` environment'ına taşınmalı: yalnız `main`, gerekirse onaylı.

## Dokploy

**Ortam değişkenleri.** Tam liste README'deki "Yapılandırma" tablosundadır; bilinmeyen `KOALA_*` değişkeni açılışı durdurur. Burada yalnız adlar ve production kuralı yazılır, değer yazılmaz.

| Değişken | Production | Not |
|---|---|---|
| `KOALA_ENV` | **her ortamda zorunlu** (`local`, `test`, `staging`, `production`) | Yoksa hiçbir alt komut açılmaz. |
| `KOALA_DATABASE_URL` | zorunlu (`serve`) | sır; production'da `sslmode=require`, `verify-ca` ya da `verify-full` |
| `KOALA_MIN_APP_VERSION` | **zorunlu (`serve`)** | En düşük desteklenen mobil sürüm (semver). Yoksa `serve` açılmaz: ayarsız kapı her derlemeyi geçirirdi (ADR-0021). Mobil zorunlu güncelleme bu değeri yükselterek yapılır. |
| `KOALA_TRUSTED_PROXIES` | **zorunlu (`serve`)** | Traefik'in (ve önünde CDN varsa onun) adresleri ya da CIDR'ları, virgülle; IPv4'te en geniş /8, IPv6'da /32. Yoksa `serve` açılmaz: bütün istemciler proxy'nin tek oran sınırı kovasını paylaşırdı (ADR-0021). **Staging:** kod zorunlu tutmaz ama staging de Traefik arkasındaysa aynı değer verilir; verilmezse staging'deki oran sınırı ölçümleri anlamsızdır. |
| `KOALA_METRICS_ADDR` | isteğe bağlı (`:9090`) | `/metrics`'in iç portu; aşağıdaki ağ ayarına bakın. |
| `KOALA_SHUTDOWN_DRAIN_DELAY` | isteğe bağlı (`5s`) | `/readyz` 503'ten dinleyicinin kapanmasına kadar geçen süre. |
| `KOALA_SHUTDOWN_TIMEOUT` | isteğe bağlı (`35s`, en az `35s`) | Süren isteklere ve WebSocket'lere verilen süre; stream'lerin zorla kapatma payı (5 sn) bunun içindedir (ADR-0022). `stop_grace_period` bu ikisinin toplamından uzun olmalı (aşağıda). |
| `KOALA_HTTP_ADDR`, `KOALA_LOG_LEVEL` | isteğe bağlı (`:8080`, `info`) | |
| `KOALA_OWNER_DATABASE_URL`, `KOALA_BOOTSTRAP_DATABASE_URL` | yalnız `migrate`/`maint` ve `bootstrap` adımında | sır |

**Metrik portunun ağ ayarı (Dokploy).** `KOALA_METRICS_ADDR` portu Dokploy'da **yayımlanmaz** (port mapping yok) ve Traefik'e domain/route olarak **bağlanmaz**. Prometheus konteyneri uygulamayla aynı iç Docker ağına (Dokploy'un uygulama ağı ya da ayrı bir `monitoring` ağı) alınır ve `koala-api:9090`'ı oradan kazır. Uygulamanın kendisi bu ayrımı yapamaz: dinleyici bütün arayüzlerde açıktır; kısıt ağ ayarıdır.

- Konteynerin durdurma süresi `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` toplamından uzun olmalı (README'deki formül). Varsayılanlarla toplam 40 sn olduğundan **`stop_grace_period` en az 45 sn** verilir. Smoke testi SIGTERM'den sonra 50 sn bekler ve drain'i (`/readyz` 503, `/livez` 200) yoklar.
- Sağlık kontrolü imajın kendisindedir (`koala healthcheck`, `/livez`). Dokploy'da ayrıca HTTP sağlık kontrolü tanımlanırsa `/livez` kullanılır, `/readyz` değil: DB kesintisi konteyneri yeniden başlatmamalı.
- Dokploy'a verilen imaj `ghcr.io/is-koala/koala-api:<sürüm>@<digest>`'tir. Hareketli etiket yoktur.
- **Faz 9 notları (deploy açılmadan önce):**
  - Webhook yalnız Dokploy'da yapılandırılmış imajı yeniden dağıtır. Yeni `<sürüm>@<digest>`'i Dokploy'a veren adım yazılmalı.
  - DB adımı (`scripts/release-db.sh`) yeni imaj trafiğe açılmadan önce koşmalı. GitHub runner production DB'ye erişemez; yeri kararlaştırılmalı.
  - Java'nın deneyimi: v0.9.32'de webhook ham bir IP adresinde, HTTP ve 3000 portu üzerindeydi; GitHub runner'larından **erişilemedi** ve dağıtım elle yapıldı. Webhook HTTPS bir alan adına taşınmadan `deploy` açılmamalı.

## Yusuf'un yapması gerekenler (insan adımları)

- [ ] **GHCR paketi.** İlk yayından sonra `ghcr.io/is-koala/koala-api` oluşur.
  - Kontrol:
    - görünürlük **Private** kalmalı;
    - "Inherit access from source repository" açık olmalı;
    - paket `is-koala/koala-api`'ye bağlı olmalı. İmajdaki `org.opencontainers.image.source` etiketi bağı kendisi kurar.
  - İlk basma 403 ile düşerse: organizasyonun **Settings → Packages → Package creation** ayarında private paket oluşturma açık olmalı.
  - Dokploy için registry ayarında yalnız `read:packages` yetkili bir token gerekir. Token sahibinin yeni pakete erişimi doğrulanmalı.
- [ ] **Branch protection.** Free planda private repo için yok (API 403). Team planı ya da public repo gerekir. Açılınca:
  - `main`: PR zorunlu, `verify`, `integration`, `image` ve `authz-conformance` kontrollerinin yeşili zorunlu, force-push ve silme yasak;
  - `v*` tag'leri için ruleset: oluşturma yalnız GitHub Actions'a ve yayınlayanlara açık, silme ve güncelleme yasak;
  - `production` environment'ı ve sırların oraya taşınması.
- [ ] **Production ortam değişkenleri.** Dokploy'da `KOALA_MIN_APP_VERSION` ve `KOALA_TRUSTED_PROXIES` tanımlanır (yukarıdaki tablo); ikisi yoksa `serve` açılmaz. Prometheus `KOALA_METRICS_ADDR` portunu iç ağdan kazır; port proxy'ye açılmaz.
- [ ] **Dokploy `stop_grace_period` ≥ 45 sn.** Formül: `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` toplamından uzun; varsayılanlarla 5 + 35 = 40 sn.
- [ ] **Webhook sırrı ve anahtarı (Faz 9).**
  - Repo **Settings → Secrets and variables → Actions** altında `DOKPLOY_DEPLOY_WEBHOOK` sırrı tanımlanır. Değeri Dokploy'daki uygulamanın HTTPS deploy webhook URL'sidir.
  - Deploy'u açmak için aynı yerde **Variables** altında `DOKPLOY_DEPLOY_ENABLED` = `true` tanımlanır. Değişken yoksa `deploy` job'ı atlanır; pre-release'te hiç koşmaz.
