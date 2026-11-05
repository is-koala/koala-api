# Yayın: imaj, tag ve dağıtım

Bu sayfa açıklamadır; kararlar ve gerekçeleri ADR-0023'te, DB adımı ADR-0017'de ve [veritabanı runbook'unda](veritabani.md). İmaj bu Mac'te build edilmez; her adım GitHub Actions'ta koşar.

## Akış

1. PR açılır. `koala-api verify` iki job koşar: `verify` (`make verify` + entegrasyon testleri) ve `image` (imaj build'i, smoke testi, yayın yolunun job registry'sine provası). İmaj boyutu ve açılış süresi `image` job'ının özetindedir.
2. PR squash merge edilir. `main` push'u aynı workflow'u yeni SHA için yeniden koşar; yayının kanıtı bu koşudur.
3. Yayınlayan kişi `main`'deki bir commit'e annotated tag atar ve push'lar:

   ```sh
   git fetch origin
   git tag -a v0.1.0 -m v0.1.0 origin/main
   git push origin v0.1.0
   ```

4. `koala-api release` (`.github/workflows/release.yml`) koşar:
   - `gate`:
     - tag `vMAJOR.MINOR.PATCH[-PRERELEASE]` biçiminde mi?
     - commit `main`'de mi?
     - aynı SHA'da yeşil `push: main` verify koşusu var mı?
   - `publish`:
     - GHCR'de aynı tag varsa durur;
     - build → smoke → `ghcr.io/is-koala/koala-api:<tag>` adresine basar; pre-release değilse `:latest`'e de basar;
     - tek manifest ve digest eşitliğini denetler;
     - SBOM ve provenance üretir.
   - `github-release`: tag için GitHub release'i açar. Ekleri: `image.json` (digest), `sbom.spdx.json`, `provenance.json` ve `SHA256SUMS`.
   - `deploy`: Dokploy webhook'u. **Faz 9'a kadar kapalı** (aşağıda).

## Bir şey ters giderse

| Belirti | Ne yapılır |
|---|---|
| `gate`: "No green verify run" | Tag, `main` verify'ı bitmeden basıldı. Verify yeşile dönünce release koşusunda **Re-run all jobs**. |
| `gate`: "not on main" | Tag yanlış commit'te. Tag'i sil (`git push --delete origin vX.Y.Z`), `main`'deki commit'e yeniden at. |
| `publish`: "already exists" | Bu tag basılmış; basılmış imaj değiştirilmez. Yeni bir tag (yama sürümü) at. |
| `publish`: smoke kırmızı | GHCR'ye hiçbir şey basılmadı. Hatayı düzelt, merge et, yeni tag at. |
| `publish`: "is an index" ya da digest uyuşmazlığı | Basılan nesne Dokploy'a uygun değil. Dağıtma; build argümanlarını incele (ADR-0023 Karar 2). |
| `github-release` kırmızı, `publish` yeşil | İmaj yerinde. Release koşusunda yalnız başarısız job'ı yeniden koştur. |

Bir release'in eklerini doğrulamak için:

```sh
gh release download vX.Y.Z -R is-koala/koala-api -D /tmp/rel && (cd /tmp/rel && sha256sum -c SHA256SUMS)
jq -r .reference /tmp/rel/image.json   # ghcr.io/is-koala/koala-api@sha256:...
```

## Dokploy

- Konteynerin durdurma süresi `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` toplamından uzun olmalı (README'deki formül). Varsayılanlarla toplam 40 sn olduğu için **`stop_grace_period` en az 45 sn** verilir. Smoke testi `docker stop -t 45` ile aynı süreyi kullanır.
- Sağlık kontrolü imajın kendisindedir (`koala healthcheck`, `/livez`). Dokploy'da ayrıca HTTP sağlık kontrolü tanımlanırsa `/livez` kullanılır, `/readyz` kullanılmaz: DB kesintisi konteyneri yeniden başlatmamalı.
- Webhook, yeni imajı çekmeden önce DB adımını (`scripts/release-db.sh`) çalıştırmaz. Faz 9'da açılmadan önce DB adımının yeri kararlaştırılmalı (ADR-0023 açık sorular).

## Yusuf'un yapması gerekenler (insan adımları)

- [ ] **GHCR paketi.** İlk tag'den sonra `ghcr.io/is-koala/koala-api` paketi oluşur.
  - Kontrol: Paket ayarlarında görünürlük **Private** kalsın, "Inherit access from source repository" açık olsun ve paket `is-koala/koala-api`'ye bağlı olsun. İmajdaki `org.opencontainers.image.source` etiketi bağı kendisi kurar.
  - İlk push 403 ile düşerse, organizasyonun **Settings → Packages → Package creation** ayarında private paket oluşturma açık olmalı.
  - Dokploy'un çekebilmesi için Dokploy registry ayarında yalnız `read:packages` yetkili bir token gerekir. Java'nın kullandığı token yeterli olabilir; token sahibinin yeni pakete erişimi doğrulanmalı.
- [ ] **Branch protection.** Free planda private repo için yok (API 403). Seçenekler: organizasyonu **Team** planına geçirmek ya da repoyu public yapmak. Açılınca:
  - `main` için şunlar zorunlu olsun:
    - PR,
    - `verify` ve `image` kontrollerinin yeşili,
    - force-push ve silme yasağı.
  - `v*` tag'leri için bir ruleset eklenmeli: oluşturma yalnız yayınlayanlara açık, silme ve güncelleme yasak.
  - O güne kadar `gate`, tag'i yalnız "main'de ve verify yeşil" koşuluyla sınırlar.
- [ ] **Dokploy `stop_grace_period` ≥ 45 sn.** Formül: `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` toplamından uzun; varsayılanlarla 5 + 35 = 40 sn.
- [ ] **Webhook sırrı ve anahtarı (Faz 9).**
  - Repo **Settings → Secrets and variables → Actions** altında `DOKPLOY_DEPLOY_WEBHOOK` sırrı tanımlanır. Değeri Dokploy'daki uygulamanın deploy webhook URL'sidir.
  - Deploy'u açmak için aynı yerde **Variables** altında `DOKPLOY_DEPLOY_ENABLED` = `true` tanımlanır. Değişken yoksa `deploy` job'ı atlanır.
