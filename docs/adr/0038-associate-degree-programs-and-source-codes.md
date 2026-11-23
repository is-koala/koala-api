# ADR-0038: Önlisans programları, bölümün derecesi ve kaynak kodları

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, S4
- Kaynak: [ADR-0012](0012-data-access-and-migrations.md) Karar 5 (UUIDv7), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 6 (`EducationDegree`, `ASSOCIATE`, tek kod `REFERENCE_ITEM_NOT_FOUND`), [ADR-0037](0037-reference-data-sync-and-lists.md) Karar 3, 3a, 4, 4a, 8 ve açık sorular 1 ve 3; Yusuf'un 23.11.2026 onayı (önlisans verisi YÖK'ün listesinden, lisansı doğrulanamadığı bilinerek)
- Dış kaynaklar: YÖK Atlas (`yokatlas.yok.gov.tr`), 2026 ÖSYM tercih kılavuzunun önlisans programları; RFC 9562 §5.5 (UUIDv5); sektörün "dış kimlik eşleme tablosu" kalıbı (bir kaynağın birden çok kodunu tek satıra bağlayan ayrı tablo)
- Kod: `db/referencedata` (`departments.csv`'nin `degree` kolonu, `department_source_codes.csv`, `VERSION` = 2, `referencedata.go`, `sync.go`, `tools/from_yok.py`, `tools/yok_universities.csv`, `tools/yok_units.csv`, `tools/clean.py`, `tools/from_java.py`), `db/migrations/0048_reference_degrees_and_source_codes.sql`, `db/queries/reference/reference.sql`, `db/queries/users/educations.sql`, `internal/reference` (`ProgramDegree`, liste süzgeci), `internal/users/educations.go`, `internal/app/refdata.go`, `api/openapi.yaml`, `db/softdelete.yaml`

## Bağlam

ADR-0037'nin verisi YÖK'ün **lisans** listesiydi (Java'nın anlık görüntüsü): 223 üniversite, 2.043 fakülte, 7.567 bölüm; meslek yüksekokulu yoktu. `EducationDegree.ASSOCIATE` yalnız üniversite düzeyinde seçilebiliyordu (Karar 8). Yusuf önlisans verisinin YÖK'ün listesinden eklenmesini onayladı (23.11.2026).

İki şey daha bu PR'ı bekliyordu: ADR-0037 açık soru 3 (yıllık eşleştirme ad ile yapılıyor, kararlı değil; bir `yok_code` önerildi) ve eğitim kaydında derece ile bölümün tutarlılığı (lisans bölümü + `ASSOCIATE`).

## Karar

### 1. Kaynak: YÖK Atlas'ın kılavuz listesi, tek istekle

`yokatlas.yok.gov.tr` 2025'te tek sayfalık bir uygulamaya dönüştü; eski `onlisans-univ.php` sayfaları yok. Sayfanın kendi kullandığı açık uç `POST /api/tercih-kilavuz/search` (kimlik istemez) yılın ÖSYM kılavuzundaki programları döner. `{"filters":{"birimTuruId":47},"size":10000}` (47 = "ÖNLİSANS") **2026 kılavuzunun 9.254 önlisans kaydını tek yanıtta** verdi. Her kayıt ÖSYM program kodunu (`kilavuzKodu`, 9 hane), YÖK'ün üniversite ve birim kimliklerini (`universiteId`, `fymkId`) ve adlarını taşır.

- Çekim 23.11.2026, toplam 7 istek (README'de URL'leriyle). `robots.txt` bir yönerge taşımıyor (sunucu yerine bir kaynak dosyası dönüyor); istekler arasında beklendi, paralel istek yok. Ham yanıt depoya girmez; depoya yalnız dönüştürülmüş CSV'ler girer.
- **Sınır:** kılavuz yalnız o yıl öğrenci alan programları listeler. O yıl öğrenci almayan ya da kapanmış bir programın mezunu programını bulamaz. Lisans verisi (Java) için de aynı durum geçerliydi.
- **Lisans:** ADR-0037 açık soru 2 gibi doğrulanamadı; Yusuf bilerek onayladı.

### 2. Veri modeli: birim fakülte, program bölüm, derece programın

- Meslek yüksekokulu (ve önlisans programı olan başka birim) bir **`faculties`** satırıdır, üniversitesinin altında. Önlisans programları o birimin **`departments`** satırlarıdır. Şema, uçlar ve eğitim kaydının bileşik FK'leri değişmeden kalır.
- **Derece programın özelliğidir, birimin değil:** `departments.degree` (`ASSOCIATE` | `BACHELOR`, `EducationDegree`'nin değerleri). Birime tür kolonu konmadı: yedi birim iki derecede de program verir (Anadolu, Atatürk, İstanbul ve Ankara'nın açık ve uzaktan öğretim fakülteleri, DAÜ'nün iki yüksekokulu); birim düzeyindeki bir tür onları ya iki satıra bölerdi ya yanlış söylerdi. YÖK de dereceyi programda (`birimTuruId`) tutar.
- `0048` mevcut satırlara `BACHELOR` verir (ADR-0037'nin verisi lisans listesidir), sonra varsayılanı kaldırır: her satırın derecesini eşitleme yazar.
- **Derece kimliğin parçasıdır.** Bir önlisans programı lisans programına dönüşmez; dönüşen YÖK satırı yeni bir programdır. Eşitleme, veritabanındaki bir bölümün derecesini değiştiren dosyayı taşınan satır gibi **reddeder** (ADR-0037 Karar 3). Eğitim kaydı dereceyi tuttuğu için sessiz bir değişiklik kayıtları tutarsız bırakırdı.
- Aynı birimde aynı adı taşıyan lisans ve önlisans programı iki programdır. Önlisansınki YÖK'ün kendi yazımıyla ayrılır: "(Önlisans)" program adından sonra, öteki parantezlerden önce ("Halkla İlişkiler ve Tanıtım (Önlisans) (Açıköğretim)"; YÖK "İlahiyat (Önlisans) (Açıköğretim)" yazar). 2026'da iki ad (Atatürk Açıköğretim). "Aynı ebeveyn altında tek ad" temizlik kuralı (ADR-0037 Karar 4a, 5–7) böylece iki derece için de geçerli kalır.

### 3. Üniversiteye bağlı olmayan vakıf meslek yüksekokulları

Dört vakıf MYO bir üniversiteye bağlı değildir; YÖK onları kendi `universiteId`'leriyle listeler: Ataşehir Adıgüzel, İstanbul Sağlık ve Sosyal Bilimler, İstanbul Şişli ve İzmir Konak MYO'ları. Her biri **bir `universities` satırı ve altında kendi adını taşıyan tek bir birimdir**.

- Ayrı bir tür (tablo ya da kolon) seçilmedi. Eğitim kaydı zorunlu bir `universityId` ile bir **yükseköğretim kurumuna** bağlanır (ADR-0036 Karar 6). Bu MYO'lar YÖK'ün hiyerarşisinde üniversiteyle aynı düzeydedir. Ayrı bir tür sözleşmeyi (`universityId` zorunlu) ve bileşik FK'yi değiştirirdi; istemci de aynı akışla seçer.
- Satırın adı YÖK'ün adıdır ("İZMİR KONAK MESLEK YÜKSEKOKULU"); listede bir üniversite olmadığı adından okunur.

### 4. Üniversite eşleştirmesi: açık bir tablo, bulanık eşleştirme yok

YÖK'ün 197 kurumu `tools/yok_universities.csv`'de tek tek karara bağlanır: YÖK kimliği, YÖK adı, bizim kimliğimiz ya da `new`, dayanak. Tabloda olmayan ya da adı tablodakinden farklı gelen kurum `from_yok.py`'yi durdurur: yeni kurum ve ad değişikliği bir insan kararıdır.

| Dayanak | Sayı | Not |
|---|---|---|
| `name` (ad aynı) | 105 | |
| `name without place` (YÖK'ün sondaki yer parantezi atılınca aynı: "TÜRK HAVA KURUMU ÜNİVERSİTESİ (ANKARA)") | 79 | |
| `renamed` | 5 | Alanya Hamdullah Emin Paşa → Alanya Üniversitesi; Antalya AKEV → Antalya Belek; Beykent → İstanbul Beykent; Nişantaşı → İstanbul Nişantaşı; İstanbul Ayvansaray → İstanbul Topkapı. Dayanak: YÖK'ün 2026 lisans listesindeki fakülte kümeleri bizim satırların fakülteleriyle örtüşüyor (tek bir ek istekle karşılaştırıldı) |
| `new` | 8 | 4 bağımsız vakıf MYO (Karar 3); Ankara Müzik ve Güzel Sanatlar, Sivas Bilim ve Teknoloji, Mudanya, Kıbrıs Aydın üniversiteleri (Java'nın lisans listesinde yoklar) |

**Ad değişiklikleri bu sürümde uygulanmadı.** Beş satır eski adlarıyla kalır, önlisans birimleri o kimliklerin altına girer. Sürüm 2 yalnız ekleme yapar (Karar 8). Yeni adlar lisans verisinin YÖK'ten yenilenmesinin işidir (açık soru 1). Dört yeni üniversitenin lisans fakülteleri de o işe kalır; bugün bu üniversitelerin yalnız önlisans birimleri var.

Birimler de aynı ilkeyle eşleşir. Önlisans programı olan dokuz lisans dışı birimden yedisi zaten `faculties.csv`'deydi; bunlar `tools/yok_units.csv`'de açıkça bağlanır. Beşi ad aynı olduğu için eşleşti. İkisi yeniden adlandırılmış birimdir: Atatürk'ün "Açık ve Uzaktan Öğretim Fakültesi" bizim "Açıköğretim Fakültesi" satırımızdır; ASBÜ'nün "Kuzey Kıbrıs Yerleşkesi" bizim bölümsüz "KKTC Akademik Birim (Lefkoşa)" satırımızdır ve artık bölümleri var. Kalan iki birim (DAÜ Sağlık Hizmetleri YO, Anadolu Engelliler Entegre YO) yeni satırdır.

### 5. Kaynak kodları: ayrı bir eşleme tablosu

`department_source_codes (code, department_id, retired_at)`; kod ÖSYM program kodudur (YÖK Atlas'ın "program kodu", `^[0-9]{9}$`), birincil anahtardır. Satırlar `department_source_codes.csv`'den gelir.

- **Neden ayrı tablo:** kontenjan varyantları (Burslu, %50 İndirimli, Ücretli, KKTC Uyruklu…) YÖK'te ayrı kodlardır. Temizlik onları tek bölümde birleştirir (Karar 7), ama gelecek yılın listesini eşleştirmek için **her kodun** bilinmesi gerekir: düz varyant kapanıp yalnız "Burslu"su kalırsa program yine bulunmalı. `departments`'a tek bir `yok_code` kolonu bunu taşıyamazdı. Bir kaynağın birden çok kimliğini tek satıra bağlayan ayrı eşleme tablosu sektörün kalıbıdır.
- **Lisans bölümlerinin kodu yok.** Java'nın verisinde YÖK kodu yoktu; 7.567 lisans bölümü için satır yok (kolonun "nullable" karşılığı budur). Lisans verisi YÖK'ten yenilendiğinde dolar (açık soru 1).
- **Eşitlemenin kuralları kodlara da uygulanır;** kodlar onları atlatamaz:
  - Dosyadan düşen kod silinmez, emekliye ayrılır; geri gelen kod yeniden kullanıma girer.
  - Veritabanındaki bir kodu başka bölümle getiren dosya **reddedilir**. Kod bir programı adlandırır; onu başka satıra çevirmek gelecek yılın eşleştirmesini yanlış satıra götürürdü.
  - Emeklilik sınırı (ADR-0037 Karar 3a) bu tabloya da ayrıca uygulanır: 9.254 kodun %5'i 462'dir. Bölümlere verilen izin kodların sınırını açmaz. Bölümleri kalan ama kod dosyası kesilmiş bir veri de durur.
  - Kodlar bölümlerden sonra planlanır ve yazılır; doğrulama her kodun bölümünün dosyada olmasını ister. Bölüm düşünce kodları da düşmek zorundadır, ebeveyn kuralı gibi.
  - Plan ve yazım genel tablo yolunun aynı adımlarıdır (`planSourceCodes`, `applySourceCodes`). Tablo kimliğe değil koda göre anahtarlandığı için `tables()`'ta değil, `Tables()`'tadır: izin (`--allow-retire=department_source_codes:<n>@<sürüm>`), sayım ve özet onu da kapsar.
- Uygulamaya `SELECT` verildi (öteki referans tabloları gibi), ama listesi yoktur: kodlar sunulmaz.

### 6. Kimlikler deterministik: UUIDv5

YÖK'ten gelen yeni satırın kimliği bir **UUIDv5**'tir. Namespace `c1f995d4-bea2-5c30-870f-ea391ab09716` = `uuid5(NAMESPACE_URL, "https://github.com/is-koala/koala-api/tree/main/db/referencedata")`; betik ikisinin tuttuğunu denetler. Ad şunlardan biridir:

- üniversite `yok:university:<universiteId>`,
- birim `yok:unit:<fymkId>` (aynı ad altında birleşen birimlerde en küçük `fymkId`),
- program `osym:<kod>` (eksiz kaydın kodu, yoksa en küçük kod).

Aynı kaynak aynı kimlikleri üretir: dosyalar ham yanıttan bayt bayt yeniden üretilebilir. **Kimlik yine değişmez.** Türetme yalnız yeni satır içindir. Var olan satır önce kodlarıyla (`department_source_codes.csv`), sonra türetilmiş kimliğiyle, sonra aynı ebeveyn altında aynı adla bulunur. Bu yüzden bir program "kanonik" kodunu yitirse de kimliği kalır.

ADR-0037 Karar 4'ün "yeni satırın kimliği UUIDv7'dir, kimlik bir doğal anahtardan türetilmez" kuralı **YÖK'ten gelen satırlar için** bunun yerine geçer (*Yerine geçtiği* 1). O kuralın gerekçesi iki şeydi: "ad değişir" ve "Java'nın verisinde kod yok". İlki türetmenin adı değil YÖK kodunu kullanmasıyla karşılanır. İkincisi önlisans için geçerli değildir. Türetilmiş kimlik bir kodun sahibini sonsuza dek tutmaz; eşleştirmeyi kod tablosu yapar. Elle eklenen satırlar (DERECİK gibi) UUIDv7 kalır. Kimliğin sürümü (5) satırın YÖK'ten türediğini gösterir; `TestAssociatePrograms` her önlisans programının kimliğinin kodlarından birinden türediğini doğrular.

### 7. Temizlik

`tools/from_yok.py` yalnız standart kütüphaneyle, `python3 -I` ile koşar ve kuralları `clean.py`'den dosya yoluyla yükler (`QUOTA`, `without_quota`, `spaces`), böylece kural tek yerdedir. Tekrarlanabilir ve idempotenttir: aynı kaynakla ikinci koşu hiçbir şey değiştirmez, `clean.py` de sonrasında değişiklik yapmaz (ikisi de denendi; README). Kurallar ve 2026'daki etkileri:

| # | Kural | Etki |
|---|---|---|
| 1 | NBSP ve fazla boşluk (`clean.spaces`) | 0 ad |
| 2 | Kontenjan ve ücret ekleri `clean.py`'nin `QUOTA` listesiyle birleşir: her (birim, eksiz ad) için tek bölüm, bütün kodlarıyla | 9.254 kayıt → 7.144 program (2.110 varyant) |
| 3 | Birim adları Türkçe büyük-küçük harfle yazılır, Java'nın fakülteleri gibi: "ANKARA HAVACILIK MESLEK YÜKSEKOKULU" → "Ankara Havacılık Meslek Yüksekokulu". "ve", "ile" küçük; kısaltmalar (OSB, BİL, KKTC) kalır; iki ad ve bir sözcük (AIRBUS → Airbus) istisna tablosundadır | 979 birim |
| 4 | Üniversite adı YÖK'ünkidir, sondaki yer parantezi atılır (mevcut 223 satır gibi) | 8 üniversite |
| 5 | Aynı üniversitede aynı adı taşıyan iki YÖK birimi tek fakültedir (İstanbul 29 Mayıs Üniversitesi, "Bilişim Teknolojileri Meslek Yüksekokulu"; YÖK kimlikleri 464891 ve 464892) | 1 birleşme |
| 6 | Lisans programıyla aynı adı taşıyan önlisans programına "(Önlisans)" eklenir (Karar 2) | 2 ad |

**Ek sayılmayanlar** ADR-0037 Karar 4a ile aynıdır: öğretim dili, **uzaktan öğretim ve açıköğretim** ("(Uzaktan Öğretim)" 90, "(Açıköğretim)" 65 kayıt) ve yerleşke ("(Bursa Yerleşkesi)", "(Ankara)") programın parçası kalır. Gerekçe aynıdır: YÖK bunları kendi koduyla ayrı program olarak listeler, öğrenim biçimi işveren için anlamlıdır. 2026 önlisans listesinde ikinci öğretim (İÖ) yok.

`clean.py` iki yerde genişledi. Kural 7 artık (fakülte, **derece**, eksiz ad) üzerinden gruplar; birleşen bölümün kodları kalan bölüme taşınır. Boşluk kuralı adı olmayan kod dosyasını atlar. `from_java.py` Java'nın bölümlerine `BACHELOR` yazar ve başlığı olan boş bir kod dosyası üretir. **Java'nın anlık görüntüsünden** `from_java.py` → `clean.py` → `from_yok.py` → `clean.py` zinciri sürüm 2'nin dosyalarını bayt bayt üretir. Bu yerelde denendi; lisans kısmı sürüm 1 ile satır satır aynıdır.

### 8. Sürüm 2 yalnız ekler

`VERSION` = 2. `published()`'a sürüm 2'nin özeti eklendi, sürüm 1'inki değişmedi. Sürüm 1'den 2'ye eşitleme:

| Tablo | Eklenen | Güncellenen | Emekli |
|---|---|---|---|
| universities | 8 | 0 | 0 |
| faculties | 979 | 0 | 0 |
| departments | 7.144 | 0 | 0 |
| department_source_codes | 9.254 | 0 | 0 |
| countries, cities, districts | 0 | 0 | 0 |

Sonuç: 231 üniversite, 3.022 fakülte (977'si MYO), 14.711 bölüm (7.567 lisans, 7.144 önlisans), 9.254 kod. Emeklilik sınırına hiçbir tablo yaklaşmaz: emekliye ayrılan satır yoktur. `TestSyncFromVersionOne` bunu bağlar: sürüm 1'i tutan bir veritabanı sürüm 2'ye eşitlenir, tablo başına eklenen sayılar tam bu tablodur, güncellenen ve emekliye ayrılan hiç yoktur, ikinci koşu hiçbir şey yazmaz.

### 9. Derece süzgeci ve doğrulaması

Kural tek yerdedir: `reference.ProgramDegree`. `ASSOCIATE` önlisans programını adlandırır. `BACHELOR`, `MASTER` ve `DOCTORATE` lisans programını adlandırır: veride lisansüstü program yok, lisansüstü kayıt alanını lisans programıyla adlandırır.

- **Doğrulama (yetkili olan):** `createMyEducation` ve `replaceMyEducation` bölümü kaydın derecesinin adlandırdığı programlar arasında arar (`GetDepartmentOf`'un `degree` koşulu). Uymayan bölüm `404 REFERENCE_ITEM_NOT_FOUND` alır, **kaydın zaten taşıdığı bölüm olsa da**. Bu, ADR-0036 Karar 6'nın "başka fakültenin bölümü" ile aynı koddur: "bu derecede böyle bir program yok". Yeni bir hata kodu ve sözleşmenin `x-error-codes` değişikliği gerekmedi. Emekli satırın "kaydın taşıdığı kalır" istisnası bir yaşam döngüsü kuralıdır; derece tutarlılığı bir doğruluk kuralıdır ve istisnası yoktur. Bugün uymayan bir kayıt yok: prod yok, önlisans bölümü bu PR'a kadar yoktu. Bölümsüz kayıtta derece birimi bağlamaz; açıköğretim fakülteleri iki derecede de program verir.
- **Süzgeç (istemcinin seçimi için):** üç akademik listeye isteğe bağlı `degree` query parametresi (`EducationDegree`) eklendi. Uyumlu genişlemedir: parametresiz istek eskisi gibidir. Bölüm listesi o derecenin programlarıdır. Fakülte listesi o derecede kullanımda bir programı olan fakültelerdir. Üniversite listesi o derecede programı olan fakültesi bulunan üniversitelerdir (2026: önlisansta 197, lisansta 223). Süzgeç join'dedir: ebeveyni kullanımda olup çocuklarının hepsi öteki derecede olan liste boş gelir, 404 değil. Temsil değişmediği için `ETag` öneki değişmedi: süzülmüş liste aynı sürümün aynı biçimdeki bir seçkisidir.
- Sektörün yaptığı ikisidir: istemci seçimi süzer, sunucu doğrular.

## "Silinirse kırmızı olur mu?"

| Kural | Nerede | Test |
|---|---|---|
| Sayılar (231/3.022/14.711, 7.567+7.144, 9.254 kod) ve sürüm 2 | dosyalar | `referencedata.TestDatasetCounts`, `TestTemplateHoldsTheReferenceData`, `app.TestMigrateCommand` |
| Sürüm 2'nin özeti; sürüm 1'inki aynı | `published()` | `referencedata.TestVersionNamesTheData` |
| Her önlisans programının kodu var, kimliği kodlarından türer; her kod bir önlisans programınındır; varyantlar birleşti | dosyalar | `referencedata.TestAssociatePrograms` |
| Tanıklar: THK Ankara Havacılık MYO / Uçak Teknolojisi (iki kod), Gülhane SMYO / Tıbbi Görüntüleme Teknikleri (Ankara) (kontenjan ekleri birleşti, yerleşke kaldı), İzmir Konak MYO (üniversite satırı), Atatürk Açıköğretim (iki derece, "(Önlisans)") | dosyalar | `referencedata.TestAssociateWitnesses` |
| Kontenjan eki yok, ebeveyn altında tek ad (iki derece birlikte) | dosyalar | `referencedata.TestCleanupHolds` (değişmeden, önlisansı da tarar) |
| Derece `ASSOCIATE`/`BACHELOR`, yalnız bölümde; kod 9 hane, tekil, bölümü dosyada | `Validate` | `referencedata.TestValidate` (her kural ayrı vaka) |
| Bölüm dosyasının `degree` kolonu, kod dosyasının biçimi | `parse`, `parseSourceCodes` | `referencedata.TestParse` |
| Sürüm 1 → 2 yalnız ekler, sayılar tam, ikinci koşu yazmaz | `Sync` | `referencedata.TestSyncFromVersionOne` |
| Derecesi değişen bölüm reddedilir, hiçbir şey yazılmaz (iki yönde) | `plan` | `referencedata.TestSyncRefusesADegreeChange` |
| Başka bölüme taşınan kod reddedilir, hiçbir şey yazılmaz | `planSourceCodes` | `referencedata.TestSyncRefusesAMovedCode` |
| Kodların emeklilik sınırı (462 geçer, 463 durur, yazımdan önce), geri gelen kod etkinleşir | `Sync` adım 5, `applySourceCodes` | `referencedata.TestSourceCodesHoldTheRetireLimit` |
| Bölümlere verilen izin kodların sınırını açmaz | `Sync` adım 5 | `referencedata.TestSyncStopsAtTheRetireLimit` |
| Bölümle düşen kodlar emekliye ayrılır, bölümle geri gelir | `planSourceCodes` | `referencedata.TestSyncFollowsTheFiles` |
| Template adı kod dosyasını kapsar | `dbtest.referenceFS` | `dbtest.TestTemplateNameCoversTheReferenceData` |
| `ProgramDegree`'nin dört derecesi; sabitler dosyalarınkiyle aynı | `reference/domain.go` | `reference.TestProgramDegree` |
| Liste süzgeci, MASTER/DOCTORATE = BACHELOR, boş liste, sözleşme dışı derece 400, süzgecin getirdiği üç `retired_at` koşulu ayrı ayrı | `reference.sql` | `app.TestReferenceListsFilterByDegree` |
| Süzülmüş listenin `ETag`'i ve 304'ü | `reference/http.go` | `app.TestReferenceListsAreCacheable` |
| Derece ile bölüm tutarlılığı, oluşturmada ve değiştirmede, kaydın taşıdığı bölümde de | `GetDepartmentOf` | `app.TestEducationDegreeNamesItsProgram` |
| Kod tablosunun yetkisi ve katalog sınıfı | `0048`, `softdelete.yaml` | `migrations.TestRoleGrants`, `TestCatalogMatchesDatabase` |
| Kod tablosunun listesi yok, öteki her tablonun bir listesi var | sözleşme | `app.TestReferenceHasNoWriteOperation` |

**Denenmedi:**
- Testlerin hiçbiri bu Mac'te koşturulmadı (CLAUDE.md); kanıt CI'dadır.
- Kuralları tek tek silip testin kırmızıya döndüğü denenmedi; tablo testin neyi bağladığını yazar.
- `koala reference sync --dry-run` yerelde koşturulmadı (veritabanı yok). Karar 8'in tablosu `from_yok.py`'nin raporundan gelir, `TestSyncFromVersionOne` aynı sayıları CI'da bağlar.
- `0048`'in varsayılanının (`BACHELOR`) sürüm 1'i tutan **gerçek** bir veritabanında koştuğu denenmedi. `TestSyncFromVersionOne`'ın sürüm 1 veritabanı, template'ten sürüm 2'nin satırları silinerek kurulur; `TestMigrateCommand` bütün migration'ları boş veritabanında koşar.
- Python betikleri (`from_yok.py`, `clean.py`) CI'da koşmaz. Yerelde iki kez koşturuldu ve ikincisi değişiklik yapmadı.

## Yerine geçtiği

1. **ADR-0037 Karar 4'ün "Yeni satırın kimliği UUIDv7'dir; kimlik bir doğal anahtardan türetilmez" maddesi**, YÖK'ten gelen satırlar için → Karar 6 (UUIDv5, YÖK kodundan). Elle eklenen satırlar UUIDv7 kalır. README'nin "Yeni satırın kimliği UUIDv7'dir" cümlesi buna göre değişti.
2. **ADR-0037 Karar 8** ("meslek yüksekokulları bu PR'da yok … yeni satırlar, UUIDv7 kimlikler") ve **açık soru 1** → bu ADR. Açık soru 3 (`yok_code`) önlisans için Karar 5 ile kapanır; lisans bölümlerinin kodu açık soru 1'dedir.
3. **ADR-0037 *"Silinirse kırmızı"* tablosundaki "Referans uçları … tabloya bağlı" satırı**: `department_source_codes` bir referans tablosudur ama listesi yoktur (`TestReferenceHasNoWriteOperation` onu ayrık tutar).

ADR-0036 Karar 6'nın tek kod kararı değişmiyor, genişliyor: derecesine uymayan bölüm de `REFERENCE_ITEM_NOT_FOUND` alır.

## Sonuçlar

- `ASSOCIATE` kaydı artık bir MYO'yu ve programını adlandırabilir. İstemci derece seçimini `degree` ile listelere geçirir.
- **Yıllık güncelleme** (README): ham yanıt yeniden çekilir, `from_yok.py` → `clean.py` koşar. Yeni ya da adı değişen kurum betiği durdurur ve `tools/yok_universities.csv`'de karara bağlanır. Programlar kodlarıyla eşleşir; kapanan program ve kodları dosyadan çıkar ve emekliye ayrılır; yeni program UUIDv5 alır.
- 2027 kılavuzunda bir programın ÖSYM kodu değişirse (kod yeniden kullanılırsa) iki durum vardır: eski kod başka programa geçmişse eşitleme onu reddeder (Karar 5); kod yalnız değişmişse program önce adıyla bulunur. İkisi de sessiz değildir.

## Açık sorular

1. **Lisans verisi YÖK'ten yenilenmeli:** beş üniversitenin yeni adı, dört yeni üniversitenin lisans fakülteleri ve lisans bölümlerinin ÖSYM kodları aynı uçtan (`birimTuruId` 46) gelir. Kimlikleri koruyan bir eşleştirme ve bir ad değişikliği sürümü ister. Ayrı PR; ürün kararı (ad değişikliği eşitlemede `UPDATED` görünür, kayıtlar yeni adı görür).
2. **Kılavuzun sınırı:** o yıl öğrenci almayan programlar listede yok. Eski mezunların programı için YÖK'ün başka bir listesi (ör. `istatistik.yok.gov.tr`) gerekir mi? Kaynak araştırması gerekir.
3. **Lisans:** ADR-0037 açık soru 2 ile aynı; YÖK Atlas verisinin açık bir lisansı doğrulanamadı.
