# ADR-0036: Profil, eğitim kayıtları ve profil senkronu giden kaydı

- Durum: Kabul edildi
- Tarih: 22.11.2026
- Karar kaydı: K10, K14, K19, S4
- Kaynak: [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0020](0020-sqlc-toolchain-and-soft-delete-platform.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) Karar 9, [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 3, 8, 9, 11 ve *Sonuçlar*, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 5, [ADR-0033](0033-email-and-phone-verification-and-sms.md) Karar 1b, 3, 4, 5 ve 8, [ADR-0034](0034-idempotency-key-store.md); Java `origin/main` (0cbbce69): `UserManager#patchCurrentUser`, `EducationManager`, `AcademicReferenceManager`, `University`/`Faculty`/`Department`/`Education` varlıkları, `SaveEducationDto`, `EducationMessages`, `db/changelog/data/{universities,faculties,departments}.csv`, `UserProfileSyncManager`, `UserProfileSyncOutboxAtomicityIT`, `UserProfileSyncDispatchIT` (okundu, koşturulmadı)
- Dış kaynaklar: RFC 7396 (JSON Merge Patch), OWASP ASVS 4.0.3 V3.7.1 (hassas işlemden önce yeniden kimlik doğrulama), JSON Schema 2020-12 `dependentRequired`, "transactional outbox" kalıbı
- Kod: `internal/users` (`domain.go`, `service.go`, `educations.go`, `http.go`), `internal/profilesync`, `internal/media` (`KindOf`, `Bind` metodu, `retire`'da benimseme), `internal/authn/verifications.go` (`Reauthenticate`, `StartTx`, `CheckTarget`, onayda giden kaydı), `internal/authn/http.go` (`verificationStartKeys`), `internal/platform/httpx/jsonschema2020.go` (`dependentRequired`), `internal/app/users.go`, `db/migrations/0046_profile_and_educations.sql`, `db/queries/users`, `db/queries/profilesync`

## Bağlam

Faz 2'nin üçüncü handler PR'ı profilin ve eğitim kayıtlarının altı operasyonunu getirir: `getMyProfile`, `updateMyProfile`, `listMyEducations`, `createMyEducation`, `replaceMyEducation`, `deleteMyEducation`. Davranış ADR-0029'da (Karar 3 son paragrafı, Karar 11'in kapsam defteri) ve ADR-0033'ün profil PR'ına bıraktığı yükümlülüklerde (Karar 1b, Karar 3, Karar 8) yazılıdır.

Bu PR'ın en büyük riski deponun baskın kusur sınıfıdır: sonradan eklenen bir yol doğru bir kuralı sessizce geçersiz kılar. Profil yaması e-posta ya da telefon değişikliği için doğrulama başlattığı için, `startMyVerification`'ın parola, bütçe, kova ve kilit kurallarını atlatan **ikinci bir kapı** olabilirdi. Kararların çoğu bunu yapısal olarak imkânsız kılmak içindir.

## Karar

### 1. Doğrulama başlatmanın tek kapısı

ADR-0033'te `Verifications.Start` hem parolayı doğruluyor hem başlatmanın kurallarını kendi transaction'ında uyguluyordu. Profil yaması alanları ve başlattığı doğrulamaları **tek transaction'da** yazmak zorunda (Karar 9); bu yüzden `Start` ikiye bölündü ve iki yüzey de aynı iki parçayı kullanır:

- **`Reauthenticate(p, currentPassword)`**: `confirmPassword`'ün kendisi (uygulama içi parola değişimiyle ortak). Transaction dışında koşar (argon2 kilit tutmaz), yanlış parolayı hesabın giriş hedefinde `FAILED` sayar, kilitli hedefi incelemeden reddeder. Dönen `Reauthentication`'ın dışa açık alanı yoktur; yalnız bu metot kurar. Kanıtın zamanını taşır ve **bir dakika** (`ReauthenticationTTL`) geçerlidir: bir istek en çok `httpx.RequestTimeout` (25 sn) yaşar, daha eski bir kanıt onu kullanan istekten gelmemiştir. Bayat kanıt yanlış parola gibi `403 PASSWORD_MISMATCH` alır, çünkü çare aynıdır: parola yeniden girilir (`TestStartTxRechecksThePassword`).
- **`StartTx(tx, p, r, channel, raw, pointer)`**: başlatmanın bütün kuralları, çağıranın transaction'ında: hedef kuralı (`targetAt`), kanıtın tazeliği, kullanıcının satır kilidi altında parolanın değişmediği (`TestStartTxRechecksThePassword`: kanıttan sonra değişen parola reddedilir, satır ve iş yazılmaz), hedefin başlatma kilidi, yeniden gönderme bütçesi (bekleme, hedefin saatlik bütçesi, kullanıcının günlük tavanı), hedef başına tek açık doğrulama, `SUCCEEDED` denemesi ve kod işinin aynı transaction'da kuyruğa girmesi. Ret hata olarak döner ve çağıranın transaction'ı geri alınır.
- `Start` artık yalnız `Reauthenticate` + kendi transaction'ında `StartTx`'tir. Profil yaması (`users.Service.Update`) aynı ikisini çağırır. Kural ikinci bir yerde yazılmadı; `users` paketi doğrulama tablolarına, bütçe sorgularına ya da kilitlere hiç dokunmaz.
- `authn.CheckTarget(channel, raw, pointer)` hedef kuralını dışa açar: yama, parolayı sormadan önce e-postayı ve numarayı aynı kuralla biçim denetiminden geçirir ve hesaptaki değerle karşılaştıracağı saklanan biçimi alır. Ret `REQUEST_INVALID`'dir ve yamanın üyesini adlandırır (`/email`, `/phoneNumber`).

### 2. Profil yamasında yeniden kimlik doğrulama (ADR-0033 Karar 1b)

- Yamada `email` ya da `phoneNumber` **varsa** `currentPassword` zorunludur. Sözleşmede `UserProfilePatch`'e yazılmaz bir `currentPassword` üyesi ve `dependentRequired: {email: [currentPassword], phoneNumber: [currentPassword]}` eklendi (uyumlu değişiklik: yeni, isteğe bağlı bir istek üyesi). Eksikse `400 REQUEST_INVALID`, `/currentPassword` `FIELD_REQUIRED`.
- `httpx`'in JSON Schema 2020-12 yolu `dependentRequired` ihlalini önceden kök göstericide `FIELD_VALUE_INVALID` diye bildiriyordu; artık eksik üyeyi `FIELD_REQUIRED` ile adlandırır (`required` gibi). `TestFields2020` bunu bağlar.
- **Değer hesaptakiyle aynı olsa da** parola istenir ve denetlenir. Kural "alan varsa" diye yazıldı (ADR-0033 Karar 1b'nin sözcüğü): denetimin sonucu gönderilen değerin hesaptakiyle aynı olup olmamasına bağlı değildir ve kural tek cümledir. Bedeli: profilin tamamını her kaydetmede gönderen bir istemci her seferinde parola ister; istemci yalnız değişen üyeyi göndermelidir (RFC 7396'nın amacı). Açık soru 2.
- Sıra: biçim (`REQUEST_INVALID`, hedef kuralı dahil) → parola (`403 PASSWORD_MISMATCH`, kilit) → transaction (kimlik dondurma, bütçe). Biçimi hatalı bir yama deneme harcamaz.
- E-posta ya da telefon taşımayan yama parola istemez.

### 3. Oran sınırı kovaları paylaşılır

`authn.StrictMiddleware` `updateMyProfile`'ı tanır: yamadaki her e-posta ya da telefon için **bir başlatmanın jetonlarını, başlatmanın kovalarından** alır. Anahtarların tek üreticisi `verificationStartKeys`'tir ve `startMyVerification` da onu kullanır; bu yüzden iki yüzeyin kovaları yapısal olarak aynıdır: çağıranın kovası (`verification:user:<id>`), adresin saatlik bütçesi (`verification_address`), (hedef, adres) kovası. İki hedefli bir yama iki başlatma sayılır. Sıra ADR-0033 Karar 4'ünkidir (önce çağıranlar, sonra adres, sonra hedefler). E-posta ya da telefon taşımayan yama bu kovalardan jeton almaz.

Veritabanı bütçesi (bekleme, hedef bütçesi, günlük tavan) `StartTx`'te olduğu için zaten ortaktır; bekleme ret'i yamada da `Retry-After` taşır (orta katman hatanın `RetryAfter`'ını okur).

### 4. Kendi verisini okuma: PII kapısı yok, denetim kaydı yok

ADR-0026 Karar 9'un PII kapısı **personelin başkasının** kişisel verisine erişimi içindir: `pii.Gate.Open(ctx, q, staff, dataSubject, …)` bir personel kimliği ister ve erişimi `pii_access_log`'a o personel adına yazar. Kullanıcının kendi profilini okuması bir üçüncü kişi erişimi değildir (KVKK m.11 ilgili kişinin kendi verisine erişim hakkı); `GET /v1/me` bu PR'dan önce de e-postayı açık döndürüyordu. Bu yüzden:

- `getMyProfile` ve `updateMyProfile`'ın yanıtı `pii.Reveal` kullanmaz, `pii_access_log` yazmaz. `TestRevealOnlyInProjections` değişmedi.
- Maskeler USR-027'nin ve sözleşmenin dediği gibidir: kimlik numarası doğrulanmışsa tümden maskelidir (`***********`; maske numaraya hiç bakmaz), doğrulanmamışsa `null`; telefon kısmen maskelidir ve doğrulamanın maskesiyle aynıdır (`+90******4567`, `authn.MaskPhone`). E-posta açık döner.
- ADR-0029 *Sonuçlar*'ın istediği `pii.FieldBirthDate` (`birth_date`) `knownFields`'e eklendi: personelin kullanıcı ayrıntısı projeksiyonu geldiğinde doğum tarihini açabilecek alan budur.

### 5. Sonraki modüllere bağlar: kolon yok, 501, tetik testi

Profilin dört alanı karşılığı olmayan tablolara bağlanır: `addressId` (adres), `maskedIban` (banka hesabı, Faz 6), `companies` (şirket üyelikleri). `profilePictureId` ve `cvId` ilk sürümde de bu listedeydi; medya modülü (ADR-0035, #27) birleşince Karar 10'la bağlandılar.

- `users` tablosuna bu bağların kolonu **eklenmedi**. FK'siz bir kolon bütünlüğü olmayan bir değer olurdu; kolonu ve FK'yi hedef tabloyu getiren PR ekler.
- `getMyProfile` bu alanları `null` (şirketler boş liste) döndürür.
- `updateMyProfile`'da `addressId`'ye bir kimlik göndermek `501 NOT_IMPLEMENTED` alır (ADR-0029 Karar 1'in evrensel kodu, `default` yanıtı); `null` bir şey değiştirmez. 404 ("böyle bir adres yok") **seçilmedi**: tablo geldiği gün gerçek bir kayda "yok" diyen sessiz bir yalana dönüşürdü, deponun baskın kusur sınıfı. Kalıp stil rehberi §11.1'e "alan düzeyinde 501" olarak yazıldı.
- `ADDRESS_NOT_FOUND` `updateMyProfile`'ın listesinde durmaz: bugün hiçbir yerden dönmez ve bağ denetimi (ADR-0032 Karar 1) dönmeyen liste üyesini kırmızı sayar. `404` yanıtı sözleşmede kalır; kod bağı getiren PR'la listeye döner.
- **Tetik testi** `users.TestProfileLinksWaitForTheirTables`: soft-delete kataloğunda `addresses`, `companies`, `bank_accounts` ya da `identity_verifications` tablosu belirdiği gün kırmızıdır ve hangi profil alanının bağlanacağını ya da hangi senkron yükümlülüğünün (Karar 8) doğduğunu yazar. Medya PR'ı birleştiğinde bu testin `media` satırı tam olarak bunu yaptı ve bağ bu PR'da kuruldu (Karar 10).

### 6. Eğitim kayıtları

- **Sahiplik:** kayıt `(id, user_id)` ile bulunur ve kilitlenir. Başkasının kaydı hiç olmamış bir kaydın `404 EDUCATION_NOT_FOUND` gövdesini bayt bayt alır (ADR-0014). Değiştirmede sıra dönem kuralı → kayıt → referans: başkasının kaydı için referans denetimi hiç koşmaz, gövde ne olursa olsun yanıt aynıdır.
- **Kullanıcı başına sınır: 20 kayıt** (`EducationsPerUser`). Java'da sınır yok; LinkedIn benzeri profillerde birkaç kayıt olağandır ve sınır bir hesabın yazabileceğini ve listenin yükünü bağlar. Aşımı yeni `409 EDUCATION_LIMIT_REACHED` (`COMPANY_GALLERY_LIMIT_REACHED`'in kalıbı). Sayım ve ekleme kullanıcının eğitim yazma kilidi altında olur (`pg_advisory_xact_lock("KE", kullanıcı kimliğinin 12–15. baytları)`): eşzamanlı eklemeler sınırı birlikte aşamaz (`TestEducationLimitHoldsUnderConcurrency`). Anahtar UUIDv7'nin rastgele kuyruğundan alınır: ilk baytlar oluşturma zamanıdır ve aynı dakikada açılan hesaplar onları paylaşır (`TestEducationLockKeysOnTheRandomBytes`).
- **Ebeveyn:** kayıt `users`'ın çocuğudur ve kullanıcıyla birlikte silinir (`cascade`). Ekleme kullanıcının etkin satırını `softdelete.LockActiveParent` ile kilitler; `ErrNotActive` oturumun kendi koduna (`401 AUTH_SESSION_NOT_FOUND_OR_INACTIVE`) çevrilir, çünkü ebeveyn çağıranın kendisidir (`TestEducationNeedsAnActiveUser`). Kilit sırası: eğitim yazma kilidi → kullanıcı satırı (`FOR SHARE`). Kullanıcı satırını `FOR UPDATE` kilitleyen yollar (başlatma, profil yaması) eğitim kilidini hiç almaz; döngü kurulamaz.
- **Silme** `softdelete.Delete` ile yapılır. Eğitime bugün `restrict` kenarıyla bağlı çocuk yok, ama `Delete`'in `409 SOFT_DELETE_RESTRICTED` yeri izlemede ulaşılabilir; bu yüzden kod `deleteMyEducation`'ın listesine ve `409` yanıtı sözleşmeye eklendi. Böyle bir kenar eklendiği gün yanıt zaten sözleşmededir.
- **Liste:** süren eğitim (bitiş yılı boş) başta, sonra bitiş yılı, başlangıç yılı ve kimlik, hepsi azalan. Bir kullanıcının kaydı en çok 20 olduğu için hepsi okunur ve sayfa serviste aynı keyset üzerinde kesilir; cursor sorgudakiyle aynı sıralama anahtarını taşır (`edu1|yön|bitiş|başlangıç|kimlik`, base64url). Bozuk cursor `400 CURSOR_INVALID` (`listMyEducations`'ın listesine eklendi, ADR-0033 Karar 13'ün kalıbı).
- **Lise derecesi yok:** sözleşmenin `EducationDegree`'sinden `HIGH_SCHOOL` çıkarıldı (ürün kararı, 22.11.2026). Kayıt zorunlu bir `universityId` ile yükseköğretim kurumuna bağlanır ve YÖK verisinde lise yoktur; değer hiçbir geçerli kayıtta kullanılamazdı. Uç bir istemcide yok, bu yüzden en ucuz an şimdiydi. `ASSOCIATE` kalır: önlisans programları üniversite altındadır. Önlisans kaydı meslek yüksekokullarının referans verisinde (fakülte düzeyinde) bulunmasını ister; Java'nın CSV'sinin onları eksiksiz taşıyıp taşımadığı bu PR'da denetlenmedi ve referans PR'ına devredildi (Karar 7).
- **Bölüm fakülte ister:** sözleşme fakülteyi ve bölümü isteğe bağlı tutuyor (Java üçünü de zorunlu tutuyordu). Bölüm bir fakültenin altında seçilir: `EducationInput`'a `dependentRequired: {departmentId: [facultyId]}` eklendi; veritabanında `CHECK` ve iki bileşik FK (`(faculty_id, university_id)` → `faculties`, `(department_id, faculty_id)` → `departments`) aynı kuralı tutar. Başka üniversitenin fakültesi ve başka fakültenin bölümü `404 REFERENCE_ITEM_NOT_FOUND` alır (Java'nın `FACULTY_NOT_IN_UNIVERSITY`/`DEPARTMENT_NOT_IN_FACULTY` ayrımı tek koda indi: ikisi de "bu üniversitede böyle bir fakülte yok"tur).

### 7. Referans verisi: tablolar bu PR'da, tohum referans PR'ında

Eğitim kaydı üniversite, fakülte ve bölüme FK ile bağlanır (ADR-0029 Karar 8: referans verisi M4); tablolar yoktu ve referans uçları `501`.

- **Tablolar bu PR'da:** `universities`, `faculties`, `departments`; M4, uygulamaya yalnız `SELECT`; satır silinmez, `retired_at` ile emekliye ayrılır (ADR-0013'ün M4 tanımı). **Emekli satır yeni seçilemez, ama kaydın zaten taşıdığı emekli satır kalır** (ürün kararı, 22.11.2026): ekleme ve başka bir referansa geçen değiştirme `404 REFERENCE_ITEM_NOT_FOUND` alır; referansı aynı bırakan değiştirme kabul edilir (`TestEducationRetiredReferences`). Sorgu emekli satırı yalnız kaydın kilit altında okunan kendi referansıysa geçirir (`allow_retired`). Kayıtların FK'si için gerekli olan budur.
- **Tohum ve uçlar referans PR'ında:** Java'nın verisi `db/changelog/data/*.csv` (223 üniversite, 2.326 fakülte, 11.250 bölüm; 2025-08 tarihli bir YÖK anlık görüntüsü). Tohumu bu PR'a koymak işe yaramazdı: istemci kimlikleri ancak `/v1/reference` uçlarından öğrenir ve onlar `501`. Verinin kaynağı ve güncelleme politikası (YÖK Atlas, yıllık yenileme, emekliye ayırma) ve meslek yüksekokullarının (önlisans) eksiksiz gelmesi uçlarla birlikte karar verilecek konulardır. O PR gelene kadar `createMyEducation` her kimlikte `REFERENCE_ITEM_NOT_FOUND` döner; prod yok, gerçek kullanıcı yok.
- **Serbest metin seçilmedi:** sözleşme kimlik ve `ReferenceItem` istiyor (ADR-0029 donmuş sözleşmesi) ve serbest metin aynı üniversitenin onlarca yazımını üretirdi.

### 8. Profil senkronu giden kaydı (USR-022)

ADR-0029 *Sonuçlar* ve ADR-0033 Karar 8 giden kaydın **yazımını** profil PR'ına, gönderim döngüsünü ve sağlayıcı çağrısını Faz 6'ya bırakır. Java her profil değişikliğinden sonra kullanıcıyı ödeme sağlayıcısına yayımlıyor (`UserProfileSyncManager#enqueue`, transactional outbox).

- **`user_profile_sync_intents`** (M2, `pii`): kullanıcı başına **bir açık niyet** (`published_at IS NULL` üzerinde kısmi tekil indeks). Profil değişikliği kendi transaction'ında niyeti yazar ya da açık olanı tazeler (`INSERT … ON CONFLICT … DO UPDATE`): zamanı ve son değişikliğin kaynağı (istek kimliği, oturum, cihaz). Değişiklik ve niyet birlikte işlenir ya da hiçbiri (`TestProfilePatchSharesTheStartBudget` geri alma yarısını, `TestProfileChangeRecordsTheSyncIntent` işlenme ve tazeleme yarısını bağlar).
- **Yük yok:** Java niyete profilin JSON'unu yazıyor. Go yazmaz: gönderici profili **yayın anında** okur. Niyet bu yüzden bayatlamaz ("yükün yerinde güncellenmesi" niyetin tazelenmesine iner) ve kendine ait kişisel veri tutmaz; tabloda yalnız kim ve nereden olduğu durur. USR-022'nin `target`'ı bunu yazar.
- **Yazanlar:** profil yaması (saklanan profil alanlarından biri değiştiyse; yalnız doğrulama başlatan ya da hiçbir şey değiştirmeyen yama niyet yazmaz) ve doğrulama onayı (e-posta ya da numara değiştiyse; aynı değerin yeniden onayı yazmaz). Numara devralındıysa eski sahibin profili de değişir ve onun niyeti de yazılır, ama **sistem kaynağıyla**: oturum ve cihaz `NULL`, yalnız numarayı taşıyan isteğin kimliği (ürün kararı, 22.11.2026). Değişikliği eski sahibin oturumu ve cihazı yapmadı; yeni sahibinkileri onun niyetine yazmak onları başkasının hesabına bağlardı (`TestConfirmationRecordsTheSyncIntents`, `TestProfileEmailChangeWaitsForConfirmation`).
- **Henüz yazmayanlar:** Java iki yerde daha yayımlıyor: kimlik doğrulama onayı (`UserManager.java:393`, `IdentityVerificationManager.java:151`) ve birincil adres güncellemesi (`UserManager.java:536`). İkisinin de tablosu yok; kimlik ve adres PR'larının yükümlülüğüdür. `TestProfileLinksWaitForTheirTables`, katalogda `identity_verifications` ya da `addresses` belirdiği gün kırmızıdır ve `profilesync.Record` yükümlülüğünü yazar.
- **River işi değil:** CLAUDE.md garantili yan etkiyi River `InsertTx`'e koyar. Burada tüketici Faz 6'ya kadar yoktur: işçisi olmayan bir iş River'da hata alıp yeniden denenir ve atılır (niyet kaybolur); hiçbir şey yapmayan bir işçi niyeti sessizce yutar. Ayrıca River'ın tekil işleri `running` durumunu kapsamak zorunda: yayın sürerken gelen ikinci değişiklik atlanırdı. Kalıcı kayıt bu yüzden bir tablodur: planın K3 satırının yedeği olan "elle yazılmış outbox" (K3-c) bu tek tüketici için seçildi, River (K3-b) değişmedi. Faz 6'nın göndericisi niyeti sahiplenir, yayımlar ve isterse River'a taşır.
- **Uyarılar:** ADR-0021 `KoalaUserProfileSyncBacklog` ve `…OldestAge`'in karşılığını "River kuyruk birikimi" diye yazıyor. Karşılık artık bu tablodur: açık niyetlerin sayısı ve en eskinin yaşı. Metrik ve kural **Faz 6'nın göndericisiyle** gelir: gönderici yokken her niyet tasarım gereği açık kalır ve bugün yazılacak bir birikim kuralı ilk profil değişikliğinden bir saat sonra çalar ve hiç susmazdı; hep çalan bir uyarı uyarıları görmezden gelmeyi öğretir.
- **USR-023** (iki işçi aynı niyeti iki kez yayımlayamaz) gönderici olmadan sınanamaz: `deferred: phase 6`. Katalogdaki eski gerekçe ("gönderim döngüsü Faz 2'nin uygulama PR'ında") ADR-0029 *Sonuçlar*'ıyla çelişiyordu; ADR'nin dediği geçerlidir.

### 9. Yama atomiktir

Yama tek transaction'dır: kullanıcının satırı `FOR UPDATE` kilitlenir (ADR-0033 Karar 5'in sırasının ilk adımı; `StartTx` aynı satırı aynı transaction'da yeniden kilitler), kimlik dondurma (USR-024) kilit altında okunur, alanlar ve giden kaydı yazılır, sonra her farklı e-posta ve numara için `StartTx` koşar. Herhangi birinin reddi (bütçe, değişmiş parola) bütün yamayı geri alır: "ad değişti ama numara başlamadı" diye yarım bir sonuç yoktur. Aynı değer (hesaptaki e-posta ya da numara) hiçbir şey başlatmaz (USR-014).

Kimlik doğrulanmışsa ad, soyad ve doğum tarihi **değişemez**; hesaptaki değerin aynısını göndermek değişiklik değildir ve kabul edilir (profili bütün olarak gönderen istemci kırılmasın). Cinsiyet donmaz (USR-029). Ad ve soyad kırpılır, boş kalamaz; metin alanı boş ya da `null` gönderilirse temizlenir; doğum tarihi gelecekte ya da 1900'den önce olamaz.

### 10. Profilin medya bağları

`users.profile_picture_id` ve `users.cv_id` medyaya (0045) FK'dir; katalogda ikisi de **`restrict`** kenarıdır (`TestMediaConsumersAreRestrictEdges`, ADR-0035 Karar 7). Bağ ADR-0035 Karar 8'in sözleşmesine göre kurulur ve yamanın transaction'ındadır (Karar 9), kullanıcının satır kilidi altında:

1. **Denetim:** yeni adlandırılan medya `media.Service.KindOf` ile kilitlenir ve türü okunur: başkasının ya da olmayan medya `MEDIA_NOT_FOUND`. Türü alana uymayan medya (`profilePictureId` için `PROFILE_PICTURE`, `cvId` için `CV`) **`400 REQUEST_INVALID`, üyeyi adlandıran `FIELD_VALUE_INVALID`** alır. Gerekçe: medya vardır ve çağıranındır, yanlış olan gönderilen değerin bu alana uygunluğudur; bu bir biçim hatasıdır, durum çakışması değil (409) ve yeni bir kod gerektirmez; gösterici istemciye hangi alanın yanlış olduğunu söyler (stil rehberi §5).
2. **Bağlama:** `media.Bind` (`bound_at`'in tek yazıcısı) aynı transaction'da: `READY` olmayan medya `MEDIA_NOT_READY`. Bağlanan medya süpürülmez. `Bind`'ın güncellemesi medya satırını kilitler ve yalnız etkin satırı bulur; ADR-0035 Karar 8'e göre ayrıca `LockActiveParent(media)` gerekmez.
3. **Yazım:** kullanıcının satırı yeni medyayı gösterir (ve giden kaydı yazılır, Karar 8).
4. **Bırakma:** artık gösterilmeyen eski medya (yerine yenisi geldi ya da `null` ile kaldırıldı) `media.Service.Release` ile aynı transaction'da soft-delete edilir ve nesnelerinin silinmesi kuyruğa girer. Sıra ADR-0035 Karar 8'inkidir: önce satır yeni medyaya döner, sonra eskisi bırakılır; ters sırada platform `SOFT_DELETE_RESTRICTED` döner.

Bağlı bir medyanın bırakılması ya da sahibince silinmesi `409 SOFT_DELETE_RESTRICTED` alır. ADR-0035 Karar 8 bunu `TestRelease`'e bağlıyor, ama o test bu durumu koşmuyor; gerçek tüketiciyle koşan test `TestProfileMediaLinks`'tir (`Release` ve `DELETE /v1/me/media/{id}` ikisi de 409). Bu yüzden `updateMyProfile`'ın listesinde `MEDIA_NOT_FOUND`, `MEDIA_NOT_READY` ve `SOFT_DELETE_RESTRICTED` vardır; sonuncusu bugün profil dışında bir tüketici olmadığı için yalnız teoriktir, ama izlemede `Release` → `Delete` yolundan ulaşılabilirdir.

**Hesabın silinmesi:** `media.owner_id → users` kenarı `cascade`, `users.profile_picture_id/cv_id → media` kenarları `restrict`'tir. Hesap silinirken platform önce kullanıcıyı işaretler, kaskadla medyayı işaretler, sonra `restrict` kenarlarını denetler; kullanıcı aynı silmede işaretlendiği için etkin bir tüketici bulunmaz ve silme engellenmez (`TestAccountDeletionTakesItsProfileMedia`). Geri alma (`softdelete.Restore`) bu ikili kenarla denenmedi.

**Medya modülünde, ilk tüketici olarak kapatılan iki not** (ADR-0035 değişmedi, davranışları burada kayda geçer):

- **Benimseme artık aynı transaction'da.** Süpürme, tüketicisi `Bind` çağırmamış bir `READY` medyayı silmeye çalışıp `SOFT_DELETE_RESTRICTED` alınca onu benimser (bağlar). İlk sürümde benimseme, silme transaction'ı geri alındıktan sonra ayrı bir `BindMedia` ile koşuyordu: arada kilit bırakılıyordu ve o aralıkta tüketicisini kaybeden medya kalıcı olarak bağlı kalabiliyordu. Şimdi silme bir savepoint altında koşar, ret savepoint'e geri sarılır ve `BindMedia` aynı transaction'da, `LockSweepable`'ın aldığı medya satırı kilidi altında koşar (`media.Service.retire`). **Kalan pencere:** platformun `restrict` denetimi tüketici satırını kilitlemez; tüketicisini `Release` etmeden bırakan bir yol denetimle commit arasında yine bir medyayı bağlı bırakabilir. Profil eski medyayı her zaman `Release` eder ve `Release` medya satırının kilidini bekler, bu yüzden profil bu pencereye düşmez.
- **İşçinin `Keep`'i iletmesi teste bağlandı.** Testlerin kuyruktaki silme işlerini koşturan yardımcısı (`runDeleteJobs`) işçiyi atlayıp `Service.DeleteObjects`'i doğrudan çağırıyordu; artık kayıtlı işçinin `Work`'ünden geçiyor (`media.Service.WorkDeleteObjects`, yalnız testte). `TestRecordedObjectSurvivesAccountDeletion` böylece `Work`'ün `Keep`'i iletmesini de bağlar.

## "Profil yolundan atlatılabilir mi?"

| Kural (#23) | Profil yolu | Neden atlatılamaz | Test |
|---|---|---|---|
| Mevcut parola (Karar 1b) | `Reauthenticate` | Tek uygulama `confirmPassword`; yama `StartTx`'e ancak bir `Reauthentication` ile ulaşır ve onu yalnız `Reauthenticate` kurar | `TestProfileContactChangeNeedsThePassword` |
| Yanlış parolanın kilide sayılması | `confirmPassword` | Aynı fonksiyon `FAILED` yazar | aynı test (`FAILED` sayısı) |
| Kilitliyken ret | `lockState` | Aynı fonksiyon | aynı test (100 `FAILED` sonrası doğru parolaya 403) |
| Parolanın kilit altında değişmemesi | `StartTx` | Tek yer | `TestStartTxRechecksThePassword` (ADR-0033'te bu kuralı bağlayan test yoktu) |
| Kanıtın tazeliği (`ReauthenticationTTL`) | `StartTx` | Tek yer | `TestStartTxRechecksThePassword` |
| Hedef kuralı (ülke, biçim) | `CheckTarget` → `targetAt` | Tek yer | `TestProfileContactChangeNeedsThePassword` (`+44…` 400, deneme harcanmaz) |
| Bekleme ve hedef bütçesi | `StartTx` → `budgetWait` | Tek yer | `TestProfilePatchSharesTheStartBudget` |
| Günlük tavan | `StartTx` → `budgetWait` | Tek yer, iki yüzey birlikte sayılır | aynı test (9 yama + 1 başlatma, 11. ret) |
| Başlatma kilidi (eşzamanlılık) | `StartTx` | Tek yer | ADR-0033'ün `TestConcurrentStartsKeepTheTargetBudget`'ı aynı fonksiyonu sınar; yamadan eşzamanlı sınanmadı |
| HTTP kovaları | `verificationStartKeys` | Anahtarların tek üreticisi | `TestProfilePatchSharesTheStartBuckets` (telefon), `TestProfileEmailPatchTakesTheStartBuckets` (e-posta) |
| Küresel tavan, gönderim penceresi | Kod işi | Yama işi aynı `InsertTx` ile kuyruğa koyar | Denenmedi: iş yolu yüzeyden bağımsız |
| Numaralandırma (Karar 3) | — | Yama hedefin hesap durumuna bakmaz; yalnız `StartTx` | Kod incelemesi; listede `VERIFICATION_TARGET_IN_USE` yok, bağ denetimi bunu zorlar |

## Java paritesi

| Konu | Java | Go | Not |
|---|---|---|---|
| E-posta/telefon değişikliği | parola yok | `currentPassword` | ADR-0033 Karar 1b |
| Hedef başkasındaysa | yamada `EMAIL_ALREADY_EXISTS` (numaralandırma) | yanıt söylemez | ADR-0033 Karar 3 |
| Aynı değer | başlatmaz | başlatmaz | USR-014 |
| Kimlik dondurma | ad/soyad/doğum tarihi gönderilirse ret | yalnız değişirse ret | Karar 9 |
| Yama atomikliği | `@Transactional`: alanlar ve doğrulama birlikte | tek transaction, ret hepsini geri alır | aynı (Karar 9) |
| IBAN yamada | `iban` alanı banka hesabı yazar | sözleşmede yok; banka hesabı Faz 6 | ADR-0029 |
| Eğitim alanları | fakülte ve bölüm zorunlu, GPA, tarih | fakülte/bölüm isteğe bağlı, derece, yıl | ADR-0029'un donmuş sözleşmesi |
| Eğitim sınırı | yok | 20 | Karar 6 |
| Sahiplik ihlali | gizli 404 (`Concealment.notFound`) | gizli 404, bayt bayt | aynı |
| Referans tutarlılığı | serviste iki ayrı hata | tek kod, bileşik FK | Karar 6 |
| Giden kaydı yükü | profilin JSON'u | yok, yayında okunur | Karar 8 |
| Devirde eski sahibin yayını | çağıranın bağlamıyla | sistem kaynağı: yalnız istek kimliği | Karar 8 |
| Kimlik doğrulamada yayın | `UserManager.java:393`, `IdentityVerificationManager.java:151` | kimlik PR'ında, tetik testi | Karar 8 |
| Birincil adres değişikliğinde yayın | `UserManager.java:536` | adres PR'ında, tetik testi | Karar 8 |
| Lise derecesi | `GradeType` içinde | yok | Karar 6 |
| Profil fotoğrafı ve özgeçmiş | `PUT /users/me/profile-picture`, `PUT`/`DELETE /users/me/cv` | yükleme + yama (`profilePictureId`, `cvId`); eski medya aynı tx'te bırakılır | Karar 10 |

## Sözleşme değişiklikleri

Altı operasyonun `x-not-implemented` işareti kalktı, iskele metotları silindi. Uç hiçbir istemcide yok.

- `getMyProfile`: açıklama kendi verisini okumayı ve sonraki modüllerin bağlarını yazar.
- `updateMyProfile`: `x-error-codes: [USER_FIELD_IMMUTABLE, PASSWORD_MISMATCH, VERIFICATION_RESEND_TOO_SOON, MEDIA_NOT_FOUND, MEDIA_NOT_READY, SOFT_DELETE_RESTRICTED]` (ADR-0033 Karar 3: `VERIFICATION_TARGET_IN_USE` çıktı; Karar 5: `ADDRESS_NOT_FOUND` çıktı; Karar 10: medya kodları ve `SOFT_DELETE_RESTRICTED`); `403` yanıtı eklendi; açıklama yeniden kimlik doğrulamayı, ortak bütçeyi, atomikliği ve medya bağını yazar.
- `UserProfilePatch`: `currentPassword` ve `dependentRequired`.
- `listMyEducations`: `x-error-codes: [CURSOR_INVALID]`.
- `createMyEducation`: `EDUCATION_LIMIT_REACHED` listede; `201` `Idempotent-Replayed` başlığını tanımlar (ADR-0034, bağlı operasyon kuralı).
- `deleteMyEducation`: `SOFT_DELETE_RESTRICTED` listede, `409` yanıtı eklendi.
- `EducationInput`: `dependentRequired: {departmentId: [facultyId]}`.
- `EducationDegree`: `HIGH_SCHOOL` çıktı (Karar 6; uç hiçbir istemcide yok).
- `UserProfilePatch.currentPassword`: e-posta ya da telefon olmadan gönderilirse yok sayılır (denetlenmez, yazılmaz); açıklama bunu yazar, davranış değişmedi.
- `createMyEducation`, `replaceMyEducation`: açıklama emekli referans kuralını yazar.
- `ErrorCode`: `EDUCATION_LIMIT_REACHED` (409) eklendi; `PASSWORD_MISMATCH`'in düzyazısı profil yamasını anar.

## Yerine geçtiği

1. **`test/scenarios/user.yaml`'da USR-023'ün `deferred: phase 2` gerekçesi** → Karar 8: gönderim döngüsü Faz 6'dadır (ADR-0029 *Sonuçlar* ile hizalandı).
2. **ADR-0033 *Sonuçlar*'ındaki "USR-014 `deferred` kalıyor"** → USR-014 bağlandı (`boundAhead`: `updateMyProfile`, `confirmMyVerification`).
3. **ADR-0029 *Sonuçlar*'ındaki üç kalem** kapandı: profil senkronu giden kaydının yazımı (Karar 8), `pii.knownFields`'e `birth_date` (Karar 4), ilk gerçek PATCH operasyonunun uçtan uca doğrulama ve bağlama testi (`TestProfileOverHTTP`: `application/json` 415, `null` temizler, gönderilmeyen üye kalır).
4. **ADR-0033 Karar 1'in `Start` tanımı** → Karar 1: davranış aynı, kod `Reauthenticate` + `StartTx`'e bölündü; bir ret artık transaction'ı geri alır (önceden commit ediyordu, yazılmış bir şey yoktu). Kanıta bir dakikalık tazelik sınırı eklendi.
5. **ADR-0010 Karar 2 ve CLAUDE.md'nin "garantili yan etki River `InsertTx` ile" kuralı** → Karar 8: profil senkronu giden kaydı, tüketicisi Faz 6'ya kadar olmadığı için bir tablodur (plan K3-c, elle yazılmış outbox). İstisna tek tüketicilidir; CLAUDE.md satırı istisnayı yazar. Kuralın kendisi değişmez.
6. **ADR-0021'in uyarı tablosundaki `KoalaUserProfileSyncBacklog`/`…OldestAge` satırı** ("River kuyruk birikimi, River worker PR'ında") → Karar 8: karşılık `user_profile_sync_intents`'teki açık niyetler; metrik ve kural Faz 6'nın göndericisiyle gelir.

ADR-0010, ADR-0013, ADR-0014, ADR-0021, ADR-0026, ADR-0029 ve ADR-0033'ün kararları başka yerde değişmiyor.

## Sonuçlar

- Bağlanan P0'lar: USR-014, USR-022 (`boundAhead`). USR-023 `deferred: phase 6`. P1'lerden USR-024, USR-027 (IBAN yarısı `partial: phase 6`), USR-028 ve P2 USR-029 da teste bağlandı.
- `users` alanının kalan donmuş operasyonları (`getPublicProfile`, kimlik belgeleri, adresler, takip edilen şirketler, medya) `boundAhead` kuralını sürdürür.
- Adres, şirket ve banka hesabı PR'ları profil bağlarını (`addressId`, `companies`, `maskedIban`) getirmek zorundadır; tetik testi bunu zorlar (Karar 5). Medya bağı bu PR'da kuruldu (Karar 10).
- **Kimlik ve adres PR'larının senkron yükümlülüğü:** onaylanan kimlik doğrulaması ve birincil adres değişikliği kendi transaction'ında `profilesync.Record` çağırır (Java'nın `UserManager.java:393`, `:536`, `IdentityVerificationManager.java:151` yayınları). Tetik testi `identity_verifications` ve `addresses` tablolarında kırmızıdır.
- Referans PR'ı tabloları tohumlar ve uçları açar; o gün `createMyEducation` gerçek kimliklerle çalışır.
- Faz 6'nın göndericisi `user_profile_sync_intents`'i sahiplenir, yayımlar, saklama süresini getirir ve USR-023'ü bağlar.
- Bağ denetimi (ADR-0032 Karar 1), platformun elle yazılmış iki sorgusundaki `QueryRow(…).Scan` çağrısının tipini çözemiyor. Gerekçeli iki `unresolvedCalls` muafiyetini medya PR'ı getirdi; eğitim ve profil handler'ları aynı yollardan (`LockActiveParent`, `Delete`, `Release`) geçer ve aynı muafiyetleri kullanır.

## Açık sorular

1. **Eğitim kaydı sınırı (20)**, **aynı değer gönderilse de parolanın istenmesi** (Karar 2) ve **kullanıcının kendi telefonunu maskeli görmesi** (USR-027) 22.11.2026 ürün kararıyla kalır. Telefon maskesi doğrulamanınkiyle aynıdır (`+90******4567`): ülke öneki abonenin numarasından bir hane göstermez; USR-027'nin `target`'ı bunu yazar.
2. **Hangi profil alanlarının senkron niyeti doğurduğu:** bugün saklanan herhangi bir alan. Sağlayıcının yalnız bazı alanlarla ilgilendiği Faz 6'da netleşirse filtre gönderici tarafına konur.
3. **Referans verisinin kaynağı, güncellenmesi ve meslek yüksekokulları** referans PR'ının kararıdır (Karar 7).
