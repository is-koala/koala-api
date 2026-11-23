# ADR-0038: Önlisans programları, bölümün derecesi, kaynak kodları ve kimlik defteri

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, S4
- Kaynak: [ADR-0012](0012-data-access-and-migrations.md) Karar 5 (UUIDv7), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 6 (`EducationDegree`, `ASSOCIATE`, tek kod `REFERENCE_ITEM_NOT_FOUND`), [ADR-0037](0037-reference-data-sync-and-lists.md) Karar 3, 3a, 3b, 4, 4a, 8 ve açık sorular 1 ve 3; Yusuf'un 23.11.2026 onayı (önlisans verisi YÖK'ün listesinden, lisansı doğrulanamadığı bilinerek); koordinasyonun 23.11.2026 kararları (PR #31'in iki incelemesi, FIX-FIRST: kimlik defteri; lisansüstü, bölümsüz önlisans ve eski kayıt ürün kararları)
- Dış kaynaklar: YÖK Atlas (`yokatlas.yok.gov.tr`), 2026 ÖSYM tercih kılavuzunun önlisans programları; RFC 9562 §5.7 (UUIDv7); sektörün iki kalıbı: dış kimlik eşleme tablosu (bir kaynağın birden çok kodunu tek satıra bağlayan ayrı tablo) ve yalnız eklenen kimlik defteri (kaynak anahtarını kalıcı iç kimliğe bağlayan, değiştirilmeyen kayıt; ana veri yönetiminde "cross-reference" ya da "key map")
- Kod: `db/referencedata` (`departments.csv`'nin `degree` kolonu, `department_source_codes.csv`, `VERSION` = 2, `referencedata.go`, `sync.go`, `tools/from_yok.py`, `tools/id_ledger.csv`, `tools/id_decisions.csv`, `tools/yok_universities.csv`, `tools/yok_units.csv`, `tools/clean.py`, `tools/from_java.py`), `db/migrations/0048_reference_degrees_and_source_codes.sql`, `db/queries/reference/reference.sql`, `db/queries/users/educations.sql`, `internal/reference` (`ProgramDegree`, liste süzgeci), `internal/users/educations.go`, `internal/app/refdata.go`, `api/openapi.yaml`, `db/softdelete.yaml`

## Bağlam

ADR-0037'nin verisi YÖK'ün **lisans** listesiydi (Java'nın anlık görüntüsü): 223 üniversite, 2.043 fakülte, 7.567 bölüm; meslek yüksekokulu yoktu. `EducationDegree.ASSOCIATE` yalnız üniversite düzeyinde seçilebiliyordu (Karar 8). Yusuf önlisans verisinin YÖK'ün listesinden eklenmesini onayladı (23.11.2026).

İki şey daha bu PR'ı bekliyordu: ADR-0037 açık soru 3 (yıllık eşleştirme ad ile yapılıyor, kararlı değil; bir `yok_code` önerildi) ve eğitim kaydında derece ile bölümün tutarlılığı (lisans bölümü + `ASSOCIATE`).

PR #31'in ilk hâli yeni satırlara YÖK kodundan türeyen UUIDv5 kimlikler veriyordu. İki inceleme bunu reddetti (FIX-FIRST). Kimlik koddan türeyince ÖSYM'nin bir kodu aynı birimde başka bir programa yeniden vermesi, yeni programa eski programın kimliğini verirdi. Eşitleme bunu sessizce "ad değişikliği" sayar, eski programı adlandıran kayıtlar yeni adı gösterirdi; kimlik ADR-0037 Karar 3'ün taşıma reddini böylece atlatırdı. Kimlikler henüz hiçbir veritabanında değildi; düzeltmenin en ucuz anı oydu. Karar 6 bu yüzden bir kimlik defteridir.

## Karar

### 1. Kaynak: YÖK Atlas'ın kılavuz listesi, tek istekle

`yokatlas.yok.gov.tr` 2025'te tek sayfalık bir uygulamaya dönüştü; eski `onlisans-univ.php` sayfaları yok. Sayfanın kendi kullandığı açık uç `POST /api/tercih-kilavuz/search` (kimlik istemez) yılın ÖSYM kılavuzundaki programları döner. `{"filters":{"birimTuruId":47},"size":10000}` (47 = "ÖNLİSANS") **2026 kılavuzunun 9.254 önlisans kaydını tek yanıtta** verdi. Her kayıt ÖSYM program kodunu (`kilavuzKodu`, 9 hane), YÖK'ün üniversite ve birim kimliklerini (`universiteId`, `fymkId`) ve adlarını taşır.

- Çekim 23.11.2026, toplam 7 istek (README'de URL'leriyle). `robots.txt` bir yönerge taşımıyor (sunucu yerine bir kaynak dosyası dönüyor); istekler arasında beklendi, paralel istek yok. Ham yanıt depoya girmez; depoya yalnız dönüştürülmüş CSV'ler ve kimlik defteri girer.
- **Sınır:** kılavuz yalnız o yıl öğrenci alan programları listeler. O yıl öğrenci almayan ya da kapanmış bir programın mezunu programını bulamaz. Lisans verisi (Java) için de aynı durum geçerliydi.
- **Lisans:** ADR-0037 açık soru 2 gibi doğrulanamadı; Yusuf bilerek onayladı.

### 2. Veri modeli: birim fakülte, program bölüm, derece programın

- Meslek yüksekokulu (ve önlisans programı olan başka birim) bir **`faculties`** satırıdır, üniversitesinin altında. Önlisans programları o birimin **`departments`** satırlarıdır. Şema, uçlar ve eğitim kaydının bileşik FK'leri değişmeden kalır.
- **Derece programın özelliğidir, birimin değil:** `departments.degree` (`ASSOCIATE` | `BACHELOR`, `EducationDegree`'nin değerleri). Birime tür kolonu konmadı: altı birim iki derecede de program verir (Anadolu, Atatürk, İstanbul ve Ankara'nın açık ve uzaktan öğretim fakülteleri, DAÜ'nün iki yüksekokulu); birim düzeyindeki bir tür onları ya iki satıra bölerdi ya yanlış söylerdi. YÖK de dereceyi programda (`birimTuruId`) tutar.
- `0048` mevcut satırlara `BACHELOR` verir (ADR-0037'nin verisi lisans listesidir), sonra varsayılanı kaldırır: her satırın derecesini eşitleme yazar.
- **Derece kimliğin parçasıdır.** Bir önlisans programı lisans programına dönüşmez; dönüşen YÖK satırı yeni bir programdır. Eşitleme, veritabanındaki bir bölümün derecesini değiştiren dosyayı taşınan satır gibi **reddeder** (ADR-0037 Karar 3); emekli bölüm başka bir dereceyle geri gelirse de: derece denetimi geri getirmeden önce gelir. Eğitim kaydı dereceyi tuttuğu için sessiz bir değişiklik kayıtları tutarsız bırakırdı.
- Aynı birimde aynı adı taşıyan lisans ve önlisans programı iki programdır. Önlisansınki YÖK'ün kendi yazımıyla ayrılır: "(Önlisans)" program adından sonra, öteki parantezlerden önce ("Halkla İlişkiler ve Tanıtım (Önlisans) (Açıköğretim)"; YÖK'ün listesinde birebir "İlahiyat (Önlisans) (Açıköğretim)" adlı bir program var). 2026'da iki ad (Atatürk Açıköğretim). "Aynı ebeveyn altında tek ad" temizlik kuralı (ADR-0037 Karar 4a, 5–7) böylece iki derece için de geçerli kalır.

### 3. Üniversiteye bağlı olmayan vakıf meslek yüksekokulları

Dört vakıf MYO bir üniversiteye bağlı değildir; YÖK onları kendi `universiteId`'leriyle listeler: Ataşehir Adıgüzel, İstanbul Sağlık ve Sosyal Bilimler, İstanbul Şişli ve İzmir Konak MYO'ları. Her biri **bir `universities` satırı ve altında kendi adını taşıyan tek bir birimdir**.

- Ayrı bir tür (tablo ya da kolon) seçilmedi. Eğitim kaydı zorunlu bir `universityId` ile bir **yükseköğretim kurumuna** bağlanır (ADR-0036 Karar 6). Bu MYO'lar YÖK'ün hiyerarşisinde üniversiteyle aynı düzeydedir. Ayrı bir tür sözleşmeyi (`universityId` zorunlu) ve bileşik FK'yi değiştirirdi; istemci de aynı akışla seçer.
- Satırın adı YÖK'ün adıdır ("İZMİR KONAK MESLEK YÜKSEKOKULU"); listede bir üniversite olmadığı adından okunur.

### 4. Üniversite eşleştirmesi: açık bir tablo, bulanık eşleştirme yok

Defterde (Karar 6) olmayan bir YÖK kurumu `tools/yok_universities.csv`'de tek tek karara bağlanır: YÖK kimliği, YÖK adı, bizim kimliğimiz ya da `new`, dayanak. Tabloda olmayan ya da adı tablodakinden farklı gelen kurum `from_yok.py`'yi durdurur: yeni kurum ve ad değişikliği bir insan kararıdır. 2026'nın 197 kurumu:

| Dayanak | Sayı | Not |
|---|---|---|
| `name` (ad aynı) | 105 | |
| `name without place` (YÖK'ün sondaki yer parantezi atılınca aynı: "TÜRK HAVA KURUMU ÜNİVERSİTESİ (ANKARA)") | 79 | |
| `renamed` | 5 | Alanya Hamdullah Emin Paşa → Alanya Üniversitesi; Antalya AKEV → Antalya Belek; Beykent → İstanbul Beykent; Nişantaşı → İstanbul Nişantaşı; İstanbul Ayvansaray → İstanbul Topkapı. Dayanak: YÖK'ün 2026 lisans listesindeki fakülte kümeleri bizim satırların fakülteleriyle örtüşüyor (tek bir ek istekle karşılaştırıldı) |
| `new` | 8 | 4 bağımsız vakıf MYO (Karar 3); Ankara Müzik ve Güzel Sanatlar, Sivas Bilim ve Teknoloji, Mudanya, Kıbrıs Aydın üniversiteleri (Java'nın lisans listesinde yoklar) |

**Ad değişiklikleri bu sürümde uygulanmadı.** Beş satır eski adlarıyla kalır, önlisans birimleri o kimliklerin altına girer. Sürüm 2 yalnız ekleme yapar (Karar 8). Yeni adlar lisans verisinin YÖK'ten yenilenmesinin işidir (açık soru 1). Dört yeni üniversitenin lisans fakülteleri de o işe kalır; bugün bu üniversitelerin yalnız önlisans birimleri var.

Birimler de aynı ilkeyle eşleşir. Önlisans programı olan MYO dışı dokuz birimden yedisi zaten `faculties.csv`'deydi; bunlar `tools/yok_units.csv`'de açıkça bağlanır. Beşi ad aynı olduğu için eşleşti. İkisi yeniden adlandırılmış birimdir: Atatürk'ün "Açık ve Uzaktan Öğretim Fakültesi" bizim "Açıköğretim Fakültesi" satırımızdır; ASBÜ'nün "Kuzey Kıbrıs Yerleşkesi" bizim bölümsüz "KKTC Akademik Birim (Lefkoşa)" satırımızdır, yalnız önlisans verir ve artık bölümleri var. Kalan iki birim (DAÜ Sağlık Hizmetleri YO, Anadolu Engelliler Entegre YO) yeni satırdır.

### 5. Kaynak kodları: ayrı bir eşleme tablosu, satırı (kod, bölüm) çifti

`department_source_codes (code, department_id, retired_at)`, birincil anahtar `(code, department_id)`; kod ÖSYM program kodudur (YÖK Atlas'ın "program kodu", `^[0-9]{9}$`). Satırlar `department_source_codes.csv`'den gelir; dosyada bir kod bir kez geçer.

- **Neden ayrı tablo:** kontenjan varyantları (Burslu, %50 İndirimli, Ücretli, KKTC Uyruklu…) YÖK'te ayrı kodlardır. Temizlik onları tek bölümde birleştirir (Karar 7), ama programı ertesi yıl bulmak için **her kodun** bilinmesi gerekir: düz varyant kapanıp yalnız "Burslu"su kalırsa program yine bulunmalı. `departments`'a tek bir `yok_code` kolonu bunu taşıyamazdı.
- **Neden çift:** ÖSYM bir kodu başka bir programa verebilir. Kod anahtar olsaydı bu iki yoldan biriyle biterdi: ya kodun satırı başka bölüme çevrilirdi (geçmiş kaybolur), ya eşitleme onu reddederdi ve karar verilmiş bir yeniden kullanmanın çıkışı olmazdı. Çift hiç değişmez. Bir kodun başka bir programa verilmesi (Karar 6'nın `new` kararı) eski çiftin emekliye ayrılması ve yenisinin eklenmesidir. Bu tek eşitlemede olur ve kuru koşuda iki satır olarak görünür. **Bir kodun kullanımda en çok bir çifti olur**: kısmi tekil indeks (`code WHERE retired_at IS NULL`) bunu veritabanında da tutar; yazım önce emekliye ayırır, sonra ekler.
- **Sessiz devralma burada değil defterde engellenir:** dosyalara bir kodu başka bölümle yazan yalnız `from_yok.py`'dir ve bunu yalnız açık bir kararla yapar (Karar 6). Bölüm kimliği koddan türemediği için yeniden kullanılan kod eski programın kimliğini taşımaz; eşitleme bir bölümü başka bir programa dönüştüremez.
- **Lisans bölümlerinin kodu yok.** Java'nın verisinde YÖK kodu yoktu; 7.567 lisans bölümü için satır yok (kolonun "nullable" karşılığı budur). Lisans verisi YÖK'ten yenilendiğinde dolar (açık soru 1).
- **Eşitlemenin kuralları kodlara da uygulanır:**
  - Dosyadan düşen çift silinmez, emekliye ayrılır; geri gelen çift yeniden kullanıma girer.
  - Emeklilik sınırı (ADR-0037 Karar 3a) bu tabloya da ayrıca uygulanır: kullanımdaki 9.254 çiftin %5'i 462'dir. Taban yalnız kullanımdaki çiftlerdir; emekli çiftler sınırı gevşetmez. Bölümlere verilen izin kodların sınırını açmaz. Bölümleri kalan ama kod dosyası kesilmiş bir veri de durur.
  - Kodlar bölümlerden sonra planlanır ve yazılır; doğrulama her kodun bölümünün dosyada olmasını ister. Bölüm düşünce kodları da düşmek zorundadır, ebeveyn kuralı gibi.
  - Plan ve yazım genel tablo yolunun aynı adımlarıdır (`planSourceCodes`, `applySourceCodes`). Tablo kimliğe değil çifte göre anahtarlandığı için `tables()`'ta değil, `Tables()`'tadır: izin (`--allow-retire=department_source_codes:<n>@<sürüm>`), sayım ve özet onu da kapsar.
- Uygulamaya `SELECT` verildi (öteki referans tabloları gibi), ama listesi yoktur: kodlar sunulmaz.

### 6. Kimlik defteri: yalnız eklenir, kimlik dış koddan türemez

`tools/id_ledger.csv` (karar koordinasyonun, 23.11.2026). YÖK'ten gelen her kaynak anahtarı için bir satır tutar: anahtar, kimlik, ebeveyn, ad, satırı ekleyen veri sürümü, köken ve karar (`key,id,parent,name,version,origin,decision`). Anahtarlar `yok:university:<universiteId>`, `yok:unit:<fymkId>` ve `osym:<kod>` biçimindedir. Köken üç değer alır:
- `minted`: kimlik bu anahtar için üretildi;
- `matched`: anahtar var olan bir satıra bağlandı (Java'nın 189 üniversitesi, 7 birimi) ya da düşmüş bir kimlik dosyaya geri döndü;
- `dropped`: kimlik dosyadan çıktı.

Bir anahtarın birden çok satırı olabilir; **anahtarın son satırı** bugünkü anlamıdır. Bir anahtarın ilk satırından sonraki her satır üç şeyden biridir: bir karar (`decision` kolonu `same` ya da `new`), bir düşme ya da düşmüş kimliğin geri dönüşü. Kararsız başka bir değişiklik `TestIDLedger`'da kırmızıdır.

- **Yeni kimlik bir UUIDv7'dir** (ADR-0012 Karar 5) ve dış bir koddan türemez. Python'un `uuid.uuid7`'si 3.14'ündür; betik kendi üreticisini taşır (RFC 9562 §5.7).
- **Defterdeki anahtar defterdeki kimliği alır**, yeter ki aynı ebeveynin altında ve aynı adla gelsin. Adlar yalnız büyük-küçük harf ve fazla boşluk gözetilmeden karşılaştırılır. **Şapka sayılır** ("kar" ile "kâr" iki ayrı sözcüktür): şapka kazanan ya da yitiren bir ad betiği durdurur ve karar ister (tutucu taraf, koordinasyonun tercihi). Bunun bedeli, YÖK'ün yalnız yazımı değiştirdiği bir adda da (ör. "Yapay Zeka" → "Yapay Zekâ") bir `same` kararı gerekmesidir.
- **Bir programın kodlarından herhangi biri defterdeyse program o kimliği taşır.** İlk görüldüğü kod kapanan program kimliğini korur. Bir programın kodları defterde iki ayrı kimliği gösterirse betik durur.
- **Yeniden kullanılan ya da taşınan kodda betik durur.** Defterdeki bir anahtar başka bir adla ya da başka bir ebeveynin altında gelirse `from_yok.py` hiçbir şey yazmadan durur. Mesaj anahtarları adlandırır ve her biri için doldurulacak karar satırını basar. Karar `tools/id_decisions.csv`'ye yazılır:
  - `same`: aynı program, yeni adıyla, aynı ebeveynde. Kimliği korur.
  - `new`: başka bir program, yeni kimlik.

  Başka ebeveyne geçmiş bir anahtar `same` olamaz (taşınan satır yeni satırdır, ADR-0037 Karar 3); betik bunu da durdurur. Karar, kararın verildiği durumla birlikte deftere bir satır olarak geçer.
- **Karar verildiği durumu taşır** (doğrulamanın bulduğu bayat karar açığına karşı). Dosyanın kolonları `key,version,decision,ledger_parent,ledger_name,source_parent,source_name,note`'tur:
  - `version`: kararın verildiği veri sürümü;
  - `ledger_parent`, `ledger_name`: defterin o anahtar için gösterdiği ebeveyn kimliği ve ad;
  - `source_parent`, `source_name`: kaynağın verdiği ebeveyn (kaynak anahtarıyla, ör. `yok:unit:11`; yeni bir birimin kimliği koşudan önce bilinmez) ve ad.

  Betik bir kararı yalnız **şu anki `VERSION`'da** ve yalnız defterin ve kaynağın durumunu **birebir** tutuyorsa uygular. Başka bir sürümün kararı, tuttuğu durumu bulamayan bir karar ve karar verilecek bir şey yokken duran bir karar betiği durdurur. Böylece sürüm 5'te verilip dosyada unutulmuş bir `same`, sürüm 6'da ÖSYM'nin aynı kodu başka bir programa vermesini sessizce geçiremez. Kararın kaydı defterdeki satırdır; dosya yalnız güncel sürümün kararlarını tutar (`TestIDDecisions`) ve sonraki `VERSION`'la boşaltılır. Bir koda `new` denip program kimliğini başka bir kodu üzerinden yine eski kimlikle alırsa betik durur.
- **Dosyadan çıkan birim ve üniversite deftere yazılır.** Programı kalmayan bir birim (MYO kapanmaları yaygındır) ya da birimi kalmayan bir üniversite dosyadan çıkar ve anahtarları bir `dropped` satırı alır. `TestIDLedger` her birim ve üniversite anahtarının ya dosyada olmasını ya da defterde düşmüş görünmesini ister; düşmüş görünüp dosyada duran kimlik de kırmızıdır. Birim geri gelirse aynı kimliği alır ve bir `matched` satırı yazılır. Düşmeyi yazmak, denetimi gevşetmekten kalıcıdır: defter neyin neden dosyada olmadığını kendisi söyler.
- **Defterde olmayan anahtar** için sıra şöyledir:
  - üniversite yalnız `tools/yok_universities.csv` ile eşleşir (Karar 4);
  - birim önce `tools/yok_units.csv` ile, sonra aynı üniversitede aynı adla eşleşir;
  - program aynı birimde aynı ad ve dereceyle eşleşir (README'nin yıllık güncelleme adım 1'i);
  - hiçbiri tutmazsa yeni kimlik alır.
- **Defter yalnız eklenir.** `TestIDLedgerIsAppendOnly`, `publishedLedger()`'daki her yayımlanmış sürüm için defterin ilk satırlarının özetini tutar. Yayımlanmış bir satırı değiştiren ya da silen ve `VERSION`'ı artırmadan satır ekleyen PR kırmızıdır. `TestIDLedger` dosyaları deftere bağlar: her kodun son satırı kendi bölümünü kendi fakültesinin altında gösterir; her `minted` kimlik UUIDv7'dir; bir anahtarın geçmişi yalnız kararla, düşmeyle ya da geri dönüşle değişir. `TestIDDecisions` karar dosyasını deftere bağlar: dosyadaki her karar, bu sürümün aynı anahtar ve kararla yazılmış bir defter satırıdır ve kararın adlandırdığı durum o satırın öncesi ve kendisidir; bu sürümün kararla yazılmış her satırının karşısında dosyada bir karar vardır.
- **Betiğin kendi testi:** `from_yok.py --self-test` uydurma veriyle geçici bir dizinde koşar ve şunları dener:
  - ilk görüldüğü kodu kapanan program kimliğini korur;
  - yeniden kullanılan kod ve başka birime taşınan kod betiği durdurur, duran koşu hiçbir şey yazmaz;
  - `new` yeni kimlik verir; `same` adı değişeni korur, taşınanı korumaz;
  - karar deftere geçer, aynı kararla ikinci koşu hiçbir şey değiştirmez;
  - **bayat karar:** önceki sürümün dosyada kalan kararı (doğrulamanın senaryosu: sürüm 5'te `same`, sürüm 6'da kod başka programa gider), başka bir durum için verilmiş karar ve karar verilecek bir şey yokken duran karar betiği durdurur;
  - **iki kimlik:** kodları defterde iki ayrı kimliği gösteren program betiği durdurur;
  - **şapka** betiği durdurur, büyük-küçük harf ve boşluk durdurmaz;
  - **birimin çıkışı:** programı kalmayan birim dosyadan çıkar, defterde `dropped` görünür, geri gelince aynı kimliği alır;
  - defter yalnız büyür.

  CI Python betiklerini koşmuyordu. Verify job'unda `python3` vardır (`make generate` `pairs.py`'yi onunla koşar); bu yüzden `TestFromYokSelfTest` öz testi bir Go birim testi olarak koşturur.
- **Kimlikler yeniden üretildi.** PR'ın ilk hâlindeki UUIDv5'ler hiçbir veritabanında değildi. Defter boş başlatıldı ve bütün yeni satırlar UUIDv7 aldı. Defterle birlikte, Java'nın anlık görüntüsünden ve ham yanıttan dosyalar bayt bayt yeniden üretilir; defter olmadan yeni kimlikler üretilir. Kimliğin kalıcılığı türetmeden değil defterden gelir.

### 7. Temizlik

`tools/from_yok.py` yalnız standart kütüphaneyle, `python3 -I` ile koşar ve kuralları `clean.py`'den dosya yoluyla yükler (`QUOTA`, `without_quota`, `spaces`); böylece kural tek yerdedir. Tekrarlanabilir ve idempotenttir: aynı kaynakla ikinci koşu hiçbir şey değiştirmez ve deftere satır eklemez; sonrasında `clean.py` de değişiklik yapmaz (yerelde denendi; README). Kurallar ve 2026'daki etkileri:

| # | Kural | Etki |
|---|---|---|
| 1 | NBSP ve fazla boşluk (`clean.spaces`) | 0 ad |
| 2 | Kontenjan ve ücret ekleri `clean.py`'nin `QUOTA` listesiyle birleşir: her (birim, eksiz ad) için tek bölüm, bütün kodlarıyla | 9.254 kayıt → 7.144 program (2.110 varyant) |
| 3 | Birim adları Türkçe büyük-küçük harfle yazılır, Java'nın fakülteleri gibi: "ANKARA HAVACILIK MESLEK YÜKSEKOKULU" → "Ankara Havacılık Meslek Yüksekokulu". "ve", "ile" küçük; kısaltmalar (OSB, BİL, KKTC) kalır; iki ad ve bir sözcük (AIRBUS → Airbus) istisna tablosundadır | 979 birim |
| 4 | Üniversite adı YÖK'ünkidir, sondaki yer parantezi atılır (mevcut 223 satır gibi) | 8 üniversite |
| 5 | Aynı üniversitede aynı adı taşıyan iki YÖK birimi tek fakültedir (İstanbul 29 Mayıs Üniversitesi, "Bilişim Teknolojileri Meslek Yüksekokulu"; YÖK kimlikleri 464891 ve 464892); defterde iki anahtar aynı kimliği gösterir | 1 birleşme |
| 6 | Lisans programıyla aynı adı taşıyan önlisans programına "(Önlisans)" eklenir (Karar 2) | 2 ad |

**Ek sayılmayanlar** ADR-0037 Karar 4a ile aynıdır: öğretim dili, **uzaktan öğretim ve açıköğretim** ("(Uzaktan Öğretim)" 90, "(Açıköğretim)" 65 kayıt) ve yerleşke ("(Bursa Yerleşkesi)", "(Ankara)") programın parçası kalır. Gerekçe aynıdır: YÖK bunları kendi koduyla ayrı program olarak listeler, öğrenim biçimi işveren için anlamlıdır. 2026 önlisans listesinde ikinci öğretim (İÖ) yok.

`clean.py` iki yerde genişledi. Kural 7 artık (fakülte, **derece**, eksiz ad) üzerinden gruplar; birleşen bölümün kodları kalan bölüme taşınır. Boşluk kuralı adı olmayan kod dosyasını atlar. `from_java.py` Java'nın bölümlerine `BACHELOR` yazar ve başlığı olan boş bir kod dosyası üretir. Lisans kısmı sürüm 1 ile satır satır aynıdır.

### 8. Sürüm 2 yalnız ekler

`VERSION` = 2. `published()`'a sürüm 2'nin özeti, `publishedLedger()`'a defterin 10.438 satırı eklendi; sürüm 1'in özeti değişmedi. Sürüm 1'den 2'ye eşitleme:

| Tablo | Eklenen | Güncellenen | Emekli |
|---|---|---|---|
| universities | 8 | 0 | 0 |
| faculties | 979 | 0 | 0 |
| departments | 7.144 | 0 | 0 |
| department_source_codes | 9.254 | 0 | 0 |
| countries, cities, districts | 0 | 0 | 0 |

Sonuç: 231 üniversite, 3.022 fakülte (977'si MYO), 14.711 bölüm (7.567 lisans, 7.144 önlisans), 9.254 kod.

Defterde 10.438 satır var:
- 197 üniversite anahtarı (189 `matched`, 8 `minted`),
- 987 birim anahtarı (7 `matched`, 980 `minted`; ikisi aynı kimliği gösterir),
- 9.254 kod anahtarı (hepsi `minted`, 7.144 kimlik).

Emeklilik sınırına hiçbir tablo yaklaşmaz: emekliye ayrılan satır yoktur. İki test bunu bağlar:
- `TestSyncFromVersionOne`: sürüm 1'i tutan bir veritabanı sürüm 2'ye eşitlenir. Tablo başına eklenen sayılar tam bu tablodur, güncellenen ve emekliye ayrılan hiç yoktur, ikinci koşu hiçbir şey yazmaz.
- `TestMigrationGivesVersionOneItsDegree` (Karar 10): aynısını migration'larla kurulmuş bir sürüm 1 veritabanında doğrular.

### 9. Derece süzgeci ve doğrulaması

Kural tek yerdedir: `reference.ProgramDegree`.
- `ASSOCIATE` önlisans programını adlandırır.
- `BACHELOR`, `MASTER` ve `DOCTORATE` lisans programını adlandırır.
- Bilmediği bir derece hiçbir programı adlandırmaz (`""`). Sözleşmeye eklenen bir değer burada karar verilene kadar hiçbir bölümle eşleşmez. `TestProgramDegree` sözleşmedeki `EducationDegree` enum'unu okur, yeni değer orada kırmızıdır.

**Doğrulama (yetkili olan):**
- `createMyEducation` ve `replaceMyEducation` bölümü, kaydın derecesinin adlandırdığı programlar arasında arar (`GetDepartmentOf`'un `degree` koşulu).
- Uymayan bölüm `404 REFERENCE_ITEM_NOT_FOUND` alır, **kaydın zaten taşıdığı bölüm olsa da**. Bu, ADR-0036 Karar 6'nın "başka fakültenin bölümü" ile aynı koddur: "bu derecede böyle bir program yok". Yeni bir hata kodu ve sözleşmenin `x-error-codes` değişikliği gerekmedi.
- Emekli satırın "kaydın taşıdığı kalır" istisnası bir yaşam döngüsü kuralıdır. Derece tutarlılığı bir doğruluk kuralıdır ve istisnası yoktur.

**Süzgeç (istemcinin seçimi için):**
- Üç akademik listeye isteğe bağlı `degree` query parametresi (`EducationDegree`) eklendi. Uyumlu genişlemedir: parametresiz istek eskisi gibidir.
- Bölüm listesi o derecenin programlarıdır. Fakülte listesi o derecede kullanımda bir programı olan fakültelerdir. Üniversite listesi o derecede programı olan fakültesi bulunan üniversitelerdir (2026: önlisansta 197, lisansta 223).
- Süzgeç join'dedir: ebeveyni kullanımda olup çocuklarının hepsi öteki derecede olan liste boş gelir, 404 değil.
- Temsil değişmediği için `ETag` öneki değişmedi: süzülmüş liste aynı sürümün aynı biçimdeki bir seçkisidir.

Sektörün yaptığı ikisidir: istemci seçimi süzer, sunucu doğrular.

**Ürün kararları (koordinasyon, 23.11.2026):**
- **Lisansüstü kayıtlar:** `MASTER` ve `DOCTORATE` kayıtlarının bir lisans programıyla adlandırılması kabul edildi. YÖK'ün lisansüstü program verisi yok; ileride eklenebilir. O gün `ProgramDegree` ve `TestProgramDegree` birlikte değişir.
- **Bölümsüz `ASSOCIATE` kaydı:** fakülte ve üniversite dereceyle denetlenmez; kabul edildi. Sektörün pratiği eğitim kaydında kurumu serbest bırakmaktır; açıköğretim fakülteleri iki derecede de program verir.
- **Önceki tutarsız kayıtlar:** bu kuraldan önce yazılmış ve derecesine uymayan bir bölümü taşıyan kayıt, değiştirilmek istendiğinde bölümünü değiştirmek zorundadır ("kilitli" kalır). Prod boş, böyle bir kayıt yok; kural yazıldığı gibi kalır.
- **Lisans verisinin YÖK'ten yenilenmesi** ayrı bir PR'dır (açık soru 1).

### 10. `0048`'in geri alınması

Down bir geliştirme aracıdır (`koala migrate` onu prod'da reddeder).

**Sorun:** Derece kolonu olmadan önlisans programları lisans programlarından ayırt edilemez. Yerlerinde bırakılsalar bir sonraki up onları `BACHELOR` yapardı. Ardından gelen eşitleme her birini "derecesi değişmiş" sayıp reddederdi; down ve up döngüsü eşitlemeyi kilitlerdi.

**Seçim:** Down önlisans programlarını ve kod tablosunu siler; kalan fakülteler, üniversiteler ve veri sürümü kalır. Bir eğitim kaydı bir önlisans programını adlandırıyorsa down açık bir mesajla durur ("delete those records first"); kaydı silmek geliştiricinin kararıdır.

**Gerekçe:**
- Sürüm kalır, çünkü sonraki up'ın eşitlemesi silinen satırları aynı kimliklerle geri yazar; bu, aynı sürümde bir sapmanın onarımıdır.
- Eski bir ikili ise daha yeni bir sürümü olduğu gibi bırakır (ADR-0037 Karar 3b).
- Sürüm 1'e çekmek eski ikilinin 979 fakülteyi emekliye ayırmaya kalkmasına ve sınıra takılmasına yol açardı.
- "Önlisans satırı varken hata ver" seçeneği seçilmedi: `TestMigrateCommand`'ın bütün migration'ları geri alan döngüsü her template veritabanında önlisans satırı bulur ve down hiç geçemezdi.

`TestMigrationGivesVersionOneItsDegree` gerçek yolu dener:
- Migration'lar `0047`'ye kadar uygulanır ve sürüm 1'in satırları o şemanın aldığı biçimde yazılır.
- `0048` uygulanır: 7.567 bölümün hepsi `BACHELOR` olur, sürüm 1'in kendi eşitlemesi hiçbir şey bulmaz, sürüm 2 yalnız ekler.
- Down önlisansı siler ve sürümü tutar; up ve eşitleme önlisansı geri getirir.
- Önlisans programını adlandıran bir kayıtla down durur ve hiçbir şey değişmez.

## "Silinirse kırmızı olur mu?"

| Kural | Nerede | Test |
|---|---|---|
| Sayılar (231/3.022/14.711, 7.567+7.144, 9.254 kod) ve sürüm 2 | dosyalar | `referencedata.TestDatasetCounts`, `TestTemplateHoldsTheReferenceData`, `app.TestMigrateCommand` |
| Sürüm 2'nin özeti; sürüm 1'inki aynı | `published()` | `referencedata.TestVersionNamesTheData` |
| Defter yalnız eklenir: yayımlanmış satırlar bayt bayt aynı, `VERSION`'sız yeni satır yok, satırlar sürüm sırasında | `tools/id_ledger.csv`, `publishedLedger()` | `referencedata.TestIDLedgerIsAppendOnly` |
| Her önlisans programının kodu var; her kodun son defter satırı kendi bölümünü kendi fakültesinde gösterir; `minted` kimlik UUIDv7'dir; her kod bir önlisans programınındır; varyantlar birleşti | dosyalar, defter | `referencedata.TestIDLedger` |
| Yeniden kullanılan ve taşınan kod betiği durdurur, duran koşu yazmaz; `new`/`same`; bayat, başka duruma ait ve gereksiz karar durur; iki kimlik durur; şapka durur; birimin çıkışı ve geri dönüşü; kanonik kodu düşen program kimliğini korur; idempotent; defter yalnız büyür | `from_yok.py` (`Ledger.lookup`, `check_new`, `drop`) | `from_yok.py --self-test`, `referencedata.TestFromYokSelfTest` |
| Anahtarın geçmişi yalnız kararla, düşmeyle ya da geri dönüşle değişir; düşmüş birim ve üniversite dosyada yok, ötekiler var | defter | `referencedata.TestIDLedger` |
| Karar dosyasında yalnız güncel sürümün kararları; her karar bu sürümün bir defter satırı ve adlandırdığı durumu tutar; kararla yazılmış her satırın kararı dosyada | `tools/id_decisions.csv` | `referencedata.TestIDDecisions` |
| Tanıklar: THK Ankara Havacılık MYO / Uçak Teknolojisi (iki kod), Gülhane SMYO / Tıbbi Görüntüleme Teknikleri (Ankara) (kontenjan ekleri birleşti, yerleşke kaldı), İzmir Konak MYO (üniversite satırı), Atatürk Açıköğretim (iki derece, "(Önlisans)") | dosyalar | `referencedata.TestAssociateWitnesses` |
| Kontenjan eki yok, ebeveyn altında tek ad (iki derece birlikte) | dosyalar | `referencedata.TestCleanupHolds` (değişmeden, önlisansı da tarar) |
| Derece `ASSOCIATE`/`BACHELOR`, yalnız bölümde; kod 9 hane, tekil, bölümü dosyada; adda Zl, Zp ve Co yok | `Validate`, `checkName` | `referencedata.TestValidate` (her kural ayrı vaka) |
| Bölüm dosyasının `degree` kolonu, kod dosyasının biçimi | `parse`, `parseSourceCodes` | `referencedata.TestParse` |
| Sürüm 1 → 2 yalnız ekler, sayılar tam, ikinci koşu yazmaz | `Sync` | `referencedata.TestSyncFromVersionOne` |
| `0048` sürüm 1'i `BACHELOR` yapar; down önlisansı siler ve sürümü tutar, up ve eşitleme geri getirir; kayıt varken down durur | `0048` | `referencedata.TestMigrationGivesVersionOneItsDegree` |
| Derecesi değişen bölüm reddedilir, hiçbir şey yazılmaz (iki yönde; emekli bölüm de, `case` sırası) | `plan` | `referencedata.TestSyncRefusesADegreeChange` |
| Başka bölüme verilen kod: eski çift emekli, yeni çift eklenir, tek eşitlemede; kodun kullanımda tek çifti (veritabanı da reddeder); emekli kodun üçüncü bölüme verilmesi yeni çifttir; eski çift geri gelir, öteki emekliye ayrılır | `planSourceCodes`, `applySourceCodes`, kısmi tekil indeks | `referencedata.TestSyncReassignsACode` |
| Kodların emeklilik sınırı (462 geçer, 463 durur, yazımdan önce), taban yalnız kullanımdaki çiftler (5.000 emekli çiftin yanında), geri gelen kod etkinleşir | `Sync` adım 5, `applySourceCodes` | `referencedata.TestSourceCodesHoldTheRetireLimit` |
| Bölümlere verilen izin kodların sınırını açmaz | `Sync` adım 5 | `referencedata.TestSyncStopsAtTheRetireLimit` |
| Bölümle düşen kodlar emekliye ayrılır, bölümle geri gelir | `planSourceCodes` | `referencedata.TestSyncFollowsTheFiles` |
| Template adı kod dosyasını kapsar | `dbtest.referenceFS` | `dbtest.TestTemplateNameCoversTheReferenceData` |
| `ProgramDegree` sözleşmenin enum'unun her değerini karara bağlar; bilinmeyen değer hiçbir programı adlandırmaz; sabitler dosyalarınkiyle aynı | `reference/domain.go` | `reference.TestProgramDegree` |
| Liste süzgeci, MASTER/DOCTORATE = BACHELOR, boş liste, sözleşme dışı derece 400, süzgecin getirdiği üç `retired_at` koşulu ayrı ayrı | `reference.sql` | `app.TestReferenceListsFilterByDegree` |
| Süzülmüş listenin `ETag`'i ve 304'ü | `reference/http.go` | `app.TestReferenceListsAreCacheable` |
| Derece ile bölüm tutarlılığı, oluşturmada ve değiştirmede, kaydın taşıdığı bölümde de | `GetDepartmentOf` | `app.TestEducationDegreeNamesItsProgram` |
| Kod tablosunun yetkisi ve katalog sınıfı | `0048`, `softdelete.yaml` | `migrations.TestRoleGrants`, `TestCatalogMatchesDatabase` |
| Kod tablosunun listesi yok, öteki her tablonun bir listesi var | sözleşme | `app.TestReferenceHasNoWriteOperation` |

**Denenmedi:**
- Testlerin hiçbiri bu Mac'te koşturulmadı (CLAUDE.md); kanıt CI'dadır. Yalnız `from_yok.py --self-test` yerelde Python 3.14 ve 3.9 ile koştu.
- `TestIDDecisions`'ın karar satırlı yolu gerçek veride boştur (2026'da karar yok); o yolu öz test dener, Go testi bugün yalnız başlığı ve boşluğu bağlar.
- Kuralları tek tek silip testin kırmızıya döndüğü denenmedi; tablo testin neyi bağladığını yazar.
- `koala reference sync --dry-run` yerelde koşturulmadı (veritabanı yok). Karar 8'in tablosu `from_yok.py`'nin raporundan gelir; `TestSyncFromVersionOne` ve `TestMigrationGivesVersionOneItsDegree` aynı sayıları CI'da bağlar.
- `TestFromYokSelfTest` verify job'unda `python3`'ün bulunmasına dayanır; bulunmazsa kırmızıdır, atlanmaz.

## Yerine geçtiği

1. **ADR-0037 Karar 4** birebir şunu der: "Yeni satırın kimliği UUIDv7'dir (ADR-0012 Karar 5). Kimlik bir doğal anahtardan (YÖK program kodu, ad) türetilmez: ad değişir, ve Java'nın verisinde YÖK kodu yok." Bu madde **değişmiyor, genişliyor**. Yeni kimlik hâlâ UUIDv7'dir ve koddan türemez. YÖK'ten gelen satırlar için kimliğin kaynağı artık kimlik defteridir (Karar 6). ADR-0037'nin (ebeveyn, ad) ile yıllık eşleştirmesi önlisansta defterle yapılır; ad eşleştirmesi yalnız defterde olmayan anahtar için kalır.
2. **ADR-0012 Karar 5** birebir şunu der: "**Kimlikler:** Uygulama UUIDv7 üretir (§3.6-4)." Bu madde değişmiyor. Defterin kimlikleri UUIDv7'dir. Referans verisinde onları uygulama değil araç (`from_yok.py`) üretir ve defter kalıcı kılar; ADR-0037'den beri referans satırlarını uygulama yazmaz.
3. **ADR-0037 Karar 8** ve **açık soru 1** → bu ADR. Karar 8 birebir (vurgusuz) şunu der: "Yusuf eklemeyi onayladı (23.11.2026); veri ayrı bir PR'da, aynı araçla ve yeni bir sürümle gelecek (yeni satırlar, UUIDv7 kimlikler)." Bu, yazıldığı gibi uygulandı. Açık soru 3 (`yok_code`) önlisans için Karar 5 ve 6 ile kapanır; lisans bölümlerinin kodu açık soru 1'dedir.
4. **ADR-0037 *"Silinirse kırmızı"* tablosundaki "Referans uçları … tabloya bağlı" satırı**: `department_source_codes` bir referans tablosudur ama listesi yoktur (`TestReferenceHasNoWriteOperation` onu ayrık tutar).

ADR-0036 Karar 6'nın tek kod kararı değişmiyor, genişliyor: derecesine uymayan bölüm de `REFERENCE_ITEM_NOT_FOUND` alır.

## Sonuçlar

- `ASSOCIATE` kaydı artık bir MYO'yu ve programını adlandırabilir. İstemci derece seçimini `degree` ile listelere geçirir.
- **Yıllık güncelleme** (README) şu adımlardır:
  1. `VERSION` artırılır, ham yanıt yeniden çekilir, `from_yok.py` koşar.
  2. Programlar defterden, kodlarıyla eşleşir. Kapanan program ve kodları dosyadan çıkar ve emekliye ayrılır; yeni program yeni bir UUIDv7 alır ve deftere girer.
  3. Betik durursa söylediği anahtarlar karara bağlanır: yeni ya da adı değişen kurum `tools/yok_universities.csv`'ye; adı değişen, yeniden kullanılan ya da taşınan kod `tools/id_decisions.csv`'ye yazılır.
  4. Ardından `clean.py` koşar ve `publishedLedger()`'a yeni sürümün satırı eklenir.
- **ÖSYM bir kodu yeniden kullanırsa** iki şey olur, ikisi de sessiz değildir: betik durur ve karar ister; `new` kararı kodu yeni programa verir. Eski program kimliğini ve kayıtlarını korur. Kodun eski çifti emekliye ayrılır, yenisi eklenir ve kuru koşu ikisini de gösterir.

## Açık sorular

1. **Lisans verisi YÖK'ten yenilenmeli** (koordinasyon: ayrı PR). Beş üniversitenin yeni adı, dört yeni üniversitenin lisans fakülteleri ve lisans bölümlerinin ÖSYM kodları aynı uçtan (`birimTuruId` 46) gelir. Bu iş deftere lisans anahtarları ekler, kimlikleri korur ve bir ad değişikliği sürümü ister. Ürün kararıdır: ad değişikliği eşitlemede `UPDATED` görünür, kayıtlar yeni adı görür.
2. **Kılavuzun sınırı:** o yıl öğrenci almayan programlar listede yok. Eski mezunların programı için YÖK'ün başka bir listesi (ör. `istatistik.yok.gov.tr`) gerekir mi? Kaynak araştırması gerekir.
3. **Lisans:** ADR-0037 açık soru 2 ile aynı; YÖK Atlas verisinin açık bir lisansı doğrulanamadı.
