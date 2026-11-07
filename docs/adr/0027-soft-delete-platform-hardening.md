# ADR-0027: Soft-delete platformunun sertleştirilmesi

- Durum: Kabul edildi
- Tarih: 07.11.2026
- Karar kaydı: K14, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.6-3 (K14), §6.1 Faz 2; [ADR-0013](0013-soft-delete.md) Karar 3 ve 4, [ADR-0017](0017-river-goose-db-roles-and-test-db.md) §8, [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md) Karar 3, 6, 10 ve 11, [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §2 ve §6, [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md); [issue #14](https://github.com/is-koala/koala-api/issues/14) "Faz 2 ön koşulları"; Postgres belgeleri ([satır kilitleri](https://www.postgresql.org/docs/current/explicit-locking.html#LOCKING-ROWS), [`security_barrier`](https://www.postgresql.org/docs/current/rules-privileges.html))
- Kod: `internal/platform/softdelete/lint.go`, `internal/platform/softdelete/softdelete.go`, `internal/platform/softdelete/softdeletetest/jointest.go`, `db/queries/authn/sessions.sql`, `db/queries/authz/staff.sql`, `scripts/sqlc-vet-probe.sh`

## Bağlam

ADR-0020 soft-delete platformunu kurdu ve iki korumasını, kendi "Bu PR'da kalanlar" listesinde, ilk gerçek kaynağa bıraktı: üretilmiş JOIN testi ve `x-resource`'un zorunlu kılınması. Faz 0–1 incelemelerinin platform bulguları da aynı gerekçeyle ertelendi; o bulgular lint'in kaçırdığı yazım biçimleri, JOIN lint'inin yanlış negatifleri, `Restore`'un kilit sırası ve iki eksik eşzamanlılık testiydi.

Gerekli kaynaklar Faz 1'le geldi. Katalogda artık gerçek `detach` kenarları var (`user_sessions` → `users` ve `devices`; `staff_sessions`, `staff_login_sources`, `staff_role_assignments` → `staff_members`), sözleşmede `x-resource` taşıyan GET'ler var, ve bu kenarların gerçek sorguları ADR-0020'nin "ebeveyne yalnız `LEFT JOIN`" kuralını iki ayrı biçimde kullanıyor: biri NULL'ı koruyup Go'da karar veriyor, öteki NULL'ı bilinçli olarak `WHERE` ile yok ediyor. İkisini lint ayırt edemiyordu.

Bu ADR o maddeleri kapatır. ADR-0020 kabul edilmiş bir karardır ve düzenlenmez (depo kuralı); ilgili maddelerinin yerine aşağıdaki kararlar geçer.

## Karar

### 1. `no-soft-delete-columns` dört biçim daha yakalar

ADR-0020 Karar 3 iki desen yazmıştı: `UPDATE … SET <kolon> =` ve `INSERT INTO … (… <kolon> …)`. Kolonlara yazan şu biçimler ikisinden de kaçıyordu:

| Biçim | Durum |
|---|---|
| Satır kurucusu: `SET (deleted_at, deletion_id) = (…)` | Yeni desen: `SET … ( … <kolon> … ) =` |
| Kolon listesiz `INSERT`: `INSERT INTO t VALUES …`, `… SELECT …`, `… DEFAULT VALUES` | Yeni desen: ilişkiden sonra kolon listesi yerine anahtar kelime gelen `INSERT` |
| `MERGE … THEN INSERT (… <kolon> …)` (`INTO`'suz `INSERT`) | Yeni desen: `INSERT` + `(` + kolon |
| `MERGE … THEN INSERT VALUES …` (kolon listesiz) | Yeni desen: `THEN INSERT` + anahtar kelime |
| Deseni yorumla bölmek: `SET deleted_at /* x */ = now()`, `INSERT /* x */ INTO …`, `INSERT INTO t /* x */ VALUES …`, `… AS /* x */ t …`, `THEN INSERT /* x */ VALUES …` | Desenlerin **her** birleşim yerinde boşluk ve yorum eşleyen `gap`/`gap1`; `\s+` kalan tek yer yok |

Kolon listesiz `INSERT` desenleri bir kolon adı değil bir **ifade biçimi** reddeder: kolonlarını saymayan bir `INSERT` ilişkinin bütün kolonlarını sırayla yazar, `deleted_at` ve `deletion_id` de içlerinde; adı yazılmayan kolonu hiçbir metin deseni göremez. Uygulama sorgusu yazdığı kolonları sayar, bu yüzden kural uygulamaya bir yük bindirmez. `MERGE`'ün `UPDATE SET` kolu zaten ilk desene düşüyordu.

- Desenler RE2'dir ve `TestNoSoftDeleteColumnsPatterns` her biçim için bir kırmızı, her doğru yazım için bir yeşil vaka taşır.
- **Yalnız RE2'de güvenli.** `gap`/`gap1`'in iç içe alternasyonu geri izlemeli bir motorda üstel: `INSERT INTO t` ile kolon listesi arasına 20 blok yorum koymak 472 ms, 25 koymak 4 saniyeden fazla sürüyor. Go'nun `regexp`'i ve CEL'in `matches()`'i RE2 olduğu için kapı etkilenmez; `sqlc.yaml`'daki 1118 karakterlik `rule:` metni `grep -P`, JS ya da Python kullanan bir ön-commit kancasına kopyalanırsa ReDoS olur.
- `scripts/sqlc-vet-probe.sh` yorumla bölünmüş `SET`'i gerçek `sqlc vet` içinde kanıtlar. Üç biçim proba girmez, çünkü prob derleme hatası kabul etmiyor: `MERGE` ve satır kurucusu `SET` sqlc'de sınanmış yollar değil, **kolon listesiz `INSERT` ise bir view'a hiç derlenmiyor** — sqlc o biçim için view'da hedef kolon saymıyor ve tek ifadeyle bile `INSERT has more expressions than target columns` diyor (CI'da iki turda ölçüldü). Üçünün koruması bugün yalnız `TestNoSoftDeleteColumnsPatterns`'tadır.
- Bunun bir yan sonucu var: kolon listesiz `INSERT` deseni uygulama sorgularında pratikte ulaşılamazdır. `active_<t>` view'ına yazılan biçim sqlc'de derlenmiyor, temel tabloya yazılan biçimi ise `no-base-table` yakalıyor. Desen gene duruyor: kuralı yazıya geçiriyor ve sqlc'nin davranışı değişirse kapı hazır.
- **Sınır (DB'de yedeği yok).** ADR-0013 Karar 3'ün "asıl koruma DB'dedir" cümlesi `no-base-table` için geçerlidir, bu kural için değil: `koala_app` `active_<t>` view'ında `UPDATE` yetkisi taşır ve `softdelete.Delete`'in kendisi tam o yetkiyle `deleted_at`'i yazar. Kolon düzeyinde `UPDATE (deleted_at, deletion_id)` iptali `Delete`'i de kırardı. Gerçek yedek işaretlemeyi `koala_maint`'in sahip olduğu bir `SECURITY DEFINER` fonksiyonuna taşımaktır; bu Faz 2'nin kapsamında değil, Sonuçlar'da açık iş olarak duruyor.
- **Sınır (metin kuralı).** Kalan kaçışlar: dize sabiti ya da yorum içindeki `deleted_at` yanlış pozitif verir; **parantez içeren** bir yorum kolon listesi desenini (`[^(]*`) böler; `COPY` ve DB tarafında view üzerine konan bir `INSTEAD OF` trigger'ı ya da rule kolonu adlandırmadan yazar ve sorgu metninde hiç görünmez. Yorumun kendisi artık bir kaçış değil: `INSERT`, `INTO`, ilişki adı, `AS`, takma ad ve `VALUES`/`SELECT` arasındaki her boşluk yorum da kabul ediyor (`TestNoSoftDeleteColumnsPatterns`'ta yedi kırmızı vaka).

### 2. Üretilmiş JOIN testi: `softdeletetest.JoinTest`

ADR-0020 Karar 11 ve "Bu PR'da kalanlar" listesinin ikinci maddesi: `detach` kenarında ebeveyn silinince **çocuğun uçları çocuğu döndürmeye devam eder**. `ReadTest` ebeveynin kendi uçlarını, `JoinTest` çocuğun uçlarını sınar.

`JoinTest` kataloğun her `detach` kenarı için bir bildirim ister ve sözleşmedeki `x-resource` ile bağlar. Bildirim üç sonuçtan birini söyler:

| Sonuç | Beklenti |
|---|---|
| `Keeps` | Kenar kuralının kendisi: çocuk yanıtta kalır ve ebeveynden gelen alanları (`NullFields`) `null` okunur. |
| `Hides` | Uç çocuğu bilinçli düşürür. Liste 200 döner ve çocuğu içermez; detay, hiç var olmamış bir çocuğun 404'ünü bayt bayt aynı gövdeyle döner (ADR-0014). Gerekçe zorunludur. |
| `Unauthenticated` | Ebeveyn çağıranın kendisidir; silinmesi oturumu bitirir ve her uç 401 döner. Gerekçe zorunludur. |

Bildirilmeyen bir kenar testi kırmızı yapar: `db/softdelete.yaml`'a eklenen yeni bir `detach` kenarı, çocuğunu okuyan bir uç yazılmadan önce burada patlar. Çocuğunu hiçbir GET okumuyorsa bildirim `Unread` + gerekçedir ve çatı bu iddiayı sözleşmeye karşı denetler; sonradan eklenen bir operasyon iddiayı yanlışlar. Katalogda olmayan bir kenara yazılmış bayat bildirim de bulgudur.

- Silmeden **önce** her uç çocuğu göstermek zorundadır, böylece sonraki kontroller tesadüfen geçemez (`ReadTest`'in kalıbı).
- Kimliği olmayan bir çocuk (bağ satırı) yanıtta onu kanıtlayan bir metni (`Shown`) adlandırır; yol parametreli bir uç çocuğun kimliğini ister.
- Çatının yanlışı yakaladığı DB'siz negatif testlerle kanıtlanır (`TestJoinPlanReportsProblems`, `TestAnswerCatchesLeaks`): kaybolan çocuk, silinmiş ebeveynin verisini hâlâ veren yanıt, gizlenmesi gereken çocuğu listeleyen uç ve silinmiş çağırana hizmet eden uç kırmızıdır.
- Gerçek sözleşmede koşar (`internal/app`): `user_sessions.user_id` `Unauthenticated`, `user_sessions.device_id` `Hides`, `staff_role_assignments.staff_id` `Unauthenticated`, `staff_sessions` ve `staff_login_sources` `Unread`.
- `Keeps` yolunu bugün gerçek sözleşme kanıtlayamıyor: okunan `detach` çocuklarının ebeveyni ya çağıranın kendisi ya da oturumu onsuz ölen cihazıdır. Pozitif yol örnek kaynaklarda kanıtlanır (`example_comments.author_id` → `example_owners`; `TestJoinTestOnExampleResources`), `ReadTest`'in `TestReadTestOnExampleResources` kalıbıyla. Faz 2'nin ilk "silinmiş kullanıcının ilanı listede kalır" ucu geldiğinde o uç `Keeps` ile bildirilir.

### 3. `detach` kenarında `Orphans` davranış koruması değildir

ADR-0020 Karar 10'un son cümlesi ("Bu durumlar için davranış koruması üretilmiş okuma testi (Karar 11) ve `Orphans`'tır") `detach` için yanlıştır. `Orphans` (ADR-0020 Karar 6) yalnız `cascade` ve `restrict` kenarlarını sayar; kod `detach` kenarını atlar. Atlaması doğrudur: `detach` kenarında "silinmiş ebeveynin aktif çocuğu" bir ihlal değil, kenarın tanımıdır. Sayılacak bir değişmez yoktur.

Bunun yerine, lint'in göremediği durumlarda `detach` kenarının koruması şudur:

1. **Davranış:** üretilmiş JOIN testi (Karar 2). Çocuğun ucu, sorgunun nasıl yazıldığını değil ne döndürdüğünü ölçer; view içindeki join, CTE üzerinden takma ad ve elle yazılmış pgx sorgusu da buna dahildir.
2. **Metin:** `CheckJoins`, gördüğü biçimler için (ADR-0020 Karar 10, bu ADR Karar 5).
3. **İnceleme:** elle yazılmış pgx sorguları ve lint'in Karar 4'te sayılan yanlış negatifleri.

`ReadTest` bu listede yoktur: o ebeveynin kendi uçlarını sınar, çocuğun uçlarını değil.

### 4. JOIN lint'inin yanlış negatifleri

ADR-0020 Karar 10'un "Sınır" listesi eksikti. Lint'in **bulgu vermediği**, incelemede bakılacak durumlar şunlardır (hepsi `TestCheckJoins`'in kapsamı dışındadır):

- **`WHERE` içindeki skaler alt sorgu:** `WHERE (SELECT name FROM active_<ebeveyn> p WHERE p.id = c.<kolon>) = $1`. Skaler çerçeve güvenli sayılır, ama sonuç `NULL` olduğu için predicate yanlış döner ve çocuk düşer.
- **LEFT JOIN'lenmiş ebeveynin kolonuna predicate:** `WHERE`/`HAVING` biçimi Karar 5'le yakalanır, parantez içindekiler dahil (`WHERE (p.x = $1)`, `WHERE coalesce(p.x, '') = $1`, `WHERE upper(p.x) = $1`, `WHERE a = $1 AND (p.x = $2 OR p.x = $3)`): açılan her parantez kendi düzeyinin yan tümcesini devralır, içinde bir yan tümce anahtar kelimesi geçerse onunla değişir. Yakalanmayan: ebeveyn kolonunu **başka bir tablonun** `INNER JOIN … ON` koşulunda kullanan biçim.
- **`LEFT JOIN LATERAL` ve dış takma ad:** `LEFT JOIN LATERAL (… p …) x ON true WHERE x.n = $1` yakalanmaz. Dedektör ebeveynin kendi takma adını tutuyor, LATERAL'ın dışarıya verdiği adı (`x`) izlemiyor.
- **`IS TRUE` / `IS FALSE` ayrımı:** `IS NULL`, `IS NOT DISTINCT FROM`, `IS NOT TRUE` ve `IS NOT FALSE` `NULL` satırını korur ve bulgu değildir; `IS TRUE` ve `IS FALSE` onu düşürür ve bulgudur. Dedektör bu dört biçimi biliyor, ama predicate'in anlamını genel olarak çözmüyor: `NULL`'ı koruyan başka bir yazım yanlış pozitif verir ve muafiyet işareti ister. Adıyla iki komşu: **`IS UNKNOWN`** `NULL`'ı korur ve yakalanmaz (yanlış pozitif), **`IS DISTINCT FROM`** ise parametreye bağlıdır — `NULL` bir `$1` ile `NULL` satırı düşer — bu yüzden kırmızı kalması savunulabilir.
- **Yan tümce devralımının getirdiği iki yanlış pozitif:** `WHERE c.body IN (o.name, $1)` ve `WHERE c.body = ANY (ARRAY[o.name, $1])` artık kırmızıdır, oysa ikisi de bazı satırları korur (listenin öteki öğesi eşleşirse). Zorlama yazımlardır ve `db/queries/**`'da yok; çaresi muafiyet işaretidir, ki Karar 2'nin bağı kenar düzeyinde olduğu için doğru `Keeps` bildirimini engellemez.
- **`UPDATE … FROM active_<ebeveyn>`:** `UPDATE active_<çocuk> c … FROM active_<ebeveyn> p WHERE p.id = c.<kolon>`. Çocuk `UPDATE`'ten sonra geldiği için "okunuyor" sayılmaz ve kenar hiç denetlenmez; oysa `FROM`'daki ebeveyn, silinmiş ebeveynin çocuğunu güncellemeden düşürür.
- **İç içe parantezli yarı-join:** `WHERE c.<kolon> IN ((SELECT id FROM active_<ebeveyn>))`. İçteki parantez skaler çerçeve açar ve yarı-join sınıflandırması kaybolur.
- **Aynı satırda `'--'` dizesi:** tokenizasyon yorumları dizelerden önce siler, bu yüzden bir dize sabitinin içindeki `--` satırın kalanını (gerçek bir `JOIN` dahil) yorum sayıp siler. Ters sıra da simetrik bir kaçış üretir (yorum içindeki tek tırnak), bu yüzden sıra değiştirilmedi.

Bu liste ADR-0020 Karar 10'daki "Sınır" maddesinin yerine geçer, onu genişletir. Hepsinin davranış koruması Karar 3'ün listesidir.

### 5. Nullable tarafı `WHERE` ile yok eden predicate: dedektör ve açık muafiyet

`LEFT JOIN active_<ebeveyn> p … WHERE p.<kolon> = …` biçimi lint'i geçiyor ve INNER join gibi davranıyor: join `NULL`'ı üretir, predicate onu düşürür. Depoda iki karşıt kalıp var ve ikisi de doğru:

- `db/queries/authn/staff.sql` ve `sessions.sql`'in oturum kontrolleri nullable'lığı **koruyor**; `NULL`'a Go karar veriyor (`userSessionLive`). Bu kalıp yeşil olmalıdır.
- `db/queries/authz/staff.sql`'in `StaffRoleGraph`'ı ve `sessions.sql`'in oturum listesi/detayı nullable tarafı **bilinçli** yok ediyor: yetki cevabı fail-closed'dır, cihazı silinmiş oturumun bildirecek bir platformu yoktur. Bu kalıp kırmızı olacak ve gerekçesini yazacaktır.

**Karar:** `CheckJoins` bir dedektör daha taşır. `detach` kenarının ebeveyni `LEFT` (ya da `FULL`) join'le okunuyorsa ve aynı sorgunun `WHERE`/`HAVING` yan tümcesi o ebeveynin takma adıyla nitelenmiş bir kolona predicate uyguluyorsa bu bulgudur. `NULL`'ı koruyan iki biçim — `IS NULL` ve `IS NOT DISTINCT FROM` — bulgu değildir.

Muafiyet açıktır, adlandırılmıştır ve gerekçelidir. Sorgunun yorum bloğuna şu satır yazılır:

```sql
-- name: StaffRoleGraph :many
-- … sorgunun kendi açıklaması …
-- softdelete:inner-by-design staff_members: yetki cevabı fail-closed'dır, silinmiş ya da DISABLED üye hiçbir role ulaşmaz
WITH RECURSIVE reached (role_id, parent_role_id) AS (
    …
    LEFT JOIN active_staff_members m ON m.id = a.staff_id
    WHERE … AND m.status = 'ACTIVE'
```

- İşaret, muaf tuttuğu `detach` **ebeveynini adlandırır** (tablo adı ya da `active_` view'ı), iki nokta üst üste ve gerekçe izler. Join satırının değil sorgunun kapsamına yazılır; böylece sqlc onu üretilmiş doc yorumuna taşır (sorgu gövdesine yazılmış bir yorumun üretilmiş `const`'a nasıl geçtiği sqlc'de garanti değil).
- **Yeri zorlanır:** işaret yalnız sorgunun **baştaki yorum bloğunda** (ilk SQL token'ına kadarki `--` satırlarında) muafiyet verir. Bu olmadan, sorgu dilimi bir sonraki `-- name:`'e kadar uzadığı için bir sonraki sorgunun üstüne konan işaret **önceki** sorguyu sessizce muaf tutuyordu.
- **Yanlış yere yazılan işaret kendi başına bulgudur.** Muafiyet verme yetkisini baştaki bloğa indirmek yetmez: işaret *her yerde* aranır ve bulunduğu yer baştaki blok değilse "yanlış yer" bulgusu verir. Yoksa gövdeye, ifadeden sonraya, bir sonraki `-- name:`'in üstüne ya da `$tag$…$tag$` dizesinin içine yazılmış işaret ölü bir beyan olarak kalırdı: bugün hiçbir şeyi bastırmaz, ama sorgunun predicate'i yumuşatıldığı anda sorgunun yaptığının tersini söyler ve Karar 2'nin iki yönlü bağı o kenar için sessizce çözülür. Dört biçimin de kırmızı vakası var — hem nullable tarafı yok eden sorguda (iki bulgu) hem de temiz sorguda (tek bulgu, yalnız yer hakkında).
- Gerekçe en az 20 karakterdir (`x-resource-reason` ile aynı eşik). Ebeveynini adlandırmayan ya da gerekçesiz işaret kendi başına bulgudur **ve** hiçbir şeyi örtmediği için join da bulgu verir: sessiz muafiyet yoktur.
- Hiçbir bulguyu bastırmayan işaret de bulgudur: bayat muafiyet geride kalmaz.
- Muafiyet yalnız bu dedektörü kapsar. INNER join, `FROM`'da ebeveyn ve yarı-join (ADR-0020 Karar 10) işaretle muaf olmaz; `TestCheckJoinsNullableSide` bunu da sabitler.
- Kapsam sorgudur, join değildir: aynı sorguda aynı ebeveyni iki kez LEFT join'leyip birinde bilinçli, ötekinde kazara nullable tarafı yok eden bir yazım ayırt edilmez. Bugün böyle bir sorgu yok; gerektiğinde işaret takma adı da adlandıracak biçimde daraltılır.
- `TestCheckJoinsNullableSide` her biçim için kırmızı/yeşil vaka taşır; `TestQueriesJoinLint` dedektörü `db/queries/**`'a uygular.
- **İşaret ile JOIN testinin bildirimi bağlanır, kenar düzeyinde.** Aynı karar iki yerde yazılı: SQL'deki işaret ve `JoinTest`'teki sonuç. `JoinTest`, `softdelete.MarkedEdges`'in taradığı **kenarları** alır (`Marked`, `<çocuk>.<kolon>` anahtarlı) ve işaret taşıyan kenarın `Keeps` bildirilmesini reddeder; işaretin kaldırılması da `CheckJoins`'i kırar. Böylece ikisinden birini kaldırmak öteki tarafta kırmızı üretir (`db.Queries()` → `internal/app/jointest_integration_test.go`).
- Anahtar **ebeveyn değil kenardır**, çünkü bir ebeveyn birden çok `detach` kenarında durur ve işaret yalnız kendi sorgusunun okuduğu çocuk için konuşur. Ebeveyn düzeyinde olsa çıkışsız bir durum doğardı: bugün `StaffRoleGraph` tek başına `staff_members`'ı işaretliyor, bu da `staff_sessions.staff_id` ve `staff_login_sources.staff_id` kenarlarında `Keeps`'i yasaklardı; o çocukları listeleyen ilk GET geldiğinde ve satır doğru biçimde kaldığında `Keeps` bildirmek zorunlu olur, test reddeder, işareti kaldırmak da JOIN lint'ini kırardı. `MarkedEdges` işareti yalnız bastırdığı kenara yazar: bugün `staff_role_assignments.staff_id` ve `user_sessions.device_id`.

Bu karar ADR-0020 Karar 10'un "çocuktan `LEFT` (ya da `FULL`) join'le, yani nullable tarafta" maddesini daraltır: `LEFT` join artık tek başına yeterli değil, nullable taraf ayakta kalmalı ya da muafiyet yazılmalı.

### 6. `Restore` kilitleri belirlenimci bir sırada alır

`Restore`'un `checkParents`'ı tek bir `SELECT … FOR SHARE OF p` ile ebeveynleri kilitliyordu; ifadede `ORDER BY` yoktu ve kenarlar `Edges()` sırasında (çocuk adına göre) dolaşılıyordu. Kilit sırası böylece plana ve satır sırasına kalıyordu.

**Karar:** iki yerde sıra sabitlendi.

- Kenarlar **ebeveyn tablosuna** göre gruplanıp dolaşılır (`lockOrder`: ebeveyn, sonra çocuk, sonra kolon). `Edges()`'in çocuk sırası, ebeveyn tablolarını çocuklarının adına göre sıralıyordu; gruplama bir tablonun kilitlerini bir arada ve tabloları tek bir sırada tutar.
- Kilit ifadesi `ORDER BY p.id` taşır. Postgres'in `LockRows` düğümü sıralamanın üstündedir, bu yüzden satırlar birincil anahtar sırasında kilitlenir.

İki `Restore` `FOR SHARE` aldığı için **zaten çakışmaz** (paylaşımlı satır kilitleri uyumludur); sıralamanın bugünkü işi bu değil. Yaptığı şudur: kenar sayısı arttıkça, aynı ebeveyn tablosunun birden çok kenar üzerinden **ayrı ifadelerle** kilitlenmesi çapraz bekleme üretebilir; gruplama bir tablonun ifadelerini bir araya, tabloları tek bir sıraya koyar. Bugün katalogda `detach` olmayan tek kenar var, yani bu durum henüz oluşmuyor.

`ORDER BY p.id`'nin kapsamı **ifadenin içidir**, ifadeler arası değil: `lockOrder` global bir satır sırası kurmuyor.

`TestLockOrderGroupsByParent` ve `TestLockOrderKeepsOneParentTogether` yalnız sıralama fonksiyonunu sabitler; sıralamanın eşzamanlılık iddiası için testi yoktur (olmayan bir bloklanmayı sınamak da mümkün değil).

**Sınır: `Restore` ↔ `Delete` arasındaki `40P01` kalıyor.** Sıralama onu kaldırmaz, çünkü iki işlemin kilit sırası tasarımın kendisinden gelir:

- `Delete` değişmezi korumak için **önce ebeveyni** işaretler, sonra çocuğa kaskad eder (ADR-0020 Karar 6).
- `Restore` önce satırları açar (çocuk kilidi), sonra ebeveyni `FOR SHARE` ile kilitler.

Restore çocuğu açmış ve ebeveyni beklerken, Delete ebeveyni işaretlemiş ve o çocuğu beklerse Postgres birini düşürür. Düzeltmek için `Restore`'un ebeveynleri **satırları açmadan önce** kilitlemesi gerekir; bu, hangi ebeveynlerin kilitleneceğini önceden bulan ek bir `SELECT` ve ADR-0020 Karar 6'nın eşzamanlılık kanıtının yeniden yazılması demektir. Bu PR'da yapılmadı: silme sıcak yol değil, `InTx` yeniden denemiyor ve çağıran hatayı bildiriyor (ADR-0020 Karar 6'nın yazılı kararı). Sonuçlar'da açık iş olarak duruyor.

### 7. İki eşzamanlılık testi eklendi

ADR-0020 Karar 6'nın dört testi `cascade` kenarındaki "Restore önce kilitler" durumunu ve `security_barrier`'lı ebeveyni atlıyordu.

- `TestDeleteCascadesOverAConcurrentRestore`: `cascade` kenarında Restore çocuğu açıp ebeveyni `FOR SHARE` ile tutar; Delete ebeveyni işaretlemek için bekler, Restore commit edince kaskad yeni ifadede açılmış çocuğu görür ve onu ebeveynin `deletion_id`'siyle götürür. 409 yoktur. Kardeşi `TestDeleteWaitsForConcurrentRestore` aynı beklemenin `restrict` kenarındaki sonucunu (`SOFT_DELETE_RESTRICTED`) sabitler.
- `TestLockActiveParentOnABarrierView`: `LockActiveParent` `example_owners` üzerinde, yani `security_barrier`'lı bir view üzerinde de kilitler. Bariyer üretimin biçimidir: gerçek şemadaki her soft-delete'li tablo `pii: true`'dur (`users`, `devices`, `staff_members`), bariyerli view düz view gibi düzleştirilmez, ve eski test yalnız bariyersiz `example_projects` ile koşuyordu.

## Yerine geçtiği

Madde madde, ADR-0020'ye göre:

1. **Karar 3, `no-soft-delete-columns`'un desen listesi** → bu ADR Karar 1'in altı deseni. ADR-0020'nin iki deseni korunuyor, dördü ekleniyor; "Sınır" maddesi bu ADR'nin iki sınır maddesiyle değişiyor (DB'de yedeği yok).
2. **Karar 10'un son cümlesi** ("Bu durumlar için davranış koruması üretilmiş okuma testi (Karar 11) ve `Orphans`'tır") → bu ADR Karar 3. `Orphans` `detach` kenarında koruma değildir; koruma JOIN testi, `CheckJoins` ve incelemedir.
3. **Karar 10'un "Sınır" listesi** → bu ADR Karar 4 ile genişliyor (beş yanlış negatif daha).
4. **Karar 10'un "çocuktan `LEFT` (ya da `FULL`) join'le, yani nullable tarafta" maddesi** → bu ADR Karar 5 ile daralıyor: nullable taraf `WHERE`/`HAVING` ile yok edilmemeli ya da sorgunun yorumuna `-- softdelete:inner-by-design <ebeveyn>: <gerekçe>` yazılmalı.
5. **Karar 6'daki `Restore`'un kilit ifadesi** (`SELECT … FOR SHARE OF p`, süzgeçsiz) → bu ADR Karar 6: süzgeçsiz kalır, `ORDER BY p.id` ekler, kenarlar ebeveyne göre dolaşılır. `40P01` kararı (yeniden denenmez) değişmiyor, kapsamı yazıya geçiyor.
6. **Karar 6'nın dört test listesi** → bu ADR Karar 7 ile altıya çıkıyor.
7. **"Bu PR'da kalanlar" listesinin ikinci maddesi** (üretilmiş JOIN testi) → bu ADR Karar 2 ile kapanıyor.

ADR-0013, ADR-0017 ve ADR-0026'nın kararları değişmiyor. ADR-0013 Karar 3'ün "asıl koruma DB'dedir" cümlesinin `deleted_at`/`deletion_id` yazımı için geçerli olmadığı Karar 1'de kayda geçti.

## Sonuçlar

- `detach` kenarı eklemenin yolu uzadı: kenarı `db/softdelete.yaml`'a yazmak yetmez, `JoinTest`'e bir bildirim de gerekir (`Keeps`, `Hides`, `Unauthenticated` ya da `Unread`), ve çocuğu düşüren bir sonuç gerekçe ister. Bildirim yoksa test kırmızıdır.
- Nullable tarafı bilinçli yok eden bir sorgu artık muaf tuttuğu ebeveyni ve gerekçesini kendi yorum bloğuna yazar. Depoda dört sorgu bu işareti taşıyor: `db/queries/authz/staff.sql`'in `StaffRoleGraph`'ı (`staff_members`) ve `db/queries/authn/sessions.sql`'in `ListOpenUserSessions`, `GetOpenUserSession`, `CountRecentDeviceSessions`'ı (`devices`).
- Kolonlarını saymayan `INSERT` için desen kuralı yazıya geçiriyor, korumasını `TestNoSoftDeleteColumnsPatterns` veriyor: o biçim bir view'a sqlc'de hiç derlenmiyor, temel tabloya yazılan biçimi ise `no-base-table` yakalıyor (Karar 1).
- Bu PR'da kalanlar:
  - `deleted_at`/`deletion_id` yazımının DB tarafında yedeği: işaretlemeyi `koala_maint`'in sahip olduğu bir `SECURITY DEFINER` fonksiyonuna taşımak ve `koala_app`'in view yetkisini kolon düzeyinde daraltmak (Karar 1).
  - `Restore` ↔ `Delete` arasındaki `40P01`: `Restore`'un ebeveynleri satırları açmadan önce kilitlemesi (Karar 6).
  - `x-resource`'u her GET operasyonunda zorunlu kılan vacuum kuralı ADR-0026'yla geldi; ADR-0020'nin "Bu PR'da kalanlar" listesinin ilk maddesi böylece kapandı.
  - `Keeps` sonucunun gerçek sözleşmede kanıtlanması, Faz 2'nin ilk "ebeveyni silinmiş çocuk listede kalır" ucuyla (Karar 2). O uç geldiğinde `NullFields` kontrolü daraltılacak: bugün alanın gövdeden tümden kaybolması da `null` sayılıyor, oysa sözleşme alanı nullable bildirecekse alan **var ve `null`** olmalıdır.
  - `LEFT JOIN LATERAL`'ın dış takma adını izlemek ve ebeveyn kolonunu başka bir tablonun `ON` koşulunda kullanan biçimi görmek (Karar 4).
  - Liste sayaçları, `retention` temizliği, anonimleştirme ve `Orphans`'ın metriğe bağlanması ADR-0020'de olduğu yerde duruyor.
