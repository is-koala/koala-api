# ADR-0030: Nesne deposu istemcisi, yükleme hattı ve EXIF temizliği

- Durum: Kabul edildi
- Tarih: 08.11.2026
- Karar kaydı: K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3 (onaylı bağımlılıklar), §3.11 (yapılandırma), §6.1 Faz 2; [ADR-0003](0003-dependency-policy.md) madde 1, [ADR-0007](0007-error-model.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (belirsiz commit), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) §9 (yüklemeler API'den geçmez), [ADR-0023](0023-image-release-pipeline-and-test-fakes.md) (test sahteleri), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) (PII kapısı), ADR-0029 Karar 5 ve 6 (kabul edilen tipler, EXIF, anahtar düzeni, karantina; PR [#17](https://github.com/is-koala/koala-api/pull/17)); Java `origin/main` — `StorageKeys`, `S3StorageService`, `UploadValidator`, `ImageMetadataStripper`, `ExifOrientation`, `IdentityVerificationManager` okundu, koşturulmadı; [koala-backend issue #362](https://github.com/is-koala/koala-backend/issues/362) (EXIF ortak sınıf)
- Dış kaynaklar: AWS SigV4 [presigned URL](https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html) ve [GET response override](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html) belgeleri; `aws-sdk-go-v2` v1.47.4 ve `service/s3` v1.114.5 kaynağı; Exif 2.32 §4.6.4 (TIFF etiketi `0x0112`); PNG 3. baskı, `eXIf` parçası; [OWASP File Upload Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html) (tipi içerikten saptama, SVG ve polyglot)
- Kod: `internal/platform/storage/{key.go,storage.go}`, `internal/platform/imaging/{imaging.go,exif.go}`, `internal/platform/fakes/s3.go`, `internal/media/{domain.go,pipeline.go}`, `internal/platform/config/config.go`, `internal/app/serve.go`, `internal/archtest/config_test.go`; belge: `README.md`, `docs/runbook/yayin.md`

## Bağlam

ADR-0029 Faz 2'nin medya sözleşmesini dondurdu: üç adımlı yükleme, karantina öneki, anahtar düzeni, kabul edilen tipler ve EXIF politikası orada karara bağlandı. Kalan iş o kararların platform katmanıydı — ADR-0029 kendi "Bu PR'da kalanlar" listesinde `internal/media`'nın karantina hattını ve `FuzzStorageKey`'i açıkça bir sonraki PR'a bıraktı.

Bu PR o katmanı yazar. HTTP handler yazmaz, sözleşmeye dokunmaz, `x-not-implemented` iskelelerini kaldırmaz ve Faz 2'nin tablolarını getirmez; `media` tablosu, `db/softdelete.yaml` sınıflandırması ve sqlc sorguları veri PR'ındadır.

Üç şey kod yazılırken ortaya çıktı ve ADR-0029'un verdiği kararların **nasıl** uygulanabileceğini daraltıyor. Bu ADR onları yazıya geçirir:

1. Presigned PUT adresi, imzasıyla içerik tipini ve boyutu **bağlayamıyor**.
2. PDF yeniden kodlanamıyor, dolayısıyla üstverisi sökülemiyor.
3. Depoda nesne deposu sahtesi yok, CI'ın `integration` job'ı tek Postgres service container'ıyla koşuyor.

Java tarafı da iki ders bırakmış. Birincisi, tipi **dosya adının uzantısından** çıkarıyor (`UploadValidator.allowedExtensions`) ve istemcinin ham `Content-Type`'ını hem S3'e hem `media.file_type`'a yazıyor; ADR-0029 Karar 5 bunu bilinçli ayrılık olarak zaten düzeltti. İkincisi, EXIF temizliğini **iki kez** yazmış: `ImageMetadataStripper.strip` ortak sınıfta duruyor, ama `IdentityVerificationManager:289` aynı işi satır içinde tekrar kuruyor. Issue #362 bunun hakkında ve bu PR'ın tek yerde toplama kuralının gerekçesi.

## Karar

### 1. Yazma adresi tek anahtarı kapsar; tipi ve boyutu kapsayamaz

`internal/platform/storage` nesne deposu istemcisidir: presigned PUT/GET, head, sınırlı get, put, copy, delete ve "copy sonra delete" anlamına gelen `Move`.

**Bağımlılık `github.com/aws/aws-sdk-go-v2/service/s3` v1.47.4/v1.114.5'tir.** ADR-0003 madde 1'in onaylı listesinde `aws-sdk-go-v2/s3` var, dolayısıyla ayrı onay istemez; Java da aynı SDK'yı kullanıyor (`S3StorageService`), yani sektör standardı seçimi zaten verilmiş durumda. `minio-go` listede değil ve ayrı onay isterdi; SigV4'ü elle yazmak ise bir imzalama kütüphanesini kendimiz bakmak anlamına gelirdi. Sürümler yayımlanalı en az on dört gün olmuş en yeni sürümlerdir (s3 v1.114.5: 22.10.2026; çekirdek v1.47.4: 22.10.2026; smithy-go v1.28.6: 21.10.2026). Kimlik sağlayıcısı `aws.CredentialsProviderFunc` + `aws.NewCredentialsCache`'tir, böylece `credentials` modülü grafiğe girmez; sır bir closure'da durur ve `Store`'un onu yazdıran metodu yoktur.

**Bulgu:** `aws-sdk-go-v2`, presigned `PutObject` üretirken `Content-Type` başlığını isteğin içinden **siler** (`api_op_PutObject.go`, `awshttp.RemoveContentTypeHeader`), ve SigV4 imzalayıcısı `Content-Length`'i yalnız imzalanan isteğin **gövdesi varsa** imzalar (`buildCanonicalHeaders`); presign'da gövde yoktur. Yani üretilen adres metodu, bucket'ı, anahtarı ve son kullanma anını kapsar; içerik tipini ve boyutu kapsamaz.

**Karar:** Adres, istemcinin **hangi anahtara** yazabileceğini sınırlar; **ne yazabileceğini** sınırlamaz. Gerçek denetim tamamlama adımında, nesnenin kendisine karşı yapılır (Karar 3 ve 5). ADR-0029 Karar 6 doğrulamayı tamamlama adımına zaten koymuştu; bu bulgu o adımın isteğe bağlı bir ikinci savunma değil, **tek** savunma olduğunu söylüyor. `StartMediaUploadRequest.sizeBytes` bu yüzden depolama katmanında hiçbir şeyi bağlamaz — sözleşmede "bildirilen boyut" demesi doğru.

Bir testi bu bulguya bağladık: sahte depo her isteğin imzaladığı başlık listesini kaydeder, ve `TestPresignPutAddressesOneKey` listede `content-type` ya da `content-length` görürse kırmızı olur. SDK davranışı değişirse haber alırız.

İki yan karar:

- **`X-Content-Type-Options` presigned GET'e konamaz.** S3'ün yanıt geçersiz kılmaları kapalı bir listedir (`response-content-type`, `-content-disposition`, `-cache-control`, `-content-encoding`, `-expires`) ve o başlık listede yok. Bu yüzden nesne **yazılırken** `Content-Disposition: attachment` ile yazılır (Java da böyle yapıyor), ve `nosniff` API'nin kendi 302'sinde taşınır (ADR-0029 Karar 6).
- **Checksum hesabı `WhenRequired`'a sabitlendi.** SDK'nın varsayılanı (`WhenSupported`) her yazmaya CRC32 başlığı ekler ve geri saramadığı gövdede isteği `aws-chunked` kodlamasına çevirir. Koala yalnız AWS'ye değil S3 uyumlu depolara da konuşuyor ve protokolün en çok farklı uygulanan parçası budur. `WhenRequired` ayrıca doğrudan yazma ile presigned yazmayı aynı davranışa getirir: presign yolu bu değeri kendisi zorluyor.

`MaxSignedTTL` bir saattir. SigV4 yedi güne izin verir; Koala'nın adresi tek istemciye tek yükleme ya da tek indirme için verilir, uzun ömürlüsü hata olur. Yükleme adresi 15 dakika (`media.UploadTTL`), personel indirmesi 3 dakikadır (ADR-0029 Karar 6).

#### Kabul edilen risk: yükleme boyutu adreste bağlı değil

Yukarıdaki bulgunun sonucu bir **kabul edilmiş risktir**, açık soru değil. Faz 2 koordinasyonunda 08.11.2026'da karara bağlandı: **presigned PUT kalıyor.**

- **Risk.** Yazma adresini alan istemci, karantina anahtarına istediği kadar büyük bir nesne yazabilir. Boyut sınırı yalnız tamamlama adımında, nesne zaten yazılmışken ölçülür.
- **Neden kabul ediliyor.** Adres `Start` çağrısından geliyor, yani yazan taraf **kimlikli** ve o çağrı oran sınırlıdır (`x-rate-limit: upload`). Nesne karantina önekinde duruyor ve `incoming/` hiçbir genel adrese çözülmüyor (`Store.PublicURL`), yani yazılan şey kimseye servis edilmiyor. Tamamlama adımı tipi **içerikten** saptıyor ve boyutu `Head` ile ölçüyor, sınırı aşan nesne `REJECTED` olup siliniyor. Tamamlanmayan yükleme süpürme kuralıyla gidiyor. Kalan zarar tek bir yüklemenin geçici depolama maliyeti.
- **Neden alternatif seçilmedi.** Boyutu ve tipi gerçekten imzalayan tek yol S3 POST policy'dir (`content-length-range` ve `Content-Type` koşulları imzaya girer). İki nedenle bu aşamada kötü takas: (a) yanıtta form alanları gerektirir, yani **yeni donmuş** `MediaUpload` şemasını değiştirmek demektir; (b) POST policy, S3 uyumlu depoların (MinIO, R2) en çok ayrıştığı yüzeydir. İkisi birlikte, ölçülmüş bir kötüye kullanım olmadan alınacak bir risk değil.
- **Tetikleyici.** Kötüye kullanım görülürse — karantinada anormal boyutlar, süpürmenin temizlediği hacmin artması, maliyet sinyali — POST policy'ye geçilir ve `MediaUpload` şeması o zaman form alanları taşır. Bu bir **sözleşme değişikliğidir** ve kendi ADR'sini ister.

### 2. Anahtar düzeni `internal/platform/storage`'ta, tek doğrulama noktasıyla

Anahtar dili ADR-0029 Karar 6'nın dilidir ve burada tekrar edilmez. Uygulama ayrıntıları:

- `NewKey` **yazma** yoludur: önek medya türünden, kimlik sunucunun ürettiği **UUIDv7**'den, uzantı **saptanan tipten** gelir. İstemci dosya adı veremez ve anahtara hiçbir istemci girdisi girmez.
  - Düzeltme: bu ADR'nin ilk hâli "dosya adı yalnız metaveri olarak taşınır" diyordu. **Taşınmıyor.** Donmuş `StartMediaUploadRequest` yalnız `kind` ve `sizeBytes` taşıyor, yani istemci özgün adı **gönderemiyor**; depoda ad diye bir alan yok ve `Content-Disposition` çıplak `attachment` olarak yazılıyor. Görünür sonucu: kullanıcı kendi CV'sini `0192f3c4-…-6a30.pdf` adıyla indiriyor. Java hem ASCII hem RFC 5987 biçimini yazıyor ve adı satıra kaydediyordu; bu bir **daralmadır** ve Sonuçlar'da kalem olarak duruyor. Özgün ad isteniyorsa `StartMediaUploadRequest`'e isteğe bağlı bir alan gerekir — uyumlu bir ekleme, acelesi yok.
- `ParseKey` **okuma** yoludur ve tek bir noktada laxtır: Java'nın yazdığı **UUIDv4** anahtarları kabul eder. Bu asimetri bilinçlidir — eski nesneler okunabilir kalmalı, ama yeni anahtar zaman sıralı olmalı.
- `Key` bir **struct**'tır, string değil. Sıfır `Key` hiçbir nesneyi adlandırmaz ve her depo metodu onu reddeder. Dolaşımdaki her `Key` ya `NewKey`'den ya `ParseKey`'den geldiği için, anahtarı tutan kodun ikinci bir denetim yazması gerekmez; "stringden anahtar üreten" ikinci bir yol depoda yoktur.
- `WithPrefix` ve `WithExt` karantinadan son yere taşımanın tek yoludur: kimlik ve uzantı taşınır, önek değişir, dolayısıyla taşıma önek kümesinin dışına çıkamaz.

**Tek çözme turunun bir sonucu yazıya geçiyor**, çünkü boşluk gibi görünüyor ve değil: iyi biçimli bir anahtarın **tek kat kodlanmış** hâli kabul edilir, ayırıcısı kodlanmış olanı dahil (`images%2f<uuid>.jpg`). Bu güvenlidir, ve gerekçesi izin listesinin kendisidir: tek çözme turundan sonra sonuç desene **birebir** uymak zorundadır, dolayısıyla `images/<uuid>.jpg`'e çözülür, kendi önekinin içinde kalır, ve `FuzzStorageKey` bunu kabul edilen her anahtar için denetler. Kuralın kapattığı şey ikinci turdur: `%252f` bir tur sonra `%2f` olur, içinde hâlâ yüzde işareti vardır ve desene hiç ulaşmadan reddedilir — yani hiçbir çağırana, sonraki bir çözücünün başka bir anahtara çevirebileceği bir string verilmez. Testte bu iki yön ayrı ayrı sabitlendi (`TestParseKeyDecodesExactlyOnce`).

**`ParseKey`'in gerçek yüzeyi, çıkış kriterinin ima ettiğinden dardır** ve bunu yazıya geçirmek kriterden fazla güven çıkarılmasını önler. Donmuş sözleşmede istemciden **anahtar alan hiçbir uç yok**: medya uçları UUID alıyor, personel indirmesi inceleme ve belge kimliği alıyor, ve ADR-0029 Karar 6 inceleme yanıtının hiçbir depolama anahtarı taşımadığını söylüyor. Dolayısıyla `ParseKey`'in bugünkü girdisi istemci değil, `media` satırından okunan ve `NewKey`'in yazdığı değerdir: işlevi bir yol geçişi savunmasından çok bir **veritabanı değeri doğrulayıcısıdır** — "bu satırdaki string gerçekten bizim yazdığımız alfabeden mi" sorusunun cevabı. Faz 2'nin "yol geçişi fuzz'ı" kriteri karşılanıyor ve savunma, istemciden anahtar alan bir uç gelirse (eski Java anahtarlarının göç yolu, bir yönetim aracı) hazır duruyor; ama bugün kapattığı delik, satırın bozulması ya da elle düzenlenmesidir.

Aynı dürüstlükle: fuzz değişmezleri **kabul** yolunu bağlıyor — kabul edilen anahtarın neye benzediğini. Reddetme tarafını `TestParseKeyRejects`'in 44 satırlık tablosu taşıyor. Yani kriter tek bir hedefe değil, **bir hedef artı bir tabloya** dayanıyor; tablodan bir satır düşerse fuzz bunu yakalamaz.

**Yer, ADR-0029'un yazdığından farklı.** ADR-0029 `FuzzStorageKey`'i `internal/media`'ya koymuştu. Anahtar sözdizimi depolama kaygısıdır: istemci onu imzalı adresi kurarken ve okuma yolunu doğrularken kullanır, ve `internal/platform/storage` onu bilmeden kendi API'sini tip güvenli kuramaz. Hedef bu yüzden `internal/platform/storage`'ta. Önek **adları** (`identity-documents/` gibi) bir alan bilgisi gibi görünebilir; bunlar depo yerleşimidir, iş kuralı değildir, ve kural (hangi türün hangi öneke gittiği) `internal/media/domain.go`'da durur — platform paketi alan paketini import etmez (depguard `platform`).

**`FuzzStorageKey` iki değişmezi sınar** (ADR-0029 Karar 6'nın istediği):

1. Kabul edilen her anahtar, kendi yazdığı biçimden tekrar ayrıştırılınca aynı anahtarı verir — yani yazdığı biçim kabul edilen dilin içindedir.
2. Kabul edilen hiçbir anahtar kendi önekinin dışına çıkmaz: `path.Clean` ile aynıdır, önekinin bildirdiği kadar parçaya sahiptir, ve içinde `%`, ters eğik çizgi, NUL, `..`, `?`, `#`, `:`, boşluk ya da ASCII dışı bayt yoktur.

Tohumlar `F.Add` ile verilir, `testdata/fuzz` korpusu ile değil: böylece kapının `verify` job'ında `go test` (yani `-fuzz` olmadan) **her tohumu koşturur** ve `go test -fuzz=FuzzStorageKey` onlardan başlar. Sınıflar: her önek ve uzantının doğru hâli, UUIDv4 eski anahtar, `..`/`.`/boş parça, tek ve çift (ve üç kat) yüzde kodlama, kesik ve geçersiz kaçış, ters eğik çizgi, NUL ve denetim karakterleri, Unicode normalleştirme (birleşik ve ayrışık biçim, tam genişlik `/`, noktasız `ı`), aşırı uzun ad, büyük/küçük harf, sorgu ve parça kaçırma, tam URL.

### 3. Tip içerikten saptanır; tablo elle yazılır, `DetectContentType` kullanılmaz

`internal/platform/imaging.Detect` sihirli bayt tablosudur ve izin listesi üç tiptir: `image/jpeg`, `image/png`, `application/pdf`.

**`net/http.DetectContentType` kullanılmadı.** O fonksiyon otuz kadar tipten birini döner — `text/html` ve `image/svg+xml` dahil — yani çağıranın cevabını zaten bir izin listesine çevirmesi gerekir, ve Koala'nın kabul etmediği bir tip o dönüşüme makul görünen bir değer olarak ulaşır. Elle yazılan tablo incelenebilir ve kapalıdır.

- JPEG: `FF D8 FF`. JFIF ve Exif çeşitleri sonrasında ayrışır, bu yüzden yalnız üç bayt sabittir.
- PNG: sekiz baytlık imza.
- PDF: `%PDF-`, **offset 0'da**. Şartname ilk 1024 bayt içinde olmasına izin verir; polyglot bir PDF'i tam olarak böyle, başka bir biçimin başlığının arkasına saklar. Koala offset 0 ister.

**Polyglot iki vakaya ayrılır ve farklı cevaplanır:**

- **Sihirli bayt bir biçimi, gövde başka biçimi söylüyorsa reddedilir** — ama hangi denetimin reddettiği önemlidir ve ilk yazımda yanlış yazılmıştı. JPEG başlığının arkasına konmuş tam bir PNG, **çözücünün kendisi** tarafından düşer: `Detect`'in JPEG öneki (`FF D8 FF`) `image/jpeg`'in kayıtlı iki baytından katıdır ve PNG imzaları aynıdır, yani `Detect`'in kabul ettiği her dosya aynı biçim olarak sniff edilir. Dolayısıyla böyle bir dosya `ErrCorrupt` ile düşer (hatta `RejectionCorrupt` olarak), `ErrUnsupportedType` ile değil. `checkFormat` bu ikisinin ayrıldığı hâli yakalar ve bugünün tablolarıyla **tetiklenemez**: iki tablonun birbirinden ayrılmasına karşı bir korumadır, canlı bir yol değil. Kodun yorumu bunu söylüyor; bu ADR de artık söylüyor. Sonuç her iki durumda aynı: nesne yazılmaz, yani birinci biçimin uzantısıyla ikinci biçimi taşıyan bir nesne oluşmaz.
- **Geçerli bir JPEG'in sonuna eklenmiş veri kabul edilir ve yok olur.** Dosya geçerli bir JPEG'dir; EOI'den sonraki HTML, SVG ya da betik yeniden kodlamadan sağ çıkmaz. Savunma reddetmek değil, yeniden kodlamaktır.

**SVG izin listesinde hiç yok.** SVG bir belgedir, raster değil: betik, dış varlık ve `xlink` taşır, ve onaylı bağımlılıklar arasında onu güvenle temizleyecek bir ayrıştırıcı yok. Kabul edilmesi gerekirse ayrı bir ADR ve ayrı bir bağımlılık onayı ister.

### 4. EXIF: yeniden kodlayarak tümden sök, yalnız yönü önce piksellere pişir

Politika ADR-0029 Karar 5'in politikasıdır. Bu PR onu **tek yerde** uygular ve iki noktada Java'yı düzeltir.

**Silinen:** her APP1/EXIF bölütü, XMP, IPTC, PNG metin parçaları (`tEXt`, `zTXt`, `iTXt`), PNG `eXIf` parçası, renk profili (`iCCP`, `gAMA`, `sRGB`), küçük resim (thumbnail), üretici ve model, seri numaraları, tarih damgaları ve **GPS enlem/boylam**.

Mekanizma listeyi tek tek silmek **değil**: görüntü çözülür ve standart kütüphanenin kodlayıcılarıyla yeniden yazılır, o kodlayıcılar da üstveri taşıyacak bir bölüt hiç yazmaz. Doğrulandı (Go 1.27 kaynağı): `image/png` yalnız `IHDR`, `PLTE`, `tRNS`, `IDAT` ve `IEND` yazar; `image/jpeg` yalnız SOI, DQT, SOF0, DHT, SOS ve EOI yazar — **hiçbir `APPn` bölütü yazmaz**, JFIF `APP0` dahil. Yani çıktıda üstverinin durabileceği bir yer yoktur ve listeyi eksik saymak mümkün değildir. Bu, listeyi sayan bir silme işleminden (Java'nın yaklaşımı değil ama yaygın olan) niteliksel olarak farklıdır: orada yeni bir bölüt tipi listeye eklenmeyi bekler, burada beklemez.

**Korunan:** yalnız **yön etiketi** (TIFF `0x0112`), ve o da saklanmaz. Önce okunur, piksellere uygulanır (görüntü döndürülür/aynalanır), sonra yeniden kodlamayla o da gider. Gerekçe: yön, fotoğraf **hakkında** bir üstveri değil, fotoğrafın **nasıl gösterileceğinin** parçasıdır — telefon sensörün rasterini yazar ve "bunu çevir" der. Uygulamadan atmak her dikey fotoğrafı yan saklamak demektir; okuyup pişirmek sektör pratiğidir. Çıktı dik durur ve ikinci bir geçiş hiçbir etiket bulamaz, yani dönüşüm iki kez uygulanamaz.

**Konum için kural en katı hâliyle uygulanır.** Projenin ilan konum kuralı "şirkette tam, bireyselde yaklaşık, haritada hep yaklaşık" diyor. Yüklenen bir fotoğrafın GPS'ini saklamanın hiçbir iş gerekçesi yok, kimlik belgesi fotoğrafı için ise açıkça zararlı: kullanıcının evini söyler. Dolayısıyla konum **hiçbir** medya türünde tutulmaz, ve test bunu bir GPS işaretçisinin kaybolmasıyla kanıtlar.

**Java'dan iki düzeltme:**

- **Tek yer.** Java'da `ImageMetadataStripper.strip` ortak sınıfta duruyor ama `IdentityVerificationManager:289` aynı işi satır içinde tekrar kuruyor (issue #362); bir tarafa yapılan düzeltme öbürüne geçmiyor. Go'da `imaging.Sanitize` tek uygulamadır ve her yükleme yolu ondan geçer.
- **PNG'de de yön okunur.** Java yön etiketini yalnız JPEG'den okuyor. PNG 2017'den beri `eXIf` parçası taşıyabiliyor; Java'da o parça atılırken yön de kayboluyordu. Go iki biçimi de okur, ikisi de aynı TIFF okuyucusuna gider.

**PDF üstverisi sökülmez — kabul edilen sınır.** Bir PDF yeniden kodlanamaz; onaylı bağımlılıklar arasında PDF kütüphanesi yok (ADR-0003) ve standart kütüphane PDF bilmez. Dolayısıyla CV'nin belge üstverisi (yazar, üretici, oluşturma tarihi) **olduğu gibi** kalır.

Bu bir kabul edilmiş sınırdır, açık soru değil; Faz 2 koordinasyonunda 08.11.2026'da karara bağlandı: **Faz 2'de sökülmeyecek.**

- **Gerekçe.** Bir PDF kütüphanesi eklemek bu PR'ın kapsamını aşar ve ayrı onay ister (ADR-0003 madde 3). KVKK açısından zorlayıcı bir gerekçe de görülmedi: özgeçmişin yazar alanındaki ad, kullanıcının kendi koyduğu ve zaten işverene gösterilen veridir — EXIF'teki GPS gibi, kullanıcının **farkında olmadan** verdiği bir veri değil. Ayrımın kendisi EXIF politikasının gerekçesidir (Karar 4'ün konum paragrafı) ve PDF'i onun dışında tutan şeydir.
- **Azaltma.** Nasıl sunulduğu: PDF `Content-Disposition: attachment` ve saptanan `Content-Type` ile saklanır, `no-store` ile işaretlenir, **genel bir adrese hiç çözülmez** (`files/` öneki `Store.PublicURL`'den `false` döner) ve yalnız kısa ömürlü imzalı adresle verilir. Sınır testte açıkça kayıtlıdır (`TestCompleteCVMovesThePDFAsItIs`), yani sessiz değil.
- **Tetikleyici.** İki durumda yeniden değerlendirilir: onaylı listeye bir PDF kütüphanesi girerse, ya da PDF'ler genel adrese çözülür hâle gelirse (o anda azaltmanın yarısı düşer). Yusuf'a bilgi olarak geçilecek.

#### Bayt yürüyücüleri fuzz'lanır

Yön okuyucusu üç elle yazılmış bayt yürüyücüsünden geçer: `jpegExifPayload` (JPEG bölüt zinciri), `pngExifChunk` (PNG parça zinciri) ve `tiffOrientation` (TIFF IFD'si). Üçü de **dosyadan gelen** uzunlukları, offset'leri ve giriş sayısını indeks olarak kullanır, ve bu katmanda girdisi doğrudan kullanıcıdan gelen tek yer burasıdır: bir panik, doğrudan servis dışı bırakmadır.

Sınır denetimleri yazıldı ve tablo testleri onları sınıyor — ama tablo testi yazarın **düşündüğü** sınırları sınar. Bu yüzden üç fuzz hedefi var:

- **`FuzzOrientation`** üç yürüyücüyü birlikte sürer. Değişmezler: panik yok, cevap her zaman EXIF aralığında (1–8; aralık dışı bir değer `applyOrientation`'da hedef rasterin dışını indeksler), görüntü olmayan tip hiç etiket taşımaz, ve okuyucu saftır (aynı girdiye iki kez aynı cevap).
- **`FuzzDetect`** izin listesinin kapalılığını sürer: kabul edilen tip her zaman `Types()`'ta, reddedilen her zaman boş, ve geçerli bir başlığın **sonuna** veri eklemek tipi değiştiremez.
- **`FuzzSanitize`** hattı uçtan uca sürer: kabul edilen çıktı aynı biçimde, verilen sınırların içinde, yön etiketi olmadan gelir ve ikinci geçiş onu değiştirmez.

Tohumlar (`F.Add`) yürüyücülerin aritmetiğinin ortasına nişan alır: sonu aşan uzunluk, kendi uzunluk alanından küçük uzunluk, yürüyüşü ilerletmeyebilecek sıfır uzunluk zinciri, kendi başlığının içine ya da sonun ötesine işaret eden IFD offset'i, veriden çok daha büyük giriş sayısı, uzunluk taşımayan işaretçiler (RST, TEM, ikinci SOI), SOS'tan sonra devam etmeyi deneyen girdi, `count ≠ 1` ve yanlış tipli yön etiketi, Exif başlığı olmayan `eXIf` yükü, ve gerçek görüntüler (üstverili ve üstverisiz) ile CRC'si onarılmış bir dekompresyon bombası.

`FuzzSanitize`'ın üstveri denetimi **yapısaldır** (etiketi geri okur), `"Exif"`/`"eXIf"` gibi alt dizge araması değil: sabit fikstürde o arama kesindir ve `TestSanitizeDropsEveryMetadataSegment` onu orada yapar, ama fuzz çıktısının sıkıştırılmış görüntü verisi rastgele bayttır ve dört baytlık bir dizi orada tesadüfen çıkıp kapıyı sebepsiz kırmızı yapabilir. Kararsız bir kapı testi, bir eksik iddiadan kötüdür.

### 5. Sınırlar ve varsayılanlar

| Sınır | Varsayılan | Nereden |
| --- | --- | --- |
| Dosya boyutu | 10 MiB | ADR-0029 Karar 5 (Java'nın değeri) |
| Tuval (genişlik × yükseklik) | 50 MP | ADR-0029 Karar 5 (Java'nın değeri) |
| JPEG yeniden kodlama kalitesi | 85 | bu ADR |
| Yükleme adresi ömrü | 15 dakika | bu ADR |
| İmzalı adres üst sınırı | 1 saat | bu ADR |
| Karantina saklama süresi | 24 saat | ADR-0029 Karar 6 (M2) |

`imaging.Limits`'in sıfır alanı **varsayılan** anlamına gelir, "sınırsız" anlamına **gelmez**; isteğe bağlı sınırlar taşıyan bir struct'ın tipik hatası budur ve bir test onu kapatır. Sınırlar türe göre yapılandırılabilir (`media.Kind.Limits()`); bugün her tür varsayılanı kullanır, ama kendi sınırı gereken tür onu orada alır, çağrı yerinde almaz.

**Kalite 85'in gerekçesi:** yeniden kodlama kayıplıdır ve kaçınılacak yolu yok — üstveriyi, entropi kodlanmış veriyi koruyarak sökmek bir JPEG ayrıştırıcısı yazmak demektir, ve sökme tam olmak zorunda. 85 tek seferlik bir dönüştürmede yerleşik seçimdir; Go'nun kendi varsayılanı 75'tir ve kimlik belgesi fotoğrafında gözle görülür şekilde daha kötüdür.

**Dekompresyon bombası tuvalden kapanır.** Tuval sınırı görüntünün **başlığından** okunur (`image.DecodeConfig`), tam çözmeden önce. 900 megapikseli bildiren ama 4×4'ün verisini taşıyan bir PNG, rasteri hiç ayrılmadan düşer; test bunu başlığın CRC'sini de düzelterek, yani gerçekten tuval sınırına çarptırarak kanıtlar.

**Zaman aşımı:** CPU işi `MaxBytes` ve `MaxPixels` ile sınırlıdır, bir zamanlayıcıyla değil — bir çözme işlemi ortasından kesilemez. `Sanitize` adımlar **arasında** `ctx`'i denetler, yani iptal edilen bir iş sonraki sınırda durur. Depo çağrılarının süre sınırı çağıranın context'idir. Bu dürüst sınır ADR'ye yazılıyor ki "zaman aşımı var" sanılmasın.

### 6. Nesne deposu sahtesi yazıldı; CI'a servis container eklenmedi

Depoda nesne deposu sahtesi, docker-compose dosyası ve MinIO yoktu. `internal/platform/fakes` ise ADR-0023'ün kurduğu kalıptır: dış HTTP servisinin yerine `httptest` sunucusu.

**Karar:** `fakes.S3`, süreç içi S3 uyumlu sahte. Gerekçe ve maliyet hesabı:

- CI'ın `integration` job'ı tek Postgres service container'ıyla koşuyor. İkinci bir servis container eklemek kapıyı **her PR için** değiştirir, job'ı yavaşlatır ve yeni bir imaj digest'i sabitlemek gerektirir.
- Koala'nın kullandığı depo yüzeyi altı operasyondur. Süreç içi sahte onu kapsar, birim test job'ında koşar, Docker istemez ve yerelde ağır iş yaratmaz (CLAUDE.md).
- Sahtenin karşısındaki istemci **gerçek** istemcidir: `storage.Store`, gerçek AWS SDK'sıyla, gerçek SigV4 imzalayarak. Yani test edilen kod üretimde koşan koddur; bellek içi store yazılmadı (CLAUDE.md).

**Sahte imzayı doğrulamaz.** İsteğin imzalı **geldiğini** denetler (SigV4 `Authorization` başlığı ya da `X-Amz-Signature` sorgu parametresi) ve her imzanın nasıl atıldığını kaydeder: presigned mi, ömrü ne, hangi başlıkları kapsıyor. İmzanın doğruluğunu kanıtlamak SigV4'ü yeniden yazmak, yani Koala'yı değil sahteyi test etmek olurdu. Kaydettiği imza bilgisi Karar 1'in testini mümkün kılan şeydir.

Gerçek MinIO'ya karşı bir `//go:build integration` testi **bu PR'da yazılmadı**: kapıyı değiştirmemek kararının diğer yüzü budur. Sonuçlar'da kalem olarak duruyor.

### 7. Yapılandırma fail-closed; red bir hata kodu değil, bir değer

**Yapılandırma** `internal/platform/config` kalıbını izler: yedi `KOALA_OBJECT_*` değişkeni `known` listesinde, biçimi `Load`'da denetlenir, ve `serve`'ün hangi durumda açılamayacağı `ServeStorageErrors`'ın kararıdır — böylece `migrate` ve `bootstrap` hiç nesne deposu yapılandırılmadan koşmaya devam eder (ADR-0025'in `ServeLoginErrors` kalıbı).

Fail-closed iki yöne işler:

- **Yarım yapılandırma her ortamda hatadır.** Yarım yapılandırma bir yazım hatası ya da yarım kalmış bir dağıtımdır; onunla açılmak her yüklemeyi, kullanıcı ilk denediği anda 500'e çevirir.
  - **Düzeltme:** bu kuralın ilk hâli `KOALA_OBJECT_PUBLIC_BASE_URL`'i zorunlu kümenin dışında bırakıyordu ve gerekçesini "500'e çevirir" diye veriyordu — ama o değişkenin eksikliği 500 vermez. `publicer` boş kalır, `PublicURL` her anahtara "yok" der, ve sözleşmenin *genel okunabilen türlerde ve hazır durumda **dolar*** dediği `Media.url` her hazır profil fotoğrafı, logo ve galeri görselinde `null` olur. Yani servis açılır, 200 döner ve sözleşmenin verdiği sözü **sessizce** bozar — bu, kuralın kapatmak istediği şeyin daha kötü hâli. Değişken artık zorunlu kümede.
- **Eksik yapılandırma `staging` ve `production`'da `serve`'ü durdurur**, çünkü yükleme uçları orada canlıdır. `local` ve `test`'te serbesttir, böylece yükleme üzerinde çalışmayan bir geliştiriciye bucket gerekmez.

Bucket adının alfabesi, anahtarın alfabesiyle aynı gerekçeyle kapalıdır: istek yoluna giriyor. Endpoint ve genel temel adres mutlak `http`/`https` origin olmak, kimlik bilgisi, sorgu ve parça taşımamak zorundadır, ve **yükleme uçlarının canlı olduğu her ortamda `https` olmak** zorundadır — yani `staging`'de de, çünkü `ServeStorageErrors` staging'i canlı sayıyor: endpoint her imzalı adresin imzasını taşır, genel temel adres ise her istemciye verilir, ve ikisi de düz metinden geçmemelidir. Düz `http` yalnız `local` ve `test`'te serbesttir, loopback'teki bir MinIO için. Sır `config.Secret`'tır, yani yapılandırmanın log'a düştüğü her yerde yer tutucu basar, ve hiçbir hata mesajı bir **değeri** yansıtmaz.

**Red bir `apperr.Code` değil, bir değerdir.** `media.Rejection` (`NOT_UPLOADED`, `TYPE_UNSUPPORTED`, `TOO_LARGE`, `CORRUPT`) doğrulamanın **iş sonucudur**: sözleşme onu medya kaynağında `rejectionCode` olarak taşıyor, yani satır nedeni kaydeder ve istemci onu tek bir yanıttan değil kaynaktan okur. `Complete`'in `error` dönüşü yalnız altyapı hatasıdır.

Bu PR hiçbir `apperr.Code` sabiti **tanımlamaz**. Medya kodları (`MEDIA_NOT_FOUND`, `MEDIA_NOT_READY`, `MEDIA_UPLOAD_INCOMPLETE`, `MEDIA_TYPE_UNSUPPORTED`, `MEDIA_TOO_LARGE`) ADR-0029'un sözleşmesinde donduruldu; `Rejection` → `apperr.Code` eşlemesi handler PR'ındadır. Bu PR'ın ilk hâli yazıldığında o kodlar bu dalın `api/components/problem.yaml`'ında yoktu ve burada tanımlamak sözleşme testini (`internal/app/api_test.go`) kırardı; ADR-0029 merge edildikten sonra kodlar `main`'de, ama eşlemenin yeri değişmiyor — handler'ı olmayan bir koda sabit tanımlamak `Codes()` listesini gerçekte dönülmeyen bir kodla doldururdu.

Handler PR'ının bağlaması gereken medya alanı kodları, ADR-0029 merge edildikten sonraki hâliyle: yukarıdaki beş `MEDIA_*`, `IDENTITY_*` kümesi, ve galeri kodları `COMPANY_GALLERY_LIMIT_REACHED` ile `COMPANY_GALLERY_DUPLICATE_IMAGE`. ADR-0029'un *Sonuçlar* bölümü ayrıca ilk handler PR'ından operasyonun `x-error-codes` listesi ile handler'ın ürettiği sabitler arasındaki bağı kurmasını istiyor; `Rejection` eşlemesi o denetimin kapsamına girer.

**Soft-delete ve PII:** bu PR hiç tablo, migration ya da sorgu getirmez, dolayısıyla `db/softdelete.yaml` kataloğuna ve `internal/platform/softdelete`'in JOIN lint'ine değmez. `media` tablosu, sınıflandırması ve fikstürü veri PR'ındadır; kimlik belgesi kaydının ADR-0026'nın PII kapısına bağlanması da (erişim kaydı yazılamazsa veri açılmaz) o PR'ın işidir. Bu PR'ın o yöndeki tek katkısı yapısaldır: `identity-documents/` öneki hiçbir genel adrese çözülmez (`Store.PublicURL` onu `false` döner), yani IDN-007 bir çağıranın dikkatine bırakılmaz.

## Zorunlu ops adımı: karantina yaşam döngüsü

Karar 1'in kabul edilen riskini kapatan şey karantinanın boşalmasıdır, dolayısıyla bu bir "sonra yapılır" kalemi değil, **dağıtımın koşuludur**:

1. **`incoming/` önekinde nesne yaşam döngüsü kuralı bulunur.** Bucket'ta, önek `incoming/` için, 1 günde süresi dolan nesneleri silen bir yaşam döngüsü (lifecycle) kuralı tanımlanır. Bu, uygulamadan bağımsızdır: River işi hiç koşmasa, uygulama hiç açılmasa bile depo kendini temizler. S3, MinIO ve R2'nin üçü de nesne yaşam döngüsünü destekler.
2. **Uygulama tarafındaki süpürme işi buna ek, yedek değil alternatif değildir.** River işi `media` satırlarını da `PENDING`'den çıkarmak zorundadır (satır nesneden bağımsız yaşar); yaşam döngüsü kuralı yalnız nesneyi siler. İş `media` tablosunu istediği için handler PR'ındadır, ama **yaşam döngüsü kuralı onu beklemez.**
3. **Üretim ve staging'de bu kural olmadan yükleme uçları açılmaz.** `serve`'ün bunu makineyle doğrulaması mümkün değil (bucket politikası uygulamanın göremediği bir şey), bu yüzden kural `docs/runbook/yayin.md`'nin "Yusuf'un yapması gerekenler" listesinde ve env tablosunda **yazılıdır**, ve dağıtım incelemesinde sorulur. Makine zorlayamıyorsa yazılı olması gerekir — ve bu ADR'nin ilk hâli bu cümleyi yazıp dosyayı güncellememişti, yani kendi yükümlülüğünü ihlal etmişti.
4. **Değişkenlerin belgede olduğunu bir test zorlar.** `KOALA_OBJECT_*`'ların README ve runbook'tan eksik kalması tam olarak yukarıdaki kusurdu: kuralı kod zorluyor (`ServeStorageErrors` dört değişkeni staging ve production'da zorunlu yapıyor), belge sessiz kalıyor, ve sıradaki dağıtım açılışta düşüyor. `internal/archtest`'teki `TestEveryVariableIsDocumented` artık `config.Variables()`'ın her adının README'de ve dağıtım runbook'unda yazılı olduğunu, `TestDocumentedVariablesExist` de ters yönü (belgenin artık okunmayan bir değişkeni adlandırmaması) denetliyor. İkisi olmadan bu kusur tekrar eder.

ADR-0029 Karar 6 karantinanın saklama süresini **M2, 24 saat** olarak koydu; buradaki kural o kararın ops karşılığıdır, süresini değiştirmez.

## Java'dan kayda geçen daralmalar

ADR-0029 Java'dan bilinçli ayrılıkları uç granülerliğinde sayıyor ve bu, düşen **alanları** yakalamıyor — kendi tarif ettiği kusur. Aşağıdakiler bu PR'ın uygulamasında ortaya çıktı ve burada kayda geçiyor.

1. **CV'de `doc` ve `docx` düştü; PDF-only kalıyor.** Java `CvManager` `FolderType.FILE` kullanıyor ve izin verilen uzantılar `{pdf, doc, docx}`; bugünkü `PUT /users/me/cv` .doc ve .docx kabul ediyor. Go yalnız PDF kabul ediyor, çünkü sözleşmenin `MediaContentType` enum'u üç değer taşıyor (ADR-0029) — ama daralma hiçbir ADR'de geçmiyordu.
   - **Karar: PDF-only kalıyor** (Faz 2 koordinasyonu, 08.11.2026). Sektör pratiği bu, ve .doc/.docx'in güvenli ayrıştırılması ile üstverisinin sökülmesi ayrı bir iştir — OOXML bir zip arşividir, ikili `.doc` ise ayrı bir biçim; ikisi de bu PR'ın yaptığı işi (içerikten tip saptama + yeniden kodlayarak strip) desteklemez.
   - Enum'a değer eklemek **uyumlu** bir değişikliktir, yani karar geri alınabilir: istemcinin kabul ettiği tip kümesi büyür, var olan hiçbir yanıt bozulmaz. Yusuf'a bilgi olarak geçiliyor.
2. **`Content-Disposition` dosya adını kaybetti.** Karar 2'nin düzeltme maddesinde ayrıntısı var: Java hem ASCII hem RFC 5987 biçimini yazıyor ve adı satıra kaydediyor; Go çıplak `attachment` yazıyor ve donmuş istek şeması adı taşımıyor. Özgün ad isteniyorsa `StartMediaUploadRequest`'e isteğe bağlı bir alan gerekir — uyumlu ekleme. Yusuf'a UX notu olarak geçiliyor.

## Yerine geçtiği

1. **ADR-0029'un *Sonuçlar* bölümündeki, `FuzzStorageKey`'i `internal/media`'ya koyan cümlesi** (Karar 6 hiçbir paket adı vermez; atıf bu ADR'nin ilk hâlinde yanlıştı) → Karar 2: hedef `internal/platform/storage`'ta, anahtar sözdizimiyle birlikte. Hedefin adı ve sınadığı iki değişmez değişmedi.
2. **ADR-0029 Karar 6'nın "boyut, saptanan tip, tuval" doğrulamasının tamamlama adımında olması** → değişmiyor, ama Karar 1 onun **tek** savunma olduğunu kayda geçirir: yazma adresi tipi ve boyutu bağlayamıyor, dolayısıyla tamamlama adımı atlanabilir bir ikinci kontrol değil.

ADR-0019 §9, ADR-0023, ADR-0026 ve ADR-0029'un geri kalan kararları değişmiyor. ADR-0029 Karar 5'in EXIF politikası aynen uygulanır; Karar 4 yalnız onun **nasıl** uygulandığını ve PDF'in neden kapsam dışı kaldığını yazar.

## Sonuçlar

- Faz 2'nin çıkış kriterinin yol geçişi kalemi kapandı: `FuzzStorageKey` yeşil ve kapının `verify` job'ında, `-fuzz` olmadan, bütün tohumlarıyla koşuyor.
- Dört fuzz hedefi var, hepsi aynı biçimde (`F.Add` tohumları, kapıda `go test` ile): `FuzzStorageKey` (Karar 2), ve `internal/platform/imaging`'in elle yazılmış bayt yürüyücüleri için `FuzzOrientation`, `FuzzDetect`, `FuzzSanitize` (Karar 4, "Bayt yürüyücüleri fuzz'lanır").
- Handler PR'ının önünde üç şey hazır duruyor: `media.Pipeline.Start`/`Complete`, `media.Rejection` ve `storage.Store`. Ona kalan, tabloyu getirmek, `Rejection`'ı sözleşmenin hata koduna çevirmek ve doğrulamayı River işine taşımaktır (ADR-0029 Karar 6: `complete` `202` döner).
- `media.Kind` sözleşmenin `MediaKind`'ini aynalar ve **elle** aynalar. Sözleşmeye tür eklenirse (PR #17'ye giren galeri ve kapak kararları böyle bir tür getirebilir) buraya da eklenmek zorundadır; eklenmezse tür hiçbir öneke çözülmez ve hat onu reddeder, yani sessiz bir yanlış yere yazma olamaz. Handler PR'ı spec'ten okuyan bir test yazarak bunu makineye bağlayabilir.
- Bu PR'da kalanlar:
  - Gerçek MinIO'ya karşı `//go:build integration` testi. Kapıya servis container eklemek gerektiği için yazılmadı (Karar 6); eklenmesi kapı değişikliği olarak kendi PR'ında tartışılmalı.
  - `incoming/` süpürme işinin **River tarafı** (`media` satırlarını `PENDING`'den çıkarmak). `media` tablosunu istediği için handler PR'ında; nesnelerin temizliği onu beklemez, "Zorunlu ops adımı" bölümündeki yaşam döngüsü kuralı onu dağıtımın koşulu yapar.
  - Yükleme uçlarının oran sınırı grubu (`x-rate-limit: upload`) `internal/app/edge.go`'nun tablosuna girecek; sözleşme PR #17'de, tablo handler PR'ında.
  - Özgün dosya adının taşınması (yukarıdaki daralma 2): `StartMediaUploadRequest`'e isteğe bağlı bir alan, `media` satırına bir kolon ve `Content-Disposition`'da RFC 5987 biçimi.
  - Galerinin sözleşmesi ADR-0029'un son turlarında değişti (`mediaIds: [uuid]` → `items: [{mediaId, caption}]`, ve `PUT`'un `x-body-limit`'i 16384). **Bu katmanı etkilemiyor:** galeri görseli yine `COMPANY_IMAGE`'dır, yine `images/` önekine iner, ve değişen şey isteğin JSON gövdesi — nesnenin kendisi değil. `MediaKind` beş değerinde kaldığı için `media.Kind`'ın elle aynaladığı enum da değişmedi.
  - **İki liste, bir kural.** Bir türün genel okunabilir olup olmadığı iki yerde yazılı: `Kind.PublicReadable()` (önek `images/` mi) ve `Store.PublicURL` (önek `images/` ya da `public/` mi). Bugün aynı cevabı veriyorlar ve `Complete` ikisini birlikte soruyor, ama kural iki yerde durduğu için ayrışabilirler. Handler PR'ında tek yere indirilmeli.
  - **`public/` öneki uykuda.** Java'da `PUBLIC_FILE` bir klasör türü; Go'da hiçbir `Kind` ona çözülmüyor, yani `Store.PublicURL`'ün onu kabul etmesi bugün erişilemez bir daldır. Önek anahtar dilinde duruyor çünkü eski nesneler okunabilir kalmalı; yeni nesne yazan bir yol yok.
  - **Yön ≠ 1'de iki kayıp, kasıtlı.** `applyOrientation` hedefi `image.RGBA` olarak kuruyor: 16 bit kanallı bir PNG 8 bite düşer, ve APNG'nin yalnız ilk karesi taşınır (zaten `image/png` de ilk kareyi çözer). İkisi de kabul edilmiş: Koala'nın görüntüleri fotoğraf ve logo, 16 bit derinlik ya da animasyon için bir iş gerekçesi yok, ve raster başına ikinci bir biçim taşımak strip'in tek-yer kuralını bozardı. Yön 1 ise görüntüye hiç dokunulmadığı için bu kayıplar da olmaz.

## Açık sorular

**Bu ADR'nin açtığı üç sorunun üçü de kabul öncesinde karara bağlandı; bugün açık soru kalmıyor.** Kayıt için, kararlar ve nerede durdukları:

1. **Yazma adresi boyutu ve tipi bağlasın mı? → Hayır, presigned PUT kalıyor.** Faz 2 koordinasyonunda 08.11.2026. Risk, azaltmaları ve geçiş tetikleyicisi Karar 1'in "Kabul edilen risk" bölümünde; riski kapatan ops koşulu "Zorunlu ops adımı" bölümünde. POST policy'ye geçmek bir sözleşme değişikliğidir ve kendi ADR'sini ister.
2. **PDF üstverisi temizlenecek mi? → Faz 2'de hayır.** Faz 2 koordinasyonunda 08.11.2026. Gerekçe, azaltma ve tetikleyici Karar 4'ün "kabul edilen sınır" bölümünde. Yusuf'a bilgi olarak geçilecek; onayı gerektiren bir adım değil, çünkü mevcut davranış korunuyor ve yeni bir bağımlılık alınmıyor.
3. **Galeri ve kapak yeni bir `MediaKind` getiriyor mu? → Hayır.** PR #17 bu PR'ın incelemesi sürerken karara bağladı: "galeri sıralaması aynı zamanda kapağı belirler: listedeki ilk görüntü kapaktır, ayrı bir kapak ucu yoktur" (ADR-0029 Karar 7). Kapak bir medya türü değil, galerinin sırası; `MediaKind` beş değerinde kalıyor (`PROFILE_PICTURE`, `COMPANY_LOGO`, `COMPANY_IMAGE`, `IDENTITY_DOCUMENT`, `CV`) ve bu katmanda hiçbir şey değişmiyor — galeri görüntüsü, kapak olanı dahil, `COMPANY_IMAGE`'dır ve `images/` önekine iner. `media.Kind` sözleşmenin enum'unu **elle** aynalamaya devam ediyor; Sonuçlar'daki kalem (bilinmeyen tür hiçbir öneke çözülmez) o aynalamanın sessizce bozulmasını engelleyen şeydir.
