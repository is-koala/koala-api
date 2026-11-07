# ADR-0029: Faz 2 sözleşmesi — kayıt, kimlik, kullanıcı, şirket ve medya

- Durum: Kabul edildi
- Tarih: 08.11.2026
- Karar kaydı: K10, K11, K14, K15 (hazırlık), K17, K19, S4, D5
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §4.1 (sözleşme ilkeleri), §4.2 (istemci işi), §5.4–5.5 (authz), §6.1 Faz 2, §6.2 (Java testlerini senaryoya çevirme); [ADR-0005](0005-api-contract-principles.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) §4 (dosya yüklemeleri), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md) §11, [ADR-0024](0024-authz-core-evaluator-and-conformance.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0028](0028-recursion-rules-body-allowlist-and-gate-completeness.md); Java `origin/main` (0cbbce69, v0.9.32) — 19 manager ve IT'leri okundu, koşturulmadı
- Dış kaynaklar: NIST SP 800-63B §5.1.3.3 ve §5.1.4.2 (bir kerelik kodlar, saklama ve deneme sınırı); OWASP ASVS 4.0 §2.1, §3.3, §4.2; RFC 9457; Google Maps Platform [Hizmet Şartları](https://cloud.google.com/maps-platform/terms) §3.2.3(b) (önbellekleme); Stripe Connect ve Upwork (işletme doğrulanmadan işlem yok); GitHub organizasyon daveti (süreli, iptal edilebilir); S3 + sunucu tarafı dönüştürme kalıbı
- Kod: `api/openapi.yaml`, `test/scenarios/{user,identity,company,media}.yaml`, `docs/faz0/authz-envanteri.csv`, `internal/authz/model.fga`, `internal/platform/softdelete/softdeletetest/readtest.go`

## Bağlam

Faz 2 plan §6.1'de tek satır: "3 adımlı kayıt, OTP, parola sıfırlama, profil, kimlik belgesi yükleme (NFC'siz), oturum listesi. Şirket, kişiler, davetler, takip. Referans ve adres verisi (Google). Yükleme, EXIF ve S3. Bu alanların admin uçları." Çıkış kriterinin ilk maddesi **"Alanın spec'i donduruldu"**; ikinci maddesi istemci işinin (plan §4.2) paralel başlamasını buna bağlıyor.

Sözleşmeyi dondurmanın önünde bu depoda iki yapısal engel var, ikisi de kapıların fail-closed olmasından geliyor:

1. **Üretilmiş sunucu arayüzü büyüyor.** `api/openapi.yaml`'a operasyon ya da şema eklemek `internal/apiv1/server.gen.go`'yu büyütür ve `make gen-check` onu üreticiyle birebir tutar. Handler'ı olmayan bir operasyon `StrictServerInterface`'i karşılamaz: derleme kırılır. `staff.Gate` ayrıca sözleşmedeki her `x-authz-exempt`'i `staff.SelfService()` listesinde arar ve izni olmayan admin operasyonunda **sunucuyu açmaz**.
2. **Üretilmiş okuma testi veri katmanını ister.** `softdeletetest.ReadTest` her GET'in `x-resource`'unu canlı veritabanının soft-delete kataloğunda arar; tablo yoksa test kırmızıdır. Faz 2'nin tabloları (şirket, davet, adres, medya, kimlik incelemesi…) henüz yok. Yani "sözleşmeyi veri katmanından önce dondur" bugünkü kapılarla hiç mümkün değil.

Üçüncü bir gerilim de var: alan 70 operasyon büyüklüğünde, ama uygulaması birkaç PR sürecek. "Spec'i parça parça ekle" çıkış kriterini karşılamaz, çünkü istemci işi alanın tamamını görmeden başlayamaz.

Bu ADR sözleşmeyi bir kerede dondurur ve eksikleri **görünür** kılar: uygulanmamış operasyon sözleşmede durur, `501` döner, ve üç ayrı test sözleşme–kod–katalog üçlüsünü birlikte tutar.

Java tarafındaki davranış, kararların dayanağı olarak okundu. Okuma 19 manager'ı ve IT'lerini kapsıyor; bulduğumuz kusurlar aşağıda "Java'dan bilinçli ayrılık" olarak işaretli. Java'nın bugünkü davranışı senaryo kataloğunun `then` alanında, Go'nun hedefi `target` alanında durur (plan §6.2-1).

## Karar

### 1. Dondurma mekanizması: `x-not-implemented` + `501`

Faz 2'nin bütün uçları bugün `api/openapi.yaml`'a girer. Handler'ı olmayan operasyon şunları taşır:

```yaml
x-not-implemented: |
  Faz 2'nin sözleşmesi donduruldu; handler'ı sonraki PR'da gelir (ADR-0029 §1).
  Bugün 501 NOT_IMPLEMENTED döner.
```

- Operasyon **`501`'i ayrı bir yanıt olarak yazmaz**; `501` operasyonun `default` yanıtının (`InternalError`) altına düşer. Gerekçe: `501`'i yazıp handler gelince silmek operasyonun durum kümesini değiştirir, stil rehberi §2 bunu uyumsuz değişiklik sayıyor. Sözleşme donduğu gün ne diyorsa uygulama geldiğinde de aynısını diyor; değişen tek şey hangi dalın koştuğu.
- `ErrorCode` listesine `NOT_IMPLEMENTED` eklenir, `apperr`'a `KindNotImplemented` ve `httpx.StatusOf`'a `501` eşlemesi.
- İstemci için anlamı açıktır: kod listesi spec'ten üretiliyor (stil rehberi §8), `x-not-implemented` uzantısı spec'te görünüyor, yani plan §4.2'nin tip üretimi bugün koşabilir ve hangi ucun henüz canlı olmadığı makinece okunabilir.

**İki yönlü bağ (`internal/app.TestNotImplementedOperationsAreDeclared`).** Sözleşmede `x-not-implemented` taşıyan operasyonların kümesi, `notImplemented` iskelesinin sunduğu operasyonların kümesine **eşit** olmak zorundadır. Unutulan handler (işaret kalkmış, iskele duruyor) ve unutulan işaret (handler yazılmış, işaret kalmış) ayrı ayrı kırmızıdır. İskele `strictServer`'a gömülür ve her metodu `apperr.New(apperr.KindNotImplemented, "NOT_IMPLEMENTED")` döner; oapi-codegen'in ürettiği `Unimplemented` yalnız strict olmayan arayüz içindir, strict tarafta karşılığı yok.

**Okuma testinin gevşemesi.** `softdeletetest.ReadTest.plan` işaretli operasyonu kataloğa bakmadan **atlar**. Böylece donmuş bir GET okuyacağı tabloyu dürüstçe adlandırabilir (`x-resource: companies`), tablo henüz yokken. İşaret kalkınca okuma testi kendiliğinden silahlanır: o gün tablo, sınıflandırma ve fikstür zorunlu olur. Bu, Karar 1'in sözleşmeyi veri katmanından önce dondurabilmesinin tek nedenidir ve `x-resource`'un zorunluluğunu (ADR-0026, ADR-0020 §11) kaldırmaz, yalnız **ne zaman** denetlendiğini erteler.

**Katalog bağı (`test/scenarios.TestFrozenDomainsDeferTheirScenarios`).** Sözleşmenin `tags`'i ile kataloğun `domain`'i eşlenir. Bir etikette hâlâ işaretli operasyon varsa o alanın her P0 senaryosu `deferred: {phase: 2, reason: …}` taşımak zorundadır; etiketin hiçbir operasyonu işaretli değilse o alanın hiçbir P0'ı `deferred` olamaz. Granülerlik **alan düzeyidir**, senaryo düzeyinde değil: senaryolar API'den bağımsız dilde yazılır (`test/scenarios/README.md`) ve operasyon adı taşımaz, bu yüzden senaryo-operasyon eşlemesi kataloğa API sızdırır. Alan düzeyi bağ, "son operasyonu uygulayan PR o alanın P0'larını bağlamak zorundadır" kuralını makineye verir.

### 2. Üç adımlı kayıt

| Adım | Ne yapar | Ne kalıcılaşır |
|---|---|---|
| 1. başlat | Ad, soyad, e-posta, işveren mi | `pending_registrations` satırı + e-postaya doğrulama bağlantısı |
| 2. doğrula | Bağlantı token'ını kayıt token'ına çevirir | — (satır tüketilmez) |
| 3. tamamla | Rızalar + parola | `users` satırı, rıza kayıtları, oturum; pending satırı tüketilir |

- **Adım 1 sayılamaz.** Adres kayıtlı olsun olmasın yanıt aynı gövdedir ve aynı maliyeti öder; kayıtlı adrese posta gitmez. Java bunu yapıyor (`AuthManager.java#startRegistration`, `RegistrationEnumerationTest`), korunuyor. AUTH-001'in giriş için kurduğu kalıbın aynısı.
- **Adım 1 aynı zamanda yeniden göndermedir.** Aynı adres için açık bir pending kayıt varsa iptal edilir ve yenisi yazılır; adres bir kayıt yarı kalınca kilitlenmez. Adres başına **tek aktif** pending kayıt, kısmi tekil indeksle (`deleted_at IS NULL` yerine `consumed_at IS NULL AND revoked_at IS NULL`). Oran sınırı `auth` grubu, hedef (adresin özeti) başına.
- **İki token, ikisi de opak ve özetlenmiş.** Bağlantı token'ı 32 rastgele bayt (base64url, 43 karakter), SHA-256 özeti saklanır, **ömrü 1 saat**. Adım 2 onu tüketmez (kullanıcı bağlantıya iki kez basabilir) ama adım 2'nin verdiği **kayıt token'ı** tek kullanımlıktır, pending kayda bağlıdır ve **ömrü 15 dakikadır**.
  - Java adım 2'de HS256 bir JWT veriyor (`JwtService#generatePasswordSetToken`, 10 dk). **Java'dan bilinçli ayrılık:** ADR-0025 bu sistemde kısa ömürlü yetki taşıyıcısını opak ve sunucuda özetli tutuyor (refresh token, bilet, parola sıfırlama). Dördüncü bir biçim olarak JWT tutmak iptal edilemez bir taşıyıcı ekler; opak token iptal edilebilir ve aynı tablodan okunur. 15 dakika parola sıfırlamanın penceresiyle aynı, böylece bir büyü sayısı eksilir.
  - İki token olmasının nedeni: parola, e-postada bir saat yaşayan token'la **birlikte** seyahat etmesin. Adım 2 kısa ömürlü bir token'a çevirir, parola yalnız onunla gelir.
- **Parola yalnız adım 3'te.** Rızalar token okunmadan önce denetlenir (Java'nın sırası, korunuyor: zorunlu rıza yoksa token hiç bakılmaz).
- **Yarı kalmış kaydın akıbeti.** `pending_registrations` **M2** (kısa ömürlü): token süresi dolmuş satır tembel değil, **temizlik işiyle** gider — 30 gün sonra hard delete. Java'da hiç temizlik yok (`PendingRegistration`'a hiçbir zamanlayıcı dokunmuyor), satırlar süresiz birikiyor; **Java'dan bilinçli ayrılık**. Süresi dolmuş token'la gelen istek `REGISTRATION_TOKEN_INVALID` alır ve satır o anda iptal edilir.
- Adım 3 başarıyla biterse kullanıcı **giriş yapmış** olur: oturum açılır ve token çifti döner (Java'nın davranışı). E-posta adım 1'de doğrulanmış sayılır, `emailVerified = true`.

### 3. OTP politikası

OTP bu alanda **yalnız** kimliği doğrulanmış kullanıcının e-posta ya da telefon değiştirmesinde kullanılır. Kayıtta OTP yoktur (bağlantı token'ı vardır, Karar 2).

| Kural | Değer | Java | Gerekçe |
|---|---|---|---|
| Uzunluk ve alfabe | 6 ondalık basamak, `000000`–`999999` **dahil** | `100000 + nextInt(900000)` | Java baştaki sıfırı dışlıyor: uzay 1.000.000 değil 900.000. Tam uzay kullanılır. |
| Üretim | CSPRNG, düzgün dağılım (modulo yanlılığı yok) | `SecureRandom` | — |
| Saklama | SHA-256 özeti; doğrulamadan sonra özet silinir | **düz metin** | NIST SP 800-63B §5.1.4.2: bant dışı sır özetlenerek saklanır. **Java'dan bilinçli ayrılık.** |
| Ömür | **10 dakika** | 15 dakika | NIST SP 800-63B §5.1.3.3: bant dışı sır 10 dakikayı aşmamalı. |
| Deneme sınırı | **5**; dolunca doğrulama yakılır (`VERIFICATION_ATTEMPTS_EXHAUSTED`), yenisi başlatılır | **yok** | NIST §5.2.2 kısıtlama zorunlu. Java'da sayaç kolonu bile yok: 15 dakika boyunca sınırsız tahmin. **Java'dan bilinçli ayrılık.** |
| Yeniden gönderme | (kullanıcı, kanal, hedef) başına **tek aktif** doğrulama; yenisi öncekini iptal eder. Bekleme 60 sn, hedef başına saatte en çok 5 gönderim | e-postada hedefe göre tekilleme var; **telefonda hiç yok** (her PATCH yeni satır, hepsi geçerli) | Telefon tarafı Java'da aynı anda onlarca geçerli kod bırakıyor. **Java'dan bilinçli ayrılık.** |
| Oran sınırı | `auth` grubu, adres **ve** hedef özeti başına (ADR-0025 §8) | genel `WRITE` kovası, 60/dk, yalnız IP | Hedef başına kova olmadan tek adrese yoğunlaşan tahmin serbest. |
| Doğrulama tüketir mi | Evet: `verified` işaretlenir, özet silinir. İkinci onay `VERIFICATION_ALREADY_CONFIRMED` (409) | Zaten doğrulanmışsa **kodu hiç bakmadan 200** döner | Java'nın erken dönüşü yanlış kodu da kabul ediyor. **Java'dan bilinçli ayrılık.** |
| Kanal | E-posta ve SMS | aynı | — |

**Denetim sırası** (Java'nın sırasından iki sapma): kayıt var mı → **sahiplik** → süre → deneme → kod.

- Sahiplik ihlali **gizlenmiş 404**'tür, Java'nın 403'ü değil: `ForbiddenException` başkasının doğrulama kimliğinin var olduğunu söylüyor (`UserVerificationManager.java#verify`). ADR-0014 "okuma reddi kaynağın 404'üdür" kuralı burada da geçerli. **Java'dan bilinçli ayrılık.**
- Süre kontrolü kodun **önüne** geçer. Java kodu önce karşılaştırıyor, bu yüzden süresi dolmuş bir doğrulamada yanlış kod `INVALID_TOKEN`, doğru kod `TOKEN_EXPIRED` alıyor: kod doğruluğu sızıyor. **Java'dan bilinçli ayrılık.**

Yeni e-posta ya da telefon **onaylanana kadar yazılmaz** (Java'nın davranışı, korunuyor): profil yaması doğrulama başlatır, eski değer ve `…Verified` bayrağı yerinde kalır.

### 4. Parola sıfırlama

Java'nın biçimi doğru ve korunuyor: 32 baytlık opak token, SHA-256 özeti (`token_digest`, tekil), kullanıcı başına tek aktif token (kısmi tekil indeks + mintlemeden önce iptal), **tek kullanımlık** ve tüketimi parola yazımıyla aynı transaction'da tek `UPDATE … WHERE consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now()` ile atomik.

- **Ömür 15 dakika.**
- Bilinmeyen, süresi dolmuş, iptal edilmiş ve zaten kullanılmış token'ın **dördü de** aynı `PASSWORD_RESET_TOKEN_INVALID` gövdesini alır.
- Kayıtsız adres ayırt edilemez: aynı 200 gövdesi, posta yok. **Java'dan ayrılık:** Java'nın bilinen dalı token üretip posta kuyruğa atıyor, bilinmeyen dalı atmıyor; zamanlama farkı `docs/research/password-reset-token-design.md`'de açık bir boşluk olarak duruyor. Go iki dalda da aynı işi yapar (bilinmeyen dalda üretilen token atılır), AUTH-001'in giriş için kurduğu kalıp.
- **Başarılı sıfırlama çağıran dahil bütün oturumları kapatır** ve token çifti **döndürmez**; kullanıcı yeniden giriş yapar (OWASP ASVS §3.3.1; Java da böyle). Uygulama içi parola değişimi ise çağıranın oturumunu açık bırakır ve diğerlerini kapatır (Java'nın davranışı, korunuyor) — ikisi farklı, çünkü sıfırlamada hesabın ele geçtiği varsayılır.
- Sıfırlama ve uygulama içi değişim, kullanıcının **açık parola sıfırlama token'larını** iptal eder.
- **Parola kuralı ADR-0025'in kuralıdır:** en az 15, en çok 128 karakter, `PASSWORD_POLICY_VIOLATION`. Java'nın 12 kod noktası / 72 bayt sınırı bcrypt'ten geliyordu; Go bcrypt kullanmadığı için 72 bayt sınırı yok. Karakter sınıfı kuralı yok (NIST SP 800-63B §5.1.1.2 kompozisyon kurallarını önermiyor).

### 5. Kimlik belgesi yükleme: kabul edilen tipler ve EXIF

- **Dört sabit yuva**, hepsi zorunlu: `selfie`, `front`, `back`, `together`. İnceleme başına tek gönderim. Açık (`PENDING`) inceleme varken ikinci gönderim reddedilir (`IDENTITY_REVIEW_PENDING`); reddedilmiş incelemeden sonra **yeni satır** açılır (Java'nın davranışı).
- **Kabul edilen tipler yalnız `image/jpeg` ve `image/png`.** Tip **içerikten** belirlenir (sihirli baytlar), istemcinin `Content-Type` başlığından ya da dosya adının uzantısından değil. **Java'dan bilinçli ayrılık:** Java izin listesini **uzantıdan** çıkarıyor, S3'e ve `media.file_type`'a istemcinin ham başlığını yazıyor; `.jpg` adlı bir metin dosyası yalnız ImageIO'nun çözememesiyle, dolaylı olarak düşüyor. Nesnenin `Content-Type`'ı sunucunun saptadığı tiptir.
- Dosya başına **10 MiB**, tuval **50 MP** (Java'nın değerleri).
- **EXIF politikası: tümden strip, yeniden kodlayarak.** Yalnız piksel rasteri taşınır; her APP1/EXIF/XMP/IPTC bölütü ve PNG metin parçası düşer — **GPS enlem/boylam dahil**. Tek istisna yön etiketidir (`0x0112`): önce okunur, piksellere pişirilir, sonra o da gider. Fotoğraf dik saklanır.
  - Gerekçe ve [[job-location-rule]] ile ilişkisi: ilan konum kuralı (04.11.2026) "şirkette tam, bireyselde yaklaşık, haritada hep yaklaşık" diyor. Kimlik fotoğrafı için konumu **saklamanın hiçbir iş gerekçesi yok**, dolayısıyla kural en katı hâlini alır: konum verisi hiç tutulmaz. Java da strip ediyor (`IdentityVerificationManager.java#withoutMetadata`) ve bir test GPS işaretçisinin kaybolduğunu kanıtlıyor; karar korunuyor ve bütün medya tiplerine genelleştirilir.
- **Reddetme gerekçesi zorunlu**, serbest metin, 1–1000 karakter; enum yok (Java'da da yok). Biçim sözleşmede doğrulanır, böylece handler hiç `null` görmez (Java'da `reason.trim()` null'da patlıyor ve bir test manager'da koruma yazılmasını yasaklıyor).
- **Kullanıcı hem onayda hem redde bilgilendirilir.** Java yalnız onayda bildiriyor; reddi kullanıcı ancak kendi incelemesini okuyarak öğreniyor. **Java'dan bilinçli ayrılık.**
- Onay tek yönlüdür: `PENDING → APPROVED | REJECTED`, ikisi de terminal; karar verilmiş incelemeye ikinci karar `IDENTITY_REVIEW_ALREADY_DECIDED` (409). Onay dört yuvanın **hepsinin** gerçek kanıt olmasını ister; **ret istemez**, böylece eksik eski kayıt da kapatılabilir (Java'nın davranışı). Karar satır kilidi (`FOR UPDATE`) altında verilir; iki yarışan karar fail-closed düşer.
- Onay `users`'a kimlik alanlarını yazar ve ad/soyadı **Türkçe yerelle** baş harfi büyük kalanı küçük yapar (`Locale("tr","TR")`; noktalı/noktasız i yük taşır). Onaydan sonra ad, soyad, doğum tarihi ve TCKN değişmez.
  - **Java'dan bilinçli ayrılık:** Java'da cinsiyetin "kimlik doğrulanmışsa değişmez" koruması **yorum satırına alınmış**, mesaj anahtarı duruyor ama hiç atılmıyor. Go kimlikten gelen alanı dondurur: cinsiyet de kimlik belgesinden geliyorsa dondurulur, gelmiyorsa serbest kalır. Bugünkü belge akışı cinsiyeti okumadığı için **serbest**; karar yazıya geçiyor ki sessiz kalmasın.
- TCKN'de **checksum doğrulaması yoktur** ve eklenmez (Java'da da yok); biçim `^[1-9][0-9]{9}[02468]$`. Java'nın deseni `[0,2,4,6,8]` yazıyor, yani son hane olarak **virgülü** de kabul ediyor; karakter sınıfı düzeltilir. Tekillik onay anında denetlenir (`TC_IDENTITY_NUMBER_ALREADY_IN_USE`, 409).
- NFC çip yolu (K15) Faz 2b'dedir ve bu sözleşmede **yoktur**. Kimlik doğrulama bugün yalnız belge incelemesidir; Java'da çip ucu `identity.passive-authentication.enabled` varsayılan `false` olduğu için zaten canlı ürün yolu değil. Çip meydan okuması (challenge) da 2b'ye bırakılır — Java'da deneme sayacı ve oran sınırı **olmadığı** için 10 dakika boyunca sınırsız denenebiliyor; 2b onu kendi ADR'siyle düzeltir.

### 6. S3 anahtar düzeni ve yol geçişi savunması

- Anahtar **`<önek>/<uuidv7>.<uzantı>`**. Önekler: `images/`, `public/`, `files/`, `files/dispute-evidence/`, `identity-documents/`, ve yükleme karantinası `incoming/`.
- **Anahtara kullanıcı girdisi girmez.** Uzantı dosya adından değil, **saptanan tipten** türetilir, dolayısıyla alfabe kapalıdır: `jpg | png | pdf`. Java uzantıyı dosya adından alıp alfanümerik ve ≤10 karakterle sınırlıyor — güvenli ama gereksizce açık; tipten türetmek sınıfı tümden kapatır. Kimlik UUIDv7'dir (plan §3.6-4), v4 değil, böylece anahtarlar zaman sırasına girer.
- **Okuma yolunda anahtar izin listesiyle doğrulanır**, kara listeyle değil: yüzde-çözme **bir kez** yapılır, sonuç `^(images|public|files/dispute-evidence|files|identity-documents|incoming)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.(jpg|png|pdf)$` deseniyle eşleşmezse `404`. Bir kez çözüldükten sonra içinde `%` kalan anahtar reddedilir. Bu, `..`, `.`, `%2e`, `%2f`, `%5c`, çift kodlama, boş parça, sondaki eğik çizgi ve mutlak yolu **yapı gereği** kapatır; bir desen listesini tek tek saymaya gerek kalmaz. `PATH_INVALID` zinciri (ADR-0021) isteğin yolunu ayrıca normalleştirir.
- Çıkış kriterinin yol geçişi testi bir **fuzz hedefidir**: `FuzzStorageKey` çözme + doğrulama zincirine rastgele girdi verir ve iki değişmezi sınar — kabul edilen her anahtar desene uyar, ve kabul edilen hiçbir anahtar kendi önekinin dışına çıkmaz.
- `identity-documents/` hiçbir genel route'tan erişilemez ve hiçbir URL'ye çözülmez. Personel indirmesi presigned URL'e **302**'dir: TTL **3 dakika**, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `Content-Disposition: attachment`. İnceleme yanıtı hiçbir depolama anahtarı ya da URL taşımaz.
- **Yükleme API'den geçmez** (stil rehberi §4, ADR-0019): istemci presigned PUT ile doğrudan nesne deposuna yazar. Java multipart kullanıyor; **Java'dan bilinçli ayrılık**, dayanağı ADR-0019'un zaten verilmiş kararı.
  - Akış üç adımdır: **başlat** (API `incoming/<uuidv7>.<uzantı>` için presigned PUT ve bir `media` satırı verir, durum `PENDING`) → istemci doğrudan yükler → **tamamla** (API nesneyi doğrular: boyut, saptanan tip, tuval; sonra EXIF'i striple yeniden kodlar ve son anahtara taşır, durum `READY`).
  - EXIF stripleme sunucu tarafında kalmak zorunda olduğu için saf presigned yükleme yetmez: karantina öneki + tamamlama adımı sektörün standart cevabıdır (S3 + sunucu tarafı dönüştürme). Doğrulama ve dönüştürme River işine taşınır; `complete` `202` döner ve medya kaynağından okunur (stil rehberi §3, "uzun iş isteğe bağlanmaz").
  - `incoming/` **M2**: tamamlanmayan yükleme 24 saat sonra temizlik işiyle silinir.
  - Rollback değişmezi Java'dan korunur ve genelleştirilir: nesne yazıldıktan sonra DB yazımı düşerse nesne silinir; **sonucu belirsiz** transaction'da nesne **silinmez** (fail-safe); temizlik hatası özgün hatayı maskelemez, yalnız `ERROR` seviyesinde loglanır.

### 7. Şirket onay akışı ve davet

**Onay.** `PENDING → APPROVED | REJECTED`; `REJECTED → APPROVED` serbesttir ve notu değiştirir; `APPROVED` terminaldir. Onay notu isteğe bağlı, **ret gerekçesi zorunlu** (1–1000). Karar `approval_status`, kırpılmış not, `approval_decided_at` ve karar veren personelin kimliğini yazar. Not **yalnız şirketin kendi üyelerine** görünür; üye olmayan durumu görür, gerekçeyi görmez (Java'nın davranışı). İzin `company.approve`.

- **Onaylanmamış şirket ilan yayınlayamaz.** Java'da bu kontrol **hiç yok**: `JobManager#createCorporateJob` yalnız üyeliğe bakıyor, oysa kendi OpenAPI metni "şirket onaylanana kadar ilan yayınlayamaz" diyor. Belge ile kod çelişiyor; Go belgeyi uygular. Sektör standardı budur (Stripe Connect, Upwork: işletme doğrulanmadan işlem yok). Onay durumu ADR-0024'ün alt kümesinde bir **relation değil ön süzgeçtir** (koşul ve öznitelik yok), yani diğer durum süzgeçleri gibi servis katmanında durur. **Bu tek kalem ürün kararıdır ve Yusuf'un onayını bekler** (açık soru 1).
- Şirket kurmak kimliği doğrulanmış kullanıcıya açıktır; kurucu şirketin `ADMIN`'i olur.
- Şirketin iş kimliği alanlarında **tekillik yoktur** (Java'da da yok: vergi numarası, ad ve e-posta tekil değil, `slug` kolonu hiç yok) ve eklenmez: vergi numarasının tekilliği şube ve devir vakalarında yanlış olur, doğrulaması `company.approve` incelemesinin işidir. Vergi numarasında VKN checksum doğrulaması yoktur.
- `taxNumber` ve `type` kurulduktan sonra değişmez (Java'da da değişmiyor).

**Üyelik.** Roller `ADMIN` ve `STAFF` (K17). Üyelik yalnız iki yolla doğar: şirketi kurmak ve daveti kabul etmek. Son `ADMIN` çıkarılamaz (`COMPANY_LAST_ADMIN_CANNOT_BE_REMOVED`, 409). Rol değiştirme operasyonu Java'da **yoktur**; bu sözleşmede de yoktur (açık soru 2). Üyelik satırı soft-delete'lidir ve aynı kişi sonra yeniden davet edilebilir.

**Davet token'ı: yoktur, ve olmayacak.** Java'da davet bir veritabanı UUID'siyle adreslenir, e-posta **hiçbir bağlantı ya da token taşımaz**, yetki kimlikten gelir. Karar korunuyor ve gerekçesi yazıya geçiyor: davet edilen **zaten kayıtlı bir Koala kullanıcısı olmak zorundadır** (davet satırının `invited_user` FK'si zorunlu), dolayısıyla token'ın ekleyeceği bir şey yok — üstelik e-postadaki token iletilince taşınan bir kimlik bilgisi olur. Davet kimliğini ele geçiren yabancı **gizlenmiş 404** alır. Bu, "var olan kullanıcıyı davet et" akışının yerleşik biçimidir (Slack, GitHub organizasyon daveti).

Java'nın eksikleri kapatılır:

- **Süre: 14 gün.** Java'da hiç yok; `PENDING` bir davet sonsuza kadar cevaplanabiliyor. Süreli davet standarttır (GitHub 7 gün). Süresi geçmiş davet `EXPIRED` okunur ve cevaplanamaz.
- **İptal: şirket `ADMIN`'i daveti geri alabilir** (`REVOKED`). Java'da hiç yol yok; gönderen daveti geri alamıyor.
- Durum kümesi: `PENDING, ACCEPTED, REJECTED, REVOKED, EXPIRED`.
- **Kabul atomiktir.** Java'da `answerToInvitation` transaction **dışında** koşuyor: davetin `ACCEPTED` yazımı ile üyelik satırı iki ayrı transaction, arada düşen süreç `ACCEPTED` ama üyeliksiz bir davet bırakıyor ve ikinci deneme `INVITATION_ALREADY_ANSWERED` aldığı için kullanıcı **kalıcı olarak dışarıda kalıyor**. İkisi tek transaction'da yazılır. (Bu, [[silent-invalidation-defect-class]]'ın bu alandaki örneği.)
- Daveti **okuyabilenler**: davet edilen, daveti gönderen ve şirketin bugünkü `ADMIN`'i. **Cevaplayabilen yalnız davet edilendir** (Java'nın ayrımı, korunuyor). Aynı (şirket, davetli) için tek açık davet.
- **Davet edilenin kimliği doğrulanmış olmalıdır.** Java burada `tcIdentityNumber != null`, şirket kurmada ise `identityVerified` bakıyor — aynı kavrama iki ölçüt. Go her ikisinde `identity_verified`'ı kullanır.

**Takip.** Herhangi bir kimlikli kullanıcı var olan herhangi bir şirketi takip edebilir (onaylanmamış ve kendi şirketi dahil). Tekrar takip sessiz no-op, takip edilmeyeni bırakmak sessiz no-op; ikisi de `204`.

- **Soft-delete sınıfı: M6 (bağ ve ayar), yani soft-delete'li değil.** Takip satırı bir aboneliktir; bırakmak bir **tercih**, silme değil. Denetim ya da hukuki değeri yok. Bırakma **hard delete**'tir, yeniden takip yeni satır açar, tekil indeks kısmi değil **tam** olabilir.
  - **Java'dan bilinçli ayrılık.** Java soft-delete ediyor ve yeniden takipte yeni satır açıyor; sonucu iki kusur: her bırakma kalıcı bir mezar taşı bırakıyor (hiç temizlenmiyor) ve `followedAt` **son** takibin tarihi oluyor, ilkinin tarihi kaybediliyor. Hard delete ikinci davranışı değiştirmez (`followedAt` gene güncel aboneliğin başlangıcı) ama mezar taşlarını kaldırır.
- Takipçiye ilan bildirimi en iyi çabadır: bir gönderim hatası kalanları durdurmaz, bireysel ilan kimseye gitmez, ilanın sahibine kendi ilanı bildirilmez, ve özellik tek anahtarla susturulabilir (Java'nın davranışı).

### 8. Adres verisi: Google'dan gelenin ne kadarı bizde kalır

- **Saklanan:** Google'ın `place_id`'si, bizim normalleştirdiğimiz bileşenler (ülke/şehir/ilçe referans tabloları + mahalle, cadde, site, blok, bina, kat, daire serbest alanları), posta kodu ve koordinatlar.
- **Saklanmayan:** Google'ın `formatted_address` dizgisi, ham API yanıtı ve diğer Google içeriği. Gerekçe **lisanstır**: Google Maps Platform Hizmet Şartları §3.2.3(b) `place_id`'nin süresiz önbelleklenmesine izin verir, diğer içeriği 30 günle sınırlar. Uzun süre `formatted_address` tutmak şartları ihlal eder. Adres metnini **kendi bileşenlerimizden** üretiriz; `place_id` kalıcı değil **yenilenebilir** bir işaretçi olarak tutulur.
- **Koordinatlar tam hassasiyetle saklanır, yuvarlama yanıtta yapılır** ([[job-location-rule]], 04.11.2026): şirket adresi tam (gizlenebilir), bireysel ilan yaklaşık, harita hep yaklaşık. Yuvarlama projeksiyon fonksiyonundadır, depolamada değil; aynı satır farklı çağırana farklı hassasiyetle görünür ve kural tek yerde durur.
- Adres arama oturumu **sahibine bağlıdır** (Scope / `SelfUserID`), ömrü 15 dakika; başka kullanıcı oturum kimliğini ele geçirse de kullanamaz (Java'da da böyle, AZ-114/115).
- Adres kaydının sahibi kullanıcı ya da şirkettir; sahiplik `owner` sınıfıdır, yani modelde relation değil `Scope`'tur (plan §5.4 "Modelin dışında kalanlar").
- Referans verisi (ülke, şehir, ilçe; üniversite, fakülte, bölüm) **M4 (referans)** sınıfındadır, salt okunurdur ve kimlik istemez. Java'daki `AcademicReferenceManager` bir tavsiye mektubu alanı değil, salt okunur üniversite/fakülte/bölüm aramasıdır; sözleşmede de öyle durur.

### 9. Admin uçları ve PII kapısı

- **Kullanıcı detayı tek operasyondur, iki projeksiyonu vardır.** `user.read` kaydı açar; e-posta, telefon, doğum tarihi, maskeli TCKN ve adres yalnız `*pii.Grant` alan projeksiyonda `pii.Reveal` ile çıkar, onu da `user.read_pii` verir. İzni olmayan personel alanları **`null`** görür. İki ayrı operasyon yazılmaz: istemcinin hangisini çağıracağını tahmin etmesi gerekmez ve `can` haritası (`/v1/admin/me/capabilities`) arayüzü zaten yönlendirir.
- **TCKN hiçbir admin yanıtında açık görünmez.** Maskeleme tam blanklamadır (`***********`), kısmi değil: kimlik numarasının son haneleri de tanımlayıcıdır. IBAN ve telefon kısmi maskelenir (ilk 4 + son 4; son 4). Java'nın maskeleri aynen.
- Kimlik incelemesi (kuyruk, detay, fotoğraf indirme) `identity.review`. Beyan edilen TCKN **incelemeciye açık görünür**, çünkü inceleme tam olarak beyan ile fotoğrafın karşılaştırılmasıdır; `identity.review` `PERSONAL_DATA` olduğu için bu **okumalar da** denetim kaydına yazılır.
- Kimlik açılma izleri (`GET /v1/admin/users/{userId}/identity-reveals`) `user.read_pii`: izin kaydının kendisi kişisel veridir (Java'nın seçimi). Satırlar ekle-sil-yapılamaz (append-only), IP yazım anında maskelenmiştir ve personel tam IP görmez.
- Şirket kuyruğu, detayı ve logo indirmesi `company.approve`.
- `PERSONAL_DATA` ya da `FINANCIAL` bir iznin altındaki her istek, **izin verilse de reddedilse de** denetim satırı yazar (ADR-0026). Sözleşmede olmayan `/v1/admin` yolu `404 ROUTE_NOT_FOUND`, izni olmayan personel `403 STAFF_PERMISSION_DENIED`.
- Bu alanın admin operasyonlarının hiçbiri `x-authz-exempt` taşımaz; `staff.SelfService()` listesi büyümez.

### 10. Hata kodları ve gizleme

Alanın bütün kodları `api/components/problem.yaml`'ın `ErrorCode` listesine girer ve her biri ilgili paketin `Codes()` listesinde sabit olarak tanımlanır (ADR-0028, `internal/archtest`). Gizleme kuralı tektir ve Java'nın üç ayrı cevabının yerine geçer:

- **Görme yetkisi olmayan çağıran kaynağın 404'ünü alır**, gövdesi hiç var olmamış bir kaynağın 404'üyle bayt bayt aynıdır (ADR-0014). Java aynı "üyesi değil" durumuna üç ayrı yanıt veriyor: `CompanyManager` 404 `USER_NOT_ASSOCIATED_WITH_COMPANY`, `JobManager#createCorporateJob` 400, `JobManager#getJobsOfCompany` 403. Go'da hepsi kaynağın 404'üdür.
- Durum çakışması 409, biçim hatası 400; "izin var ama durum elvermiyor" 403 değildir.

## Yerine geçtiği

1. **ADR-0020 §11 ve ADR-0026'nın `x-resource` kuralı** → bu ADR Karar 1'in son paragrafı: kural duruyor, ama `x-not-implemented` taşıyan operasyonda **denetimi erteleniyor**. İşaret kalkınca kural tam hâliyle geri gelir.
2. **ADR-0027'nin "Bu PR'da kalanlar" listesindeki "`Keeps` sonucunun gerçek sözleşmede kanıtlanması"** → bu ADR'de **kapanmıyor**: Faz 2'nin tabloları bu PR'da gelmediği için ilk gerçek `detach` kenarı da gelmedi. Kalem Sonuçlar'a taşınıyor.
3. **ADR-0025 §9'un self-servis listesi** → değişmiyor; bu ADR Karar 9 listeyi büyütmediğini kayda geçirir.
4. **Plan §6.1 Faz 2'nin çıkış kriterindeki "Alanın P0 senaryolarının %100'ü … yeşil"** → bu PR o kriteri karşılamaz ve karşılamayı iddia etmez: sözleşme ve katalog donuyor, senaryolar `deferred: {phase: 2}` ile Faz 2'nin uygulama PR'larını bekliyor. Karar 1'in katalog bağı bunu makinece zorlar.
5. **Plan §6.1 Faz 2'nin "kimlik belgesi yükleme (NFC'siz)" kalemi** → Karar 5: çip yolu ve çip meydan okuması Faz 2b'ye, kendi ADR'siyle.

ADR-0013, ADR-0014, ADR-0019, ADR-0024, ADR-0026 ve ADR-0028'in kararları değişmiyor. ADR-0019 §4'ün "dosya yüklemeleri API'den geçmez" kararı Karar 6'da karantina öneği ve tamamlama adımıyla **uygulanıyor**, değiştirilmiyor.

## Sonuçlar

- Sözleşme bugün kamuya açık: plan §4.2'nin istemci işi (web → admin → mobil) Faz 2'nin uygulamasını beklemeden tip üretimine başlayabilir. Hangi ucun canlı olmadığı spec'te `x-not-implemented` ile okunur.
- Uygulanmamış operasyon `501` döner. Bu, "404 ROUTE_NOT_FOUND" ile karıştırılmaz: yol **vardır**, sözleşmesi bağlayıcıdır, yalnız handler'ı yoktur.
- Bir operasyonu uygulamanın yolu uzadı ve adımları makine zorluyor: işareti kaldır → iskeleyi sil (`TestNotImplementedOperationsAreDeclared`) → `x-resource`'un tablosunu ve fikstürünü getir (`ReadTest`) → alanın P0 senaryolarını bağla (`TestFrozenDomainsDeferTheirScenarios`). Üçü de ayrı ayrı kırmızı verir.
- `server.gen.go` bu PR'da büyük ölçüde büyüyor. Dosya **üretilmiştir**, elle düzenlenmez ve birebirliğini `make gen-check` kanıtlar; incelemenin odağı spec, senaryo katalogu, authz ve bu ADR'dir.
- Bu PR'da kalanlar:
  - Faz 2'nin migration'ları, `db/softdelete.yaml` sınıflandırmaları, sqlc sorguları ve fikstürleri. `x-resource`'un tabloları o PR'da gerçek olur.
  - `internal/media`'nın karantina → doğrula → striple → taşı River işi ve `FuzzStorageKey` (Karar 6). Çıkış kriterinin yol geçişi fuzz'ı o PR'da yeşil olur.
  - `incoming/` ve `pending_registrations`'ın temizlik işleri (M2 saklama süreleri).
  - Onaylanmamış şirketin ilan yayınlayamaması (Karar 7) Faz 3'ün ilan ucunda uygulanır; bu sözleşmede ilan ucu yok.
  - ADR-0027'den devredilen `Keeps` kalemi: ilk gerçek "ebeveyni silinmiş çocuk listede kalır" ucu Faz 2'nin veri PR'ıyla gelir.

## Açık sorular

1. **Onaylanmamış şirket ilan yayınlayabilir mi?** Karar 7 "yayınlayamaz" diyor ve sektör standardı bu, ama Java'da kontrol hiç yok: bugün `PENDING` bir şirket ilan açabiliyor, davet gönderebiliyor, takip edilebiliyor. Bu bir **ürün kararıdır** ve Yusuf'un onayını bekler. "Yayınlayabilir" seçilirse Karar 7'nin o maddesi düşer, geri kalanı durur.
2. **Şirket üyesinin rolü değiştirilebilsin mi?** Java'da operasyon yok: rol davet anında donuyor ve yalnız çıkarıp yeniden davet ederek değişiyor. Sözleşmeye `PATCH …/contacts/{userId}` eklemek küçük bir iştir ama K17'nin "v1'de ADMIN/STAFF" kararının ötesinde bir ürün kararı; sorulmadan eklenmedi.
3. **Davetin 14 günü ve iptali** Java'da olmayan iki yeni davranış. Sektör standardı oldukları için karar verildi, ama süre (14 gün) bir ürün tercihidir.
