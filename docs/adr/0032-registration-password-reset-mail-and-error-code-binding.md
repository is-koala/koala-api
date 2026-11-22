# ADR-0032: Kayıt, parola sıfırlama, hesap postası ve `x-error-codes` bağı

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K10, K12, K19, S4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.1 (`platform/mailer`, `integrations/smtp`), §3.3 (`go-mail`), §3.5 (transaction ve garantili yan etki), §6.1 Faz 2; [ADR-0010](0010-job-queue-river.md), [ADR-0013](0013-soft-delete.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0028](0028-recursion-rules-body-allowlist-and-gate-completeness.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 1–4, [ADR-0030](0030-object-storage-media-pipeline-and-exif.md); Java `origin/main` (0cbbce69): `AuthManager`, `PendingRegistrationManager`, `PasswordResetTokenManager`, `UserManager#updateCurrentUserPassword`, `LegalManager`, `AuthLinkManager`, `CommercialConsentSource`, `welcome-email-template.html`, `2026-09-05-05-versioned-agreement-content-urls.xml` (okundu, koşturulmadı)
- Dış kaynaklar: OWASP ASVS 4.0.3 V2.2.1 (kimlik doğrulama denemesinin sınırlanması), V2.2.3 (kimlik doğrulama bilgisi değişince güvenli bildirim), V2.5.x (kimlik bilgisi kurtarma); RFC 8314 §3.3 (posta gönderiminde TLS); RFC 5321; Ticari İletişim ve Ticari Elektronik İletiler Hakkında Yönetmelik (onay kayıtlarının saklanması; hukuk teyidi bekliyor)
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
3. Listedeki her kod ulaşılan bir yerden döner. Ulaşılamayan liste üyesi kırmızıdır; izin yalnız operasyon ve kod başına gerekçeli bir muafiyetle (`unseenCodes`) verilir.

**Nasıl çalışır.** Handler, ikinci parametresi `apiv1.<Op>RequestObject` olan metottur; `internal/pending` (501 iskelesi) ve `internal/app` (iletme metotları) bu aramaya girmez. Tam bir tane bulunmazsa kırmızıdır. Sonraki her handler PR'ı bu yüzden **kendiliğinden** denetime girer: işaretini kaldırdığı operasyonun handler'ı aranır, bulunur ve izlenir. Sözleşmede olup strict sunucunun dışında kalan operasyon (realtime'ın iki ucu, `exclude-tags: realtime`) gerekçeli `nonStrictOperations` listesinde olmak zorundadır.

İzleme bir AST yürüyüşü ve küçük bir tip çıkarımıdır, tip denetimli bir çağrı grafiği değildir: `golang.org/x/tools` onaylı listede değil (ADR-0003). Yürüyüş karşılaştığı her çağrıyı çözer:

- modülün fonksiyonları: düz, paket nitelikli ya da generic (`F[T](…)`);
- metotlar: alıcıda, parametrede, yerel değişkende, alan zincirinde ya da bir çağrının sonucunda. Her birinin tipi bildiriminden ya da onu atayan çağrının sonuç tipinden çıkarılır. Aynı adlı iki metot (`Service.Login`, `StaffService.Login`) bu yüzden karışmaz;
- arayüz metodu: modülde arayüzün bütün metotlarını taşıyan her tipe;
- argüman olarak geçen fonksiyon değerleri (metot değeri, paket fonksiyonu) çağrılmış gibi ziyaret edilir.

Yalnız **`*apperr.Error` kurabilen** paketlere girilir: `apperr`'ı import edenler ve böyle bir paketi import edenler. Başka bir modül paketine ya da modül dışına giden çağrı bir modül kodu üretemez, izlenmez. `authndb`, `db`, `clock`, `tokenkeys` ve `authz` bu yüzden yürüyüşe girmez.

**Çözülemeyen çağrı kırmızıdır, sessizce atlanmaz.** Bunlar: tipi bilinmeyen alıcı, indeksli ya da hesaplanmış çağrılan, modülde hiçbir tipin uygulamadığı bir arayüz, fonksiyon tipli bir alan. Son ikisi yalnız imza `error` döndürüyorsa bulgudur. Her biri yalnız gerekçeli bir muafiyetle (`unresolvedCalls`, en az 20 karakter) açılır. Bugün tek muafiyet `internal/authn.jobQueue`'dur: River'ın istemcisi onu modül dışında uygular. Kullanılmayan muafiyet bayattır ve kırmızıdır.

Hata yeri, `kind`'ı bir `apperr.Kind` sabiti ve kodu bir sabit olan `apperr.New(kind, code)` ya da `apperr.Wrap(cause, kind, code)`'dur. Kod, bir yerel ada bağlanmış fonksiyon literalinin parametresiyse (`end := func(reason string, code apperr.Code) …`, refresh yollarının biçimi) o adın her çağrısında geçen sabit okunur. Paket düzeyi `var errX = apperr.New(…)` adının geçtiği her yerde hata yeri sayılır. **Ulaşılıp okunamayan yer kırmızıdır.**

Muafiyet listeleri de denetlenir: gerekçesi 20 karakterden kısa olan, var olmayan bir operasyona ya da operasyonun listelemediği bir koda verilmiş `unseenCodes` kaydı kırmızıdır.

**Neyi görmez** (bilinçli):

- **Kuramayan bir paketin üzerinden gelen kod.** Bir platform paketi, kurabilen bir paketin uyguladığı bir arayüzü ya da geri çağrıyı çağırırsa ve oradan bir kod geri dönerse yürüyüş onu görmez. Listede yazılı bir kod ise üçüncü kurala takılır.
- Aynı fonksiyonda **aynı adla farklı tipte iki yerel değişken** (ilk bildirim kazanır), bir düzeyden derin gömülü metot, ve başka bir fonksiyonda değişkene konup taşınan kod.

**Gerçek vaka gördüğünün kanıtı.** Boş kümede geçen denetim kanıt değildir, bu yüzden test kendi tabanını da denetler:

- İzlenen operasyon sayısının bir tabanı vardır (≥ 20; bugün 21).
- Hiçbir modül bildirimine ulaşmayan iz ("handler'dan çıkmayan iz") kırmızıdır.
- On adlandırılmış yer, izlenen yerler arasında bulunmak zorundadır:
  - `refreshUserSession` → `AUTH_REFRESH_TOKEN_REUSED` yalnız literal-parametre çözümüyle görülür;
  - `getStaffMe` → `STAFF_SESSION_ENDED` `staff`'ın `accounts` arayüzü üzerinden görülür;
  - listesi boş iki operasyon (`startRegistration`, `requestPasswordReset`) → `REQUEST_INVALID`, `invalidField` yardımcısı üzerinden görülür.
- `TestErrorTraceOnADemoModule` bir demo modülde şunları kanıtlar: neyi izlediğini (alan zinciri, arayüz uygulaması, paket değişkeni, yerel değişkenin metodu, generic çağrı, closure), iki aynı adlı metodu ayırdığını, kuramayan pakete girmediğini, uygulamasız arayüzü, fonksiyon tipli alanı ve okunamayan yeri bildirdiğini. `TestBindingFindings` ve `TestExemptionFindings` karşılaştırmanın ve muafiyet denetiminin kırmızı ve yeşilini, `TestKindStatusesCoverEveryKind` ise `apperr`'a eklenen bir `Kind`'ın tabloya girmeden geçemediğini kanıtlar.
- **Faz 1'in handler'larında gerçek bir bulgu:** `refreshStaffSession`'ın `x-error-codes`'unda `STAFF_LOGIN_FAILED` yazıyordu. Handler bu kodu hiç döndürmüyor; döndürdüğü üç refresh token kodu (`AUTH_REFRESH_TOKEN_INVALID`, `_EXPIRED`, `_REUSED`) ise listede yoktu. Durumların üçü de 401 olduğu için ADR-0029'un durum kümesi testi yeşil kalmıştı. Liste bu PR'da düzeltildi; yanıt kümesi değişmediği için bu uyumlu bir düzeltmedir.

Statik izleme yapısı gereği aşırıdır: handler'dan ulaşılan her `apperr.New`'i sayar, o dalın gerçekte koşup koşmadığına bakmaz.

### 2. Adres alan iki istek yalnız iş kuyruğuna yazar

`startRegistration` ve `requestPasswordReset` adresin kayıtlı olup olmadığını **istek içinde sormaz.** İstek biçimi denetler (adres, isimler) ve tek bir River işini bir transaction'da kuyruğa yazar; kararı iş verir:

- **Kayıt:** adres kayıtlıysa hiçbir şey yazılmaz ve posta gitmez (USR-001). Değilse adresin açık kaydı iptal edilir, yenisi yazılır ve bağlantı postasının işi **aynı transaction'da** kuyruğa girer (USR-002).
- **Sıfırlama:** adresin **etkin** bir hesabı yoksa hiçbir şey olmaz. Askıya alınmış ya da silinmiş hesap, kayıtsız adresle aynı muameleyi görür; istek yolu üçünde de aynı işi yaptığı için numaralandırma eşitliği korunur. Etkin hesapta kullanıcının açık token'ı iptal edilir, yeni istek satırı yazılır ve postasının işi aynı transaction'da kuyruğa girer (USR-017). Satır `users`'ın çocuğudur; transaction önce `softdelete.LockActiveParent` ile kullanıcıyı kilitler.
- **Parolasız (yalnız OTP) hesap** bugün yok, çünkü kayıt her zaman parola yazar. Olursa Java'nın davranışı izlenir: sıfırlama postası gider ve sıfırlama bir parola belirler (`AuthManager#requestPasswordReset` parolaya bakmıyor).

ADR-0029 "aynı gövde, aynı maliyet" diyor. Senkron yolda bu, kayıtlı dalda yazılmayan satırın ve atılmayan posta işinin maliyetini taklit etmek demekti (commit'in WAL yazımı dahil). Taklit her zaman bir ölçüde ayrışır. Asenkron yolda iki dal **aynı işi yapar**, taklide gerek kalmaz. `202` yanıtı sözleşmede zaten "istek kuyruğa girdi" diyor.

Bedeli: iş, adresi ve isimleri taşır. `river_job` bu yüzden katalogda `pii: true`'dur. River biten işi saklama süresinden sonra siler (tamamlanan ve iptal edilen 24 saat, atılan 7 gün). Postanın işi ise yalnız satırın kimliğini taşır, ne adresi ne token'ı.

### 3. Postalar ve token'ları

- **Token'lar posta gönderilirken üretilir.** Bağlantı ve sıfırlama token'ını postayı gönderen iş üretir: 32 rastgele bayt üretir, SHA-256 özetini satıra tek bir koşullu `UPDATE` ile yazar (satır hâlâ açıksa), bu deyim commit olur, sonra posta gönderilir. Düz token yalnız o çağrıda ve postada yaşar: veritabanında, iş kuyruğunda ve log'da yoktur. USR-003'ün testi bunu iki yerde sınar: satırın metin hâlinde ve River'ın sakladığı bitmiş işlerin argümanlarında token aranır.
- **Ömür, üretimden sayılır:** bağlantı 1 saat, kayıt token'ı 15 dakika, sıfırlama 15 dakika (ADR-0029 Karar 2 ve 4).
- **Gönderim başarısız olursa** River işi yeniden dener (`auth_mail`, en çok 10 deneme). Her deneme yeni token üretir; geç giden posta da çalışan bir bağlantı taşır. Ender bir yan etki: aktarıcı postayı kabul edip bağlantı kapanırken hata verirse ikinci bir posta gider ve birincinin bağlantısı geçersiz olur. **Teslim edilemez alıcı** (adres biçimi geçersiz ya da aktarıcı `RCPT TO`'yu 5xx ile kalıcı olarak reddetti) işi yeniden denemez, iptal eder (`mailer.ErrUndeliverable` → `river.JobCancel`).
- **Karşılaştırma sabit zamanlıdır**, çünkü Go'da hiç karşılaştırma yapılmaz: arama özetle yapılır (ADR-0025'in refresh token kalıbı).
- Kayıt token'ı (adım 2) istekte üretilir ve yanıtta döner; yeni bir doğrulama öncekinin yerine yazar. Bağlantı tüketilmez (kullanıcı iki kez basabilir).
- **Kayıt postası ad taşımaz.** Adres henüz doğrulanmamıştır; postada istekte yazılan ad olsaydı, başkasının adresine istediği metni gönderen bir saldırgan postanın içeriğini de seçerdi. Selamlama geneldir ve şablon yalnız bağlantıyı okur; bunu `TestRegistrationMailNamesNobody` bağlar. Sıfırlama, "parolan değişti" ve hoş geldin postalarındaki ad doğrulanmış hesaptan gelir.
- **Hoş geldin postası** Java'nın şablonunun aynasıdır (`welcome-email-template.html`: ad, kayıt bilgileri, özellikler, ipucu, destek adresi). Kayıt tamamlanırken kullanıcıyla aynı transaction'da kuyruğa girer.

### 4. Posta altyapısı

- **`internal/platform/mailer`:** `Message` ve modül şablonları. Bir mesajın konusu ve düz metni `text/template` ile `*.txt`'ten, HTML'i `html/template` ile `*.html`'den gelir; `html/template` eklediği her değeri kaçırır. Eksik anahtar hatadır, boş dize değil. Modül açılışta şablonlarının tam olduğunu denetler; eksik şablonla servis açılmaz.
- **`internal/integrations/smtp`:** `github.com/wneessen/go-mail` (plan §3.3, MIT), sürüm **v0.8.2**.
  - v0.8.2 GitHub'da 2026-11-08T14:08:26Z'de yayımlandı (modül vekilinin kaydı 13:31:06Z). On dört günü bu PR'ın düzeltme turunda doldu. Ayrıca GHSA-f82v-42xw-72cj ve GHSA-w8w6-94hg-2qpm'i kapatan ve bağlantı sızıntılarını düzelten bir güvenlik yamasıdır, yani CLAUDE.md'nin istisnası kapsamındadır. İlk tur v0.8.1'le açılmıştı.
  - Her gönderim bağlanır, tek mesaj gönderir ve kapanır.
  - `local` ve `test` dışında şifreleme zorunludur: 465'te örtük TLS, diğer portlarda STARTTLS. STARTTLS sunmayan aktarıcıya gönderim düşer, düz metne inmez (RFC 8314). Bu bağ uygulama yapılandırmasından okunur ve `TestSMTPConfigRequiresTLSWhereLive` onu sınar.
  - go-mail'in gönderim hatası metninde alıcıları taşır, River da işin hatasını saklar. Hata bu yüzden yalnız aktarıcının kararıyla sarılır.
- **Yapılandırma:** `KOALA_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `KOALA_MAIL_FROM`, `KOALA_WEB_BASE_URL` ve `KOALA_REGISTRATION_MAILS_PER_HOUR`. `config.ServeMailErrors` `ServeStorageErrors` kalıbındadır. Staging ve production'da eksikse `serve` açılmaz, çünkü yoksa kayıt ve sıfırlama `202` döner, posta gitmez ve bunu hiçbir şey bildirmez. Yarım yapılandırma her ortamda hatadır. `local` ve `test`'te aktarıcı yoksa posta düşürülür ve yalnız konusu log'a yazılır.
- **Test:** gerçek SMTP yoktur. `fakes.SMTP` geri döngü adresinde düz SMTP konuşan bir aktarıcıdır. STARTTLS sunmadığı için zorunlu TLS'nin sessizce atlanmadığını kanıtlar; bir alıcıyı kalıcı olarak reddederek teslim edilemezliği de sınar.
- **Bağlantılar** Java'nın web yollarını kullanır (`<origin>/hesap-dogrula?token=…`, `<origin>/sifre-sifirla?token=…`).

### 5. Senaryo bağının operasyon düzeyi istisnası (`boundAhead`)

ADR-0029 Karar 1'in katalog bağı alan düzeyidir: bir etikette işaretli operasyon kaldıkça alanın her P0'ı `deferred` olmak zorundadır. `users` etiketinin 25 operasyonundan bu PR 6'sını getiriyor. Kural olduğu gibi kalsa bu PR'ın 14 P0'ı bağlanamazdı.

Karar: `test/scenarios/contract_test.go`'daki `boundAhead` haritası, alanın son operasyonu gelmeden bağlanan P0'ı **test kodunda** operasyonlarına eşler. Bir kayıt şu hâllerde kırmızıdır (`aheadFindings`, kendi testi `TestBoundAheadFindings`):

- senaryo hâlâ `deferred`'dır;
- adlandırdığı operasyon yoktur ya da hâlâ işaretlidir;
- operasyonun etiketlerinden hiçbiri senaryonun alanına (`frozenDomains()[alan]`) ait değildir. Örneğin `"USR-022": {"loginUser"}` kırmızıdır: senaryo başka bir alanın gelmiş handler'ına binemez;
- alan artık donmuş değildir, ya da kayıt hiçbir donmuş alan P0'ına denk gelmez.

Harita katalogda değil test kodundadır, çünkü ADR-0029'un gerekçesi (senaryo API'den bağımsız dilde yazılır) hâlâ geçerlidir. Haritada olmayan her P0 için alan düzeyi kural aynen işler.

### 6. Posta bütçesi

E-posta bombası üç katmanla sınırlanır:

1. **HTTP kovası** (ADR-0021, ADR-0025 §8): kayıt başlatma ve sıfırlama isteği, **posta kutusu anahtarının** HMAC hedefinin iki kovasından jeton alır. (Hedef, adres) kovası `auth` grubunda, hedef geneli kova `auth_account` grubundadır. Girişten farkı, bilinen kaynak kaçışının olmamasıdır. Token sunan adımlar (kayıt doğrulama ve tamamlama, sıfırlama tamamlama) token özetinin kovasından, uygulama içi değişim kullanıcınınkinden jeton alır. Üretim kovalarıyla `TestAccountRateLimitKeys` sınar.
2. **İşteki bütçe:** bir posta kutusu (kayıt) ya da bir hesap (sıfırlama) son postasından sonraki **60 saniye** içinde ve bir saat içinde **5**'ten fazla bağlantı postası almaz. Bu, ADR-0029 Karar 3'ün OTP yeniden gönderme kuralının bağlantılara uygulanmasıdır. Bütçeyi aşan istek diğerleriyle aynı `202`'yi alır ve posta sessizce düşer.
3. **Küresel tavan:** bütün adresler için saatte en çok `KOALA_REGISTRATION_MAILS_PER_HOUR` (öntanım **500**) kayıt postası gider. Çok sayıda farklı adrese yayılmış bir sel aktarıcının itibarını yakmasın diye vardır. Tutulan posta `koala_account_mails_held_total{reason}` metriğinde sayılır (`mailbox`, `account`, `global`). `KoalaRegistrationMailCapReached` uyarısı tavan doldu diye çalar.

**Posta kutusu anahtarı:** saklanan adres değişmez, yalnız sınır anahtarı türetilir. `+`'dan sonrası atılır, `googlemail.com` `gmail.com` sayılır ve Gmail'de yerel kısmın noktaları silinir. Böylece `kurban+1@…`, `kurban+2@…` ve `k.urban@gmail.com` tek kutudur.

**Kabul edilen risk:** bütçe hedef başınadır. Bir saldırgan kurbanın adresine bir saatte beş istek atarak o saat boyunca **kurbanın kendi kayıt ya da sıfırlama postasını engelleyebilir**. Hedef başına sınırın doğası budur; posta bombasına karşı korumanın bedeli olarak kabul edildi. Kurban bir saat sonra yeniden ister.

### 7. Oturumlar, kimlik bilgisi dönemi ve bildirim

- Sıfırlama kullanıcının **bütün** oturumlarını, değişim çağıran dışındakileri kapatır (ADR-0029 Karar 4). Kapanan oturumların realtime bağlantıları commit'ten sonra kapanır.
- **Kimlik bilgisi dönemi** (`users.password_changed_at`): sıfırlama ve değişim onu ileri alır. Yalnız bu dönemden **sonra açılmış** bir oturum cihazını bilinen iyi kaynak olarak tanıtır (ADR-0025 §5'in genişletilmesi; personeldeki `success_token_version` kalıbının karşılığı). Yalnız kapanma sebebine bakmak yetmezdi. Saldırgan aynı cihazdan iki kez girerse ilk oturumu `REPLACED` ile kapanır ve sıfırlamadan sonra da sayılırdı. `PASSWORD_RESET` ve `PASSWORD_CHANGED` sebepleri de hariç tutulmaya devam eder. İkisinin testi ayrıdır:
  - `TestResetForgetsTheDevicesBeforeIt`: saldırganın cihazı bilinmez, kurbanın cihazları bilinir;
  - `TestPasswordRevokeReasonsDoNotVouch`: iki sebep de kefil olmaz.
- **Sıfırlamayı tamamlayan cihaz** posta kutusunun sahibi olduğunu kanıtlamıştır. O cihaz bilinen kaynak sayılır (`devices.credentials_proven_at`) ve hedef için bir `SUCCEEDED` deneme satırı yazılır. Kilit altındaki kullanıcı sıfırladıktan sonra hemen girer, hem o cihazdan hem başkasından (`TestResetLiftsTheLock`). **Sıfırlama oturum açmaz** (USR-019 kalıyor).
- Sıfırlama ve değişim, kullanıcının açık sıfırlama token'ını iptal eder (USR-017). Silinmiş ya da askıya alınmış hesabın sıfırlaması geçersiz token gibi reddedilir.
- **Uygulama içi değişimde yanlış mevcut parola** girişin deneme kaydına yazılır: hesabın giriş hedefinde `FAILED` sayılır ve aynı yüz denemelik kilide katkıda bulunur. Hedef kilitliyken değişim incelenmeden reddedilir (OWASP ASVS 4.0.3 V2.2.1). Ayrı bir sayaç yerine giriş kilidi seçildi, çünkü tahmin edilen şey aynı parola ve iki yüzey arasında bölünmüş iki bütçe saldırgana iki katını verirdi. Girişteki bilinen cihaz istisnası burada uygulanmaz, çünkü oturumu tutan cihaz çağıranın kendisidir. Başarılı değişim bir `SUCCEEDED` yazar (`TestPasswordChangeCountsFailures`).
- İkisi de "parolan değişti" bildirimini aynı transaction'da kuyruğa yazar: bildirim yalnız commit olan değişiklik için gider (AUTH-006). Java bildirimi yalnız sıfırlamada gönderiyor; Go uygulama içi değişimde de gönderir (ASVS V2.2.3). **Java'dan bilinçli ayrılık.**
- Değişimde denetimlerin sırası: onay eşleşmesi → parola kuralı → yeni parola verilen mevcut parolayla aynı mı → kilit → mevcut parolanın doğrulanması.

### 8. Tablolar, rızalar ve saklama

| Tablo | Sınıf | Gerekçe |
|---|---|---|
| `pending_registrations` | M2, `pii` | Adres, isim, posta kutusu anahtarı, iki token'ın özeti. **Kayıt tüketilince ya da iptal edilince adres ve isimler hemen silinir** (KVKK veri en aza indirme). 30 gün yalnız bütçenin saydığı alanlarla (anahtar, zaman) kalır, sonra hard delete (USR-007). Adres başına tek açık satır, kısmi tekil indeksle. |
| `password_reset_tokens` | M2 | Token özeti; kullanıcı başına tek açık satır, kısmi tekil indeksle. 30 gün sonra hard delete. `users`'a giden kenar `detach`'tir. |
| `agreement_versions` | M4 | Belge başına tek güncel sürüm, Java'nın tohumu ve meta verisiyle: `locale`, `content_url` ve `content_hash`. Rızanın hangi metne verildiğinin kanıtı bunlardır. Java'nın tohumunda içerik özeti yoktur, alan boştur. `DISTANCE_SERVICE_CONTRACT` da gelir; Java onu kayıtta değil işlemle istediği için kayıtta zorunlu değildir. |
| `consent_records` | M1, `pii` | Kim, hangi sürüme, ne zaman, **hangi istemci adresi ve User-Agent'tan** (Yusuf'un kararı, Java gibi). Uygulama yalnız ekler, okumaz, güncellemez. |

- **İstemci adresi** kenarın güvenilen proxy'lerin arkasında çözdüğü adrestir (`httpx.ClientIPFrom`). Güvenilmeyen `X-Forwarded-For` okunmaz. User-Agent en çok 512 bayttır ve karakter sınırında kesilir.
- **Saklama kuralı:** IP ve User-Agent, rızanın sona ermesinden 3 yıl sonra anonimleştirilir. Sona erme geri çekilme, yeni sürümle değişme ya da hesabın silinmesidir. Rıza satırının kendisi M1 olarak kalır. Dayanak, Ticari İletişim Yönetmeliği'nin onay kayıtlarına koyduğu süredir; **hukuk teyidi bekliyor.** Sona erme bugün modellenmediği için kural katalogda ve burada yazılıdır; anonimleştirme işi, sona ermeyi modelleyen PR'la gelir.
- `users`'a kayıt alanları ve `password_changed_at`, `devices`'a `credentials_proven_at` eklendi. İki view `soft-delete-view` yönergesiyle yeniden kuruldu (db/README). Yeni iki `detach` kenarı JOIN testinde `Unread` bildirildi.

### 9. Rızalar ve tamamlama sırası

- Rızalar kayıt token'ı okunmadan önce denetlenir (USR-005). Güncel bir sürümü adlandırmayan ya da iki kez adlandıran karar `REQUEST_INVALID` alır (`/consents/<i>`, `FIELD_VALUE_INVALID`). Zorunlu bir belgenin güncel sürümü verilmemişse `CONSENT_REQUIRED` döner. İkisi de token hakkında bir şey söylemez ve onu tüketmez.
- Sıra: cihaz → rızalar → parola kuralı → token. Token önce tüketilmeden aranır, böylece çalışmayacak bir token argon2 maliyeti ödetmez. Ardından tek transaction'da şunlar yazılır: tüketme, kullanıcı, rıza kayıtları, cihaz, oturum, giriş denemesi satırı ve hoş geldin postasının işi.
- Adres adım 1 ile 3 arasında başka bir hesap almışsa `VERIFICATION_TARGET_IN_USE` (409) döner; transaction geri alınır, kayıt token'ı tüketilmez.

### 10. Denetim kaydı ve PII

Bu uçlar personel uçları değildir; ADR-0026'nın denetim kaydı onlara uygulanmaz. Adres ve isim log'a yazılmaz; posta bütçesi ve düşürülen posta yalnız sabit mesaj ve konu yazar. Yanıtlar PII taşımaz.

### 11. Kabul edilen riskler ve web istemcisine notlar

- **Başkası adına kayıt başlatma.** Kayıt başlatan kişi ad ve `employer` bayrağını seçer. Adresin sahibi bağlantıya basıp kaydı tamamlarsa hesap bu değerlerle doğar. Kayıt postası adı göstermediği için (Karar 3) içerik saldırgan kontrolünde değildir. Ad profil uçlarından düzeltilebilir. `employer` ise kayıttan sonra değişmez (ADR-0029); bu PR'da kod değişikliği yok, risk kabul edildi.
- **K4, web:** bağlantı token'ı sorgu dizgisinde taşınır (Java'nın web yolları). Web istemcisi bağlantı sayfalarında `Referrer-Policy: no-referrer` göndermeli ve token'ı okur okumaz `history.replaceState` ile URL'den silmelidir. Bu web'in işidir; API'de kod değişikliği yok.

## Yerine geçtiği

1. **ADR-0029 Karar 1'in katalog bağının "granülerlik alan düzeyidir" cümlesi** → Karar 5: alan düzeyi kural kalır, ama `boundAhead`'de kendi alanının operasyonlarıyla adlandırılan P0 alan donmuşken bağlanabilir.
2. **ADR-0029 Karar 1'in üçüncü sınırının "Gereklilik" paragrafı** → Karar 1 ile karşılandı.
3. **ADR-0029 Karar 4'ün "kayıtsız dalda üretilen token atılır" cümlesi** → Karar 2 ve 3: kayıtsız dalda token üretilmez, çünkü istek hiç token üretmez; eşitlik istek yolunun kendisinde kurulur. USR-018'in `rule`, `then` ve `target` metinleri ve sözleşmenin açıklaması buna göre güncellendi.
4. **ADR-0025 §5'in bilinen cihaz kuralı** → genişletildi: kimlik bilgisi döneminden önce açılmış oturum cihazına kefil olmaz; sıfırlamayı tamamlayan cihaz bilinen kaynaktır (Karar 7).
5. **ADR-0029 *Sonuçlar*'ındaki "`pending_registrations`'ın temizlik işi"** → bu PR'da (`Accounts.Purge`, `auth_purge` işi).
6. **Stil rehberi §7'nin "Uygulaması Faz 1'dedir" cümlesi** → gerçeğe çekildi: depo uygulanmadı (issue #22).

## Sonuçlar

- Bağlanan P0'lar: USR-001…007 ve USR-015…021; AUTH-006, AUTH-007 ve AUTH-008. AUTH-009'un `partial`'ı kapandı, çünkü kural artık her yüzeyde çağrılıyor. OTP ve doğrulama senaryoları (USR-008…014) ile profil senkronu (USR-022, 023) `deferred` kalıyor.
- Bir operasyonu uygulayan her PR artık `x-error-codes`'unu handler'la bağlamak zorunda. Liste yanlışsa, `Kind` düzyazıyla çelişiyorsa ya da izleme bir çağrıyı çözemiyorsa `verify` job'ı kırmızıdır.
- **Medya:** ADR-0030'un `Complete` durum kapısı ve `RejectionCorrupt` kod karşılığı medya uçlarının PR'ına devredildi; bu PR medyaya dokunmaz.
- Bu PR'da kalanlar:
  - **Idempotency deposu** ayrı bir platform PR'ının işidir (issue #22). `completeRegistration`'ın açıklaması bugünkü davranışı yazar: başlık doğrulanır, yanıtı kaybolan tekrar `REGISTRATION_TOKEN_INVALID` alır, yan etki iki kez olmaz, kullanıcı parolasıyla girer. Depo gelince parametrenin 422/409 vaadi karşılanır.
  - **İYS:** `COMMERCIAL_MESSAGE` rızası kaydedilir, İYS'ye iletilmez. Bu bir parite eksiği değildir: Java da kayıttaki rızayı iletmiyor. `CommercialConsentSource` yalnız tercih merkezini, abonelik çıkış bağlantısını ve İYS mutabakatını tanıyor. Kayıttaki ticari ileti rızasının İYS'ye gidip gitmeyeceği Faz 9'un (`privacy`) kararıdır.
  - **Güncel sözleşme sürümlerinin listesi:** istemcinin okuyacağı uç bu PR'da yok, referans verisi PR'ına kalıyor.
  - Rıza IP/UA'sının anonimleştirme işi, sona ermeyi modelleyen PR'la gelir (Karar 8).
  - Rıza kayıtları bugün `authn`'in sorgularıyla yazılıyor; `privacy` modülü geldiğinde oraya taşınır.

## Açık sorular

1. **Rıza IP/UA'sının 3 yıllık saklama süresi** Ticari İletişim Yönetmeliği'ne dayanıyor; **hukuk teyidi bekliyor.**
2. **Küresel kayıt postası tavanının öntanımı** (saatte 500) ölçülmedi; trafik görüldüğünde gözden geçirilir.
