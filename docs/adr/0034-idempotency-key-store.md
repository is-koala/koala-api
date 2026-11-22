# ADR-0034: Idempotency-Key deposu

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: issue #22
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.5 (transaction), §4.1-6 (`idempotency_keys`); [ADR-0005](0005-api-contract-principles.md) madde 6 ve *Sonuçlar*, [ADR-0007](0007-error-model.md), [ADR-0012](0012-data-access-and-migrations.md), [ADR-0013](0013-soft-delete.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (commit belirsizliği), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) (doğrulanmış değer), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §5 (giriş biberi), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1 ve *Sonuçlar*; stil rehberi §4 ve §7
- Dış kaynaklar: IETF `draft-ietf-httpapi-idempotency-key-header` (anahtar, parmak izi, 409/422); Stripe API "Idempotent requests" (aktör ve anahtar, 24 saat, `Idempotent-Replayed`, doğrulamada reddedilen istek saklanmaz); RFC 8785 (JSON kanonikleştirme); RFC 5869 (HKDF); NIST SP 800-38D (GCM); KVKK (şifreli kişisel veri kişisel veridir)
- Kod: `internal/platform/idempotency`, `internal/platform/httpx/idempotency.go` (`Idempotent-Replayed`), `internal/platform/httpx/router.go` (`IsStream`), `internal/platform/apperr` (`KindUnprocessable`), `internal/authn/http.go` (`CompleteRegistration`, `RegistrationTokenActor`, `RegistrationReplayThrottle`), `internal/authn/accounts.go`, `internal/app/api.go` (`idempotencyConfig`), `internal/app/workers.go` (`idempotency_purge`), `internal/archtest/idempotency_test.go`, `db/migrations/0044_idempotency_keys.sql`, `db/queries/idempotency/keys.sql`, `ops/monitoring/prometheus-alerts.yml` (`KoalaIdempotencyUnbound`)

## Bağlam

Sözleşmede `Idempotency-Key` başlığını zorunlu alan on iki operasyon var. Başlık bugüne kadar yalnız biçim olarak doğrulanıyordu; parametrenin ve stil rehberi §7'nin "aynı anahtar farklı gövdeyle 422, ilk istek sürerken 409" vaadi karşılanmıyordu. İlk kullanıcı `completeRegistration`. Yanıtı kaybolan tekrar bugüne kadar `REGISTRATION_TOKEN_INVALID` alıyordu. Kayıt token'ı tek kullanımlık olduğu için yan etki iki kez olmuyordu, ama istemci oturumunu kaybediyordu.

Bu ADR deponun beş sorusunu karara bağlar: anahtarın kime ait olduğu, iki isteğin ne zaman aynı sayıldığı, saklanan yanıtın gizliliği, "işlendi" kaydının iş verisiyle nasıl birlikte commit olduğu ve hangi yanıtların saklandığı. Ayrıca bağın makineyle nasıl denetlendiğini yazar.

## Karar

### 1. Aktör

Satırın anahtarı (aktör, operasyon, `Idempotency-Key`) üçlüsüdür (plan §4.1-6, Stripe).

- **Çağıranı olan operasyonda** aktör çağırandır: `user:<id>` ya da `staff:<id>`. Kimlik doğrulama middleware'den önce gelir.
- **Çağıranı olmayan operasyonda** (`security: []`) aktör, isteğin sunduğu ve yalnız meşru istemcinin bildiği bir **yetkinin özetidir**. `completeRegistration`'da bu, gövdedeki kayıt token'ıdır: `registration:` + base64url(SHA-256("koala-api idempotency actor v1" ‖ 0x00 ‖ token)). Ön ek, değeri `pending_registrations.registration_token_hash` ile aynı olmaktan ayırır.

Reddedilen adaylar:

- **`X-Device-Id`:** istemcinin beyanıdır. Saldırgan kurbanın cihaz kimliğini beyan edip anahtarını tahmin ederse kurbanın token çiftini okurdu.
- **İstemci adresi:** NAT arkasındaki herkes aynı aktör olurdu. Ağ değiştiren mobil istemci ise kendi yeniden denemesinde başka aktör olur ve yanıtını alamazdı.

Token'ın özeti, kimliksiz uçta başkasının yanıtını okumayı kapatır. Anahtarı bilen ama token'ı bilmeyen saldırgan başka bir aktörün satırına düşer: kendi isteği yeni bir istek olarak işler ve `REGISTRATION_TOKEN_INVALID` alır. Token zaten operasyonun tükettiği yetkidir: onu tutan, kaydı kendisi de tamamlayabilirdi.

**Token ve anahtar birlikte ele geçerse:** saldırgan kurbanın satırına düşer. Parolayı bilmiyorsa yanlış parola 422, doğru parola saklanan 201 alır. Bu fark bir **parola kâhinidir**, ve iki yanıt da handler'ın token kovasına (`authn.StrictMiddleware`'in `registration-token:` kovası) ulaşmadan dönerdi: depo, sonradan gelen bir mekanizma olarak token başına oran sınırını sessizce devre dışı bırakırdı (#21 ve #23'ün sınıfı). Bu yüzden çağıranı olmayan operasyonda deponun handler'ı koşturmadan verdiği her yanıt (tekrar, 422, 409), **aynı kovadan** bir jeton harcar (`PublicOperation.Throttle`; `completeRegistration`'da `authn.RegistrationReplayThrottle`, anahtarı `registrationTokenLimitKey` tek yerde üretir). Kova boşalınca yanıt 429 `RATE_LIMITED` + `Retry-After` olur. Kâhin böylece token'ın kendi oran sınırıyla, yani parolanın doğrudan denenmesiyle aynı hızla sınırlıdır; depo bir şey eklemez (`TestRegistrationReplaysTakeFromTheTokenBucket`).

**Kural:** Çağıranı olmayan ve anahtar alan her operasyon `idempotency.Config.Public`'te aktörünü ve kovasını (`PublicOperation{Actor, Throttle}`) adlandırır. Adlandırmayan, ikisinden birini eksik bırakan böyle bir operasyon, çağıranı olan bir operasyona verilmiş girdi ya da bağlı olmayan bir operasyonun girdisi sunucuyu açtırmaz (`NewMiddleware`, `TestNewMiddlewareBindsTheContract`, `TestIdempotencyBindsTheContract`).

### 2. İstek özeti

İki istek, kanonik biçimleri (parmak izleri) aynıysa aynıdır; değilse "farklı bir istek"tir ve 422 alır. Parmak izi, handler'ın istekten okuduğu her şeyin kanonik JSON'udur (taslak §2.4'ün "SHOULD document"ı; sözleşmede `IdempotencyKey`, `UnprocessableContent` ve `ErrorCode`'da yazılı):

- doğrulanmış gövde (stil rehberi §4: kin şema öntanımlarını yazmıştır; öntanımlı üyeyi göndermeyen istekle öntanımı gönderen istek aynıdır);
- yol ve sorgu parametreleri;
- oturumun bağlandığı istemci başlıkları (`X-Device-Id`, `X-Client-Platform`).

Kenarın denetlediği başlıklar (sürüm, istek kimliği) ve User-Agent buna girmez, çünkü operasyonun ne yaptığını değiştirmezler.

Kanonik JSON RFC 8785'e (JCS) yakındır; sayılar değerle yazılır (`1`, `1.0` ve `1e0` aynıdır). JCS'den dört farkı vardır (`canonical.go`, `TestCanonicalJSON`):

- Üyeler UTF-8 bayt sırasıyla sıralanır, JCS UTF-16 kod birimi sırasını kullanır; iki sıra yalnız BMP dışı ve U+E000 üstü anahtarlarda ayrışır.
- U+2028 ve U+2029 kaçışlanır (`encoding/json` onları her zaman kaçışlar); JCS onları olduğu gibi yazar.
- int64'e sığan tam sayı rakamlarıyla yazılır; JCS float64'ten geçirir. Bu yüzden 2^53 üstünde `9007199254740993` kendisi kalır, `9007199254740993.0` ise float64'tür (`9007199254740992`) ve farklı bir özet verir.
- Unicode normalizasyonu yapılmaz (JCS de yapmaz): bir adın NFC ve NFD yazımları iki ayrı istektir.

Parmak izinin tek işi her değere tek bir biçim vermektir. Yeniden deneyen istemci kendi baytlarını yeniden gönderir; bu farkların hiçbiri bir tekrarı aslından ayırmaz. Çözülmüş JSON'da bulunmayan bir tip hatadır ve 500 döner, sessizce başka bir şey olarak özetlenmez.

Saklanan `request_hash`, kanonik biçimin HMAC-SHA256'sıdır. Anahtar giriş biberinden HKDF ile türetilir (`"koala-api idempotency request hash v1"`; ADR-0025 §5'in biberi, ADR-0033'ün türetme kalıbı). Sızan bir tablo, tahmin edilen bir gövdeyi doğrulamaz. `completeRegistration`'ın gövdesi parolayı taşıdığı için bu ayrıca önemlidir.

### 3. Saklanan yanıtın gizliliği

`completeRegistration`'ın yanıtı yeni oturumun token çiftidir. Yanıtı düz saklamak, refresh token'ı veritabanında açık tutmak demekti. ADR-0025 ise refresh token'ı yalnız özetiyle saklar. Üç seçenek değerlendirildi:

| Seçenek | Sonuç |
|---|---|
| **Token'ı yeniden basmak** (tekrarda yeni çift) | Reddedildi. Tekrarın kendisi bir yan etki olur. Eşzamanlı iki gönderimde ilk çiftin refresh token'ı geçersizleşir ve kullanıcı onu kullanınca ADR-0025'in yeniden kullanım tespiti oturum ailesini kapatır. Sözleşmenin "ilk yanıt aynen döner" vaadi de bozulur. |
| **Yalnız referans saklamak** (oturum kimliği) | Reddedildi. Refresh token özetinden geri üretilemez, yani bu seçenek yeniden basmaya döner. |
| **Şifreli saklamak** | **Seçildi**, anahtarı istekten türeterek. |

**Mühür:** yanıt (durum, temsil başlıkları, gövde) AES-256-GCM ile şifrelenir. Anahtar `HKDF-SHA256(gizli = kanonik istek, tuz = biberden türeyen tuz, bilgi = "koala-api idempotency response key v1")`'dir. Ek doğrulanan veri (AAD) satırın kimliğidir (aktör, operasyon, anahtar); kopyalanan bir mühür başka satırda açılmaz. Nonce her mühürde rastgeledir.

Sonuç olarak yanıtı yalnız **aynı isteğin tekrarı** açabilir:

- Veritabanı tek başına açamaz.
- Biber tek başına açamaz.
- `completeRegistration`'da biber ve veritabanı birlikte de açamaz, çünkü kanonik istek 256 bitlik kayıt token'ını ve parolayı taşır. İkisi de saklanmaz; token yalnız özetiyle saklanır.

Sunucu tarafında ayrıca bir anahtar yönetimi yoktur: anahtar istekte gelir ve işi biten istekle gider. Gövdesi düşük entropili bir operasyonda (ör. `{}` gövdeli, çağıranı olan bir uç) koruma biberin gizliliğine iner. Bu yine düz saklamadan güçlüdür.

**Bayat refresh token ve istemci kuralı:** tekrar, ilk yanıtın çiftini (R1) taşır. **Elinde token çifti olan istemci `Idempotent-Replayed` yanıtındaki çifti kullanmaz** (sözleşmede `IdempotencyKey`'in istemci notu, stil rehberi §7). Neden koruyucu: tekrarı alan iki istemci vardır.

- **İlk yanıtı kaybetmiş istemci** R1'i hiç almadı ve hiç kullanmadı; tekrardaki çift onun için tazedir. Yanıtı kaybolan yeniden denemenin amacı da budur.
- **İlk yanıtı almış istemci** (ör. iki kez gönderen) R1'i almış ve belki yenilemiştir (R1 → R2). Tekrardaki R1'i sunarsa ADR-0025'in yeniden kullanım tespiti onu çalıntı sayar ve oturum ailesini kapatır. Bu istemci zaten geçerli bir çifte sahiptir, tekrardakini atarak hiçbir şey kaybetmez.

Kural, depoyu değiştirmeden ikinci durumu güvenli kılar. Tekrarda çifti yeniden basmak bu sorunu çözmezdi; yukarıdaki tablonun gerekçesiyle reddedildi.

**Bedeli:** Biber değişirse saklanan her anahtar "başka istek" görünür ve kalan ömrü boyunca, yani en çok 24 saat, tekrar 422 alır. Biber yalnız sızma şüphesinde döndürülür; işletim notu `docs/runbook/yayin.md`'nin "Giriş biberinin değişmesi" maddesindedir. `local` ve `test` ortamında biber yoksa her süreç kendi biberini üretir; yeniden başlatmadan önce alınmış anahtarlar o ortamda 422 döner.

### 4. Transaction birleşimi

Akış üç adımdır:

1. **Talep, kendi kısa tx'inde** (`Store.claim`, middleware handler'dan önce):
   - `INSERT … ON CONFLICT DO NOTHING` ile satır `IN_PROGRESS` yazılır. Satır yeni bir `attempt` kimliği ve `locked_until = şimdi + Lease` taşır.
   - Satır varsa `SELECT … FOR UPDATE` ile kilitlenir ve karar satıra göre verilir. Sıra şöyledir: süresi dolmuşsa anahtar yenidir; özet farklıysa 422; `COMPLETED` ise tekrar; kira sürüyorsa 409; kira bitmişse devralınır.
   - Özet durumdan önce denetlenir. Başka istek, ilki sürerken de 422 alır.
   - Eşzamanlı iki `INSERT`'te ikincisi birincinin tx'ini bekler. Bu yüzden aynı anahtarla iki istek aynı anda talep alamaz.
2. **Sonuç, iş tx'inin içinde:** handler başarı yanıtını operasyonun kendi tx'inde `idempotency.Commit(ctx, q, render)` ile saklar. `render`, handler'ın döndüreceği yanıt nesnesinin üretilmiş `Visit…Response` metodudur; yanıt bu yüzden handler'ın yazacağıyla bayt bayt aynıdır. Yazım koşulludur: `WHERE attempt = $talep AND state = 'IN_PROGRESS'`.
   - Talep başka bir denemeye geçmişse hiçbir satır değişmez. `Commit` 409 `IDEMPOTENCY_KEY_IN_USE` döner ve iş tx'i geri alınır.
   - Satır kilidi commit'e kadar tutulur. Devralmaya çalışan istek bu kilidi bekler ve `COMPLETED` görür.
   - Servis, bu adımı tx'inin sonunda çağırdığı bir parametre olarak alır (`Accounts.CompleteRegistration`'ın `commit`'i). Katman yönü `http → service` korunur: servis HTTP'yi ve depoyu bilmez.
3. **Handler'dan sonra** (middleware, isteğin iptalinin ulaşmadığı bir context'te):
   - çağıranı olan operasyonun kalıcı 4xx'i kısa bir yazımla saklanır (Karar 5);
   - 5xx ve geçici hata anahtarı koşullu bir `DELETE` ile serbest bırakır (`… AND attempt = $talep AND state = 'IN_PROGRESS'`).

**Commit belirsizliği (ADR-0017):** "işlendi" kaydı iş verisiyle aynı tx'tedir, yani ikisi birlikte commit olur ya da hiçbiri olmaz.

- Bağlantı COMMIT'ten sonra koparsa handler 500 döner ve middleware serbest bırakmayı dener.
- Satır `COMPLETED` ise koşullu `DELETE` ona dokunmaz. Yeniden deneme saklanan başarıyı alır: iş bir kez oldu, istemci yanıtını aldı.
- Tx gerçekten geri alındıysa satır `IN_PROGRESS`'tir ve serbest kalır; yeniden deneme işi yapar.

Bunu `TestAmbiguousCommitIsAnsweredByTheStoredSuccess` sınar.

**Aynı tx'in ölçüsü:** iş satırı ile anahtar satırının `xmin`'i eşittir. Anahtar satırını talep ekler, son olarak da yanıtı saklayan tx günceller. `Commit` iş tx'inde koştuysa ikisinin `xmin`'i aynı tx kimliğidir; kendi tx'inde koştuysa farklıdır. `TestCommitSharesTheOperationsTransaction` iki yönü de gösterir: doğru handler'da eşittir, `Commit`'i ayrı bir `InTx`'te çağıran sahte handler'da farklıdır (ve o handler da 201 döner, yani sayaçlar onu ayıramazdı). Dinamik bağ testi her bağlı operasyonda bunu ölçer (Karar 6.3).

**`Commit`'te ek savunma olarak "tx'te yazım var mı" denetimi eklenmedi.** `pg_current_xact_id_if_assigned() IS NOT NULL`, `Commit`'ten önce tx'te bir yazım olduğunu gösterir. Ama yan etkisi var: yazımsız başarı yanıtı veren meşru bir handler'ı (ör. "zaten onaylı" durumunu 200 ile bildiren bir onay ucu) 500'e çevirir. Ayrı tx'teki `Commit`'i de yakalamaz, ayrı tx'te önce bir yazım yapılmışsa. Ölçü testtedir (`xmin`), çalışma zamanında değil.

**Stream route'ları:** kira, isteğin `httpx.RequestTimeout`'tan uzun yaşamadığı varsayımına dayanır. `Router.Stream` route'ları bu sınırın dışındadır. Bu yüzden middleware, bağlı bir operasyonun isteğini context'inde kiradan kısa bir süre sınırı yoksa anahtar almadan 500 ile reddeder (`TestUnboundedRequestIsRefused`). `TestIdempotentOperationsAreBounded` da hiçbir anahtarlı operasyonun Stream route olmadığını gerçek router'da sınar; yürüyüşün gerçekten Stream route gördüğünü realtime ucuyla kanıtlar.

**Kira (lease):** `Lease = httpx.RequestTimeout + 35 sn` (60 sn).

- `RequestTimeout` isteğin context'ini ve onunla tx'ini sınırlar. Kirası bitmiş bir talep, artık koşmayan bir isteğindir: çökmüş bir örnek ya da veritabanına ulaşamamış bir serbest bırakma.
- Aynı anahtarla gelen sonraki istek talebi devralır. Yeni `attempt` eski isteğin tx'ini çitler, yani eski isteğin geç kalan `Commit`'i satırı değiştiremez ve geri alınır.
- Bunu `TestLeaseExpiryTakesTheKeyOverAndFencesTheOldAttempt` sınar. Kira içinde aynı istek 409, kira bitince devralma vardır ve eski deneme 409 ile geri alınır; yan etki bir kez olur.
- Bu testte yeni deneme eski denemeden önce bitiyordu. Satır `COMPLETED` olduğu için `CompleteKey`'in `state` koşulu tek başına eski denemeyi durduruyordu, yani `attempt` koşulu silinse test yeşil kalırdı. Her çit koşulunu yalnız kendisinin yakaladığı iki test bağlar:
  - `TestOldAttemptCannotCommitOverARunningTakeover`: yeni deneme hâlâ `IN_PROGRESS` iken eski deneme commit etmeye çalışır ve 409 alır. `CompleteKey`'den `attempt = $4` silinirse eski deneme commit eder ve test kırmızı olur.
  - `TestOldAttemptReleaseSparesTheTakeover`: devralmadan sonra eski denemenin 5xx'i serbest bırakmaya çalışır; yeni denemenin satırı kalır ve başarısı saklanır. `ReleaseKey`'den `attempt = $4` silinirse yeni satır silinir ve test kırmızı olur.

**Bağlanmamış başarı:** başarı döndüğü hâlde `Commit` çağrılmamışsa middleware yanıtı handler'dan sonra saklar. Bu yazım atomik değildir. Olay `koala_idempotency_requests_total{result="unbound"}` sayacına ve hata log'una yazılır, `KoalaIdempotencyUnbound` uyarısı çalar. Bu bir bağlama hatasıdır; Karar 6'nın iki testi onu birleşmeden önce yakalar.

### 5. Hangi yanıtlar saklanır

Seçim Stripe'ın kuralı ve taslağın 409/422 ayrımıyla yapıldı; Stripe'tan iki yerde ayrılır (aşağıda):

| Yanıt | Anahtar | Gerekçe |
|---|---|---|
| 2xx | saklanır (iş tx'inde) | Tekrar aynı sonucu alır. |
| 4xx (408, 425, 429 hariç), çağıranı olan operasyon | saklanır (handler'dan sonra) | Yürütmeye başlamış istek hiçbir şeyi değiştirmedi; aynı istek aynı biçimde reddedilir (Stripe). |
| 4xx, çağıranı olmayan operasyon | serbest kalır | Karar 5a. |
| 5xx, 408, 425, 429, 3xx | serbest kalır | Geçicidir; istemci aynı anahtarla yeniden dener. 5xx için Karar 5b. |
| Doğrulamada (400, 413, 415), kimlik doğrulamada (401, 403) ya da kökün ve kimlik başına oran sınırında (429) reddedilen | anahtar hiç alınmaz | Middleware bunlardan sonra gelir (Stripe: doğrulamada reddedilen istek saklanmaz). Strict sunucunun içindeki hedef kovası (`authn.StrictMiddleware`) ise sonra gelir; onun 429'u anahtarı serbest bırakır. |
| 409 `IDEMPOTENCY_KEY_IN_USE`, 422 `IDEMPOTENCY_KEY_REUSED`, 429 (Karar 1'in kovası) | saklanmaz | Depo kendisi üretir. Çitlenmiş bir denemenin 409'unun koşullu yazımı hiçbir satırı değiştirmez. |
| 1 MiB'ı aşan yanıt | saklanmaz, anahtar serbest kalır | Karar 5c. |

**5a. Çağıranı olmayan operasyonda yalnız başarı saklanır.** Rastgele bir token ve yeni bir anahtarla gelen her `completeRegistration` isteği bir 400 alır. 4xx saklansaydı her biri 24 saatlik bir satır yazardı: kökün adres sınırıyla adres başına günde yaklaşık 43 bin satır ve 51 MB, saldırganın doldurduğu bir depolama. Bu operasyonlarda 4xx anahtarı serbest bırakır (`TestPublicRefusalWritesNoKey`: tahmin edilen token isteklerinden sonra tablo boştur). `completeRegistration`'da anlam değişmez: 4xx'te iş verisi yazılmamıştır (tx geri alınmış ya da boş commit olmuştur), ve aynı isteğin yeniden koşması yine aynı 4xx'i verir.

**5b. 5xx'te anahtar bırakılır; Stripe'tan bilinçli ayrılık.** Stripe 500'ü de saklar, çünkü 500'ün ardında yarım kalmış bir yan etki olabilir ve yeniden koşmak onu ikiler. Burada başarının kaydı iş verisiyle aynı tx'tedir (Karar 4). 5xx ya tx'in geri alındığı (yan etki yok, yeniden koşmak güvenli) ya da commit'in belirsiz kaldığı (satır `COMPLETED`, koşullu serbest bırakma ona dokunmaz, yeniden deneme saklanan başarıyı alır) anlamına gelir. Atomik commit, Stripe'ın 500'ü saklama gerekçesini ortadan kaldırır; istemci geçici bir hatadan sonra aynı anahtarla yeniden deneyebilir. **Para operasyonlarında yeniden bakılır:** dış çağrı (Iyzico) tx dışındadır (ADR-0012), ve dış çağrıdan sonraki bir 5xx'in ardında geri alınamayan bir yan etki olabilir.

**5c. Büyük yanıt.** Saklanan gövde en çok 1 MiB'tır (`maxStoredBody`). Sınır iki yolda da geçerlidir:
- **`Commit`'te:** yanıt sınırı aşarsa `Commit` yanıtı yazmaz, ama çiti yine uygular. `FenceKey` (`… WHERE attempt = $talep AND state = 'IN_PROGRESS'`) iş tx'inde satırı kilitler. Talep başka bir denemeye geçmişse hiçbir satır değişmez, `Commit` 409 döner ve iş tx'i geri alınır. Böylece devralınmış eski bir deneme, yanıtı büyük diye yan etkiyi ikinci kez commit edemez (`TestOversizedOldAttemptIsFenced`). Çit tutarsa iş tx'i commit olur;
- **Handler'dan sonra:** aynı biçimde.

Middleware yanıtı gönderir, anahtarı serbest bırakır, hata log'una "too large to keep" yazar ve `result="too_large"` sayar. Yanlış bir "differs" log'u düşmez (`TestLargeResponseIsNotKept`). Yeniden deneme işi yeniden yapar. Bağlı operasyonlar küçük JSON belgeleri döner, bu yüzden bu bir bağlama hatası sinyalidir.

**Saklanan 4xx'in bayatlığı (kabul edilen sonuç):** çağıranı olan bir operasyonun saklanan 403'ü ya da 404'ü 24 saat boyunca aynı anahtarla aynen döner. Bu, durum değişse de geçerlidir (ör. yetki sonradan verildi). İstemci yeni bir mantıksal işlem için yeni anahtar üretir (taslak ve Stripe'ın kuralı); aynı anahtarla aynı reddi alması, Stripe modelinin beklenen sonucudur.

Tekrar şunları taşır: saklanan durum, temsil başlıkları (`Content-Type`, `Content-Language`, `Location`, `ETag`, `Last-Modified`) ve gövde, ayrıca `Idempotent-Replayed: true` ve `Cache-Control: no-store`. `Idempotent-Replayed` sözleşmede `headers.yaml#/components/headers/IdempotentReplayed` olarak tanımlıdır ve bağlı operasyonun başarı yanıtı onu referanslar; `completeRegistration`'ın 201'i referanslar (uyumlu genişleme). `X-Request-Id` tekrarın kendi kimliğidir. Saklanan bir problem belgesinin gövdesindeki `requestId` ise ilk isteğinkidir, çünkü tekrar bayt bayt aynıdır.

### 6. Makine denetimi: bağ, sözleşmeden

Depo, operasyonları elle yazılmış bir listeden değil sözleşmeden okur (`idempotency.Operations`): `Idempotency-Key` başlık parametresini alan ve `x-not-implemented` taşımayan her operasyon bağlıdır. Bir handler PR'ı işareti kaldırdığında operasyon **kendiliğinden** bağlanır ve aşağıdaki dört denetimin hepsine girer:

1. **Açılış (fail-closed):** çağıranı olmayan bağlı operasyonun aktörü ya da kovası yoksa sunucu açılmaz (Karar 1). Gerçek sözleşme üzerinde `TestIdempotencyBindsTheContract` sınar: `completeRegistration` bağlıdır, girdisi silinince açılış reddedilir.
2. **Statik yarı:** `archtest.TestIdempotentHandlersCommitTheirKey`, ADR-0032 Karar 1'in iz yürüyüşünü kullanır.
   - Her bağlı operasyonun handler izi `Commit`'in çitli yazımına ulaşmalıdır. Bu, `internal/platform/idempotency`'deki `KindConflict IDEMPOTENCY_KEY_IN_USE` yeridir.
   - Anahtar almayan hiçbir handler o yere ulaşmamalıdır. Böylece her handler'ın paylaştığı bir yol denetimi yeşil tutamaz: bir handler'ın `Commit`'i silinirse test kırmızı olur.
   - Anahtarlı bir operasyonun tam bir strict handler'ı yoksa kırmızıdır; sessizce atlanmaz.
   - Boş kümede geçmez: `completeRegistration` bağlı listede olmalı ve anahtarsız en az 10 handler izlenmiş olmalıdır.
3. **Dinamik yarı:** `app.TestIdempotentOperationsReplay` (integration) her bağlı operasyonu gerçek router ve veritabanı üzerinde koşturur:
   - ilk yanıt başarıdır, ve anahtar satırının `xmin`'i fikstürün adlandırdığı iş satırınınkine eşittir: yanıt iş tx'inde saklanmıştır (Karar 4'ün ölçüsü). Sayaçlar da `committed` 1, `unbound` 0 olmalıdır;
   - ikinci gönderim ilk yanıtın bayt bayt aynısıdır ve tekrar işareti taşır.

   Fikstür listesi sözleşmenin bağlı listesine eşit olmak zorundadır. Fikstürü olmayan yeni operasyon kırmızıdır, fikstürü kalmış eski operasyon da kırmızıdır; boş liste kırmızıdır. Statik yürüyüş `Commit`'in tx içinde çağrıldığını göremez, bu test `xmin` ile ölçer.
4. **Sözleşme** (`app.TestIdempotentOperationsDeclareTheirRefusals`):
   - anahtar alan her operasyon (donmuş olanlar dahil) 409 ve 422 yanıtlarını tanımlar. `startMediaUpload`, `createMyAddress` ve `createMyEducation`'da 409 eksikti; üçüne de `Conflict` yanıtı eklendi. Bu uyumlu bir genişlemedir, çünkü üç operasyon da henüz 501 döner;
   - bağlı operasyonun başarı yanıtı `Idempotent-Replayed`'i tanımlar;
   - anahtar almayan operasyon 422 tanımlamaz ve deponun kodlarını `x-error-codes`'ta listelemez.
5. **Kodlar yalnız depoda** (`archtest.TestIdempotencyCodesStayInTheStore`): depo paketi dışında hiçbir paket `CodeIdempotencyKey*` sabitlerini adlandırmaz ve kod değerlerini dize olarak yazmaz (Karar 7).

### 7. Hata modeli ve kodlar

- `apperr.KindUnprocessable` → 422 eklendi. ADR-0005 *Sonuçlar*'ı bunu idempotency PR'ına bırakmıştı. Yalnız `IDEMPOTENCY_KEY_REUSED` için kullanılır; alan kuralı ihlali 400 kalır (stil rehberi §5).
- `IDEMPOTENCY_KEY_REUSED` ve `IDEMPOTENCY_KEY_IN_USE` deponun kodlarıdır (`idempotency.Codes()`, sözleşme testinde okunur). Evrensel oldukları küme dardır: **yalnız `idempotency.Operations`'ın operasyonlarında** evrenseldirler ve onların `x-error-codes`'unda listelenmezler (`TestHandlersReturnTheirDeclaredErrorCodes`'un `keyedCodes`'u). Anahtarsız bir operasyonun handler izi onlara ulaşırsa izleme onları liste dışı kod olarak bulur. Alan paketlerinin onları kullanması ve anahtarsız operasyonun onları listelemesi ya da 422 tanımlaması kırmızıdır (Karar 6.4 ve 6.5). Böylece istemci bu kodları gördüğünde onların yalnız anahtar hakkında olduğunu bilir.
- **`IDEMPOTENCY_KEY_REQUIRED` sözleşmeden ve stil rehberinden çıkarıldı** (Yusuf'un kararı). Hiç dönmüyordu: başlığı olmayan ya da biçimi bozuk istek doğrulayıcıda 400 `REQUEST_INVALID` + `FIELD_REQUIRED`/`FIELD_VALUE_INVALID` (`parameter: Idempotency-Key, in: HEADER`) alır. Bu, başka her eksik başlığın yanıtıyla aynıdır. İstemci yok, kodu kaldırmanın en ucuz anı bu. Ölü bir kod, istemcileri hiç gelmeyecek bir dalı yazmaya zorlardı.

### 8. Tablo ve saklama

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `idempotency_keys` | M2, `pii` | Anahtar başına özet ve mühürlü yanıt. 24 saat (Stripe) sonra hard delete. Bu, `idempotency_purge` River periyodik işidir: saatte bir ve açılışta koşar, `expires_at`'ı geçmiş satırları `koala_app` olarak siler. Süresi dolmuş ama henüz silinmemiş satırın anahtarı yenidir (`RestartKey`). `pii`: aktör kullanıcı ya da personel kimliğidir, ve gelecekteki bir operasyonun yanıtı kişisel veri taşıyabilir (ör. bir adres). Şifreli kişisel veri de kişisel veridir, bu yüzden tablo saklama envanterindedir. `completeRegistration`'ın yanıtı kişisel veri taşımaz, token taşır. |

`koala_app`'in yetkisi `SELECT, INSERT, UPDATE, DELETE`'tir (`TestRoleGrants`). Soft-delete'li tabloya FK yoktur. Birincil anahtar plan §4.1-6'nın üçlüsüdür.

## Kapının bu PR'daki gerçek vakaları

- `completeRegistration` bağlı tek operasyondur. Statik ve dinamik yarı onu gerçek sözleşmeden bulur ve üç teste girer: `TestRegistrationKeyBelongsToTheToken` (başkasının anahtarıyla saklanan yanıt okunamaz, tablo token'ları düz taşımaz), `TestConcurrentRegistrationCompletions` (eşzamanlı aynı anahtar: bir kayıt, bir oturum) ve `TestIdempotentOperationsReplay`.
- Sözleşme denetimi üç donmuş operasyonda eksik 409'u buldu (Karar 6.4).
- İnceleme beş açık buldu, hepsi bu ADR'de kapandı:
  - kimliksiz uçta saklanan 4xx'in doldurduğu depolama (Karar 5a);
  - token kovasını atlayan parola kâhini (Karar 1);
  - "aynı tx" iddiasını ölçmeyen dinamik test (Karar 4'ün `xmin` ölçüsü);
  - birbirini maskeleyen çit koşulları (Karar 4'ün iki testi);
  - anahtarlı operasyonu sessizce atlayan statik denetim (Karar 6.2).

## Yerine geçtiği

1. **ADR-0005 *Sonuçlar*'ındaki "422 için `apperr`'e yeni `Kind` … idempotency PR'ında yapılır"** → Karar 7 ile karşılandı (426 ayrı kalır; ADR-0019 onu 400 `APP_VERSION_UNSUPPORTED` yaptı).
2. **ADR-0032 *Sonuçlar*'ındaki "Idempotency deposu ayrı bir platform PR'ının işidir … `completeRegistration`'ın açıklaması bugünkü davranışı yazar"** → bu ADR ile karşılandı. Sözleşmedeki "bugünkü davranış" paragrafı kaldırıldı; açıklama ADR-0032 ve bu ADR'ye atıf verir.
3. **Stil rehberi §7'nin "Depo henüz uygulanmadı" cümlesi** (ADR-0032 Yerine geçtiği 6) → gerçeğe çekildi.
4. **ADR-0007 *Sonuçlar*'ındaki "422 (Idempotency-Key gövde uyuşmazlığı, ADR-0005) … için yeni `Kind` gerekecek"** → Karar 7 ile kapandı (`KindUnprocessable`). Aynı maddenin 426'sı ADR-0019 ile 400 olarak kapanmıştı.
5. **Stil rehberi §5'in 400 satırındaki `IDEMPOTENCY_KEY_REQUIRED`** → çıkarıldı (Karar 7).

## Kabul edilen riskler

- **Talep, oran sınırından önce gelir.** `authn.StrictMiddleware`'in token kovası strict sunucunun içindedir, yani middleware'den sonra. Oran sınırına takılan bir `completeRegistration` isteği önce bir talep tx'i ve ardından bir serbest bırakma yazar. Kökteki adres başına sınır bundan önce gelir; maliyet iki kısa yazımdır ve satır kalmaz (Karar 5a).
- **Saklanan 4xx 24 saat bayat kalabilir** (Karar 5).
- **Büyük yanıtın anahtarı serbest kalır** (Karar 5c). Yeniden deneme işi yeniden yapar; log ve `too_large` sayacı bunu bildirir.
- **Bağlanmamış başarı atomik değildir** (Karar 4). Birleşmeden önce iki test, sonra uyarı yakalar.

## Sonuçlar

- `Idempotency-Key` alan her operasyonun handler PR'ı başarı yanıtını kendi tx'inde `idempotency.Commit` ile saklar, başarı yanıtına `Idempotent-Replayed`'i ekler ve `app.idempotentFixtures`'a iş satırını adlandıran bir fikstür ekler. Çağıranı yoksa `idempotencyConfig`'e aktörünü ve kovasını yazar. Biri unutulursa kapı kırmızıdır.
- Uyarı: `KoalaIdempotencyUnbound` (`ops/monitoring/prometheus-alerts.yml`).

## Açık sorular

1. **Saklama süresi** Stripe'ın 24 saatidir. Mobil istemcinin yeniden deneme penceresi ölçülmedi. Daha uzun bir çevrimdışı kuyruk gelirse süre gözden geçirilir.
2. **Para operasyonları** (ödeme oturumu, iade) geldiğinde Commit'in "son adım" kuralı, ADR-0012'nin "rezerve et → commit → dış çağrı → kısa tx" akışıyla birleşir: başarı yanıtı son kısa tx'te saklanır. Bunun Iyzico'nun kendi idempotency anahtarıyla birlikte tasarımı ödeme PR'ının işidir.
