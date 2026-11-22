# ADR-0035: Medya uçları, doğrulama işi, yetim süpürmesi ve yükleme bütçesi

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K14, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.5 (dış çağrıda tx yok, garantili yan etki), §6.1 Faz 2; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (belirsiz commit), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md) §5, [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) (PII kapısı), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 5 ve 6, [ADR-0030](0030-object-storage-media-pipeline-and-exif.md) (tamamı; *Sonuçlar*'daki "Handler PR'ının önünde…" ve "Bu PR'da kalanlar"), [ADR-0031](0031-gallery-cover-flag-and-cv-document-types.md) Karar 3, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1, [ADR-0034](0034-idempotency-key-store.md); `test/scenarios/media.yaml`
- Dış kaynaklar: River `UniqueOpts` (v0.49.0, `insert_opts.go`); AWS S3 presigned GET yanıt geçersiz kılmaları; Cloudinary ve AWS Rekognition'ın "geçersiz görüntü" hata modeli (bozuk ve desteklenmeyen biçimi tek kodla verir)
- Kod: `internal/media/{domain.go,service.go,jobs.go,store.go,http.go}`, `internal/media/mediadb`, `db/migrations/0045_media.sql`, `db/queries/media/media.sql`, `db/softdelete.yaml`, `internal/platform/storage/{key.go,storage.go}` (`Prefix.Public`), `internal/platform/fakes/server.go` (`Before`), `internal/app/{media.go,api.go,authn.go,workers.go}`, `internal/pending/pending.go`, `api/openapi.yaml`, `api/components/problem.yaml`

## Bağlam

ADR-0029 medya sözleşmesini dondurdu, ADR-0030 nesne deposu istemcisini ve yükleme hattını (`media.Pipeline`) yazdı, ADR-0031 Karar 3 başlatma isteğinin tip boşluğunu kapattı. Bu PR dört operasyonun handler'ını getirir: `startMediaUpload` (POST `/v1/me/media`), `completeMediaUpload` (POST `/v1/me/media/{mediaId}/complete`), `getMedia` ve `deleteMedia`. Medyayı **kullanan** uçlar (kimlik belgesi, galeri, logo, profil fotoğrafı) kapsam dışıdır; bu PR'ın medyası "sahipli ama hiçbir yere bağlanmamış" durumda kalır.

ADR-0030 bu PR'a on yükümlülük bıraktı. Bu ADR her birinin nasıl karşılandığını ve uygulamanın gerektirdiği yeni kararları yazar. Her yeni mekanizma için "bu hangi mevcut kuralı atlatabilir?" sorusu soruldu; cevaplar ilgili kararın altında.

## Karar

### 1. `media` tablosu

- `media` soft-delete'lidir (K14) ve `pii: true`'dur: kimlik belgesi fotoğrafı, profil fotoğrafı ve özgeçmiş bir kişiye aittir; view `security_barrier`'dır. Kenar `media.owner_id → users` **cascade**'dir: hesabın silinmesi medya satırlarını aynı `deletion_id` ile siler, geri alma onları geri getirir.
- Kolonlar: tür, durum, karantina anahtarı (`upload_key`), bildirilen boyut, tamamlama isteğinin ve kararın anı; `READY`'de son anahtar, saptanan tip ve boyut; `REJECTED`'da hattın kendi `Rejection` değeri. Durum makinesinin kolonları bir `CHECK`'le tutulur.
- `x-resource: media` gerçek olur: `getMedia` üretilmiş okuma testine girer; fikstür bir kullanıcı ve onun `PENDING` yüklemesidir, okuyan sahibidir.
- `media_upload_starts` M2'dir (Karar 6).

### 2. Doğrulama River işindedir; `complete` 202 döner; durum kapısı satır kilidindedir

ADR-0029 Karar 6 `complete`'in 202 dönmesini istedi. Akış:

1. **İstek** (`RequestCompletion`): satır sahibiyle birlikte `FOR UPDATE` kilitlenir. Yalnız `PENDING` satır bir `media_complete` işi alır; `READY` ya da `REJECTED` satır olduğu gibi 202 ile döner, hiçbir şey kuyruğa girmez. Bu, ADR-0030'un handler'a bıraktığı **durum kapısıdır**: `Pipeline.Complete` karara bağlanmış bir satır için hiç çağrılmaz, dolayısıyla hâlâ geçerli yazma adresine ikinci tipte bir nesne yazıp yeniden tamamlayan istemci ikinci bir son nesne üretemez.
2. **İş** (`Service.Complete`): "karar ver, dışarı çağır, yaz" sırası; nesne deposu çağrıları sırasında tx açık değildir.
   - Satır kilit altında yeniden okunur; `PENDING` değilse iş biter. Bu, kapının işteki ikinci yarısıdır.
   - `Pipeline.Complete` koşar. `Rejection` bir iş sonucudur; `error` deponun hatasıdır.
   - Sonuç kilit altında yazılır, satır hâlâ `PENDING` ise: `PENDING → READY | REJECTED` (`CanTransition` ve `UPDATE … WHERE status = 'PENDING'`).
3. **Hata sınıfları:** okunamayan satır (`fromRow` hatası; tablonun `CHECK`'leri altında erişilemez) **kalıcıdır**, iş `JobCancel` ile biter ve süpürme satırı alır. Depo ya da veritabanı hatası **geçicidir**, River yeniden dener: `completeAttempts = 8` (River'ın `attempt⁴` saniyelik beklemesiyle ≈2,5 saat; süpürmenin 24 saatinin içinde). İş argüman başına **tekildir** (`UniqueOpts{ByArgs}`): bekleyen ya da koşan bir iş varken ikinci istek iş eklemez; River'ın vazgeçtiği (discarded) iş yenisini engellemez, sahibi yeniden deneyebilir.
4. **Nesnenin akıbeti (MED-006):** satır yazımı belirsiz kaldıysa (tx hatası) nesne korunur ve yeniden deneme onu son anahtarında bulup yazar (`finishedEarlier`); "yüklenmedi" demek doğru nesneyi kaybettirirdi. Satır bu arada silinmiş ya da karara bağlanmışsa hiçbir satır nesneyi göstermeyecektir ve commit'ten sonra silinir (`Discard, WriteFailed`). Bu, Java'nın "kayıt düşerse nesne silinir" değişmezinin iş kuyruğundaki karşılığıdır: düşen yazım yeniden denenebildiği için silme yalnız satırın kesin olarak gittiği durumdadır.

**Atlatma sorusu ve cevabı:** kapı üç katmandır (istek, iş, yetim silme) ve her biri diğer ikisi olmadan da yetimi engeller. Bu yüzden testler her katmanı **ayrı** ölçer: `TestCompletionGateHoldsForADecidedUpload`'ın "request" alt testi önce ilk işin `river_job` satırını siler (yoksa `UniqueOpts` ikinci işi zaten reddeder ve kapı silinse de test yeşil kalırdı), "job" alt testi deponun karantinaya hiç istek görmediğini ölçer (yoksa yetim silme ikinci nesneyi temizler ve test yine yeşil kalırdı), "orphan" alt testi yazma sırasında satırı siler.

### 3. `Rejection` → sözleşme kodu: `CORRUPT` `MEDIA_TYPE_UNSUPPORTED`'a katlanır, satır ayrımı korur

`Media.rejectionCode` bir `ErrorCode`'dur. Eşleme `Rejection.Code`'dadır:

| `Rejection` | `rejectionCode` |
|---|---|
| `NOT_UPLOADED` | `MEDIA_UPLOAD_INCOMPLETE` |
| `TYPE_UNSUPPORTED` | `MEDIA_TYPE_UNSUPPORTED` |
| `CORRUPT` | `MEDIA_TYPE_UNSUPPORTED` |
| `TOO_LARGE` | `MEDIA_TOO_LARGE` |

**Gerekçe:** sihirli baytı taşıyıp o biçim olarak çözülmeyen dosya, iddia ettiği tipin geçerli bir dosyası değildir; istemcinin yapacağı şey de aynıdır (geçerli bir JPEG ya da PNG göndermek). Görüntü API'lerinin yerleşik cevabı budur (bozuk ve desteklenmeyen biçim tek "geçersiz görüntü" kodudur). Beşinci bir kod, istemciye farklı davranamayacağı iki hatayı ayırt ettirirdi. Ayrım kaybolmaz: satır kodu değil `Rejection`'ı saklar (`media.rejection = 'CORRUPT'`), yalnız yanıt daralır. `MEDIA_TYPE_UNSUPPORTED` ve `MEDIA_TOO_LARGE`'ın düzyazısı buna göre güncellendi; ikincisi ADR-0030 Karar 5'in istediği "ölçülen boyut yeniden kodlanmış nesnenindir" cümlesini taşır.

**Sözleşme düzeltmesi:** red bir problem yanıtı değildir, kaynağın alanıdır. `completeMediaUpload`'ın `x-error-codes`'u bu yüzden `[MEDIA_NOT_FOUND]`'a indi; `startMediaUpload`'ınki `MEDIA_TYPE_UNSUPPORTED`'ı bıraktı (başlatma tip görmez) ve bütçe kodlarını aldı (Karar 6). Operasyonlar 501 döndüğü için bu uyumlu bir düzeltmedir; ADR-0032 Karar 1'in bağ denetimi listeyi handler'ın ulaşabildiği hata yerlerine kilitler.

### 4. Yetim süpürmesi: kimlik yüklü tasarım, liste taraması yok

**Yapısal değişmez:** son anahtarın kimliği satırın kimliğidir. Satırın id'si karantina anahtarının UUID'sidir (`Start`), `Complete` son anahtarı karantina anahtarından önek ve uzantı değiştirerek kurar, ve iki `CHECK` (`media_upload_key_check`, `media_object_key_check`) bunu veritabanında da tutar. Sonuç: hattın bir medya için bırakabileceği her nesne `{türün öneki}/{id}.{türün kabul ettiği uzantı}` kümesindedir ve bu küme satırdan hesaplanır (`candidates`).

Süpürme üç yerde koşar:

1. **Süresi dolan yükleme** (`media_sweep`, saatlik): 24 saatten eski `PENDING` ve `REJECTED` satırlar `SKIP LOCKED` ile alınır, `PENDING` olanlar `REJECTED (NOT_UPLOADED)` olur (artık hiçbir iş onu yazamaz), sonra tx dışında **bütün aday son anahtarlar ve karantina anahtarı** silinir, sonra satır sistem adına `retention` gerekçesiyle soft-delete edilir. Nesneleri silinemeyen satır `REJECTED` kalır ve sonraki koşuda yeniden alınır. Kapsadığı yetim: nesnesini yazıp kaydedemeyen ve yeniden denemeleri biten tamamlama.
2. **`READY` geçişi** (`deleteSiblings`): satırın adlandırmadığı diğer uzantılar silinir. Kapsadığı yetim: önceki bir koşunun yazıp kaydedemediği, sonra istemcinin aynı adrese başka tipte yazdığı nesne (ADR-0030'un yeniden-PUT yetiminin kapıdan önce oluşmuş hâli).
3. **Silme** (Karar 7).

**Liste taraması (ListObjectsV2 + "satırı olmayan nesne") neden seçilmedi:** `koala_app` yalnız `active_media`'yı görür. Hesabı silinmiş ama 30 günlük geri alma penceresindeki (ADR-0013 madde 6) bir kullanıcının medyası `active_media`'da yoktur, yani liste taraması onun nesnelerini "satırsız" sayıp siler ve geri almayı sessizce bozardı — bu deponun baskın kusur sınıfının ta kendisi: sonradan eklenen bir mekanizma doğru bir kuralı geçersiz kılar. Ayrıca her koşu kovanın tamamını listelerdi. Kimlik yüklü süpürme yalnız kendi satırının adaylarına dokunur.

**Sınır:** hattın **dışında** yazılmış bir nesneyi (bugün yok) ve sert silinmiş bir satırın nesnesini (bugün medya sert silinmez) kapsamaz. Kapıdan önce oluşmuş yetim de yoktur: tablo ve handler bu PR'da birlikte geliyor, yani kapısız koşmuş bir handler hiç olmadı.

### 5. Okuma ve adres: türe göre üç kural, tek önek kuralı

`Service.Address`:

- **Genel okunabilen tür** (profil fotoğrafı, logo, galeri): kalıcı genel adres (`Store.PublicURL`), `urlExpiresAt: null`.
- **Özgeçmiş:** yalnız `getMedia` **beş dakikalık** imzalı indirme adresi üretir (`PresignGet`, `attachment`, `no-store`, saptanan tip). Beş dakika: istemci adresi hemen izler; günlüğe ya da ekran görüntüsüne düşen adres kısa sürede ölür (personelin kimlik belgesi indirmesi 3 dakikadır; sahibin kendi özgeçmişi telefondan çekilir). `startMediaUpload` ve `completeMediaUpload` imzalı adres vermez: başlatmanın yanıtı Idempotency-Key için saklanır ve tekrar edildiğinde ölü bir adres taşırdı.
- **Kimlik belgesi:** hiçbir durumda, hiçbir çağırana adres yok (IDN-007). Sahip kendi belgesinin kaydını görür, fotoğrafını görmez; personel fotoğrafa incelemenin kendi indirmesinden ulaşır ve o yol ADR-0026'nın PII kapısı ile denetim kaydından geçer. `getMedia` bu yüzden PII kapısına hiç dokunmaz: açtığı bir kişisel değer yoktur.

Sözleşmeye `Media.urlExpiresAt` (`NullableTimestamp`, zorunlu-nullable) eklendi ve `url`'ün düzyazısı türe göre ayrımı yazar. Operasyonlar 501 döndüğü için uyumlu bir genişlemedir.

**İki liste, bir kural (ADR-0030):** "bu önek genel mi" artık tek yerde, `storage.Prefix.Public`'tedir; `Store.PublicURL` ve `Kind.PublicReadable` onu çağırır. `TestPublicReadableIsTheStoresRule` her tür ve her önek için iki cevabı karşılaştırır.

### 6. Yükleme bütçesi

Presigned PUT boyutu bağlamaz (ADR-0030 Karar 1, kabul edilmiş risk); bir yazma adresi, deponun boyutunu sınırlamadığı tek şeydir. İki sınır kullanıcı başına kaç adresin var olabileceğini bağlar:

| Sınır | Değer | Gerekçe | Kod |
|---|---|---|---|
| Açık (`PENDING`) yükleme | 10 | Ürünün en büyük meşru toplu yüklemesi on görüntülük şirket galerisidir (MED-011), paralel yüklenir; kimlik incelemesi dört fotoğraftır. | 409 `MEDIA_UPLOAD_PENDING_LIMIT` |
| Yirmi dört saatte başlatma | 50 | Her şeyini bir günde kuran kullanıcı (profil, logo, on galeri, dört kimlik, özgeçmiş: 17) bunu iki kez baştan yapsa da altında kalır. | 429 `MEDIA_UPLOAD_DAILY_LIMIT` + `Retry-After` |

İkisi aynı tx'te, kullanıcının danışma kilidi altında (`pg_advisory_xact_lock('MU', hashtext(owner))`) sayılır ve harcanır; iki eşzamanlı başlatma ikisini birden geçemez.

**Atlatma sorusu ve cevabı:** `PENDING` bir yüklemeyi silmek açık sınırdaki yerini boşaltır, yani "başlat-sil-başlat" sınırsız adres üretirdi. Günlük bütçe bu yüzden `media`'yı değil **kendi defterini** (`media_upload_starts`, M2) sayar; soft-delete'li satır `active_media`'dan düşer ama defterden düşmez. `TestUploadBudget` ve `TestDailyUploadLimitAnswersRetryAfter` bütçeyi tam olarak sil-başlat döngüsüyle doldurur; bütçe `media`'yı saysaydı ikisi de kırmızı olurdu. Defter satırı Idempotency-Key ile aynı tx'tedir: yanıtı kaybolan başlatmanın tekrarı bütçe harcamaz.

**Oran sınırı grubu (`x-rate-limit: upload`):** grup ADR-0021'in tablosunda ve `edge.go`'da zaten vardı (adres başına 100/dk, burst 20; kimlik başına 20/dk, burst 5); bu PR'la `startMediaUpload` ve `completeMediaUpload` handler'lı olarak gerçekten o gruptadır. **Değerler korundu:** bir yükleme iki çağrıdır (başlat + tamamla) ve arada istemci nesneyi depoya PUT eder; kimlik başına 20/dk dakikada on yüklemedir, on görüntülük galeri bir dakikaya sığar. Bu sınır saniye ölçeğindeki seli keser; günlük ölçek Karar 6'nın bütçesidir. Değeri değiştirmek ADR-0021'in tablosunu değiştirmek olurdu (`TestRateLimitGroups`); ölçüm olmadan gerek görülmedi.

### 7. Silme ve nesnenin akıbeti

- **Sahibin silmesi** (`deleteMedia`): satır kilit altında `softdelete.Delete` ile (aktör kullanıcı, `user_request`) silinir ve aynı tx'te `media_delete_objects` işi kuyruğa girer (garantili yan etki). İş, satır bu arada geri alınmadıysa bütün aday anahtarları ve karantina anahtarını siler; başka bir medyanın nesnesine dokunmaz (id'ler ayrı). Sahibin silmesi nesne için kesindir: personel satırı geri alırsa satır nesnesiz döner. Bu bilinçlidir — kullanıcının açıkça sildiği bir fotoğrafı (genel okunabilen türde herkese servis edilen) saklamaya devam etmek KVKK açısından savunulamaz.
- **Hesabın silinmesi** (cascade): nesneler **silinmez**, çünkü geri alma penceresi boyunca hesap geri gelebilir. Nesnelerin silinmesi hesabın anonimleştirilmesinin işidir (ADR-0013 madde 6, `privacy`); o PR medya satırlarını `koala_maint` olarak okuyup `candidates` kümesini silmek zorundadır.
- **Bağlanmış medya:** medyayı kullanan her tablo (`company_images`, kimlik incelemesinin belge yuvası, logo, profil fotoğrafı) `media`'ya **`restrict`** kenarıyla bağlanır ve kenarını `db/softdelete.yaml`'da öyle sınıflar. Böylece bağlanmış medyanın silinmesi `softdelete.Delete`'in kendi denetimiyle `409 SOFT_DELETE_RESTRICTED` olur, handler ikinci bir kural yazmaz. Kullanan uçlar bağlarken `softdelete.LockActiveParent(media)` çağırır ve medyanın `READY` olmasını ister (`MEDIA_NOT_READY`). Bugün böyle bir kenar yok; kural bu ADR'de yazılı, uygulaması kullanan uçların PR'larındadır.
- **CDN önbelleği:** genel görüntü `public, max-age=31536000, immutable` ile yazılır (ADR-0030). Silinen görüntünün CDN'deki kopyası CDN'in kendi süresine kadar adresiyle erişilebilir kalır; adres tahmin edilemez bir UUID taşır. CDN temizleme (purge) bir ops işidir ve bu PR'da yoktur.

### 8. `media.Kind` ↔ sözleşmenin `MediaKind`'i

ADR-0030'un istediği bağ zaten `internal/app.TestMediaKindsMatchTheContract`'ta iki yönlü kurulu (spec'ten okur). Bu PR'ın eklediği: handler `MediaContentType`'ı `media.ContentTypeValue` ile üretir ve `TestMediaContentTypesMatchTheContract` onun testin bağımsız kuralıyla aynı yazımı verdiğini denetler.

### 9. `AllowedTypes()[0]` (ADR-0031 Karar 3)

`Service.Start`'ta çağrı yerinde yorumla yazılıdır: istek tip taşımaz, `Pipeline.Start` karantina uzantısı için bir tip ister, ilk tip verilir ve seçim kozmetiktir — son uzantı içerikten saptanan tipten gelir (`TestKeysAreTheServersAndCarryTheRowsID`: `.jpg` adresine yazılan PNG `images/<id>.png` olur).

### 10. Nesne deposu yapılandırılmamışsa

`local` ve `test`'te `KOALA_OBJECT_*` olmadan servis açılır (ADR-0030 Karar 7). Medya modülü yine kurulur: başlatma 500 döner ve log yapılandırmanın eksikliğini adlandırır; depo olmadan hiçbir yükleme başlayamadığı için nesnesi gereken bir satır da oluşamaz, okuma adressiz döner.

## Yerine geçtiği

1. **ADR-0030 *Sonuçlar*, "`RejectionCorrupt`'ın sözleşmede karşılığı yok"** → Karar 3: katlanır, satır ayrımı saklar.
2. **ADR-0030 Karar 5 ve *Sonuçlar*, "`Complete` için durum kapısı ve `images/` önekinde yetim süpürmesi"** → Karar 2 ve 4.
3. **ADR-0030 *Sonuçlar*, "İki liste, bir kural"** → Karar 5: `storage.Prefix.Public`.
4. **ADR-0030 *Sonuçlar*, "`incoming/` süpürme işinin River tarafı"** → Karar 4'ün birinci maddesi. Nesne tarafındaki yaşam döngüsü kuralı (ADR-0030, "Zorunlu ops adımı") değişmez ve hâlâ dağıtımın koşuludur: süpürme karantina anahtarını da siler, ama süpürme koşmasa bile kova kendini temizlemelidir.
5. **ADR-0030 *Sonuçlar*, "Yükleme uçlarının oran sınırı grubu"** → Karar 6: tablo satırı ADR-0021'inkidir ve değişmedi.
6. **ADR-0029 Karar 6'nın `completeMediaUpload`'ı ret kodlarını problem olarak döndüren hâli** (`x-error-codes`'unda dört kod) → Karar 3: ret kaynağın `rejectionCode`'unda, liste `[MEDIA_NOT_FOUND]`.
7. **`Media.url`'ün "yalnız genel okunabilen türlerde dolar" cümlesi** → Karar 5: özgeçmişte `getMedia`'nın imzalı adresi de dolar, `urlExpiresAt` eklendi.

ADR-0029'un, ADR-0030'un ve ADR-0031'in geri kalan kararları değişmiyor.

## Sonuçlar

- `MED-001`…`MED-006` ve `MED-014` Go testlerine bağlandı (`boundAhead`; alanın adres ve referans operasyonları hâlâ donmuş). `MED-007` Faz 3'te, `MED-008`…`MED-010` adres PR'ında.
- Yeni hata kodları: `MEDIA_UPLOAD_PENDING_LIMIT` (409), `MEDIA_UPLOAD_DAILY_LIMIT` (429). Yeni tablolar: `media` (soft-delete, pii), `media_upload_starts` (M2, pii). Yeni işler: `media_complete`, `media_delete_objects`, `media_sweep` (saatlik, açılışta). Yeni değişken yok, yeni bağımlılık yok.
- `fakes.Server.Before`: bir isteğin cevabından önce bir kez koşan kanca; testlerin "istemci depoyu beklerken başka bir tx commit eder" anını kurmasını sağlar.
- **Bu PR'da kalanlar:**
  - Hesap anonimleştirmesinin medya nesnelerini silmesi (Karar 7), `privacy` PR'ında.
  - Medyayı kullanan uçların `restrict` kenarları ve `MEDIA_NOT_READY` (Karar 7).
  - CDN önbelleğinin silmede temizlenmesi (Karar 7), ops işi.
  - Bütçe ve sınır değerleri ölçülmedi; üretim trafiği gelince gözden geçirilir. Kötüye kullanım görülürse ADR-0030 Karar 1'in tetikleyicisi (POST policy) geçerlidir.

## Açık sorular

1. **Silinen medyanın geri alınması.** Karar 7 sahibin silmesini nesne için kesin sayıyor. Personelin tek bir medyayı geri alması bir ürün ihtiyacıysa nesne silme işi geri alma penceresi kadar ertelenebilir (ör. 30 gün); bu, silinen fotoğrafın o kadar süre genel adresinden servis edilmesi demektir. Bugünkü seçim KVKK yönünde.
2. **Bütçe değerleri** (10 açık, 50/gün) ürünün bugünkü en büyük kullanımından türetildi; ilan galerisi gibi yeni bir toplu yükleme geldiğinde gözden geçirilir.
