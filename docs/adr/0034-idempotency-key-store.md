# ADR-0034: Idempotency-Key deposu

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: issue #22
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.5 (transaction), §4.1-6 (`idempotency_keys`); [ADR-0005](0005-api-contract-principles.md) madde 6 ve *Sonuçlar*, [ADR-0007](0007-error-model.md), [ADR-0012](0012-data-access-and-migrations.md), [ADR-0013](0013-soft-delete.md), [ADR-0017](0017-river-goose-db-roles-and-test-db.md) (commit belirsizliği), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) (doğrulanmış değer), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §5 (giriş biberi), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1 ve *Sonuçlar*; stil rehberi §4 ve §7
- Dış kaynaklar: IETF `draft-ietf-httpapi-idempotency-key-header` (anahtar, parmak izi, 409/422); Stripe API "Idempotent requests" (aktör ve anahtar, 24 saat, `Idempotent-Replayed`, doğrulamada reddedilen istek saklanmaz); RFC 8785 (JSON kanonikleştirme); RFC 5869 (HKDF); NIST SP 800-38D (GCM); KVKK (şifreli kişisel veri kişisel veridir)
- Kod: `internal/platform/idempotency`, `internal/platform/httpx/idempotency.go`, `internal/platform/apperr` (`KindUnprocessable`), `internal/authn/http.go` (`CompleteRegistration`, `RegistrationTokenActor`), `internal/authn/accounts.go`, `internal/app/api.go` (`idempotencyConfig`), `internal/app/workers.go` (`idempotency_purge`), `internal/archtest/idempotency_test.go`, `db/migrations/0044_idempotency_keys.sql`, `db/queries/idempotency/keys.sql`, `ops/monitoring/prometheus-alerts.yml` (`KoalaIdempotencyUnbound`)

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

Token'ın özeti, kimliksiz uçta başkasının yanıtını okumayı kapatır. Anahtarı bilen ama token'ı bilmeyen saldırgan başka bir aktörün satırına düşer: kendi isteği yeni bir istek olarak işler ve `REGISTRATION_TOKEN_INVALID` alır. Token'ı çalan ama parolayı bilmeyen saldırgan aynı satıra düşer, ama gövdesi başka olduğu için 422 alır (Karar 2). Token zaten operasyonun tükettiği yetkidir: onu tutan, kaydı kendisi de tamamlayabilirdi. Depo bu yetkiye bir şey eklemez.

**Kural:** Çağıranı olmayan ve anahtar alan her operasyon `idempotency.Config.Public`'te kendi aktörünü adlandırır. Adlandırmayan böyle bir operasyon, çağıranı olan bir operasyona verilmiş istek aktörü ya da bağlı olmayan bir operasyonun girdisi sunucuyu açtırmaz (`NewMiddleware`, `TestNewMiddlewareBindsTheContract`, `TestIdempotencyBindsTheContract`).

### 2. İstek özeti

İki istek, kanonik biçimleri aynıysa aynıdır. Kanonik biçim, handler'ın istekten okuduğu her şeyin kanonik JSON'udur:

- doğrulanmış gövde (stil rehberi §4: kin şema öntanımlarını yazmıştır; öntanımlı üyeyi göndermeyen istekle öntanımı gönderen istek aynıdır);
- yol ve sorgu parametreleri;
- oturumun bağlandığı istemci başlıkları (`X-Device-Id`, `X-Client-Platform`).

Kenarın denetlediği başlıklar (sürüm, istek kimliği) ve User-Agent buna girmez, çünkü operasyonun ne yaptığını değiştirmezler.

Kanonik JSON RFC 8785'tir; tek fark üye sırasıdır. Üyeler UTF-8 bayt sırasıyla sıralanır, RFC 8785 ise UTF-16 kod birimi sırasını kullanır; iki sıra yalnız BMP dışı ve U+E000 üstü anahtarlarda ayrışır. Sayılar değerle yazılır: `1`, `1.0` ve `1e0` aynıdır. Çözülmüş JSON'da bulunmayan bir tip hatadır ve 500 döner, sessizce başka bir şey olarak özetlenmez.

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

**Bedeli:** Biber değişirse saklanan her anahtar "başka istek" görünür ve tekrar 422 alır. Biber, anahtarın 24 saatlik ömrü içinde döndürülmez. `local` ve `test` ortamında biber yoksa her süreç kendi biberini üretir; yeniden başlatmadan önce alınmış anahtarlar o ortamda 422 döner.

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
   - kalıcı 4xx kısa bir yazımla saklanır;
   - 5xx ve geçici hata anahtarı koşullu bir `DELETE` ile serbest bırakır (`… AND attempt = $talep AND state = 'IN_PROGRESS'`).

**Commit belirsizliği (ADR-0017):** "işlendi" kaydı iş verisiyle aynı tx'tedir, yani ikisi birlikte commit olur ya da hiçbiri olmaz.

- Bağlantı COMMIT'ten sonra koparsa handler 500 döner ve middleware serbest bırakmayı dener.
- Satır `COMPLETED` ise koşullu `DELETE` ona dokunmaz. Yeniden deneme saklanan başarıyı alır: iş bir kez oldu, istemci yanıtını aldı.
- Tx gerçekten geri alındıysa satır `IN_PROGRESS`'tir ve serbest kalır; yeniden deneme işi yapar.

Bunu `TestAmbiguousCommitIsAnsweredByTheStoredSuccess` sınar.

**Kira (lease):** `Lease = httpx.RequestTimeout + 35 sn` (60 sn).

- `RequestTimeout` isteğin context'ini ve onunla tx'ini sınırlar. Kirası bitmiş bir talep, artık koşmayan bir isteğindir: çökmüş bir örnek ya da veritabanına ulaşamamış bir serbest bırakma.
- Aynı anahtarla gelen sonraki istek talebi devralır. Yeni `attempt` eski isteğin tx'ini çitler, yani eski isteğin geç kalan `Commit`'i satırı değiştiremez ve geri alınır.
- Bunu `TestLeaseExpiryTakesTheKeyOverAndFencesTheOldAttempt` sınar. Kira içinde aynı istek 409, kira bitince devralma vardır ve eski deneme 409 ile geri alınır; yan etki bir kez olur.

**Bağlanmamış başarı:** başarı döndüğü hâlde `Commit` çağrılmamışsa middleware yanıtı handler'dan sonra saklar. Bu yazım atomik değildir. Olay `koala_idempotency_requests_total{result="unbound"}` sayacına ve hata log'una yazılır, `KoalaIdempotencyUnbound` uyarısı çalar. Bu bir bağlama hatasıdır; Karar 6'nın iki testi onu birleşmeden önce yakalar.

### 5. Hangi yanıtlar saklanır

Seçim Stripe'ın kuralı ve taslağın 409/422 ayrımıyla yapıldı:

| Yanıt | Anahtar | Gerekçe |
|---|---|---|
| 2xx | saklanır (iş tx'inde) | Tekrar aynı sonucu alır. |
| 4xx (408, 425, 429 hariç) | saklanır (handler'dan sonra) | İstek hiçbir şeyi değiştirmedi. Aynı istek aynı biçimde reddedilir. |
| 5xx, 408, 425, 429, 3xx | serbest kalır | Geçicidir; istemci aynı anahtarla yeniden dener. |
| Doğrulamada (400, 413, 415), kimlik doğrulamada (401, 403) ya da kökün ve kimlik başına oran sınırında (429) reddedilen | anahtar hiç alınmaz | Middleware bunlardan sonra gelir (Stripe: doğrulamada reddedilen istek saklanmaz). Strict sunucunun içindeki hedef kovası (`authn.StrictMiddleware`) ise sonra gelir; onun 429'u anahtarı serbest bırakır. |
| 409 `IDEMPOTENCY_KEY_IN_USE`, 422 `IDEMPOTENCY_KEY_REUSED` | saklanmaz | Depo kendisi üretir. Çitlenmiş bir denemenin 409'unun koşullu yazımı hiçbir satırı değiştirmez. |

Tekrar şunları taşır: saklanan durum, temsil başlıkları (`Content-Type`, `Content-Language`, `Location`, `ETag`, `Last-Modified`) ve gövde, ayrıca `Idempotent-Replayed: true` ve `Cache-Control: no-store`. `X-Request-Id` tekrarın kendi kimliğidir. Saklanan bir problem belgesinin gövdesindeki `requestId` ise ilk isteğinkidir, çünkü tekrar bayt bayt aynıdır.

Bir yanıt 1 MiB'ı aşarsa saklanmaz ve anahtar serbest kalır (log). Bağlı operasyonlar küçük JSON belgeleri döner.

### 6. Makine denetimi: bağ, sözleşmeden

Depo, operasyonları elle yazılmış bir listeden değil sözleşmeden okur (`idempotency.Operations`): `Idempotency-Key` başlık parametresini alan ve `x-not-implemented` taşımayan her operasyon bağlıdır. Bir handler PR'ı işareti kaldırdığında operasyon **kendiliğinden** bağlanır ve aşağıdaki dört denetimin hepsine girer:

1. **Açılış (fail-closed):** çağıranı olmayan bağlı operasyonun aktörü yoksa sunucu açılmaz (Karar 1). Gerçek sözleşme üzerinde `TestIdempotencyBindsTheContract` sınar: `completeRegistration` bağlıdır, aktörü silinince açılış reddedilir.
2. **Statik yarı:** `archtest.TestIdempotentHandlersCommitTheirKey`, ADR-0032 Karar 1'in iz yürüyüşünü kullanır.
   - Her bağlı operasyonun handler izi `Commit`'in çitli yazımına ulaşmalıdır. Bu, `internal/platform/idempotency`'deki `KindConflict IDEMPOTENCY_KEY_IN_USE` yeridir.
   - Anahtar almayan hiçbir handler o yere ulaşmamalıdır. Böylece her handler'ın paylaştığı bir yol denetimi yeşil tutamaz: bir handler'ın `Commit`'i silinirse test kırmızı olur.
   - Boş kümede geçmez: `completeRegistration` bağlı listede olmalı ve anahtarsız en az 10 handler izlenmiş olmalıdır.
3. **Dinamik yarı:** `app.TestIdempotentOperationsReplay` (integration) her bağlı operasyonu gerçek router ve veritabanı üzerinde koşturur:
   - ilk yanıt başarıdır ve **iş tx'inde** saklanmıştır (`committed` sayacı 1, `unbound` 0);
   - ikinci gönderim ilk yanıtın bayt bayt aynısıdır ve tekrar işareti taşır.

   Fikstür listesi sözleşmenin bağlı listesine eşit olmak zorundadır. Fikstürü olmayan yeni operasyon kırmızıdır, fikstürü kalmış eski operasyon da kırmızıdır; boş liste kırmızıdır. Statik yürüyüş `Commit`'in tx içinde çağrıldığını göremez, bu test görür.
4. **Sözleşme:** anahtar alan her operasyon (donmuş olanlar dahil) 409 ve 422 yanıtlarını tanımlar (`app.TestIdempotentOperationsDeclareTheirRefusals`). `startMediaUpload`, `createMyAddress` ve `createMyEducation`'da 409 eksikti. Üçüne de `Conflict` yanıtı eklendi. Bu uyumlu bir genişlemedir, çünkü üç operasyon da henüz 501 döner.

### 7. Hata modeli ve kodlar

- `apperr.KindUnprocessable` → 422 eklendi. ADR-0005 *Sonuçlar*'ı bunu idempotency PR'ına bırakmıştı. Yalnız `IDEMPOTENCY_KEY_REUSED` için kullanılır; alan kuralı ihlali 400 kalır (stil rehberi §5).
- `IDEMPOTENCY_KEY_REUSED` ve `IDEMPOTENCY_KEY_IN_USE` `httpx` kodlarıdır ve `PlatformCodes()`'tadır. Böylece evrenseldirler ve `x-error-codes`'ta listelenmezler (stil rehberi §8).
- `IDEMPOTENCY_KEY_REQUIRED` bugün dönmez. Başlığı olmayan istek doğrulayıcıda 400 `REQUEST_INVALID` + `FIELD_REQUIRED` alır.

### 8. Tablo ve saklama

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `idempotency_keys` | M2, `pii` | Anahtar başına özet ve mühürlü yanıt. 24 saat (Stripe) sonra hard delete. Bu, `idempotency_purge` River periyodik işidir: saatte bir ve açılışta koşar, `expires_at`'ı geçmiş satırları `koala_app` olarak siler. Süresi dolmuş ama henüz silinmemiş satırın anahtarı yenidir (`RestartKey`). `pii`: aktör kullanıcı ya da personel kimliğidir, ve gelecekteki bir operasyonun yanıtı kişisel veri taşıyabilir (ör. bir adres). Şifreli kişisel veri de kişisel veridir, bu yüzden tablo saklama envanterindedir. `completeRegistration`'ın yanıtı kişisel veri taşımaz, token taşır. |

`koala_app`'in yetkisi `SELECT, INSERT, UPDATE, DELETE`'tir (`TestRoleGrants`). Soft-delete'li tabloya FK yoktur. Birincil anahtar plan §4.1-6'nın üçlüsüdür.

## Kapının bu PR'daki gerçek vakaları

- `completeRegistration` bağlı tek operasyondur. Statik ve dinamik yarı onu gerçek sözleşmeden bulur ve üç teste girer: `TestRegistrationKeyBelongsToTheToken` (başkasının anahtarıyla saklanan yanıt okunamaz, tablo token'ları düz taşımaz), `TestConcurrentRegistrationCompletions` (eşzamanlı aynı anahtar: bir kayıt, bir oturum) ve `TestIdempotentOperationsReplay`.
- Sözleşme denetimi üç donmuş operasyonda eksik 409'u buldu (Karar 6.4).

## Yerine geçtiği

1. **ADR-0005 *Sonuçlar*'ındaki "422 için `apperr`'e yeni `Kind` … idempotency PR'ında yapılır"** → Karar 7 ile karşılandı (426 ayrı kalır; ADR-0019 onu 400 `APP_VERSION_UNSUPPORTED` yaptı).
2. **ADR-0032 *Sonuçlar*'ındaki "Idempotency deposu ayrı bir platform PR'ının işidir … `completeRegistration`'ın açıklaması bugünkü davranışı yazar"** → bu ADR ile karşılandı. Sözleşmedeki "bugünkü davranış" paragrafı kaldırıldı; açıklama ADR-0032 ve bu ADR'ye atıf verir.
3. **Stil rehberi §7'nin "Depo henüz uygulanmadı" cümlesi** (ADR-0032 Yerine geçtiği 6) → gerçeğe çekildi.

## Kabul edilen riskler

- **Talep, oran sınırından önce gelir.** `authn.StrictMiddleware`'in token kovası strict sunucunun içindedir, yani middleware'den sonra. Oran sınırına takılan bir `completeRegistration` isteği önce bir talep tx'i ve ardından bir serbest bırakma yazar. Kökteki adres başına sınır bundan önce gelir; maliyet iki kısa yazımdır.
- **Tekrar, token kovasına girmez.** Saklanan yanıt strict middleware'den önce döner. Tekrar yalnız aynı isteği (token ve parola dahil) sunan istemciye gider, ve kökün adres sınırı onu da sınırlar.
- **Bağlanmamış başarı atomik değildir** (Karar 4). Birleşmeden önce iki test, sonra uyarı yakalar.

## Sonuçlar

- `Idempotency-Key` alan her operasyonun handler PR'ı başarı yanıtını kendi tx'inde `idempotency.Commit` ile saklar ve `app.idempotentFixtures`'a bir fikstür ekler. Çağıranı yoksa `idempotencyConfig`'e aktörünü yazar. Üçü de unutulursa kapı kırmızıdır.
- Uyarı: `KoalaIdempotencyUnbound` (`ops/monitoring/prometheus-alerts.yml`).

## Açık sorular

1. **Saklama süresi** Stripe'ın 24 saatidir. Mobil istemcinin yeniden deneme penceresi ölçülmedi. Daha uzun bir çevrimdışı kuyruk gelirse süre gözden geçirilir.
2. **Para operasyonları** (ödeme oturumu, iade) geldiğinde Commit'in "son adım" kuralı, ADR-0012'nin "rezerve et → commit → dış çağrı → kısa tx" akışıyla birleşir: başarı yanıtı son kısa tx'te saklanır. Bunun Iyzico'nun kendi idempotency anahtarıyla birlikte tasarımı ödeme PR'ının işidir.
