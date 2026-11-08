# ADR-0030: Nesne deposu istemcisi, yükleme hattı ve EXIF temizliği

- Durum: Kabul edildi
- Tarih: 08.11.2026
- Karar kaydı: K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3 (onaylı bağımlılıklar), §3.11 (yapılandırma), §6.1 Faz 2; [ADR-0003](0003-dependency-policy.md) madde 1, [ADR-0007](0007-error-model.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (belirsiz commit), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) §9 (yüklemeler API'den geçmez), [ADR-0023](0023-image-release-pipeline-and-test-fakes.md) (test sahteleri), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) (PII kapısı), ADR-0029 Karar 5 ve 6 (kabul edilen tipler, EXIF, anahtar düzeni, karantina; PR [#17](https://github.com/is-koala/koala-api/pull/17)); Java `origin/main` — `StorageKeys`, `S3StorageService`, `UploadValidator`, `ImageMetadataStripper`, `ExifOrientation`, `IdentityVerificationManager` okundu, koşturulmadı; [koala-backend issue #362](https://github.com/is-koala/koala-backend/issues/362) (EXIF ortak sınıf)
- Dış kaynaklar: AWS SigV4 [presigned URL](https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html) ve [GET response override](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html) belgeleri; `aws-sdk-go-v2` v1.47.4 ve `service/s3` v1.114.5 kaynağı; Exif 2.32 §4.6.4 (TIFF etiketi `0x0112`); PNG 3. baskı, `eXIf` parçası; [OWASP File Upload Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html) (tipi içerikten saptama, SVG ve polyglot)
- Kod: `internal/platform/storage/{key.go,storage.go}`, `internal/platform/imaging/{imaging.go,exif.go}`, `internal/platform/fakes/s3.go`, `internal/media/{domain.go,pipeline.go}`, `internal/platform/config/config.go`, `internal/app/serve.go`

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

### 2. Anahtar düzeni `internal/platform/storage`'ta, tek doğrulama noktasıyla

Anahtar dili ADR-0029 Karar 6'nın dilidir ve burada tekrar edilmez. Uygulama ayrıntıları:

- `NewKey` **yazma** yoludur: önek medya türünden, kimlik sunucunun ürettiği **UUIDv7**'den, uzantı **saptanan tipten** gelir. İstemci dosya adı veremez; dosya adı yalnız metaveri olarak taşınır ve anahtara hiç girmez.
- `ParseKey` **okuma** yoludur ve tek bir noktada laxtır: Java'nın yazdığı **UUIDv4** anahtarları kabul eder. Bu asimetri bilinçlidir — eski nesneler okunabilir kalmalı, ama yeni anahtar zaman sıralı olmalı.
- `Key` bir **struct**'tır, string değil. Sıfır `Key` hiçbir nesneyi adlandırmaz ve her depo metodu onu reddeder. Dolaşımdaki her `Key` ya `NewKey`'den ya `ParseKey`'den geldiği için, anahtarı tutan kodun ikinci bir denetim yazması gerekmez; "stringden anahtar üreten" ikinci bir yol depoda yoktur.
- `WithPrefix` ve `WithExt` karantinadan son yere taşımanın tek yoludur: kimlik ve uzantı taşınır, önek değişir, dolayısıyla taşıma önek kümesinin dışına çıkamaz.

**Tek çözme turunun bir sonucu yazıya geçiyor**, çünkü boşluk gibi görünüyor ve değil: iyi biçimli bir anahtarın **tek kat kodlanmış** hâli kabul edilir, ayırıcısı kodlanmış olanı dahil (`images%2f<uuid>.jpg`). Bu güvenlidir, ve gerekçesi izin listesinin kendisidir: tek çözme turundan sonra sonuç desene **birebir** uymak zorundadır, dolayısıyla `images/<uuid>.jpg`'e çözülür, kendi önekinin içinde kalır, ve `FuzzStorageKey` bunu kabul edilen her anahtar için denetler. Kuralın kapattığı şey ikinci turdur: `%252f` bir tur sonra `%2f` olur, içinde hâlâ yüzde işareti vardır ve desene hiç ulaşmadan reddedilir — yani hiçbir çağırana, sonraki bir çözücünün başka bir anahtara çevirebileceği bir string verilmez. Testte bu iki yön ayrı ayrı sabitlendi (`TestParseKeyDecodesExactlyOnce`).

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

- **Sihirli bayt bir biçimi, gövde başka biçimi söylüyorsa reddedilir.** `Sanitize`, `image.DecodeConfig`'in seçtiği çözücüyü saptanan tiple karşılaştırır. JPEG başlığının arkasındaki tam bir PNG böyle düşer — sessizce ikinci biçime yeniden kodlanıp birinci biçimin uzantısıyla saklanmaz.
- **Geçerli bir JPEG'in sonuna eklenmiş veri kabul edilir ve yok olur.** Dosya geçerli bir JPEG'dir; EOI'den sonraki HTML, SVG ya da betik yeniden kodlamadan sağ çıkmaz. Savunma reddetmek değil, yeniden kodlamaktır.

**SVG izin listesinde hiç yok.** SVG bir belgedir, raster değil: betik, dış varlık ve `xlink` taşır, ve onaylı bağımlılıklar arasında onu güvenle temizleyecek bir ayrıştırıcı yok. Kabul edilmesi gerekirse ayrı bir ADR ve ayrı bir bağımlılık onayı ister.

### 4. EXIF: yeniden kodlayarak tümden sök, yalnız yönü önce piksellere pişir

Politika ADR-0029 Karar 5'in politikasıdır. Bu PR onu **tek yerde** uygular ve iki noktada Java'yı düzeltir.

**Silinen:** her APP1/EXIF bölütü, XMP, IPTC, PNG metin parçaları (`tEXt`, `zTXt`, `iTXt`), PNG `eXIf` parçası, renk profili, küçük resim (thumbnail), üretici ve model, seri numaraları, tarih damgaları ve **GPS enlem/boylam**. Mekanizma listeyi tek tek silmek değil: görüntü çözülür ve standart kütüphanenin kodlayıcılarıyla yeniden yazılır. O kodlayıcılar hiç üstveri yazmaz, dolayısıyla yalnız piksel rasteri taşınır. Listeyi eksik saymak mümkün değildir.

**Korunan:** yalnız **yön etiketi** (TIFF `0x0112`), ve o da saklanmaz. Önce okunur, piksellere uygulanır (görüntü döndürülür/aynalanır), sonra yeniden kodlamayla o da gider. Gerekçe: yön, fotoğraf **hakkında** bir üstveri değil, fotoğrafın **nasıl gösterileceğinin** parçasıdır — telefon sensörün rasterini yazar ve "bunu çevir" der. Uygulamadan atmak her dikey fotoğrafı yan saklamak demektir; okuyup pişirmek sektör pratiğidir. Çıktı dik durur ve ikinci bir geçiş hiçbir etiket bulamaz, yani dönüşüm iki kez uygulanamaz.

**Konum için kural en katı hâliyle uygulanır.** Projenin ilan konum kuralı "şirkette tam, bireyselde yaklaşık, haritada hep yaklaşık" diyor. Yüklenen bir fotoğrafın GPS'ini saklamanın hiçbir iş gerekçesi yok, kimlik belgesi fotoğrafı için ise açıkça zararlı: kullanıcının evini söyler. Dolayısıyla konum **hiçbir** medya türünde tutulmaz, ve test bunu bir GPS işaretçisinin kaybolmasıyla kanıtlar.

**Java'dan iki düzeltme:**

- **Tek yer.** Java'da `ImageMetadataStripper.strip` ortak sınıfta duruyor ama `IdentityVerificationManager:289` aynı işi satır içinde tekrar kuruyor (issue #362); bir tarafa yapılan düzeltme öbürüne geçmiyor. Go'da `imaging.Sanitize` tek uygulamadır ve her yükleme yolu ondan geçer.
- **PNG'de de yön okunur.** Java yön etiketini yalnız JPEG'den okuyor. PNG 2017'den beri `eXIf` parçası taşıyabiliyor; Java'da o parça atılırken yön de kayboluyordu. Go iki biçimi de okur, ikisi de aynı TIFF okuyucusuna gider.

**PDF üstverisi sökülmez — bilinen sınır.** Bir PDF yeniden kodlanamaz; onaylı bağımlılıklar arasında PDF kütüphanesi yok (ADR-0003) ve standart kütüphane PDF bilmez. Dolayısıyla CV'nin belge üstverisi (yazar, üretici, oluşturma tarihi, varsa gömülü betik) **olduğu gibi** kalır. Azaltma, nasıl sunulduğudur: PDF `Content-Disposition: attachment` ve saptanan `Content-Type` ile saklanır, `no-store` ile işaretlenir, genel bir adrese hiç çözülmez ve yalnız kısa ömürlü imzalı adresle verilir. Sınır testte açıkça kayıtlıdır (`TestCompleteCVMovesThePDFAsItIs`), yani sessiz değil. Gerçek temizlik gerekirse ayrı bir ADR ve bağımlılık onayı ister (Açık soru 2).

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
- **Eksik yapılandırma `staging` ve `production`'da `serve`'ü durdurur**, çünkü yükleme uçları orada canlıdır. `local` ve `test`'te serbesttir, böylece yükleme üzerinde çalışmayan bir geliştiriciye bucket gerekmez.

Bucket adının alfabesi, anahtarın alfabesiyle aynı gerekçeyle kapalıdır: istek yoluna giriyor. Endpoint ve genel temel adres mutlak `http`/`https` origin olmak, kimlik bilgisi, sorgu ve parça taşımamak zorundadır, ve **production'da `https` olmak** zorundadır: endpoint her imzalı adresin imzasını taşır, genel temel adres ise her istemciye verilir. Sır `config.Secret`'tır, yani yapılandırmanın log'a düştüğü her yerde yer tutucu basar, ve hiçbir hata mesajı bir **değeri** yansıtmaz.

**Red bir `apperr.Code` değil, bir değerdir.** `media.Rejection` (`NOT_UPLOADED`, `TYPE_UNSUPPORTED`, `TOO_LARGE`, `CORRUPT`) doğrulamanın **iş sonucudur**: sözleşme onu medya kaynağında `rejectionCode` olarak taşıyor, yani satır nedeni kaydeder ve istemci onu tek bir yanıttan değil kaynaktan okur. `Complete`'in `error` dönüşü yalnız altyapı hatasıdır.

Bu PR hiçbir `apperr.Code` sabiti **tanımlamaz**. Medya kodları (`MEDIA_NOT_FOUND`, `MEDIA_NOT_READY`, `MEDIA_UPLOAD_INCOMPLETE`, `MEDIA_TYPE_UNSUPPORTED`, `MEDIA_TOO_LARGE`) ADR-0029'un PR'ında donduruldu ve bu dalın `api/components/problem.yaml`'ında **yok**; burada tanımlamak sözleşme testini (`internal/app/api_test.go`) kırardı. `Rejection` → `apperr.Code` eşlemesi handler PR'ındadır.

**Soft-delete ve PII:** bu PR hiç tablo, migration ya da sorgu getirmez, dolayısıyla `db/softdelete.yaml` kataloğuna ve `internal/platform/softdelete`'in JOIN lint'ine değmez. `media` tablosu, sınıflandırması ve fikstürü veri PR'ındadır; kimlik belgesi kaydının ADR-0026'nın PII kapısına bağlanması da (erişim kaydı yazılamazsa veri açılmaz) o PR'ın işidir. Bu PR'ın o yöndeki tek katkısı yapısaldır: `identity-documents/` öneki hiçbir genel adrese çözülmez (`Store.PublicURL` onu `false` döner), yani IDN-007 bir çağıranın dikkatine bırakılmaz.

## Yerine geçtiği

1. **ADR-0029 Karar 6'nın `FuzzStorageKey`'i `internal/media`'ya koyan cümlesi** → Karar 2: hedef `internal/platform/storage`'ta, anahtar sözdizimiyle birlikte. Hedefin adı ve sınadığı iki değişmez değişmedi.
2. **ADR-0029 Karar 6'nın "boyut, saptanan tip, tuval" doğrulamasının tamamlama adımında olması** → değişmiyor, ama Karar 1 onun **tek** savunma olduğunu kayda geçirir: yazma adresi tipi ve boyutu bağlayamıyor, dolayısıyla tamamlama adımı atlanabilir bir ikinci kontrol değil.

ADR-0019 §9, ADR-0023, ADR-0026 ve ADR-0029'un geri kalan kararları değişmiyor. ADR-0029 Karar 5'in EXIF politikası aynen uygulanır; Karar 4 yalnız onun **nasıl** uygulandığını ve PDF'in neden kapsam dışı kaldığını yazar.

## Sonuçlar

- Faz 2'nin çıkış kriterinin yol geçişi kalemi kapandı: `FuzzStorageKey` yeşil ve kapının `verify` job'ında, `-fuzz` olmadan, bütün tohumlarıyla koşuyor.
- Handler PR'ının önünde üç şey hazır duruyor: `media.Pipeline.Start`/`Complete`, `media.Rejection` ve `storage.Store`. Ona kalan, tabloyu getirmek, `Rejection`'ı sözleşmenin hata koduna çevirmek ve doğrulamayı River işine taşımaktır (ADR-0029 Karar 6: `complete` `202` döner).
- `media.Kind` sözleşmenin `MediaKind`'ini aynalar ve **elle** aynalar. Sözleşmeye tür eklenirse (PR #17'ye giren galeri ve kapak kararları böyle bir tür getirebilir) buraya da eklenmek zorundadır; eklenmezse tür hiçbir öneke çözülmez ve hat onu reddeder, yani sessiz bir yanlış yere yazma olamaz. Handler PR'ı spec'ten okuyan bir test yazarak bunu makineye bağlayabilir.
- Bu PR'da kalanlar:
  - Gerçek MinIO'ya karşı `//go:build integration` testi. Kapıya servis container eklemek gerektiği için yazılmadı (Karar 6); eklenmesi kapı değişikliği olarak kendi PR'ında tartışılmalı.
  - `incoming/` süpürme işi (24 saat, M2). River işidir ve `media` tablosunu ister.
  - PDF belge üstverisinin temizlenmesi (Karar 4, Açık soru 2).
  - `internal/platform/imaging`'in elle yazılmış bayt yürüyücüleri (`jpegExifPayload`, `pngExifChunk`, `tiffOrientation`) için fuzz hedefleri. Sınır denetimleri yazılı ve tablo testleriyle sınanıyor, ama o üç fonksiyon saldırganın verdiği uzunlukları ve offset'leri **indeks olarak** kullanıyor; bir `FuzzOrientation`/`FuzzSanitize` çifti "panik etmiyor ve 1–8 aralığının dışına çıkmıyor" değişmezlerini kanıtlardı. Bu PR'ın kapı turu bütçesi dolduğu için ayrı bir PR'a bırakıldı; çıkış kriterinin istediği yol geçişi fuzz'ı (`FuzzStorageKey`) yeşil ve bu kalem onun yerine geçmiyor, yanına ekleniyor.
  - Yükleme uçlarının oran sınırı grubu (`x-rate-limit: upload`) `internal/app/edge.go`'nun tablosuna girecek; sözleşme PR #17'de, tablo handler PR'ında.

## Açık sorular

1. **Yazma adresi boyutu ve tipi bağlasın mı?** Bağlamanın tek yolu S3 POST policy'dir (`content-length-range` ve `Content-Type` koşulları gerçekten imzalanır). Ama POST policy, yanıtta form alanları ister; ADR-0029'un `MediaUpload` şeması (`uploadUrl`, `uploadExpiresAt`) onları taşımıyor ve donmuş durumda. Bugünkü karar: presigned PUT, denetim tamamlamada. Değişmesi **sözleşme değişikliği** olur ve PR #17'nin alanına girer — Yusuf'a ve o PR'a kalan bir karar.
2. **PDF üstverisi temizlenecek mi?** Bugün temizlenmiyor (Karar 4) ve azaltma sunum tarafında. Temizlemek onaylı liste dışında bir PDF kütüphanesi ister, yani ADR-0003 madde 3'e göre ayrı onay. Bir CV'nin yazar alanı bir KVKK kalemi mi, yoksa kullanıcının kendi koyduğu veri mi — bu bir **hukuk/ürün kararı** ve Yusuf'un onayını bekliyor.
3. **Galeri ve kapak yeni bir `MediaKind` getiriyor mu?** PR #17'ye giren kararlar galeri değiştirmeyi ve kapak kavramını ekliyor. Kapak, var olan `COMPANY_IMAGE` satırında bir işaret olarak mı duruyor, yoksa kendi türü mü oluyor? İkincisiyse `media.Kind` ve `Kind.Prefix()` bir değer daha alır; birincisiyse bu katmanda hiçbir şey değişmez. PR #17'nin kararını bekliyor.
