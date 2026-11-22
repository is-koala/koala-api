# ADR-0035: Medya uçları, doğrulama işi, yetim süpürmesi ve yükleme bütçesi

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K14, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.5 (dış çağrıda tx yok, garantili yan etki), §6.1 Faz 2; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (belirsiz commit), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md) §5, [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) (PII kapısı), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 5 ve 6, [ADR-0030](0030-object-storage-media-pipeline-and-exif.md) (tamamı; *Sonuçlar*'daki "Handler PR'ının önünde…" ve "Bu PR'da kalanlar"), [ADR-0031](0031-gallery-cover-flag-and-cv-document-types.md) Karar 3, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1, [ADR-0034](0034-idempotency-key-store.md); `test/scenarios/media.yaml`; PR #27'nin iki bağımsız incelemesi (FIX-FIRST) ve koordinasyonun ürün kararları (22.11.2026)
- Dış kaynaklar: River `UniqueOpts` (v0.49.0, `insert_opts.go`); AWS S3 `CopyObject` `x-amz-copy-source-if-match` ve presigned GET yanıt geçersiz kılmaları; Cloudinary ve AWS Rekognition'ın "geçersiz görüntü" hata modeli (bozuk ve desteklenmeyen biçimi tek kodla verir); RFC 9110 §15.5.2 (401) ve RFC 6585 §4 (429)
- Kod: `internal/media/{domain.go,service.go,jobs.go,store.go,http.go,pipeline.go}`, `internal/media/mediadb`, `db/migrations/0045_media.sql`, `db/queries/media/media.sql`, `db/softdelete.yaml`, `internal/platform/storage/{key.go,storage.go}` (`Prefix.Public`), `internal/platform/fakes/server.go` (`Before`), `internal/app/{media.go,api.go,authn.go,workers.go}`, `internal/pending/pending.go`, `api/openapi.yaml`, `api/components/problem.yaml`, `test/scenarios/scenarios_test.go` (fuzz hedefleri de bağlanır)

## Bağlam

ADR-0029 medya sözleşmesini dondurdu, ADR-0030 nesne deposu istemcisini ve yükleme hattını (`media.Pipeline`) yazdı, ADR-0031 Karar 3 başlatma isteğinin tip boşluğunu kapattı. Bu PR dört operasyonun handler'ını getirir: `startMediaUpload` (POST `/v1/me/media`), `completeMediaUpload` (POST `/v1/me/media/{mediaId}/complete`), `getMedia` ve `deleteMedia`. Medyayı **kullanan** uçlar (kimlik belgesi, galeri, logo, profil fotoğrafı) kapsam dışıdır; bu PR'ın medyası "sahipli ama hiçbir yere bağlanmamış" durumda kalır ve Karar 8'in süresiyle yaşar.

ADR-0030 bu PR'a bir dizi kalem bıraktı. Hepsi, kapananlar ve devredilenler, sonda "ADR-0030'un kalemleri" tablosunda tek tek eşlenir. Her yeni mekanizma için "bu hangi mevcut kuralı atlatabilir?" sorusu soruldu; cevaplar ilgili kararın altında.

## Karar

### 1. Doğrulanan baytlar yazılır; karantina sunucu tarafında kopyalanmaz

ADR-0030 Karar 1'in öncülü: yazma adresi **anahtarı** bağlar, içeriği bağlamaz, ve `UploadTTL` (15 dakika) boyunca geçerlidir. ADR-0030'un hattı görüntüyü yeniden kodlayıp `Put` ediyordu, ama PDF, DOC ve DOCX'i karantinadan son anahtara `Move` (sunucu tarafı `CopyObject` + `DeleteObject`) ile taşıyordu. Bu bir TOCTOU deliğiydi: iş karantinayı okuyup türünü saptadıktan sonra, kopya **o anki** nesneyi alır; istemci arada aynı adrese bir HTML sayfası, bir çalıştırılabilir dosya ya da gigabaytlarca veri yazarsa o içerik `READY` bir özgeçmiş olarak saklanırdı, kayıtlı boyut da ilk `Head`'den gelirdi.

**Karar:** her tür, okunup doğrulanan baytlarla (`Get` bellekte, türün sınırıyla, en çok 10 MiB) son anahtara `Put` edilir; kayıtlı boyut yazılan baytların boyutudur. `x-amz-copy-source-if-match` ile koşullu kopya AWS'de deliği kapatırdı, ama S3 uyumlu depolarda desteği eşit değil (ADR-0030 Karar 1'in POST policy gerekçesiyle aynı yüzey) ve baytlar zaten bellekte. Kanıt: `TestCompleteCVStoresTheValidatedBytes` karantinayı son anahtarın yazıldığı anda değiştirir ve son nesnenin doğrulanan PDF olduğunu iddia eder; `Move`'a dönen hat orada kırmızıdır, çünkü kopya kaynağını tam o anda okur.

### 2. Doğrulama River işindedir; `complete` 202 döner; durum kapısı satır kilidindedir

ADR-0029 Karar 6 `complete`'in 202 dönmesini istedi. Akış:

1. **İstek** (`RequestCompletion`): satır sahibiyle birlikte `FOR UPDATE` kilitlenir. Yalnız `PENDING` satır bir `media_complete` işi alır; `READY` ya da `REJECTED` satır olduğu gibi 202 ile döner, hiçbir şey kuyruğa girmez. Bu, ADR-0030'un handler'a bıraktığı **durum kapısıdır**: `Pipeline.Complete` karara bağlanmış bir satır için hiç çağrılmaz, dolayısıyla hâlâ geçerli yazma adresine ikinci tipte bir nesne yazıp yeniden tamamlayan istemci ikinci bir son nesne üretemez.
2. **İş** (`Service.Complete`): "karar ver, dışarı çağır, yaz" sırası; nesne deposu çağrıları sırasında tx açık değildir.
   - Satır kilit altında yeniden okunur. `PENDING` değilse iş biter (kapının işteki yarısı). Satır **yoksa** (sahibi ya da süpürme sildi) iş, kimliğin bütün nesnelerini kendi işi olarak siler: önceki bir koşu nesnesini yazıp kaydedememiş olabilir.
   - `Pipeline.Complete` koşar. `Rejection` bir iş sonucudur; `error` deponun hatasıdır.
   - Sonuç kilit altında yazılır, satır hâlâ `PENDING` ise: `PENDING → READY | REJECTED`. **Aynı tx'te**, satırın adlandırmadığı her nesnenin silinmesi kuyruğa girer (`media_delete_objects`, garantili yan etki): `READY`'de karantina ve kimliğin diğer uzantıları, satır bu arada silinmiş ya da karara bağlanmışsa bu koşunun yazdığı nesne.
3. **Hata sınıfları:** okunamayan satır (`fromRow`; tablonun `CHECK`'leri altında erişilemez) **kalıcıdır**, iş `JobCancel` ile biter. Satır `PENDING` kalır ve 24 saat sonra süpürme onu siler; süpürme satırı ayrıştırmaz (kimliği, türü ve anahtarları dize olarak okur), nesne silme işi ise ayrışmayan anahtarda `JobCancel` olur, yani böyle bir satırın nesneleri yaşam döngüsü kuralına ve log'a kalır. Depo ya da veritabanı hatası **geçicidir**, River yeniden dener: `completeAttempts = 8` (≈2,5 saat).
4. **Belirsiz kayıt (MED-006):** sonucu yazan tx düşerse (ADR-0017) nesne korunur (`WriteIndeterminate`); yeniden deneme onu son anahtarında bulup yazar ya da satırı yok bulup siler. Silme kararlı olduğu her yerde garantili bir iştir, "best effort" bir çağrı değildir.
5. **Tekil iş ve kuyruk:** iş medya kimliğine göre tekildir (`UniqueOpts{ByArgs}`, yalnız `media` alanı `river:"unique"`): bekleyen ya da koşan bir iş varken ikinci istek iş eklemez; River'ın vazgeçtiği iş yenisini engellemez. İş **kendi kuyruğundadır** (`media`, örnek başına `QueueWorkers = 1`): 50 MP'lik bir görüntünün çözülmesi saniyelerce bir CPU ve tuval sınırında ≈200 MiB'lık bir RGBA rasteri tutar, yön uygulanırsa iki katı; varsayılan kuyruğun iki işçisini posta ve OTP işleriyle paylaşsaydı on dakikalık bir kod onun arkasında bekleyebilirdi. Tek işçi, bir yükleme selinin örnek başına belleğini ≈400 MiB'ta sınırlar; 12 MP'lik tipik bir telefon fotoğrafı bir saniyenin altında tamamlandığı için tek işçi dakikada onlarcasını bitirir, ölçek örnek sayısıyla büyür. Silme ve süpürme işleri G/Ç'dir ve varsayılan kuyrukta kalır.
6. **Temizliğin bağlamı:** hattın en iyi çaba temizlikleri (karantina silmesi, `Discard`) çağıranın context'iyle değil `context.WithoutCancel` ve kendi 30 saniyelik süresiyle koşar (`CleanupTimeout`). Çağıranın context'i tam da temizliğin gerektiği anda (iş zaman aşımı, kopan istek) ölür.

**Atlatma sorusu ve cevabı:** kapı katmanlıdır (istek, iş, satırı giden ya da karara bağlanmış satırın nesnesinin silinmesi) ve her katman diğerleri olmadan da yetimi engeller. Testler her katmanı **ayrı** ölçer: `TestCompletionGateHoldsForADecidedUpload` "request" alt testi önce ilk işin `river_job` satırını siler (yoksa `UniqueOpts` kapı silinse de ikinci işi reddederdi), "job" alt testi deponun karantinaya hiç istek görmediğini ölçer, "orphan" alt testi sahibin silme işini nesne yazılmadan **önce** koşturur (yoksa o iş nesneyi tek başına silerdi), "decided" alt testi satırı yazım sırasında reddeder. `TestCompletionKeepsAnObjectItCouldNotRecord` kayıt tx'ini context'le değil havuzu kapatarak düşürür; temizlik artık kendi context'inde koştuğu için iptal edilmiş bir context `WriteIndeterminate → WriteFailed` mutasyonunu gizlerdi.

**Silinen çağıran:** `Start`, çağıranı `softdelete.LockActiveParent(users)` ile kilitler. Çağıranın hesabı kimlik doğrulamadan sonra silinmişse `ErrNotActive` gelir; cevap **401 `AUTH_SESSION_NOT_FOUND_OR_INACTIVE`**'dır, kimlik doğrulamanın aynı çağırana bir istek sonra vereceği cevap. 404 değildir: buradaki ebeveyn çağıranın kendisidir, `x-resource-self` okumasında olduğu gibi (`TestStartOfADeletedCallerIsUnauthenticated`).

### 3. `Rejection` → sözleşme kodu: `CORRUPT` `MEDIA_TYPE_UNSUPPORTED`'a katlanır, satır ayrımı korur

`Media.rejectionCode` bir `ErrorCode`'dur. Eşleme `Rejection.Code`'dadır:

| `Rejection` | `rejectionCode` |
|---|---|
| `NOT_UPLOADED` | `MEDIA_UPLOAD_INCOMPLETE` |
| `TYPE_UNSUPPORTED` | `MEDIA_TYPE_UNSUPPORTED` |
| `CORRUPT` | `MEDIA_TYPE_UNSUPPORTED` |
| `TOO_LARGE` | `MEDIA_TOO_LARGE` |

**Gerekçe:** sihirli baytı taşıyıp o biçim olarak çözülmeyen dosya, iddia ettiği tipin geçerli bir dosyası değildir; istemcinin yapacağı şey de aynıdır. Görüntü API'lerinin yerleşik cevabı budur. Satır kodu değil `Rejection`'ı saklar (`media.rejection = 'CORRUPT'`), yalnız yanıt daralır.

**Sözleşme düzeltmesi:** red bir problem yanıtı değildir, kaynağın alanıdır. `completeMediaUpload`'ın `x-error-codes`'u `[MEDIA_NOT_FOUND]`'a indi ve hiçbir kodun dönmediği `409` yanıtı kaldırıldı (anahtar almadığı için `IDEMPOTENCY_KEY_IN_USE` de yok); operasyon bu PR'a kadar 501 döndü, yani hiçbir istemci o duruma bağlanmadı. `MEDIA_UPLOAD_INCOMPLETE` ve `MEDIA_TYPE_UNSUPPORTED` hiçbir operasyondan problem olarak dönmediği için `problem.yaml`'da durumsuz "ret kodları" bölümüne taşındı ve `api.statusFreeCodes`'a gerekçeyle girdi; `MEDIA_TOO_LARGE` başlatmada problem olarak döndüğü için `400`'ünü korur.

### 4. Yetim süpürmesi: kimlik yüklü tasarım, liste taraması yok

**Yapısal değişmez:** son anahtarın kimliği satırın kimliğidir, öneki türünden, uzantısı saptanan tipten gelir. Satırın id'si karantina anahtarının UUID'sidir (`Start`), `Complete` son anahtarı karantina anahtarından önek ve uzantı değiştirerek kurar. Veritabanı bunu da tutar: `media_upload_key_check` karantina anahtarının kimliğini, `media_object_key_check` son anahtarın tamamını (`<türün öneki><id>.<tipin uzantısı>`), `media_content_type_check` türün kabul ettiği tipleri bağlar (`TestDatabaseHoldsTheKeyRules`: başka türün öneki, başka tipin uzantısı, türün reddettiği tip ve başka satırın kimliği `23514` alır). Sonuç: hattın bir medya için bırakabileceği her nesne `{türün öneki}/{id}.{türün kabul ettiği uzantı}` kümesindedir ve bu küme satırdan hesaplanır (`candidates`).

Silme tek bir işte toplanır, `media_delete_objects` (`DeleteObjects`): kimliğin aday son anahtarlarını ve karantina anahtarını siler. İki şeyi tutar: işin **kendi argümanındaki `Keep`** anahtarını, hiçbir durumda; ve iş koştuğunda **aktif satırın adlandırdığını**. Aktif satır yoksa ya da `REJECTED` ise geri kalan gider, `READY` ise `object_key` de kalır, `PENDING` ise hiçbir şeye dokunulmaz (yükleme hâlâ tamamlamanındır). İşi kuyruğa koyanlar: silme, kaydedilen tamamlama (`Keep` = yazdığı nesne), satırı gitmiş ya da karara bağlanmış tamamlama, süpürme ve `Release`, hepsi kendi kararlarının tx'inde.

**`Keep` neden işin argümanında:** kaydedilen tamamlamanın işi yalnız aktif satırı okusaydı, iş koşmadan önce hesabın silinmesi (cascade) satırı `active_media`'dan düşürürdü; iş "aktif satır yok" görüp satırın kendi `object_key`'i dahil her şeyi silerdi ve hesabın geri alınması nesnesiz bir `READY` satırı döndürürdü (Karar 7'nin ihlali; depo arızasında ≈7 saatlik bir pencere). Tamamlama tuttuğu anahtarı işe yazar; `TestRecordedObjectSurvivesAccountDeletion` hesabı iş koşmadan siler ve nesnenin durduğunu ister, `Keep` kalkarsa kırmızıdır.

**Süpürme** (`media_sweep`, saatlik) iki tür medyayı alır: `PENDING`/`REJECTED` olup 24 saatten eski ve tamamlaması son `SweepGrace` (15 dakika) içinde istenmemiş yükleme; `READY` olup hiçbir şeye bağlanmamış ve 7 günden önce karara bağlanmış medya (Karar 8). Onları `(created_at, id)` imleciyle sayfalar ve her birini kendi tx'inde emekli eder: satır yeniden kilitlenir (`SKIP LOCKED`), `PENDING` olan `REJECTED (NOT_UPLOADED)` olur, satır sistem adına `retention` gerekçesiyle soft-delete edilir ve nesnelerinin silinmesi aynı tx'te kuyruğa girer. Emekli edilemeyen medya log'a yazılır, `koala_media_swept_total{result="kept"}` sayılır (uyarı `KoalaMediaSweepKept`) ve imleç onu geçer; sayfa kırılması alınanların değil **listelenenlerin** sayısına bakar. `TestSweepPagesPastMediaItCannotRetire` tam bir sayfa emekli edilemeyen medyanın arkasındaki silinebilir satıra ulaşıldığını ölçer.

**`Bind`'ı unutan tüketici ve süpürmenin açlığı.** `READY` bir medyaya `restrict` kenarında aktif bir tüketici satırı bağlıysa ama tüketici `Bind`'ı çağırmamışsa, medya 7 gün sonra listelenir ve her koşuda `SOFT_DELETE_RESTRICTED` ile düşerdi; bunlardan yeterince olunca her koşunun sayfaları (en çok 2000 satır) onlara harcanır ve arkadakiler hiç süpürülmezdi. Liste sorgusu bu medyayı **kendisi dışlayamaz**: tüketici tabloları henüz yok ve `restrict` kenarları katalogdadır, statik bir sorgu onları adlandıramaz; `restrict` denetimi platformundur (`softdelete.Delete`). Bu yüzden süpürme, emekliliği `SOFT_DELETE_RESTRICTED` ile düşen `READY` medyayı **benimser**: `BindMedia` (`bound_at`'in tek yazıcısı) ile bağlar, bundan sonra hiçbir liste onu döndürmez, ve `result="adopted"` sayılır (uyarı `KoalaMediaBoundBySweep`: tüketicide hata var). `TestSweepAdoptsMediaAConsumerForgotToBind` ikinci koşunun medyayı listelemediğini ölçer; benimseme kalkarsa ikinci koşu yine sayar ve test kırmızıdır.

**Liste taraması (ListObjectsV2 + "satırı olmayan nesne") neden seçilmedi:** `koala_app` yalnız `active_media`'yı görür. Hesabı silinmiş ama geri alma penceresindeki (ADR-0013 madde 6) bir kullanıcının medyası `active_media`'da yoktur, yani liste taraması onun nesnelerini silip geri almayı sessizce bozardı — deponun baskın kusur sınıfı. Ayrıca her koşu kovanın tamamını listelerdi.

**Spec D4 (süpürme + nesne yazımı + belirsiz kayıt):** sıra şudur: tamamlama nesneyi yazar, kaydı düşer (belirsiz); süpürme satırı siler ve silme işini koyar; o iş nesne yazılmadan önce koşmuş olabilir. Kapatan üç şey: (a) süpürme, tamamlaması son 15 dakikada istenmiş satırı almaz (Karar 4, `SweepGrace`), yani olağan yarış hiç başlamaz; (b) kayıt tx'i satırı yok ya da karara bağlanmış bulursa silmeyi aynı tx'te koyar; (c) kaydı düşen koşunun yeniden denemesi satırı yok bulursa kimliğin nesnelerini kendisi siler (Karar 2.2). Kalan delik: iş **sekiz denemesinin hepsinde** düşerse (River işi atar) nesne yetim kalır; bu River'ın `discarded` işi olarak görünür ve kalıcı bir hata sinyalidir. `TestOrphanedFinalObjectsAreSwept/row gone` (c)'yi ölçer.

**Sınır:** hattın **dışında** yazılmış bir nesneyi (bugün yok) ve sert silinmiş bir satırın nesnesini (bugün medya sert silinmez) kapsamaz.

### 5. Okuma ve adres: türe göre üç kural, tek önek kuralı

`Service.Address`:

- **Genel okunabilen tür** (profil fotoğrafı, logo, galeri): kalıcı genel adres (`Store.PublicURL`), `urlExpiresAt: null`.
- **Özgeçmiş:** yalnız `getMedia` **beş dakikalık** imzalı indirme adresi üretir (`PresignGet`, `attachment`, `no-store`, `response-content-type` saptanan tip). `startMediaUpload` ve `completeMediaUpload` indirme adresi vermez.
- **Kimlik belgesi:** hiçbir durumda, hiçbir çağırana adres yok (IDN-007). Personel fotoğrafa incelemenin kendi indirmesinden, ADR-0026'nın PII kapısı ve denetim kaydından geçerek ulaşır; `getMedia` PII kapısına dokunmaz, açtığı kişisel değer yoktur.

**Tekrar ve imzalı adres — kabul edilen sonuç:** başlatmanın yanıtı Idempotency-Key için saklanır ve 24 saat tekrar edilir; yanıttaki `uploadUrl` da imzalıdır ve 15 dakika yaşar. Yani 15 dakikadan sonra gelen bir tekrar **ölü** bir yazma adresi döner. Kabul ediyoruz: tekrar, kaybolan yanıtı kurtarmak içindir ve istemci yanıtı kaybettiği anda yeniden dener; 15 dakika sonra gelen tekrarın istemcisi yeni bir yükleme başlatır (yeni anahtarla). Tekrarda adresi yeniden imzalamak "ilk yanıt aynen döner" vaadini bozardı (ADR-0034 Karar 3'ün reddettiği seçenek). Özgeçmişin indirme adresinin başlatma ve tamamlama yanıtlarına konmamasının gerekçesi bu değildir; o adres bir okuma yetkisidir ve yalnız okuma operasyonunda verilir.

**İki liste, bir kural (ADR-0030):** "bu önek genel mi" tek yerde, `storage.Prefix.Public`'tedir; `Store.PublicURL` ve `Kind.PublicReadable` onu çağırır (`TestPublicReadableIsTheStoresRule`).

Sözleşmeye `Media.urlExpiresAt` (`NullableTimestamp`, zorunlu-nullable) eklendi; operasyonlar 501 döndüğü için uyumlu bir genişlemedir.

### 6. Yükleme bütçesi

Presigned PUT boyutu bağlamaz (ADR-0030 Karar 1, kabul edilmiş risk); iki sınır kullanıcı başına kaç yazma adresinin var olabileceğini bağlar:

| Sınır | Değer | Gerekçe | Kod |
|---|---|---|---|
| Açık (`PENDING`) yükleme | 10 | Ürünün en büyük meşru toplu yüklemesi on görüntülük şirket galerisidir (MED-011), paralel yüklenir; kimlik incelemesi dört fotoğraftır. | 429 `MEDIA_UPLOAD_PENDING_LIMIT` + `Retry-After` |
| Yirmi dört saatte başlatma | 50 | Her şeyini bir günde kuran kullanıcı (17 yükleme) bunu iki kez baştan yapsa da altında kalır. | 429 `MEDIA_UPLOAD_DAILY_LIMIT` + `Retry-After` |

Değerler koordinasyonun kararıyla (22.11.2026) 10 ve 50'de kaldı.

**Açık sınır 409 değil 429'dur** (koordinasyon kararı): açık yükleme sayısı geçici bir durumdur ve bir 409 Idempotency-Key deposunda 24 saat bayat kalırdı (ADR-0034 Karar 5 4xx'i saklar, 429'u saklamaz). `Retry-After` en eski açık yüklemenin süpürüleceği ana kadardır; istemci bir yüklemeyi tamamlayarak ya da silerek yeri hemen boşaltabilir.

**`Retry-After`'ın alt sınırı bir dakikadır** (`RetryFloor`). Hesaplanan bekleme sıfır ya da eksi olabilir: en eski açık yükleme saklama süresini aşmış ama saatlik süpürme onu henüz almamıştır, ya da bir tamamlama işi onu sayım ile okuma arasında `PENDING`'den çıkarmıştır (o iş kullanıcının kilidini almaz; okuma boş dönerse cevap 500 değil yine 429'dur). `Retry-After: 0` istemciyi süpürmenin bir saati boyunca kımıldamayacak bir sınıra karşı döngüye sokardı; bir dakika yeniden deneyen istemciyi ucuz tutar. `TestUploadCapWaitHasAFloor` saklama süresini aşmış ama süpürülmemiş yüklemeyle tabanı ister.

İki sınır aynı tx'te, kullanıcının danışma kilidi altında (`pg_advisory_xact_lock('MU', hashtext(owner))`) sayılır ve harcanır. `TestUploadCapsHoldUnderConcurrency` her sınırın son yerine sekiz eşzamanlı başlatma gönderir ve tam birinin geçtiğini ister; kilit silinirse hepsi aynı sayıyı görüp birden fazlası geçer. Bu bir yarıştır: tek bir koşuda mutasyon yeşil kalabilir, ama neredeyse her koşuda kırmızıdır.

**Atlatma sorusu ve cevabı:** `PENDING` bir yüklemeyi silmek açık sınırdaki yerini boşaltır, yani "başlat-sil-başlat" sınırsız adres üretirdi. Günlük bütçe bu yüzden `media`'yı değil **kendi defterini** (`media_upload_starts`, M2) sayar; soft-delete'li satır `active_media`'dan düşer ama defterden düşmez. `TestUploadBudget` ve `TestDailyUploadLimitAnswersRetryAfter` bütçeyi tam olarak sil-başlat döngüsüyle doldurur.

**Oran sınırı grubu (`x-rate-limit: upload`):** grup ADR-0021'in tablosunda ve `edge.go`'da zaten vardı (adres başına 100/dk, burst 20; kimlik başına 20/dk, burst 5); değerler korundu: bir yükleme iki çağrıdır, dakikada on yükleme on görüntülük galeriyi bir dakikaya sığdırır. Günlük ölçek bu kararın bütçesidir.

### 7. Silme, sahiplik ve nesnenin akıbeti

- **Sahibin silmesi** (`deleteMedia`): satır kilit altında `softdelete.Delete` ile (aktör kullanıcı, `user_request`) silinir ve aynı tx'te `media_delete_objects` kuyruğa girer. Sahibin silmesi nesne için kesindir; **tekil medya geri alma yoktur** (koordinasyon kararı): personel satırı geri alırsa satır nesnesiz döner. Kullanıcının açıkça sildiği bir fotoğrafı saklamaya devam etmek KVKK açısından savunulamaz.
- **Sahiplik iki sınıftır** (koordinasyon kararı; Java paritesi ve sektör):
  - **Kişisel türler** (profil fotoğrafı, özgeçmiş, kimlik belgesi) kullanıcıyla birlikte cascade olur: kenar `media.owner_id → users` **cascade**'dir, hesap silinince satırlar aynı `deletion_id` ile silinir, geri alma onları getirir. Kaydedilmiş nesneler hesap silindiğinde **silinmez** (geri alma penceresi açıktır; kayıt işinin `Keep`'i, Karar 4); **anonimleştirme yalnız kişisel türlerin nesnelerini siler** (`privacy` PR'ı, `koala_maint` olarak satırı okuyup `candidates` kümesini silerek).
  - **Bitmemiş yükleme hesap silinmesinden sağ çıkmaz.** Hesabın cascade'i `PENDING` bir satırı düşürdüyse, onun tamamlama koşusu satırı yok bulur ve kimliğin nesnelerini siler (yazdığı nesne dahil); hesap geri alınırsa satır `PENDING` döner, sonraki tamamlaması `NOT_UPLOADED` ile reddedilir ve süpürme onu alır, kullanıcı yeniden yükler. Bilinçli: bitmemiş bir yükleme henüz kullanıcının verisi değildir ve korumak, süpürmenin kapsamadığı bir nesneyi geri alınmayacak bir hesap için saklamak olurdu.
  - **Şirket türleri** (logo, galeri görseli) bağlandıklarında **şirkete aittir**: yükleyen yöneticinin hesabı silindiğinde ne silinir ne de hesabın silinmesini engeller. Bugünkü cascade kenarı bağlanmamış şirket görselini yükleyenle birlikte siler, bu doğrudur (henüz şirketin değildir). **Şirket PR'ının yükümlülüğü:** galeri ya da logo bağlarken medyanın sahipliğini şirkete taşımak ya da şirket türleri için kullanıcı kenarını `detach` yapmak; aksi hâlde yöneticinin hesabının silinmesi şirketin logosunu cascade ile siler ya da tüketici satırının `restrict` kenarı hesabın silinmesini kilitler (spec Y1). Hangi yolun seçildiği şirket PR'ının ADR'sinde yazılır.
- **Bağlanmış medya:** medyayı kullanan her tablo `media`'ya **`restrict`** kenarıyla bağlanır; bağlanmış medyanın silinmesi `softdelete.Delete`'in kendi denetimiyle `409 SOFT_DELETE_RESTRICTED` olur.
- **CDN önbelleği:** genel görüntü `immutable` yazılır; silinen görüntünün CDN kopyası CDN'in süresine kadar adresiyle erişilebilir kalır. CDN temizleme bir ops işidir.
- **Kimlik belgesinin hesap silindikten sonraki saklaması** (KYC yükümlülüğü ile KVKK'nın amaçla sınırlılığı arasında) bir **hukuk kararıdır** ve bu ADR'de verilmez; kimlik PR'ına açık soru olarak devredilir.

### 8. Bağlanma ve bağlanmamış medyanın süresi

Hiçbir yere bağlanmamış `READY` medya süresiz kalsaydı kova ücretsiz görsel barındırma olurdu ve gönderilmemiş bir kimlik incelemesinin fotoğrafları süresiz saklanırdı (KVKK). Koordinasyon kararı:

- `media.bound_at` kolonu: tüketici kaynak medyayı bağladığında dolar. Yalnız `READY` satırda dolabilir (`media_bound_check`).
- **Tek yazıcı `media.Bind`'dır** (`BindMedia` sorgusu; süpürmenin benimsemesi de aynı sorguyu kullanır): sahibin `READY` medyasını, bağlama tx'inde, satır kilidiyle bağlar; başkasının ya da olmayan medya `MEDIA_NOT_FOUND`, `READY` olmayan `MEDIA_NOT_READY` alır. Güncelleme satırı kilitlediği için tüketicinin ayrıca `LockActiveParent(media)` çağırmasına gerek yoktur.
- **Bırakma `Service.Release`'tir** (koordinasyon kararı, sektör standardı): bir tüketici medyayı değiştirdiğinde (yeni profil fotoğrafı, kimlik yuvasına yeni fotoğraf) eskisi bağlanmamış bir hafta beklemez; **bağı değiştiren tx eskisini `Release` eder**: aynı tx'te soft-delete (sahip adına) ve nesnelerinin silinmesi kuyruğa girer. Tüketici önce kendi satırını yeni medyaya çevirir, sonra eskisini bırakır; eskisine hâlâ aktif bir tüketici satırı bağlıysa platform `SOFT_DELETE_RESTRICTED` döner (`TestRelease`). Profil PR'ı bunu kullanır.
- **Tüketici kenarı `restrict`'tir:** `TestMediaConsumersAreRestrictEdges` katalogda ebeveyni `media` olan her kenarın `restrict` olmasını ister; `detach` (sahibi gösterilen bir medyayı silebilirdi) ve `cascade` (profili ya da galeriyi medyayla silerdi) kırmızıdır. Bugün böyle bir kenar yok, gerçek katalog üzerindeki denetim boş kümede geçer ve ilk tüketiciyle anlam kazanır; testin demo katalogları her politikadan bir kenarla kuralın gerçekten koştuğunu kanıtlar.
- `READY` olup `bound_at IS NULL` ve **7 günden önce** karara bağlanmış medya süpürmede soft-delete edilir, nesneleri silinir (`TestSweepTakesUnboundMediaAndSparesRecentCompletions`).
- **Kullanan her PR `Bind`'ı bağlama tx'inde çağırır, bağı değiştiren tx eskisini `Release` eder.** Makinenin zorladığı yarı: `TestOnlyBindWritesBoundAt` `db/queries`'te `bound_at`'i `BindMedia` dışında hiçbir sorgunun yazmadığını (atama ya da `INSERT` kolon listesi) ve modülün test dışı Go kodunda, üretilmiş `mediadb` dışında, hiçbir dize literalinin `bound_at`'i adlandırmadığını ister. Kör noktaları: çalışma anında parçalardan kurulan SQL (hiçbir literal kolonu adlandırmaz), `db/queries` dışındaki bir dosyadan okunan SQL ve test kodu (fikstür yazabilir). Zorlanamayan yarı: bağlama tx'inin `Bind`'ı **çağırdığı** sorgu metninden görülemez; unutulursa süpürme medyayı tüketici satırı yüzünden silemez ve benimser (uyarı), tüketici satırı yoksa bir hafta sonra siler. Bu, tüketici PR'ının kendi handler testinin işidir ve burada yükümlülük olarak durur.

### 9. `media.Kind` ↔ sözleşmenin `MediaKind`'i ve `AllowedTypes()[0]`

Bağ zaten `internal/app.TestMediaKindsMatchTheContract`'ta iki yönlü kurulu; bu PR handler'ın `media.ContentTypeValue`'sunu testin bağımsız kuralına bağladı. `AllowedTypes()[0]` (ADR-0031 Karar 3) `Service.Start`'ta çağrı yerinde yorumla yazılıdır.

### 10. Nesne deposu yapılandırılmamışsa

`local` ve `test`'te `KOALA_OBJECT_*` olmadan servis açılır (ADR-0030 Karar 7). Başlatma 500 döner ve hata yapılandırmayı adlandırır, hiçbir satır yazılmaz; okuma adressiz döner (`TestWithoutAnObjectStore`).

## ADR-0030'un kalemleri

| ADR-0030'daki kalem | Durum | Nerede |
|---|---|---|
| Handler PR'ının önünde: tabloyu getirmek | Kapandı | Karar 4, `0045_media.sql`, okuma testi fikstürü |
| `Rejection`'ı sözleşmenin koduna çevirmek; `RejectionCorrupt`'ın karşılığı | Kapandı | Karar 3 |
| Doğrulamayı River işine taşımak, `complete` 202 | Kapandı | Karar 2 |
| `Complete` durum kapısı ve `images/` yetim süpürmesi | Kapandı | Karar 2 ve 4 |
| `incoming/` süpürme işinin River tarafı | Kapandı | Karar 4 (yaşam döngüsü kuralı yine dağıtımın koşulu) |
| Yükleme oran sınırı grubu | Kapandı, değer değişmedi | Karar 6 |
| İki liste, bir kural | Kapandı | Karar 5, `Prefix.Public` |
| `media.Kind` elle aynalama | Kapandı (önceden) | Karar 9 |
| Gerçek MinIO'ya karşı integration testi | **Devredildi** | issue [#29](https://github.com/is-koala/koala-api/issues/29) |
| Özgün dosya adının taşınması | Karar verilmiş: taşınmıyor | ADR-0031 Karar 3 |
| Galeri sözleşmesinin değişmesi | Bu katmanı etkilemiyor | ADR-0030 |
| `public/` öneki uykuda | Değişmedi; `Prefix.Public` onu genel sayar, hiçbir tür ona çözülmez | ADR-0030 |
| Yön ≠ 1'de 16 bit ve APNG kaybı | Kabul edilmiş, değişmedi | ADR-0030 |

## Yerine geçtiği

1. **ADR-0030 *Sonuçlar*'ının kalemleri** → yukarıdaki tablo.
2. **ADR-0030'un PDF/DOC/DOCX'i karantinadan `Move` ile taşıyan hattı** (`Pipeline.Complete`, Karar 4'ün "PDF olduğu gibi kalır" uygulaması) → Karar 1: doğrulanan baytlar `Put` edilir. Belgenin olduğu gibi kalması (üstveri sökülmez) değişmedi.
3. **ADR-0029 Karar 6'nın `completeMediaUpload`'ı ret kodlarını problem olarak döndüren hâli** → Karar 3.
4. **`Media.url`'ün "yalnız genel okunabilen türlerde dolar" cümlesi** → Karar 5.
5. **`MEDIA_UPLOAD_INCOMPLETE` ve `MEDIA_TYPE_UNSUPPORTED`'ın problem kodu olarak durumu** (ADR-0029'un `409` ve `400`'ü) → Karar 3: durumsuz ret kodları.

ADR-0029'un, ADR-0030'un ve ADR-0031'in geri kalan kararları değişmiyor.

## Sonuçlar

- `MED-001`…`MED-006` ve `MED-014` Go testlerine bağlandı (`boundAhead`; alanın adres ve referans operasyonları hâlâ donmuş). Senaryo kataloğu artık fuzz hedeflerini de bağlayabilir (`FuzzStorageKey`, MED-003/004). `MED-007` Faz 3'te, `MED-008`…`MED-010` adres PR'ında.
- Yeni metrik `koala_media_swept_total{result}` (`deleted`, `adopted`, `kept`) ve iki uyarı: `KoalaMediaSweepKept`, `KoalaMediaBoundBySweep`.
- `CLAUDE.md`'nin `ErrNotActive` kuralına "ebeveyn çağıranın kendisiyse 401" istisnası eklendi (Karar 2).
- Yeni hata kodları: `MEDIA_UPLOAD_PENDING_LIMIT` (429), `MEDIA_UPLOAD_DAILY_LIMIT` (429). Yeni tablolar: `media` (soft-delete, pii), `media_upload_starts` (M2, pii). Yeni işler: `media_complete` (`media` kuyruğu), `media_delete_objects`, `media_sweep`. Yeni değişken yok, yeni bağımlılık yok.
- `fakes.Server.Before`: testin "istemci depoyu beklerken başka bir şey olur" anını kurduğu kanca.
- **Devredilen yükümlülükler:**
  - Kullanan her PR: bağlarken `media.Bind`, değiştirirken eskisine `Service.Release`, tüketici tablosu `media`'ya `restrict` kenarı (Karar 7, 8).
  - Şirket PR'ı: şirket türlerinin sahipliğini şirkete taşımak ya da kenarı `detach` yapmak (Karar 7).
  - `privacy` PR'ı: anonimleştirme kişisel türlerin nesnelerini siler (Karar 7).
  - Kimlik PR'ı: hesap silindikten sonra kimlik belgesinin saklaması, hukuk kararı (Karar 7).
  - MinIO integration testi: issue #29.
  - CDN temizleme: ops.

## Açık sorular

1. **Kimlik belgesinin hesap silindikten sonraki saklama süresi** (Karar 7): hukuk kararı, kimlik PR'ına devredildi.
2. **Bütçe değerleri** ölçülmedi; üretim trafiği gelince gözden geçirilir.
