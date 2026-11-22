# ADR-0032: Kayıt, parola sıfırlama, hesap postası ve `x-error-codes` bağı

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K10, K12, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.1 (`platform/mailer`, `integrations/smtp`), §3.3 (`go-mail`), §3.5 (transaction ve garantili yan etki), §6.1 Faz 2; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0028](0028-recursion-rules-body-allowlist-and-gate-completeness.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 1–4, [ADR-0030](0030-object-storage-media-pipeline-and-exif.md); Java `origin/main` (0cbbce69): `AuthManager`, `PendingRegistrationManager`, `PasswordResetTokenManager`, `UserManager#updateCurrentUserPassword`, `LegalManager`, `AuthLinkManager` (okundu, koşturulmadı)
- Dış kaynaklar: OWASP ASVS 4.0.3 V2.2.3 (kimlik doğrulama bilgisi değişince güvenli bildirim), V2.5.x (kimlik bilgisi kurtarma); RFC 8314 §3.3 (posta gönderiminde TLS); RFC 5321
- Kod: `internal/authn/accounts.go`, `internal/authn/accounts_jobs.go`, `internal/authn/mail/`, `internal/platform/mailer`, `internal/integrations/smtp`, `internal/platform/fakes/smtp.go`, `internal/archtest/errorbinding_test.go`, `db/migrations/0042_registration_and_password_reset.sql`, `test/scenarios/contract_test.go`

## Bağlam

Faz 2'nin ilk handler PR'ı altı operasyonu uygular: `startRegistration`, `verifyRegistration`, `completeRegistration`, `requestPasswordReset`, `completePasswordReset`, `changeMyPassword`. Davranışları ADR-0029 Karar 2 ve 4'te yazılıdır; bu ADR onları değiştirmez. Uygulamanın gerektirdiği, ADR-0029'da olmayan kararları kayda geçirir:

- ADR-0029 Karar 1'in "Gereklilik" paragrafı: ilk handler PR'ı, handler'ın döndürdüğü kodları operasyonun `x-error-codes` listesine ve kodun `Kind`'ını düzyazıdaki duruma bağlayan denetimi kurar.
- İki operasyon posta gönderir. Postanın altyapısı (plan §3.1'in `platform/mailer` ve `integrations/smtp`'si) bu PR'da gelir.
- ADR-0029 Karar 1'in katalog bağı alan düzeyidir: `users` etiketinde 501 dönen operasyon kaldıkça `user` alanının hiçbir P0'ı bağlanamaz. Bu PR o alanın operasyonlarının bir kısmını getirir.

## Karar

### 1. `x-error-codes` ↔ handler bağı

`internal/archtest.TestHandlersReturnTheirDeclaredErrorCodes`, `x-not-implemented` taşımayan her operasyon için handler'ı bulur, ulaşabildiği hata yerlerini statik olarak toplar ve üç kuralı denetler:

1. Ulaşılan her kod operasyonun `x-error-codes`'unda yazılıdır; yalnız evrensel kodlar muaftır (`universalCodes`: `httpx.PlatformCodes()`, oturum kodları, `STAFF_PERMISSION_DENIED`, `NOT_IMPLEMENTED`; ADR-0029 Karar 1'in birinci sınırı).
2. Ulaşılan her yerin `apperr.Kind`'ı, `httpx.StatusOf` üzerinden, `ErrorCode` açıklamasının o kod için yazdığı duruma eşittir. Düzyazıyı okuyan tek yer `api.ErrorCodeStatuses`'tır; `api`'nin kendi testi de aynı fonksiyonu kullanır.
3. Listedeki her kod ulaşılan bir yerden döner. Ulaşılamayan liste üyesi kırmızıdır; izin yalnız operasyon ve kod başına gerekçeli bir muafiyetle (`unseenCodes`) verilir ve izin verilen kod sonradan görülürse muafiyet bayattır, kırmızıdır.

**Nasıl çalışır.** Handler, ikinci parametresi `apiv1.<Op>RequestObject` olan metottur; `internal/pending` (501 iskelesi) ve `internal/app` (iletme metotları) bu aramaya girmez. Tam bir tane bulunmazsa kırmızıdır. Sonraki her handler PR'ı bu yüzden **kendiliğinden** denetime girer: işaretini kaldırdığı operasyonun handler'ı aranır, bulunur ve izlenir; bir listeye ya da kayda ihtiyaç yoktur. Sözleşmede olup strict sunucunun dışında kalan operasyon (realtime'ın iki ucu, `exclude-tags: realtime`) gerekçeli `nonStrictOperations` listesinde olmak zorundadır; liste bayatlarsa kırmızıdır.

İzleme bir AST yürüyüşüdür, tip denetimli bir çağrı grafiği değildir: `golang.org/x/tools` onaylı listede değil (ADR-0003), ve standart kütüphanenin kaynak içe aktarıcısıyla bütün modülü tip denetlemek bu Mac'te ve CI'da doğrulanamayacak kadar kırılgan. Yürüyüş şunları izler:

- aynı paketin fonksiyonu (`f(…)`), modülün başka paketinin fonksiyonu (`pkg.F(…)`);
- alıcının metodu (`r.M(…)`) ve alıcının **yapı alanları** üzerinden ulaşılan metot (`r.f.g.M(…)`). Alanın bildirilen tipi modülün bir yapısıysa metot o tipte aranır. Aynı adlı iki metot (`Service.Login`, `StaffService.Login`) bu yüzden karışmaz.

Hata yeri, `kind`'ı bir `apperr.Kind` sabiti ve kodu modülün bir sabiti olan `apperr.New(kind, code)` ya da `apperr.Wrap(cause, kind, code)`'dur. Kod, bir yerel ada bağlanmış fonksiyon literalinin parametresiyse (`end := func(reason string, code apperr.Code) …`, refresh yollarının biçimi) o adın her çağrısında geçen sabit okunur. **Ulaşılıp okunamayan yer kırmızıdır, atlanmaz:** kind ya da kod sabit değilse test o yeri adlandırır ve "sabit yaz" der.

**Neyi görmez** (bilinçli, burada kayda geçiyor):

- **Arayüz üzerinden çağrı.** Arayüz tipli alan, fonksiyon değeri, parametre. Alanlar arası çağrı arayüzle yapılır (CLAUDE.md), yani bir alanın handler'ı başka bir alanın kodunu döndürürse yürüyüş onu görmez. Bu körlüğün iki yüzü farklı davranır: listede yazılı ama arayüzün arkasından gelen kod üçüncü kurala takılır (kırmızı, `unseenCodes` ister); **listede yazılmamış ve arayüzün arkasından gelen kod kapıyı geçer.** Tek açık yol budur.
- **Yerel değişkenin metodu** (`q := authndb.New(tx); q.M()`), gömülü alan, ve başka bir fonksiyonda değişkene konup taşınan kod.
- Bir fonksiyonun **parametresiyle** gelen kod (literal değil, adlı fonksiyon). Bu biçim bugün yok; görülürse okunamayan yer olarak kırmızıdır.

**Gerçek vaka gördüğünün kanıtı.** Boş kümede geçen denetim kanıt değildir, bu yüzden test kendi tabanını da denetler:

- İzlenen operasyon sayısının bir tabanı vardır (≥ 20; bugün 21).
- Yedi adlandırılmış yer, izlenen yerler arasında bulunmak zorundadır. Biri (`refreshUserSession` → `AUTH_REFRESH_TOKEN_REUSED`) yalnız literal-parametre çözümüyle görülebilir, biri (`completeRegistration` → `VERIFICATION_TARGET_IN_USE`) transaction kapanışının içindedir.
- `TestErrorTraceOnADemoModule` iki paketlik bir demo modülde neyi izlediğini, iki aynı adlı metodu ayırdığını, arayüzü izlemediğini ve okunamayan yeri bildirdiğini kanıtlar. `TestBindingFindings` karşılaştırmanın kırmızı ve yeşilini, `TestKindStatusesCoverEveryKind` ise `apperr`'a eklenen bir `Kind`'ın tabloya girmeden geçemediğini kanıtlar.
- **Faz 1'in handler'larında gerçek bir bulgu:** `refreshStaffSession` `x-error-codes`'unda `STAFF_LOGIN_FAILED` yazıyordu. Handler bu kodu hiç döndürmüyor; döndürdüğü üç refresh token kodu (`AUTH_REFRESH_TOKEN_INVALID`, `_EXPIRED`, `_REUSED`) ise listede yoktu. Durumların üçü de 401 olduğu için ADR-0029'un durum kümesi testi yeşil kalmıştı. Liste bu PR'da düzeltildi; bu uyumlu bir düzeltmedir, çünkü yanıt kümesi değişmedi.

Bu bulgu kuralın değerinin ölçüsüdür ve aynı zamanda sınırını gösterir: kural, **ulaşılabilen** kodu görür, ulaşılmayanı görmez. Statik izleme yapısı gereği aşırıdır: handler'dan ulaşılan her `apperr.New`'i sayar, o dalın gerçekte koşup koşmadığına bakmaz.

### 2. Adres alan iki istek yalnız iş kuyruğuna yazar

`startRegistration` ve `requestPasswordReset` adresin kayıtlı olup olmadığını **istek içinde sormaz.** İstek biçimi denetler (adres, isimler) ve tek bir River işini bir transaction'da kuyruğa yazar; kararı iş verir:

- kayıt: adres kayıtlıysa hiçbir şey yazılmaz ve posta gitmez (USR-001); değilse adresin açık kaydı iptal edilir, yenisi yazılır ve bağlantı postasının işi **aynı transaction'da** kuyruğa girer (USR-002);
- sıfırlama: adresin hesabı yoksa hiçbir şey olmaz; varsa kullanıcının açık token'ı iptal edilir, yeni istek satırı yazılır ve postasının işi aynı transaction'da kuyruğa girer (USR-017).

ADR-0029 "aynı gövde, aynı maliyet" diyor. Senkron yolda bu, kayıtlı dalda yazılmayan satırın ve atılmayan posta işinin maliyetini taklit etmek demekti (commit'in WAL yazımı dahil); taklit her zaman bir ölçüde ayrışır. Asenkron yolda iki dal **aynı işi yapar**, taklide gerek kalmaz. `202` yanıtı sözleşmede zaten "posta kuyruğa girdi" diyor.

Bedeli: iş, adresi ve isimleri taşır. `river_job` bu yüzden katalogda `pii: true`'dur. River biten işi saklama süresinden sonra siler (tamamlanan ve iptal edilen 24 saat, atılan 7 gün). Bağlantı postasının işi ise yalnız satırın kimliğini taşır, ne adresi ne token'ı.

### 3. Postadaki token'lar posta gönderilirken üretilir

Bağlantı token'ı ve sıfırlama token'ı, **postayı gönderen iş** tarafından üretilir: iş, 32 rastgele bayt üretir, SHA-256 özetini satıra tek bir koşullu `UPDATE` ile yazar (satır hâlâ açıksa), bu deyim commit olur, sonra posta gönderilir. Düz token yalnız o çağrıda ve postada yaşar: veritabanında, iş kuyruğunda ve log'da yoktur (USR-003'ün testi satırın metin hâlinde token'ı aramasıyla bunu sınar).

- **Ömür, üretimden sayılır:** bağlantı 1 saat (`RegistrationLinkTTL`), kayıt token'ı 15 dakika, sıfırlama 15 dakika (ADR-0029 Karar 2 ve 4).
- **Gönderim başarısız olursa** River işi yeniden dener (`auth_mail`, en çok 10 deneme, River'ın attempt⁴ geri çekilmesiyle birkaç saat). Her deneme yeni token üretir ve öncekinin yerine yazar; geç giden posta da çalışan bir bağlantı taşır. Ender bir yan etki: aktarıcı postayı kabul edip bağlantı kapanırken hata verirse ikinci deneme ikinci bir posta gönderir ve birincinin bağlantısı geçersiz olur.
- **Karşılaştırma sabit zamanlıdır** çünkü Go'da karşılaştırma yoktur: arama özetle yapılır (ADR-0025'in refresh token kalıbı), token Go'da hiçbir sırla karşılaştırılmaz.
- Kayıt token'ı (adım 2) istekte üretilir ve yanıtta döner; yeni bir doğrulama öncekinin yerine yazar. Bağlantı tüketilmez (kullanıcı iki kez basabilir).

### 4. Posta altyapısı

- `internal/platform/mailer`: `Message` ve modül şablonları. Bir mesajın konusu ve düz metni `text/template` ile `*.txt`'ten, HTML'i `html/template` ile `*.html`'den gelir (`html/template` eklediği her değeri kaçırır). Eksik anahtar hatadır, boş dize değil. Modül açılışta şablonlarının tam olduğunu denetler (`Templates.Check`); eksik şablonla servis açılmaz.
- `internal/integrations/smtp`: `github.com/wneessen/go-mail` (plan §3.3, MIT). Sürüm **v0.8.1**'dir: v0.8.2 08.11.2026 13:31 UTC'de yayımlandı ve bu PR yazılırken on dört günü dolmamıştı (CLAUDE.md "Bağımlılıklar"). Her gönderim bağlanır, tek mesaj gönderir ve kapanır. `local` ve `test` dışında şifreleme zorunludur: 465'te örtük TLS, diğer portlarda STARTTLS; STARTTLS sunmayan aktarıcıya gönderim düşer, düz metne inmez (RFC 8314). go-mail'in gönderim hatası alıcıları metninde taşır ve River işin hatasını saklar ve log'a yazar; bu yüzden hata yalnız aktarıcının kararıyla (aşama, SMTP kodu, geçici mi) sarılır.
- Yapılandırma `KOALA_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `KOALA_MAIL_FROM`, `KOALA_WEB_BASE_URL`. `config.ServeMailErrors` `ServeStorageErrors` kalıbındadır: staging ve production'da eksikse `serve` açılmaz (yoksa kayıt ve sıfırlama `202` döner, hiç posta gitmez ve bunu hiçbir şey bildirmez), yarım yapılandırma her ortamda hatadır. `local` ve `test`'te aktarıcı yoksa posta düşürülür ve yalnız konusu log'a yazılır; alıcı ve gövde (canlı token taşır) yazılmaz.
- Testte gerçek SMTP yoktur. `fakes.SMTP` geri döngü adresinde düz SMTP konuşan bir aktarıcıdır; STARTTLS sunmadığı için zorunlu TLS'nin sessizce atlanmadığını da kanıtlar. Servis testleri gönderici arayüzünün kayıt tutan bir uygulamasını kullanır.
- Bağlantılar Java'nın web yollarını kullanır (`<origin>/hesap-dogrula?token=…`, `<origin>/sifre-sifirla?token=…`), böylece web istemcisi rotalarını korur. Derin bağlantı çözümü istemcinin işidir (ADR-0029 Karar 11, `AuthLinkController` satırı).

### 5. Senaryo bağının operasyon düzeyi istisnası (`boundAhead`)

ADR-0029 Karar 1'in katalog bağı alan düzeyidir: bir etikette işaretli operasyon kaldıkça alanın her P0'ı `deferred` olmak zorundadır. `users` etiketinin 25 operasyonundan bu PR 6'sını getiriyor; kural olduğu gibi kalsa bu PR'ın 14 P0'ı bağlanamazdı ve bağlanmış testler yeşil koşarken katalog hâlâ "bekliyor" derdi.

Karar: `test/scenarios/contract_test.go`'daki `boundAhead` haritası, alanın son operasyonu gelmeden bağlanan P0'ı **test kodunda** operasyonlarına eşler. Kural şu hâli alır:

- donmuş alanda bir P0 ya `deferred`'dır ya da `boundAhead`'de adlandırılmıştır ve adlandırdığı operasyonların **hepsi** işaretsizdir;
- `boundAhead`'de olup hâlâ `deferred` olan senaryo, var olmayan ya da hâlâ işaretli bir operasyonu adlandıran kayıt, hiçbir donmuş alan P0'ına denk gelmeyen kayıt ve alanı artık donmuş olmayan kayıt kırmızıdır.

Harita katalogda değil test kodundadır, çünkü ADR-0029'un gerekçesi (senaryo API'den bağımsız dilde yazılır) hâlâ geçerlidir. Alan düzeyi kural kaybolmaz: haritada olmayan her P0 için aynen işler. Alanın son operasyonu geldiğinde harita o alandan boşalır.

### 6. Hedef başına posta bütçesi

E-posta bombası iki katmanla sınırlanır:

1. **HTTP kovası** (ADR-0021, ADR-0025 §8): kayıt başlatma ve sıfırlama isteği, adresin HMAC hedefinin iki kovasından jeton alır, (hedef, adres) `auth` grubunda ve hedef geneli `auth_account` grubunda. Girişten farkı, bilinen kaynak kaçışının olmamasıdır. Token sunan adımlar (kayıt doğrulama ve tamamlama, sıfırlama tamamlama) token özetinin kovasından, uygulama içi değişim kullanıcınınkinden jeton alır.
2. **İşteki bütçe** (yeni karar): bir adres (kayıt) ya da bir hesap (sıfırlama) son postasından sonraki **60 saniye** içinde ve bir saat içinde **5**'ten fazla bağlantı postası almaz. Bu, ADR-0029 Karar 3'ün OTP yeniden gönderme kuralının bağlantılara uygulanmasıdır. Bütçeyi aşan istek diğerleriyle aynı `202`'yi alır, posta gitmez ve yalnız sabit bir bilgi satırı log'a düşer (adres yazılmaz). Sayaç satırların kendisidir; bu yüzden iptal edilmiş satır saklama süresi boyunca durur.

HTTP kovası tek başına yetmezdi: hedef geneli kova dakikada 30 istek kabul eder, yani tek bir adrese saatte yüzlerce posta.

### 7. Oturumlar ve bildirim

- Sıfırlama kullanıcının **bütün** oturumlarını, değişim çağıran dışındakileri kapatır (ADR-0029 Karar 4; görevdeki yönlendirmeyle aynı, çelişki yok). Kapanan oturumların realtime bağlantıları commit'ten sonra kapanır.
- `user_sessions.revoke_reason`'a `PASSWORD_RESET` ve `PASSWORD_CHANGED` eklendi. Bu iki sebeple kapanan oturum, cihazını **bilinen iyi kaynak** olarak tanıtmaz (ADR-0025 §5'in `DEVICE_MISMATCH`, `REFRESH_REUSED`, `USER_REVOKED` listesine eklendi): sıfırlamada hesabın ele geçtiği varsayılır ve kapatılan cihaz saldırganınki olabilir. Meşru kullanıcının cihazı yeni bir girişle yeniden tanınır.
- Sıfırlama ve değişim, kullanıcının açık sıfırlama token'ını iptal eder (USR-017).
- İkisi de "parolan değişti" bildirimini aynı transaction'da kuyruğa yazar: bildirim yalnız commit olan değişiklik için gider (AUTH-006). Java bildirimi yalnız sıfırlamada gönderiyor; Go uygulama içi değişimde de gönderir (OWASP ASVS 4.0.3 V2.2.3). **Java'dan bilinçli ayrılık.**
- Uygulama içi değişimde yanlış mevcut parola `PASSWORD_MISMATCH` (403) alır ve giriş kilit sayacına yazılmaz; tekrarını kullanıcı başına `auth` kovası sınırlar. Personel değişimindeki (ADR-0025 §6) adres kilidi burada yoktur, çünkü çağıranın geçerli bir oturumu vardır.
- Denetimlerin sırası: onay eşleşmesi → parola kuralı → yeni parola mevcut parolayla aynı mı (verilen düz metinle) → mevcut parolanın doğrulanması. Hash gerektirmeyen denetimler önce gelir.

### 8. Tablolar ve sınıfları

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `pending_registrations` | M2, `pii` | Adres, isim, iki token'ın özeti. Yazıldıktan 30 gün sonra hard delete (USR-007). İptal edilmiş satır o süre boyunca durur, çünkü posta bütçesi onu sayar. Adres başına tek açık satır, kısmi tekil indeksle. |
| `password_reset_tokens` | M2 | Token özeti; kullanıcı başına tek açık satır, kısmi tekil indeksle. 30 gün sonra hard delete. `users`'a giden kenar `detach`'tir (M2 kaskadlanamaz); tüketme kullanıcının aktif satırını kilitler, silinmiş kullanıcı sıfırlanamaz. |
| `agreement_versions` | M4 | Belge başına tek güncel sürüm; Java'nın tohumu (`TERMS_OF_SERVICE`, `PRIVACY_POLICY`, `EXPLICIT_CONSENT` zorunlu, `COMMERCIAL_MESSAGE` isteğe bağlı, hepsi `1.0`). Yeni sürüm migration'la gelir, eski emekli edilir, silinmez. |
| `consent_records` | M1, `pii` | Kim, hangi sürüme, ne zaman, evet mi hayır mı. Uygulama yalnız ekler, okumaz, güncellemez. Hesap anonimleştirilse de durur (Java ADR-0012). IP ve tarayıcı bilgisi **yazılmaz** (veri en aza indirme; açık soru 2). |

`users`'a `first_name`, `last_name`, `employer`, `email_verified`, `phone_verified`, `identity_verified` eklendi; view `soft-delete-view` yönergesiyle yeniden kuruldu (db/README). Yeni iki `detach` kenarı JOIN testinde `Unread` bildirildi: hiçbir GET onları okumuyor.

### 9. Rızalar

- Rızalar kayıt token'ı okunmadan önce denetlenir (USR-005): güncel bir sürümü adlandırmayan ya da iki kez adlandıran karar `REQUEST_INVALID` (`/consents/<i>`, `FIELD_VALUE_INVALID`), zorunlu bir belgenin güncel sürümü verilmemişse `CONSENT_REQUIRED`. İkisi de token hakkında bir şey söylemez ve onu tüketmez.
- Sıra: cihaz → rızalar → parola kuralı → token (önce tüketmeden bakılır, böylece çalışmayacak bir token argon2 maliyeti ödetmez) → tek transaction'da tüketme, kullanıcı, rıza kayıtları, cihaz, oturum ve giriş denemesi satırı.
- Adres adım 1 ile 3 arasında başka bir hesap almışsa `VERIFICATION_TARGET_IN_USE` (409); transaction geri alınır, kayıt token'ı tüketilmez.

### 10. Denetim kaydı ve PII

Bu uçlar personel uçları değildir; ADR-0026'nın denetim kaydı onlara uygulanmaz. Adres ve isim log'a yazılmaz; posta bütçesi ve düşürülen posta yalnız sabit mesaj ve konu yazar. Yanıtlar PII taşımaz.

## Yerine geçtiği

1. **ADR-0029 Karar 1'in katalog bağının "granülerlik alan düzeyidir" cümlesi** → Karar 5: alan düzeyi kural kalır, ama `boundAhead`'de operasyonlarıyla adlandırılan P0 alan donmuşken bağlanabilir.
2. **ADR-0029 Karar 1'in üçüncü sınırının "Gereklilik" paragrafı** → Karar 1 ile karşılandı.
3. **ADR-0029 Karar 4'ün "kayıtsız dalda üretilen token atılır" cümlesi** → Karar 2 ve 3: kayıtsız dalda token üretilmez, çünkü istek hiç token üretmez; eşitlik istek yolunun kendisinde kurulur. USR-018'in `target` metni buna göre güncellendi.
4. **ADR-0025 §5'in bilinen cihaz kuralı** → genişletildi: parola sıfırlama ve değişimiyle kapanan oturum cihazına kefil olmaz (Karar 7).
5. **ADR-0029 *Sonuçlar*'ındaki "`pending_registrations`'ın temizlik işi"** → bu PR'da (`Accounts.Purge`, `auth_purge` işi).

## Sonuçlar

- Bağlanan P0'lar: USR-001…007, USR-015…021; AUTH-006, AUTH-007, AUTH-008 ve AUTH-009'un `partial`'ı kapandı (kural artık her yüzeyde çağrılıyor). OTP ve doğrulama senaryoları (USR-008…014) ile profil senkronu (USR-022, 023) `deferred` kalıyor.
- Bir operasyonu uygulayan her PR artık `x-error-codes`'unu handler'la bağlamak zorunda; liste yanlışsa ya da `Kind` düzyazıyla çelişiyorsa `verify` job'ı kırmızıdır.
- **Medya:** ADR-0030'un `Complete` durum kapısı ve `RejectionCorrupt` kod karşılığı medya uçlarının PR'ına devredildi; bu PR medyaya dokunmaz.
- Bu PR'da kalanlar:
  - `completeRegistration` sözleşmede `Idempotency-Key` ister ve başlık doğrulanır, ama **tekrar deposu yok** (stil rehberi §7'nin `idempotency_keys`'i hiçbir operasyonda uygulanmadı). Yanıtı kaybolan bir tamamlama tekrarlanırsa kayıt token'ı tüketilmiş olduğu için `REGISTRATION_TOKEN_INVALID` alır; kullanıcı parolasıyla giriş yapabilir. Platform özelliği olarak ayrı bir PR'ın işidir (açık soru 3).
  - Java kayıttan sonra hoş geldin postası gönderiyor; bu PR göndermiyor (içerik ürün kararı).
  - `COMMERCIAL_MESSAGE` rızası kaydedilir ama İYS'ye iletilmez; İYS giden kaydı Faz 9'un (`privacy`) işidir.
  - Rıza kayıtları bugün `authn`'in sorgularıyla yazılıyor; `privacy` modülü geldiğinde oraya taşınır.

## Açık sorular

1. **Posta bütçesinin değerleri** (60 sn, saatte 5) OTP'den taşındı; ürün başka bir değer isterse yalnız iki sabit değişir.
2. **Rıza kaydı kanıt olarak IP ve tarayıcı bilgisi taşısın mı, ve ne kadar saklansın?** Java ikisini de yazıyor; bu PR yazmıyor ve `consent_records`'a saklama süresi koymadı (M1, silinmez). Hukuki karardır.
3. **Idempotency deposu** hangi PR'da gelsin? İlk ihtiyacı bu PR'ın `completeRegistration`'ı.
4. **Sözleşme belgelerinin güncel sürümleri** (`1.0`) Java'nın tohumundan alındı; istemci onları bugün bir uçtan okuyamıyor (sürüm listesi ucu sözleşmede yok).
