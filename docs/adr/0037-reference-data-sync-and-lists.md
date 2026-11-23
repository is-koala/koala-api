# ADR-0037: Referans verisi: sürümlü dosyalar, eşitleme aracı ve listeler

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, S4
- Kaynak: [ADR-0012](0012-data-access-and-migrations.md) Karar 3 ("referans verisi `0002_seed.sql`'e") ve Karar 5 (UUIDv7), [ADR-0013](0013-soft-delete.md) (M4), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (rol modeli, test DB template'i), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8 ve kapsam defteri, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1 ve 5, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 6 ve 7, açık soru 3; stil rehberi §6; `test/scenarios/media.yaml` MED-012; Java `origin/main` (0cbbce69): `db/changelog/data/*.csv`, `2025/08/21-01-initial-education-data.xml`, `2025/08/23-01-initial-address-data.xml`, `2026/09/05-04-canonical-addresses.xml` (Eminönü), `ReferenceDataController`, `LocationManager`, `AcademicReferenceManager`, `DistrictDao` (okundu, koşturulmadı); koordinasyonun 23.11.2026 notları (yıllık güncelleme asıl gereksinim; PR #30'un iki incelemesi, FIX-FIRST; veri temizliği, önbellek ve meslek yüksekokulu ürün kararları)
- Dış kaynaklar: Flyway "repeatable migrations" ve Liquibase `loadUpdateData`/`runOnChange` (referans verisini her dağıtımda kaynağa eşitleyen yerleşik kalıp); PostgreSQL 17 `CREATE COLLATION … (provider = icu)`, `ALTER COLLATION … REFRESH VERSION`; ICU Türkçe kuralları (`&C<ç<<<Ç &G<ğ<<<Ğ &[before 1]i<ı<<<I &i<<<İ &O<ö<<<Ö &S<ş<<<Ş &U<ü<<<Ü`); RFC 9110 §8.8.3 (`ETag`), §13.1.2 (`If-None-Match`), §13.2.2 (önkoşulların sırası), §15.4.5 (304); RFC 9111 §5.2 (`Cache-Control`); 6360 sayılı Kanun (büyükşehir ilçeleri)
- Kod: `db/referencedata` (`referencedata.go`, `sync.go`, altı CSV, `VERSION`, `README.md`, `tools/from_java.py`, `tools/clean.py`), `db/migrations/0047_reference_places.sql`, `db/queries/reference/reference.sql`, `internal/reference` (`domain.go`, `service.go`, `http.go`, `referencedb`), `internal/app/{refdata.go,reference.go,migrate.go,api.go,authn.go}`, `internal/platform/config` (`KOALA_REFERENCE_ALLOW_RETIRE`), `internal/platform/dbtest/dbtest.go`, `internal/pending/pending.go`, `api/openapi.yaml`, `api/components/headers.yaml`, `db/softdelete.yaml`

## Bağlam

ADR-0036 Karar 7 üniversite, fakülte ve bölüm tablolarını getirdi ama satırlarını ve `/v1/reference` uçlarını bu PR'a bıraktı: o gün `createMyEducation` her kimliğe `REFERENCE_ITEM_NOT_FOUND` dönüyordu. Ülke, şehir ve ilçe tabloları hiç yoktu; adres PR'ı onlara bağlanacak (`AddressInput.cityId`, `districtId`).

YÖK'ün resmi bir API'si ya da toplu indirme dosyası yok; YÖK Atlas sarmalayıcıları resmi değil ve kırılgan. Sektörün yaptığı: veri yerel tablolarda tutulur, yılda bir güncellenir. Bu yüzden **asıl gereksinim yıllık güncellemenin kolay ve güvenli olmasıdır**: yeni kayıt eklenir, değişen güncellenir, düşen silinmez emekliye ayrılır, kimlik hiç değişmez; ve bozuk ya da eski bir dosya veriyi sessizce götüremez.

## Karar

### 1. Veri sürümlü CSV dosyalarıdır, tablolara bir eşitleme aracı yazar

Altı tablo için altı CSV ve bir sürüm numarası (`VERSION`) `db/referencedata/`'dadır ve ikiliye gömülür. Tek fonksiyon `referencedata.Sync(ctx, tx, data, opts)` tabloları dosyaların söylediğine getirir. Seçilmeyenler:

- **Elle yazılmış tek seferlik SQL migration** (`INSERT … VALUES`): ilk yüklemeyi yapar, yıllık güncellemeyi yapmaz. Her yıl elle yazılmış bir fark migration'ı gerekir; ekleneni, değişeni ve düşeni insan hesaplar. Ayrıca megabaytlarca SQL, `sqlc`'nin `db/migrations`'ı şema olarak okuyan 12 girdisinin her birinde yeniden ayrıştırılırdı; bugün dizin 176 KB. ADR-0012 Karar 3'ün `0002_seed.sql`'i bu yüzden yerine geçildi (*Yerine geçtiği* 1).
- **goose Go migration'ı** (gömülü CSV + `COPY`): bir kez koşar. Dosya ertesi yıl değişirse eski migration'ın etkisi değişir, ya da her yıl yeni bir Go migration'ı gerekir.
- **Ayrı bir tohum betiği**: yayında unutulabilir adımdır; unutulduğunda listeler boş, eğitim kayıtları reddedilir.

Seçilen, Flyway'in "repeatable migration"ı ve Liquibase'in `loadUpdateData`'sının kalıbıdır: **kaynak dosya doğruluktur, her dağıtım tabloları ona eşitler, eşitleme idempotenttir.**

### 2. Nerede koşar

- **`koala migrate up`** goose'tan sonra `Sync`'i `koala_owner` olarak tek transaction'da koşar ve tablo başına özeti log'a yazar. Yayın adımı (`scripts/release-db.sh`) yeni bir komut almadı: yeni bir dosya ile gelen sürüm tabloları da değiştirir, ayrı bir adımı unutmak mümkün değildir.
- **`koala reference sync [--dry-run] [--allow-retire=<tablo>:<n>@<sürüm>]`** aynı fonksiyonu tek başına koşar. Önce tablo başına `INSERTED UPDATED RETIRED UNCHANGED`, sonra **yazılan her satırı** basar: `+` eklenen, `~` yeniden adlandırılan (eski → yeni ad, ülkede eski → yeni kod), `^` yeniden kullanıma giren, `-` emekliye ayrılan; her biri kimlik ve adla. Kuru koşu **aynı yoldur**: `Sync` planlar ve yazar, komut transaction'ı geri alır. Ayrı bir "ne yapardım" hesabı yoktur, bu yüzden kuru koşu gerçek koşunun yapmayacağı bir şey söyleyemez.
- **Test template'i** (`dbtest`) migration'lardan sonra aynı `Sync`'i çağırır; her test DB'si veriyle gelir. Template adının özeti migration'ları, CSV'leri, `VERSION`'ı **ve eşitlemenin Go dosyalarını** (`referencedata.SourceFS`, `//go:embed *.go`) kapsar: eşitlemenin mantığı değişince de template yeniden kurulur. `New` adı `templateOf`'tan alır; test o yardımcıyı ve dosya kümesini bağlar.
- **Rol:** tablolar `koala_owner`'ındır, uygulamaya yalnız `SELECT` verilir. `koala_maint`'e yazma verilmedi: referans verisi şemanın durumudur, migration'la aynı kimlikle yazılır.
- **Eşzamanlılık:** `Sync` transaction'ın başında bir advisory kilit alır (`pg_advisory_xact_lock(0x5244, 0)`); planı kilitten sonra okur. İki eşitleme sıraya girer, ikincisi birincinin işlediğini görür ve yapacak bir şey bulmaz. Kilitsiz ikinci, birincinin işlemediği anlık görüntüde plan yapar ve aynı satırı yeniden ekleyip birincil anahtarda düşerdi (`TestConcurrentSyncsSerialize`).
- **Sorgular sqlc'de değil:** eşitleme `koala_owner`'ın migration adımıdır; sqlc'nin paketleri uygulama ve bakım rolleri içindir (ADR-0020). İfadeler `tables()`'taki sabit adlardan kurulur, girdiden ad alınmaz. Diziler `unnest($1::uuid[], …)` ile tek parametrede gider; geçici tablo yok (`0001` `PUBLIC`'in `TEMPORARY` hakkını kaldırır).

### 3. Eşitlemenin kuralları

`Sync` beş adımdır ve ilk dördü hiçbir şey yazmaz:

1. **Doğrulama** (`Dataset.Validate`, veritabanına bakmadan, satırı adlandırarak): sürüm pozitif; başlık ve UUID; tablo içinde tekil kimlik; ebeveyni ebeveyn dosyasında olan satır; ad 1–200 karakter, baştaki ya da sondaki boşluk yok, kontrol karakteri yok, **görünmez biçim karakteri (Unicode Cf: U+200B, U+202E, U+FEFF…) yok**; ülke kodu ISO 3166-1 alpha-2 ve tekil. Ebeveyni düşen satır da düşmek zorundadır: bir üniversiteyi emekliye ayırmak onun fakültelerini ve bölümlerini dosyadan çıkarmayı gerektirir.
2. **Sürüm** (Karar 3b).
3. **Plan:** her tablo okunur ve dosyayla karşılaştırılır, ebeveynler önce. Dosyada olup tabloda olmayan kimlik eklenecektir; adı (ülkede kodu) değişen ya da emekliyken geri gelen güncellenecektir; tabloda kullanımda olup dosyada olmayan emekliye ayrılacaktır. **Taşınan satır reddedilir:** tablodaki bir kimlik dosyada başka bir ebeveynle gelirse eşitleme durur. Başka üniversiteye geçen fakülte başka bir fakültedir ve yeni kimlik alır; eğitim kaydının bileşik FK'si (fakülte, üniversite) çiftini tutar (ADR-0036 Karar 6).
4. **Emeklilik sınırı** (Karar 3a).
5. **Yazım:** planın satırları, tam olarak: güncellemeler, eklemeler, emeklilikler. Her ifadenin etkilediği satır sayısı planınkine eşit olmak zorundadır; kilit altında başka yazan yoktur, fark bir hata ya da elle düzenlemedir ve transaction geri alınır.

**Kimlik değişmez, satır silinmez.** **İkinci koşu hiçbir şey yazmaz**, aynı değeri yeniden yazmak ve sürümü yeniden kaydetmek dahil: plan boşsa hiçbir ifade koşmaz; test transaction'ın hiç kimlik almadığını (`pg_current_xact_id_if_assigned() IS NULL`) doğrular. Ad aynı ebeveyn altında tekil değildir (kısıt yok); temizlenmiş veride tekrarlanan ad kalmadı (Karar 4a), ama kısıt gelecekteki bir YÖK listesini reddedebilirdi.

#### 3a. Emeklilik sınırı

Bir tablonun kullanımdaki satırlarından **%5'inden fazlasını, en az 10 satırdan fazlasını** emekliye ayıracak eşitleme (`RetireLimit(active) = max(10, active·5/100)`), hiçbir şey yazmadan durur ve tabloyu, sayıyı, sınırı ve aşma yolunu adlandırır (`RetireLimitError`). Yalnız başlık satırından oluşan, kesilmiş ya da yanlış dosyayla değiştirilmiş bir CSV bütün tabloyu emekliye ayırırdı; doğrulama bunu yakalayamaz (boş bir yaprak dosya geçerlidir), çünkü önceki duruma bakmaz. Sınır ilk yüklemede devreye girmez: boş tablodan emekli edilecek satır yoktur.

- Sayılar: ülkeler, iller ve 199'a kadar satırlı tablolar için 10; ilçe 48, fakülte 102, bölüm 378.
- **Sıra bağlıdır:** önce plan, sonra sınır, en son yazım. Sınıra takılan eşitlemenin transaction'ı hiç kimlik almaz (`pg_current_xact_id_if_assigned()` boş): yazımlar sınırdan önceye kaysa kırmızıdır (`TestSyncStopsAtTheRetireLimit`).
- **Kullanımdaki satır sayılır:** sınırın tabanı (`Active`) emekli satırları saymaz; geçmiş yılların emekli satırları tabloyu büyütür ve sayılsalar sınırı gevşetirdi (`TestRetireLimitCountsRowsInUse`: 5.000 emekli satırın yanında da 379 emeklilik durur).
- **Aşma yolu bilinçli, tabloya ve veri sürümüne özgüdür:** `koala reference sync --allow-retire=departments:400@2` ya da `migrate up` için `KOALA_REFERENCE_ALLOW_RETIRE=departments:400@2` (virgülle birkaç tablo). Değer, **o veri sürümüne** eşitlemede emekli edilecek satırların üst sınırıdır; daha azı da geçer, fazlası durur. Başka bir sürümü adlandıran izin yok sayılır: eşitleme onu sonuçta (`Result.Ignored`) ve sınır hatasında adlandırır, çağıran `reference allowance ignored` uyarısını basar. Böylece bir yayından sonra kaldırılmayı unutulmuş değişken, sonraki yılın bozuk bir dosyasını geçirmez. Değişken `config.known`'dadır ve biçimi orada denetlenir (`<tablo>:<satır>@<sürüm>`); tablo adlarını eşitleme denetler. Yalnız o yayın için verilir, sonra kaldırılır (runbook).
- **Gerekçe:** yıllık YÖK güncellemesinde kapanan programlar binlerce değil onlarcadır; %5 olağan bir yılı geçirir, bozuk bir dosyayı durdurur. Sınırı açmak bir komut satırı ya da bir değişkenle mümkündür ama sessiz değildir: kuru koşu satırları gösterir, `migrate up`'ın hatası aşma değerini yazar.

#### 3b. Sürüm ve geri sarma

`db/referencedata/VERSION` tekdüze artan bir tam sayıdır; dosyalardaki her değişiklik onu artırır. Eşitlenen sürüm `reference_data_version` tablosunda (tek satır, M6) durur.

- **Dosyanın sürümü veritabanındakinden eskiyse** `Sync` hiçbir şey yazmaz, sonucu `Skipped` döner ve hata vermez; çağıran bir uyarı log'u basar (`reference data left as it is: the database holds a newer version`). Bir yayın eski imaja geri alındığında `migrate up` eski dosyalarla koşar; veriyi geri götürmek yeni sürümün eklediği ya da geri getirdiği satırları emekliye ayırır ve o satırları adlandıran kayıtlar yeni seçilemeyen satırlara bakardı. Sürümler arasında şema değişmediği için yeni veri eski ikiliyle de okunur.
- **Aynı sürüm** eşitler: ikinci koşu bir şey bulmaz, elle yapılmış bir sapma onarılır, sürüm yeniden yazılmaz.
- **Yeni sürüm** eşitler ve sürümü kaydeder.
- Geri alınan yayının verisini de geri almak gerekirse yol, eski dosyalara **daha yüksek** bir sürüm numarası vermektir; eşitleme onu yeni sürüm olarak uygular.
- **Yayımlanmış bir sürüm yeniden yazılmaz.** `TestVersionNamesTheData` her sürümün dosyalarının özetini tutar: dosyaları değiştirip `VERSION`'ı artırmayan bir PR, veritabanlarının zaten tuttuğu bir sürümün özetini değiştirir ve kırmızıdır; yeni sürüm satırını ekler, eskiler kalır.

### 4. Kaynak ve kimlik

- Anlık görüntü Java'nın `db/changelog/data/*.csv`'leridir (0cbbce69). **Kimlikler Java'nınkilerle aynıdır**: istemcinin önbelleğindeki ya da olası bir veri taşımasındaki her kimlik aynı satırı bulur. Java'nın dört denetim kolonu atıldı (`tools/from_java.py`).
- **Eminönü alınmadı.** 2008'de Fatih'e katıldı; Java onu `2026-09-05-04` changelog'unda emekliye ayırdı ve adreslerini Fatih'e taşıdı. Java'nın `district_aliases` tablosu (Google'dan gelen "Eminönü"yü Fatih'e çözen takma ad) adres PR'ının işidir.
- Yeni satırın kimliği UUIDv7'dir (ADR-0012 Karar 5). Kimlik bir doğal anahtardan (YÖK program kodu, ad) türetilmez: ad değişir, ve Java'nın verisinde YÖK kodu yok.

#### 4a. Veri temizliği (ürün kararı, 23.11.2026)

Prod boş, gerçek kullanıcı yok ve ilk yükleme bu PR; temizliğin en ucuz anı şimdiydi. Temizlik `tools/clean.py` ile yapıldı (yalnız standart kütüphane, `python3 -I`); betik idempotenttir ve Java'nın anlık görüntüsünden `from_java.py` + `clean.py` dosyaları bayt bayt yeniden üretir. Kurallar, sırayla:

| # | Kural | Etki | Gerekçe |
|---|---|---|---|
| 1 | NBSP → boşluk, ardışık boşluk → tek, uçlar kırpılır | 61 ad | Çift boşluk ve görünmez farklar arama ve sıralamayı bozar |
| 2 | 30 büyükşehirdeki "MERKEZ" ilçesi çıkar | 23 ilçe | 6360 sayılı Kanun (2012) ve 2014 seçimleriyle büyükşehirlerin merkez ilçeleri adlı ilçelere bölündü; büyükşehirde "Merkez" ilçe yok. Büyükşehir olmayan 51 ilin MERKEZ'i kalır. Betik 30 ilin hepsinin `cities.csv`'de olduğunu denetler |
| 3 | YÖK'ün yurt dışı üniversite notu ("Uluslararası anlaşmalar ile kurulmuş…") fakülte satırı olarak çıkar | 2 fakülte (Hoca Ahmet Yesevi, Manas) | Akademik birim değil, bölümü yok |
| 2b | DERECİK (Hakkari) eklenir; yeni UUIDv7 `01a2077d-d01b-7bb5-8d9b-9c94173f5c34` | +1 ilçe | 2018'de Şemdinli'den ayrıldı, Java verisinde yok. Bununla 81 ilin hepsinde ilçe sayısı resmî sayıyla aynıdır, toplam 973 (koordinasyonun incelemesi, 23.11.2026) |
| 4 | İzmir Yüksek Teknoloji Enstitüsü, İzmir Katip Çelebi'nin fakültesi değil, **kendi başına bir devlet üniversitesidir**; YÖK listesinde hemen ardından gelen üç fakülte (Fen `8119fff5…`, Mimarlık `b2903de4…`, Mühendislik `54fc7dd0…`) ve 18 bölümü de İYTE'nindir | 1 fakülte → 1 üniversite, **aynı kimlik**; 3 fakülte İYTE'ye taşınır, kimlikler aynı | İlk yükleme: hiçbir DB bu kimlikleri tutmuyor, bu yüzden taşıma reddi (Karar 3) burada geçerli değildir. Bölümler İYTE'nin programlarıdır (Fotonik, Biyomühendislik, Şehir ve Bölge Planlama…); İKÇÜ altında kalsalardı İKÇÜ'de iki "Bilgisayar Mühendisliği (İngilizce)" olurdu |
| 4b | "İÇİŞLERİ BAKANLIĞI VE MİLLİ SAVUNMA BAKANLIĞI ADINA SAĞLIK BİLİMLERİ ÜNİVERSİTESİNDE EĞİTİM ALACAKLAR" üniversite satırı, fakülte ve bölümleriyle çıkar | −1 üniversite, −6 fakülte, −9 bölüm | Bir kontenjan notudur, üniversite değil. Altındaki her fakülte ve bölüm, kontenjan eki ve yerleşke parantezi atılınca Sağlık Bilimleri Üniversitesi'nin bir birimidir (Gülhane Diş Hekimliği, Gülhane Eczacılık, Gülhane Tıp, Hamidiye Tıp); taşımak kopya üretirdi. Betik her birinin SBÜ'de karşılığı olduğunu denetler, yoksa durur |
| 5 | Bölümü olan bir kardeşiyle aynı adı taşıyan bölümsüz fakülte çıkar | 5 fakülte | Listedeki ikinci "Tıp Fakültesi" seçilirse bölüm listesi boş gelirdi |
| 6 | Fakültelerde kontenjan ve ücret eki ("(Ücretli)", "(Burslu)", "(%50 İndirimli)", "(İÖ)", "(KKTC Uyruklu)", "(M.T.O.K.)", "(Erkek)", "(Kız)", "(Milli Savunma Bakanlığı Adına)", "(İçişleri Bakanlığı Adına)", "(%25/%75 İndirimli)") | 269 fakülte çıkar, 167 fakülte ekten arındırılmış ada döner, 283 bölüm kalan fakülteye taşınır | YÖK fakülte düzeyindeki programları (Tıp, Hukuk, Diş Hekimliği) kontenjan türü başına bir kez listeler; "Hukuk Fakültesi (Ücretli)" bir fakülte değildir. Kalan: eksiz ad, sonra bölümü olan, sonra en küçük kimlik. Bölümlerin taşınması ilk yükleme olduğu için mümkündür |
| 7 | Bölümlerde aynı ekler: her (fakülte, eksiz ad) için tek satır | 3.674 bölüm çıkar, 2.357 bölüm eksiz ada döner | Ek kontenjanı anlatır, programı değil; özgeçmişteki eğitim kaydı için sektör standardı programın kendisidir. Kalan: eksiz satır varsa o, yoksa en küçük kimlik |

**Ek sayılmayanlar** (programın parçası kalır): öğretim dili ("(İngilizce)", "(Almanca)"…), **uzaktan öğretim ve açıköğretim**, ortak lisans programları ("(UOLP-…)", "SUNY …"), yerleşke ("(Ankara)", "(Kahta)"…). Gerekçe: YÖK bunları kendi program koduyla ayrı program olarak listeler; öğretim dili ve uzaktan öğretim müfredatın ve öğrenimin biçimidir ve işveren için anlamlıdır; ortak program iki diploma verir. **İkinci öğretim (İÖ) ise ek sayıldı:** aynı programın akşam öğretimidir, diplomada ayrım yoktur.

Sonuç: 1 ülke, 81 il, **973** ilçe (resmî sayı; her ilde ilçe sayısı resmî sayıyla aynı), **223** üniversite, **2.043** fakülte, **7.567** bölüm. Bir ilin en çok 39 ilçesi (İstanbul), bir fakültenin en çok 41 bölümü, bir üniversitenin en çok 27 fakültesi var. İki fakülte bölümsüz kaldı ("KKTC Akademik Birim (Lefkoşa)", "Uluslararası Tıp Fakültesi").

### 5. Yer tabloları ve sürüm tablosu

`0047` `countries`, `cities`, `districts`'i getirir: M4, `retired_at`, uygulamaya `SELECT`. Adres PR'ı için iki şey baştan konur:

- `cities (id, country_id)` ve `districts (id, city_id)` tekildir: adres ilçesini şehrine bileşik FK ile bağlayabilir, eğitimin fakültesini üniversitesine bağladığı gibi (ADR-0036 Karar 6). `AddressInput` `cityId` ve `districtId` taşır, ülke şehirden çıkar.
- Ülke ISO 3166-1 alpha-2 kodunu taşır (`TR`, tekil): Google çağrısının ülke kısıtı ve telefon kuralı bu kodla konuşur. **Kod yanıtta yok** (`ReferenceItem` yalnız `id`, `name`); bir istemci ihtiyacı doğana kadar eklenmiyor.

`reference_data_version` (M6, tek satır): eşitlenen sürüm ve zamanı. `koala_owner` yazar, uygulama `ETag` için okur.

### 6. Sıralama: kolonun ICU Türkçe collation'ı

Listeler ada göre sıralıdır ve adlar Türkçedir. Bayt sırası Ç, Ğ, İ, Ö, Ş, Ü'yü Z'den sonraya atar ve ISPARTA'yı IĞDIR'dan önce koyar; ICU kök collation'ı Ş'yi S'yle, İ'yi I'yla, Ç'yi C'yle bir tutar ve ı'yı i'den sonraya koyar ("Tirebolu", "Tıp"tan önce gelirdi). Sektör standardı veritabanının ICU collation'ıdır: `CREATE COLLATION turkish (provider = icu, locale = 'tr-TR')`, altı tablonun `name` kolonunda. Sıra kolonun özelliğidir; her `ORDER BY name` onu kullanır. Eşit adlarda kimlik sırası belirler, sıra tamdır.

- Go'da sıralama (`golang.org/x/text/collate`) seçilmedi: onaylı listede olmayan yeni bir doğrudan bağımlılık olurdu (ADR-0003) ve sıra sorgudan ayrı ikinci bir yerde yazılırdı.
- ICU'suz derlenmiş bir sunucuda `0047` düşer, sessizce bayt sırasına dönmez. CI'ın `postgres:17.11` imajı ICU'ludur. Yönetilen PostgreSQL'lerin (RDS, Cloud SQL) hepsi ICU sunar.
- **ICU yükseltmesi** (işletim sistemi ya da PostgreSQL imajı değişince): collation sürümü uyarısı çıkar. Bugün `turkish` kullanan indeks yoktur, bu yüzden `ALTER COLLATION turkish REFRESH VERSION` yeter. Bir indeks eklendiği gün (adres PR'ı muhtemelen `name` üzerine ekleyecek) önce o indeksler `REINDEX` edilir; adımlar runbook'tadır (`docs/runbook/veritabani.md`). `TestNoIndexUsesTheTurkishCollation` böyle bir indeks belirdiği gün kırmızıdır ve testi ile runbook'u birlikte güncellemeyi ister.

### 7. Uçlar

- **Tek sayfa.** Sözleşme `ReferenceItemList`'i "tek sayfa" diye dondurdu (ADR-0029, MED-012); stil rehberi §6'nın "sayfalama her yerde cursor'lı" kuralının istisnasıdır ve rehbere yazıldı. Listeler küçük ve sınırlıdır: en büyüğü 223 üniversite, bir ilin en çok 39 ilçesi, bir fakültenin en çok 41 bölümü. İstemci bir seçim kutusunu tek istekle doldurur.
- **Emekli satır görünmez.** Ebeveyni yoksa ya da emekliyse liste `404 REFERENCE_ITEM_NOT_FOUND` alır; satırı olmayan ebeveyn (2 fakülte) boş liste alır. İkisini tek sorgu ayırır: ebeveyn `LEFT JOIN` çocuk; satır yoksa ebeveyn yok, çocuğu boş satır varsa liste boş. **Java'dan bilinçli sapma:** Java bilinmeyen ebeveyne `200` ve boş liste dönüyordu (`LocationManager` ve `AcademicReferenceManager` ebeveynin varlığını denetlemiyor; `ReferenceDataController`'ın 404 belgesi kodla tutmuyor). Go'nun 404'ü donmuş sözleşmenin `x-error-codes: [REFERENCE_ITEM_NOT_FOUND]`'ına uyar ve istemcinin bayat bir kimliği "boş il" diye göstermesini önler.
- **Kimlik istemez** (`security: []`, MED-012). Oran sınırı GET varsayılanıdır.
- **Önbellek (ürün kararı, 23.11.2026):** her liste `Cache-Control: public, max-age=86400` ve veritabanındaki veri sürümünden ve revizyonundan türeyen `ETag: "reference-v<sürüm>.<revizyon>"` taşır. Revizyon `reference_data_version`'dadır ve satır yazan her eşitlemede artar, aynı sürümde bir sapmayı onaran eşitlemede de: yalnız sürümden türeyen etiket onarımda değişmez, 304 alan istemci onarımı hiç görmezdi (`TestRepairMovesTheRevision`, `TestReferenceListsAreCacheable`). Hiçbir şey yazmayan eşitleme revizyonu değiştirmez. `If-None-Match` (tam, `W/` ile zayıf, listede ya da `*`) eşleşirse yanıt gövdesiz `304`'tür ve aynı iki başlığı taşır. Sözleşmeye `components/headers.yaml`'da `IfNoneMatch` parametresi ve `ETag`, `CacheControl` yanıt başlıkları, operasyonlara `304` yanıtı eklendi (uyumlu genişleme: yeni isteğe bağlı başlık, yeni yanıt).
  - **Birden çok `If-None-Match` satırı:** üretilmiş bağlayıcı başlık parametresinin tek satırını kabul eder, ikincisinde 400 döner. RFC 9110 §5.3 liste alanlarının satırlarının `, ` ile tek değere birleştirilmesine izin verir; `httpx.CombineListFields` kök zincirde `If-None-Match` ve `If-Match` satırlarını birleştirir. Üretilmiş kod elle düzenlenmedi; sözleşmede bir parametreyi dizi yapmak da seçilmedi, çünkü tek satırlı istemcilerin tipini değiştirir ve birleştirme her gelecek koşullu uç için de geçerlidir. Kök zincirde durur (CLAUDE.md: yeni kapı kök zincire konur), bu yüzden WebSocket dahil her istekte aynıdır (`TestCombineListFields`; uçtan uca iki satırla 304 `TestReferenceListsAreCacheable`).
  - Önkoşul yalnız başarılı olacak istekte değerlendirilir (RFC 9110 §13.2.2): handler önce listeyi okur, bilinmeyen ebeveyn `If-None-Match: *` ile de 404'tür.
  - Sürüm listeden **önce** okunur: araya bir eşitleme girerse liste etiketinden yenidir ve bir sonraki koşullu istek yeni etiketle tam yanıt alır; tersi (eski liste yeni etiketle bir gün önbellekte) olmaz.
  - Etiket temsilin sürümüdür: `ReferenceItem`'a alan eklemek gibi bir biçim değişikliği `ETag` önekini de değiştirmelidir (`reference.ETag`).
- `x-resource` tabloları M4'tür; üretilmiş okuma testi onları atlar (soft-delete'li değiller), ama katalogda sınıflıdırlar.

### 8. Meslek yüksekokulları (önlisans) bu PR'da yok

Java'nın verisinde "Meslek Yüksekokulu" adlı tek bir birim yok; veri YÖK'ün **lisans** listesidir. `EducationDegree.ASSOCIATE` (ADR-0036 Karar 6) bugün yalnız üniversite düzeyinde seçilebilir; fakülte ve bölüm isteğe bağlı olduğu için kayıt yine yazılır. **Yusuf eklemeyi onayladı (23.11.2026); veri ayrı bir PR'da, aynı araçla ve yeni bir sürümle gelecek** (yeni satırlar, UUIDv7 kimlikler).

## "Silinirse kırmızı olur mu?"

| Kural | Nerede | Test |
|---|---|---|
| Sayılar (1/81/973/223/2.043/7.567) ve sürüm | dosyalar | `referencedata.TestDatasetCounts` (DB'siz) |
| Temizliğin yedi kuralı | dosyalar | `referencedata.TestCleanupHolds` (kural başına bir denetim) |
| Template veriyle ve sürümle gelir | `dbtest.syncReference` | `referencedata.TestTemplateHoldsTheReferenceData` |
| `migrate up` veriyi yükler | `runMigrate` | `app.TestMigrateCommand` (up ve down/up sonrası sayılar, özet log'u) |
| Template adı dosyaları, `VERSION`'ı ve eşitlemenin Go dosyalarını kapsar | `dbtest.templateOf`, `templateName`, `referenceFS` | `dbtest.TestTemplateNameCoversTheReferenceData` |
| `New` o adın template'ini kurar ve klonlar | `dbtest.New` | `dbtest.TestNewClonesTheNamedTemplate` |
| İkinci koşu yazmaz | plan boşsa ifade yok | `referencedata.TestSyncIsIdempotent` (xid atanmaz) |
| Düşen satır emekliye ayrılır, geri gelen etkinleşir, satırlar adlandırılır | plan, `apply` | `referencedata.TestSyncFollowsTheFiles` |
| Emeklilik sınırı: yalnız başlıklı CSV durur, sınır tam (378 geçer, 379 durur), izin açar, sınıra takılan transaction hiçbir şey yazmaz | `Sync` adım 5 | `referencedata.TestSyncStopsAtTheRetireLimit`, `TestRetireLimit` |
| Sınırın tabanı kullanımdaki satırlardır | `plan`'ın `Active`'i | `referencedata.TestRetireLimitCountsRowsInUse` |
| İzin bir veri sürümüne bağlıdır; başkası yok sayılır ve adlandırılır | `Sync`, `ParseAllowRetire` | `referencedata.TestAllowanceIsForOneVersion`, `TestParseAllowRetire`, `app.TestMigrateUpHoldsTheRetireLimit` |
| Onarım revizyonu, dolayısıyla `ETag`'i değiştirir | `Sync` adım 6 | `referencedata.TestRepairMovesTheRevision`, `app.TestReferenceListsAreCacheable` |
| Birden çok `If-None-Match` satırı birleştirilir | `httpx.CombineListFields` | `httpx.TestCombineListFields`, `app.TestReferenceListsAreCacheable` |
| İYTE'nin üç fakültesi, İKÇÜ'de aynı adlı bölüm yok, SBÜ kontenjan notu yok, Hakkari'nin beş ilçesi | dosyalar | `referencedata.TestCleanupHolds`, `TestDatasetKeepsJavaIDs` |
| `migrate up` sınırı tutar, değişken açar | `runMigrate` | `app.TestMigrateUpHoldsTheRetireLimit` |
| Eski sürüm yazmaz, aynı sürüm onarır, yeni sürüm kaydedilir | `Sync` adım 2 | `referencedata.TestSyncKeepsANewerVersion` |
| Dosya değişikliği yeni `VERSION` ister | `published()` | `referencedata.TestVersionNamesTheData` |
| Taşınan satır reddedilir, hiçbir şey yazılmaz | plan | `referencedata.TestSyncRefusesAMovedRow` |
| `Sync` kendisi doğrular; izin yalnız referans tablosuna | `Sync` adım 1 | `referencedata.TestSyncRefusesInvalidData` |
| Danışma kilidi | `Sync` adım 2'den önce | `referencedata.TestConcurrentSyncsSerialize` |
| Her doğrulama kuralı (Cf dahil) | `Validate`, `parse`, `parseVersion`, `ParseAllowRetire` | `TestValidate`, `TestParse`, `TestParseVersion`, `TestParseAllowRetire` |
| Değişkenin biçimi | `config.Load` | `config.TestLoadReadsTheReferenceAllowRetire` |
| Kuru koşu yazmaz ve satırları gösterir, gerçek koşu onarır | `syncReference`, `writeResult` | `app.TestReferenceSyncCommand` |
| `reference sync` süper kullanıcıyı reddeder | `db.Open` | `app.TestOwnerCommandsRefuseSuperuser` |
| Türkçe sıra | kolon collation'ı | `app.TestReferenceOverHTTP`: 81 il ve Ankara'nın ilçeleri ICU'dan bağımsız bir sıralayıcıyla; üniversitelerde, Giresun'un fakültelerinde (ı/i) ve Ankara DTCF'nin bölümlerinde (Ç/C ve Ç/D) bayt ve kök sırasını ayıran tanıklar |
| Eşit adlarda kimlik sırası | `ORDER BY …, id` | `app.TestReferenceOrderBreaksTiesByID` (büyük kimlik önce yazılır) |
| Altı `retired_at` süzgeci ayrı ayrı | `reference.sql` | `app.TestReferenceListsHideRetiredRows` (her adım bir satır) |
| Ebeveyn yok 404, çocuksuz ebeveyn boş liste | `children`, `LEFT JOIN` | `reference.TestChildren`, `app.TestReferenceOverHTTP` |
| `ETag`, `Cache-Control`, 304, 404'ün önceliği, sürüm değişince etiket | `reference.http.go`, `ETag`, `Matches` | `app.TestReferenceListsAreCacheable`, `reference.TestETagMatches` |
| ICU indeksi yok | şema | `referencedata.TestNoIndexUsesTheTurkishCollation` |
| Referans uçları yalnız GET, açık, tabloya bağlı | sözleşme | `app.TestReferenceHasNoWriteOperation` |
| Gerçek kimlikle eğitim kaydı | tohum + `createMyEducation` | `app.TestEducationOnTheSeededReference` |
| Java kimlikleri | dosyalar | `referencedata.TestDatasetKeepsJavaIDs` (örneklem) |

**Denenmedi:** testlerin hiçbiri bu Mac'te koşturulmadı (CLAUDE.md); kanıt CI'dadır. Her kuralı tek tek silip testin kırmızıya döndüğü denenmedi; tablo testin neyi bağladığını yazar. ICU sürüm yükseltmesi denenmedi. `from_java.py` + `clean.py`'nin Java'nın anlık görüntüsünden bugünkü dosyaları ürettiği yerelde bir kez denendi; CI bunu koşmaz.

## Java paritesi

| Konu | Java | Go |
|---|---|---|
| Veri yükleme | Liquibase `loadData`, bir kez | her `migrate up`'ta idempotent eşitleme |
| Güncelleme | elle yazılmış changelog (Eminönü) | dosyayı değiştir, sürümü artır; araç ekler, günceller, emekliye ayırır |
| Kimlikler | UUID v4 | aynı UUID'ler; yeni satır UUIDv7 |
| Veri | YÖK lisans listesi olduğu gibi | temizlenmiş (Karar 4a) |
| Silme | `is_deleted` (Eminönü) | `retired_at`, satır kalır |
| Sıra | `ORDER BY name` (veritabanının varsayılan collation'ı) | ICU `tr-TR`, eşitlikte kimlik |
| Bilinmeyen ebeveyn | `200`, boş liste | `404 REFERENCE_ITEM_NOT_FOUND` (bilinçli sapma, Karar 7) |
| Emekli satır listede | `is_deleted` filtresine bağlı | görünmez |
| Önbellek | yok | `Cache-Control`, `ETag`, 304 |
| Kimlik | istemez | istemez |

## Yerine geçtiği

1. **ADR-0012 Karar 3'ün "Referans verisi `0002_seed.sql`'e konur" maddesi** → Karar 1–3: referans verisi bir SQL migration'ı değil, `db/referencedata`'nın sürümlü dosyalarıdır ve `koala migrate up` onları eşitler. ADR-0012'nin geri kalanı değişmiyor.
2. **ADR-0036 Karar 7'nin "tohum referans PR'ında" kalemi** → bu ADR; "verinin kaynağı ve güncelleme politikası (YÖK Atlas, yıllık yenileme, emekliye ayırma)" Karar 1–4'tedir. ADR-0036 açık soru 3 kapanır; meslek yüksekokulları Karar 8'de ayrı PR'a bağlandı.
3. **`db/softdelete.yaml`'daki "rows come by migration"** (akademik tablolar) → satırlar `db/referencedata`'dan `koala migrate up` ile gelir. `0046`'nın yorumu aynı cümleyi taşır; uygulanmış migration düzenlenmedi.
4. **Stil rehberi §6'nın "sayfalama her yerde cursor'lı" kuralı** → referans listeleri tek sayfadır; istisna rehbere yazıldı.
5. **`TestFrozenOperationAnswers501`'in tanığı** `GET /v1/reference/countries` idi; artık `GET /v1/companies/{companyId}/images`.

ADR-0013, ADR-0029 ve ADR-0036'nın kararları başka yerde değişmiyor.

## Sonuçlar

- `createMyEducation` gerçek kimliklerle çalışır (`TestEducationOnTheSeededReference`). Eğitim testleri kendi fikstürlerini (`seedReference`, rastgele kimlikler) kullanmaya devam eder; tohumla çakışmazlar.
- MED-012 (P1) bağlandı. `media` alanının adres operasyonları dondurulmuş kaldığı için alanın P0'ları (MED-007…010) `deferred`'dır; referans uçlarının P0 senaryosu yok.
- **Adres PR'ının yükümlülükleri:** adres `(district_id, city_id)` → `districts (id, city_id)` bileşik FK'siyle bağlanır; Google'ın il ve ilçe adlarını bizim satırlarımıza çözen eşleme (Java: `DistrictDao.findByNameAndCityAllIgnoreCase` ve `district_aliases`) o PR'ın kararıdır; emekli ilçeyi taşıyan adres kalır, yeni adres emekli ilçeyi seçemez (eğitimin kuralı); `name` üzerine `turkish` collation'lı bir indeks eklerse `TestNoIndexUsesTheTurkishCollation`'ı ve runbook'u günceller.
- **Yıllık güncelleme:** `db/referencedata/README.md`'deki adımlar; PR `VERSION`'ı artırır, sayı testlerini ve README tablosunu birlikte değiştirir; yayında `--dry-run` satırları incelenir, sınırı aşan bir emeklilik `KOALA_REFERENCE_ALLOW_RETIRE` ister.
- CI süresi: template bir kez kurulur; eşitleme ~11 bin satırı birkaç ifadeyle yazar.

## Açık sorular

1. **Meslek yüksekokulları:** ayrı PR'da eklenecek, Yusuf onayladı (23.11.2026). Kaynak dosya, tarih ve lisans notu o PR'ın işidir.
2. **Lisans:** YÖK ve il/ilçe listeleri kamu kurumlarının yayımladığı olgusal listelerdir; YÖK'ün bunlar için açık bir veri lisansı yayımladığı doğrulanamadı.
3. **YÖK program kodu:** Java'nın verisinde yok. Yıllık eşleştirme bugün (ebeveyn, ad) ile yapılır; bir ad değiştiğinde ya da aynı adı taşıyan kardeşler oluştuğunda eşleştirme belirsizdir ve elle yapılır (README). İlk YÖK yenilemesinde bir `yok_code` kolonu eşleştirmeyi kararlı yapar.
