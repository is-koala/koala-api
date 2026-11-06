# ADR-0026: Personel RBAC, denetim kaydı çekirdeği ve PII kapısı

- Durum: Kabul edildi
- Tarih: 06.11.2026
- Karar kaydı: K13 (hazırlık), K14 ve S4 (M1'in saklama süreli istisnası), D5; ADR-0014 Karar 5'in uygulaması
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §5.2-5, §5.3, §5.5, §5.6-4/5, §6.1 Faz 1, §6.3; [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md) §8 ve §11, [ADR-0024](0024-authz-core-evaluator-and-conformance.md) §6 ve §10, [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §6, §9 ve açık soru 3; issue #7 (PR #12 kalanları); N01 §4.2–4.4; N05 (core-backend denetim kalıbı); `docs/faz0/authz-personel-izinleri.csv`; Zanzibar (Leopard dizini), GCP IAM, Kubernetes RBAC, NIST SP 800-53 AU-9 ve AC-5, OWASP Logging Cheat Sheet, KVKK m.4 ve m.12
- Kod: `db/migrations/0040_staff_rbac.sql`, `0041_admin_audit.sql`; `internal/authz/permissions.go`, `internal/authz/authzsql`, `internal/audit`, `internal/pii`, `internal/staff`, `internal/app/staff.go`
- Yerine geçtiği:
  - Plan §5.5'teki "Eşleşmeyen yol 403 döner" (Karar 4).
  - ADR-0013 Karar 1 ve 7'nin (S4) M1 tanımındaki "silinmez"i, saklama süresi kataloğa yazılmış kanıt tabloları için daraltır (Karar 7).
  - ADR-0020 §8 (a)'daki "her view bir `active_` view'ıdır" kuralına `authz_tuples` istisnasını ekler (Karar 2); §8 (f)'deki "M1 tablosunda `koala_maint`'in `DELETE` yetkisi yok" kuralını `retention` taşıyan M1 tabloları için daraltır (Karar 7).
  - ADR-0017'nin giriş kümesine (`KOALA_DATABASE_URL`, `KOALA_OWNER_DATABASE_URL`, `KOALA_BOOTSTRAP_DATABASE_URL`) `KOALA_MAINT_DATABASE_URL`'i ekler ve plan §6.1'de Faz 8'e yazılan saklama temizliğini denetim kaydı için Faz 1'e çeker (Karar 6, 7).
  - ADR-0020 §11'deki "`x-resource` zorunluluğunu denetleyen vacuum kuralı ilk gerçek kaynakla gelir" notunu kapatır (Karar 12).
  - ADR-0025 §9'un personel self-servis listesine `/v1/admin/me/capabilities`'i ekler; ADR-0005 madde 1'deki üç biçime (`x-authz`, `security: []`, `x-authz-exempt`) dördüncüsünü, yalnız `x-authz-permission` taşıyan personel operasyonunu ekler (Karar 3).

## Bağlam

ADR-0014 personel RBAC'ın (25 izin, 10 rol, DAG ve görev ayrılığı) aynen taşınmasına, izinlerin her istekte tek özyinelemeli sorguyla çözülmesine ve `x-authz-permission` eşlemesinin fail-closed olmasına karar verdi. ADR-0024 personel tiplerini (`staff`, `staff_role`, `platform`) modele koydu ama matrise, tuple kaynağına ve uygulamaya Faz 1'in bu PR'ına bıraktı; ilk ilişki tablosunu getiren PR'ın yazım zorlamasını kurmasını istedi (§10). ADR-0025 personel girişini getirdi; personel hesabının uygulama tarafından açılamayacağını, hesabı kapatanın `token_version`'ı artırması gerektiğini (§6) ve ilk OWNER'ın nasıl açılacağının açık olduğunu (açık soru 3) yazdı. Issue #7 (PR #12 kalanları) `koala staff create` komutunu ve worker metriklerinin uyarılarını istedi.

Java'da (N01 §4.2–4.4): izinler `StaffPermission` enum'unda, roller ve rol→izin ile rol→rol tabloları veritabanında; görev ayrılığı (`StaffSeparationOfDuties`) güven dalını (`MODERATOR`, `DISPUTE_HANDLER`, `TRUST_LEAD`) finans dalından (`FINANCE_*`) ayırır, `OWNER` muaftır; denetim `admin_audit_entries` (24 ay, finansal 10 yıl) ve `admin_audit_entry_details`'tır (≤8 KB gövde, 6 ay). Java'da PII erişim kaydı yoktur.

## Karar

### 1. İzinler kodda, roller ve atamalar veride

| | İzin tablosu (`staff_permissions`) | **Kodda sabit katalog + DB CHECK** |
|---|---|---|
| Sektör | Bazı yönetim panelleri izinleri de veri tutar | GCP IAM: izinler sistemin tanımıdır, roller izin kümeleridir. Kubernetes RBAC: fiil ve kaynak API'nin tanımıdır, Role/RoleBinding veridir. Keycloak, Auth0: izin adı uygulamanın kodundadır |
| Anlam | Kodun hiç denetlemediği bir izin satırı eklenebilir | İzin, onu denetleyen kodla birlikte doğar; yeni izin = yeni relation + migration |
| Bütünlük | FK | `staff_role_permissions.permission` CHECK'i katalogdur; `TestPermissionCatalogMatchesDatabase` ikisini eşit tutar |

**Karar: izinler kodda** (`authz.Permissions()`: kod, durum değiştirip değiştirmediği, hassasiyet). Her izin `platform:koala`'nın bir relation'ıdır (`payout.release` → `payout_release`); `TestPermissionsMatchTheModel` katalogla `model.fga`'yı eşit tutar. **Roller, rol→izin, rol DAG'ı ve atamalar veridir**:

| Tablo | Sınıf | Not |
|---|---|---|
| `staff_roles` | M4 | `retired_at` ile emekli edilir, silinmez: denetim satırları rolleri kimlikle anar. `duty_branch` (`TRUST`/`FINANCE`) ve `separation_exempt` görev ayrılığının verisidir. |
| `staff_role_permissions`, `staff_role_includes`, `staff_role_assignments` | M6 | Bağlantı; geri alma hard delete'tir, iz `authz_change_log`'dadır. |
| `authz_change_log` | M1 | İlişki değişiminin olay kaydı (plan §5.2-5); saklama süresi yok, silinmez. |

Seed Java'nın rol kimlikleriyle, 10 rol, 25 izin ataması ve 11 kapsama kenarıdır (0040). Rol yönetimi Faz 8'e kadar yalnız migration'la değişir.

### 2. Çözüm: tek özyinelemeli sorgu, karar `authz.Checker`'da

- `authzsql.StaffChecker` her kararda tek sorgu koşar (`StaffRoleGraph`): personelin atandığı ve kapsamayla ulaştığı her rol, kenarlarıyla, ve bu rollerin izinleri. Sorgu personeli `active_staff_members`'a `LEFT JOIN`'le bağlar ve `status = 'ACTIVE'` ister (ADR-0013'ün `detach` kuralı): soft-delete'li ya da `DISABLED` personel hiçbir role ulaşmaz (`TestInactiveStaffHoldNothing`). Emekli rol ne izin verir ne kapsar.
- Satırlar personelin tuple'larına çevrilir; kararı ADR-0024'ün genel değerlendiricisi verir. Önbellek yoktur: rol iptali ve kapsama silinmesi bir sonraki kararda geçerlidir (AUTH-012).
- **Neden tüm tuple'lar değil de öznenin alt grafiği:** Bir personelin ulaşamadığı rol ona hiçbir şey veremez; alt graftaki karar ve `Basis` tüm tuple'lardakiyle aynıdır. İç içe grupları düzleştirmek Zanzibar'ın Leopard dizininin yaptığıdır. Kaynak başka bir personel için sorulursa ret değil hata verir.
- `Basis` rol yolunu taşır: `platform:koala#payout_release > staff_role:<id>#assignee@staff:<id>`. Denetim ve PII erişim satırına yazılır.
- Personel nesnesi olmayan tip sorulursa `StaffChecker` hata verir. Pazaryeri öznesi hiçbir personel izni tutmaz (D5).
- **Platform reddi gizlenmez:** `platform` bir kaynak değildir; izinsiz personel `403 STAFF_PERMISSION_DENIED` alır (`conceal.go`).
- `authz_tuples` VIEW'ı aynı tabloların OpenFGA biçimidir; yalnız uygunluk testi, hata ayıklama ve ileride dışa aktarım içindir, sıcak yolda okunmaz (plan §5.2-1). Hiçbir uygulama rolü onu okumaz. ADR-0020 §8 (a)'nın tek istisnasıdır; katalog testi onu adıyla bekler.

### 3. `x-authz-permission` kapısı fail-closed

- `staff.Gate` authn ve kimlik başına oran sınırından sonra, istek doğrulamasından önce çalışır.
- **Açılışta:** `adminBearer` kullanan her operasyon ya bilinen bir izni (`x-authz-permission`) ya da muafiyeti (`x-authz-exempt`) taşır; ikisi birden ya da hiçbiri, katalogda olmayan izin, `/v1/admin` dışında izin açılışı durdurur. **Muafiyet listesi kapıda kodla sabittir** (`staff.SelfService()`: `/v1/admin/me`, `/v1/admin/me/capabilities`, çıkış, parola değişimi); lint aynı listeyi spec'te zorlar, ama listeye incelemesiz eklenen bir muafiyet sunucuyu açtırmaz.
- **İstekte:** kapının bilmediği route ve personel öznesi olmadan korunan route 500'dür, geçiş değildir.
- **Personel öznesini yalnız `authn` koyar.** Kapı context'te bulduğu principal'ın gerçek izinlerini verir ve denetim satırını onun adına yazar, bu yüzden principal'ı elle koymak bir kimlik taklidi primitifidir: authn'nin `token_version`, `status` ve soft-delete kontrollerini atlar, kapatılmış ya da silinmiş bir OWNER adına da çalışır. Primitif `authn.ContextWithStaffForTest`'tir, adıyla test içindir ve **makineyle fenslenir**: `TestNoStaffImpersonationOutsideTests` (`internal/archtest`) `internal/` ve `cmd/` altındaki test dışı hiçbir dosyanın bu adı anmamasını ister; çağrı, fonksiyon değeri, takma adlı ve nokta import aynı şekilde yakalanır. Pazaryeri tarafında böyle bir kurucu bilerek yoktur: `UserPrincipal` context'e yalnız `authn.Middleware`'den girer. (Go'da arkadaş paket yok; depguard import fensler, fonksiyon fenslemez — bu yüzden kural mimari testtedir.)
- Lint: `koala-operation-authz` dördüncü biçimi kabul eder: yalnız `x-authz-permission` taşıyan, `security`'si boş olmayan operasyon. `x-authz` ile `x-authz-permission` birlikte **reddedilir**: hem genel kuralın birinci biçiminde (`koala-operation-authz`, her yolda, self-servis uçları dahil) hem `koala-admin-permission`'da; personel izninin nesnesi platformdur, ikinci bir karar kaynağı yazılmaz.
- İlk izinli operasyon `GET /v1/admin/permissions`'tır (`role.manage`, Java PI-077).

### 4. Eşlenmemiş personel yolu: 404, 403 değil (AUTH-015)

Plan §5.5 ve Java "eşleşmeyen `/admin/x` 403" der; Java'da bu, Spring Security'nin yetki kararını handler eşlemesinden önce vermesinin sonucudur. **Karar: sözleşmede olmayan yol `404 ROUTE_NOT_FOUND`'dur; sözleşmedeki ama izni olmayan personel `403 STAFF_PERMISSION_DENIED` alır.**
- Sektör pratiği: GitHub, Stripe ve Google API'leri bilinmeyen yola 404 döner; 403 "var ama yetkin yok" demektir (RFC 9110 §15.5.4).
- Fail-closed'un koruduğu şey **eşlemesi eksik operasyondur**; o durumda sunucu açılmaz (Karar 3). Var olmayan yolun koruyacağı bir şey yoktur.
- Sözleşme açıktır (repo public); 404 bir varlık bilgisi sızdırmaz. Kimliksiz istek de aynı 404'ü alır.

Karar AUTH-015'in `target`'ındadır; `go_test`'i (`unmapped staff path` → 404) onu sabitler.

### 5. Görev ayrılığı ve DAG yazım anında değişmez

`staff_roles`, `staff_role_includes` ve `staff_role_assignments`'a her yazım deyiminden sonra, **silme dahil**, veritabanı bütün grafiği denetler (`staff_rbac_check`):
- kapsama grafiği döngüsüzdür (`staff_role_includes_acyclic`);
- muaf olmayan hiçbir rol hem `TRUST` hem `FINANCE` dalına ulaşmaz; muaf rolü olmayan hiçbir personel ikisine birden ulaşmaz (`staff_separation_of_duties`). `OWNER` muaftır (Java); **emekli bir muaf rol muafiyet vermez**. İki dalı da tutan bir personelden OWNER'ı almak ya da OWNER'ı emekli etmek reddedilir (`TestSeparationOfDutiesOnDeleteAndRetire`).

Yazanlar, silenler dahil, aynı danışma kilidinde sıralanır (`BEFORE … FOR EACH STATEMENT`): ikinci yazan birincinin commit'ini bekler, sonra onu görerek denetlenir (`TestGraphWritersQueueOnTheLock`: tek başına geçerli iki atama birlikte reddedilir). Kural veritabanında tek yerdedir (NIST SP 800-53 AC-5).

### 6. Personel yönetimi: `koala staff` (koala_maint)

- `koala staff create --email … --role …` hesabı rollerle ve tek kullanımlık parolayla açar (`must_change_password`); parola yalnız bir kez stdout'a yazılır. **İlk süper admin** bununla, `--role OWNER` ile açılır (issue #7; ADR-0025 açık soru 3).
- Pending satırı önce commit edilir; ayrıntı satırı, hesap, roller ve `SUCCEEDED` satırı **tek tx'tedir**: ayrıntı ya da sonuç yazılamazsa hesap da geri alınır ve parola verilmez. Hata ayrı bir tx'te `FAILED` olarak kaydedilir.
- `koala staff disable --email …` hesabı kapatır. `token_version` artışı bir tetikleyicidir (`staff_members_disable_bumps_token_version`): hesabı `ACTIVE`'den çıkaran her güncelleme sürümü artırır, kim yazarsa yazsın (ADR-0025 §6).
- Yeni değişken `KOALA_MAINT_DATABASE_URL` (`koala_maint` üyesi giriş; ADR-0017'nin giriş kümesine eklenir). **`serve` ortamında sahip, bakım ya da bootstrap girişi görürse açılmaz** (`config.ServeLoginErrors`, en az yetki): bu girişler tek seferlik komutlarındır.
- Rol ataması `store.go`'dadır ve aynı tx'te `authz.RelationChange`'i `authz_change_log`'a yazar. Zorlama (ADR-0024 §10): `TestRelationWritesEmitChanges`, `staffstore` ve `authzdb` depguard kuralları.

### 7. Denetim kaydı çekirdeği

- `admin_audit_entries` ekleme-yalnız kanıttır (M1). **Mutasyon:** önce `PENDING` satır yazılır ve commit edilir; yazılamazsa mutasyon koşmaz. Sonra sonuç satırı (`SUCCEEDED`/`FAILED`) `result_of` ile pending satırı anar. **Ret** tek `DENIED` satırdır, **hassas okuma** tek sonuç satırıdır.
- Satır: aktör, basis, hedef, eylem, izin, request id, durum değiştirip değiştirmediği, hassasiyet, sonuç. Kişisel kısım `admin_audit_entry_details`'tadır: maskelenmiş gövde (≤ 8 KB) ve istemci adresi.
- **Gövde maskesi** iki katmandır: anahtar adı kişisel bir parça içeriyorsa ya da kişisel bir sonekle bitiyorsa (`contactEmail`, `user_phone`, `ibanNumber`, `refreshToken`, `cardNumber`, `authorization`, `apiKey`, `tcKimlikNo`, `soyadi`, `userName`, `pickupLat`…) değer maskelenir; anahtarı ne olursa olsun e-posta içeren, telefon numarası olan ya da Luhn'dan geçen 13-19 haneli kart numarası olan değer de maskelenir — JSON **sayısı** olarak gelse bile. İstisnalar da sonek mantığıyla eşleşir, böylece sözleşmenin kendi alanları kurtulur (`targetRoleName`, `mustChangePassword`, `memberTokenVersion`, `tableName`). Telefon deseni 10-15 hane olduğu için milisaniye epoch'u da maskeler; bilinçli tercih, maske yönünde yanılmak kayıt okunurluğundan ucuzdur.
- Maske bir **red listesidir**: serbest metindeki kişisel veri ve listede olmayan bir anahtarın altındaki değer geçer (açık soru 2). İzin listesine geçiş (operasyonun bildirdiği alanlar dışında hiçbir şey) Faz 2 işidir.
- **Kapı yazar:** personel kapısı reddi, izinli mutasyonu ve hassas izinle okumayı kendisi kaydeder; mutasyon denetimden kaçamaz (`TestGateAuditsOnTheDatabase`, `TestGateRunsNoMutationWithoutItsPendingRow`).
- **Hassas okuma fail-open'dır:** okuma satırı yanıttan sonra yazılır; yazılamazsa yanıt zaten gitmiştir, hata log'a düşer. Mutasyon fail-closed'dur (pending yoksa koşmaz). Okuma satırını yanıttan önce yazmak, her hassas okumayı denetim tablosunun erişilebilirliğine bağlardı; bu bilinçli bir tercihtir (açık soru 3).
- **Değişmezlik veritabanındadır:** `UPDATE` her rol için, sahip dahil, tetikleyiciyle reddedilir; `TRUNCATE` reddedilir; `DELETE` yalnız `koala_maint`'e ve yalnız `retain_until`'i geçmiş satıra açıktır. Sonuç satırı aynı aktör ve eylemin pending satırını kapatmalıdır; bir pending satırın en çok bir sonucu olur.
- **Zamanı veritabanı damgalar:** `created_at := statement_timestamp()`; yazanın gönderdiği değer yok sayılır, `retain_until` ondan türetilir. Hiçbir istemci saati saklamayı kaydıramaz (`TestCreatedAtIsTheDatabases`). Damga fonksiyonları `SECURITY DEFINER`'dır; `koala_app` denetim tablosunu okuyamaz, yalnız ekler.
- **Saklama:** satır 24 ay, `FINANCIAL` satır 10 yıl, ayrıntı 6 ay. Bu değerler Java'nın ve plan §5.5'in değerleridir, sektör pratiğiyle uyumludur (finansal kayıtta VUK ve TTK'nın 10 yılı; işlem kaydında iki yıllık denetim penceresi).
- **M1 ve saklama (S4 ve ADR-0013'ün daraltılması):** M1'in "silinmez"i defter ve kanıt için geçerli kalır. Denetim kaydı yasal saklama süresi olan bir kanıttır: süresi içinde değişmez ve silinmez, süresi dolunca yalnız `koala_maint` siler (KVKK m.4: amaç için gerekli süre). Katalog bunu `retention` alanıyla yazar (yalnız M1'de geçerli); katalog testi bu tablolarda `koala_maint`'in `DELETE` tutmasını, diğer M1 tablolarında tutmamasını ister (ADR-0020 §8 (f)'nin daraltılması). `TestRetentionMatchesCatalog` veritabanının hesapladığı süreyi katalogla karşılaştırır. Temizlik `koala maint audit-purge`'dür (aylık ops işi); `TestPurge` geçmiş tarihli satırları damga tetikleyicisini test veritabanında sahip olarak bir tx için kapatıp yazar, çünkü başka hiçbir yol geçmişi yazamaz.
- **AU-9 sınırı, açıkça:** değişmezlik tetikleyicilerle sağlanır ve tablo sahibi (`koala_owner`, migration'ları koşan rol) bir tetikleyiciyi kapatabilir, tabloyu düşürebilir. Uygulama ve bakım rolleri yapamaz; sahip girişi yalnız migration'da kullanılır ve `serve` onu görürse açılmaz (Karar 6). Bir migration'ın tetikleyiciyi kapalı bırakması `TestEvidenceTriggersAreEnabled` ile yakalanır (`pg_trigger.tgenabled = 'O'`). Sahibe karşı koruma (ayrı bir denetim deposu, WORM yedek) Faz 9'un konusudur.

### 8. PII erişim kaydının saklama süresi: 24 ay

`pii_access_log` 24 ay saklanır, süresi dolunca `koala_maint` siler.
- Java'da karşılığı yoktur; bu kayıt Go tasarımıyla gelir (plan §5.5).
- Gerekçe: sektör pratiği (kişisel veri erişim kayıtları denetim kaydıyla aynı pencerede tutulur) ve KVKK'daki hesap verebilirlik: ilgili kişinin "verime kim, neden baktı" sorusu (m.11) ve Kurul incelemesi için erişimin kanıtlanması gerekir; amaç sona erince silme (m.4, m.7).
- Hukukçu teyidi bekleniyor (açık soru 1).

### 9. PII kapısı iskeleti

- `pii.Gate.Open(ctx, q, staff, dataSubject, purpose, fields)` kararı `user.read_pii`'den verir ve izinliyse `pii_access_log` satırını **aynı tx'te** yazar: personel, basis, veri sahibi, amaç, alanlar, **uç** (route), request id. Ret `(nil, nil)`'dir: yanıt maskelenir (Java PI-088). Satır yazılamazsa grant yoktur; satır bir savepoint'te yazıldığı için tx ayakta kalır ve maskeli yanıt verebilir.
- **Grant commit'ten sonra kullanılır:** erişim satırı tx'le birlikte kalıcı olur; tx geri alınırsa satır da gider. Yanıt, `InTx` nil döndükten sonra grant'la kurulur; böylece hiçbir kişisel değer kayıtlı erişimi olmadan çıkmaz.
- **Tip sistemi:** kişisel değer `pii.Value[T]`'dir, dışa açık alanı yoktur. Açık hâli yalnız `pii.Reveal(grant, value)` ile çıkar. `String`, `GoString`, `Format` (her fmt fiili), `LogValue` ve `MarshalJSON` maskeli döner (`TestValueLeavesMasked`). `Grant`'in `Open` dışında kurucusu yoktur; sıfır `Grant` ve `nil` hiçbir şey açmaz.
- **`pii.Value` dışa açık alanda taşınır.** `fmt` iç içe bir değerin metotlarını yalnız `reflect.Value.CanInterface()` doğruysa çağırır; dışa **kapalı** alandan okunan değer `flagRO` taşır, `fmt` metodu çağıramaz ve struct'ın içini basar — `time.Time`'ın `{wall ext loc}` görünmesiyle aynı neden. Yani `type row struct { phone pii.Value[string] }` + `%+v` ya da `slog.Any("row", r)` açık değeri sızdırır. Go'da bunu tipin içinden kapatmak mümkün değil; kural mimari testtedir: `TestPIIValuesStayInExportedFields` `internal/` ve `cmd/` altında `pii.Value` tutan dışa kapalı alanı (dilim, pointer, map ve iç içe struct içinde de) reddeder.
- **Mimari test** (`TestRevealOnlyInProjections`): `pii.Reveal` yalnız `*pii.Grant` parametresi alan bir fonksiyonda doğrudan çağrılır. Takma adlı ve nokta import, paket düzeyinde func literal ve fonksiyon değeri olarak `Reveal` da yakalanır; dedektörün kendi testi her biçimi sınar.

### 10. Uçlar

- `GET /v1/admin/me` rolleri ve çözülmüş izinleri döner. İşleyici `authn`'dan `staff`'a taşındı.
- `GET /v1/admin/me/capabilities` → `{permissions, can}`; ikisi de aynı çözümden, `Check`'ten (plan §5.5).

### 11. Matris ve uygunluk

- `pairs.py` personel satırlarını üretir (çift S01–S26, 98 satır, hepsi `[go, openfga]`). CSV'nin 101 satırının 92'si matris satırlarının `inventory` etiketlerinde anılır; kalan dokuzu (PI-058–062 kimlik uçları, PI-084–087 kapı davranışı) izin sahipliği sorusu değildir ve karşılıkları `docs/faz0/README.md`'deki eşleme tablosundadır.
- **Verilebilirlik (PI-096–101) ölçülmüyor:** matris `role.manage` ve `staff.manage` sahipliğini ölçer, "verenin kendisinde olmayan izni verememesi" kuralını ölçmez. Rol ve izin verme uçları Faz 8'in personel yönetimiyle gelir; kural AUTH-019'da `deferred: phase 8` olarak izlenir.
- Üç bacak: Go değerlendiricisi, OpenFGA ve **veritabanı** (`TestStaffMatrixOnTheDatabase`: `authz_tuples` = matrisin personel tuple'ları, her satır DB'de matristeki kararı verir). Uygunluk koşusunda 14 nesne tipi iki motorda koşar: önceki 12'ye `platform` ve `staff_role` eklendi (`staff` tipinin relation'ı yoktur).

### 12. Okuma testi ve `x-resource` kuralı

- Her GET ya `x-resource: <tablo>` taşır ya da `x-resource: none` ile en az 20 karakterlik `x-resource-reason` yazar (`koala-get-resource`, lint probu).
- `softdeletetest.ReadTest` gerçek sözleşmeye ve router'a bağlandı (`TestReadTestOnTheContract`): `users` ve `staff_members` fikstürlüdür; muaf tablolar (`user_sessions`, `realtime_tickets`, `staff_role_assignments`) atlanır. Çağıranın kendisini okuyan operasyon `x-resource-self: true` taşır (`/v1/me`, `/v1/admin/me`): silinmeden önce 200 ve kimliği, silindikten sonra 401 ve kimliksiz yanıt beklenir. Testin çağıranı `Authorize` kancasıyla giriş yapar.

### 13. Metrik ve uyarı

`koala_workers_up`, `koala_worker_start_failures_total` ve `koala_auth_lockouts_total` için uyarı kuralları eklendi. `TestAlertRulesUseRegisteredMetrics` artık `serve`'ün kaydettiği her metriği görür (`wireService`; issue #7).

## Sonuçlar

- AUTH-012 tam bağlandı; `currentPhase` 2 oldu. AUTH-015 TOTP için Faz 8'de `partial` kalır. Rol verilebilirliği (PI-096–101) AUTH-019 olarak Faz 8'e `deferred`.
- Yeni değişken: `KOALA_MAINT_DATABASE_URL`. Yeni komutlar: `koala staff create|disable`, `koala maint audit-purge`. Yeni hata kodu: `STAFF_PERMISSION_DENIED`. Yeni bağımlılık yok.
- Her personel isteği izin için bir sorgu daha koşar; ölçüm Faz 9'un yük testine kalır.
- **Açık sorular:**
  1. PII erişim kaydının 24 aylık saklaması (Karar 8) KVKK açısından hukukçuya teyit ettirilecek.
  2. Gövde maskesi anahtar adına ve e-posta/telefon desenine göredir; serbest metindeki ad, adres ya da TCKN maskelenmez. Faz 8'in not ve bayrak uçları bunu yeniden değerlendirir.
  3. Hassas okumanın denetimi fail-open'dır (Karar 7) ve izin düzeyindedir. Java'nın "filtreli arama / tekil okuma" ayrımı uçlar geldikçe operasyon işaretiyle eklenebilir.
  4. Sahibe karşı değişmezlik (AU-9'un tam karşılığı) Faz 9'da ayrı depo ya da WORM yedekle ele alınır.
