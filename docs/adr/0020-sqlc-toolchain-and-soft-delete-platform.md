# ADR-0020: sqlc araç zinciri ve soft-delete platformu

- Durum: Önerildi (PR incelemesinde kabul edilir)
- Tarih: 05.11.2026
- Karar kaydı: K7, K14, K19
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §3.6-3, §6.1 (Faz 1, "Soft-delete korumaları" ve çıkış kriterleri); [ADR-0003](0003-dependency-policy.md) Sonuçlar, [ADR-0004](0004-quality-gate-and-tool-pinning.md) Karar 4 ve Sonuçlar, [ADR-0012](0012-data-access-and-migrations.md), [ADR-0013](0013-soft-delete.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) §4 ve §8; N08 §B; sqlc belgeleri ([vet](https://docs.sqlc.dev/en/latest/howto/vet.html), [goose şeması](https://docs.sqlc.dev/en/latest/howto/ddl.html))
- Kod: `sqlc.yaml`, `db/softdelete.yaml`, `db/queries/`, `db/migrations/0010_deletions.sql`, `internal/platform/softdelete/`, `internal/platform/db/maintdb/`, `internal/tools/softdeletegen/`, `scripts/sqlc-vet-probe.sh`

## Bağlam

ADR-0012 sqlc'yi, ADR-0013 soft-delete'in korumalarını seçti; ikisi de uygulamayı Faz 1'e bıraktı. ADR-0003 ve ADR-0004, sqlc'nin nasıl sabitleneceğine "onu ilk kullanan PR, araç uygulamanın modül grafiğine karışıyor mu ölçütüyle karar verir" dedi. Plan §3.3 tabloda sqlc için `go tool` yazıyor.

Bu PR sqlc'yi ve soft-delete platformunu birlikte kuruyor, çünkü birbirlerine bağlılar: sqlc tipleri `active_<t>` view'larından üretir, `sqlc vet` kuralı katalogdan gelir. Henüz gerçek bir alan tablosu yok; davranış testleri yalnız test DB'lerine uygulanan örnek tablolarla koşar (`internal/platform/dbtest/testdata`).

## Karar

### 1. sqlc'nin sabitlenmesi: `Makefile` + `go install` → `./bin`, `CGO_ENABLED=0`

| Seçenek | Artı | Eksi |
|---|---|---|
| `go.mod` `tool` direktifi (plan §3.3) | Sürüm `go.mod`'da durur. | sqlc v1.31.1'in modül grafiği uygulamanınkine girer: `lib/pq`, `go-sql-driver/mysql`, SQLite (`ncruces/go-sqlite3`), gRPC, TiDB ayrıştırıcısı, CEL, wazero. `lib/pq` depguard'da yasak bir kütüphane. MVS ortak modülleri (`x/sync`, `x/text`, pgx) uygulama için de yükseltebilir; `go.sum` şişer. ADR-0004'ün ölçütü bunu dışlıyor. |
| `go run pkg@sürüm` (oapi-codegen'in yolu) | Modül grafiği ayrı kalır. | sqlc `make generate`, `sqlc vet` ve probta üç kez çağrılıyor; her çağrı derleme önbelleğine bakıp yeniden bağlar. cgo'lu derlemede C araç zinciri gerekir. |
| Yayımlanmış ikiliyi indirmek | En hızlısı. | Platform başına sağlama toplamı sabitlemek gerekir; kaynak sum.golang.org ile doğrulanmaz. |
| **`go install …@sürüm` → `./bin/sqlc-<sürüm>`** | golangci-lint, govulncheck ve vacuum ile aynı yol (ADR-0004 Karar 4). Modül grafiği ayrı; kaynak sum.golang.org ile doğrulanır. | İlk CI koşusunda derleme (~20 sn). |

**Karar:** sqlc `Makefile`'da `SQLC_VERSION := v1.31.1` ile sabittir ve `CGO_ENABLED=0 go install` ile `./bin/sqlc-v1.31.1`'e kurulur. Bu, plan §3.3'teki "`go tool`" notunun yerine geçer; ADR-0003 ve ADR-0004 bu kararı bu PR'a bırakmıştı.

- `CGO_ENABLED=0`: sqlc'nin Postgres ayrıştırıcısı bu durumda cgo'lu `pg_query_go` yerine aynı libpg_query'nin WebAssembly derlemesini (`wasilibs/go-pgquery`, wazero) kullanır. C derleyicisi gerekmez; Mac'te ve runner'da aynı ikili çıkar.
- Sürüm: v1.31.1, 22.04.2026'da yayımlandı (ADR-0017 §2'deki 14 gün kuralı). sqlc'nin kendi `go.mod`'u `go 1.26.0`; Go 1.27 ile derlenir.
- `go.mod`'a hiçbir şey eklenmedi. Üretilen kod yalnız zaten onaylı olan pgx/v5 ve google/uuid'i kullanır.

### 2. Sorgu düzeni ve sqlc yapılandırması

- `db/queries/<alan>/*.sql`: uygulama sorguları, `koala_app` olarak koşar. Alan başına `sqlc.yaml`'da bir `sql` girdisi vardır ve her birinde `no-base-table` kuralı bulunur.
- `db/queries/maint/*.sql`: `koala_maint` sorguları (geri alma, temizlik, anonimleştirme). Hepsi tek bir pakete, `internal/platform/db/maintdb`'ye üretilir. Bu girdide `no-base-table` kuralı yoktur.
- depguard'ın `maint` kuralı `maintdb`'yi yalnız `internal/platform/softdelete`, `internal/privacy` ve `internal/staff`'a açar; River temizlik işleri kendi paketleriyle listeye eklenir. `scripts/lint-probe.sh` kuralın tetiklendiğini kanıtlar.
- `TestSQLCConfig` (`db/queries_test.go`) düzeni korur: her sorgu dizini tam bir girdidedir, `maint` dizini yalnız `maintdb`'ye üretilir, her uygulama girdisinde kural vardır.
- Şema `db/migrations`'tır; sqlc goose dosyalarının Down bölümlerini atlar. Tip eşlemesi: `uuid` → `uuid.UUID` (NULL: `uuid.NullUUID`), `timestamptz` → `time.Time` (NULL: `*time.Time`). `omit_unused_structs` açık: River tabloları modellere girmez.
- `make generate` sırası: `softdeletegen` (Karar 4) → `sqlc generate` (çıktı dizinleri önce boşaltılır) → oapi-codegen. `make gen-check` bu üçünün çıktısını da denetler. `make verify`'a `sqlc-vet` adımı eklendi: `sqlc vet` ve prob.

### 3. `sqlc vet` kuralı: `no-base-table`

- Kural CEL'dir: `query.sql.matches(r"(?i)\b(?:from|join|update|into|using|truncate)\s+(?:only\s+)?(?:table\s+)?(?:public\.)?(?:<tablolar>)\b")`. Tablo listesi `db/softdelete.yaml`'daki soft-delete'li tablolardır ve `make generate` `sqlc.yaml`'daki `rule:` satırını yeniden yazar. Katalogda soft-delete'li tablo yokken kural `false`'tur; ilk tablo kataloğa girdiğinde kendiliğinden dolar.
- CEL'in `matches()`'i RE2'dir, Go'nun `regexp` sözdizimi. Bu yüzden desen Go birim testinde de aynı davranır (`TestNoBaseTablePattern`).
- **Kanıt:** `scripts/sqlc-vet-probe.sh`, kuralı gerçek katalog + örnek tablo kataloğundan aynı üreticiyle kurar ve geçici bir sqlc projesinde `sqlc vet` çalıştırır. Temel tablo adı geçen yedi uygulama sorgusu (SELECT, şemalı ad, JOIN, INSERT, UPDATE, DELETE, büyük harf) tek tek bulgu verir; view üzerinden giden, LEFT JOIN'li ve M1 tablosuna yazan sorgular ile `maint` paketindeki temel tablo sorguları bulgu vermez. Çıktıda başka satır (derleme hatası dahil) olursa prob kırmızıdır; bu, sqlc'nin üretilmiş view'ları gördüğünü de kanıtlar.
- **Sınır:** kural metin okur. Virgüllü join (`FROM active_a, b`) ve tırnaklı ad (`"b"`) kaçar. Asıl koruma DB'dedir: `koala_app`'in temel tabloda yetkisi yok (ADR-0013 Karar 3). Kural nedeni testten önce ve daha anlaşılır söyler.

### 4. View ve GRANT yardımcısı: migration içinde sürümlü direktif

| Seçenek | Artı | Eksi |
|---|---|---|
| DB'de yardımcı fonksiyon (`SELECT koala_soft_delete('t')`; Supabase `audit.enable_tracking`, PostGIS `AddGeometryColumn`, pg_partman `create_parent` kalıbı) | Tek yerde durur, migration kısa kalır. | **sqlc'nin statik kataloğu fonksiyonun içinde kurulan view'ı ve kolonları görmez**; `active_<t>`'ye giden her sorgu derlenmez. DDL incelemede görünmez. |
| Şablonu elle kopyalamak | Araç yok. | Sapma ancak entegrasyon testinde yakalanır. |
| Bir kez üretip bırakmak (Rails generator kalıbı) | Basit. | Üretilen bloğa elle dokunulduğunu `gen-check` göremez. |
| **Migration'da direktif + üretici, sürümlü dondurulmuş şablon** | SQL migration dosyasının içinde durur: sqlc görür, inceleme görür. `gen-check` elle değişikliği yakalar. Şablon sürümlü olduğu için uygulanmış migration'lar şablon değişince değişmez. | Küçük bir üretici (`internal/tools/softdeletegen`) bakımı. |

**Karar:** Son seçenek. Yazar tabloyu kurar ve altına tek satır yazar:

```sql
CREATE TABLE notes (id uuid PRIMARY KEY, body text NOT NULL);
-- +koala soft-delete v1 notes [security_barrier]
```

`make generate` satırın altına `-- +koala begin generated …` / `-- +koala end generated` arasında şunu yazar: `deleted_at`, `deletion_id → deletions(id)`, `<t>_deleted_together` CHECK'i, `deletion_id` kısmi indeksi, `koala_maint`'e temel tabloda DML, `CREATE VIEW active_<t> [WITH (security_barrier)] AS SELECT * FROM <t> WHERE deleted_at IS NULL` (CHECK OPTION yok) ve `koala_app`'e view'da `SELECT, INSERT, UPDATE`.

- Kolon eklemek ya da çıkarmak için yazar önce `DROP VIEW active_<t>;` yazar, tabloyu değiştirir, sonra `-- +koala soft-delete-view v1 <t>` direktifini kullanır; bu yalnız view'ı ve GRANT'ini yazar. Down bölümünde de aynı direktif kullanılır. View `SELECT *`'ı kurulduğu anda genişletir; katalog testi "view kolonları = tablo kolonları" unutulan yeniden kurmayı yakalar.
- `v1` şablonu dondurulmuştur. Şablon değişirse `v2` eklenir; eski migration'lar `v1` olarak kalır.
- Tablo adı en çok 46 bayttır (`<t>_deleted_together` 63 bayta sığsın diye) ve `active_` ile başlayamaz.
- Var olan büyük bir tabloya `CREATE INDEX` transaction içinde kilit alır; o gün `NO TRANSACTION` dosyasında `CONCURRENTLY` yazılır (ADR-0017 §1).

### 5. `deletions` tablosu (`0010_deletions.sql`)

`id`, `root_table`, `root_id`, `actor_kind` (`user`/`staff`/`system`), `actor_id` (sistemde NULL, kişide dolu; CHECK), `reason` (`user_request`/`staff_action`/`retention`/`account_deletion`), `created_at` (uygulama saati), `restored_at`.

- Tablo M1'dir: satırları silinmez. `koala_app`: `SELECT, INSERT`. `koala_maint`: `SELECT, UPDATE` (geri alma `restored_at`'i damgalar). Kimsede `DELETE` yok.
- N08 §B2.2'deki `cascade_root` nedeni alınmadı: kaskadın kökü zaten `root_table`/`root_id`'dir.
- Geri alınan silmenin satırı kalır ve `restored_at` taşır; aynı silme ikinci kez geri alınamaz.

### 6. `softdelete` paketi

- `Delete(ctx, q, DeleteRequest{Table, ID, Actor, Reason, At})`, `koala_app` olarak çağıranın tx'inde koşar: önce `deletions` satırı, sonra kök, sonra kataloğun `cascade` kenarları sırayla (yalnız aktif satırlar güncellendiği için ağaç gibi döngüler kendiliğinden biter). Hepsi aynı `deletion_id`'yi alır. Bir `restrict` kenarında aktif çocuk varsa 409 `SOFT_DELETE_RESTRICTED` döner ve çağıranın `InTx`'i her şeyi geri alır; kontrol kaskadın her seviyesinde yapılır. `detach` kenarına dokunulmaz. Kök yoksa ya da zaten silinmişse `ErrNotActive` döner; çağıran kendi kaynağının 404'üne çevirir.
- `Restore(ctx, q, deletionID, at)`, `koala_maint` olarak (temel tablolarda) koşar: yalnız o `deletion_id`'li satırları açar. Benzersiz anahtar çakışması 409 `RESTORE_CONFLICT`, `cascade` ya da `restrict` kenarındaki ebeveyni başka bir silmeyle hâlâ silinmiş satır 409 `RESTORE_PARENT_DELETED` verir. Bilinmeyen silme `ErrDeletionNotFound`, geri alınmış olan `ErrAlreadyRestored`'dır. `koala_app` ile çağrılırsa `permission denied` alır.
- `Orphans(ctx, q)`: plan §3.6-3'teki değişmez sorgusu. Her `cascade` ve `restrict` kenarı için "silinmiş ebeveynin aktif çocuğu" sayılır. Testlerde koşar; izleme işi ve `koala_softdelete_orphans` metriği metrik PR'ında bağlanır.
- Üç yeni hata kodu `api/components/problem.yaml`'a eklendi; `TestPlatformCodesAreInTheContract` bunları da denetler.
- Kaskad DB trigger'ıyla yapılmaz (plan §3.6-3).

### 7. Katalog: `db/softdelete.yaml`

`tables` her tabloyu `soft_delete` (isteğe bağlı `pii`, `unique_exceptions`) ya da gerekçeli `M1`–`M6` olarak sınıflar. `edges` soft-delete'li tabloya giden her FK'yi `child`, `column`, `parent` ve `policy` ile listeler. Ayrıştırıcı bilinmeyen anahtarı, gerekçesiz muafiyeti, muaf ebeveyne kenarı ve muaf çocuğa `cascade`'i reddeder.

- **YAML ayrıştırma yeni bağımlılık getirmez.** Onaylı listede YAML modülü yok. Katalog, `go.mod`'da zaten doğrudan bağımlılık olan kin-openapi'nin dışa açık `openapi3filter.YamlBodyDecoder`'ı ile okunur ve JSON üzerinden katı biçimde (`DisallowUnknownFields`) yapıya çevrilir. Bu, bir istek gövdesi çözücüsünü yapılandırma dosyası için kullanmak demektir; ayrı bir YAML modülü (ör. `go.yaml.in/yaml/v3`) onaylanırsa yerine konur.
- Testler gerçek kataloğu örnek tabloların kataloğuyla birleştirir (`dbtest.SoftDeleteCatalog`).

### 8. Katalog testleri (`TestCatalogMatchesDatabase`, entegrasyon)

Göç edilmiş şemayı katalogla karşılaştırır:

- (a) her tablo katalogda ve her katalog tablosu DB'de; her view bir soft-delete'li tablonun `active_` view'ı;
- soft-delete'li tabloda `id uuid` birincil anahtar, `deleted_at`, `deletions`'a FK'li `deletion_id` ve CHECK var;
- (b) view kolonları (ad, tip, sıra) tablo kolonlarına eşit; view `FROM <t> WHERE deleted_at IS NULL`, CHECK OPTION yok;
- `security_barrier` ⇔ katalogda `pii: true` (Karar 9);
- (c) `koala_app`'in temel tabloda yetkisi yok, view'da tam `SELECT, INSERT, UPDATE` var; `koala_maint`'in temel tabloda DML'i var;
- (d) her benzersiz indeks `deleted_at IS NULL` ile kısmi ya da gerekçeli istisna;
- (e) soft-delete'li tabloya giden her FK tek kolonlu, `id`'ye gidiyor ve katalogda politikası var; katalogdaki her kenar gerçek bir FK;
- (f) muaf tabloda `deleted_at` yok; M1 tablosunda `koala_app` ve `koala_maint`'in `DELETE`/`TRUNCATE` yetkisi yok.

`TestRoleGrants` (ADR-0017) gerçek `deletions` tablosuna ve üretilmiş GRANT'lere göre güncellendi.

### 9. KVKK: `security_barrier`

ADR-0017 §8'in kuralı katalogla bağlanır: kişisel veri süzen tablo katalogda `pii: true` taşır, direktifi `security_barrier` ile yazılır, katalog testi ikisinin eşit olduğunu denetler. Örnek tablolardan `example_owners` bu yolu kanıtlar.

### 10. JOIN lint'i (`softdelete.CheckJoins`, birim testi)

`detach` kenarının ebeveynine `active_<parent>` ile LEFT ya da FULL olmayan her JOIN (düz, `INNER`, `RIGHT`, `CROSS`, `NATURAL`), sorgu çocuğu da (`<child>` ya da `active_<child>`) adlandırıyorsa bulgudur. Yorumlar ve dize sabitleri atılır, sonra token'lara bakılır.

- `TestQueriesJoinLint` (`db/queries_test.go`) `db/queries/**` altındaki her sqlc sorgusunu gerçek katalogla tarar; `make verify`'daki `go test`'te koşar.
- `TestCheckJoins` lint'in kanıtıdır: yedi kırmızı vaka ve sekiz yeşil vaka (LEFT, LEFT OUTER, FULL, `cascade` kenarı, çocuğun geçmediği sorgu, yorum ve dize içindeki JOIN).
- **Sınır:** virgüllü join ve elle yazılmış pgx sorguları (dinamik admin aramaları) taranmaz. sqlc vet'te değil Go'da: kural katalog kenarlarına bakıyor, CEL'deki RE2'de geriye bakış yok.

### 11. Üretilmiş okuma testi çatısı (`softdeletetest.ReadTest`, OpenAPI `x-resource`)

Her GET operasyonu okuduğu tabloyu `x-resource: <tablo>` ile adlandırır. `ReadTest` bu operasyonları toplar ve her soft-delete'li tablo için tablonun fikstürüyle nesne kurar. Önce her operasyonun nesneyi gösterdiğini doğrular; böylece sonraki kontroller tesadüfen geçemez. Sonra nesneyi `softdelete.Delete` ile siler:

- detay (tek yol parametresi): 404 ve gövdesi hiç var olmamış bir id'ninkiyle bayt bayt aynı (ADR-0014);
- liste (yol parametresi yok): 200 ve id artık yok.

Fikstürü olmayan soft-delete'li tablo, katalogda olmayan `x-resource` ve iki yol parametreli operasyon kırmızıdır. Çatı bu PR'da iki örnek kaynakta (`example_owners`: liste + detay, `example_projects`: detay) yeşildir (`TestReadTestOnExampleResources`). Gerçek spec'te henüz kaynak yok.

## Sonuçlar

- Yeni tablo eklemenin yolu: tabloyu yaz → `-- +koala soft-delete v1 <t>` (ya da gerekçeli muafiyet) → `db/softdelete.yaml`'a sınıf ve FK politikaları → `make generate`. Bir adım unutulursa katalog testi ya da `gen-check` kırmızıdır.
- Bu PR'da kalanlar:
  - `x-resource`'u her GET operasyonunda zorunlu kılan vacuum kuralı (`x-authz` gibi) ilk gerçek kaynakla gelir. O zamana kadar çatı yalnız uzantıyı taşıyan operasyonları görür.
  - Üretilmiş JOIN testi (`detach` kenarında çocuğun uçları çocuğu döndürmeye devam eder): çatı genişletilecek; bu PR'da aynı davranış DB düzeyinde `TestDeleteCascades`'te sınanıyor.
  - Liste sayaçları ve `total`'ın silineni saymaması: sayfalama uçlarıyla.
  - `retention`, temizlik işi, anonimleştirmeden sonra `RESTORE_EXPIRED`: Faz 8.
  - `Orphans`'ın periyodik işe ve metriğe bağlanması: metrik PR'ı.
  - Sunucunun `koala_maint` havuzunu açması ve `softdelete.Deleter`'ın `internal/app`'te kurulması: ilk kullanan alanla.
- **Kanıtlanmadı:** sqlc'nin sorunlu view biçimleri (alt sorgulu view'da nullability #4262, UNION'lı view) kullanılmadı; `active_<t>` view'ları bunlara girmiyor.
