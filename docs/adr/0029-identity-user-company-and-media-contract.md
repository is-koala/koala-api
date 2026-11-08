# ADR-0029: Faz 2 sözleşmesi — kayıt, kimlik, kullanıcı, şirket ve medya

- Durum: Kabul edildi
- Tarih: 08.11.2026
- Karar kaydı: K10, K11, K14, K15 (hazırlık), K17, K19, S4, D5
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §4.1 (sözleşme ilkeleri), §4.2 (istemci işi), §5.4–5.5 (authz), §6.1 Faz 2, §6.2 (Java testlerini senaryoya çevirme); [ADR-0005](0005-api-contract-principles.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) §4 (dosya yüklemeleri), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md) §11, [ADR-0024](0024-authz-core-evaluator-and-conformance.md), [ADR-0025](0025-authentication-tokens-sessions-and-staff-sign-in.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0028](0028-recursion-rules-body-allowlist-and-gate-completeness.md); Java `origin/main` (0cbbce69, v0.9.32) — 19 manager ve IT'leri okundu, koşturulmadı
- Dış kaynaklar (madde numaraları **NIST SP 800-63B Rev. 3**'e ve **OWASP ASVS 4.0.3**'e göredir; Rev. 4 bütün bölüm numaralarını değiştirdi, 5.x → 3.x):
  - NIST SP 800-63B Rev. 3 §5.1.3.2 (bant dışı doğrulayıcılar: doğrulama 10 dakika içinde tamamlanmazsa geçersiz), §5.1.1.2 (hatırlanan sırların özetlenerek saklanması ve kompozisyon kuralı önerilmemesi), §5.2.2 (kısıtlama: ardışık başarısız deneme sınırı)
  - OWASP ASVS 4.0.3 V3.3.3 (diğer etkin oturumları kapatma seçeneği; bizim kuralımız bundan daha sıkıdır, Karar 4)
  - RFC 9457, RFC 7396, RFC 6901
  - Google Maps Platform [Hizmet Şartları](https://cloud.google.com/maps-platform/terms) ve Places için Service Specific Terms: yer kimliği süresiz, diğer içerik sınırlı süre önbelleklenebilir (madde numarası açık soru 4)
  - Stripe Connect ve Upwork (işletme doğrulanmadan işlem yok); GitHub organizasyon daveti (süreli, iptal edilebilir); S3 + sunucu tarafı dönüştürme kalıbı
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

**Üretilmiş testlerin gevşemesi.** `softdeletetest.ReadTest.plan` ve `softdeletetest.JoinTest.readers` işaretli operasyonu kataloğa bakmadan **atlar**. Böylece donmuş bir GET okuyacağı tabloyu dürüstçe adlandırabilir (`x-resource: companies`), tablo henüz yokken. İşaret kalkınca okuma testi kendiliğinden silahlanır: o gün tablo, sınıflandırma ve fikstür zorunlu olur. Bu, Karar 1'in sözleşmeyi veri katmanından önce dondurabilmesinin tek nedenidir ve `x-resource`'un zorunluluğunu (ADR-0026, ADR-0020 §11) kaldırmaz, yalnız **ne zaman** denetlendiğini erteler.

**Operasyon kendi hata kodlarını sayar.** Donmuş bir sözleşmenin en kolay kırıldığı yer burasıydı: handler'ın üreteceği kodun HTTP durumu operasyonun yanıt kümesinde yoksa istemcinin üretilmiş tipi yanıtı çözemez, ve durumu sonradan eklemek stil rehberi §2'ye göre uyumsuz değişikliktir — yani 501'i ayrı yanıt yazmama gerekçesinin aynısı, ters yönden. Bu PR'ın incelemesinde beş operasyonda tam bu kusur bulundu (`POST /v1/companies` 403, 404 ve 409'u taşımıyordu).

Çözüm: her operasyon `x-error-codes` ile **kendi alan kodlarını** sayar (kodu olmayan operasyon boş liste yazar), ve `api.TestOperationStatusesCoverTheirErrorCodes` listelenen her kodun `ErrorCode` açıklamasındaki durumunu operasyonun yanıt kümesinde arar. Aynı test her `ErrorCode` üyesinin bir durumu olmasını da ister; olmayan üye gerekçeli istisna listesindedir (`FIELD_*` kodları `ProblemField.code`'da taşınır, realtime olay kodları soket üzerinde).

**Üç sınır** bilinçlidir ve burada kayda geçer. Mekanizma bir gerileme değil daralmadır — eskiden böyle bir liste hiç yoktu ve kusur yalnız insan gözüyle bulunabiliyordu — ama ne yapmadığını yazmadan "bağlandı" demek yanlış olur:

1. **Evrensel kodlar denetlenmez.** Platform (`REQUEST_INVALID`, `RATE_LIMITED`, `INTERNAL`…), oturum (`AUTH_TOKEN_*`, `STAFF_PERMISSION_DENIED`…) ve Idempotency-Key kodları ortak yanıtlardan gelir; her operasyonda tekrar edilmeleri listeyi gürültüye çevirirdi.
2. **Ters yön denetlenmez:** karşılığı hiçbir kod olmayan bir yanıt durumu (gereksiz bir `409` gibi) kırmızı vermez. Çünkü 400, 401, 429 ve 404'ün çoğu evrensel kodlarla ya da yol parametresinin varlığıyla açıklanır; kuralı ters yöne çevirmek yanlış pozitif üretirdi. İncelemede bakılır.
3. **"Operasyonun ürettiği her kod listede mi" diye soran bir şey yok.** Test yalnız listelenen kodun durumunu denetliyor; listede hiç yazılmamış bir kodu göremez, çünkü bugün handler yok ve kodun hangi operasyondan çıktığını sözleşmeden çıkarmanın yolu da yok. Üstelik bir kodun durumu artık **iki bağlı olmayan yerde** yazılı: `problem.yaml`'ın düzyazısı ve `apperr.Kind` → `httpx.StatusOf` eşlemesi. Bir handler `KindConflict` ile dönerken düzyazı 404 demişse, ya da kod listeye hiç yazılmamışsa, kapı yeşil kalır ve istemci yanıtı çözemez.

   **Gereklilik:** Faz 2'nin ilk handler PR'ı, handler'ın ürettiği kodlar ile operasyonun `x-error-codes` listesini bağlayan denetimi kurar — handler'ın döndürdüğü `apperr.Code` sabitleri ile listenin karşılaştırılması, ve kodun `Kind`'ının düzyazıdaki durumla aynı olması. Bugün yazılacak bağ spekülatif olurdu; mekanizma anlam kazandığı anda gelir.

Dördüncü, daha küçük bir sınır: istisna listesi (`statusFreeCodes`) **tek yönlüdür.** Enum'dan silinen bir kodu yakalar, ama "bu kod artık bir REST yolu kazandı" durumunu yakalamaz. `CONVERSATION_NOT_FOUND`'un gerekçesi tam bu: "Faz 4 ona bir REST yolu verene kadar" — o gün istisna sessizce yerinde kalır ve kod bir durum belgelemeden yaşamaya devam eder. Faz 4'ün PR'ı listeyi gözden geçirmek zorundadır.

**Dondurmanın dört yan kararı**, uygulama sırasında çıktı ve burada kayda geçiyor:

1. **`GET /v1/me` kimlik çekirdeği kalır; profil `/v1/me/profile`'dadır.** `Me` şemasına zorunlu profil alanları eklemek derlemeyi kırmazdı — Go'da adlandırılmış alanlı struct literal'i eksik alanla da derlenir — ama bugünkü handler o alanları yazmadığı için yanıt sessizce sıfır değer (`""`, `false`) taşırdı. Bu tam olarak [[silent-invalidation-defect-class]]'ın biçimidir, bu yüzden profil ayrı bir kaynağa konuldu. `/v1/me`'nin açıklamasındaki "profil alanları Faz 2'de gelir" cümlesinin yerine bu karar geçer.
2. **Nullable yanıt alanı `required`'dır.** Stil rehberi §9 "değer yok null ile yazılır, alan atlanmaz" diyor; üretici bunu ancak alan `required` ise sağlıyor. Aksi hâlde `omitempty` koyuyor ve alan yanıttan tümden düşüyor. Alanın görünür ve `null` olması istemci için "bilinmiyor" ile "yok"u ayırt edilebilir kılar.
3. **`NullableTimestamp`** `components/datetime.yaml`'a eklendi. `oneOf: [{$ref: Timestamp}, {type: "null"}]` biçimi `koala-instant-is-date-time`'ı geçmiyor (çözülmüş şemanın kökünde `format` yok) ve Go'da birlik tipi üretiyor. Bileşen `type: [string, "null"] + format: date-time + pattern: Z$` taşır ve `Timestamp` ile aynı `x-go-type`'ı kullanır, böylece `*jsontime.UTC` olur ve UTC kuralı tek yerde kalır.
4. **Yükleme `/v1/me/media` altındadır**, `/v1/media` değil. Medya sahibine bağlı bir kaynaktır (`owner` sınıfı, Scope); `/v1/me/...` altında olması ADR-0025'in oturum uçlarında kurduğu `x-authz-exempt` kalıbını aynen uygulamayı sağlar ve "başkasının medyası var olmayandan ayırt edilemez" kuralını yol düzeyinde de söyler.

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
| Saklama | SHA-256 özeti; doğrulamadan sonra özet silinir | **düz metin** | Sektör pratiği ve §5.1.1.2'nin hatırlanan sırlar için koyduğu kuralın benzeşimi: NIST'in hiçbir maddesi bant dışı sırrın özetlenmesini **istemiyor**, ama kodun düz metin saklanması veritabanı okumasını tam bir kimlik doğrulama atlatmasına çeviriyor. **Java'dan bilinçli ayrılık.** |
| Ömür | **10 dakika** | 15 dakika | NIST SP 800-63B Rev. 3 §5.1.3.2: bant dışı doğrulama 10 dakika içinde tamamlanmazsa geçersiz sayılmalı. |
| Deneme sınırı | **5**; dolunca doğrulama yakılır (`VERIFICATION_ATTEMPTS_EXHAUSTED`), yenisi başlatılır | **yok** | §5.2.2 kısıtlamayı zorunlu kılıyor; verdiği üst sınır 100 ardışık başarısız denemedir. **5 bizim daha sıkı tercihimizdir** (altı haneli kodda 100 deneme 10.000'de 1 tahmin şansı demek). Java'da sayaç kolonu bile yok: ömür boyunca sınırsız tahmin. **Java'dan bilinçli ayrılık.** |
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
- Kayıtsız adres ayırt edilemez: aynı `202` gövdesi, posta yok. (`202`, çünkü posta kuyruğa girer ve iş arka planda koşar; stil rehberi §5.) **Java'dan ayrılık:** Java'nın bilinen dalı token üretip posta kuyruğa atıyor, bilinmeyen dalı atmıyor; zamanlama farkı `docs/research/password-reset-token-design.md`'de açık bir boşluk olarak duruyor. Go iki dalda da aynı işi yapar (bilinmeyen dalda üretilen token atılır), AUTH-001'in giriş için kurduğu kalıp.
- **Başarılı sıfırlama çağıran dahil bütün oturumları kapatır** ve token çifti **döndürmez**; kullanıcı yeniden giriş yapar (Java da böyle). ASVS 4.0.3 V3.3.3 bundan **daha gevşektir**: kullanıcıya diğer oturumları kapatma *seçeneği* verilmesini ister, kapatılmasını zorunlu kılmaz. Bizim kararımız standardın üstündedir, çünkü sıfırlamada hesabın ele geçtiği varsayılır ve o anda seçim kullanıcıda değil, saldırgandadır. Uygulama içi parola değişimi ise çağıranın oturumunu açık bırakır ve diğerlerini kapatır (Java'nın davranışı, korunuyor) — ikisi farklı, çünkü sıfırlamada hesabın ele geçtiği varsayılır.
- Sıfırlama ve uygulama içi değişim, kullanıcının **açık parola sıfırlama token'larını** iptal eder.
- **Parola kuralı ADR-0025'in kuralıdır:** en az 15, en çok 128 karakter, `PASSWORD_POLICY_VIOLATION`. Java'nın 12 kod noktası / 72 bayt sınırı bcrypt'ten geliyordu; Go bcrypt kullanmadığı için 72 bayt sınırı yok. Karakter sınıfı kuralı yok (NIST SP 800-63B Rev. 3 §5.1.1.2 kompozisyon kurallarını önermiyor).

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
  - Karantina anahtarının uzantısı **başlatma anında istemcinin bildirdiği türden** türetilir ve yalnız yazma adresini kurar; son anahtarın uzantısı tamamlama adımında **saptanan** tipten gelir. İkisi ayrışabilir: karantinadaki `.jpg`, PNG olduğu anlaşılırsa `images/<uuid>.png` olarak taşınır. Karantina anahtarı hiçbir yanıtta kaynak adresi olarak verilmez.
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

**Galerinin sırası, kapağı ve açıklamaları.** Galeri tümden değiştirilir, sıra anlamlıdır ve **kapak dizinin ilk öğesidir**; her öğe isteğe bağlı bir açıklama (`caption`, en çok 255 karakter) taşır. Böylece sıralamak, kapağı seçmek ve açıklamayı güncellemek tek işlemdir.

Bu, Java'ya göre **bilinçli bir daralmadır** ve kayda geçmesi gerekiyor: Java her öğeye bir `cover` boolean'ı alıyor ve "en fazla bir kapak" değişmezini doğruluyor, yani sıra `[A, B, C]` iken kapağın `B` olması temsil edilebiliyor. Türetilmiş kapakta bu temsil edilemez. Daralmayı kabul ediyoruz çünkü Java'nın iki gözlenebilir davranışı korunuyor (kapak çıkarılırsa sıradaki kapak olur; ilk görsel kendiliğinden kapaktır), eksilen şey bir değişmez ve sıralamayla kapak seçmek yaygın bir kalıptır. Yine de bugün kayıtsız olan bir yeteneği kaldırıyor; **ürün tarafına bilgi olarak geçer.**

Java'nın **ikinci galeri değişmezi** de donduruldu: aynı medya listede iki kez geçemez (`COMPANY_GALLERY_DUPLICATE_IMAGE`, CMP-024). Bu kural **şema ile ifade edilemiyor** ve nedeni şekil değişikliğinin kendisi: `mediaIds: [uuid]` biçiminde `uniqueItems: true` yeterdi, ama açıklamaları taşımak için öğeler nesneye dönüştü ve `uniqueItems` bütün nesneyi karşılaştırıyor — aynı `mediaId`, farklı `caption` iki ayrı öğe sayılır. JSON Schema'da "şu anahtara göre tekil" diye bir anahtar sözcük yok. Bu yüzden kural gövde kuralı olarak kalır: sözleşmede kodu, açıklaması ve senaryosu var, denetimi handler'da.

`sortOrder` da ayrı bir alan taşımaz: dizideki konuma katlanmıştır. `caption` katlanamaz — türetilebilecek bir kaynağı yok — bu yüzden sözleşmeye alındı. **Kapsam defterinin (Karar 11) sınırı buradan görülüyor:** defter uç granülerliğindedir, yani bir Java ucunun karşılığı varsa "kapsandı" der; o ucun **alanlarından** birinin düşmesini yakalamaz. `caption` tam böyle düşmüştü ve defter sessiz kaldı. Alan düzeyi karşılaştırma uygulama PR'larının işidir: handler yazılırken Java DTO'su ile Go şeması yan yana okunur.

**Galeri görünürlüğü.** Şirketin galerisi **kimliksiz okunabilir** (Java'nın kararı, korunuyor): galeri şirketin vitrinidir ve SEO'lu şirket sayfasında görünür. Şirketin kendisi (`GET /v1/companies/{companyId}`) kimlik ister, yani galeri kimliksiz bir **varlık oracle'ıdır**: var olmayan şirketin galerisi 404, var olanın 200 döner. Kabul ediyoruz, çünkü şirket kimliği UUIDv7'dir ve tahmin edilemez; şirket listesi kimliksiz hiçbir uçtan sayılamaz. Galeri sıralaması aynı zamanda kapağı belirler: listedeki ilk görüntü kapaktır, ayrı bir kapak ucu yoktur.

**Takip.** Herhangi bir kimlikli kullanıcı var olan herhangi bir şirketi takip edebilir (onaylanmamış ve kendi şirketi dahil). Tekrar takip sessiz no-op, takip edilmeyeni bırakmak sessiz no-op; ikisi de `204`.

- **Soft-delete sınıfı: M6 (bağ ve ayar), yani soft-delete'li değil.** Takip satırı bir aboneliktir; bırakmak bir **tercih**, silme değil. Denetim ya da hukuki değeri yok. Bırakma **hard delete**'tir, yeniden takip yeni satır açar, tekil indeks kısmi değil **tam** olabilir.
  - **Java'dan bilinçli ayrılık.** Java soft-delete ediyor ve yeniden takipte yeni satır açıyor; sonucu iki kusur: her bırakma kalıcı bir mezar taşı bırakıyor (hiç temizlenmiyor) ve `followedAt` **son** takibin tarihi oluyor, ilkinin tarihi kaybediliyor. Hard delete ikinci davranışı değiştirmez (`followedAt` gene güncel aboneliğin başlangıcı) ama mezar taşlarını kaldırır.
- Takipçiye ilan bildirimi en iyi çabadır: bir gönderim hatası kalanları durdurmaz, bireysel ilan kimseye gitmez, ilanın sahibine kendi ilanı bildirilmez, ve özellik tek anahtarla susturulabilir (Java'nın davranışı).

### 8. Adres verisi: Google'dan gelenin ne kadarı bizde kalır

- **Saklanan:** Google'ın `place_id`'si, bizim normalleştirdiğimiz bileşenler (ülke/şehir/ilçe referans tabloları + mahalle, cadde, site, blok, bina, kat, daire serbest alanları), posta kodu ve koordinatlar.
- **Saklanmayan:** Google'ın `formatted_address` dizgisi, ham API yanıtı ve diğer Google içeriği. Gerekçe **lisanstır**: Google Maps Platform'un yayımlanmış politikası `place_id`'nin süresiz önbelleklenmesine izin verir, diğer içeriği sınırlı bir süreyle (yaygın olarak 30 gün) sınırlar. Uzun süre `formatted_address` tutmak şartları ihlal eder. Kuralın tam madde numarası Places'in Service Specific Terms'inde arandı ve doğrulanamadı; **açık soru 4**. Adres metnini **kendi bileşenlerimizden** üretiriz; `place_id` kalıcı değil **yenilenebilir** bir işaretçi olarak tutulur.
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

### 11. Kapsam defteri: Java ucu → Go operasyonu ya da faz

"Alanın spec'i donduruldu" ancak alan **gerçekten** kapsanmışsa doğrudur, ve bu iddia bugüne kadar hiçbir yerde denetlenebilir değildi: plan §1.2 yalnız toplu sayı (266 uç) veriyor. Aşağıdaki defter Faz 2'nin alanına değen 12 controller'ın her ucunu ya bir Go operasyonuna ya da onu alacak faza bağlar. Sonradan çıkan bir uç, sözleşmeye uyumsuz ekleme olarak geri döner (bu PR'ın incelemesinde dört örneği çıktı), bu yüzden defter sözleşmenin parçasıdır.

Kaynak: `koala-backend` `origin/main` (0cbbce69), `webAPI/*Controller.java`'nın `@*Mapping` bildirimleri.

| Java controller | Uç | Go karşılığı ya da faz |
|---|---|---|
| `AuthController` (8) | `POST /auth/start-registration` | `POST /v1/auth/registrations` |
| | `POST /auth/verify-registration-token` | `POST /v1/auth/registrations/verify` |
| | `POST /auth/complete-registration` | `POST /v1/auth/registrations/complete` |
| | `POST /auth/forgot-password` | `POST /v1/auth/password-reset` |
| | `POST /auth/reset-password` | `POST /v1/auth/password-reset/complete` |
| | `POST /auth/login`, `/refresh-token`, `/logout` | Faz 1 (ADR-0025) |
| `UsersController` (13) | `GET /users/{userId}` | `GET /v1/users/{userId}` |
| | `PATCH /users/me` | `PATCH /v1/me/profile` |
| | `PUT /users/me/password` | `PUT /v1/me/password` |
| | `PUT /users/me/profile-picture` | `POST /v1/me/media` + `PATCH /v1/me/profile` (`profilePictureId`) |
| | `GET /users/me/identity-documents` | `GET /v1/me/identity-documents` |
| | `POST /users/me/identity-documents` | `POST /v1/me/identity-documents` |
| | `PUT /users/me/address` | `POST`/`PUT /v1/me/addresses` (tek adres yerine liste) |
| | `GET /users/me` | Faz 1 (`GET /v1/me`) |
| | `POST /users/me/identity-verification`, `…/challenge` | **Faz 2b** (çip, K15) |
| | `GET /users/{userId}/employer-stats` | Faz 3 |
| | `GET`/`POST /users/me/jobs` | Faz 3 |
| `UserProfileController` (13) | `GET`/`POST /users/me/educations`, `PUT`/`DELETE /…/{educationId}` | `GET`/`POST /v1/me/educations`, `PUT`/`DELETE /v1/me/educations/{educationId}` |
| | `GET /users/me/followed-companies` | `GET /v1/me/followed-companies` |
| | `PUT /users/me/cv` | `POST /v1/me/media` (`kind: CV`) + `PATCH /v1/me/profile` (`cvId`) |
| | `DELETE /users/me/cv` | `PATCH /v1/me/profile` ile `cvId: null` |
| | `PUT`/`DELETE /users/me/devices/push-token`, `PATCH /…/devices/{deviceId}` | Faz 5 (bildirim) |
| | `GET`/`POST`/`DELETE /users/me/saved-jobs…` | Faz 3 |
| `VerificationsController` (1) | `POST /verifications/{verificationId}/confirm` | `POST /v1/me/verifications/{verificationId}/confirm` |
| `CompaniesController` (17) | `POST /companies` | `POST /v1/companies` |
| | `GET`/`PATCH /companies/{companyId}` | `GET`/`PATCH /v1/companies/{companyId}` |
| | `GET /companies/{companyId}/contacts` | `GET /v1/companies/{companyId}/contacts` |
| | `DELETE /companies/{companyId}/contacts/{userId}` | `DELETE /v1/companies/{companyId}/contacts/{userId}` |
| | `POST`/`GET /companies/{companyId}/contact-invitations` | aynı yollar `/v1` altında |
| | `PUT`/`DELETE /companies/{companyId}/logo` | `PUT`/`DELETE /v1/companies/{companyId}/logo` |
| | `POST`/`DELETE /companies/{companyId}/follow` | `PUT`/`DELETE /v1/companies/{companyId}/follow` (idempotent metot) |
| | `GET`/`POST`/`PUT /companies/{companyId}/images`, `DELETE /…/{imageId}` | aynı yollar `/v1` altında; `PUT` kapağı da belirler |
| | `POST`/`GET /companies/{companyId}/jobs` | Faz 3 |
| `CompanyContactInvitationsController` (2) | `GET /contact-invitations/{invitationId}` | `GET /v1/contact-invitations/{invitationId}` |
| | `POST /contact-invitations/{invitationId}` (kabul/ret) | `POST /v1/contact-invitations/{invitationId}/accept` ve `/decline` (ayrı alt eylemler) |
| | — (Java'da yok) | `POST /v1/contact-invitations/{invitationId}/revoke` (**yeni karar**, Karar 7) |
| `AddressSearchController` (4) | `POST /locations/address-search-sessions` | `POST /v1/me/address-search-sessions` |
| | `GET /locations/address-search-sessions/{sessionId}/suggestions` | aynı, `/v1/me/…` altında |
| | `POST /locations/address-search-sessions/{sessionId}/candidates/{candidateId}/resolve` | aynı, `/v1/me/…` altında |
| | `POST /locations/address-candidates/reverse` | `POST /v1/me/address-lookups/reverse` |
| `ReferenceDataController` (7) | `GET /reference/countries` | `GET /v1/reference/countries` |
| | `GET /reference/countries/{countryId}/cities` | `GET /v1/reference/countries/{countryId}/cities` |
| | `GET /reference/cities/{cityId}/districts` | `GET /v1/reference/cities/{cityId}/districts` |
| | `GET /reference/universities`, `…/{universityId}/faculties`, `/faculties/{facultyId}/departments` | aynı yollar `/v1` altında |
| | `GET /reference/job-categories` | Faz 3 |
| `ImagesController` (2) | `POST /images` | `POST /v1/me/media` + `POST /v1/me/media/{mediaId}/complete` |
| | `DELETE /images/{imageId}` | `DELETE /v1/me/media/{mediaId}` |
| `AdminController` (28; 13'ü bu alanda) | `GET /admin/companies`, `/{companyId}` | `GET /v1/admin/companies`, `/{companyId}` |
| | `POST /admin/companies/{companyId}/approve`, `/reject` | aynı yollar `/v1` altında |
| | `GET /admin/users`, `/{userId}` | `GET /v1/admin/users`, `/{userId}` |
| | `GET /admin/users/{userId}/identity-reveals` | aynı yol `/v1` altında |
| | `GET /admin/identity-verifications`, `/{reviewId}`, `…/documents/{documentId}/download` | `GET /v1/admin/identity-verifications`, `/{reviewId}`, `…/documents/{documentId}` |
| | `POST /admin/identity-verifications/{reviewId}/approve`, `/reject` | aynı yollar `/v1` altında |
| | `GET /admin/companies/{companyId}/images/{imageId}/download` | **ikame:** galeri kimliksiz okunabiliyor ve bayt CDN'den geliyor; ayrı bir personel indirmesi bir şey eklemiyor (Karar 7) |
| | `DELETE /admin/images/{imageId}` | Faz 8 (`content.moderate`) |
| | kalan 14 uç (atama, ödeme, ilan, banka hesabı, rol) | Faz 3, 6, 8 |
| `StorageDownloadController` (3) | `GET /uploads/images/**`, `/public/**` | **ikame:** nesne adresi yanıtta CDN URL'si olarak verilir (`logoUrl`, `profilePictureUrl`, galeri). Ayrı bir okuma ucu yok; Karar 6'nın "okuma yolunda anahtar izin listesiyle doğrulanır" kuralı CDN'in önündeki nesne deposuna ve personel indirmesine uygulanır |
| | `GET /uploads/files/**` | **ikame:** özel dosya (özgeçmiş) indirmesi Faz 3'e kalıyor, çünkü yetkisi "başvuru almış işveren"e bağlı (MED-007) |
| `AuthLinkController` (2) | `GET /auth-links/complete-registration`, `/reset-password` | **kapsam dışı:** istemciye 301 yönlendiren web katmanı uçları. Go API'si token'ı gövdede alıyor; derin bağlantı çözümü web ve mobil istemcinin işi (plan §4.2) |

Defterin dışındaki 37 controller Faz 3–9'a aittir ve o fazların ADR'lerinde aynı biçimde defterlenir.

### 12. Atlanan lint kuralı kapıyı kırar

Sözleşme 88 operasyona çıkınca `make api-lint` düştü, ama bir kural ihlalinden değil: vacuum her kurala dosya başına 5 saniyelik bir bütçe veriyor ve bütçesi dolan kuralı **atlıyor**. Beş operasyon düzeyi kural (`koala-operation-authz`, `koala-operation-id-camel-case`, `koala-body-limit`, `koala-error-responses-are-problems`, `koala-deprecation-has-sunset`) sınırı aştı.

Önemli olan bulgunun biçimi: **atlanan kural hiçbir şey bildirmez, ve hiçbir şey bildirmeyen kural geçen kuraldan ayırt edilemez.** Yani koruma yerinde, çalışmıyor ve kapı yeşil görünüyor — bu deponun baskın kusur sınıfının ([[silent-invalidation-defect-class]]) kapının kendisindeki hâli.

Karar iki parçalı:

1. **Bütçe 60 saniye** (`VACUUM_TIMEOUT`). Bugünkü sözleşmenin on katına yeter.
2. **Atlama kapıyı kırar.** `scripts/api-lint.sh` vacuum'u sarar, çıktısını okur ve atlama imzasını (`Rule timed out, skipping`, `timed out after`) bulursa adımı düşürür. `scripts/api-lint-probe.sh` aynı korumayı kendi koşusuna uygular, çünkü atlanan kural orada "vakası olmayan kural" gibi görünür ve probe bütçeyi değil fikstürü suçlar.

Yalnız bütçeyi büyütmek yetmezdi: plan §1.2'ye göre sözleşme 266 uca doğru büyüyecek, yani 60 saniye de bir gün dolacak. Fark şu: dolduğu gün **kırmızı** olacak, kör kalmayacak.

Neden çıkış koduna güvenilmiyor: vacuum v0.36.2 zaten zaman aşımından sonra dosyayı reddediyor ve 2 ile çıkıyor (kapı bu yüzden düştü, biz de böyle gördük). Ama "skipping" sözcüğü aracın tercihinin değişebileceğini söylüyor, ve bütçenin bir gün yine dolacağı kesin. Koruma aracın çıkış kodu politikasından bağımsız olmalı.

## Yerine geçtiği

1. **ADR-0020 §11 ve ADR-0026'nın `x-resource` kuralı** → bu ADR Karar 1'in son paragrafı: kural duruyor, ama `x-not-implemented` taşıyan operasyonda **denetimi erteleniyor**. İşaret kalkınca kural tam hâliyle geri gelir.
2. **ADR-0027'nin "Bu PR'da kalanlar" listesindeki "`Keeps` sonucunun gerçek sözleşmede kanıtlanması"** → bu ADR'de **kapanmıyor**: Faz 2'nin tabloları bu PR'da gelmediği için ilk gerçek `detach` kenarı da gelmedi. Kalem Sonuçlar'a taşınıyor.
3. **ADR-0025 §9'un self-servis listesi** → değişmiyor; bu ADR Karar 9 listeyi büyütmediğini kayda geçirir.
4. **Plan §6.1 Faz 2'nin çıkış kriterindeki "Alanın P0 senaryolarının %100'ü … yeşil"** → bu PR o kriteri karşılamaz ve karşılamayı iddia etmez: sözleşme ve katalog donuyor, senaryolar `deferred: {phase: 2}` ile Faz 2'nin uygulama PR'larını bekliyor. Karar 1'in katalog bağı bunu makinece zorlar.
5. **Plan §6.1 Faz 2'nin "kimlik belgesi yükleme (NFC'siz)" kalemi** → Karar 5: çip yolu ve çip meydan okuması Faz 2b'ye, kendi ADR'siyle.
6. **Plan §6.1 Faz 2'nin çıkış kriterindeki `owner` tipi** → `owner` bir model tipi değildir: plan **§5.4'ün "Modelin dışında kalanlar" maddesi** onu açıkça modelin dışına, `Scope`/`SelfUserID`'ye koyuyor. Kriter bu yüzden planın kendi §5.4'üyle çelişiyor. Modeli `owner` için genişletmek yanlış olurdu: kullanıcı-sahipli her satır için tuple yazmak ADR-0024'ün "yetki kararı veriden türetilir" tasarımını tersine çevirir. Kriterin karşılığı şudur: modelin sahiplik temsili `private_file#owner`'dır ve onun 4 satırı iki motorda yeşildir; kalan sahiplik kararları (`bank_account`, `education`, `device`, `session`, `address_search_session`, `media`…) `go_only` kalır ve Go değerlendiricisinin dışında, store'un `Scope`'unda sınanır. Kriter metninin düzeltilmesi Yusuf'a bildirildi.
7. **Plan §6.1'in eğitim kayıtlarını hiçbir faza atamaması** → `/users/me/educations` dört ucu Faz 2'ye alındı (Karar 11'in kapsam defteri). Eğitim kaydı profilin alt kaynağıdır; planın boşluğu Yusuf'a bildirildi.

ADR-0013, ADR-0014, ADR-0019, ADR-0024, ADR-0026 ve ADR-0028'in kararları değişmiyor. ADR-0019 §4'ün "dosya yüklemeleri API'den geçmez" kararı Karar 6'da karantina öneği ve tamamlama adımıyla **uygulanıyor**, değiştirilmiyor.

## Sonuçlar

- Sözleşme bugün kamuya açık: plan §4.2'nin istemci işi (web → admin → mobil) Faz 2'nin uygulamasını beklemeden tip üretimine başlayabilir. Hangi ucun canlı olmadığı spec'te `x-not-implemented` ile okunur.
- Uygulanmamış operasyon `501` döner. Bu, "404 ROUTE_NOT_FOUND" ile karıştırılmaz: yol **vardır**, sözleşmesi bağlayıcıdır, yalnız handler'ı yoktur.
- Bir operasyonu uygulamanın yolu uzadı: işareti kaldır → iskeleyi sil → `x-resource`'un tablosunu ve fikstürünü getir → alanın P0 senaryolarını bağla. **Kapıların granülerliği operasyon değil, şu üç şeydir** ve bunu olduğu gibi yazmak önemli:
  - `TestNotImplementedOperationsAreDeclared` ve `TestPendingServerServesExactlyItsOperations` **operasyon başına** bağlar: işaret, liste satırı ve iskele metodu üçü birlikte gider ya da birlikte kalır.
  - `ReadTest` ve `JoinTest` yalnız **`x-resource` taşıyan GET'lerde** silahlanır. 71 donmuş operasyonun 43'ünün `x-resource`'u yok (mutating uçlar), yani onlarda okuma testi hiçbir şey zorlamaz.
  - `TestFrozenDomainsDeferTheirScenarios` **alan başına** bağlar, operasyon başına değil: `company`'nin 19 ucundan 18'ini uygulamak 23 CMP senaryosunun hiçbirini bağlamaya zorlamaz. Granülerlik bilinçlidir (senaryo API'den bağımsız yazılır ve operasyon adlandırmaz, §1), ama "her uç kendi senaryosunu zorlar" demek **yanlış** olur.
- Kalan iki sessiz yol, kabul edilmiş sınır olarak: (a) bir operasyonun işaretini ve liste satırını kaldırıp iskele metodunu silmeyi unutmak — `TestPendingServerServesExactlyItsOperations` sayıyı yakalar, ama aynı turda başka bir operasyon donarsa sayı tutar ve `x-resource`'u olmayan mutating bir uç sonsuza kadar 501 döner; (b) `strictServer`'a iletme metodu yazarak iskeleyi gölgelemek — `TestFrozenOperationsAreNotShadowed` bunu kapatır.
- `server.gen.go` bu PR'da büyük ölçüde büyüyor. Dosya **üretilmiştir**, elle düzenlenmez ve birebirliğini `make gen-check` kanıtlar; incelemenin odağı spec, senaryo katalogu, authz ve bu ADR'dir.
- Bu PR'da kalanlar:
  - Faz 2'nin migration'ları, `db/softdelete.yaml` sınıflandırmaları, sqlc sorguları ve fikstürleri. `x-resource`'un tabloları o PR'da gerçek olur.
  - `internal/media`'nın karantina → doğrula → striple → taşı River işi ve `FuzzStorageKey` (Karar 6). Çıkış kriterinin yol geçişi fuzz'ı o PR'da yeşil olur.
  - `incoming/` ve `pending_registrations`'ın temizlik işleri (M2 saklama süreleri).
  - Onaylanmamış şirketin ilan yayınlayamaması (Karar 7) Faz 3'ün ilan ucunda uygulanır; bu sözleşmede ilan ucu yok.
  - ADR-0027'den devredilen `Keeps` kalemi: ilk gerçek "ebeveyni silinmiş çocuk listede kalır" ucu Faz 2'nin veri PR'ıyla gelir.
  - `softdeletetest` iki yol parametreli GET'i desteklemiyor ("operations with 2 path parameters are not supported yet"). Sözleşmede böyle bir uç var: `GET /v1/admin/identity-verifications/{reviewId}/documents/{documentId}`. Bugün `x-not-implemented` taşıdığı için atlanıyor; handler'ı getiren PR önce `ReadTest`'i genişletmek zorundadır.
  - İlk gerçek PATCH operasyonunun uçtan uca doğrulama ve bağlama testi (stil rehberi §3): `PATCH /v1/me/profile` ve `PATCH /v1/companies/{companyId}` sözleşmede var ama handler'ları yok, bu yüzden test onların uygulama PR'ında gelir.
  - **Profil senkronu giden kaydı (outbox) ve River gönderim döngüsü** (USR-022, USR-023). Plan §6.1 Iyzico istemcisini Faz 6'ya koyuyor, ama giden kaydın **yazımı** profil değişikliğiyle aynı transaction'da olmak zorunda, yani Faz 2'nin profil PR'ına ait; gönderim döngüsü ve sağlayıcı çağrısı Faz 6'da bağlanır. İki senaryo bu yüzden `deferred: phase 2`.
  - `pii.knownFields()` `birth_date` taşımıyor; ADR Karar 9 doğum tarihini PII projeksiyonuna koyuyor. `Gate.Open` bilinmeyen alanda fail-closed olduğu için bugün bir sızıntı yok, ama profil PR'ı listeyi büyütmek zorundadır.
  - `StaffUserDetail` maskeli kimlik numarası alanı taşımıyor. Karar 9 maskeyi sayıyor; yanıta alan eklemek uyumlu bir değişiklik olduğu için uygulama PR'ında eklenebilir.
  - **Üretilmiş okuma testinin bugünkü kapsamı:** 71 donmuş operasyonun 28'i `x-resource` taşıyor ve 17 ayrı tabloyu adlandırıyor; hepsi atlanıyor. `JoinTest` tarafında atlanan `detach` kenarı **yok** (bastırılan tek şey iki yol parametreli ucun global problemiydi). Sayı burada duruyor ki atlamanın büyüklüğü sessiz kalmasın.

## Açık sorular

1. **Onaylanmamış şirket ilan yayınlayabilir mi?** Karar 7 "yayınlayamaz" diyor ve sektör standardı bu, ama Java'da kontrol hiç yok: bugün `PENDING` bir şirket ilan açabiliyor, davet gönderebiliyor, takip edilebiliyor. Bu bir **ürün kararıdır** ve Yusuf'un onayını bekler. "Yayınlayabilir" seçilirse Karar 7'nin o maddesi düşer, geri kalanı durur.
2. **Şirket üyesinin rolü değiştirilebilsin mi?** Java'da operasyon yok: rol davet anında donuyor ve yalnız çıkarıp yeniden davet ederek değişiyor. Sözleşmeye `PATCH …/contacts/{userId}` eklemek küçük bir iştir ama K17'nin "v1'de ADMIN/STAFF" kararının ötesinde bir ürün kararı; sorulmadan eklenmedi.
3. **Davetin 14 günü ve iptali** Java'da olmayan iki yeni davranış. Sektör standardı oldukları için karar verildi, ama süre (14 gün) bir ürün tercihidir.
4. **Google Maps Platform önbellekleme kuralının madde numarası.** Karar 8'in dayanağı Google'ın yayımlanmış politikasıdır ve esası (yer kimliği süresiz, diğer içerik sınırlı süre) doğrulandı; ayrıntılı kuralın Places'in Service Specific Terms'indeki madde numarası doğrulanamadı. **Hukukçu teyidi bekliyor.** Teyit gelene kadar karar değişmiyor: `formatted_address` ve ham yanıt saklanmıyor.
