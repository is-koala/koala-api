# ADR-0017: River + goose entegrasyonu, DB rol modeli ve test DB altyapısı

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K3, K6, K7, K14 (ayrıntı). Şu kalemlerin yerine geçer:
  - [ADR-0003](0003-dependency-policy.md) madde 6 (test DB'si: testcontainers ya da docker CLI);
  - [ADR-0004](0004-quality-gate-and-tool-pinning.md) Karar 1'deki "tek adım";
  - [ADR-0012](0012-data-access-and-migrations.md) Karar 3'teki migration adları (`0001_baseline.sql`, `0002_seed.sql`);
  - ADR-0012 Karar 4'teki `InTx` imzası (`func(q Querier) error`).
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §3.5, §3.6-3, §6.1 (Faz 0 ve Faz 1), §9 R7; [ADR-0010](0010-job-queue-river.md), [ADR-0012](0012-data-access-and-migrations.md), [ADR-0013](0013-soft-delete.md); River'ın [migration belgesi](https://riverqueue.com/docs/migrations)
- Kod: `db/`, `internal/platform/{db,jobs,dbtest}`, `internal/app/{bootstrap,migrate,maint}.go`, `scripts/{release-db,test-integration}.sh`, [runbook](../runbook/veritabani.md)

## Bağlam

ADR-0010 Karar 5 River şemasının goose'a SQL olarak alınmasını Faz 0 spike'ına bağladı. ADR-0012 `koala migrate`'i yer tutucu bıraktı. ADR-0013 rol modelini (`koala_app`, `koala_maint`) tarif etti ama kurulumunu Faz 1'e bıraktı.

Spike bunları aynı DB'de birlikte kurdu. Kararları uygulamaya dökerken birkaç yeni karar gerekti: bağımlılık sürüm kuralı, rollerin üretimde kurulması, River indeks bakımı ve test DB'sinin CI'da nasıl koşacağı. Bu ADR hepsini kaydeder; ADR-0010, 0012 ve 0013 değişmez.

## Karar

### 1. River şeması goose dosyalarında

| Seçenek | Artı | Eksi |
|---|---|---|
| **A. River SQL'ini goose dosyalarına almak** (`river migrate-get`) | Tek migration geçmişi, tek araç, tek sıra. Şema PR'da SQL olarak incelenir. | River sürümü yeni migration getirince elle alınır. |
| B. `rivermigrate`'i goose'tan önce ya da sonra çalıştırmak | River kendi sürümünü izler. | İki geçmiş (`goose_db_version`, `river_migration`), iki sıra. River tablolarına GRANT veren migration'ımız sıraya bağımlı olur; down yolu iki araca bölünür. |
| C. goose Go migration'ı içinde `rivermigrate` | River belgesinde örneği var. | `riverdatabasesql` sürücüsü gerekir, onaylı listede değil. Şema SQL olarak görünmez. |

River'ın belgesi harici migration aracı kullananlara A'yı öneriyor: `river migrate-get --line main --all --exclude-version 1`. Sürüm sınırları korunur, her sürüm kendi transaction'ında koşar. 1. sürüm River'ın kendi takip tablosudur (`river_migration`), harici araçla gerekmez; 5. sürüm yokluğunu açıkça tolere ediyor. Elixir'de Oban da kuyruk şemasını uygulamanın tek migration zincirine koyuyor.

**Karar: A.**
- Her River sürümü ayrı bir goose dosyasıdır: `NNNN_river_main_vVVV.sql`. Dosya tek `StatementBegin/End` bloğudur, `$$` gövdeleri bölünmez.
- 1. sürüm alınmaz. River client'ı çalışırken `river_migration`'a bakmıyor.
- **Kayma testi** (`db/migrations/migrations_test.go`, DB'siz) goose dosyalarını `go.mod`'daki River sürümünün SQL'iyle karşılaştırır. Eksik sürüm, fazla dosya, yanlış sıra ya da elle değişmiş SQL kırmızıdır.
- Yükseltme adımı: `go get` → kayma testi kırmızıysa `river migrate-get --line main --version N --up/--down` ile yeni dosya → yeni tablo ya da fonksiyon varsa yeni GRANT migration'ı.
- Büyük `river_job` tablosuna indeks ekleyen bir River sürümünde River CHANGELOG'unun yolu izlenir: indeks önce `-- +goose NO TRANSACTION` dosyasında `CONCURRENTLY` kurulur, River'ın dosyası ardından gelir (`IF NOT EXISTS` kullanıyor).

### 2. Bağımlılık sürümü: en az iki haftalık en yeni sürüm

Yeni bir bağımlılık ya da yükseltme, yayımlanmasının üzerinden **en az 14 gün geçmiş en yeni sürümle** alınır. Gerekçe tedarik zinciri pratiği: geri çekilen, gerileme getiren ya da ele geçirilmiş hesapla yayımlanan sürümler genellikle ilk günlerde fark edilir.

İstisna: govulncheck bulgusunu kapatan güvenlik yaması. O durumda yayın tarihi PR gövdesine yazılır.

Bu PR'daki sürümler (05.11.2026):

| Modül | Sürüm | Yayın |
|---|---|---|
| `github.com/riverqueue/river`, `riverdriver/riverpgxv5` | v0.49.0 | 05.10.2026 |
| `github.com/jackc/pgx/v5` | v5.11.0 | 07.09.2026 |
| `github.com/pressly/goose/v3` | v3.29.0 | 11.10.2026 |
| `github.com/google/uuid` | v1.6.0 | 23.01.2024 |

River v0.50 ve v0.51 ile pgx v5.12 iki haftayı doldurmadığı için alınmadı. River'ın Postgres migration SQL'i v0.49 ile v0.51 arasında yalnız yorum satırlarında farklıdır.

### 3. Migration düzeni

- `0001_roles_and_schema.sql`: rollerin varlığını ve `current_user = koala_owner` koşulunu denetler, DB ve `public` şemasını kapatır.
- `0002`–`0008`: River ana hattının 2–8. sürümleri.
- `0009_river_grants.sql`: `koala_app`'in River hakları.
- **Faz 1'in temiz baseline'ı ≥ `0010`'dan başlar.** Bu madde ADR-0012 Karar 3'teki ve plan §3.6-1'deki `0001_baseline.sql` ve `0002_seed.sql` adlarının yerine geçer. Temiz baseline kararı (Liquibase taşınmaz, hedef şema elle yazılır) aynen geçerlidir.
- **`migrate down` bir geliştirme aracıdır.** Tek adım geri alır, `KOALA_ENV=production`'da reddedilir. River'ın down migration'ları iş tablolarını satırlarıyla düşürür. `NO TRANSACTION` dosyaları atomik değildir; yarıda kalan `CONCURRENTLY` indeksi `INVALID` kalır ve elle düşürülür. Üretimde geri dönüş ileri bir migration ya da yedekten dönüştür.

### 4. Rol modeli

- **Grup rolleri giriş yapamaz.**
  - `koala_owner` bütün nesnelerin sahibidir.
  - `koala_app` yalnız `active_*` view'ları, River tabloları, River dizileri ve River fonksiyonu üzerinde hak taşır.
  - `koala_maint` temel tablolarda çalışır.
- **Kurulum yayının parçasıdır.**
  - `koala bootstrap` (`db/bootstrap/roles.sql`) idempotenttir ve her yayında `koala migrate up`'tan önce koşar (`scripts/release-db.sh`).
  - Eşzamanlı iki bootstrap bir advisory lock'ta sıraya girer; kilidi almayan bir oturumla `CREATE ROLE` yarışı (`42710`) yutulur.
  - Grup rolünde `LOGIN` bulunursa `NOLOGIN`'e çekilir.
  - Bootstrap `CREATEROLE` yetkili ayrı bir girişle (`KOALA_BOOTSTRAP_DATABASE_URL`) `postgres` DB'sine bağlanır. Rolleri ilk o kurduğu için onlarda `ADMIN` hakkı da vardır; PG16'dan beri var olan rolü değiştirmek bunu ister.
  - Runbook yalnız açıklamadır.
- **Hiçbir giriş süper kullanıcı değildir.** `koala migrate`, `koala maint` ve `koala bootstrap` süper kullanıcı girişini `ErrSuperuserLogin` ile reddeder; `koala_owner`/`koala_maint` havuzları da öyle. Bootstrap için istisna yoktur: rol kurmak `CREATEROLE` ister, süper kullanıcılık gerekmez.
- **Giriş kullanıcıları tek role üyedir.** Sunucu `koala_app`'e, göç ve bakım `koala_owner`'a (`KOALA_OWNER_DATABASE_URL`) üye girişler kullanır.
- **`SET ROLE` güvenlik sınırı değildir.** Oturum `RESET ROLE` ile girişe döner; `SET ROLE` yalnız kazara geniş yetkiyi daraltır. Sınır giriş kullanıcısının kendisidir.
  - `koala_app` havuzu her yeni bağlantıda `session_user`'ı **izin listesine** göre denetler:
    - doğrudan ya da dolaylı rol üyelikleri tam olarak `{koala_app}` olmalı (`pg_read_all_data`, `pg_write_all_data` gibi yerleşik roller de sayılır);
    - `SUPERUSER`, `BYPASSRLS`, `CREATEROLE`, `CREATEDB` ve `REPLICATION` özniteliklerinden hiçbiri olmamalı;
    - kendine ait bir nesnesi ya da tablo, view, dizi veya kolon üzerinde doğrudan yetkisi olmamalı.

    Uymayan bağlantı `ErrLoginTooPrivileged` ile reddedilir.
  - **Gerçek davranış:** `koala serve` açılışta DB'ye ulaşabiliyorsa ve giriş reddediliyorsa çıkış koduyla kapanır. Açılışta DB'ye ulaşamıyorsa sunucu açılır. Sonraki her bağlantı denemesi aynı denetimden geçer: fazla yetkili giriş hiçbir sorgu çalıştıramaz ve `/readyz` 503 kalır.
- **DB düzeyi:** `PUBLIC`'in `CONNECT` ve `TEMPORARY` hakkı kaldırılır; `CONNECT` yalnız `koala_app` ve `koala_maint`'e verilir. Sahip zaten bağlanabilir. `koala_owner`'ın sonradan kurduğu fonksiyonların `PUBLIC` `EXECUTE` hakkı varsayılan yetkilerle kaldırılır; her `EXECUTE` açıkça verilir.
- **Ortam başına ayrı cluster.** Roller cluster düzeyindedir. Staging ve production aynı cluster'ı paylaşırsa `koala_app` iki ortamın verisine birden erişir. Her ortamın kendi Postgres cluster'ı vardır.
- **Production'da TLS zorunludur.** Her DB URL'si `sslmode=require`, `verify-ca` ya da `verify-full` taşır; yoksa yapılandırma reddedilir. Bir bağlantı parametresi URL'de iki kez geçerse (ör. `sslmode=disable&sslmode=require`) her ortamda reddedilir: hangi değerin kazanacağı sürücüye göre değişir.

### 5. Oturum durumu: yalnız `SET LOCAL`

- Uygulama kodu oturum düzeyinde `SET` çalıştırmaz; ayar gerekirse `InTx` içinde `SET LOCAL`'dır.
  - Tek istisna bağlantı açılırken çalışan `SET ROLE`'dür.
  - Havuz bağlantıyı her verişinde `current_user`'ı denetler. Rol değişmişse sorgu `ErrRoleChanged` ile düşer ve bağlantı yok edilir.
  - Bu denetim her edinime bir gidiş-dönüş ekler; bilinçli bir bedeldir.
- **PgBouncer'ın transaction modu bu modeli bozar.** Rol ve oturum ayarları sunucu bağlantısına bağlıdır. Transaction modunda sunucu bağlantısı istemciler arasında paylaşılır: bir istemcinin `SET ROLE`'ü başkasına sızar ya da hiç uygulanmaz. Koala'nın bağlantıları Postgres'e doğrudan ya da PgBouncer'ın session modundan gider.

### 6. Havuz ayarları

URL'deki değer (`pool_max_conns`, `application_name` ya da ayarın kendisi) önceliklidir.

| Rol | `MaxConns` | `statement_timeout` | `idle_in_transaction_session_timeout` | `application_name` |
|---|---|---|---|---|
| `koala_app` | 10 | 20 sn (`httpx.RequestTimeout` 25 sn'nin altında) | 10 sn | `koala-api/app` |
| `koala_maint` | 2 | 5 dk | 1 dk | `koala-api/maint` |
| `koala_owner` | 2 | yok (migration, `REINDEX CONCURRENTLY`) | 1 dk | `koala-api/owner` |

River worker'ları `koala_app` havuzunu kullanır; 20 sn'yi aşan sorgu gereken iş toplu (batch) çalışır.

### 7. Transaction: `db.InTx(ctx, func(ctx, q Querier) error)`

Bu imza ADR-0012 Karar 4'teki `db.InTx(ctx, func(q Querier) error)` imzasının yerine geçer. Karar 4'ün geri kalanı (tx servis sınırında açılır, dış çağrı tx dışında, açık kilit) aynen geçerlidir.

- `fn` tx'e işaretli bir context alır. O context'le yeniden `InTx` çağrılırsa `ErrNestedTx` döner: iç çağrı ikinci bir bağlantıda bağımsız bir tx açardı. `Querier` aşağı geçirilir.
- `fn`'in hatası değişmeden döner. Panikte rollback yapılır ve panik sürer. İptal edilmiş context'te oluşan "conn closed" rollback gürültüsü hataya eklenmez. Rollback gerçekten başarısız olursa sonuç `errors.Join(fnErr, rollbackErr)` olur: `errors.Is`/`errors.As` `fn`'in hatasını (ör. `*apperr.Error`) yine bulur, mesaj ikisini de taşır.
- **Commit hatası belirsizdir.** COMMIT gönderildikten sonra bağlantı koparsa tx işlenmiş olabilir. Tekrarı güvenli olmayan yazım idempotent yapılır (benzersiz anahtar, River `UniqueOpts`) ya da tekrar öncesi sonuç okunur.
- `q.AfterCommit` best-effort işler içindir. Her hook kendi `recover`'ında koşar; panik log'a yazılır, kalan hook'lar çalışır. `fn` döndükten sonra saklanan bir `Querier`'a eklenen hook hiç çalışmaz.
- `q.PgxTx()` River `InsertTx` içindir; `Commit`/`Rollback` `ErrTxManaged` döner. Kazaya karşı korur, güvenlik sınırı değildir: ham `COMMIT` ifadesi yine çalışır.

### 8. `active_` view: doğruluk sınırı, gizlilik sınırı değil

`active_<t>` view'ı ve `koala_app`'in temel tablolarda yetkisiz olması **doğruluğu** korur: silinmiş satır kazayla dönmez, unutulan filtre ilk testte `permission denied` verir. View, kendisine yetkisi olan role karşı bir gizlilik sınırı değildir. Postgres planlayıcısı kullanıcının sızdıran bir fonksiyonunu view'ın `WHERE`'inden önce çalıştırabilir ve gizlenen satırlar görünür olur.

**KVKK kapsamındaki veriyi süzen view'lar `WITH (security_barrier)` ile kurulur.** Hassas kolonlar (TCKN, IBAN şifreli değerleri) gereksizse view'a hiç alınmaz.

### 9. River indeks bakımı: `koala maint reindex`

River'ın süreç içi reindexer'ı kapalıdır (`ReindexerSchedule: river.NeverSchedule()`), çünkü `REINDEX` sahiplik ister. PG17'nin `MAINTAIN` yetkisi yetmez: yarıda kalmış bir `REINDEX CONCURRENTLY`'nin bıraktığı geçersiz `_ccnew`/`_ccold` indekslerini düşürmek `DROP INDEX` ister, o da sahipliktir.

Bakım bu yüzden ayrı bir alt komuttur: `koala maint reindex`. Bu komut:
- `koala_owner` girişiyle çalışır;
- River'ın varsayılan indeks listesindeki her indeks için önce geçersiz artıkları düşürür, sonra `REINDEX INDEX CONCURRENTLY` yapar;
- haftalık zamanlanmış bir ops işi olarak koşar; sunucu süreci bu işi yapmaz.

### 10. Test altyapısı: GitHub Actions service container + template DB

| Seçenek | Artı | Eksi |
|---|---|---|
| **A. `services: postgres` (GitHub Actions)** | Konteyneri runner başlatır, sağlık kontrolü geçene kadar ilk adımı bekletir. Test sürecinde Docker API çağrısı, Ryuk yan konteyneri ve test süresine giren imaj çekme yok. Go bağımlılığı eklemez. Tek sunucu template DB kalıbıyla uyumlu. | Yerelde test için erişilebilir bir Postgres gerekir. |
| B. testcontainers-go | Testler kendi konteynerini kurar, yerelde tek komut. | Test süreci Docker Engine API'sine bağlanır; API sürüm değişiklikleri istemciyi kırabiliyor. Ryuk ve imaj çekme her paket sürecinde zaman ve hata noktası ekler. Büyük modül ağacı `go.sum`'a girer. |
| C. docker CLI (core-backend kalıbı) | Bağımlılık yok. | Konteyner yaşam döngüsü test kodunda elle yönetilir. |

**Karar: A.** Bu karar ADR-0003 madde 6'nın ve plan §3.3'ün "testcontainers ya da docker CLI" kararının yerine geçer. Gerekçe: Koala'da yerelde ağır iş yok, kapı CI'da koşuyor (ADR-0004). Yerelde konteyner kuran bir kütüphanenin getirisi kalmıyor; CI'da daha az hareketli parça var.

- İmaj `postgres:17.11` etiketi ve digest'iyle sabittir (Java tarafıyla aynı ana sürüm). Port yalnız runner'ın loopback'ine bağlanır. `trust` auth kullanılır: hiçbir dosyada parola değeri yok. Testlerin oluşturduğu girişlerin parolaları test sürecinde üretilir.
- **Template DB (`internal/platform/dbtest`):**
  - Rollerin, gerçek ve örnek migration'ların SQL özetiyle adlanan bir template DB kurulur. `koala migrate up`'ın kullandığı `db.Migrator`, `koala_owner` üyesi geçici bir girişle uygular.
  - Her test bu template'i `CREATE DATABASE … TEMPLATE` ile klonlar ve her grup rolü için üretilmiş parolalı yeni bir giriş alır. `CREATE DATABASE` DB düzeyi yetkileri kopyalamadığı için klona template'in DB ACL'i yeniden uygulanır.
  - Template kurma, düşürme ve artık süpürme dışlayıcı advisory lock altında yapılır, klonlama aynı kilidin paylaşımlı hâliyle. Böylece iki şema sürümü aynı sunucuda koşsa da kopyalanan template düşürülmez.
  - DB ve giriş adları oluşturma zamanını taşır. Bir saatten eski olanlar ölmüş bir koşunun artığıdır ve sonraki koşu onları düşürür.
- **Kapı iki adımlıdır:** `make verify` ve ardından `make test-integration`. Bu, ADR-0004'ün "tek adım `make verify`" kaleminin yerine geçer. İkisi de `verify` işinin adımıdır ve PR'ın tek gerekli kontrolü olarak kalır.
- **Ayrım:**
  - Entegrasyon testleri `//go:build integration` etiketli `*_integration_test.go` dosyalarındadır. golangci-lint `build-tags: [integration]` ile onları da denetler.
  - `make test-integration` (`scripts/test-integration.sh`) yalnız bu dosyalardaki testleri koşar, birim testleri ikinci kez koşmaz.
  - Koşmadan önce şunları denetler: her entegrasyon dosyası tam `//go:build integration` ile başlıyor mu, başka bir test dosyasında build kısıtı var mı (yanlış yazılmış etiket), `dbtest` kullanan her test dosyası entegrasyon dosyası mı.
  - Postgres'e bağlanamayan test atlanmaz, kırmızı olur.

### 11. Periyodik işler

River'ın periyodik zamanlayıcısı durumu yalnız bellekte tutar; lider değişince takvim baştan başlar. Bir çalıştırmanın kaçması ya da tekrarlanması sorun olan iş (ör. aylık saklama temizliği) idempotent yazılır. Gerekirse `RunOnStart` ve `UniqueOpts` (`ByPeriod`) birlikte kullanılır.

## Kanıtlar (entegrasyon testleri, CI)

| Kanıt | Test |
|---|---|
| `migrate status → up → up → down ×N → up`; down'lar River'ı iz bırakmadan kaldırıyor | `TestMigrateCommand` |
| `koala_owner` üyesi olmayan giriş migrate edemiyor ve nedeni (`42501`, `set role`) çıktıda | `TestMigrateRefusesNonOwner` |
| `bootstrap` `CREATEROLE` girişiyle idempotent | `TestBootstrapIsIdempotent` |
| `koala_app` temel tabloda, `goose_db_version`'da, geçici tabloda ve `public`'te nesne kurmada `42501` alıyor | `TestAppCannotTouchBaseTable` |
| Yetki kataloğu: DB, şema, tablo, view, kolon, dizi, fonksiyon; `MAINTAIN` dahil; `koala_app`, `koala_maint`, `PUBLIC`; hiçbir yerde `WITH GRANT OPTION` yok; sahiplik | `TestRoleGrants` |
| Sunucu girişi izin listesine göre denetleniyor: fazla üyelik (owner, maint, `pg_read_all_data`, `pg_write_all_data`), öznitelikler, doğrudan tablo yetkisi reddediliyor; `serve` DB'ye ulaşınca açılmıyor | `TestAppLoginMustNotBeMorePrivileged`, `TestServeRefusesPrivilegedLogin` |
| Süper kullanıcı girişi owner/maint havuzlarında ve `migrate`, `maint`, `bootstrap`'ta reddediliyor | `TestOwnerAndMaintPoolsRefuseSuperuser`, `TestOwnerCommandsRefuseSuperuser` |
| `roles.sql` grup rolündeki `LOGIN`'i alıyor | `TestRolesSQLTakesLoginAway` |
| Oturum düzeyinde rol değişikliği yakalanıyor, bağlantı yenileniyor | `TestSessionLevelRoleChangeIsCaught` |
| `InTx`: commit, hata, panik, `SET LOCAL`, iç içe çağrı, hook panikleri, `PgxTx` | `internal/platform/db` |
| Veri ve iş aynı tx'te; `UniqueOpts` çift kuyruklamayı engelliyor | `TestInsertTxCommitsWithTheData`, `TestUniqueOptsSkipsDuplicates` |
| Tek lider; çekilen liderin yerine geçme; çöken liderin TTL sonrası devri | `TestPeriodicJobRunsOnTheLeaderOnly`, `TestLeaderCrashFailsOverAfterTTL` |
| `maint reindex` River indeksinin geçersiz artığını düşürüp indeksleri yeniden kuruyor; geçerli `_ccnew` adlı indekse ve başka addaki geçersiz indekse dokunmuyor | `TestMaintReindex` |
| River SQL'i goose dosyalarıyla birebir | `TestRiverMigrationsMatchRiver` (birim) |

## Sonuçlar

- `koala` alt komutları: `bootstrap`, `migrate up|down|status`, `maint reindex`. Değişkenler: `KOALA_DATABASE_URL`, `KOALA_OWNER_DATABASE_URL`, `KOALA_BOOTSTRAP_DATABASE_URL` (hepsi `config.Secret`).
- Soft-delete örnek tablosu ve `CONCURRENTLY` örneği yalnız test DB'lerinde uygulanır (`internal/platform/dbtest/testdata/migrations/900x_*`). Gerçek `deletions` tablosu, view/GRANT goose yardımcıları, `softdelete.yaml`, sqlc ve `sqlc vet` Faz 1'dedir.
- `serve` henüz River worker'ı başlatmıyor; sıralı kapanıştaki yer (`serve.go`) ilk gerçek işle dolar.
- `scripts/release-db.sh` koala-api'nin yayın hattı kurulduğunda o hatta bağlanır; bugün koala-api'nin imaj yayını yok.
- **Kanıtlanmadı:** büyük `river_job` tablosunda River yükseltmesi ve `CONCURRENTLY` yolu; PgBouncer'lı kurulum; production cluster'ında rol kurulumu.
