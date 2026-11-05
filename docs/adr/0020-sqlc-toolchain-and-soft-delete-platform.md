# ADR-0020: sqlc araç zinciri ve soft-delete platformu

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K7, K14, K19
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §3.6-3, §6.1 (Faz 1, "Soft-delete korumaları" ve çıkış kriterleri); [ADR-0003](0003-dependency-policy.md) Sonuçlar, [ADR-0004](0004-quality-gate-and-tool-pinning.md) Karar 4 ve Sonuçlar, [ADR-0012](0012-data-access-and-migrations.md), [ADR-0013](0013-soft-delete.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) §4 ve §8; N08 §B; sqlc belgeleri ([vet](https://docs.sqlc.dev/en/latest/howto/vet.html), [goose şeması](https://docs.sqlc.dev/en/latest/howto/ddl.html)); Postgres belgeleri ([satır kilitleri](https://www.postgresql.org/docs/current/explicit-locking.html#LOCKING-ROWS), [READ COMMITTED](https://www.postgresql.org/docs/current/transaction-iso.html#XACT-READ-COMMITTED))
- Kod: `sqlc.yaml`, `db/softdelete.yaml`, `db/queries/`, `db/migrations/0010_deletions.sql`, `internal/platform/softdelete/`, `internal/platform/db/maintdb/`, `internal/tools/softdeletegen/`, `scripts/sqlc-vet-probe.sh`

## Bağlam

ADR-0012 sqlc'yi, ADR-0013 de soft-delete'in korumalarını seçti; ikisi de uygulamayı Faz 1'e bıraktı. ADR-0003 ve ADR-0004, sqlc'nin nasıl sabitleneceğine onu ilk kullanan PR'ın karar vermesini istedi. Ölçüt: araç uygulamanın modül grafiğine karışıyor mu? Plan §3.3'teki tabloda sqlc için `go tool` yazıyor.

Bu PR sqlc'yi ve soft-delete platformunu birlikte kuruyor, çünkü birbirlerine bağlılar: sqlc tipleri `active_<t>` view'larından üretir, `sqlc vet` kuralları katalogdan gelir. Henüz gerçek bir alan tablosu yok. Davranış testleri yalnız test DB'lerine uygulanan örnek tablolarla koşar (`internal/platform/dbtest/testdata`).

İki bağımsız incelemenin (05.11) bulguları ve Yusuf'un kararları bu metne işlendi: YAML modülü, `tool` sınıfı, geri alma aktörü, `deletions` yetkileri, eşzamanlılık ve JOIN lint'inin kapsamı.

## Karar

### 1. sqlc'nin sabitlenmesi: `Makefile` + `go install` → `./bin`, `CGO_ENABLED=0`

| Seçenek | Artı | Eksi |
|---|---|---|
| `go.mod` `tool` direktifi (plan §3.3) | Sürüm `go.mod`'da durur. | sqlc v1.31.1'in modül grafiği uygulamanınkine girer: `lib/pq`, `go-sql-driver/mysql`, SQLite (`ncruces/go-sqlite3`), gRPC, TiDB ayrıştırıcısı, CEL, wazero. `lib/pq` depguard'da yasak bir kütüphane. MVS ortak modülleri (`x/sync`, `x/text`, pgx) uygulama için de yükseltebilir; `go.sum` şişer. ADR-0004'ün ölçütü bunu dışlıyor. |
| `go run pkg@sürüm` (oapi-codegen'in yolu) | Modül grafiği ayrı kalır. | sqlc `make generate`, `sqlc vet` ve probta üç kez çağrılıyor; her çağrıda yeniden bağlanır. cgo'lu derlemede C araç zinciri gerekir. |
| Yayımlanmış ikiliyi indirmek | En hızlısı. | Platform başına sağlama toplamı sabitlemek gerekir; kaynak sum.golang.org ile doğrulanmaz. |
| **`go install …@sürüm` → `./bin/sqlc-<sürüm>`** | golangci-lint, govulncheck ve vacuum ile aynı yol (ADR-0004 Karar 4). Modül grafiği ayrı kalır; kaynak sum.golang.org ile doğrulanır. | İlk CI koşusunda derleme (~20 sn). |

**Karar:** sqlc, `Makefile`'da `SQLC_VERSION := v1.31.1` ile sabitlenir ve `CGO_ENABLED=0 go install` ile `./bin/sqlc-v1.31.1`'e kurulur. Bu karar plan §3.3'teki "`go tool`" notunun yerine geçer; ADR-0003 ve ADR-0004 kararı bu PR'a bırakmıştı.

- `CGO_ENABLED=0`: sqlc'nin Postgres ayrıştırıcısı bu durumda cgo'lu `pg_query_go` yerine aynı libpg_query'nin WebAssembly derlemesini (`wasilibs/go-pgquery`, wazero) kullanır. C derleyicisi gerekmez; Mac'te ve runner'da aynı ikili çıkar.
- Sürüm: v1.31.1, 22.04.2026'da yayımlandı (ADR-0017 §2'deki 14 gün kuralı). sqlc'nin `go.mod`'u `go 1.26.0`; Go 1.27 ile derlenir.
- sqlc `go.mod`'a girmez. Üretilen kod yalnız zaten onaylı olan pgx/v5 ve google/uuid'i kullanır.

### 2. Sorgu düzeni ve sqlc yapılandırması

- `db/queries/<alan>/*.sql`: uygulama sorguları, `koala_app` olarak koşar. `sqlc.yaml`'da alan başına bir `sql` girdisi vardır ve her birinde iki kural bulunur: `no-base-table` ve `no-soft-delete-columns`.
- `db/queries/maint/*.sql`: `koala_maint` sorguları (geri alma, temizlik, anonimleştirme). Hepsi tek bir pakete, `internal/platform/db/maintdb`'ye üretilir; bu girdide kural yoktur.
- depguard'ın `maint` kuralı `maintdb`'yi yalnız `internal/platform/softdelete`, `internal/privacy` ve `internal/staff`'a açar. River temizlik işleri kendi paketleriyle listeye eklenir. `scripts/lint-probe.sh` kuralın tetiklendiğini kanıtlar.
- `TestSQLCConfig` (`db/queries_test.go`) düzeni korur:
  - her sorgu dizini tam bir girdidedir;
  - `maint` dizini yalnız `maintdb`'ye üretilir ve kural taşımaz;
  - her uygulama girdisinde iki kural da vardır;
  - kural metinleri üreticinin yazdığıyla aynıdır.
- Şema `db/migrations`'tır; sqlc goose dosyalarının Down bölümlerini atlar.
- Tip eşlemesi: `uuid` → `uuid.UUID` (NULL: `uuid.NullUUID`), `timestamptz` → `time.Time` (NULL: `*time.Time`). `omit_unused_structs` açıktır, River tabloları modellere girmez.
- `make generate` sırası: `softdeletegen` (Karar 4) → `sqlc generate` (çıktı dizinleri önce boşaltılır) → oapi-codegen. `make gen-check` üçünün çıktısını da denetler. `make verify`'a `sqlc-vet` adımı eklendi: önce `sqlc vet`, sonra prob.

### 3. `sqlc vet` kuralları

**`no-base-table`:** uygulama sorgusu soft-delete'li bir temel tablonun adını `FROM`, `JOIN`, `UPDATE`, `INTO`, `USING` ya da `TRUNCATE`'ten sonra geçiremez.

- CEL ifadesi: `query.sql.matches(r"(?i)\b(?:from|join|update|into|using|truncate)\s+(?:only\s+)?(?:table\s+)?(?:public\.)?(?:<tablolar>)\b")`.
- Tablo listesi `db/softdelete.yaml`'dan gelir; `make generate` `sqlc.yaml`'daki `rule:` satırını yeniden yazar. Katalogda soft-delete'li tablo yokken kural `false`'tur ve ilk tablo kataloğa girince kendiliğinden dolar.

**`no-soft-delete-columns`:** `deleted_at` ve `deletion_id`'ye yalnız `softdelete.Delete` ve `Restore` yazar. Şu biçimler kırmızıdır:

- `UPDATE … SET` içinde atama (bu, `ON CONFLICT DO UPDATE SET`'i de kapsar);
- `INSERT` kolon listesinde bu kolonların geçmesi;
- tırnaklı adlar.

`SET`'ten sonra gelen `deletion_id = $1` karşılaştırması da bulgu sayılır. Uygulama sorgusunun bu kolonla süzmesi için sebep yok, çünkü view zaten süzüyor. Okumak serbesttir. Desenler sabittir ve kural satırını da üretici yazar.

- CEL'in `matches()`'i RE2'dir, Go'nun `regexp` sözdizimi. Bu yüzden desenler Go birim testinde de aynı davranır (`TestNoBaseTablePattern`, `TestNoSoftDeleteColumnsPatterns`). Raw CEL dizesi `"` içeremediği için tırnak `\x22` diye yazılır.
- **Kanıt:** `scripts/sqlc-vet-probe.sh` iki kuralı gerçek ve örnek kataloglardan aynı üreticiyle kurar ve geçici bir sqlc projesinde `sqlc vet` çalıştırır. Çıktı tam olarak beklenen 11 satır olmalıdır:
  - temel tablo adıyla 8 sorgu (`USING` dahil) `no-base-table` verir;
  - kolonlara yazan 3 sorgu `no-soft-delete-columns` verir;
  - view, LEFT JOIN ve M1 sorguları ile `maint` paketindeki aynı sorgular bulgu vermez;
  - derleme hatası çıkmaz, yani sqlc üretilmiş view'ları görür.
- **Sınır:** kurallar metin okur. Virgüllü join (`FROM active_a, b`) ve tırnaklı tablo adı `no-base-table`'dan kaçar. Asıl koruma DB'dedir: `koala_app`'in temel tabloda yetkisi yoktur (ADR-0013 Karar 3). Kural nedeni testten önce ve daha anlaşılır söyler.

### 4. View ve GRANT yardımcısı: migration içinde sürümlü direktif

| Seçenek | Artı | Eksi |
|---|---|---|
| DB'de yardımcı fonksiyon (`SELECT koala_soft_delete('t')`; Supabase `audit.enable_tracking`, PostGIS `AddGeometryColumn`, pg_partman `create_parent` kalıbı) | Tek yerde durur, migration kısa kalır. | **sqlc'nin statik kataloğu fonksiyonun içinde kurulan view'ı ve kolonları görmez**; `active_<t>`'ye giden her sorgu derlenmez. DDL incelemede görünmez. |
| Şablonu elle kopyalamak | Araç yok. | Sapma ancak entegrasyon testinde yakalanır. |
| Bir kez üretip bırakmak (Rails generator kalıbı) | Basit. | Üretilen bloğa elle dokunulduğunu `gen-check` göremez. |
| **Migration'da direktif + üretici, sürümlü dondurulmuş şablon** | SQL migration dosyasının içinde durur; sqlc de inceleme de görür. `gen-check` elle değişikliği yakalar. Şablon sürümlü olduğu için uygulanmış migration'lar şablon değişince değişmez. | Küçük bir üretici (`internal/tools/softdeletegen`) bakımı. |

**Karar:** son seçenek. Yazar tabloyu kurar ve altına tek satır yazar:

```sql
CREATE TABLE notes (id uuid PRIMARY KEY, body text NOT NULL);
-- +koala soft-delete v1 notes [security_barrier]
```

`make generate` bu satırın altına, `-- +koala begin generated …` ve `-- +koala end generated` işaretleri arasına şunları yazar:

- `deleted_at` kolonu;
- `deletions(id)`'ye giden `deletion_id` kolonu;
- `<t>_deleted_together` CHECK'i;
- `deletion_id` kısmi indeksi;
- `koala_maint`'e temel tabloda DML;
- `CREATE VIEW active_<t> [WITH (security_barrier)] AS SELECT * FROM <t> WHERE deleted_at IS NULL` (CHECK OPTION yok);
- `koala_app`'e view'da `SELECT, INSERT, UPDATE`.

Kurallar:

- Kolon eklemek ya da çıkarmak için yazar şu sırayı izler: `DROP VIEW active_<t>;` → tablo değişikliği → `-- +koala soft-delete-view v1 <t>`. Bu direktif yalnız view'ı ve GRANT'ini yazar; Down bölümünde de kullanılır. View `SELECT *`'ı kurulduğu anda genişletir. Yazar view'ı yeniden kurmayı unutursa katalog testindeki "view kolonları = tablo kolonları" kontrolü yakalar.
- `v1` şablonu dondurulmuştur. `TestV1TemplatesAreFrozen`, bariyerli ve bariyersiz iki direktifin çıktısını bayt bayt sabitler. Şablon değişirse `v2` eklenir; eski migration'lar `v1` olarak kalır.
- Tablo adı en çok `softdelete.MaxTableName` = 46 bayttır ve `active_` ile başlayamaz. Sınır, `<t>_deleted_together`'ın 63 bayta sığması içindir; katalog ve üretici aynı sabiti kullanır.
- Var olan büyük bir tabloya `CREATE INDEX` transaction içinde kilit alır. Böyle bir durumda indeks `NO TRANSACTION` dosyasında `CONCURRENTLY` ile kurulur (ADR-0017 §1).

### 5. `deletions` tablosu (`0010_deletions.sql`)

| Kolon | Açıklama |
|---|---|
| `id` | Silme kimliği; uygulama üretir (UUIDv7). |
| `root_table`, `root_id` | Silmenin kök kaydı. |
| `actor_kind`, `actor_id` | `actor_kind` `user`/`staff`/`system`. `actor_id` kişide dolu, sistemde NULL (CHECK). |
| `reason` | `user_request`/`staff_action`/`retention`/`account_deletion`. |
| `created_at` | Uygulama saati. |
| `restored_at`, `restored_by_kind`, `restored_by` | Geri alma damgası. |

- Geri alma damgası bütün olarak yazılır. `restored_at` ile `restored_by_kind` birlikte NULL ya da birlikte doludur; `restored_by` yalnız kişi geri aldığında doludur. İkisi de CHECK ile zorlanır (tasarım notu §B2.7).
- Personel geri almasının denetim kaydı Faz 8'de denetim çekirdeğiyle gelir. Bugün aktör yalnız bu satırda durur.
- **Yetkiler:**
  - `koala_app` yalnız `INSERT` alır. Kimliği uygulama ürettiği ve `RETURNING` kullanılmadığı için okumaya ihtiyacı yok. Kayıttaki FK kontrolleri tablo sahibinin yetkisiyle koşar.
  - `koala_maint` `SELECT` ve yalnız `UPDATE (restored_at, restored_by_kind, restored_by)` alır.
  - Kimsede `DELETE` yok. Tablo M1'dir, satırları silinmez.
  - `TestRoleGrants` kolon düzeyindeki yetkileri tek tek sayar.
- `MarkDeletionRestored` yalnız `restored_at IS NULL` olan satırı günceller ve etkilenen satır sayısını döner. Parametreleri cast'lerle NULL olamaz.
- N08 §B2.2'deki `cascade_root` nedeni alınmadı: kaskadın kökü zaten `root_table`/`root_id`'dir.

### 6. `softdelete` paketi ve eşzamanlılık

**`Delete(ctx, q, DeleteRequest{Table, ID, Actor, Reason, At})`** `koala_app` olarak çağıranın tx'inde koşar. Adımlar:

1. `deletions` satırını yazar.
2. Kökü işaretler.
3. `cascade` kenarlarını sırayla izler. Yalnız aktif satırlar güncellendiği için ağaç ve döngü kendiliğinden biter.
4. Kaskad bittikten sonra her `restrict` kenarını bu silmenin işaretlediği **bütün** ebeveynler üzerinde toplu denetler.

Bütün satırlar aynı `deletion_id`'yi alır. Aynı silmenin götürdüğü çocuk `restrict` kenarını tıkamaz, sonuç kenarların sırasına bağlı değildir. Başka bir aktif çocuk varsa 409 `SOFT_DELETE_RESTRICTED` döner. Muaf (M1) çocuğa giden `restrict` kenarı temel tabloda denetlenir; o çocuk hiç silinmediği için ebeveyni sonsuza dek tutar. `detach` kenarına dokunulmaz.

Kök yoksa ya da zaten silinmişse `ErrNotActive` döner. Bu durumda `deletions` satırı yazılmış olur; çağıran hatayı `InTx`'ten döndürmek, yani tx'i geri almak zorundadır.

**`Restore(ctx, q, RestoreRequest{DeletionID, Actor, At})`** `koala_maint` olarak temel tablolarda koşar ve yalnız o `deletion_id`'li satırları açar. Hatalar:

| Durum | Hata |
|---|---|
| Bilinmeyen silme | 404 `DELETION_NOT_FOUND` |
| Daha önce geri alınmış silme | 409 `DELETION_ALREADY_RESTORED` |
| Benzersiz anahtar ya da exclusion çakışması (`23505`, `23P01`) | 409 `RESTORE_CONFLICT` |
| Açılan satırın `cascade` ya da `restrict` kenarındaki ebeveyni silinmiş | 409 `RESTORE_PARENT_DELETED` |
| Kök kayıt yok (temizlenmiş) | 409 `RESTORE_EXPIRED` |

`koala_app` ile çağrılırsa `permission denied` alır.

**`LockActiveParent(ctx, q, table, id)`**, ebeveynin aktif satırını view üzerinden `FOR SHARE` ile kilitler; satır yoksa `ErrNotActive` döner. Çocuk ekleyen ya da çocuğun FK'sini başka ebeveyne çeviren kod bunu aynı tx'te önce çağırır (CLAUDE.md).

**`Orphans(ctx, q)`** plan §3.6-3'teki değişmez sorgusudur: her `cascade` ve `restrict` kenarı için "silinmiş ebeveynin aktif çocuğu" sayısını verir.

Diğer kurallar:

- Hata kodlarının altısı da `api/components/problem.yaml`'dadır; `TestPlatformCodesAreInTheContract` bunu denetler.
- Kaskad DB trigger'ıyla yapılmaz (plan §3.6-3).
- Katalog testi `cascade` ve `restrict` kenarında çocuk kolonuyla başlayan bir indeks ister. Delete, Restore ve Orphans çocukları bu kolonla arar.

**Eşzamanlılık.** Korunan değişmez şu: `cascade` ve `restrict` kenarlarında silinmiş ebeveynin aktif çocuğu olmaz. READ COMMITTED altında bunu, çocuğu aktif yapan her yolun ebeveyn satırında, silmenin satır kilidiyle çakışan bir kilit alması sağlar:

- `Delete` önce ebeveyni işaretler (UPDATE, satır kilidi). Kaskadı ve `restrict` denetimini sonra, her biri o ana kadar commit edilmiş olanı gören yeni bir ifadede yapar.
- `Restore` açtığı her satırın ebeveynini `SELECT … FOR SHARE OF p` ile **süzgeçsiz** kilitler ve `deleted_at`'i Go'da denetler.
  - Kilitte `deleted_at IS NOT NULL` gibi bir süzgeç olsaydı aktif ebeveyn kilitlenmezdi. Eşzamanlı bir Delete onu işaretleyip çocuğu görmeden geçebilirdi.
  - Süzgeçsiz kilitte iki durum var:
    - Delete önce davrandıysa Restore onu bekler ve kilit döndüğünde ebeveynin yeni sürümünü (silinmiş) görür.
    - Restore önce davrandıysa Delete'in UPDATE'i bekler. Delete'in sonraki ifadesi açılmış çocuğu görür: kaskad onu da işaretler ya da `restrict` 409 verir.
- Çocuk ekleyen kod `LockActiveParent` ile aynı korumayı alır.
- Dört entegrasyon testi bunu kanıtlar:
  - `TestRestoreWaitsForConcurrentDelete`;
  - `TestDeleteWaitsForConcurrentRestore`;
  - `TestLockActiveParentWaitsForConcurrentDelete`;
  - `TestDeleteCascadesToAChildInsertedConcurrently`.

  Her test bir tx'i bilinen noktada açık tutar, ikinciyi başlatır ve Postgres'in `pg_locks`'ta onu "kilit bekliyor" gösterdiğini görmeden ilerlemez. Kilitler olmasaydı ikinci tx beklemezdi, test kırmızı olurdu.
- **Kilitlenme (`40P01`):** örtüşen alt ağaçları silen iki işlem kilitlenebilir; Postgres birini düşürür.
  - `InTx` yeniden denemez, çünkü geri çağrısının iki kez koşmasının güvenli olup olmadığını bilemez.
  - Silme sıcak yol değildir. Çağıran hatayı bildirir, kullanıcı ya da iş yeniden dener. Bu yazılı karardır.
- **Büyük alt ağaç:** Delete tek tx'te ve `koala_app`'in 20 sn'lik ifade süre sınırı altında koşar. Binlerce satırlık bir alt ağaç (bütün geçmişiyle bir şirket) istek içinde değil, sistem aktörüyle bir River işinde silinir.

### 7. Katalog: `db/softdelete.yaml`

`tables` her tabloyu sınıflar:

- `soft_delete` (isteğe bağlı `pii` ve `unique_exceptions`);
- gerekçeli `M1`–`M6`;
- gerekçeli `tool`.

`edges` soft-delete'li tabloya giden her FK'yi `child`, `column`, `parent` ve `policy` ile listeler. Ayrıştırıcı şunları reddeder:

- bilinmeyen anahtar;
- gerekçesiz muafiyet;
- muaf ebeveyne giden kenar;
- muaf çocuğa `cascade`;
- sınırı aşan ad.

- **`tool` sınıfı (yeni):** kendini kendi aracı yöneten tablolar içindir. İlk örnek goose'un `goose_db_version`'ıdır. `koala_app` ve `koala_maint` bu tablolarda tablo ya da kolon düzeyinde hiçbir yetki almaz; katalog testi bunu denetler. M3 kuyruk tablolarına ayrıldı (River), çünkü River tablolarında `koala_app` DML taşır.
- **YAML:** katalog `go.yaml.in/yaml/v3` (v3.0.6, 13.10.2026, MIT ve Apache-2.0; Yusuf 05.11'de onayladı) ile katı okunur:
  - `KnownFields(true)`: bilinmeyen alan hatadır.
  - Bir eşlemede aynı anahtar iki kez geçerse hatadır.
  - Boş girdi ve ikinci belge hatadır.

  `TestCatalogYAMLIsStrict` bu davranışları sabitler. Diğer YAML okumaları (authz matrisi vb.) ayrı PR'larda bu modüle geçer.
- Testler gerçek kataloğu örnek tabloların kataloğuyla birleştirir (`dbtest.SoftDeleteCatalog`).

### 8. Katalog testleri (`TestCatalogMatchesDatabase`, entegrasyon)

Göç edilmiş şemayı katalogla karşılaştırır:

- (a) Her tablo katalogda, her katalog tablosu DB'de. Her view, bir soft-delete'li tablonun `active_` view'ı.
- Soft-delete'li tabloda `id uuid` birincil anahtar, `deleted_at`, `deletions`'a FK'li `deletion_id` ve CHECK var.
- (b) View kolonları (ad, tip, sıra) tablo kolonlarına eşit. View `FROM <t> WHERE deleted_at IS NULL` biçiminde, CHECK OPTION yok.
- `security_barrier` ⇔ katalogda `pii: true` (Karar 9).
- (c) `koala_app`'in temel tabloda yetkisi yok; view'da tam `SELECT, INSERT, UPDATE` var. `koala_maint`'in temel tabloda DML'i var.
- (d) Her benzersiz indeks `deleted_at IS NULL` ile kısmi ya da gerekçeli istisna.
- (e) Soft-delete'li tabloya giden her FK tek kolonlu, `id`'ye gidiyor ve katalogda politikası var. Katalogdaki her kenar gerçek bir FK. `cascade` ve `restrict` kenarının çocuk kolonunda indeks var.
- (f) Muaf tabloda `deleted_at` yok. M1 tablosunda `koala_app` ve `koala_maint`'in `DELETE`/`TRUNCATE` yetkisi yok. `tool` tablosunda iki rolün de hiçbir yetkisi yok.

`TestRoleGrants` (ADR-0017) gerçek `deletions` tablosuna, kolon düzeyindeki yetkilere ve üretilmiş GRANT'lere göre güncellendi.

### 9. KVKK: `security_barrier`

ADR-0017 §8'in kuralı katalogla bağlanır. Kişisel veri süzen tablo katalogda `pii: true` taşır ve direktifi `security_barrier` ile yazılır. Katalog testi ikisinin eşit olduğunu denetler. Örnek tablolardan `example_owners` bu yolu kanıtlar.

### 10. JOIN lint'i (`softdelete.CheckJoins`, birim testi)

Kural: çocuğun satırlarını üreten bir sorguda `detach` kenarının ebeveynine yalnız şu yollarla ulaşılır:

- çocuktan `LEFT` (ya da `FULL`) join'le, yani nullable tarafta;
- kendisi LEFT-join'lenmiş bir derived tablo ya da LATERAL içinden;
- skaler bir alt sorgudan (select listesinde arama, `NOT EXISTS`).

Ebeveyne başka her konumda ulaşmak, ebeveyni silinmiş çocukları düşürür ve bulgudur:

- `FROM`'da ebeveyn (ebeveyn FROM'da durup çocuğa INNER JOIN ile bağlanan biçim dahil);
- `INNER`, `RIGHT`, `CROSS` ya da `NATURAL` join;
- virgüllü join;
- iç join'lenmiş derived tablo ya da LATERAL;
- ebeveyni okuyan ve iç join'le kullanılan CTE (adı ebeveynin yerine geçer);
- `IN`/`EXISTS`/`ANY` yarı-join'i.

Tırnaklı ve şemalı adlar düz ad sayılır. Çocuk yalnız yazılıyorsa (`INSERT INTO`), yalnız ebeveyne LEFT-join'leniyorsa (ebeveyn merkezli sorgu) ya da yalnız bir alt sorguda sayılıyorsa bulgu yoktur.

- Lint yorumları ve dize sabitlerini atar, sonra token'ları parantez düzeyleriyle yürür. Her düzeyin türünü (derived, yarı-join, CTE, skaler) önündeki token belirler.
- `TestQueriesJoinLint` (`db/queries_test.go`), `db/queries/**` altındaki her sqlc sorgusunu gerçek katalogla tarar; `make verify`'daki `go test`'te koşar.
- `TestCheckJoins` lint'in kanıtıdır: 20 kırmızı ve 17 yeşil vaka, yukarıdaki her biçim dahil.
- **Sınır.** Lint sözdizimi ağacı değil token okur. Şunları yakalamaz:
  - Elle yazılmış pgx sorguları (dinamik admin aramaları): sqlc dosyası olmadıkları için taranmazlar.
  - Bir view'ın kendi içindeki join: ebeveyne iç join yapan bir view üzerinden okuma görünmez.
  - Çocuğa `RIGHT JOIN`: ebeveyn FROM'da, çocuk RIGHT JOIN'le bağlandığında ebeveyn aslında nullable taraftadır, ama lint yine bulgu verir (yanlış pozitif).
  - CTE ya da derived tablo üzerinden çocuk takma adı: çocuk takma adla okunduğunda doğru iz sürülmez; çocuk aynı sorguda bir yerde okunduğu için kenar denetlenir.
  - Fonksiyon gövdesi, `UNION` kollarının ayrı ayrı anlamı, `USING`/`ON` koşulunun içeriği. Lint join koşulunun doğru kolonu kullandığını denetlemez.

  Bu durumlar için davranış koruması üretilmiş okuma testi (Karar 11) ve `Orphans`'tır.
- sqlc vet'te değil Go'da çalışır, çünkü kural katalog kenarlarına bakıyor ve CEL'deki RE2'de geriye bakış yok.

### 11. Üretilmiş okuma testi çatısı (`softdeletetest.ReadTest`, OpenAPI `x-resource`)

Her GET operasyonu okuduğu tabloyu `x-resource: <tablo>` ile adlandırır (stil rehberi §11). `ReadTest` önce DB'siz bir plan çıkarır. Şunlar kırmızıdır:

- uzantısı tablo adı olmayan operasyon;
- katalogda olmayan tablo;
- fikstürü olmayan soft-delete'li tablo;
- iki yol parametreli operasyon;
- hiç `x-resource` olmaması.

Sonra her soft-delete'li tablo için fikstürle nesne kurar ve her operasyonun nesneyi gösterdiğini doğrular; böylece sonraki kontroller tesadüfen geçemez. Ardından nesneyi `softdelete.Delete` ile siler ve şunları bekler:

- detay (tek yol parametresi): 404 ve gövde, hiç var olmamış bir id'ninkiyle bayt bayt aynı (ADR-0014);
- liste (yol parametresi yok): 200 ve id artık yok.

Çatının yanlışı yakaladığı DB'siz negatif testlerle kanıtlanır (`TestPlanReportsContractProblems`, `TestVisibilityCatchesLeaks`): silineni hâlâ veren, 404 gövdesinde id'yi yazan, silinmişe başka durum dönen ya da listede silineni gösteren handler kırmızıdır. Çatı iki örnek kaynakta yeşildir (`TestReadTestOnExampleResources`). Gerçek spec'te henüz kaynak yok.

## Sonuçlar

- Yeni tablo eklemenin yolu:
  1. tabloyu yaz;
  2. `-- +koala soft-delete v1 <t>` ekle (ya da gerekçeli muafiyet);
  3. `db/softdelete.yaml`'a sınıfı ve FK politikalarını yaz; `cascade`/`restrict` çocuk kolonuna indeks kur;
  4. `make generate` çalıştır.

  Bir adım unutulursa katalog testi ya da `gen-check` kırmızı olur.
- Çocuk ekleyen kod `LockActiveParent`'ı çağırır. Bu kural makine tarafından zorlanmaz; incelemede bakılır (CLAUDE.md).
- Bu PR'da kalanlar:
  - `x-resource`'u her GET operasyonunda zorunlu kılan vacuum kuralı (`x-authz` gibi) ilk gerçek kaynakla gelir.
  - Üretilmiş JOIN testi (`detach` kenarında çocuğun uçları çocuğu döndürmeye devam eder) çatıya eklenecek. Bu PR'da aynı davranış DB düzeyinde `TestDeleteCascades`'te sınanıyor.
  - Liste sayaçlarının ve `total`'ın silineni saymaması sayfalama uçlarıyla gelir.
  - `retention`, temizlik işi ve anonimleştirme Faz 8'de gelir; `RESTORE_EXPIRED` bugün yalnız kök satırın yokluğuna bakıyor.
  - Personel geri almasının denetim kaydı Faz 8'de gelir.
  - `Orphans`'ın periyodik işe ve `koala_softdelete_orphans` metriğine bağlanması metrik PR'ında yapılır.
  - Sunucunun `koala_maint` havuzunu açması ve `softdelete.Deleter`'ın `internal/app`'te kurulması, ilk kullanan alanla gelir.
- **Kanıtlanmadı:** sqlc'nin sorunlu view biçimleri (alt sorgulu view'da nullability #4262, UNION'lı view) kullanılmadı; `active_<t>` view'ları bunlara girmiyor.
