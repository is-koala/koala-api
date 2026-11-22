# ADR-0033: E-posta ve telefon doğrulaması, SMS altyapısı ve doğrulama bütçesi

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K10, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.1 (`integrations/iletimerkezi`), §3.x SMS satırı ("İleti Merkezi REST; düz JSON POST"), §3.5 (garantili yan etki), §6.1 Faz 2; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §5 ve §8, [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 1 ve 3, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md); Java `origin/main` (0cbbce69): `UserVerificationManager`, `EmailVerificationManager`, `PhoneVerificationManager`, `IletiMerkeziSmsHelper` ve istek/yanıt sınıfları, `IletiMerkeziProperties`, `IletiMerkeziIysClient`, `SmsConsumer`, `UserManager#updatePhoneVerification`/`#updateEmailVerification`, `messages_tr.properties` (`verification.phone.code.sms`), `email-verification-template.html` (okundu, koşturulmadı)
- Dış kaynaklar: NIST SP 800-63B Rev. 3 §5.1.3.2 (bant dışı doğrulayıcı, 10 dakika), §5.2.2 (kısıtlama); OWASP ASVS 4.0.3 V2.5.x ve V2.2.3 (kimlik bilgisi değişikliğinin bildirimi); OWASP "SMS pumping / toll fraud" rehberi ve sağlayıcıların (Twilio Verify Fraud Guard, Vonage) yerleşik kalıbı: ülke izin listesi, numara ve hesap başına hız sınırı, hacim uyarısı; ITU-T E.164; İleti Merkezi JSON API belgesi (`POST /v1/send-sms/json`, yanıt kodları tablosu; toplusmsapi.com)
- Kod: `internal/authn/verifications.go`, `internal/authn/verifications_jobs.go`, `internal/authn/domain.go`, `internal/authn/http.go`, `internal/authn/mail/verification_code.*`, `internal/authn/mail/contact_changed.*`, `internal/platform/sms`, `internal/integrations/iletimerkezi`, `internal/platform/fakes/iletimerkezi.go`, `internal/platform/config` (`SMS`, `ServeSMSErrors`), `db/migrations/0043_user_verifications.sql`, `db/queries/authn/verifications.sql`

## Bağlam

Faz 2'nin ikinci handler PR'ı oturum açmış kullanıcının yeni e-posta adresini ya da telefon numarasını kodla doğrulamasını getirir: `listMyVerifications`, `startMyVerification`, `getMyVerification`, `cancelMyVerification`, `confirmMyVerification`. Davranış ADR-0029 Karar 3'te yazılıdır ve bu ADR onu değiştirmez; uygulamanın gerektirdiği, ADR-0029'da olmayan kararları kayda geçirir:

- Kodun **nerede üretildiği** ve nasıl saklandığı (ADR-0032'nin "token posta gönderilirken üretilir" kalıbının kod karşılığı).
- SMS kanalının altyapısı: plan §3.1'in `integrations/iletimerkezi`'si bu PR'da gelir.
- Bütçe ve oran sınırının kanal başına biçimi, SMS pompalama savunması.
- Hedef başka bir hesaptaysa bunun **kime ve hangi adımda** söyleneceği.

## Karar

### 1. Kod gönderim işinde üretilir, anahtarlı özetle saklanır

- İstek doğrulama satırını yazar ve kodun işini (`verification_code`, yalnız satırın kimliği ve kanalı) **aynı transaction'da** River'a koyar. Kodu **iş** üretir: `crypto/rand.Int(rand.Reader, 10⁶)` (sınırın altında düzgün çekim, modulo yanlılığı yok; `000000`–`999999`, baştaki sıfır dahil), özetini satıra tek bir koşullu `UPDATE` ile yazar (satır hâlâ açık ve süresi dolmamışsa), bu deyim commit olur, sonra kod gönderilir. Kod yalnız o çağrıda ve iletide yaşar: istekte, işin argümanlarında, veritabanında ve log'da yoktur (USR-008'in testi satırların ve bütün işlerin her değerini kodla karşılaştırır).
- **Özet anahtarlıdır.** ADR-0029 Karar 3 "SHA-256 özeti" diyor. Altı haneli bir kodun düz SHA-256'sı bir milyon değeri denemekle bir saniyenin altında tersine çevrilir; sızan bir tablo bu durumda kodu verir ve kararın gerekçesi ("veritabanı okuması tam bir atlatma olmasın") karşılanmaz. Saklanan değer bu yüzden HMAC-SHA256'dır: anahtar giriş biberinden HKDF ile türetilir (`koala-api verification code v1`; refresh token halefi anahtarının kalıbı, ADR-0025 §3), girdi doğrulamanın kimliği ve koddur. Karşılaştırma `hmac.Equal` ile sabit zamanlıdır. Bu, ADR-0029'un kararının daraltılması değil, gerekçesinin uygulanmasıdır.
- **Ömür başlangıçtan sayılır** (10 dakika), üretimden değil. Sözleşmenin `expiresAt`'i "başlangıçtan 10 dakika sonra" diyor ve 201 yanıtında döner; iş geç koşarsa kodu yine aynı sona kadar geçerlidir, süresi dolmuş doğrulamaya kod gönderilmez. ADR-0032'nin bağlantıları için "ömür üretimden" kuralı burada uygulanmaz.
- **Yeniden deneme:** iş en çok 5 kez denenir (River'ın `attempt⁴` saniyelik geri çekilmesiyle yaklaşık 6 dakika, kodun ömrünün içinde). Her deneme yeni kod üretir ve özeti değiştirir; geç giden ileti de çalışan bir kod taşır. Ender yan etki: sağlayıcı iletiyi kabul edip bağlantı koparsa ikinci bir kod gider ve birincisi geçersizleşir (ADR-0032 Karar 3'ün aynı kabulü). Kalıcı ret (`sms.ErrUndeliverable`, `mailer.ErrUndeliverable`) işi `river.JobCancel` ile sonlandırır.
- Kod onaylanınca, iptal edilince ya da doğrulama yanınca özet satırdan silinir; bunu bir `CHECK` da zorlar.

### 2. Denetim sırası ve görünürlük

ADR-0029 Karar 3'ün sırası uygulanır: kayıt ve sahiplik → durum → süre → deneme → kod.

- Satır `id` **ve** `user_id` ile aranır ve kilitlenir. Başkasının doğrulaması hiç bulunmaz: görülmez, kilitlenmez, denemesi harcanmaz; hiç olmamış bir kimlikle aynı `VERIFICATION_NOT_FOUND` (USR-011). HTTP testi üç operasyonda gövdeleri bayt bayt karşılaştırır, doğru kodla bile.
- Onaylanmış doğrulama kod hiç bakılmadan `VERIFICATION_ALREADY_CONFIRMED` (USR-013), yanmış olan `VERIFICATION_ATTEMPTS_EXHAUSTED` (USR-009) alır; ikisi de süreden önce gelir, çünkü sonlanmış bir doğrulamanın süresi anlam taşımaz.
- Süre kodun önündedir: süresi dolmuşta doğru ve yanlış kod aynı gövdeyi alır ve deneme harcanmaz (USR-012).
- Yanlış kod denemeyi satır kilidi altında bir artırır; beşinci yanlış doğrulamayı aynı deyimde yakar ve `VERIFICATION_CODE_INVALID` alır, altıncı istek doğru kodla bile `EXHAUSTED` alır. **Beş yarış altında da beştir:** onay önce kullanıcının satırını, sonra doğrulamanınkini `FOR UPDATE` ile kilitler; ayrıca tabloda `failed_attempts BETWEEN 0 AND 5` ve "yanmış ⇔ deneme 5" `CHECK`'leri vardır. On iki eşzamanlı yanlış kodun tam beşi `CODE_INVALID`, yedisi `EXHAUSTED` alır (`TestAttemptLimitHoldsUnderConcurrency`).
- **Kilit sırası:** başlatma ve onay ikisi de önce kullanıcının satırını, sonra doğrulama satırını alır. Ters sıra (onayın doğrulamayı, başlatmanın kullanıcıyı önce kilitlemesi) aynı hedefe eşzamanlı başlatma ve onayda kilitlenme döngüsü kurardı.
- **Görünürlük:** sözleşmenin `getMyVerification` açıklaması "yakılmış doğrulama 404" diyor. Aynı kural iptal edilmişe de uygulanır: onay, okuma ve iptal üçü de yanmış ve iptal edilmiş doğrulamayı yoktan ayırmaz (onaydaki tek istisna yanmışın `EXHAUSTED`'i, USR-009). İptal edilmiş doğrulamanın onayı da 404'tür; böylece yeniden göndermeyle değişen eski kodun (USR-010'un C1'i) cevabı "böyle bir doğrulama yok"tur, "yanlış kod" değil. Onaylanmış doğrulamanın iptali `204` döner ve bir şey değiştirmez: yazılan değer yerinde kalır.
- Bu yüzden sözleşmedeki `VerificationStatus`'un `BURNED` ve `CANCELLED` değerleri bugün hiçbir yanıtta dönmez. Enum'dan değer silmek uyumsuz değişiklik olduğu için sözleşmede kalırlar (açık soru 3).

### 3. Hedef başka bir hesaptaysa: yalnız kodu doğru girene, onayda

`VERIFICATION_TARGET_IN_USE` sözleşmede hem başlatmada hem onayda listeliydi. Başlatmada dönmesi, oturum açmış **her** kullanıcıya herhangi bir adresin ya da numaranın kayıtlı olup olmadığını sorma imkânı verirdi: kayıt (USR-001) ve parola sıfırlama (USR-018) bunu bilerek kapatmışken üçüncü bir numaralandırma kanalı açılırdı. Java bunu yapıyor (`UserManager` profil yamasında `existsByEmail` ile ayrı bir hata dönüyor).

Karar:

- **Başlatma hedefin hesap durumuna hiç bakmaz.** Kayıtlı ve serbest hedef aynı satırı, aynı işi, aynı bütçe tüketimini ve aynı `201`'i alır; kod iki dalda da gönderilir.
- **Bilgi yalnız onayda, doğru koddan sonra döner.** Yanlış kod yanlış koddur. Doğru kodu giren, hedefin sahibi olduğunu kanıtlamıştır; ona hedefin başka bir hesapta olduğunu söylemek ona yeni bir şey öğretmez. Yazım `users_email_key` ya da `users_phone_key` tekil indeksine takılır, transaction geri alınır: doğrulama tüketilmez, değer yazılmaz, `409 VERIFICATION_TARGET_IN_USE` döner.
- Sözleşmede `startMyVerification`'ın `x-error-codes` listesinden `VERIFICATION_TARGET_IN_USE` çıkarıldı (ADR-0032 Karar 1'in bağ denetimi ulaşılamayan liste üyesini kırmızı sayar). `409` yanıtı sözleşmede kalır: durum kümesinden yanıt silmek stil rehberi §2'ye göre uyumsuz değişikliktir ve ters yön (kodsuz yanıt) denetlenmez (ADR-0029 Karar 1, sınır 2). `ErrorCode` düzyazısı bu kuralı yazar.
- Bedeli: kayıtlı bir adrese, sahibinin istemediği bir doğrulama kodu gidebilir. Bu, adrese istenmeden posta gönderilmesidir, içeriği saldırgan seçmez (Karar 9) ve hedef bütçesiyle sınırlıdır (Karar 4); numaralandırma kanalından ucuzdur.

### 4. Bütçe ve oran sınırı

Dört katman:

1. **HTTP kovaları** (ADR-0021, ADR-0025 §8): başlatma, hedefin anahtarlı özetinin iki kovasından jeton alır (`(hedef, adres)` `auth` grubunda, hedef geneli `auth_account` grubunda), kayıt başlatmanın kalıbıyla. Onay, doğrulamanın kimliğinin kovasından (`auth`) jeton alır. Her ikisi `x-rate-limit: auth` ile kenarın adres başına sınırına ve kimlik başına sınıra da girer.
2. **İstek içi yeniden gönderme bütçesi**, senkron ve `429 VERIFICATION_RESEND_TOO_SOON` + `Retry-After` ile (sözleşme böyle yazıyor):
   - **Bekleme:** hedefin son kodundan sonra 60 saniye.
   - **Hedef başına saatlik bütçe:** bir hedef bir saatte en çok 5 kod alır.
   - İkisi de **bütün kullanıcılar üzerinden** sayılır. ADR-0029'un "hedef başına" sözü bu kararla netleşir: amaç hedefin sahibini bombardımandan korumaktır, ve çok sayıda hesap açan bir saldırgan kullanıcı başına bir bütçeyi hesap sayısı kadar çoğaltırdı.
   - **Kullanıcının kanal başına günlük tavanı:** SMS'te 10, e-postada 20 doğrulama, bütün hedefler üzerinden. Hesap başına pompalama sınırıdır (Karar 5).
   - Sayım ve yazım, hedefin anahtarı üzerinde bir `pg_advisory_xact_lock` altında olur: iki eşzamanlı başlatma bütçeyi birlikte aşamaz. ADR-0032'nin küresel tavanı gibi yumuşak değil, kesindir.
   - Neden senkron: ADR-0032'deki posta bütçesi sessizdi, çünkü istek oturumsuzdu ve cevabı adresin kayıtlı olup olmadığına göre değişirse numaralandırma kanalı olurdu. Burada bütçe hedefin hesap durumuna hiç bakmaz (Karar 3): kayıtlı ve serbest hedef onu aynı biçimde tüketir. Sızan tek bilgi "bu hedefe son bir saatte bir kod gitti" olur; kabul edilen riskler aşağıda.
3. **Küresel saatlik SMS tavanı**, işte: bütün kullanıcılar için saatte en çok `KOALA_SMS_PER_HOUR` (öntanım **200**) kod SMS'le gider. Tavan doluyken kod **tutulur**: istek yine `201` almıştır, iş göndermez, `koala_verification_codes_total{channel="PHONE",outcome="held"}` artar ve `KoalaVerificationTextCapReached` çalar. Sayım işin içindedir ve eşzamanlı işler birbirini görmez, bu yüzden tavan birkaç ileti aşılabilen yumuşak bir sınırdır. Tavan isteğe değil işe konur: senkron bir küresel 429, saldırgana tavanın ne zaman dolduğunu tam olarak söylerdi. Tavanı iki dal (kayıtlı ve serbest hedef) aynı biçimde tüketir, çünkü işin dalı yoktur.
4. **Hedef anahtarı:** saklanan hedef normalize edilmiş değerdir; bütçe anahtarı ise e-postada posta kutusu (`+etiket` atılır, Gmail'de noktalar silinir; ADR-0032 Karar 6'nın `mailboxKey`'i), telefonda E.164 numaranın kendisidir (Karar 6). Anahtar giriş biberiyle HMAC-SHA256'dır (`target_key`); tablo adres ya da numara listesiyle eşleştirilemez. `kurban+1@…`, `k.urban@gmail.com`, `0532 123 45 67` ve `+905321234567` her biri kendi kutusunun ya da numarasının tek hedefidir.

**Bütçe kanal başına ayrıdır, tek kavram değildir.** E-posta ve SMS'in maliyeti ve tehdidi farklıdır: e-postada risk aktarıcının itibarı ve hedefin kutusudur, SMS'te para (her ileti ücretli, uluslararası pompalama). Ortak kalan yalnız hedef bütçesinin biçimidir (60 sn, saatte 5); kullanıcı tavanı kanal başına ayrıdır ve küresel tavan yalnız SMS'tedir. E-posta OTP'sine ayrı bir küresel tavan konmadı: hedef bütçesi ve kullanıcı tavanı onu sınırlar, ve tek göndericisi oturum açmış, e-postası doğrulanmış hesaplardır.

**Kabul edilen riskler:**

- **Hedefin doğrulaması bir saat engellenebilir.** Bir saldırgan, kurbanın numarasına ya da adresine bir saatte beş doğrulama başlatarak (birkaç hesapla, her biri kendi günlük tavanından harcayarak) o saat boyunca kurbanın kendi doğrulamasını `429`'a düşürebilir. Hedef başına sınırın doğası budur (ADR-0032 Karar 6'nın aynı kabulü).
- **"Bu hedefe yakın zamanda kod gitti" sızar.** Bekleme ya da saatlik bütçe başka bir kullanıcının isteğiyle dolmuşsa çağıran bunu `429`'dan öğrenir. Bu, hedefin bir hesabı olup olmadığını söylemez (bütçe hesap durumuna bakmaz), yalnız son bir saatteki bir doğrulama girişimini; değeri düşüktür ve kendisi de oran sınırlıdır.
- **Dağıtık bir saldırı küresel SMS tavanını doldurabilir.** Tavanı dolduran her hesap günlük 10 SMS'le sınırlıdır ve hesap açmak kayıt bütçelerine takılır (ADR-0032 Karar 6), ama yeterince hesapla tavan dolar. O saat meşru telefon doğrulamaları da kod almaz. Tavanın varlık nedeni faturadır, sürekliliği değil; azaltmalar uyarı, runbook ve yeniden başlatmadan önce ayarlanabilen değerdir.

### 5. SMS pompalama (toll fraud) savunması

Sektörde yerleşik kalıp üç sınırla bir izlemedir; dördü de uygulanır:

- **Ülke ve numara türü izin listesi:** kod yalnız izin verilen ülkenin **cep** numarasına gider. Bugün yalnız Türkiye: `+90` ve ulusal numarası `5` ile başlayan on hane. Sabit hat (`+90 212…`), paylaşılan maliyet (`0850`), katma değerli (`0900`) ve her yabancı numara (pompalamanın yaşadığı uluslararası ve uydu aralıkları dahil) `REQUEST_INVALID` (`/target`) alır; hiçbir satır yazılmaz, iş kuyruğa girmez. Gerekçe: bugünkü bütün kullanıcılar Türkiye'de, Java da yalnız yurt içine gönderiyor; pompalama kârını yüksek ücretli uluslararası sonlandırmadan elde eder. Kural tek yerdedir (`authn.TextableNumber`, tablo `textableCountries`); yeni ülke yeni bir satırdır. İstemci (`iletimerkezi.National`) de `+90` dışını göndermeyi reddeder: ikinci savunma hattı.
- **Hesap başına günlük tavan:** 10 SMS. Meşru bir kullanıcı numarasını bir günde birkaç kez değiştirmez; yazım hatası ve kodun gecikmesi 10'un içinde kalır. 20 e-posta OTP'si aynı mantıkla, ama ileti ücretsiz olduğu için daha geniş.
- **Küresel saatlik tavan:** 200 (Karar 4). Ölçülmedi, çünkü gerçek trafik yok; bir saatte iki yüz numara değişikliği bugünkü kullanıcı tabanının çok üstündedir ve tavanın aylık maliyeti öngörülebilir kalır.
- **Metrik ve uyarı:** `koala_verification_codes_total{channel, outcome}` (`sent`, `held`, `undeliverable`, `throttled`). `KoalaVerificationTextCapReached` tavan dolduğunda, `KoalaVerificationTextVolumeHigh` bir saatte 100'den fazla SMS gittiğinde çalar: pompalama tavandan önce hacim olarak görünür.

### 6. Telefon numarası normalizasyonu

`authn.NormalizePhone` numarayı E.164'e çevirir: boşluk, tire, nokta ve parantez atılır; `00` ile başlayan uluslararası önek `+` olur; ülke kodu olmayan Türk numarası (`05XX…`, `5XX…`, `905XX…`) `+90` alır. Ardından E.164 biçimi (`^\+[1-9][0-9]{7,14}$`) aranır; başka karakter ya da biçim reddedilir, tahmin edilmez. Saklanan, bütçe anahtarı olan ve SMS'e giden değer bu biçimdir; ekranda yalnız maskelisi döner (`+90******4567`). Kütüphane (libphonenumber'ın Go portu) onaylı listede değil ve tek ülkelik bir izin listesi için gerekmiyor.

Telefon numarası kişisel veridir: log'a, hata metnine ve metrik etiketine yazılmaz. İleti Merkezi istemcisinin hata metinleri numara, metin ve kimlik bilgisi taşımaz (testi her hata sınıfında arar); sağlayıcının serbest metin mesajı da hataya konmaz.

### 7. SMS altyapısı

- **`internal/platform/sms`:** `Message{To (E.164), Text}` ve `ErrUndeliverable`. Alan bilgisi taşımaz; posta için `platform/mailer` ne ise SMS için odur.
- **`internal/integrations/iletimerkezi`:** düz `net/http` ile tek bir JSON `POST`; yeni bağımlılık yok. Biçim sağlayıcının belgesidir:
  - Uç `https://api.iletimerkezi.com/v1/send-sms/json`; gövde `{"request": {"authentication": {key, hash}, "order": {sender, sendDateTime: [], iys: "0", iysList: "", message: {text, receipents: {number: ["5XXXXXXXXX"]}}}}}`. Alıcı anahtarı sağlayıcının yazımıyla `receipents`'tir (harf hatası onun); kodda iki parçada yazılır ki yazım denetçisi bir kablo adını "düzeltmesin". Numara ulusal on hane gider.
  - Kimlik bilgisi yalnız gövdededir, adreste ya da başlıkta değil. `iys` `"0"`'dır: doğrulama kodu ticari ileti değildir (Java da öyle gönderiyor).
  - Yanıt `{"response": {"status": {"code", "message"}, "order": {"id"}}}`; `code` belgede hem dize hem sayı görünür, ikisi de okunur. HTTP durumu değil gövdedeki kod belirleyicidir.
  - **Kalıcı ret** (`452` alıcı yok/geçersiz/engelli, `453` metin çok uzun, `454` metin boş) `sms.ErrUndeliverable`'ı sarar ve işi iptal ettirir. **İşletimsel ret** (`400`, `401` kimlik ya da IP, `402` bakiye, `450` onaysız başlık, `451` yineleme, `468`–`470` İYS ayarı), okunamayan yanıt ve ağ hatası yeniden denenir: düzeltildiklerinde bir sonraki deneme geçer.
  - **Java'dan iki bilinçli ayrılık**, ikisi de Java'nın kusuru: `IletiMerkeziSmsHelper` alıcıyı `recipients` diye gönderiyor ve yanıtı `response` sarmalayıcısı olmadan okuyor (`response.getStatus()` boş kalır, her gönderim `ExternalServiceException` ile düşer). Aynı deponun İYS istemcisi (`IletiMerkeziIysClient`) sarmalayıcıyı doğru okuyor. Go sağlayıcının belgesini izler; bu yüzden ilk gerçek gönderim staging'de denetlenmelidir (açık soru 2).
- **Yapılandırma:** `KOALA_ILETIMERKEZI_API_KEY` ve `_API_HASH` (ikisi de `config.Secret`), `KOALA_ILETIMERKEZI_SENDER` (onaylı başlık, 3–11 ASCII), `KOALA_SMS_PER_HOUR` (1–100000). `config.ServeSMSErrors` `ServeMailErrors` kalıbındadır: staging ve production'da eksikse `serve` açılmaz, çünkü yoksa telefon doğrulaması `201` döner ve hiçbir SMS gitmez; yarım yapılandırma her ortamda hatadır. `local` ve `test`'te sağlayıcı yoksa ileti düşürülür ve yalnız düştüğü log'a yazılır (numara ve metin değil).
- **Test:** gerçek sağlayıcı yoktur. `fakes.IletiMerkezi`, `fakes.Server` üzerinde belgedeki biçimi konuşur: yabancı kimlik bilgisine `401`, okunamayan isteğe `400`, `RejectNumber` ile işaretli numaraya `452`, gerisine sipariş kimliği. Gerçek istemci ona karşı koşar (`internal/integrations/iletimerkezi`'nin testleri: kablo adları, ulusal numara, sınıflandırma, PII'siz hata).

### 8. Onay değeri yazar; profil yaması bu PR'da değil

- Onay, doğrulamayı tüketir ve yeni değeri aynı transaction'da kullanıcıya yazar: e-postada `email` ve `email_verified = true`, telefonda `phone` ve `phone_verified = true`. Değer onaylanana kadar yazılmaz (USR-014'ün onay yarısı; `TestConfirmedValueIsWrittenOnlyOnConfirmation`).
- **E-posta değişikliği** giriş kimliğini değiştirir. Aynı transaction'da kullanıcının açık parola sıfırlama token'ı iptal edilir (bağlantı eski adrese gitmişti) ve değişiklik **eski adrese** postayla bildirilir (`contact_changed`; OWASP ASVS 4.0.3 V2.2.3'ün kimlik bilgisi değişikliği bildirimi). Telefon değişikliği hesabın adresine bildirilir. Bildirim işi eski adresi taşır, çünkü hesap artık onu tutmuyor; `river_job`'un `pii` gerekçesi buna göre genişletildi. Bildirim yalnız commit olan değişiklik için gider. Adres zaten hesabın adresiyse (yalnız doğrulanıyorsa) bildirim gitmez.
- Oturumlar e-posta değişikliğinde kapatılmaz (Java da kapatmıyor); açık soru 5.
- **Profil yamasının doğrulama başlatması** (USR-014'ün "profilde e-posta ya da telefon değiştirmek bir doğrulama başlatır; aynı değer hiçbir şey başlatmaz" yarısı) profil PR'ına kalır. O PR `Verifications.Start`'ı arayüz üzerinden çağırır; USR-014 o güne kadar `deferred` kalır.
- **Profil senkronu giden kaydı** (USR-022, USR-023): Java onaydan sonra `publishUserProfileUpdated` çağırıyor. Giden kaydın yazımı profil PR'ının işidir (ADR-0029 *Sonuçlar*); o PR onayın yazdığı değeri de kapsamalıdır.

### 9. Postalar

- **Kod postası kimseyi adlandırmaz.** Adres henüz doğrulanmamıştır; postada hesabın adı olsaydı, bir hesap sahibi kendi profil adını (istediği metni) istediği adrese gönderebilirdi. ADR-0032 Karar 3'ün kayıt postası kuralı burada da geçerlidir; şablon yalnız kodu okur (`TestCodeMailNamesNobody`). **Java'dan ayrılık:** Java'nın `email-verification-template`'i ad, soyad ve yeni adresi basıyor.
- SMS metni Java'nın metnidir, bir uyarıyla: "Koala doğrulama kodunuz: 123456. Bu kodu kimseyle paylaşmayın." Türkçe harfler UCS-2 kodlamasını gerektirir; metin tek bölüme (70 karakter) sığar ve bunu bir test bağlar.

### 10. Tablolar ve saklama

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `user_verifications` | M2, `pii` | Hedef (adres ya da numara), hedef anahtarının HMAC'i, kodun anahtarlı özeti (doğrulama bitince silinir), deneme sayısı ve bitiş. Doğrulama geri gelmez, bu yüzden soft-delete'li değildir. **İki gün sonra hard delete:** en uzun bütçe penceresi bir gündür, ötesinde satırın amacı yoktur (KVKK veri en aza indirme). `users`'a giden kenar `detach`'tir. |

- `users`'a `phone` (E.164, `CHECK`) ve `users_phone_key` (etkin satırlar arasında tekil, `users_email_key`'in kalıbı) eklendi; view `soft-delete-view` yönergesiyle yeniden kuruldu. Telefon tekilliği Java'nın `existsByPhoneNumber`'ının karşılığıdır.
- Yeni `detach` kenarı JOIN testinde `Unauthenticated` bildirildi: doğrulamanın kullanıcısı çağıranın kendisidir, silinince oturumu biter ve liste de detay da `401` döner. `user_verifications` muaf (M2) olduğu için okuma testi onun için gizlenecek bir şey aramaz.
- Doğrulama `users`'ın çocuğudur: başlatma, satırı yazmadan önce kullanıcıyı `FOR UPDATE` ile kilitler (`LockUserForSignIn`; `softdelete.LockActiveParent`'ın güvencesinden güçlüdür ve kullanıcının başlatmalarını günlük tavan için sıralar).

### 11. Liste ve cursor

`listMyVerifications` açık doğrulamaları (onaylanmamış, iptal edilmemiş, yanmamış, süresi dolmamış) yeniden eskiye döndürür, `(created_at, id)` üzerinde iki yönlü keyset sayfalamayla. Cursor opaktır (sürüm önekli base64url); bozuk ya da başka biçimdeki cursor `CURSOR_INVALID` alır. Kod platform kodlarından değildir, bu yüzden operasyonun `x-error-codes`'una eklendi (yanıt kümesi değişmedi: `400` zaten vardı). Kodun sabiti bugün `authn`'dedir; sayfalamayı ikinci kullanan operasyon gelince ortak bir platform yardımcısına taşınır.

## Sözleşme değişiklikleri

Beş operasyonun `x-not-implemented` işareti kalktı ve iskele metotları silindi. Uyumlu değişiklikler:

- `startMyVerification`: `x-error-codes`'tan `VERIFICATION_TARGET_IN_USE` çıktı (Karar 3), açıklama bütçeyi, normalizasyonu ve ülke kuralını yazar.
- `listMyVerifications`: `x-error-codes: [CURSOR_INVALID]`.
- `getMyVerification`, `cancelMyVerification`, `confirmMyVerification`: açıklamalar iptal edilmiş doğrulamanın görünürlüğünü ve hedef çakışmasını yazar.
- `ErrorCode` düzyazısında `VERIFICATION_TARGET_IN_USE` ve `VERIFICATION_RESEND_TOO_SOON`'un açıklaması genişletildi; durumları değişmedi.

## Kapının bu PR'daki gerçek vakaları

- `TestHandlersReturnTheirDeclaredErrorCodes` beş yeni handler'ı kendiliğinden izler; adlandırılmış yerler on üçe çıktı: `startMyVerification` → `VERIFICATION_RESEND_TOO_SOON` (bir bileşik literal içinden), `listMyVerifications` → `CURSOR_INVALID` (yerel bir ada bağlanmış yer), `confirmMyVerification` → `VERIFICATION_TARGET_IN_USE`.
- Bağ denetimi bu PR'da bir gerçek bulgu verdi: `startMyVerification`'ın listesindeki `VERIFICATION_TARGET_IN_USE`'a hiçbir yerden ulaşılmıyor (Karar 3). Liste düzeltildi.
- Kuralların her biri gerçek bir vakaya karşı sınanır, boş kümeye değil: ülke izin listesi her iki yönde (`TestTextableNumber`, `TestOnlyTextableNumbersGetACode`), bütçenin üç sınırı ayrı ayrı ve kendi `Retry-After`'ıyla (bekleme, saatlik bütçe başka bir kullanıcı ve başka bir yazımla, günlük tavan), küresel tavan tutulan ve gönderilen ileti sayısıyla, kalıcı ve geçici ret ikisi de iki kanalda.

## Yerine geçtiği

1. **ADR-0029 Karar 3'ün "Saklama: SHA-256 özeti"** → Karar 1: anahtarlı HMAC-SHA256. Gerekçe ADR-0029'un kendi gerekçesidir.
2. **ADR-0029 Karar 3'ün "hedef başına" bekleme ve saatlik bütçesi** → Karar 4: bütün kullanıcılar üzerinden sayılır; kullanıcı başına günlük tavan ve küresel SMS tavanı eklendi.
3. **Sözleşmenin `startMyVerification`'da `VERIFICATION_TARGET_IN_USE` vaadi** → Karar 3: bilgi yalnız onayda, doğru koddan sonra döner.
4. **ADR-0032 *Sonuçlar*'ındaki "OTP ve doğrulama senaryoları (USR-008…014) `deferred` kalıyor"** → USR-008…013 bağlandı; USR-014 profil PR'ını bekliyor (Karar 8).

ADR-0013, ADR-0014, ADR-0025 ve ADR-0032'nin kararları değişmiyor.

## Sonuçlar

- Bağlanan P0'lar: USR-008, USR-009, USR-010, USR-011, USR-012, USR-013 (`boundAhead`, ADR-0032 Karar 5). USR-014 `deferred` kalıyor: senaryonun "profilde değiştirmek doğrulama başlatır" ve "aynı değer hiçbir şey başlatmaz" adımları profil yamasının davranışıdır; onay yarısının testi bu PR'da yazıldı ama senaryo tamamını kanıtlamadan bağlanmaz.
- Staging ve production `serve`'ü artık İleti Merkezi hesabı olmadan açılmaz; ilk deploy'dan önce üç değişken tanımlanmalı ve başlık İleti Merkezi'de onaylı olmalıdır (runbook).
- `river_job` artık bir notice işinde eski e-posta adresini de taşır (Karar 8); River biten işi 24 saatte siler.

## Açık sorular

1. **SMS tavanlarının değerleri** (küresel saatte 200, kullanıcı başına günde 10, hacim uyarısı 100/sa) ölçülmedi; gerçek trafik görülünce `koala_verification_codes_total` ile gözden geçirilir.
2. **İleti Merkezi kablo biçimi** sağlayıcının belgesinden kuruldu; Java'nın istemcisi üretimde çalışan bir kanıt sunmuyor (Karar 7'deki iki kusur). İlk gerçek gönderim staging'de, gerçek bir hesapla elle doğrulanmalıdır. Hesapta sabit IP kısıtı varsa sunucu adresleri izin listesine eklenir.
3. **`VerificationStatus`'un `BURNED` ve `CANCELLED` değerleri** hiçbir yanıtta dönmüyor (Karar 2). Sözleşmenin bir sonraki büyük sürümünde kaldırılabilir; ya da ürün "yanmış ve iptal edilmiş doğrulama sahibine görünsün" derse okuma bunları döndürür.
4. **Telefon tekilliği** Java'nın niyetini izliyor (`existsByPhoneNumber`); bir numaranın iki hesapta (ör. aile) bulunmasına izin verilecekse bu bir ürün kararıdır ve `users_phone_key` kalkar.
5. **E-posta değişikliğinde diğer oturumlar kapatılsın mı?** Değişiklik giriş kimliğini değiştirir ve sektörün bir kısmı (ör. parola değişimindeki gibi) diğer oturumları kapatır. Java kapatmıyor; bu PR da kapatmıyor, yalnız eski adrese bildirim gönderiyor ve açık sıfırlama token'ını iptal ediyor. Ürün kararı.
