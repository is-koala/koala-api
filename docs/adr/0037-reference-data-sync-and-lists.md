# ADR-0037: Referans verisi: sürümlü dosyalar, eşitleme aracı ve listeler

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, S4
- Kaynak: [ADR-0013](0013-soft-delete.md) (M4), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (rol modeli, test DB template'i), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8 ve kapsam defteri, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1 ve 5, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 6 ve 7, açık soru 3; stil rehberi §6; `test/scenarios/media.yaml` MED-012; Java `origin/main` (0cbbce69): `db/changelog/data/*.csv`, `2025/08/21-01-initial-education-data.xml`, `2025/08/23-01-initial-address-data.xml`, `2026/09/05-04-canonical-addresses.xml` (Eminönü), `ReferenceDataController`, `DistrictDao` (okundu, koşturulmadı); koordinasyonun 23.11.2026 notu (yıllık güncelleme asıl gereksinim, tekrar çalıştırılabilir araç)
- Dış kaynaklar: Flyway "repeatable migrations" ve Liquibase `loadUpdateData`/`runOnChange` (referans verisini her dağıtımda kaynağa eşitleyen yerleşik kalıp); PostgreSQL 17 `CREATE COLLATION … (provider = icu)`; ICU Türkçe kuralları (`&C<ç<<<Ç &G<ğ<<<Ğ &[before 1]i<ı<<<I &i<<<İ &O<ö<<<Ö &S<ş<<<Ş &U<ü<<<Ü`)
- Kod: `db/referencedata` (`referencedata.go`, altı CSV, `README.md`), `db/migrations/0047_reference_places.sql`, `db/queries/reference/reference.sql`, `internal/reference` (`domain.go`, `service.go`, `http.go`, `referencedb`), `internal/app/{refdata.go,reference.go,migrate.go,api.go,authn.go}`, `internal/platform/dbtest/dbtest.go`, `internal/pending/pending.go`, `api/openapi.yaml`, `db/softdelete.yaml`

## Bağlam

ADR-0036 Karar 7 üniversite, fakülte ve bölüm tablolarını getirdi ama satırlarını ve `/v1/reference` uçlarını bu PR'a bıraktı: o gün `createMyEducation` her kimliğe `REFERENCE_ITEM_NOT_FOUND` dönüyordu. Ülke, şehir ve ilçe tabloları hiç yoktu; adres PR'ı onlara bağlanacak (`AddressInput.cityId`, `districtId`).

YÖK'ün resmi bir API'si ya da toplu indirme dosyası yok; YÖK Atlas sarmalayıcıları resmi değil ve kırılgan. Sektörün yaptığı: veri yerel tablolarda tutulur, yılda bir güncellenir. Bu yüzden **asıl gereksinim yıllık güncellemenin kolay ve güvenli olmasıdır** (koordinasyon, 23.11.2026): yeni kayıt eklenir, değişen güncellenir, düşen silinmez emekliye ayrılır, kimlik hiç değişmez.

## Karar

### 1. Veri sürümlü CSV dosyalarıdır, tablolara bir eşitleme aracı yazar

Altı tablo için altı CSV `db/referencedata/`'dadır ve ikiliye gömülür. Tek fonksiyon `referencedata.Sync(ctx, tx, data)` tabloları dosyaların söylediğine getirir. Seçilmeyenler:

- **Elle yazılmış tek seferlik SQL migration** (`INSERT … VALUES` ile 14.876 satır): ilk yüklemeyi yapar, yıllık güncellemeyi yapmaz. Her yıl elle yazılmış bir fark migration'ı gerekir; ekleneni, değişeni ve düşeni insan hesaplar. Ayrıca ~1,6 MB SQL, `sqlc`'nin `db/migrations`'ı şema olarak okuyan 12 girdisinin her birinde (üretim, `vet`, `vet` probu) yeniden ayrıştırılırdı; bugün dizin 176 KB.
- **goose Go migration'ı** (gömülü CSV + `COPY`): bir kez koşar. Dosya ertesi yıl değişirse eski migration'ın etkisi değişir (taze kurulumda yeni veri, eski kurulumda eski veri), ya da her yıl yeni bir Go migration'ı gerekir.
- **Ayrı bir tohum betiği**: yayında unutulabilir adımdır; unutulduğunda listeler boş, eğitim kayıtları reddedilir.

Seçilen, Flyway'in "repeatable migration"ı ve Liquibase'in `loadUpdateData`'sının kalıbıdır: **kaynak dosya doğruluktur, her dağıtım tabloları ona eşitler, eşitleme idempotenttir.**

### 2. Nerede koşar

- **`koala migrate up`** goose'tan sonra `Sync`'i `koala_owner` olarak tek transaction'da koşar ve tablo başına özeti log'a yazar. Yayın adımı (`scripts/release-db.sh`) değişmedi: yeni bir dosya ile gelen sürüm tabloları da değiştirir, ayrı bir adımı unutmak mümkün değildir.
- **`koala reference sync [--dry-run]`** aynı fonksiyonu tek başına koşar ve tablo başına `INSERTED UPDATED RETIRED UNCHANGED` basar. Kuru koşu **aynı yoldur**: `Sync` transaction'da sayar, komut transaction'ı geri alır. Ayrı bir "ne yapardım" hesabı yoktur, bu yüzden kuru koşu gerçek koşunun yapmayacağı bir şey söyleyemez.
- **Test template'i** (`dbtest`) migration'lardan sonra aynı `Sync`'i çağırır; her test DB'si veriyle gelir. Template adının özeti CSV'leri de kapsar: dosya değişince template yeniden kurulur (`TestTemplateNameCoversTheReferenceData`).
- **Rol:** tablolar `koala_owner`'ındır, uygulamaya yalnız `SELECT` verilir. `koala_maint`'e yazma verilmedi: referans verisi şemanın durumudur, migration'la aynı kimlikle yazılır.
- **Eşzamanlılık:** `Sync` transaction'ın başında bir advisory kilit alır (`pg_advisory_xact_lock(0x5244, 0)`); iki eşitleme sıraya girer, ikincisi yapacak bir şey bulmaz.
- **Sorgular sqlc'de değil:** eşitleme `koala_owner`'ın migration adımıdır; sqlc'nin paketleri uygulama ve bakım rolleri içindir (ADR-0020). Altı tablonun dört ifadesi `tables()`'taki sabit adlardan kurulur, girdiden ad alınmaz. Diziler `unnest($1::uuid[], …)` ile tek parametrede gider; geçici tablo yok (`0001` `PUBLIC`'in `TEMPORARY` hakkını kaldırır).

### 3. Eşitlemenin kuralları

- **Kimlik değişmez.** Dosyada olup tabloda olmayan kimlik eklenir; adı (ülkede kodu) değişen güncellenir; emekliyken dosyada yeniden görünen yeniden etkinleşir (güncellenmiş sayılır); dosyadan düşen `retired_at` ile emekliye ayrılır. Hiçbir satır silinmez: eğitim kayıtları (ve adresler) onları adlandırır.
- **Taşınan satır reddedilir.** Tablodaki bir kimlik dosyada başka bir ebeveynle gelirse eşitleme durur ve hiçbir şey yazılmaz. Başka üniversiteye geçen fakülte başka bir fakültedir ve yeni kimlik alır: eğitim kaydının bileşik FK'si (fakülte, üniversite) çiftini tutar (ADR-0036 Karar 6).
- **Önce doğrulama**, veritabanına dokunmadan, satırı adlandırarak (`Dataset.Validate`; `Sync` de çağırır): başlık, UUID, tablo içinde tekil kimlik, ebeveyni ebeveyn dosyasında olan satır, 1–200 karakter, baştaki ya da sondaki boşluk ve kontrol karakteri yok, ülke kodu ISO 3166-1 alpha-2 ve tekil. Ebeveyni düşen satır da düşmek zorundadır: bir üniversiteyi emekliye ayırmak onun fakültelerini ve bölümlerini dosyadan çıkarmayı gerektirir.
- **İkinci koşu hiçbir şey yazmaz**, aynı değeri yeniden yazmak da dahil: güncelleme `IS DISTINCT FROM` ile yalnız farklı satırı seçer, ekleme `ON CONFLICT DO NOTHING`'dir. Test, transaction'ın hiç kimlik almadığını (`pg_current_xact_id_if_assigned() IS NULL`) doğrular.
- Ad aynı ebeveyn altında **tekil değildir**: kaynakta 5 fakülte ve 17 bölüm adını bir kardeşiyle paylaşıyor. Kısıt eklenmedi; kısıt bütün anlık görüntüyü reddederdi.

### 4. Kaynak ve kimlik

- Anlık görüntü Java'nın `db/changelog/data/*.csv`'leridir (0cbbce69). **Kimlikler Java'nınkilerle aynıdır**: istemcinin önbelleğindeki ya da olası bir veri taşımasındaki her kimlik aynı satırı bulur. Java'nın dört denetim kolonu (`created_at`, `updated_at`, `created_by_id`, `updated_by_id`) atıldı; adlar olduğu gibi alındı (59 adda çift boşluk ve iki fakülte adında açıklama cümlesi var; açık soru 4).
- **Eminönü alınmadı.** 2008'de Fatih'e katıldı; Java onu `2026-09-05-04` changelog'unda emekliye ayırdı ve adreslerini Fatih'e taşıdı. Dosya kullanımdaki satırları listelediği için Eminönü yoktur; hiçbir Java kaydı onu adlandırmaz. Java'nın `district_aliases` tablosu (Google'dan gelen "Eminönü"yü Fatih'e çözen takma ad) adres PR'ının işidir.
- Sayılar: 1 ülke, 81 il, 995 ilçe, 223 üniversite, 2.326 fakülte, 11.250 bölüm. Dosyalar ~1,5 MB.
- Yeni satırın kimliği rastgele UUID'dir. Kimlik bir doğal anahtardan (YÖK program kodu, ad) türetilmez: ad değişir, ve Java'nın verisinde YÖK kodu yok.

### 5. Yer tabloları

`0047` `countries`, `cities`, `districts`'i getirir: M4, `retired_at`, uygulamaya `SELECT`. Adres PR'ı için iki şey baştan konur:

- `cities (id, country_id)` ve `districts (id, city_id)` tekildir: adres ilçesini şehrine bileşik FK ile bağlayabilir, eğitimin fakültesini üniversitesine bağladığı gibi (ADR-0036 Karar 6). `AddressInput` `cityId` ve `districtId` taşır, ülke şehirden çıkar.
- Ülke ISO 3166-1 alpha-2 kodunu taşır (`TR`, tekil): Google çağrısının ülke kısıtı ve telefon kuralı bu kodla konuşur. Kod yanıtta yok (`ReferenceItem` yalnız `id`, `name`).

### 6. Sıralama: kolonun ICU Türkçe collation'ı

Listeler ada göre sıralıdır ve adlar Türkçedir. Bayt sırası Ç, Ğ, İ, Ö, Ş, Ü'yü Z'den sonraya atar ve ISPARTA'yı IĞDIR'dan önce koyar; ICU kök collation'ı Ş'yi S'yle, İ'yi I'yla bir tutar (IŞIK, ISPARTA'dan önce gelir). Sektör standardı veritabanının ICU collation'ıdır: `CREATE COLLATION turkish (provider = icu, locale = 'tr-TR')`, altı tablonun `name` kolonunda. Sıra kolonun özelliğidir; her `ORDER BY name` onu kullanır. Eşit adlarda kimlik sırası belirler, sıra tamdır.

- Go'da sıralama (`golang.org/x/text/collate`) seçilmedi: onaylı listede olmayan yeni bir doğrudan bağımlılık olurdu (ADR-0003) ve sıra sorgudan ayrı ikinci bir yerde yazılırdı.
- ICU'suz derlenmiş bir sunucuda `0047` düşer, sessizce bayt sırasına dönmez. CI'ın `postgres:17.11` imajı ICU'ludur. Yönetilen PostgreSQL'lerin (RDS, Cloud SQL) hepsi ICU sunar.
- ICU sürümü değişince collation sürümü uyarısı çıkar; `name` üzerinde indeks olmadığı için yeniden indeksleme gerekmez, `ALTER COLLATION turkish REFRESH VERSION` yeter.

### 7. Uçlar

- **Tek sayfa.** Sözleşme `ReferenceItemList`'i "tek sayfa" diye dondurdu (ADR-0029, MED-012); stil rehberi §6'nın "sayfalama her yerde cursor'lı" kuralının bilinçli istisnasıdır, çünkü listeler küçük ve sınırlıdır: en büyüğü 223 üniversite, bir şehrin en çok 40 ilçesi, bir fakültenin en çok 55 bölümü. İstemci bir seçim kutusunu tek istekle doldurur.
- **Emekli satır görünmez.** Ebeveyni yoksa ya da emekliyse liste `404 REFERENCE_ITEM_NOT_FOUND` alır (Java da bilinmeyen ebeveyne 404 dönüyordu); satırı olmayan ebeveyn (10 fakülte) boş liste alır. İkisini tek sorgu ayırır: ebeveyn `LEFT JOIN` çocuk; satır yoksa ebeveyn yok, çocuğu boş satır varsa liste boş.
- **Kimlik istemez** (`security: []`, MED-012). Oran sınırı GET varsayılanıdır.
- **Önbellek başlıkları eklenmedi.** Sözleşmede `ETag` ya da `Cache-Control` tanımlı değil; sözleşmeyi bu PR'da genişletmek yerine açık soru 3.
- `x-resource` tabloları M4'tür; üretilmiş okuma testi onları atlar (soft-delete'li değiller), ama katalogda sınıflıdırlar.

### 8. Meslek yüksekokulları (önlisans) yok

Java'nın verisinde "Meslek Yüksekokulu" adlı tek bir birim yok: 2.178 fakülte, 142 yüksekokul, 1 akademi ve 5 diğer; bölüm adlarında önlisans programı yok. Veri YÖK'ün **lisans** listesidir. `EducationDegree.ASSOCIATE` (ADR-0036 Karar 6) bugün yalnız üniversite düzeyinde seçilebilir; fakülte ve bölüm isteğe bağlı olduğu için kayıt yine yazılır. Önlisans programlarının verisi YÖK'ün yayımladığı listelerden aynı araçla eklenebilir (yeni satırlar, yeni kimlikler). İnternetten veri indirilip commit edilmedi; açık soru 1.

## "Silinirse kırmızı olur mu?"

| Kural | Nerede | Test |
|---|---|---|
| Sayılar (1/81/995/223/2.326/11.250) | dosyalar | `referencedata.TestDatasetCounts` (DB'siz) |
| Template veriyle gelir | `dbtest.syncReference` | `referencedata.TestTemplateHoldsTheReferenceData` |
| `migrate up` veriyi yükler | `runMigrate` | `app.TestMigrateCommand` (up sonrası ve down/up sonrası sayılar, özet log'u) |
| Template özeti dosyaları kapsar | `dbtest.templateName` | `dbtest.TestTemplateNameCoversTheReferenceData` |
| İkinci koşu yazmaz | `IS DISTINCT FROM`, `ON CONFLICT DO NOTHING` | `referencedata.TestSyncIsIdempotent` (xid atanmaz) |
| Düşen satır emekliye ayrılır, geri gelen etkinleşir | `retire`, `update` | `referencedata.TestSyncFollowsTheFiles` |
| Taşınan satır reddedilir, hiçbir şey yazılmaz | `moved` | `referencedata.TestSyncRefusesAMovedRow` |
| `Sync` kendisi doğrular | `Sync` → `Validate` | `referencedata.TestSyncRefusesInvalidData` |
| Her doğrulama kuralı | `Validate`, `parse` | `referencedata.TestValidate`, `TestParse` (kural başına bir vaka) |
| Kuru koşu yazmaz, gerçek koşu onarır | `syncReference` | `app.TestReferenceSyncCommand` |
| Türkçe sıra | kolon collation'ı | `app.TestReferenceOverHTTP` (81 il ve Ankara'nın ilçeleri ICU'dan bağımsız bir sıralamayla, üniversitelerde bayt ve kök sırasını ayıran tanıklar) |
| Emekli satır görünmez, emekli ebeveyn 404 | sorgular | `app.TestReferenceOverHTTP` |
| Ebeveyn yok 404, çocuksuz ebeveyn boş liste | `children`, `LEFT JOIN` | `reference.TestChildren`, `app.TestReferenceOverHTTP` |
| Referans uçları yalnız GET, açık, tabloya bağlı | sözleşme | `app.TestReferenceHasNoWriteOperation` |
| Gerçek kimlikle eğitim kaydı | tohum + `createMyEducation` | `app.TestEducationOnTheSeededReference` |
| Java kimlikleri | dosyalar | `referencedata.TestDatasetKeepsJavaIDs` (örneklem) |

**Denenmedi:** testlerin hiçbiri bu Mac'te koşturulmadı (CLAUDE.md); kanıt CI'dadır. `TestTemplateNameCoversTheReferenceData` `templateName`'in dosyaları kapsadığını bağlar, `New`'in ona doğru kümeyi verdiğini bağlamaz (çağrı tek satırdır, incelemede bakılır). ICU sürüm yükseltmesi denenmedi. Dosyalar Java'nın verisiyle satır satır karşılaştırılmadı, dönüşüm betiği bir kez koştu ve sayılar ile örneklem bağlandı.

## Java paritesi

| Konu | Java | Go |
|---|---|---|
| Veri yükleme | Liquibase `loadData`, bir kez | her `migrate up`'ta idempotent eşitleme |
| Güncelleme | elle yazılmış changelog (Eminönü) | dosyayı değiştir; araç ekler, günceller, emekliye ayırır |
| Kimlikler | UUID v4 | aynı UUID'ler |
| Silme | `is_deleted` (Eminönü) | `retired_at`, satır kalır |
| Sıra | `ORDER BY name` (veritabanının varsayılan collation'ı) | ICU `tr-TR` |
| Bilinmeyen ebeveyn | 404 | 404 `REFERENCE_ITEM_NOT_FOUND` |
| Emekli satır listede | `is_deleted` filtresine bağlı | görünmez |
| Kimlik | istemez | istemez |

## Yerine geçtiği

1. **ADR-0036 Karar 7'nin "tohum referans PR'ında" kalemi** → bu ADR; "verinin kaynağı ve güncelleme politikası (YÖK Atlas, yıllık yenileme, emekliye ayırma)" Karar 1–4'tedir. ADR-0036 açık soru 3 kapanır, meslek yüksekokulları açık soru 1 olarak sürer.
2. **`db/softdelete.yaml`'daki "rows come by migration"** (akademik tablolar) → satırlar `db/referencedata`'dan `koala migrate up` ile gelir. `0046`'nın yorumu aynı cümleyi taşır; uygulanmış migration düzenlenmedi.
3. **`TestFrozenOperationAnswers501`'in tanığı** `GET /v1/reference/countries` idi; artık `GET /v1/companies/{companyId}/images`.

ADR-0013, ADR-0029 ve ADR-0036'nın kararları başka yerde değişmiyor.

## Sonuçlar

- `createMyEducation` gerçek kimliklerle çalışır (`TestEducationOnTheSeededReference`). Eğitim testleri kendi fikstürlerini (`seedReference`, rastgele kimlikler) kullanmaya devam eder; tohumla çakışmazlar.
- MED-012 (P1) bağlandı. `media` alanının adres operasyonları dondurulmuş kaldığı için alanın P0'ları (MED-007…010) `deferred`'dır; referans uçlarının P0 senaryosu yok.
- **Adres PR'ının yükümlülükleri:** adres `(district_id, city_id)` → `districts (id, city_id)` bileşik FK'siyle bağlanır; Google'ın il ve ilçe adlarını bizim satırlarımıza çözen eşleme (Java: `DistrictDao.findByNameAndCityAllIgnoreCase` ve `district_aliases`) o PR'ın kararıdır; emekli ilçeyi taşıyan adres kalır, yeni adres emekli ilçeyi seçemez (eğitimin kuralı).
- **Yıllık güncelleme:** `db/referencedata/README.md`'deki adımlar; PR sayı testlerini ve README tablosunu birlikte değiştirir; yayında `--dry-run` özeti incelenir.
- CI süresi: template bir kez kurulur; eşitleme ~15 bin satırı birkaç ifadeyle yazar. Her test DB'si template'in kopyasıdır ve kopya birkaç MB büyür. Etki CI'ın `integration` job süresinde ölçülür (PR gövdesi).

## Açık sorular

1. **Meslek yüksekokulları (önlisans programları)**: veri Java'da yok. YÖK'ün yayımladığı önlisans listesinden eklenmesi bir veri kararıdır (kaynak dosya, tarih, lisans); indirme ve commit onay bekler.
2. **Verinin lisansı**: YÖK ve il/ilçe listeleri kamu kurumlarının yayımladığı olgusal listelerdir; YÖK'ün bunlar için açık bir veri lisansı yayımladığı doğrulanamadı.
3. **Önbellek başlıkları**: referans listeleri seyrek değişir ve herkese açıktır; `Cache-Control: public, max-age=…` ve `ETag` sektörde yaygındır. Sözleşmede tanımlı değil; eklemek uyumlu bir değişikliktir ama istemcinin önbellek davranışını bağlar.
4. **Veri temizliği**: (a) 23 büyükşehirde 2012/2014'te kaldırılmış "MERKEZ" ilçesi listede (Antalya, Aydın, …; 995 ilçenin 74'ü MERKEZ, resmi sayı 973); (b) 5 fakülte, bölümü olan bir kardeşiyle aynı adı taşıyan boş bir kopya; (c) iki fakülte adı bir açıklama cümlesi ("Uluslararası anlaşmalar ile kurulmuş…"), biri "İZMİR YÜKSEK TEKNOLOJİ ENSTİTÜSÜ (Devlet Üniversitesi)" adıyla İzmir Katip Çelebi'nin altında; (d) 59 adda çift boşluk. Hepsi dosyadan satır çıkarmak ya da adı düzeltmekle, aynı araçla düzelir (kimlik değişmez). Java paritesi için bu PR'da dokunulmadı.
5. **YÖK program kodu**: Java'nın verisinde yok. Yıllık eşleştirme bugün (ebeveyn, ad) ile yapılır ve aynı adı taşıyan kardeşler (Karar 3) orada belirsizdir. İlk YÖK yenilemesinde bir `yok_code` kolonu eklemek eşleştirmeyi kararlı yapar.
