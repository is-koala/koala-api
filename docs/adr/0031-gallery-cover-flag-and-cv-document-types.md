# ADR-0031: Galeri kapağı bağımsız bir işaret, özgeçmiş üç belge türü

- Durum: Kabul edildi
- Tarih: 08.11.2026
- Karar kaydı: K10, K11 (sözleşme), K19 (medya)
- Kaynak: Yusuf'un 08.11.2026 kararları (galeri kapağı ve özgeçmiş dosya türleri); [plan](../plan/go-gecis-degerlendirme.md) §4.1, §5.4, §6.1 Faz 2; [ADR-0003](0003-dependency-policy.md) (bağımlılık politikası), [ADR-0005](0005-api-contract-principles.md), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) Karar 2 (2020-12 doğrulayıcısı), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 5–7, [ADR-0030](0030-object-storage-media-pipeline-and-exif.md) Karar 3–4 ve açık soru 1; Java `origin/main` (0cbbce69, v0.9.32) — `GalleryManager`, `UpdateGalleryDto`, `GalleryImageEntryDto`, `UploadValidator`, `UploadProperties` okundu, koşturulmadı
- Dış kaynaklar: LinkedIn, Indeed, Greenhouse, Lever ve Kariyer.net'in özgeçmiş yükleme biçimleri (üçü de PDF + DOC + DOCX); JSON Schema 2020-12 §10.3.1.3 (`minContains`'in öntanımı), ECMA-376 (OOXML paket yapısı), MS-CFB (OLE bileşik dosyası)
- Kod: `api/openapi.yaml`, `internal/platform/imaging`, `internal/platform/storage/key.go`, `internal/media`, `api/closedbody_internal_test.go`, `test/scenarios/{company,media}.yaml`, `docs/plan/go-gecis-degerlendirme.md`

## Bağlam

ADR-0029 ve ADR-0030 kabul edildi ve birleşti; ikisinde de ürün tarafına bilgi olarak geçmiş birer kalem vardı. Yusuf ikisini de geri çevirdi:

1. **Galeri kapağı.** ADR-0029 Karar 7 kapağı galerinin sırasından **türetti** ("kapak dizinin ilk öğesidir") ve bunun Java'ya göre bilinçli bir daralma olduğunu, ürün tarafına bilgi olarak geçeceğini yazdı. Geçti: arayüzde "bu fotoğrafı kapak yap" düğmesi var ve türetilmiş kapak onu temsil edemiyor. Kapak, sırası kaçıncı olursa olsun herhangi bir öğe olabilmeli.
2. **Özgeçmiş dosya türü.** ADR-0030 açık soru 1 "PDF-only kalıyor" dedi ve gerekçesini sektör pratiğine dayandırdı. Dayanak yanlıştı: LinkedIn, Indeed, Greenhouse, Lever ve Kariyer.net üçünü de (PDF, DOC, DOCX) kabul ediyor, birkaçı `.docx`'i en iyi ayrıştırdığı için tercih ediyor. Java da bugün üçünü kabul ediyor, yani PDF-only kayıtsız bir **daralmaydı**.

Kabul edilmiş ADR değiştirilmez (`docs/adr/README.md`), bu yüzden düzeltme bu ADR'dedir. ADR-0029 ve ADR-0030'un dosyalarına dokunulmadı; ikisinin durumu "Kabul edildi" kalıyor, çünkü yerine geçilen şey bütünleri değil, aşağıda tek tek sayılan kayıtlarıdır (ADR-0018'in ADR-0022 karşısındaki konumuyla aynı).

Planın §6.1'i kabul edilmiş bir ADR değil, bu yüzden metni düzeltilebilir; neyin değiştiği Karar 4'te.

## Karar

### 1. Galeri kapağı öğenin kendi işaretidir, sırasından türetilmez

`CompanyGalleryItemInput` **isteğe bağlı** bir `cover` boolean'ı alır. İsteğe bağlı olması uyumluluğu kendiliğinden korur: alanı hiç göndermeyen bir istemci hiçbir öğeyi işaretlememiş sayılır, o durumda **listenin ilk öğesi kapak olur** ve istemci ADR-0029'un davranışını aynen görür. Yeni alan, yanıta değil isteğe eklendi; yanıtta kapak zaten `CompanyGallery.coverMediaId` ile tek yerde adlandırılıyor, bu yüzden `GalleryImage` bir `cover` alanı **almıyor** — iki yerde durum tutmak, ikisinin ayrışmasına açık kapı bırakmak olurdu.

Java'nın dört gözlenebilir davranışı da korunuyor ve bu ADR'nin kapsamı tam olarak bu dördü:

- işaretlenen öğe kapak olur (`replaceCompanyGallery` → `existing.setCover(...)`);
- hiçbiri işaretlenmemişse ilki kapak olur (`applyDefaultCover`);
- ekleme kapağı değiştirmez, yalnız galeri boşken eklenen görüntü kapak olur (`addCompanyImage` → `setCover(current.isEmpty())`);
- kapak çıkarılırsa kalanların ilki kapak olur (`removeCompanyImage` → `applyDefaultCover`).

**"En çok bir kapak" değişmezi şemada yazılı, kendi hata kodu yok.** Java bunu `@AssertTrue isAtMostOneCoverSelected` ile doğruluyor. Bizde `ReplaceCompanyGalleryRequest.items` şu üç anahtarı taşıyor:

```yaml
contains: { type: object, required: [cover], properties: { cover: { const: true } } }
minContains: 0
maxContains: 1
```

`minContains: 0` süs değil: JSON Schema 2020-12'de `minContains`'in öntanımı **1**'dir, yani onu yazmadan `contains` en az bir eşleşme ister ve **kapaksız liste reddedilirdi** — tam da korumak istediğimiz uyumluluk kapısı kapanırdı.

Bu, sözleşmenin **bir istek gövdesindeki ilk** 2020-12'ye özgü anahtarıdır. Sözleşmenin bütününde ilk değil: `problem.yaml` zaten `dependentRequired`, `not`, `if` ve `then` kullanıyor, ama onlar **yanıt** şemasında ve `compileBody2020` yalnız istek gövdelerine bakıyor — yanıtlar doğrulanmıyor. Ayrımı yazmak gerekiyor, çünkü "ilk kullanım" iddiası zincirin niye tek tek doğrulandığının gerekçesi: "koruma yazılı ama çalışmıyor, kapı yeşil" kalıbı bu depoda üç kez çıktı (ADR-0029 Karar 11–12) ve dördüncüsü olmayacak:

- `httpx.schemaUses2020Here` `Contains` ve `MaxContains`'i tanıyor, yani operasyon `compileBody2020` yoluna giriyor;
- `inlineSchema` `contains`'i açıkça iç içe geçiriyor (`one("contains", s.Contains)`) ve `maxContains` kin-openapi'nin `json:"maxContains"` etiketiyle serileşiyor, yani kısıt derlenen şemaya **giriyor**;
- doğrulama `santhosh-tekuri/jsonschema/v6` ile 2020-12 taslağında koşuyor;
- `fields2020` `*kind.MaxContains`'i açıkça eşliyor, yani iki kapak taşıyan gövde `400 REQUEST_INVALID` ve `/items` işaretçisinde `FIELD_OUT_OF_RANGE` alıyor — "sınıflandırılamayan doğrulama hatası" değil;
- anahtar `not`/`oneOf` altında değil ve bir parametrede değil, yani `compileBody2020`'nin açılışta reddettiği iki biçimden de uzak.

Zincirin okunması kanıt değil, gerekçedir; **kanıt koşan iki testtir** ve ikisi de DB'siz, authn'siz:

- `httpx.TestFields2020`'nin üç yeni satırı `fields2020`'nin `*kind.MaxContains` ve `*kind.MinContains` dallarını koşturuyor. Üçüncü satır `minContains: 0`'ın niçin taşıyıcı olduğunu gösteriyor: aynı şema onsuz yazıldığında kapaksız liste `kind.Contains` ile reddediliyor, yani uyumluluk sözü düşüyor. Böylece `minContains: 0`'ı sözleşmeden düşürmenin gözlenebilir bir anlamı var, yalnız bir yorumu değil.
- `httpx.TestValidateRequestsAtMostOneCover` gerçek router ve gerçek `ValidateRequests` üzerinden, sözleşmedekiyle **aynı üç anahtarı** taşıyan bir operasyonla altı vakayı koşturuyor: bir kapak, kapaksız (eski istemcinin gövdesi), boş liste ve hepsi `false` geçiyor; iki kapak ve on `true` `400` + `/items` `FIELD_OUT_OF_RANGE` alıyor.

Gerçek galeri ucunun kendisi üzerinden uçtan uca kısım handler PR'ına kalıyor, çünkü doğrulama authn'den **sonra** koşuyor ve galeri PUT'u kimlikli; ADR-0029 Karar 1'in 501 tanığını "kamuya açık ve gövdesiz" seçme gerekçesi aynı nedenle duruyor.

Kuralın kendi hata kodunu istememesi, CMP-024'ün yinelenen medya kuralıyla arasındaki farkın doğrudan sonucudur ve ikisini ayırmak önemli: yinelenme şemayla ifade **edilemiyor** (öğeler nesne, `uniqueItems` bütün nesneyi karşılaştırır, aynı `mediaId` + farklı `caption` iki ayrı öğe sayılır), bu yüzden `COMPANY_GALLERY_DUPLICATE_IMAGE` bir gövde kuralı olarak duruyor ve handler'ı yazmak zorunda. Kapak kuralı ifade **edilebiliyor**, bu yüzden kenarda zorlanıyor ve handler'ın unutabileceği bir yükümlülük olmaktan çıkıyor. Yeni hata kodu eklenmedi; `x-error-codes` listeleri ve durum kapsama testi (ADR-0029 Karar 10) bu yüzden değişmedi.

**Kapalı gövde yürüyüşü bu anahtara kördü.** `api/closedbody_internal_test.go`'nin yürüyüşü `properties`, `allOf`, `oneOf`, `anyOf`, `items` ve `additionalProperties` dışındaki yuvalara girmiyordu, yani 2020-12'nin yedi yuvasının hepsi denetimin dışındaydı. İlk kullanıcısı olduğum için yürüyüş genişletildi: gövdenin taşıyabileceği bir üyeyi **tanımlayabilen** yuvalar (`prefixItems`, `then`, `else`, `dependentSchemas`, `patternProperties`, `unevaluatedItems`) artık geziliyor; `contains`, `not`, `if` ve `propertyNames` gezilmiyor ve bu bir **karar**, eksik değil: her biri doğrulayıcının saydığı ya da sınadığı bir yüklem, gövdenin alabileceği bir biçim değil. Birinin altına `additionalProperties: false` yazmak hiçbir şeyi kapatmaz, yüklemin neyle eşleştiğini değiştirir — bizim `contains` alt şemamız gerçek bir öğeyle eşleşmek zorunda olduğu için kapatılmamalıdır. Gerekçe testin kendi başlığında duruyor ki sonraki okuyan "düzeltmeye" kalkmasın.

Genişletmenin kanıtı da koşuyor: sözleşmede bu altı yuvanın hiçbiri geçmediği için yeni dallar gerçek sözleşmede **sıfır düğüm** geziyordu, yani `s.Then` yerine ikinci kez `s.Else` yazılsa hiçbir şey kırmızı olmazdı. `TestOpenRequestObjectsFindsAllOfGaps`'in sentetik spec'i artık altı yuvanın her biri için ayrı bir yol taşıyor ve beklenen yolları adlandırıyor; `/predicate-slots` yolu da işin öbür yarısı — `contains`, `not`, `if` ve `propertyNames` altındaki açık nesneler **hiçbir bulgu üretmemek** zorunda, yani muafiyetin yanlış pozitif üretmediği de sınanıyor.

Gövde sınırı (`x-body-limit: 16384`) değişmedi: `cover` ile en kötü durum 16.081 bayt (öğe başına 36 baytlık `mediaId`, `\u` kaçışlı 255 karakter ve `"cover":true` ile 1.606 bayt; 10 öğe, ayraçlar ve zarf). Hesap operasyonun yorumunda güncellendi, çünkü sınırın neden 16 KiB olduğunu açıklayan aritmetik artık bir terim daha taşıyor.

### 2. Özgeçmiş PDF, DOC ve DOCX kabul eder; tür içerikten saptanır

`MediaContentType` iki değer büyüdü ve `KindCV.AllowedTypes()` üç tip döner. Enum'a değer eklemek uyumlu bir değişikliktir (stil rehberi §2). Boyut sınırı Java'nın `maxFileSizeMb`'si olan **10 MiB'de kaldı**.

**Adlandırma.** Değerler `APPLICATION_MSWORD` ve `APPLICATION_VND_OPENXMLFORMATS_OFFICEDOCUMENT_WORDPROCESSINGML_DOCUMENT`. İkincisi uzun; yine de kısaltılmadı, çünkü enum'un mevcut üç değeri medya tipinin UPPER_SNAKE yazımıdır (stil rehberi §1) ve kuralın mekanik kalması eşlemenin iki yönde de fonksiyon olmasını sağlar. `OFFICEDOCUMENT`'ı atan daha kısa bir yazım önerildi; alınmadı, çünkü onu hiçbir kural üretmiyor — elle kısaltılmış tek bir değer, "medya tipinden türet" kuralını "bir istisna dışında türet"e çevirir.

**`.docx` ve `.doc` nasıl saptanıyor — bu PR'ın tasarım kararı.** Sorun şu: `PK\x03\x04` yalnız DOCX'in değil xlsx, pptx, odt, jar, apk ve sıradan her zip'in de ilk dört baytı; `D0 CF 11 E0 A1 B1 1A E1` yalnız DOC'un değil xls, ppt ve msi'nin de imzası. Sihirli bayt bu iki tür için **karar vermiyor**, yalnız hangi kap olduğunu söylüyor. Seçenekler ve seçim:

- **Sihirli baytı kabul edip beyan edilen türe güvenmek.** Alınmadı. Alınsaydı ADR-0029 Karar 5'in "dosya adına ve beyan edilen tipe güvenmeyiz" kararından bir **sapma** olurdu ve bunu böyle yazmak gerekirdi. Üstelik sözleşme bunu desteklemiyor: `StartMediaUploadRequest` yalnız `kind` ve `sizeBytes` taşır, yani istemcinin güvenilecek bir beyanı yok. Sapma yok; kayda geçen şey sapmanın **alınmadığı**dır.
- **Kabın kendi yapısını okumak.** Alındı. DOCX için arşivin merkezi dizini (içindekiler listesi) okunur ve `word/document.xml` aranır; bir hesap tablosu `xl/workbook.xml`, bir sunum `ppt/presentation.xml` taşır, bu yüzden bu tek ad aileyi ayırır. DOC için OLE başlığından sonra, bir dizin girdisinin yazdığı biçimde (UTF-16LE) `WordDocument` akış adı aranır.
- **Biçimi tam ayrıştırmak.** Alınmadı: onaylı listede OOXML ya da CFB kütüphanesi yok (ADR-0003) ve yazmak bu PR'ın kapsamını kat kat aşar.

Sektör de ilk seçeneği artı zararlı yazılım taramasını yapıyor; biz ikinciyi yapıyoruz, yani tür kararında sektörden **daha sıkıyız**, zararlı yazılım taramasında ise hiçbir şey yapmıyoruz (aşağıdaki sınır).

**Seçimin sınırları, tek tek:**

- **Koruma dikkatsiz yüklemeye karşıdır, saldırgana karşı değil.** Bunu olduğu gibi yazmak gerekiyor, çünkü ilk hâli olduğundan güçlüydü. **İki işaret de taklit edilebilir:** `isWordprocessingML` yalnız arşivin içindekiler listesinde `word/document.xml` **adını** arıyor, dolayısıyla o adda boş bir girdi eklenmiş bir apk geçer; `isWordBinary` yalnız 24 baytlık UTF-16LE diziyi arıyor, dolayısıyla o baytlar gömülmüş herhangi bir OLE dosyası geçer. İkisi de yapı **kanıtı** değil, taklit edilebilir **işaret**tir; DOC için yazdığım "hatanın yönü" DOCX için de aynen geçerlidir ve ilk hâlde yazılmamıştı. Yanlışlıkla mp4 yükleyen kullanıcı reddedilir — satın alınan budur, ve dar olduğu için dar yazılmalı: içinde hiçbir Word işareti olmayan dosya **hiç** saklanamıyor, yani yuva açık bir dosya barındırıcısı değil.
- **Taklit edilen bir işaretin getirdiği şey azaltmalarla sınırlı.** Nesne ek olarak, önbelleklenmeden, genel adrese hiç çözülmeden, kısa ömürlü imzalı adresle, 10 MiB sınırında ve **kullanıcının kendi yuvasında** duruyor. Yani en kötü hâli, kimlikli bir kullanıcının kendi CV yuvasını küçük bir özel depo gibi kullanması; XSS yolu yok, üçüncü kişiye sunum yok. Kabul edilen bu yanlış pozitif testte duruyor (`TestCompleteCVAcceptsAForgedWordContainer`, `TestCompleteRejections`'ın yanında) ve azaltmaları da aynı test doğruluyor — yalnız reddetme tarafı değil, **kabul** tarafı da görünür.
- **Makro ve zararlı yazılım hiç saptanmıyor.** Makro (`.doc`'ta ve `.docx` kabında), gömülü nesne, harici varlık: hiçbiri bakılmıyor. Zararlı yazılım taraması tür saptamasının yerine geçmez ve bu ADR'nin açık sorularından biridir.
- **DOC saptaması bir sinyaldir, ayrıştırma değil**, ve hatasının yönü belli: Word nesnesi **gömülü** bir xls ya da ppt gerçekten `WordDocument` akışı taşır, dolayısıyla DOC olarak kabul edilir. Bileşik dosyanın dizinine ulaşmak FAT zincirini izlemek demek; saldırganın verdiği ofsetler üzerinde yeni bir ayrıştırıcı yazmanın buradaki kazancı yok. Sonucu sınırlı: nesne ek olarak saklanıyor, Koala tarafından hiç görüntülenmiyor ve genel bir adrese çözülmüyor. Akış adını hiç taşımayan dosya kabul edilmiyor; yuvayı koruyan şey o.
- **Zip dizinini ayrıştırmak yeni bir yüzeydir.** Daraltıldı: hiçbir girdi **açılmıyor**, yalnız içindekiler listesi okunuyor, dolayısıyla zip bombası hiç şişirilmiyor. İş ve bellek `len(b)` ile sınırlı; çağıran onu zaten 10 MiB'de sınırlıyor ve `archive/zip` dosyanın taşıyamayacağı bir kayıt sayısı gördüğünde onu **reddetmiyor**, yalnız dizin dilimini ön-ayırmayı atlıyor (`reader.go:131`) — sonuç aynı, kurulan dizin en çok 46 bayt girdiye bir kayıt, ama fiili böyle yazılmalı. Ayrıştırıcı standart kütüphanenin, yani yeni bağımlılık yok. `FuzzDetect`'in korpusuna gerçek bir arşiv ile gerçek bir bileşik dosya **tohum olarak** eklendi; kapıda `-fuzz` koşmadığı için bunlar bugün sabit tohumlar olarak koşuyor, mutasyon yalnız yerelde `-fuzz` ile çalıştıran için devreye girer. Aynı şey EXIF hedefleri için de doğru, yani gerileme değil — ama cümle olduğundan fazlasını söylemesin: mutasyonlu fuzz'ın kapıda olmaması Sonuçlar'da izleyen kalem olarak duruyor.
- **`Detect` artık bütün nesneyi istiyor.** Eski imza "ilk birkaç baytı okur, çağıran bir baş verebilir" diyordu; DOCX'in içindekiler listesi dosyanın **sonunda** olduğu için bu artık doğru değil. İlk aşamanın cevabını tek başına almanın yolu bilinçli olarak yok: dışarı sızmış bir "bu bir zip" kimsenin doğrulamadığı bir tür kararı olurdu. Bir baş, DOCX olarak değil, kabul edilmeyen bir şey olarak cevaplanır — fail-closed yön.
- **Belge üstverisi sökülmüyor — kabul edilen sınır.** ADR-0030 Karar 4 bunu PDF için kaydetmişti; DOC ve DOCX de aynı yerde. Yazar, şirket, düzeltme geçmişi, gömülü önizleme: hepsi olduğu gibi kalıyor. *Gerekçe:* sökmek OOXML ve CFB yazabilen bir kütüphane ister (ADR-0003 madde 3, ayrı onay) ve ADR-0030'un ayrımı burada da geçerli — özgeçmişin yazar alanındaki ad kullanıcının kendi koyduğu ve zaten işverene gösterilen veridir, EXIF'teki GPS gibi farkında olmadan verdiği bir veri değil. *Azaltma:* `Content-Disposition: attachment`, `Cache-Control: no-store`, `files/` öneki (`PublicURL` `false` döner, yani genel adres **hiç** üretilmez) ve yalnız kısa ömürlü imzalı adres. *Tetikleyici:* onaylı listeye bir OOXML/CFB kütüphanesi girerse, ya da bu nesneler genel adrese çözülür hâle gelirse — o anda azaltmanın yarısı düşer. Sınır testte açıkça duruyor (`TestCompleteCVMovesTheDocumentAsItIs` üç türün de yazar adını saklanan baytlarda arar), yani sessiz değil.
- **Zararlı yazılım taraması yok ve bu bir sonraki iştir.** Tür saptaması onun yerine geçmez. Özgeçmişi indiren işverenin makinesinde açılacak bir dosyadır; ek olarak sunmak tarayıcıyı korur, işvereni korumaz. Faz 2'nin uygulama PR'ının değil, ayrı bir kalemin konusu: Sonuçlar'da duruyor.

Anahtar alfabesi `jpg | png | pdf` üçlüsünden `jpg | png | pdf | doc | docx`'e genişledi; `storage.Exts()`, `keyPattern` ve `media.ExtOf` birlikte büyüdü. Üçlüyü **iki** test bağlıyor, biri değil: `storage.TestEveryPrefixAndExtIsParsable` `Exts()` ile `keyPattern`'i bağlıyor ama `media.ExtOf`'u anamıyor (platform paketi `media`'yı import edemez, CLAUDE.md), `media.TestExtOf` ise `imaging.Types()` ile `ExtOf`'u bağlıyor. Kalan tek yön `Exts()`'teki ölü bir uzantı: hiçbir tipin üretmediği bir uzantı eklenirse ikisi de sessiz kalır. `MaxKeyLen` değişmedi (256), ama yorumundaki "50–62 bayt" aralığı yanlıştı ve düzeltildi: en kısa önekle üç harfli uzantıda 46, en uzun önekle `docx`'te 64 bayt.

`imaging.Types()` ile imza tablosu arasına dışarıdan bir bağ eklendi (`TestEveryAcceptedTypeIsDetectedFromASample`): bir tür `Types()`'a girip imzası eklenmezse, o türün her yüklemesi `TYPE_UNSUPPORTED` ile reddedilirdi ve hiçbir test kırmızıya dönmezdi. `Type.Container()` de ayrı bir liste değil, imza tablosundan okunuyor; ikisinin ayrışması mümkün değil.

**Üçüncü enum da bağlandı.** `internal/app/media_contract_test.go` `MediaKind` ve `MediaStatus`'u sözleşmeye bağlıyordu, `MediaContentType`'ı atlamıştı — ve bu PR iki yanı da ikişer değer büyüttü, yani biri atlansa hiçbir test kırmızı olmayacaktı. Drift'in bedeli iki yönde de somut: `imaging`'in kabul ettiği ama enum'un taşımadığı bir tip, `Complete`'in kaydettiği ve yanıtın taşıdığı, sözleşmede **bildirilmemiş** bir `MediaContentType` demek — yanıtlar doğrulanmadığı için bunu hiçbir şey yakalamaz; ters yön ise sunucunun hiç üretemeyeceği ölü bir enum değeri. Bağ, adlandırma kuralının kendisi üzerinden kurulu (`contentTypeEnumValue`: büyük harf, her ayırıcı alt çizgi) ve iki yönde de sınanıyor. Bu, enum adını elle kısaltmamanın somut karşılığıdır: hiçbir kuralın üretmediği bir değer bu fonksiyonu, dolayısıyla bu bağı, yazılamaz hâle getirirdi.

### 3. Yükleme başlatma isteği: dosya adı bildirilmez, beyan edilen tipi handler seçer

Bu PR'ın açtığı iki soruyu Faz 2 koordinasyonu 08.11.2026'da karara bağladı; ikisi de `StartMediaUploadRequest` ile `Pipeline.Start` arasındaki boşlukla ilgili ve ikisi de burada kapanıyor, ki handler PR'ı tartışmayı yeniden açmasın.

**Dosya adı bildirilmez.** `StartMediaUploadRequest`'e `fileName` alanı **girmiyor**; istek `kind` ve `sizeBytes` ile kalıyor. Karar **Yusuf'undur**, gerekçesi sade kalmak ve saklanacak bir kullanıcı metni daha olmaması: bildirilen her ad doğrulanacak, saklanacak, maskelenecek ve KVKK kapsamında düşünülecek bir alan daha demektir. Görünür sonucu ADR-0030'un kaydettiği daralmadır — kullanıcı özgeçmişini `0192f3c4-….pdf` adıyla indirir — ve bu daralma **kabul ediliyor**. Kapı kapanmıyor: alanı sonradan eklemek uyumlu bir değişikliktir (stil rehberi §2), yani ihtiyaç görülürse ayrı bir kararla gelir. ADR-0030'un "özgün ad isteniyorsa isteğe bağlı bir alan gerekir" kalemi bu yüzden açık soru olmaktan çıkıp karara dönüşüyor.

**Beyan edilen tip için handler `AllowedTypes()[0]`'ı seçer ve bunu açıkça yazar.** `Pipeline.Start(ctx, kind, declared)` bir tip istiyor ama istek bir tip taşımıyor; boşluk gerçek, sonucu yok. Gerekçe: karantina anahtarının uzantısı **kozmetiktir**. Son anahtarın uzantısı `Complete`'in içerikten saptadığı tipten gelir (`final.WithExt(ext)`), dolayısıyla yanlış tahmin edilen bir karantina uzantısının hiçbir gözlenebilir sonucu yoktur — bugün de öyle: bir istemci PNG beyan edip JPEG yüklediğinde son anahtar `jpg` oluyor (`TestCompleteNamesTheKeyFromTheDetectedType`). Sözleşmeye beyan alanı eklemek bu yüzden gereksiz; seçim handler'da, kaynağında yorumla yazılacak. Bu PR seçimi CV'de bir seçenekten üçe çıkardı, karar o yüzden burada kayda geçiyor: `AllowedTypes()` sıralı ve ilk öğesi CV'de PDF'tir, yani kozmetik tahmin de en yaygın türü tahmin ediyor.

Sıra bu kararla **yük taşıyan** hâle geldiği için bağlandı: `media.TestKindRules` artık üyelik değil `slices.Equal` ile tam diziyi sınıyor, yani diziyi alfabetik sıraya sokan bir düzenleme kırmızıya döner. Bağımlılığı bu PR yarattı, çivisini de bu PR çakıyor.

### 4. Planın §6.1'inde iki düzeltme

Plan kabul edilmiş bir ADR değil, bu yüzden §6.1'in metni düzeltildi. ADR-0029 ikisini de "Yusuf'a bildirildi" diye kaydetmişti; bildirim karşılığını buldu:

1. **Faz 2'nin çıkış kriterindeki `owner` tipi.** Kriter "`company`, `user` ve `owner` tiplerinin satırları hem Go'da hem OpenFGA'da yeşil" diyordu. `owner` bir model tipi değil: planın kendi §5.4'ü ("Modelin dışında kalanlar") onu açıkça modelin dışına, `Scope`/`SelfUserID`'ye koyuyor, yani kriter planın kendisiyle çelişiyordu. Yeni metin: `company` ve `user` iki motorda yeşil; sahipliğin model karşılığı `private_file#owner`'ın satırları iki motorda yeşil; kalan sahiplik kararları (`bank_account`, `education`, `device`, `session`…) `go_only` kalır ve store'un `Scope`'unda sınanır. Modeli `owner` için genişletmek yanlış olurdu: kullanıcı-sahipli her satır için tuple yazmak ADR-0024'ün "yetki kararı veriden türetilir" tasarımını tersine çevirir.
2. **Eğitim kayıtları hiçbir faza atanmamıştı.** `/users/me/educations` dört ucu ADR-0029 Karar 11'in kapsam defterinde Faz 2'ye alınmıştı; §6.1'in Faz 2 kapsam hücresi artık "eğitim kayıtları"nı sayıyor.

### 5. 2020-12 saptamasının kapsamı: çocuk listesi genişletilmiyor

İnceleme `schemaUses2020`'nin çocuk listesinin eksik olduğunu, dolayısıyla `prefixItems`, `patternProperties`, `dependentSchemas` ya da `then`/`else` altına konan bir 2020 anahtarının **sessizce doğrulanmayacağını** söyledi ve iki fail-closed çözüm önerdi: çocuk listesini genişlet, ya da reddetme kapısını büyüt. İkisi de alınmadı, çünkü **boşluk yok** — ve bunun nedeni `schemaUses2020`'ye bakarak görünmüyor, o yüzden buraya yazılıyor.

`inlineSchema`'nın serileştirdiği on dokuz alt şema yuvası var. Yedisi (`properties`, `allOf`, `anyOf`, `oneOf`, `not`, `items`, `additionalProperties`) **geziliyor**. Kalan on ikisi (`prefixItems`, `patternProperties`, `dependentSchemas`, `contains`, `propertyNames`, `if`, `then`, `else`, `$defs`, `contentSchema`, `unevaluatedProperties`, `unevaluatedItems`) gezilmiyor ve gezilmesi **gerekmiyor**: her biri kendisi 2020-12'ye özgü bir anahtar, yani onu taşıyan şema içine hiç bakılmadan `schemaUses2020Here` tarafından zaten bildiriliyor. İki küme on dokuz yuvanın tamamını, artan hiçbir şey kalmadan kaplıyor. Örnek: `{properties: {a: {prefixItems: [{const: 1}]}}}` — kök `properties`'i geziyor, `a` ise `prefixItems` taşıdığı için bildiriliyor, `const`'a inmek hiç gerekmiyor.

Aynı argüman `uses2020UnderNotOrOneOf`'u da kapsıyor: o da aynı iki kümeyi yürüyor, yani `not`/`oneOf` altındaki bir 2020 anahtarı ya gezilerek ya bildirilerek bulunuyor ve açılışta reddediliyor.

Karar: **ikisi de genişletilmiyor.** Gerekçe iki tane. Birincisi, genişletmek davranışı hiç değiştirmiyor — bugünkü sözleşmede de, kurgusal örneklerde de cevap aynı. İkincisi, zararı var: `inlineSchema` yuva listesinin **zaten tam** olan üçüncü kopyası, dördüncüsünü açmak onu da senkron tutulacak bir yer yapar; ve genişletilmiş listeyi gören sonraki okuyucu örtünün oradan geldiğini sanar, oysa örtü `schemaUses2020Here`'den geliyor. Değişmeyen koda "her ihtimale karşı" eklenen satır incelemeyi de yanıltır.

Örtünün rastlantı olmadığını `httpx.TestSchemaUses2020FindsAKeywordAnywhere` çiviliyor: on dokuz yuvanın her biri için bir satır, artı 2020 anahtarı taşımayan bir şemanın 2020 yoluna **girmediğini** gösteren negatif satır. Test, örtü hangi mekanizmayla sağlanırsa sağlansın "gövdenin herhangi bir yerindeki 2020 anahtarı saptanır" özelliğini sınıyor; `schemaUses2020Here` daraltılırsa (`contains` düşürülürse, diyelim) ve yürüyüş aynı kalırsa boşluk açılır ve o satır kırmızıya döner.

**Kalan risk, kayda geçsin:** kin-openapi ne gezilen ne de kendini bildiren yeni bir alt şema yuvası eklerse örtü sessizce delinir. Uyarı kendiliğinden gelir, çünkü aynı yükseltme `schemaUses2020Here` ve `inlineSchema`'yı da güncellemek zorunda kalır; ama mekanik bir bağ (kin'in `Schema` alanlarını yansımayla taramak) yazılmadı — kütüphanenin alan adlarına ve etiketlerine bağlı olurdu, ki `additionalProperties` ve iki `unevaluated` yuvası `BoolSchema` taşıdığı için o tarama tek tiple de yapılamaz.

## Yerine geçtiği

1. **ADR-0029 Karar 7'nin türetilmiş kapak daralması** → Karar 1. Yerine geçen karar **Yusuf'un 08.11.2026 kararıdır**, teknik bir yeniden değerlendirme değil: ADR-0029 daralmanın "ürün tarafına bilgi olarak geçeceğini" yazmıştı, geçti ve ürün tarafı geri çevirdi. Düşen üç kayıt: Karar 7'nin "kapak dizinin ilk öğesidir" cümlesi, aynı kararın "Java'ya göre bilinçli bir daralmadır" paragrafı (daralma artık yok) ve Karar 7'nin galeri görünürlüğü paragrafındaki "galeri sıralaması aynı zamanda kapağı belirler: listedeki ilk görüntü kapaktır, ayrı bir kapak ucu yoktur" cümlesi. Son cümlenin yalnız ilk yarısı düşüyor: **ayrı bir kapak ucu yine yoktur**, kapak galeriyi değiştiren aynı operasyonla seçilir.
2. **ADR-0030'un açık soru 3'ündeki kapak cümlesi** → Karar 1. Sorunun cevabı ("kapak yeni bir `MediaKind` getirmiyor, `MediaKind` beş değerinde kalıyor") **değişmiyor**; düşen şey gerekçesinde ADR-0029'dan alıntıladığı "listedeki ilk görüntü kapaktır" cümlesidir. Galeri görüntüsü, kapak olanı dahil, `COMPANY_IMAGE` olarak kalıyor.
3. **ADR-0029 Karar 5 ve Karar 6'nın "izin verilen tipler" ve anahtar alfabesi kayıtları** → Karar 2. Karar 6'nın "alfabe kapalıdır: `jpg | png | pdf`" cümlesi ve aynı kararın okuma izin listesi deseni (`\.(jpg|png|pdf)$`) iki uzantı büyüdü. Kapalılık ilkesi değişmiyor: alfabe hâlâ kapalı ve hâlâ saptanan tipten türüyor.
4. **ADR-0030 Karar 3'ün üç tipli izin listesi** → Karar 2. "`imaging.Detect` sihirli bayt tablosudur ve izin listesi üç tiptir" cümlesi beş tipe çıkıyor; tablonun sihirli bayt olması ve `net/http.DetectContentType` kullanılmaması değişmiyor, ama tablo iki tür için **ikinci bir aşama** kazanıyor (kabın yapısı), çünkü o iki türde sihirli bayt tek başına karar vermiyor.
5. **ADR-0030'un açık soru 1'i: "CV'de `doc` ve `docx` düştü; PDF-only kalıyor"** → Karar 2. Yerine geçen karar **Yusuf'un 08.11.2026 kararıdır** ve gerekçesi sektör standardıdır: LinkedIn, Indeed, Greenhouse, Lever ve Kariyer.net üçünü de kabul ediyor, Java da üçünü kabul ediyor. ADR-0030'un "sektör pratiği PDF-only'dir" dayanağı yanlıştı.
6. **ADR-0030 Karar 4'ün "PDF üstverisi sökülmez" sınırı** → Karar 2'de **genişletiliyor**, kaldırılmıyor: aynı gerekçe, aynı azaltma ve aynı tetikleyici DOC ve DOCX için de geçerli, ve sınırı gösteren test üç türü de kapsar hâle geldi.
7. **ADR-0030'un `finishedEarlier` sayısı** → Karar 2. "Görüntü türünde iki `Head`, CV'de bir" cümlesi artık "CV'de üç": kurtarma yolu türün kabul ettiği her uzantıyı dener ve CV'nin üç uzantısı var. Yol yalnız karantina boşken koşuyor, yani sıcak yolda bir maliyet yok.
8. **Plan §6.1 Faz 2'nin çıkış kriterindeki `owner` tipi ve eğitim kayıtları boşluğu** → Karar 4. ADR-0029'un "Yerine geçtiği" bölümündeki 6. ve 7. kalemler "Yusuf'a bildirildi" diyordu; bu PR bildirimi planın metnine işliyor, yani iki kalem kapanıyor.

9. **ADR-0030'un "özgün ad isteniyorsa `StartMediaUploadRequest`'e isteğe bağlı bir alan gerekir — uyumlu bir ekleme, acelesi yok" kalemi** → Karar 3. Kalem kapanıyor: Yusuf'un kararı "bildirilmesin". Daralmanın kendisi (nesne kendi UUID'siyle iniyor) **duruyor** ve kabul ediliyor; değişen şey, onun bir gün kapatılacak bir açık uç olarak değil, gerekçeli bir karar olarak durması.

ADR-0029'un geri kalan on bir kararı, ADR-0030'un geri kalan altı kararı, ADR-0003, ADR-0005, ADR-0014, ADR-0019 ve ADR-0024 değişmiyor. ADR-0029 Karar 1'in dondurma mekanizması bu PR'da **aynen** duruyor: 71 donmuş operasyonun işaretleri, `pending` listesi ve iskeleleri değişmedi, bu PR handler yazmıyor.

## Sonuçlar

- Sözleşmenin bir istek gövdesinde ilk 2020-12'ye özgü anahtarı var, yani `compileBody2020` yolu artık bir operasyonda **canlı**. İki şey bugün gözleniyor: **derleme**, çünkü `ValidateRequests` açılışta bütün operasyonları hevesle derliyor ve `newAPIRouter` her CI koşusunda onu çağırıyor, yani bozuk bir 2020 şeması kapıyı düşürür; ve **reddin kendisi**, `TestValidateRequestsAtMostOneCover` ile `TestFields2020` üzerinden. Geriye yalnız gerçek sözleşme + gerçek galeri ucu üzerinden uçtan uca doğrulama kalıyor; o, doğrulamanın authn'den sonra koşması ve galeri PUT'unun kimlikli olması yüzünden handler PR'ına ait.
- **Mutasyonlu fuzz kapıda değil.** `Makefile` ve workflow'larda `-fuzz`/`-fuzztime` hiç geçmiyor, yani dört fuzz hedefi (EXIF'in üçü ve `FuzzStorageKey`) CI'da yalnız sabit tohumlarını koşuyor. Bu PR'ın getirdiği kap tohumları da öyle. Gerileme değil, bu PR'ın açtığı bir şey değil, ama kayda geçsin: korpus büyütmek koruma değil, kapıya `-fuzztime` koymak koruma. İzleyen kalem.
- **Zararlı yazılım taraması bir sonraki kalemdir.** Özgeçmiş artık işverenin makinesinde açılacak iki belge biçimini daha kabul ediyor; ek olarak sunmak tarayıcıyı korur, dosyayı açan insanı korumaz. Onaylı bir tarayıcı (ClamAV ya da sağlayıcı tarafı) ve nerede koşacağı (tamamlama adımı mı, sonradan bir River işi mi) ayrı bir karar ister. Yusuf'a bilgi olarak geçilecek.
- Handler PR'ının Karar 3'ten devraldığı tek yükümlülük: `Pipeline.Start`'a verdiği tipin `AllowedTypes()[0]` olduğunu ve bunun neden kozmetik olduğunu çağrı yerinde yazmak. Sessiz bir varsayım olarak doğmaması bu yüzden kayda geçti.
- `MediaContentType`'ın ikinci Word değeri 71 karakter ve üretilen Go sabiti 82; mekanik olması pahasına çirkin. Kuralı bozmaktan iyi, ama istemci üreteçlerinde (Dart, TypeScript) uzun enum adının bir sorun çıkarması Faz C'de görülecek.
- `RejectionCorrupt`'ın hata kodu olmaması (ADR-0030'un yükümlülüğü) değişmedi ve genişlemedi: DOC ve DOCX yolunda `Sanitize` koşmuyor, yani yeni bir `CORRUPT` kaynağı doğmadı — yapısı bozuk bir kap `TYPE_UNSUPPORTED` ile düşüyor.
- Senaryo katalogu 86'dan 88'e, P0'lar 70'ten 72'ye çıktı: CMP-025 (kapak) ve MED-014 (özgeçmiş türleri). İkisi de `deferred: {phase: 2}`, çünkü uçları donmuş durumda; MED-014'ün tür kabulü ve taşıması `internal/media`'da bugün yeşil ve bu, gerekçesinde yazılı. CMP-023 yeniden yazıldı: başlığı "sıra kapağı belirler"den "sıra ve açıklamalar tek işlemde değişir"e döndü, kapak kendi senaryosuna taşındı.
- Bu PR'da kalanlar: galeri handler'ı (kapak kuralının dört davranışı ve `COMPANY_GALLERY_DUPLICATE_IMAGE`), özgeçmiş uçları ve zararlı yazılım taraması. Dosya adı ve beyan edilen tip kalem değil, karar (Karar 3).

## Açık sorular

1. **Kapak, galeri dışındaki görüntüler için de işaretle mi seçilecek?** Java'nın `JobImage` tarafında aynı `cover` alanı ve aynı `applyDefaultCover` var (`replaceJobGallery`, `removeJobImage`). İlan galerisi Faz 3'ün konusu; bu ADR yalnız şirket galerisini bağlıyor. Faz 3 aynı biçimi almalı — iki galerinin kapak kuralının ayrışması, Java'nın tek yerde yazıp iki yerde kopyaladığı hatanın (koala-backend #362) sözleşme düzeyindeki karşılığı olurdu.
2. **Zararlı yazılım taraması nerede koşacak?** Özgeçmiş artık işverenin makinesinde açılacak iki belge biçimi daha kabul ediyor ve tür saptaması taramanın yerine geçmiyor. Onaylı bir tarayıcı (ClamAV ya da sağlayıcı tarafı) ve koşma yeri — tamamlama adımı mı, sonradan bir River işi mi — ayrı bir karar ister ve Yusuf'un listesinde duruyor. Bu PR'da çözülmedi, izleyen kalem.

Özgeçmişin özgün adı ve `Pipeline.Start`'ın beyan ettiği tip bu listede **değil**: ikisi Karar 3'te karara bağlandı.
