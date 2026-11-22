# ADR-0033: E-posta ve telefon doğrulaması, SMS altyapısı ve doğrulama bütçesi

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K10, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.1 (`integrations/iletimerkezi`), §3.x SMS satırı ("İleti Merkezi REST; düz JSON POST"), §3.5 (garantili yan etki), §6.1 Faz 2 ve Faz 5; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) §5 ve §8, [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 1 ve 3, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md); Java `origin/main` (0cbbce69): `UserVerificationManager`, `EmailVerificationManager`, `PhoneVerificationManager`, `IletiMerkeziSmsHelper` ve istek/yanıt sınıfları, `IletiMerkeziProperties`, `IletiMerkeziIysClient`, `SmsConsumer`, `NotificationManager#sendSms`, `UserManager#updatePhoneVerification`/`#updateEmailVerification`, `UserDao#existsByPhoneNumber`, `EmailVerificationDao#deleteByNewEmail`, `messages_tr.properties`/`messages_en.properties` (`verification.phone.code.sms`), `email-verification-template.html` (okundu, koşturulmadı)
- Dış kaynaklar: NIST SP 800-63B Rev. 3 §5.1.3.2 (bant dışı doğrulayıcı, 10 dakika), §5.2.2 (kısıtlama); OWASP ASVS 4.0.3 V3.7.1 (hassas işlemden önce yeniden kimlik doğrulama), V2.2.3 (kimlik bilgisi değişikliğinin bildirimi); OWASP "SMS pumping / toll fraud" rehberi ve sağlayıcıların (Twilio Verify Fraud Guard, Vonage) yerleşik kalıbı: ülke izin listesi, numara, hesap ve adres başına hız sınırı, hacim uyarısı; ITU-T E.164; geri dönüştürülmüş numaralarda WhatsApp ve Telegram'ın "yeni doğrulanan sahip numarayı devralır" kalıbı; İleti Merkezi API belgesi: [send-sms](https://www.iletimerkezi.com/en/docs/api/send-sms.md) ve [error-codes](https://www.iletimerkezi.com/en/docs/api/error-codes.md)
- Kod: `internal/authn/verifications.go`, `internal/authn/verifications_jobs.go`, `internal/authn/accounts.go` (`confirmPassword`, `CompletePasswordReset`), `internal/authn/domain.go`, `internal/authn/http.go`, `internal/authn/mail/{verification_code,contact_changed,phone_released}.*`, `internal/platform/sms`, `internal/integrations/iletimerkezi`, `internal/platform/fakes/iletimerkezi.go`, `internal/platform/config` (`SMS`, `Verification`, `ServeSMSErrors`), `internal/app/edge.go` (`verification_address`), `db/migrations/0043_user_verifications.sql`, `db/queries/authn/verifications.sql`

## Bağlam

Faz 2'nin ikinci handler PR'ı oturum açmış kullanıcının yeni e-posta adresini ya da telefon numarasını kodla doğrulamasını getirir: `listMyVerifications`, `startMyVerification`, `getMyVerification`, `cancelMyVerification`, `confirmMyVerification`. Davranış ADR-0029 Karar 3'te yazılıdır; bu ADR onu uygular ve uygulamanın gerektirdiği, ADR-0029'da olmayan kararları kayda geçirir. ADR-0029'dan ve ADR-0032'den ayrıldığı yerler "Yerine geçtiği" bölümünde tek tek sayılır.

## Karar

### 1. Kod gönderim işinde üretilir, anahtarlı özetle saklanır

- İstek doğrulama satırını yazar ve kodun işini (`verification_code`; argümanında yalnız satırın kimliği ve kanal) **aynı transaction'da** River'a koyar. Kodu **iş** üretir: `crypto/rand.Int(rand.Reader, 10⁶)` (sınırın altında düzgün çekim, modulo yanlılığı yok; `000000`–`999999`, baştaki sıfır dahil). İş satırı kilitleyip özetini yazan kısa transaction'ı commit eder, sonra kodu gönderir. Kod yalnız o çağrıda ve iletide yaşar: istekte, işin argümanlarında, veritabanında ve log'da yoktur (USR-008'in testi satırların ve bütün işlerin her değerini kodla karşılaştırır).
- **Özet anahtarlıdır.** ADR-0029 Karar 3 "SHA-256 özeti" diyor. Altı haneli bir kodun düz SHA-256'sı bir milyon değeri denemekle bir saniyenin altında tersine çevrilir; sızan bir tablo bu durumda kodu verir ve kararın gerekçesi ("veritabanı okuması tam bir atlatma olmasın") karşılanmaz. Saklanan değer bu yüzden HMAC-SHA256'dır: anahtar giriş biberinden HKDF ile türetilir (`koala-api verification code v1`; refresh token halefi anahtarının kalıbı, ADR-0025 §3), girdi doğrulamanın kimliği ve koddur. Karşılaştırma `hmac.Equal` ile sabit zamanlıdır.
- **Ömür başlangıçtan sayılır** (10 dakika), üretimden değil: sözleşmenin `expiresAt`'i "başlangıçtan 10 dakika sonra" diyor ve 201 yanıtında döner. ADR-0032'nin bağlantıları için "ömür üretimden" kuralı burada uygulanmaz.
- **Gönderim penceresi:** kod, doğrulamanın ömründen en az **2 dakika** (`VerificationSendWindow`) kalmışsa gönderilir. Daha azında iş kod üretmez; satır "tutuldu" (`held_at`) işaretlenir, ömrü bitene kadar `PENDING` kalır ve sonra `EXPIRED` olur. (Gönderim anında bütçe artık izin vermiyorsa ise satır gönderilmeden iptal edilir, Karar 4.) Kodu gelmeyen kullanıcı yeniden başlatır; tutulan satır bütçeden yer harcamaz (Karar 4). Saniyeler içinde süresi dolacak bir kod yalnız başarısız bir onaya yol açardı.
- **Etkin kullanıcı:** iş doğrulamayı, kullanıcısının satırı `active_users`'ta ve durumu `ACTIVE` iken kilitler (`LockVerificationForCode`, `LEFT JOIN active_users` + `softdelete:inner-by-design users` işareti). Silinmiş ya da askıya alınmış kullanıcının doğrulamasına kod gitmez; satır tutuldu işaretlenir.
- **Yeniden deneme:** iş en çok 5 kez denenir (River'ın `attempt⁴` saniyelik geri çekilmesiyle yaklaşık 6 dakika). Her deneme yeni kod üretir ve özeti değiştirir; geç giden ileti de çalışan bir kod taşır. Ender yan etki: sağlayıcı iletiyi kabul edip bağlantı koparsa ikinci bir kod gider ve birincisi geçersizleşir (ADR-0032 Karar 3'ün aynı kabulü). Kalıcı ret (`sms.ErrUndeliverable`, `mailer.ErrUndeliverable`) işi `river.JobCancel` ile sonlandırır.
- Kod onaylanınca, iptal edilince ya da doğrulama yanınca özet satırdan silinir; bunu bir `CHECK` da zorlar.

### 1b. Başlatma mevcut parolayı ister

E-posta değişikliği giriş kimliğini değiştirir. Yeniden kimlik doğrulama olmadan çalınmış bir oturum şu zinciri kurar: `startMyVerification(EMAIL, saldırganın adresi)` → kendi kutusundan `confirm` → `requestPasswordReset` → `completePasswordReset`. Sonuç kalıcı ele geçirmedir ve `changeMyPassword`'ün mevcut parola denetimini boşa çıkarır.

Karar (OWASP ASVS 4.0.3 V3.7.1):

- `startMyVerification` gövdesinde **`currentPassword`** zorunludur, **iki kanalda da**. Telefon da hassas bir iletişim bilgisidir (SMS ile hesap kurtarma ve bildirim kanalı olur) ve tek kural daha sadedir.
- Denetim `changeMyPassword`'ün kalıbıdır ve kodu tek yerdedir (`Service.confirmPassword`, iki çağıranı var): yanlış parola hesabın giriş hedefinde `FAILED` sayılır ve aynı yüz denemelik kilide katkıda bulunur; hedef kilitliyken parola incelenmeden reddedilir ve aynı maliyeti ödemek için tuzak hash'e karşı doğrulanır; doğru parola başlatmanın transaction'ında `SUCCEEDED` yazar. Parola transaction dışında doğrulanır (argon2 maliyeti kilit tutmasın); başlatma kullanıcının satırını kilitlediğinde hash'in o arada değişmediğini yeniden denetler, değişmişse aynı `PASSWORD_MISMATCH` döner (`changeMyPassword`'ün aynı denetimi). Yanıt `403 PASSWORD_MISMATCH`'tir (mevcut kod; sözleşmeye `403` yanıtı eklendi). Sıra: biçim (`REQUEST_INVALID`) → parola → bütçe.
- **Girişteki kilit doğrulama başlatmayı da kapatır.** Giriş kilidi hesabın giriş hedefine bağlıdır ve kimin tetiklediğine bakmaz: yüz yanlış girişle hesabı kilitleyen anonim bir saldırgan, kilit süresince (bir saat) hesap sahibinin e-posta ya da telefon doğrulaması başlatmasını da engeller. Bu, ADR-0032 Karar 7'nin uygulama içi parola değişimi için kabul ettiği bedelin aynısıdır: tahmin edilen şey aynı paroladır ve iki yüzey arasında bölünmüş iki bütçe saldırgana iki katını verirdi. Parola değişiminde olduğu gibi bilinen cihaz istisnası yoktur, çünkü oturumu tutan cihaz çağıranın kendisidir; sahip kilit bitince başlatır ya da parolasını sıfırlar (sıfırlama kilidi kaldırır, ADR-0032 Karar 7).
- **Parolası olmayan hesap:** bugün yok. Kayıt her zaman parola yazar (ADR-0032 Karar 2), Java'da da parolasız giriş yolu yok (`AuthManager` yalnız parolayla giriyor; sosyal ya da OTP'li giriş yok). `users.password_hash` ileride gelebilecek bir yalnız-OTP hesap için boş bırakılabiliyor. Böyle bir hesap parolası olmadan doğrulama başlatamaz: denetim kukla hash'e karşı yapılır ve her zaman reddedilir; hesap önce parola sıfırlamayla bir parola belirler (sıfırlama parolaya bakmıyor). `TestStartNeedsThePassword` bunu bağlar.
- **Profil PR'ının yükümlülüğü:** `updateMyProfile` ileride e-posta ya da telefon değişikliğinde `Verifications.Start`'ı çağıracak. O operasyon da **aynı yeniden kimlik doğrulamayı ister**: e-posta ya da telefon alanı yamada varsa mevcut parola da gelir. Yoksa yukarıdaki zincir profil ucundan yeniden açılır.
- **Diğer oturumlar e-posta değişince kapatılmaz.** Yeniden kimlik doğrulama, çalınmış bir oturumun tek başına değişiklik yapmasını engelliyor; değişikliği yapan parolayı bilen kişidir ve parolayı bilen biri oturumları ayrıca kapatabilir (`revokeMyOtherSessions`). Değişiklik eski adrese bildirilir ve eski adrese gitmiş açık sıfırlama bağlantısı iptal edilir (Karar 8). Java da kapatmıyor.
- **"Bu ben değildim" geri alma bağlantısı** (OWASP önerisi) bu PR'da gelmiyor: issue [#24](https://github.com/is-koala/koala-api/issues/24).

### 2. Denetim sırası ve görünürlük

Sıra: kayıt ve sahiplik → durum → süre → kod.

- Satır `id` **ve** `user_id` ile aranır ve kilitlenir. Başkasının doğrulaması hiç bulunmaz: görülmez, kilitlenmez, denemesi harcanmaz; hiç olmamış bir kimlikle aynı `VERIFICATION_NOT_FOUND` (USR-011). HTTP testi üç operasyonda gövdeleri bayt bayt karşılaştırır, doğru kodla bile.
- Onaylanmış doğrulama kod hiç bakılmadan `VERIFICATION_ALREADY_CONFIRMED` (USR-013), yanmış olan `VERIFICATION_ATTEMPTS_EXHAUSTED` (USR-009) alır; ikisi de **süreden önce** gelir. ADR-0029 Karar 3'ün sırası "süre → deneme" diyor; burada yanmış doğrulama (deneme) süreden önce denetlenir, çünkü sonlanmış bir doğrulamanın süresi anlam taşımaz ve yanmış doğrulamanın cevabı süre dolduktan sonra da `EXHAUSTED` kalmalıdır. Kod doğruluğu sızmaz: yanmış doğrulamanın özeti silinmiştir, kod hiç karşılaştırılmaz. Bu, ADR-0029'un sırasından bir sapmadır ("Yerine geçtiği" 2).
- Süre kodun önündedir: süresi dolmuşta doğru ve yanlış kod aynı gövdeyi alır ve deneme harcanmaz (USR-012).
- Yanlış kod denemeyi satır kilidi altında bir artırır; beşinci yanlış doğrulamayı aynı deyimde yakar ve `VERIFICATION_CODE_INVALID` alır, altıncı istek doğru kodla bile `EXHAUSTED` alır. **Beş yarış altında da beştir:** onay önce kullanıcının satırını, sonra doğrulamanınkini `FOR UPDATE` ile kilitler (Karar 5); tabloda `failed_attempts BETWEEN 0 AND 5` ve "yanmış ⇔ deneme 5" `CHECK`'leri de vardır. On iki eşzamanlı yanlış kodun tam beşi `CODE_INVALID`, yedisi `EXHAUSTED` alır.
- **Görünürlük:** yanmış ve iptal edilmiş doğrulama onay, okuma ve iptalde yoktan ayırt edilmez (onaydaki tek istisna yanmışın `EXHAUSTED`'i). İptal edilmiş doğrulamanın onayı 404'tür: yeniden göndermeyle değişen eski kodun (USR-010'un C1'i) cevabı "böyle bir doğrulama yok"tur. Onaylanmış doğrulamanın iptali `204` döner ve bir şey değiştirmez. Bu yüzden `VerificationStatus`'un `BURNED` ve `CANCELLED` değerleri bugün hiçbir yanıtta dönmez; sözleşmenin açıklaması bunu yazar (açık soru 3).

### 3. E-posta adresi başka bir hesaptaysa: yalnız kodu doğru girene, onayda

`VERIFICATION_TARGET_IN_USE` sözleşmede hem başlatmada hem onayda listeliydi. Başlatmada dönmesi, oturum açmış **her** kullanıcıya herhangi bir adresin kayıtlı olup olmadığını sorma imkânı verirdi: kayıt (USR-001) ve parola sıfırlama (USR-018) bunu bilerek kapatmışken üçüncü bir numaralandırma kanalı açılırdı. Java bunu yapıyor (`UserManager` profil yamasında `existsByEmail` ile ayrı bir hata dönüyor).

- **Başlatma hedefin hesap durumuna hiç bakmaz.** Kayıtlı ve serbest hedef aynı satırı, aynı işi, aynı bütçe tüketimini (Karar 4'ün küresel tavanları dahil) ve aynı `201`'i alır; kod iki dalda da gönderilir.
- **E-postada bilgi yalnız onayda, doğru koddan sonra döner.** Doğru kodu giren adresin sahibi olduğunu kanıtlamıştır. Yazım `users_email_key`'e takılır, transaction geri alınır: doğrulama tüketilmez, değer yazılmaz, `409 VERIFICATION_TARGET_IN_USE` döner.
- **Telefonda numara onaylayana geçer** (Karar 10); telefon için `VERIFICATION_TARGET_IN_USE` dönmez.
- Sözleşmede `startMyVerification`'ın `x-error-codes` listesinden `VERIFICATION_TARGET_IN_USE` çıkarıldı. `409` yanıtı sözleşmede kalır (stil rehberi §2: durum kümesinden yanıt silmek uyumsuz) ve açıklama "bugün dönmez" diyor.
- **`updateMyProfile` için not (profil PR'ı):** sözleşmede bu operasyonun listesinde `VERIFICATION_TARGET_IN_USE` var (`api/openapi.yaml`, `updateMyProfile`). Profil yaması doğrulamayı yalnız başlatacağı için o kodu döndüremez; profil PR'ı onu listeden çıkarmalı, yerine `VERIFICATION_RESEND_TOO_SOON`'u ve `PASSWORD_MISMATCH`'i (Karar 1b) eklemeli, `429` ve `403` yanıtlarını sağlamalıdır.

### 4. Bütçe ve oran sınırı

**1. HTTP kovaları** (ADR-0021, ADR-0025 §8). Başlatma sırasıyla üç kovadan jeton alır; bir kova reddederse sonrakilerden jeton harcanmaz:

- **Çağıranın kovası** (`verification:user:<id>`, `auth` grubunun `PerCustom`'ı, dakikada 10, burst 5). İlk sırada durur: kendi kovasını aşan bir çağıranın istekleri adres bütçesine hiç ulaşmaz.
- **İstemci adresi başına saatlik bütçe:** `verification_address` grubunda, **saatte 60, burst 60** (`KOALA_VERIFICATION_STARTS_PER_IP_HOUR`, 1–1000). ADR-0032 Karar 6'daki `registration_address` kalıbıdır: tek bir adresin çok hesapla küresel SMS tavanını doldurmasını sınırlar. **CGNAT:** Türkiye'de mobil hatlar yoğun taşıyıcı NAT'ı (CGNAT) arkasındadır; bir mobil çıkış adresini aynı anda yüzlerce abone paylaşır. Bu yüzden değer kayıttakinden (10) yüksektir ve kova çağıranın kovasının **arkasındadır**: tek bir çağıran adresin bütçesini yakamaz, bütçeyi ancak çok sayıda ayrı hesap birlikte tüketebilir. 429 oranı `koala_http_rate_limited_total{group="verification_address"}` ile izlenir; meşru kullanıcılar bir NAT arkasında 429 alıyorsa değer yükseltilir (runbook).
- **(Hedef, adres) kovası** (`auth` grubunun `PerCustom`'ı): hedefin anahtarlı özeti ve adres.

Sırayı `TestVerificationCallerBucketComesFirst` bağlar: kendi kovasını aşan bir çağıran aynı adresteki ikinci çağıranın başlatmasını engellemez; sıra ters dönerse reddedilen istek adresin son jetonunu alır ve test kırmızıdır.

**Hedef geneli HTTP kovası yoktur.** İlk sürümde başlatma kayıt başlatmanın iki kovasını kullanıyordu; hedef geneli kova (`auth_account`) herkesin harcayabildiği bir kovaydı. Bir saldırgan onu sürekli boşaltarak hedefin sahibinin doğrulamasını süresiz engelleyebilirdi. Hedef bombardımanını aşağıdaki veritabanı bütçesi zaten sınırlıyor. `TestVerificationRateLimitKeys` bunu, herhangi bir kovanın burst'ünden fazla ayrı çağıranın aynı hedefe ayrı adreslerden başlatmasıyla sınar: hepsi veritabanı bütçesine ulaşır, hiçbiri boşalmış bir kovaya takılmaz. Onay, doğrulamanın kimliğinin kovasından (`auth`) jeton alır. `auth` grubunun kimlik başına sınırı yoktur (`PerIdentity` tanımsız); kenarın adres başına sınırı (dakikada 30) her isteğe uygulanır.

<!-- rate-limit-groups: internal/app TestRateLimitGroups bu tabloyu ADR-0021, ADR-0025 ve ADR-0032'ninkilerle birlikte okur; ilk dört sütunun biçimi "dk / burst", "N/sa / burst" ya da "—". -->
| Grup | Adres başına | Kimlik başına | Özel anahtar | Java karşılığı (IP başına) | Java'ya oran |
|---|---|---|---|---|---|
| `verification_address` | — | — | 1 / 60 | yok (Java'da doğrulama genel `WRITE` kovasındaydı, 60/dk) | yeni; saatte 60 (dakikada bir dolar, burst 60) |

**2. İstek içi yeniden gönderme bütçesi**, senkron ve `429 VERIFICATION_RESEND_TOO_SOON` + `Retry-After` ile:

- **Bekleme:** hedefin son kodundan sonra 60 saniye.
- **Hedef başına saatlik bütçe:** bir hedef bir saatte en çok 5 kod alır.
- İkisi **bütün kullanıcılar üzerinden** sayılır: amaç hedefin sahibini bombardımandan korumaktır; kullanıcı başına bir bütçeyi saldırgan hesap sayısıyla çoğaltırdı.
- **Kullanıcının kanal başına günlük tavanı:** SMS'te 10, e-postada 20 doğrulama, bütün hedefler üzerinden.
- Sayım ve yazım, hedefin anahtarı üzerinde bir `pg_advisory_xact_lock` altında olur: eşzamanlı başlatmalar bütçeyi birlikte aşamaz (`TestConcurrentStartsKeepTheTargetBudget`: sekiz kullanıcının aynı numaraya eşzamanlı başlatmasından biri geçer).
- **Tutulan satır başlatmada sayılmaz, gönderimde yeniden denetlenir.** Kodu gönderilmeden tutulan (küresel tavan ya da gönderim penceresi) doğrulama, kodu sonradan gönderilmedikçe hedef ve kullanıcı bütçesinde sayılmaz; böylece kodu tutulan kullanıcı yeniden başlatabilir. Ama tutulan satırın kodu sonunda gönderilecekken iş, başlatmanın yaptığının aynısını yapar: hedefin başlatma kilidini alır ve bütçeyi (bekleme, hedefin saatlik bütçesi, kullanıcının günlük tavanı) yeniden sayar. Bütçe artık izin vermiyorsa kod üretilmez ve doğrulama **gönderilmeden iptal edilir** (`canceled_at`; sahibine iptal edilmiş doğrulama gibi 404 görünür, bütçede sayılmaz); `koala_verification_codes_total{outcome="throttled"}` artar. Yeniden denetim olmasaydı tavan doluyken N hesap aynı numaraya başlatabilir ve tavan boşalınca numara N kod alırdı; tek kullanıcı da günlük tavanı aşardı (`TestHeldCodesRecheckTheTargetBudget`, `TestHeldCodesRecheckTheDailyCap`). Böylece hedef bütçesi, tutulan satırlarla da, bir hedefin alabileceği kodların üst sınırıdır.
- Neden senkron: ADR-0032'deki posta bütçesi sessizdi, çünkü istek oturumsuzdu ve cevabı adresin kayıtlı olup olmadığına göre değişirse numaralandırma kanalı olurdu. Burada bütçe hedefin hesap durumuna hiç bakmaz (Karar 3).

**3. Kanal başına küresel saatlik tavan**, işte:

- **SMS:** bütün kullanıcılar için saatte en çok `KOALA_SMS_PER_HOUR` (öntanım **200**) kod.
- **E-posta:** saatte en çok `KOALA_VERIFICATION_MAILS_PER_HOUR` (öntanım **500**) kod postası. Gerekçe ADR-0032 Karar 6'daki kayıt postası tavanıyla aynıdır: aktarıcının itibarı.
- Tavan doluyken kod **tutulur**: satır `held_at` ile işaretlenir (başlatmada bütçede sayılmaz), satır ilk kez tutulduğunda `koala_verification_codes_total{channel,outcome="held"}` bir kez artar (ertelemeler sayılmaz), iş `river.JobSnooze` ile **30 saniye** (`CapSnooze`) ertelenir ve yeniden dener (erteleme deneme sayısını artırmaz). Önceki bir denemenin ürettiği ama gönderemediği kod (iş yalnız başarısız bir denemeden sonra yeniden koşar) tutulurken silinir ve gönderilmemiş sayılır; ilk sürümde böyle bir satır tutulamıyor ve iş kodu sessizce bırakıyordu (`TestRetriedCodeIsHeldNotDropped`). Doğrulamanın ömründen erteleme ve gönderim penceresi kadar kalmamışsa iş vazgeçer. Uyarılar: `KoalaVerificationTextCapReached`, `KoalaVerificationMailCapReached`.
- Sayım işin içindedir ve eşzamanlı işler birbirini görmez: tavan birkaç ileti aşılabilen yumuşak bir sınırdır. Tavan isteğe değil işe konur: senkron bir küresel 429 saldırgana tavanın ne zaman dolduğunu tam olarak söylerdi. Tavanı kayıtlı ve serbest hedef aynı biçimde tüketir, çünkü işin dalı yoktur.

**4. Hedef anahtarı:** saklanan hedef normalize edilmiş değerdir; bütçe anahtarı e-postada posta kutusu (`+etiket` atılır, Gmail'de noktalar silinir; ADR-0032'nin `mailboxKey`'i), telefonda E.164 numaranın kendisidir. Anahtar giriş biberiyle HMAC-SHA256'dır (`target_key`).

**Bütçe kanal başına ayrıdır.** E-posta ve SMS'in maliyeti ve tehdidi farklıdır. Ortak kalan yalnız hedef bütçesinin biçimidir (60 sn, saatte 5); kullanıcı tavanı ve küresel tavan kanal başına ayrı değerlerdir.

**Kabul edilen riskler:**

- **Hedefin doğrulaması bir saat engellenebilir.** Bir saldırgan, kurbanın numarasına ya da adresine bir saatte beş doğrulama başlatarak o saat boyunca kurbanın kendi doğrulamasını `429`'a düşürebilir. Hedef başına sınırın doğası budur (ADR-0032 Karar 6'nın aynı kabulü). Saldırgan her başlatma için kendi parolasını girmek zorundadır (Karar 1b).
- **Bir numaraya taciz.** Birden çok hesapla bir numaraya saatte 5, günde 120 SMS gönderilebilir; 12 hesap, günde 10 SMS tavanıyla, bir numarayı gün boyu rahatsız edebilir. Hedef bütçesi bunun üst sınırıdır, tutulan satırlar dahil (gönderimde yeniden denetim); adres başına bütçe tek kaynaktan gelen saldırıyı daha da daraltır. Azaltma: hacim uyarısı (`KoalaVerificationTextVolumeHigh`) ve İleti Merkezi'nin gönderim dökümü.
- **"Bu hedefe yakın zamanda kod gitti" sızar.** Bekleme ya da saatlik bütçe başka bir kullanıcının isteğiyle dolmuşsa çağıran bunu `429`'dan öğrenir. Bu, hedefin bir hesabı olup olmadığını söylemez (bütçe hesap durumuna bakmaz).
- **Dağıtık bir saldırı küresel SMS tavanını doldurabilir.** Tavanı dolduran her hesap günde 10 SMS'le, her adres saatte 60 başlatmayla sınırlıdır ve hesap açmak kayıt bütçelerine takılır (ADR-0032 Karar 6); yeterince hesap ve adresle tavan yine dolar. O saat meşru telefon doğrulamaları da kod almaz (kodları tutulur ve ertelenir). Tavanın varlık nedeni faturadır, sürekliliği değil; azaltmalar uyarılar, runbook ve yeniden başlatmadan önce ayarlanabilen değerlerdir.

### 5. Tek kilit sırası

Kullanıcıyla ilişkili satırları kilitleyen her yol şu sırayı izler:

1. Danışma kilitleri: numaranın sahiplik kilidi (`LockPhoneClaim`, iki anahtarlı biçim) ya da hedefin başlatma kilidi (`LockVerificationTarget`, tek anahtarlı biçim; ikisi ayrı kilit uzaylarıdır).
2. Kullanıcı satırları, **kimlik sırasıyla** (`lockUsers`): çağıranın ve varsa numaranın eski sahibinin.
3. Doğrulama satırı.
4. Parola sıfırlama satırları.

Başlatma kullanıcıyı kilitler, sonra başlatma kilidini alır. Bu, 1→2 sırasının tersidir; başlatma kilidini başlatmadan başka yalnız kod işi alır, ve iş kullanıcı satırına hiç dokunmaz: önce başlatma kilidini, sonra doğrulama satırını kilitler (1→3). Kilidi tutan başlatma doğrulama satırlarını beklerken o satırları kilitlemiş bir iş başlatma kilidini zaten almıştır; döngü kurulamaz.

İlk sürümde `CompletePasswordReset` önce token'ı tüketiyor (satırı kilitliyor), sonra kullanıcıyı kilitliyordu. E-posta onayı ise önce kullanıcıyı kilitleyip sonra kullanıcının açık sıfırlama token'ını iptal ediyor: eşzamanlı bir sıfırlama ve e-posta onayında kilitlenme döngüsü kurulurdu. `CompletePasswordReset` artık token'la kullanıcıyı kilitsiz bulur (`PeekPasswordReset`), sonra kullanıcıyı kilitler, en son tüketir; tüketilen token başka bir kullanıcınınsa (olamaz, özet tekil) reddeder. `ChangeMyPassword` ve sıfırlama isteğinin işi zaten önce kullanıcıyı kilitliyor.

### 6. SMS pompalama (toll fraud) savunması

- **Ülke ve numara türü izin listesi:** kod yalnız izin verilen ülkenin **cep** numarasına gider. Bugün yalnız Türkiye: `+90` ve ulusal numarası `5` ile başlayan on hane. Sabit hat, `0850`, `0900` ve her yabancı numara `REQUEST_INVALID` (`/target`) alır; hiçbir satır yazılmaz, iş kuyruğa girmez. Kural tek yerdedir (`authn.TextableNumber`); istemci (`iletimerkezi.National`) de `+90` dışını göndermeyi reddeder.
- **Hesap başına günlük tavan:** 10 SMS. E-posta için 20.
- **Adres başına saatlik bütçe:** 60 başlatma, çağıranın kovasının arkasında (Karar 4; CGNAT).
- **Küresel saatlik tavan:** 200 (Karar 4). Ölçülmedi, çünkü gerçek trafik yok.
- **Metrik ve uyarı:** `koala_verification_codes_total{channel, outcome}` (`sent`, `held`, `undeliverable`, `throttled`). `KoalaVerificationTextCapReached` tavan dolduğunda, `KoalaVerificationTextVolumeHigh` bir saatte 100'den fazla SMS gittiğinde çalar: pompalama tavandan önce hacim olarak görünür.

### 7. Telefon numarası normalizasyonu

`authn.NormalizePhone` numarayı E.164'e çevirir: boşluk, tire, nokta ve parantez atılır; `00` ile başlayan uluslararası önek `+` olur; ülke kodu olmayan Türk numarası (`05XX…`, `5XX…`, `905XX…`) `+90` alır. Ardından E.164 biçimi aranır; başka karakter ya da biçim reddedilir, tahmin edilmez. Saklanan, bütçe anahtarı olan ve SMS'e giden değer bu biçimdir; ekranda yalnız maskelisi döner (`+90******4567`).

Telefon numarası kişisel veridir: log'a, hata metnine ve metrik etiketine yazılmaz. İleti Merkezi istemcisinin hata metinleri numara, metin ve kimlik bilgisi taşımaz; sağlayıcının serbest metin mesajı da hataya konmaz.

### 8. Onay değeri yazar; bildirimler

- Onay, doğrulamayı tüketir ve yeni değeri aynı transaction'da kullanıcıya yazar: e-postada `email` ve `email_verified = true`, telefonda `phone` ve `phone_verified = true`. Değer onaylanana kadar yazılmaz (USR-014'ün onay yarısı).
- **E-posta değişikliği** aynı transaction'da kullanıcının açık parola sıfırlama token'ını iptal eder (bağlantı eski adrese gitmişti) ve değişikliği **eski adrese** postayla bildirir (`contact_changed`; ASVS V2.2.3). Telefon değişikliği hesabın adresine bildirilir. Bildirim işi eski adresi taşır, çünkü hesap artık onu tutmuyor; `river_job`'un `pii` gerekçesi buna göre genişletildi.
- **Aynı değer yeniden doğrulanırsa** (hesaptaki adres ya da numara) yalnız doğrulanmış bayrağı yazılır, bildirim gitmez.
- **Profil yamasının doğrulama başlatması** (USR-014'ün diğer yarısı) profil PR'ına kalır; Karar 1b ve 3'teki yükümlülükleriyle.
- **Profil senkronu giden kaydı** (USR-022, USR-023): Java onaydan sonra `publishUserProfileUpdated` çağırıyor; giden kaydın yazımı profil PR'ının işidir ve onayın yazdığı değeri de kapsamalıdır.
- **Faz 5'e (bildirim) devredilenler:** Java her SMS ve postayı bir `Notification` satırıyla kaydediyor (kategori, öncelik, teslim talebi ve sonucu, sağlayıcının sipariş kimliği; `NotificationManager`, `SmsConsumer`) ve metinleri `messages_en.properties` ile İngilizce de sunuyor. Bu PR'da bildirim kaydı yok, iletiler yalnız Türkçe. İkisi de plan §6.1 Faz 5'in (bildirim kaydı, şablonlar, tercihler) kapsamıdır ve o fazda doğrulama iletileri de oraya bağlanır.

### 9. SMS altyapısı

- **`internal/platform/sms`:** `Message{To (E.164), Text}` ve `ErrUndeliverable`.
- **`internal/integrations/iletimerkezi`:** düz `net/http` ile tek bir JSON `POST`; yeni bağımlılık yok. Biçim sağlayıcının belgesidir:
  - Uç `https://api.iletimerkezi.com/v1/send-sms/json`; gövde `{"request": {"authentication": {key, hash}, "order": {sender, iys: "0", iysList: "", message: {text, receipents: {number: ["5XXXXXXXXX"]}}}}}`. Alıcı anahtarı sağlayıcının yazımıyla `receipents`'tir; belge "`recipients` çalışmaz" diyor. Kodda iki parçada yazılır ki yazım denetçisi bir kablo adını "düzeltmesin". Numara ulusal on hane gider (belgenin kabul ettiği biçimlerden biri).
  - **`sendDateTime` gönderilmez.** Belgede isteğe bağlı bir dizedir (`dd/MM/yyyy HH:mm`); yoksa ileti hemen gider. İlk sürüm boş bir dizi gönderiyordu, sağlayıcı onu biçimsiz tarih (`457`) sayarsa hiçbir SMS gitmezdi. Java da göndermiyor.
  - Kimlik bilgisi yalnız gövdededir. **Yönlendirme izlenmez:** istemcinin `CheckRedirect`'i her yönlendirmeyi reddeder, çünkü 307 ve 308 gövdeyi, yani kimlik bilgisini yeni adrese taşır.
  - Yanıt `{"response": {"status": {"code", "message"}, "order": {"id"}}}`; `code` HTTP durumunu yansıtır ve belgede hem dize hem sayı görünür, ikisi de okunur. Belirleyici gövdedeki koddur.
  - **Kalıcı ret** işi iptal ettirir: `452` (alıcı yok, geçersiz ya da engelli), `453` (sipariş azami boyutu aştı: yedi ileti bölümü), `454` (metin boş), `466` (geçersiz numara, genellikle yanlış operatör öneki). **İşletimsel ret** (`400`, `401` kimlik ya da "API erişimine izin ver" ayarı, `402` bakiye, `450` onaysız başlık, `451` yineleme, `457`, `459`, `460`, `468`–`470`, `503`), okunamayan yanıt ve ağ hatası yeniden denenir.
  - **Java'dan iki ayrılık**, ikisi de Java'nın kusuru: `IletiMerkeziSmsHelper` alıcıyı `recipients` diye gönderiyor (belgeye göre `452`) ve yanıtı `response` sarmalayıcısı olmadan okuyor (`response.getStatus()` boş kalır, her gönderim `ExternalServiceException` ile düşer). Java'nın SMS gönderimi bu yüzden hiç çalışmamış olmalı.
- **Yapılandırma:** `KOALA_ILETIMERKEZI_API_KEY` ve `_API_HASH` (`config.Secret`), `KOALA_ILETIMERKEZI_SENDER` (onaylı başlık, 3–11 ASCII), `KOALA_SMS_PER_HOUR`, `KOALA_SMS_DRIVER`. `config.ServeSMSErrors` `ServeMailErrors` kalıbındadır: staging ve production'da hesap eksikse `serve` açılmaz; yarım yapılandırma her ortamda hatadır; `local` ve `test`'te sağlayıcı yoksa ileti düşürülür.
- **`KOALA_SMS_DRIVER`:** öntanım `iletimerkezi`. `log` hiçbir şey göndermez ve yalnız "gönderildi" diye log'a yazar (numara ve kod yazılmaz); İleti Merkezi değişkenlerini istemez. Sağlayıcının başlık onayı gün sürebildiği için staging onu bekleyebilir. `log` yalnız `local`, `test` ve `staging`'de kabul edilir; `production`'da yapılandırma yüklenirken açılış durur, `ServeSMSErrors` de ayrıca reddeder.
- **Test:** gerçek sağlayıcı yoktur. `fakes.IletiMerkezi` belgedeki biçimi ve hata kodlarını konuşur: yabancı kimlik bilgisine `401`, okunamayan isteğe `400`, `iys` yoksa `468`, biçimsiz `sendDateTime`'a (boş dizi dahil) `457`, başlık yoksa `459`, uzun ya da Türkçe harfli başlığa `460`, onaysız başlığa `450`, boş metne `454`, yedi bölümü aşan metne `453`, alıcı üyesi yoksa (doğru yazılmış `recipients` dahil) ya da numara listesi boşsa `452`, biçimsiz numaraya `466`. Yanıtın HTTP durumu kodu yansıtır.

### 10. Geri dönüştürülmüş numara: yeni doğrulanmış sahip devralır

Operatörler kullanılmayan numaraları yeni abonelere verir. Bir numara başka bir hesapta doğrulanmışsa ve yeni sahibi onu kendi hesabında doğrularsa **numara yeni sahibe geçer** (WhatsApp ve Telegram modeli): aynı transaction'da eski hesaptan numara ve `phone_verified` kaldırılır, eski hesabın adresine bir bildirim (`phone_released`) kuyruğa girer. Bu numaralandırma açmaz: onaylayan kişi numaraya gelen kodu girmiştir. E-postada `VERIFICATION_TARGET_IN_USE` aynen kalır; adresler geri dönüştürülmez ve adres giriş kimliğidir.

Yarış ve kilitler:

- **SIM swap:** devralma kurbanın hesabına erişim vermez, yalnız numarayı alır: onay oturum açmış bir çağıranın kendi hesabına numara yazar, eski hesapta oturum, parola ya da e-posta değişmez. Bugün SMS ile hesap kurtarma ya da giriş yok. İleride SMS ile kurtarma gelirse devralma akışı yeniden değerlendirilir, çünkü o gün numarayı ele geçiren eski hesabı da ele geçirebilir.
- **Askıya alınmış hesap:** numarası da devralınır ve o hesaba bildirim gider. Bilinçli: yeni sahip numaranın sahibi olduğunu kanıtladı; askıya alma numarayı rezerve etmek için bir gerekçe değildir.
- Bir numaranın onayları numaranın **sahiplik kilidi** altında sıralanır (`LockPhoneClaim`, `pg_advisory_xact_lock(sınıf, anahtar)`). Numarayı yazan tek yol onaydır ve hep bu kilidi alır; bu yüzden kilit altında okunan eski sahip, transaction boyunca sahip kalır ya da (başka bir numarayı devralırken) numarayı bırakmış olur; ikisi de güvenli yöndedir, çünkü kaldırma kilitli satırın güncel değerine bakar.
- Sonra çağıranın ve eski sahibin satırları **kimlik sırasıyla** kilitlenir (Karar 5). İki hesabın birbirinin numarasını aynı anda devralması kilitlenmeden biter (`TestNumberClaimsUnderConcurrency`); aynı numaranın iki eşzamanlı onayı sırayla geçer ve numara tek hesapta kalır.
- `users_phone_key` tekil indeksi kalır: kilidin dışından bir yazar olursa yazım `VERIFICATION_TARGET_IN_USE` ile geri alınır.

**Java'dan bilinçli ayrılık:** Java'da telefon tekilliği **yok**. `UserDao#existsByPhoneNumber` tanımlı ama hiçbir yerde çağrılmıyor; veritabanında da kısıt yok, aynı numara birden çok hesapta doğrulanmış durabiliyor. Go numarayı tek hesapta tutar ve devralmayla taşır.

### 11. Postalar

- **Kod postası kimseyi adlandırmaz.** Adres henüz doğrulanmamıştır; postada hesabın adı olsaydı, bir hesap sahibi kendi profil adını (istediği metni) istediği adrese gönderebilirdi (ADR-0032 Karar 3'ün kuralı). **Java'dan ayrılık:** Java'nın `email-verification-template`'i ad, soyad ve yeni adresi basıyor.
- SMS metni Java'nın metnidir, bir uyarıyla: "Koala doğrulama kodunuz: 123456. Bu kodu kimseyle paylaşmayın." Tek bölüme sığar; bunu bir test bağlar.

### 12. Tablolar ve saklama

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `user_verifications` | M2, `pii` | Hedef, hedef anahtarının HMAC'i, kodun anahtarlı özeti (doğrulama bitince silinir), deneme sayısı, gönderim ve tutulma anı, bitiş. Doğrulama geri gelmez, soft-delete'li değildir. **İki gün sonra hard delete:** en uzun bütçe penceresi bir gündür. `users`'a giden kenar `detach`'tir; JOIN testinde `Unauthenticated`, kod işinin sorgusunda `inner-by-design`. |

- `users`'a `phone` (E.164) ve `users_phone_key` (etkin satırlar arasında tekil) eklendi; view yeniden kuruldu.
- Doğrulama `users`'ın çocuğudur: başlatma satırı yazmadan önce kullanıcıyı `FOR UPDATE` ile kilitler.

### 13. Liste ve cursor

`listMyVerifications` açık doğrulamaları (onaylanmamış, iptal edilmemiş, yanmamış, süresi dolmamış) yeniden eskiye, `(created_at, id)` üzerinde iki yönlü keyset sayfalamayla döndürür. Bozuk cursor `CURSOR_INVALID` alır.

`problem.yaml` bu kodu sayfalamanın ortak (evrensel) 400'ü olarak tanımlar. Bağ denetiminin evrensel kümesi ise `httpx`'in ürettiği platform kodlarıdır (`httpx.PlatformCodes()`); cursor'ı bugün `httpx` değil bu modül çözüyor ve kodun sabiti `authn`'de. Denetim ulaşılan bir modül kodunu listede aradığı için kod operasyonun `x-error-codes`'una yazıldı (yanıt kümesi değişmedi). Sayfalama ortak bir platform yardımcısına taşındığında kod oraya ve evrensel kümeye geçer, liste kaydı düşer.

## Java paritesi

| Konu | Java | Go | Not |
|---|---|---|---|
| Kod uzayı | `100000 + nextInt(900000)` | `000000`–`999999` | ADR-0029 Karar 3 |
| Saklama | düz metin | anahtarlı HMAC | Karar 1 |
| Ömür | 15 dk | 10 dk | ADR-0029 Karar 3 |
| Deneme sınırı | yok | 5, yarışta da | Karar 2 |
| Sahiplik ihlali | 403 | gizli 404 | ADR-0029 Karar 3 |
| İkinci onay | kod bakılmadan 200 | 409 | USR-013 |
| Yeniden kimlik doğrulama | yok | mevcut parola | Karar 1b |
| E-postada tekilleme | `deleteByNewEmail`: aynı yeni adrese açık **bütün kullanıcıların** doğrulamalarını siler; bir kullanıcı başkasının doğrulamasını iptal edebilir | (kullanıcı, kanal, hedef) başına tek açık doğrulama; başkasınınkine dokunulmaz | **Java'dan bilinçli ayrılık** |
| Telefonda tekilleme | yok, her istek yeni geçerli kod | (kullanıcı, kanal, hedef) başına tek | USR-010 |
| Telefon tekilliği | yok (`existsByPhoneNumber` çağrılmıyor, kısıt yok) | `users_phone_key` + devralma | Karar 10, **Java'dan bilinçli ayrılık** |
| Hedef başkasındaysa | profil yamasında ayrı hata (numaralandırma) | e-postada yalnız doğru koddan sonra, telefonda devralma | Karar 3, 10 |
| SMS istemcisi | `recipients`, sarmalayıcısız yanıt: çalışmıyor | belgedeki biçim | Karar 9 |
| Kod postası | ad, soyad, adres | yalnız kod | Karar 11 |
| Bildirim kaydı, İngilizce metin | `Notification` satırı, `messages_en` | yok | Faz 5'e devredildi (Karar 8) |

## Sözleşme değişiklikleri

Beş operasyonun `x-not-implemented` işareti kalktı ve iskele metotları silindi. Değişiklikler (uç henüz hiçbir istemcide yok):

- `startMyVerification`: gövdede zorunlu `currentPassword`; `x-error-codes: [PASSWORD_MISMATCH, VERIFICATION_RESEND_TOO_SOON]` (`VERIFICATION_TARGET_IN_USE` çıktı); `403` yanıtı eklendi; açıklama bütçeyi, yeniden kimlik doğrulamayı, normalizasyonu, ülke kuralını ve "409 bugün dönmez"i yazar.
- `listMyVerifications`: `x-error-codes: [CURSOR_INVALID]`.
- `getMyVerification`, `cancelMyVerification`, `confirmMyVerification`: açıklamalar görünürlüğü, e-posta çakışmasını ve numara devralmayı yazar.
- `VerificationStatus`: `BURNED` ve `CANCELLED`'ın bugün dönmediği yazıldı.
- `ErrorCode` düzyazısında `PASSWORD_MISMATCH`, `VERIFICATION_TARGET_IN_USE` ve `VERIFICATION_RESEND_TOO_SOON` genişletildi; durumları değişmedi.

## Kapının bu PR'daki gerçek vakaları

- `TestHandlersReturnTheirDeclaredErrorCodes` beş yeni handler'ı kendiliğinden izler; adlandırılmış yerler on üçe çıktı. Denetim bu PR'da bir gerçek bulgu verdi: `startMyVerification`'ın listesindeki `VERIFICATION_TARGET_IN_USE`'a hiçbir yerden ulaşılmıyordu.
- Her kural kendi gerçek vakasına karşı sınanır: ülke izin listesi iki yönde, bütçenin üç sınırı ayrı ayrı (e-posta ve SMS günlük tavanı dahil), başlatma kilidi eşzamanlı başlatmayla, küresel tavanlar erteleme ve tutulan satırın bütçeden düşmesiyle, oran sınırı anahtarları üretim kovalarıyla (`TestVerificationRateLimitKeys`: çağıranın kovası, (hedef, adres) kovası, hedef geneli kovanın yokluğu, onay kovası; `TestVerificationStartsPerAddress`), yeniden kimlik doğrulama yanlış parola, kilit ve parolasız hesapla, devralma eşzamanlı ve çapraz onaylarla.

## Yerine geçtiği

1. **ADR-0029 Karar 3'ün "Saklama: SHA-256 özeti"** → Karar 1: anahtarlı HMAC-SHA256.
2. **ADR-0029 Karar 3'ün denetim sırası ("süre → deneme → kod")** → Karar 2: yanmış doğrulama (deneme) ve onaylanmış doğrulama süreden önce denetlenir.
3. **ADR-0029 Karar 3'ün "hedef başına" bekleme ve saatlik bütçesi** → Karar 4: bütün kullanıcılar üzerinden sayılır; kullanıcı başına günlük tavan, adres başına saatlik bütçe ve kanal başına küresel tavan eklendi; tutulan satır sayılmaz.
4. **ADR-0029 Karar 3'ün oran sınırı satırı ("adres ve hedef özeti başına")** → Karar 4: hedef geneli kova kaldırıldı, yerine kullanıcı başına kova; (hedef, adres) kovası kalır.
5. **Sözleşmenin `startMyVerification`'da `VERIFICATION_TARGET_IN_USE` vaadi** → Karar 3: bilgi e-postada yalnız onayda, doğru koddan sonra döner; telefonda numara devralınır (Karar 10).
6. **ADR-0032 Karar 3'ün "ömür üretimden sayılır" kuralı** → doğrulama kodları için geçerli değildir; ömür başlangıçtan sayılır (Karar 1). Bağlantılar için kural değişmez.
7. **ADR-0032 *Sonuçlar*'ındaki "OTP ve doğrulama senaryoları (USR-008…014) `deferred` kalıyor"** → USR-008…013 bağlandı; USR-014 profil PR'ını bekliyor.
8. **ADR-0032 Karar 7'nin `CompletePasswordReset` sırası** (önce token, sonra kullanıcı) → Karar 5: önce kullanıcı, sonra token.

ADR-0013, ADR-0014 ve ADR-0025'in kararları değişmiyor. ADR-0032'nin kararları yukarıdaki 6–8. maddeler dışında değişmiyor.

## Sonuçlar

- Bağlanan P0'lar: USR-008, USR-009, USR-010, USR-011, USR-012, USR-013 (`boundAhead`). USR-014 `deferred` kalıyor: onay yarısının testi bu PR'da (`TestConfirmedValueIsWrittenOnlyOnConfirmation`), profil yaması yarısı profil PR'ında.
- Profil PR'ının yükümlülükleri: e-posta ya da telefon değiştiren yamada mevcut parola (Karar 1b), `updateMyProfile`'ın kod listesinin düzeltilmesi (Karar 3), profil senkronu giden kaydı (Karar 8).
- Staging ve production `serve`'ü artık İleti Merkezi hesabı olmadan açılmaz; staging başlık onayı beklerken `KOALA_SMS_DRIVER=log` ile açılabilir (runbook).
- `river_job` bir bildirim işinde eski e-posta adresini taşır; River biten işi 24 saatte siler.

## Açık sorular

1. **Tavanların ve bütçelerin değerleri** (SMS saatte 200, e-posta saatte 500, kullanıcı başına günde 10/20, adres başına saatte 20, hacim uyarısı 100/sa) ölçülmedi; gerçek trafik görülünce `koala_verification_codes_total` ve `koala_http_rate_limited_total` ile gözden geçirilir.
2. **İleti Merkezi kablo biçimi** sağlayıcının belgesinden kuruldu; Java'nın istemcisi çalışan bir kanıt sunmuyor. İlk gerçek gönderim staging'de, gerçek bir hesapla elle doğrulanmalıdır. Hesapta sabit IP kısıtı ya da "API erişimine izin ver" ayarı varsa açılmalıdır.
3. **`VerificationStatus`'un `BURNED` ve `CANCELLED` değerleri** hiçbir yanıtta dönmüyor. Sözleşmenin bir sonraki büyük sürümünde kaldırılabilir.
