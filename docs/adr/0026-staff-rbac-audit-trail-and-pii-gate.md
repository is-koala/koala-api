# ADR-0026: Personel RBAC, denetim kaydı çekirdeği ve PII kapısı

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K13 (hazırlık), K14 ve S4 (M1'in saklama süreli istisnası), D5; ADR-0014 Karar 5'in uygulaması
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §5.2-5, §5.3, §5.5, §5.6-4/5, §6.1 Faz 1, §6.3; [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0024](0024-authz-core-evaluator-and-conformance.md) §6 ve §10, [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §6 ve §9; N01 §4.2–4.4; N05 (core-backend denetim kalıbı); `docs/faz0/authz-personel-izinleri.csv`; Zanzibar (Leopard dizini), GCP IAM, Kubernetes RBAC, NIST SP 800-53 AU-9 ve AC-5, OWASP Logging Cheat Sheet
- Kod: `db/migrations/0040_staff_rbac.sql`, `0041_admin_audit.sql`; `internal/authz/permissions.go`, `internal/authz/authzsql`, `internal/audit`, `internal/pii`, `internal/staff`, `internal/app/staff.go`
- Yerine geçtiği:
  - Plan §5.5'teki "Eşleşmeyen yol 403 döner" (Karar 4).
  - ADR-0013'ün M1 tanımındaki "silinmez"i, saklama süresi kataloğa yazılmış M1 tabloları için daraltır (Karar 7).
  - ADR-0025 §9'un personel self-servis listesine `/v1/admin/me/capabilities`'i ekler; ADR-0005 madde 1'deki üç biçime (`x-authz`, `security: []`, `x-authz-exempt`) dördüncüsünü, yalnız `x-authz-permission` taşıyan personel operasyonunu ekler (Karar 3).

## Bağlam

ADR-0014 personel RBAC'ın (25 izin, 10 rol, DAG ve görev ayrılığı) aynen taşınmasına, izinlerin her istekte tek özyinelemeli sorguyla çözülmesine ve `x-authz-permission` eşlemesinin fail-closed olmasına karar verdi. ADR-0024 personel tiplerini (`staff`, `staff_role`, `platform`) modele koydu ama matrise, tuple kaynağına ve uygulamaya Faz 1'in bu PR'ına bıraktı; ilk ilişki tablosunu getiren PR'ın yazım zorlamasını kurmasını istedi (§10). ADR-0025 personel girişini getirdi; personel hesabının uygulama tarafından açılamayacağını ve hesabı kapatanın `token_version`'ı artırması gerektiğini yazdı.

Java'da (N01 §4.2–4.4): izinler `StaffPermission` enum'unda, roller ve rol→izin ile rol→rol tabloları veritabanında; görev ayrılığı (`StaffSeparationOfDuties`) güven dalını (`MODERATOR`, `DISPUTE_HANDLER`, `TRUST_LEAD`) finans dalından (`FINANCE_*`) ayırır, `OWNER` muaftır; denetim `admin_audit_entries` (24 ay, finansal 10 yıl) ve `admin_audit_entry_details`'tır (≤8 KB gövde, 6 ay).

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
| `authz_change_log` | M1 | İlişki değişiminin olay kaydı (plan §5.2-5). |

Seed Java'nın rol kimlikleriyle, 10 rol, 25 izin ve 11 kapsama kenarıdır (0040). Rol yönetimi Faz 8'e kadar yalnız migration'la değişir.

### 2. Çözüm: tek özyinelemeli sorgu, karar `authz.Checker`'da

- `authzsql.StaffChecker` her kararda tek sorgu koşar (`StaffRoleGraph`): personelin atandığı ve kapsamayla ulaştığı her rol, kenarlarıyla, ve bu rollerin izinleri. Satırlar personelin tuple'larına çevrilir (`staff_role:<id>#assignee@staff:<id>`, `staff_role:<çocuk>#includer@staff_role:<ebeveyn>`, `platform:koala#<izin>@staff_role:<id>#assignee`); kararı ADR-0024'ün genel değerlendiricisi verir. Önbellek yoktur: rol iptali ve kapsama silinmesi bir sonraki kararda geçerlidir (AUTH-012).
- **Neden tüm tuple'lar değil de öznenin alt grafiği:** Bir personelin ulaşamadığı rol ona hiçbir şey veremez; bu yüzden alt grafikteki karar ve `Basis` tüm tuple'lardakiyle aynıdır. İç içe grupları düzleştirmek Zanzibar'ın Leopard dizininin yaptığıdır. Kaynak başka bir personel için sorulursa ret değil hata verir.
- `Basis` rol yolunu taşır: `platform:koala#payout_release > staff_role:<id>#assignee@staff:<id>`. Denetim satırına ve PII erişim satırına yazılır.
- Personel nesnesi olmayan tip (pazaryeri nesneleri) sorulursa `StaffChecker` hata verir. Pazaryeri öznesi hiçbir personel izni tutmaz (D5); sorgu atılmadan reddedilir.
- **Platform reddi gizlenmez.** `platform` bir kaynak değildir, personel yüzeyi sözleşmede açıktır; izinsiz personel `403 STAFF_PERMISSION_DENIED` alır (`conceal.go`).
- `authz_tuples` VIEW'ı aynı tabloların OpenFGA biçimidir; yalnız uygunluk testi, hata ayıklama ve ileride dışa aktarım içindir, sıcak yolda okunmaz (plan §5.2-1). Hiçbir uygulama rolü onu okumaz.

### 3. `x-authz-permission` kapısı fail-closed

- `staff.Gate`, authn ve kimlik başına oran sınırından sonra, istek doğrulamasından önce çalışır: izinsiz çağıran gövde biçimi hakkında bir şey öğrenmez.
- **Açılışta:** `adminBearer` kullanan her operasyon ya bilinen bir izni (`x-authz-permission`) ya da belgeli bir muafiyeti (`x-authz-exempt`, lint'in self-servis listesi) taşır; ikisi birden ya da hiçbiri, katalogda olmayan izin ve `/v1/admin` dışında izin açılışı durdurur.
- **İstekte:** kapının bilmediği route ve personel öznesi olmadan korunan route 500'dür, geçiş değildir.
- Lint: `koala-operation-authz` dördüncü biçimi kabul eder: yalnız `x-authz-permission` taşıyan, `security`'si boş olmayan operasyon (izin nesnesi platformdur; `x-authz` ile birlikte kullanılabilir). `koala-admin-self-service` listesine `/v1/admin/me/capabilities` eklendi.
- İlk izinli operasyon `GET /v1/admin/permissions`'tır (`role.manage`, Java PI-077): izin kataloğu. AUTH-012'yi uçtan uca, gerçek router'da bağlar.

### 4. Eşlenmemiş personel yolu: 404, 403 değil (AUTH-015)

Plan §5.5 ve Java "eşleşmeyen `/admin/x` 403" der. Java'da bu, Spring Security'nin yetki kararını handler eşlemesinden önce vermesinin sonucudur. **Karar: sözleşmede olmayan yol `404 ROUTE_NOT_FOUND`'dur; sözleşmedeki ama izni olmayan personel `403 STAFF_PERMISSION_DENIED` alır.** Gerekçe:
- Sektör pratiği: GitHub, Stripe ve Google API'leri bilinmeyen yola 404 döner; 403 "var ama yetkin yok" anlamındadır (RFC 9110 §15.5.4).
- Fail-closed'un korunan şeyi **eşlemesi eksik operasyondur**; o durumda sunucu hiç açılmaz (Karar 3). Var olmayan bir yolun koruyacağı bir şey yoktur.
- Sözleşme açıktır (repo public, OpenAPI); 404 bir varlık bilgisi sızdırmaz. Kök zincir 404'ü authn'dan önce verir; kimliksiz istek de aynı 404'ü alır.

AUTH-015'in `go_test`'i (`unmapped staff path` → 404) bu kararı sabitler; senaryonun gerekçe metnine yazıldı.

### 5. Görev ayrılığı ve DAG yazım anında değişmez

`staff_role_includes` ve `staff_role_assignments`'a (ve rolün dal/muafiyet kolonlarına) her yazım deyiminden sonra veritabanı bütün grafiği denetler (`staff_rbac_check`):
- kapsama grafiği döngüsüzdür (`staff_role_includes_acyclic`);
- muaf olmayan hiçbir rol hem `TRUST` hem `FINANCE` dalına ulaşmaz; muaf rolü olmayan hiçbir personel ikisine birden ulaşmaz (`staff_separation_of_duties`). `OWNER` muaftır (Java).

Yazanlar aynı danışma kilidinde sıralanır (`BEFORE … FOR EACH STATEMENT`), böylece iki eşzamanlı yazım ayrı ayrı geçip birlikte kuralı bozamaz. Kural veritabanında tek yerdedir; hangi rolle ya da araçla yazılırsa yazılsın uygulanır (NIST SP 800-53 AC-5). Hata kısıt adıyla döner.

### 6. Personel yönetimi: `koala staff` (koala_maint)

- `koala staff create --email … --role …` hesabı rollerle ve tek kullanımlık parolayla açar (`must_change_password`); parola yalnız bir kez stdout'a yazılır, log'a yazılmaz. **İlk süper admin** bununla, `--role OWNER` ile açılır.
- `koala staff disable --email …` hesabı kapatır. `token_version` artışı bir veritabanı tetikleyicisidir (`staff_members_disable_bumps_token_version`): hesabı `ACTIVE`'den çıkaran her güncelleme sürümü artırır, kim yazarsa yazsın (issue #7, ADR-0025 §6).
- Yeni değişken `KOALA_MAINT_DATABASE_URL` (`koala_maint` üyesi giriş). Uygulama rolü personel açamaz (ADR-0025).
- Rol ataması `store.go`'dadır ve aynı tx'te `authz.RelationChange`'i `authz_change_log`'a yazar. Zorlama (ADR-0024 §10): `TestRelationWritesEmitChanges` (mimari test), `staffstore` depguard kuralı (maintdb yalnız `store.go`'dan) ve `authzdb` depguard kuralı (ilişki tabloları yetki için yalnız `internal/authz`'tan okunur, plan §5.6-4).
- Personel yönetimi API'si Faz 8'dedir.

### 7. Denetim kaydı çekirdeği

- `admin_audit_entries` ekleme-yalnız kanıttır (M1). **Mutasyon:** önce `PENDING` satır yazılır ve commit edilir; yazılamazsa mutasyon koşmaz. Sonra sonuç satırı (`SUCCEEDED`/`FAILED`) `result_of` ile pending satırı anar. Sonucu olmayan pending satır, sonucu bilinmeyen bir denemedir (core-backend'in team audit kalıbı). **Ret** tek `DENIED` satırdır, **hassas okuma** tek sonuç satırıdır.
- Satır: aktör (personel ya da sistem), basis, hedef, eylem (operationId ya da `staff.member.create` gibi), izin, request id, durum değiştirip değiştirmediği, hassasiyet, sonuç.
- Kişisel kısım `admin_audit_entry_details`'tadır: alan adına göre maskelenmiş gövde (e-posta, telefon, TCKN, IBAN, parola, token, adres, koordinat, ad…; ≤ 8 KB, aşan gövde boyutuyla bir işarete dönüşür) ve istemci adresi.
- **Kapı yazar:** personel kapısı reddi, izinli mutasyonu (pending → sonuç) ve hassas izinle okumayı kendisi kaydeder; bir mutasyon denetimden kaçamaz. `koala staff` aynı kalıbı sistem aktörüyle kullanır.
- **Değişmezlik veritabanındadır:** `UPDATE` her rol için, sahip dahil, tetikleyiciyle reddedilir; `TRUNCATE` reddedilir; `DELETE` yalnız `koala_maint`'e ve yalnız `retain_until`'i geçmiş satıra açıktır (NIST SP 800-53 AU-9). Sonuç satırı aynı aktör ve eylemin pending satırını kapatmalıdır; bir pending satırın en çok bir sonucu olur.
- **Saklama** (Java): satır 24 ay, `FINANCIAL` satır 10 yıl, ayrıntı 6 ay, PII erişim kaydı 24 ay. `retain_until`'i veritabanı hesaplar; sonuç satırı pending satırınınkini alır.
- **M1 ve saklama (ADR-0013'ün daraltılması):** M1'in "silinmez"i defter ve kanıt için geçerli kalır. Denetim kaydı yasal saklama süresi olan bir kanıttır: süresi içinde değişmez ve silinmez, süresi dolunca yalnız `koala_maint` siler (KVKK m.7: amaç sona erince silme). Katalog bunu `retention` alanıyla yazar (yalnız M1'de geçerli; `P24M`, `P10Y`); katalog testi bu tablolarda `koala_maint`'in `DELETE` tutmasını, diğer M1 tablolarında tutmamasını ister. `TestRetentionMatchesCatalog` veritabanının hesapladığı süreyi katalogla karşılaştırır.
- Temizlik `koala maint audit-purge`'dür (aylık ops işi, `koala_maint`).

### 8. PII kapısı iskeleti

- `pii.Gate.Open(ctx, q, staff, dataSubject, purpose, fields)` kararı `user.read_pii`'den verir ve izinliyse `pii_access_log` satırını **aynı tx'te** yazar: personel, basis, veri sahibi, amaç, alanlar, request id. Ret `(nil, nil)`'dir: yanıt maskelenir, reddedilmez (Java PI-088). Satır yazılamazsa grant yoktur ve hata döner; satır bir savepoint'te yazıldığı için çağıranın tx'i ayakta kalır ve maskeli yanıt verebilir.
- **Tip sistemi:** kişisel değer `pii.Value[T]`'dir, dışa açık alanı yoktur. Açık hâli yalnız `pii.Reveal(grant, value)` ile çıkar; o da grant'in değerin alanını ve veri sahibini açmasını ister. `Grant`'in dışa açık alanı ve `Open` dışında kurucusu yoktur; sıfır `Grant` ve `nil` hiçbir şey açmaz. Maskeli biçim `pii.MaskString`'dir.
- **Mimari test:** `pii.Reveal` yalnız `*pii.Grant` parametresi alan bir projeksiyon fonksiyonunda çağrılır (`TestRevealOnlyInProjections`, dedektörünün kendi testiyle).
- Gerçek PII alanları (TCKN, IBAN, adres, konum) alanlarıyla Faz 2'den itibaren gelir; burada iskelet, örnek projeksiyon (`ExampleReveal`) ve testler var.

### 9. Uçlar

- `GET /v1/admin/me` artık doğrudan atanmış rolleri ve çözülmüş izinleri döner (Java `theIdentityCarriesTheStaffRecordItsRolesAndItsResolvedPermissions`). İşleyici `authn`'dan `staff`'a taşındı.
- `GET /v1/admin/me/capabilities` → `{permissions: [...], can: {<izin>: bool}}`; ikisi de aynı çözümden, `Check`'ten hesaplanır (plan §5.5). Arayüz uç okunamazsa kapalı başlar.

### 10. Matris ve uygunluk

- `pairs.py` personel satırlarını üretir: her izin için doğrudan sahibi, kapsamayla `OWNER`, karşı daldan bir personel ve rolsüz personel; ayrıca pazaryeri `ROLE_ADMIN`'i (D5) ve iki `staff_role#assignee` satırı. 98 satır, hepsi `[go, openfga]`; kaynak PI satırları ve Java testleri.
- Üç bacak: Go değerlendiricisi (`TestMatrixGoEvaluator`), OpenFGA (`TestConformanceWithOpenFGA`) ve **veritabanı** (`TestStaffMatrixOnTheDatabase`): migration'ın rol grafiği matrisin personeliyle atandığında `authz_tuples` matrisin personel tuple'larına (OpenFGA'ya yüklenenlere) eşittir ve her personel satırı tek sorgu + değerlendiriciyle matristeki kararı verir. Böylece plan §5.6-5'in DB ayağı personel tipleri için kapanır.

### 11. Metrik ve uyarı

`koala_workers_up`, `koala_worker_start_failures_total` ve `koala_auth_lockouts_total` için uyarı kuralları eklendi. `TestAlertRulesUseRegisteredMetrics` artık yalnız router'ın değil, `serve`'ün kaydettiği her metriği görür (`wireService`; issue #7).

## Sonuçlar

- AUTH-012 tam bağlandı (`partial` kalktı); `currentPhase` 2 oldu. AUTH-015 TOTP için Faz 8'de `partial` kalır.
- Yeni değişken: `KOALA_MAINT_DATABASE_URL`. Yeni komutlar: `koala staff create|disable`, `koala maint audit-purge`. Yeni hata kodu: `STAFF_PERMISSION_DENIED`.
- Yeni bağımlılık yok.
- Her personel isteği izin için bir sorgu daha koşar (oturum kontrolüne ek). Rol sayısı küçük; ölçüm Faz 9'un yük testine kalır.
- **Açık sorular:**
  1. PII erişim kaydının saklama süresi (24 ay) planda yazılı değildir; denetim satırıyla aynı tutuldu. KVKK açısından hukukçuya teyit ettirilecek.
  2. Gövde maskelemesi alan adına göredir. Alan adı kişisel veri olduğunu söylemeyen serbest metin (not, açıklama) maskelenmez; Faz 8'in not ve bayrak uçları bunu yeniden değerlendirir.
  3. Hassas okumanın denetimi izin düzeyindedir (izin `NORMAL` değilse her okuma). Java'nın "filtreli arama" ve "tekil kaynak okuma" ayrımı uçlar geldikçe kapıya operasyon düzeyinde bir işaretle eklenebilir.
  4. Rol yönetimi uçlarında "verilebilirlik" (aktör verdiği rolün izinlerini tutmalı, PI-096–101) Faz 8'dedir.
