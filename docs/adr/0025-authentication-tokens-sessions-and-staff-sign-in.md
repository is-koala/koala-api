# ADR-0025: Kimlik doğrulama: token sınıfları, oturumlar, refresh rotasyonu ve personel girişi

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K12, K13 (hazırlık), D5; Yusuf'un 05.11 inceleme kararları ve doğrulama turu (parola uzunluğu: NIST SP 800-63B-4, en az 15; bilinen iyi kaynaklar; personel kilidi, marketplace kilidi, refresh toleransı, giriş hedefi HMAC'i, temizlik işleri, `/v1/meta/ping`'in kaldırılması)
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K12–K13, §3.3, §3.9, §4.1-1, §5.5, §6.1 Faz 1, §6.2-1, §6.3; [ADR-0005](0005-api-contract-principles.md) madde 1, [ADR-0015](0015-sessions-and-staff-mfa.md), [ADR-0018](0018-realtime-ticket-and-ws-protocol.md), [ADR-0019](0019-openapi-31-codegen-and-request-validation.md) §10, [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0022](0022-realtime-binding-and-session-check-corrections.md); N01 §3; `test/scenarios/auth.yaml`; RFC 7519 §4.1.4, RFC 8725 (JWT BCP), RFC 9068, RFC 9106 (Argon2), RFC 6749 §5.1, RFC 6750, RFC 9700 (OAuth 2.0 Security BCP) §4.14, OWASP Password Storage ve Authentication Cheat Sheet'leri, OWASP ASVS 5.0 V6–V7, NIST SP 800-63B §5.1.1 ve §5.2.2, NIST SP 800-63B-4 §3.1.1.2, PCI DSS 4.0 8.3.4
- Kod: `internal/authn`, `internal/platform/tokenkeys`, `internal/app/authn.go`, `internal/app/workers.go`, `internal/app/keygen.go`, `internal/realtime/store.go`, `db/migrations/0020`–`0022`, `db/queries/authn`, `db/queries/realtime`, `test/scenarios/scenarios_test.go`
- Yerine geçtiği: ADR-0018 §2'deki "Postgres bilet deposu Faz 4'te" notu (§10); ADR-0019 §10 (`/v1/meta/ping`, §9). ADR-0005 madde 1'i ve plan §4.1-1 ile §5.5'i `/v1/admin` için genişletir (§9). Plan §6.1 Faz 1 çıkış kriterinden ve §6.2-1'den sapmayı kaydeder (§11).

## Bağlam

ADR-0015 oturum modelini seçti (15 dk erişim token'ı, refresh rotasyonu, yeniden kullanım tespiti, personel kilidi, `token_version`) ama algoritmayı, anahtar yönetimini, süreleri, şemayı ve akışların ayrıntısını Faz 1'in authn PR'ına bıraktı. ADR-0018 ve ADR-0022 realtime'ın `Sessions` ve `Authenticator` arayüzlerini bu PR'a, ADR-0021 de `WithRateLimitIdentity`, cihaz eşlemesi ve `x-rate-limit: auth` işlerini bıraktı.

Java'da (N01 §3): tek HS256 anahtarı bütün sınıfları imzalıyor, `iss` doğrulanmıyor, marketplace erişim token'ı 12 saat, refresh token BCrypt'li ve rotasyonsuz, personel refresh'i 12 saatlik bir JWT, parola hash'i BCrypt cost 10, marketplace girişinde kilit yok, personel kilidi hesap başına 5 hata / 15 dk. Proje prodda olmadığı için eski token ya da hash uyumluluğu gerekmiyor.

PR #12'nin güvenlik ve sözleşme incelemeleri (05.11) bu metnin ilk taslağını değiştirdi; Yusuf'un kararları aşağıda ilgili maddelere işlendi.

## Karar

### 1. Erişim token'ı: EdDSA (Ed25519), sınıf başına ayrı anahtar halkası

| | HS256, sınıf başına ayrı anahtar | **EdDSA (Ed25519)** | RS256 / ES256 |
|---|---|---|---|
| Güvenlik | Yeterli (RFC 8725). Doğrulayan her yer imzalayabilir. | Doğrulama anahtarı imzalayamaz. Deterministik imza, nonce hatası yok. | RSA büyük anahtar ve imza; ECDSA nonce'a duyarlı. |
| Maliyet | ~1 µs | İmza ~20 µs, doğrulama ~50 µs | RSA doğrulama hızlı, imza yavaş |
| Bağımlılık | golang-jwt + stdlib | golang-jwt + stdlib `crypto/ed25519` | aynı |
| İleride | İkinci doğrulayıcı (admin BFF, ayrı realtime süreci) sırrı paylaşmak zorunda | Yalnız açık anahtar dağıtılır (JWKS'e hazır) | aynı |

**Karar: EdDSA.** Tek servis için HS256 de sektörde kabul görüyor; ama erişim token'ını doğrulayacak ikinci bir bileşen çıktığında HMAC sırrı dağıtılmak zorunda kalır ve her doğrulayıcı token basabilir. Ed25519 bu seçeneği bedavaya açık tutar; istek başına ~50 µs doğrulama, her istekteki oturum sorgusunun yanında ihmal edilir. Auth0, Okta ve Cognito gibi sağlayıcılar da asimetrik imzayı varsayılan yapıyor.

- **İki sınıf:** marketplace (`aud: koala-marketplace`) ve personel (`aud: koala-staff`). Her sınıfın kendi anahtar halkası vardır; halkalar anahtar paylaşamaz (yapılandırma açılışta reddeder). Realtime bileti imzalı değildir (ADR-0018 §3).
- **Doğrulama eksiksizdir:** `alg` yalnız `EdDSA` (`none` ve HS256 karışıklığı reddedilir, RFC 8725 §3.1); `typ: at+jwt` (RFC 9068 §2.1); `kid` sınıfın halkasında olmalı; imza; `iss = https://api.iskoala.com`; `aud`; `exp` zorunlu; `nbf` ve `iat` zorunlu; `jti` UUID; `sub` ve `sid` UUID. Süre aşımı dışındaki her ret aynı `AUTH_TOKEN_INVALID`'dır; imzası geçerli ama süresi dolmuş token `AUTH_TOKEN_EXPIRED` alır (istemci yeniler).
- **Saat kayması:** `exp`, `nbf` ve `iat` 5 saniyelik toleransla denetlenir (`authn.ClockSkew`; RFC 7519 §4.1.4 küçük bir payı öngörür). Birden çok örneğin saatleri arasındaki farkı karşılar; token `exp + 5 sn`'ye kadar geçerlidir.
- **İçerik:** `sub` (kullanıcı ya da personel), `sid` (oturum), personelde `stv` (`token_version`) ve `amr: ["pwd"]` (RFC 8176). Rol ve izin token'da yoktur (AUTH-012).
- **Ömür:** 15 dakika (K12).

### 2. Anahtar halkası, sırlar ve rotasyon

- `KOALA_JWT_MARKETPLACE_KEYS` ve `KOALA_JWT_STAFF_KEYS`: `kid:tohum[,kid:tohum…]`, tohum 32 baytlık Ed25519 seed'i (unpadded base64url). İlk anahtar imzalar, hepsi doğrular. **Halka başına en çok 4 anahtar** (`tokenkeys.MaxKeys`): rotasyon için ikisi yeter, kalanı acil değişim payıdır.
- **Tohum denetimi:** rastgele olmayan tohum reddedilir. 16'dan az farklı bayt değeri taşıyan 32 bayt (tamamı sıfır, tekrar eden desen, bir kelime) rastgele bir kaynaktan gelemez; rastgele 32 baytta ortalama ~30 farklı değer vardır ve 16'nın altı 2⁻¹⁰⁰'den küçük bir olasılıktır.
- **Rotasyon:** yeni anahtarı ikinci sıraya ekle → yayımla → birinci sıraya al → eski anahtarın son token'ı öldükten (15 dk + 5 sn) sonra çıkar. Refresh token'lar opak olduğu için rotasyon kimseyi çıkış yaptırmaz.
- **Üretim:** `koala keygen jwt <kid>` bir halka girdisi, `koala keygen pepper` bir giriş biberi basar. İkisi de yalnız stdout'a yazar; macOS'ta da `openssl` ya da GNU araçları gerekmez.
- Staging ve production'da iki halka ve `KOALA_LOGIN_PEPPER` (§5) `serve` için zorunludur (`config.ServeAuthErrors`). `KOALA_TRUSTED_PROXIES` de artık staging'de zorunludur (`config.ServeEdgeErrors`): staging de Traefik arkasındadır ve onsuz bütün istemciler proxy'nin oran sınırı kovasını ve personel adres kilidini paylaşır. Bu, ADR-0021'in yalnız production'da zorunlu tuttuğu değişkeni staging'de de fail-closed yapar. Local ve test ortamında ayarlı değilse süreç başına rastgele değer üretilir ve WARN yazılır. Değerler hiçbir dosyada yoktur; test anahtarları yalnız testte üretilir.

### 3. Refresh token: opak, rotasyonlu, aileye göre yeniden kullanım tespiti, tolerans penceresi

- İlk token 32 rastgele bayttır, istemciye 43 karakter base64url; DB'de yalnız SHA-256'sı tutulur. Arama hash'le DB'de yapılır; Go'da sır karşılaştırılmaz.
- **Aile oturumdur.** Her yenileme sunulan token'ı tüketir ve ardılını verir. Ardıl `HMAC-SHA256(k, token ‖ tuz)`'dur. Tuz tüketimde rastgele üretilir ve saklanır. `k`, `KOALA_LOGIN_PEPPER`'dan HKDF-SHA256 ile ayrı bir bağlamda (`koala-api refresh token successor v1`) türetilen sunucu anahtarıdır; yeni değişken gerekmez. Ardılı hesaplamak için hem token hem sunucu sırrı gerekir: DB sızıntısı (hash ve tuz) ardılı vermez, yalnız token'ı ele geçiren de sırrı bilmeden ardılı türetemez.
- **Yeniden kullanım:** tüketilmiş token yeniden gelirse oturum kapanır (`AUTH_REFRESH_TOKEN_REUSED`, RFC 9700 §4.14.2).
- **Tolerans penceresi (Yusuf, 05.11; Auth0 ve Okta pratiği):** token tüketildikten sonraki **10 saniye** içinde, **aynı cihazdan**, **ardılı henüz kullanılmamışken** gelirse bu bir istemci tekrarıdır: oturum kapanmaz, zaten verilmiş ardıl yeni bir erişim token'ıyla yeniden döner (`authn.RefreshGraceWindow`). Pencere geçmişse, ardıl kullanılmışsa ya da istek başka cihazdan geliyorsa yeniden kullanımdır. Eşzamanlı iki yenileme `FOR UPDATE` ile sıralanır; ikincisi pencere içindedir ve aynı ardılı alır. Bunu deterministik bir test kanıtlar: satır ayrı bir tx'te kilitlenir, iki yenileme Postgres'in kilit beklediğini gösterdiği ana kadar kuyruğa girer, kilit bırakılır.
- **Oturum token'ın kendisinden bulunur.** Yenileme erişim token'ı istemez; süresi günlerce önce dolmuş olsa da fark etmez (AUTH-003 hedefi). Bilinmeyen token hiçbir oturuma bağlanamaz ve hiçbir şeyi değiştirmez (`AUTH_REFRESH_TOKEN_INVALID`). Senaryo kataloğu AUTH-005'te bunu K12'ye bağlıyordu; K12 bu konuda bir şey söylemez, karar bu ADR'nindir.
- **Süreler:**

| | Boşta (kayan) | Mutlak | Gerekçe |
|---|---|---|---|
| Marketplace | 30 gün | 90 gün | Java'nın 30 günlük kayan penceresi (AUTH-005) korunur; rotasyonla birlikte bir tavan eklenir. |
| Personel | 30 dakika | 12 saat | ASVS 5.0 V7.3 (L2): 30 dk hareketsizlik ya da 12 saatte bir yeniden kimlik doğrulama. |

  Yeni token'ın süresi `min(şimdi + boşta, oturum sonu)`'dur. Süresi dolan token ya da oturum `AUTH_REFRESH_TOKEN_EXPIRED` alır ve oturum kapanır.

### 4. Kullanıcı oturumu ve cihaz

- Oturum bir cihaza bağlıdır (`devices`: kullanıcı + `X-Device-Id`). Aynı cihazda yeni giriş o cihazın açık oturumlarını kapatır; farklı cihazlar bağımsızdır (AUTH-002). Cihaz kimliği kimlik bilgisine bakılmadan önce denetlenir.
- **Her istekte oturum kontrolü** (ADR-0015 §2): erişim token'ı doğrulanır, sonra tek sorgu (oturumun PK'si + cihaz ve kullanıcı PK'leri) oturumun açık, süresi dolmamış, token'ın kullanıcısına ait, kullanıcısının ve cihazının aktif olduğunu ve isteğin oturumun cihazından geldiğini denetler. Başka cihazdan gelen istek ya da yenileme oturumu kapatır ve WARN yazar (AUTH-004). `last_seen_at` en çok 5 dakikada bir yazılır.
- Kullanıcı ya da cihaz soft-delete edilince oturumları kendiliğinden ölür: `user_sessions` → `users`/`devices` kenarları `detach`'tır ve kontrol iki ebeveyne `LEFT JOIN active_*` ile ulaşıp aktif olmalarını ister.
- Oturum kapanınca commit'ten sonra realtime hub'ı o oturumun bağlantılarını kapatır (`RevokeHook` → `Hub.DisconnectSession`).
- Kullanıcı uçları: giriş, yenileme, çıkış, `/v1/me`, oturum listesi, oturum ayrıntısı, tek oturum iptali ve "diğerlerini kapat". Başkasının oturumu var olmayanla aynı 404'ü alır (AUTH-016).

### 5. Parola, numaralandırma direnci ve marketplace kilidi

- **Hash:** argon2id (`x/crypto/argon2`), OWASP'ın yapılandırması: **m = 19 MiB, t = 2, p = 1, 16 bayt tuz, 32 bayt anahtar**. PHC string'i (`$argon2id$v=19$m=19456,t=2,p=1$…`); parametreler hash'te taşındığı için ileride değişebilir. Eski parametreli hash doğrulanır, başarılı girişte güncel parametreyle yeniden yazılır.
- **Sınırlar:** DB'den okunan hash'in parametreleri sınırlanır (bellek ≤ 256 MiB, t ≤ 10, p ≤ 16). Aynı anda en çok `GOMAXPROCS` hash koşar; bekleme isteğin context'ine bağlıdır. Anahtar karşılaştırması `subtle.ConstantTimeCompare`'dır; bir test paketin değişken zamanlı karşılaştırma kullanmadığını denetler.
- **Numaralandırma direnci:** kayıtsız adres ve parolası olmayan hesap, açılışta güncel parametrelerle üretilmiş bir sahte hash'e karşı doğrulanır. Kilitli hedef ile personelde devre dışı hesap, kilitli adres ve bekleme ise saklı hash'e karşı bir **göstermelik doğrulama** (`VerifyDecoy`) öder: gerçek doğrulamanın türetmelerinin aynısı yapılır, sonuç atılır. Eski parametreli bir hash doğrulandığında güncel parametrelerle bir türetme daha yapılır, sonuç doğru da yanlış da olsa; göstermelik doğrulama bu ek türetmeyi de yapar. Böylece aynı hesapta reddedilen ve incelenen deneme aynı sayıda türetme öder (`TestDecoyPaysTheDerivationsOfTheHash`). Kalan fark kayıtsız adresle eski parametreli hash arasındadır, eski parametrelerin maliyeti kadardır ve hash'ler yeniden yazılana kadar sürer.
- **Rehash yarışı:** giriş, parolayı kilit dışında doğrular ve kullanıcı satırını `FOR UPDATE` ile kilitlediğinde hash'in değişmediğini denetler (personeldeki gibi). Arada parola değiştiyse giriş yanlış parola sayılır.
- **Parola kuralı (Yusuf, 05.11; AUTH-009 hedefi):** **en az 15, en çok 128 karakter**, bileşim şartı yok. Karakter Unicode kod noktasıdır.
  - **Alt sınır:** NIST SP 800-63B-4 §3.1.1.2 parola tek faktörse en az 15, çok faktörlü kimlik doğrulamanın bir parçasıysa en az 8 karakter ister. Marketplace kullanıcısı için parola tek faktördür. Personel için de TOTP gelene kadar tek faktördür; bu yüzden ikisi de 15. **Personelde TOTP (K13, Faz 8) zorunlu olunca personelin alt sınırı 8'e inebilir;** bu, Faz 8'in ADR'sinde karar verilecek bir değişikliktir ve kural o zaman ilke türüne göre ayrılır. Java'nın 12 karakteri bir standarda dayanmıyordu.
  - **Üst sınır:** aynı belge en az 64 karaktere izin verilmesini ister; 128 aynı zamanda hash işini sınırlar. Java'daki 72 bayt sınırı bcrypt'ten geliyordu; argon2id'de gerekçesi yok.
  - Kural tek yerdedir (`authn.PasswordMinChars`, `authn.CheckPassword`) ve yalnız yeni parola belirlenirken uygulanır. Girişte uygulanmaz: daha kısa mevcut parolalar çalışmaya devam eder (Java'dan taşınan hesap yok; kural bugün yalnız personel parola değişiminde devrede).
- **Giriş hedefi (Yusuf, 05.11):** `login_attempts` ve oran sınırı anahtarı normalize adresin **HMAC-SHA256**'sını kullanır; anahtar sunucu sırrı `KOALA_LOGIN_PEPPER`'dır (en az 32 rastgele bayt, staging ve production'da zorunlu). Tuzsuz SHA-256, sızan bir tablonun bir adres listesiyle eşleştirilmesine izin veriyordu.
- **Marketplace kilidi (Yusuf, 05.11; NIST SP 800-63B §5.2.2):** kalıcı sayaç `login_attempts`'in kendisidir. Bir hedefin son başarılı girişinden sonraki her **100 ardışık başarısız deneme** hedefi **1 saat** kilitler. Kilit, yüzü tamamlayan denemenin anından başlar. Kilitliyken doğru parola da yanlış parolanın yanıtını bayt bayt aynı alır, aynı maliyeti öder ve `LOCKED` olarak kaydedilir. `LOCKED` deneme başarısız sayılmaz, kilidi uzatmaz. Başarılı giriş sayacı sıfırlar. Kayıtsız adres de aynı kurala tabidir, bu da numaralandırmayı kapatır. Kilidin başlaması `koala_auth_lockouts_total{principal="user",scope="target"}`'ı artırır ve WARN yazar.
  - **Bilinen iyi cihaz (doğrulama turu, 05.11):** hesabın son 30 gün içinde oturumu olmuş cihazı (`X-Device-Id`) kilitten muaftır (`authn.KnownSourceWindow`). Çalınma ya da başka cihaz yüzünden kapanan ve kullanıcının kendi kapattığı (`USER_REVOKED`) oturumlar sayılmaz. Bu cihaz hesap geneli oran sınırı kovası boşken de geçer (§8). Böylece saldırgan, kullanıcıyı kendi cihazından dışarıda tutamaz. Kontrol her girişte ve her hesap kovası reddinde koşar; kayıtsız adreste aynı iki sorgu boş kullanıcıyla koşar, bu yüzden adres var mı yok mu sorusuna süre farkı eklemez.
  - **Muafiyetin bedeli, açıkça:** cihaz kimliği bir sır değildir. Onu bilen (ya da tahmin eden) saldırgan kilidi de hesap kovasını da atlar. Ona kalan tek sınır (hedef, adres) kovasıdır (dakikada 10, burst 5) ve kök zincirin adres sınırıdır. Bu ikisi adres başınadır ve IPv6 istemci /64'üyle anahtarlanır. Tek bir /48 bloğu 65 536 farklı /64 demektir, yani 65 536 kat bu bütçe. **Bilinen cihaz kimliğini bilen dağıtık bir saldırgan için hesap düzeyinde bir tahmin sınırı yoktur.** Bu boşluk, cihaz kimliği yerine cihaza özgü bir anahtar (cihaz bağlama) gelene kadar kabul edilmiş bir risktir (açık soru 4).
  - **Sayaç okuması kilit almaz:** aynı hedefin eşzamanlı girişleri, hiçbiri yazılmadan sayılan durumu görür. Kilit en çok o kadar deneme geç başlar. Eşzamanlı deneme sayısını hedefin kovaları sınırlar: adres başına burst 5, adresler arasında 10. Her deneme yine yazılır, kilidin başlangıcı da sıradan bağımsız olarak yüzüncü hatanın anıdır. Kilidin metriği ve uyarısı 99 hata okuyan denemede tetiklenir; eşzamanlı denemelerde atlanabilir ya da tekrarlanabilir.

### 6. Personel girişi

- Ayrı token sınıfı, ayrı uçlar (`/v1/admin/auth/*`, `/v1/admin/me`). Personel token'ı pazaryerinde, pazaryeri token'ı personel yüzeyinde geçmez (D5, AUTH-015). Oturum cihaza bağlı değildir (panel cihaz göndermez, ADR-0021 §2); `staff_sessions` + rotasyonlu refresh token (aynı tolerans penceresiyle).
- **`token_version`:** erişim token'ı `stv` taşır, oturum açıldığı andaki sürümü saklar. Her istekte hesabın güncel sürümü, oturumunki ve token'ınki eşit olmalıdır. Çıkış ve parola değişimi sürümü artırır: hesabın bütün cihazlardaki erişim ve refresh token'ları, isteği yapan dahil, ölür (AUTH-013).
- **Kilit (Yusuf, 05.11 ve doğrulama turu; PCI DSS 4.0 8.3.4, OWASP):**
  - **(Hesap, istemci adresi) çifti:** o adresten **5 ardışık hata** hesabı o adres için **30 dakika** kilitler (`staff_login_sources`; IPv6 istemci /64'üyle). Diğer adresler etkilenmez. Bu kilit bilinen iyi adresler dahil her adrese uygulanır.
  - **Hesap geneli, adresten bağımsız:** **10 ardışık hatadan** sonra her hata **ikiye katlanan bir bekleme** koyar: 1 sn, 2 sn, 4 sn… en çok **15 dakika** (`staff_members.backoff_until`). Sert kilit yoktur.
  - **Bilinen iyi kaynak (known-good source):** personelin son 30 gün içinde başarılı giriş yaptığı adres, o başarıdan sonra hesabın `token_version`'ı değişmediyse beklemeden ve hesap geneli oran kovasından muaftır (`staff_login_sources.last_success_at` ve `success_token_version`, `authn.KnownSourceWindow`). Çıkış (hesabın bütün oturumları kapanır), parola değişimi ve hesabın kapatılması sürümü artırır, bu yüzden diğer adreslerin bilinen iyi durumunu bitirir. İsteği yapan adres durumunu korur: parola değişiminde bilinen iyi kaynak olur, çıkışta öyleyse öyle kalır. Hesabı kapatan personel yönetimi (Faz 8, `koala_maint`) durumla birlikte sürümü de artırmak zorundadır. Bekleme yalnız yeni ya da bilinmeyen adreslere uygulanır. İlk taslak "saldırgan personeli dışarıda tutamaz" diyordu, bu doğru değildi; düzeltilmiş hâli şudur. Personel kullandığı yerlerden her zaman girer, adres kilidi dışında (aşağıda). **Yeni bir adresten giriş ise süresiz geciktirilebilir:** saldırgan her bekleme bitince yeni bir adresten bir hata daha denedikçe bekleme yenilenir, en çok 15 dakikalık adımlarla, saldırı sürdükçe. Bilinen iyi kaynak adres kilidinden muaf değildir: o adresten 5 hata onu da kilitler (`TestStaffKnownSourceIsStillLocked`).
  - **Kilit ya da bekleme sürerken deneme incelenmeden reddedilir.** Karar hash'lemeden önce, hesabın ve adresin durumundan verilir. Devre dışı hesap da aynı yoldan geçer. Parola saklı hash'e karşı göstermelik doğrulanır: aynı türetmeler yapılır, sonuç atılır. Yanıt süresi parolanın doğru olup olmadığına bağlı değildir ve rehash yapılmaz. Rehash yalnız başarılı girişten sonra ödenir (`UPDATE … WHERE password_hash = <doğrulanan>`).
  - Kilitli deneme hiçbir sayacı değiştirmez. Yanıt yanlış parolanın yanıtıyla bayt bayt aynıdır (`STAFF_LOGIN_FAILED`, AUTH-011). Başarılı giriş hesap sayacını ve o adresin sayacını sıfırlar, adresi bilinen iyi kaynak yapar.
  - **Her ret aynı deyimleri koşar** (AUTH-010): kayıtsız adres, devre dışı hesap, kilitli adres ya da bekleme ve yanlış parola. Hepsi işlemden önce iki okuma yapar (kimlik bilgisi, adres durumu). İşlemde beş deyim koşar: satır kilidi, adres durumu `FOR UPDATE`, iki sayaç yazımı ve deneme satırı. Sayılacak hatası olmayan retlerde iki yazım, üyesi olmayan retlerde de kilitler boş kimlikte koşar ve hiçbir satıra dokunmaz. Kalan fark dokunulan satır sayısıdır: aynı plan, sıfır ya da bir satır. Bu fark milisaniyenin altındadır ve bir argon2id türetmesinin (onlarca milisaniye) gürültüsü içinde kalır.
  - Kilit ve bekleme olayları `koala_auth_lockouts_total{principal="staff",scope="address"|"backoff"}`'ı artırır ve WARN yazar.
  - **Parola değişimi de aynı korumadan geçer.** Adres kilitliyse ya da hesap beklemedeyse ve adres bilinen iyi değilse, değişim incelenmeden reddedilir (göstermelik doğrulama, `STAFF_CURRENT_PASSWORD_MISMATCH`). Durum yazmadan önce üyenin satır kilidi altında yeniden okunur; arada kilitlenmiş ya da beklemeye girmişse değişim yine reddedilir. Yanlış mevcut parola iki sayaca da girer. Adresi kilitleyen ya da hesabı beklemeye sokan yanlış deneme oturumu kapatır (`revoke_reason = LOCKED`).
  - Java'nın hesap başına 5 hata / 15 dk kilidi bir saldırganın personeli dışarıda tutmasına izin veriyordu; senaryo AUTH-011'in `target` alanı bu kararı taşır.
- **Panelin sunucu tarafından çağırması (koala-admin BFF):** adres temelli personel korumaları gerçek tarayıcı adresine dayanır: adres kilidi, bilinen iyi kaynak ve (hedef, adres) kovası. Panel API'yi sunucusundan çağırırsa iki koşul sağlanmalıdır:
  - BFF, tarayıcının adresini `X-Forwarded-For`'un **sonuna ekler**; gelen değerin solundakini kopyalamaz.
  - BFF'nin adresi `KOALA_TRUSTED_PROXIES` içindedir.
  Bu sağlanmadan bütün personel tek bir adresten (BFF'den) geliyor görünür. O zaman bir saldırgan BFF üzerinden 5 hatayla herkesin girişini kilitler, bilinen iyi kaynak da herkes için aynı adres olur. koala-admin tarafındaki iş ayrı bir issue'dadır. Koşul runbook'tadır.
- **`must_change_password`:** işaretli hesap yalnız parola değişimine ve çıkışa erişir; başka her personel ucu `403 STAFF_PASSWORD_CHANGE_REQUIRED` döner (AUTH-014).
- **Personel hesabını uygulama oluşturamaz:** `koala_app`'in `active_staff_members` üzerinde `INSERT` yetkisi yoktur (katalogda `app_view: [SELECT, UPDATE]`); hesaplar bootstrap ve personel yönetimiyle `koala_maint` üzerinden açılır (Faz 8).
- **TOTP'ye hazırlık (K13, Faz 8):** `staff_sessions.mfa_verified_at` ve token'daki `amr` bugün "yalnız parola"yı anlatır. Faz 8'de giriş, parola doğrulandıktan sonra ikinci adım ister; `amr`'a `otp` eklenir ve personel uçları `mfa_verified_at` dolu oturum ister. Parola o zaman ikinci faktörün yanında kullanıldığı için personelin parola alt sınırı 15'ten 8'e inebilir (§5).

### 7. Şema, soft-delete sınıfları ve temizlik

| Tablo | Sınıf | Not |
|---|---|---|
| `users` | soft_delete, pii | Kimlik çekirdeği: e-posta, parola hash'i (NULL olabilir), durum. Profil ve TCKN Faz 2. |
| `devices` | soft_delete, pii | Kullanıcı + `X-Device-Id`; `users`'a `cascade`. |
| `staff_members` | soft_delete, pii, `app_view: [SELECT, UPDATE]` | Hesap sayacı ve beklemesi, `must_change_password`, `token_version`. Roller 0040+'da. |
| `login_attempts` | M2, **pii** | Güvenlik kaydı ve marketplace kilit sayacı. **M1 değil:** 90 günlük saklama süresi var ve süre dolunca gerçekten silinir; M1 "silinmez" der. Hedef sırla HMAC'lenmiş olsa da zamanla birlikte kişisel veridir, bu yüzden `pii`. |
| `staff_login_sources` | M2, **pii** | Personelin bir adresteki durumu: hata sayacı, adres kilidi ve son başarılı giriş (bilinen iyi kaynak). Hesaba bağlı istemci adresi kişisel veridir. **Saklama:** kilidi bitmiş satır son değişikliğinden bir gün sonra, bilinen iyi kaynak son başarılı girişinden 30 gün sonra silinir. |
| `user_sessions`, `user_refresh_tokens`, `staff_sessions`, `staff_refresh_tokens`, `realtime_tickets` | M2 | Süresi dolunca hard delete. |

- Katalogda `pii` artık muaf tablolarda da yazılabilir: view'ı olmayan tablonun kişisel veri taşıdığını saklama ve anonimleştirme envanteri için işaretler. Soft-delete'li tablolarda `pii` ⇔ `security_barrier` kuralı aynen geçerlidir.
- **Yetkiler:** `koala_app` `login_attempts`'e yalnız `SELECT, INSERT, DELETE` alır (kayıt güncellenmez). Refresh token tablolarına `SELECT, INSERT, UPDATE` alır; refresh token oturumuyla silinir (`ON DELETE CASCADE` sahibin yetkisiyle koşar).
- Oturumların soft-delete'li ebeveynlere kenarları `detach`'tır. Oturum açan ve cihaz ekleyen akış kullanıcı satırını `FOR UPDATE` ile kilitler; bu, `softdelete.LockActiveParent`'ın güvencesini sağlar ve aynı kullanıcının iki girişini sıralar.
- **Temizlik (Yusuf, 05.11):** `auth_purge` bir River periyodik işidir; saatte bir koşar, açılışta da. Sildikleri:
  - biten oturumlar, 30 gün sonra, refresh token'ları ve biletleriyle;
  - denemeler, 90 gün sonra;
  - adres durumları, yukarıdaki saklama süresine göre;
  - süresi geçen biletler, hemen.
- **İşçilerin başlaması ve durması:**
  - `serve`, River istemcisini veritabanını beklemeden başlatır. Başlatma başarısız olursa 5 saniyede bir yeniden dener ve her denemeyi `koala_worker_start_failures_total`'a sayar. `koala_workers_up`, istemci çalışırken 1'dir. Kalıcı başlatma hatası, sayaç artarken bu gösterge 0'da kalarak görünür.
  - Kapanışta HTTP drain'inden sonra önce işçilerin context'i iptal edilir: hâlâ veritabanına bağlanmaya çalışan bir başlatma hemen döner, koşan iş de iptal olur. Temizlik idempotent olduğundan bir sonraki koşu yarım kalanı bitirir. Ardından istemcinin durması en çok 5 saniye beklenir. Bu süre `stop_grace_period` payının içindedir.

### 8. Oran sınırı

- Giriş, yenileme, personel girişi/yenilemesi ve personel parola değişimi `x-rate-limit: auth` grubundadır. Kök zincir adres başına sınırı uygular (dakikada 30, burst 10).
- **Giriş (Yusuf, 05.11)** iki `KeyCustom` kovasından jeton alır: (hedef özeti, adres) için `auth` grubunun özel kovası (dakikada 10, burst 5) ve hedef geneli, adresten bağımsız, `auth_account` grubunun özel kovası (dakikada 30, burst 10). Aynı NAT arkasındaki farklı hesaplar birbirini kilitlemez; çok adresten tek hesaba hızlı deneme de yapılamaz (AUTH-018). Yenileme token özetinin, personel parola değişimi personel kimliğinin kovasını kullanır.
- **Bilinen iyi kaynak hesap geneli kovadan muaftır** (§5, §6): marketplace'te hesabın son 30 gündeki bir oturumunun cihazı, personelde bilinen iyi adres. Kova boşsa ara katman bunu sorar, öyleyse isteği geçirir. Böylece saldırgan, hesabın kovasını çok adresten boşaltarak kullanıcıyı ya da personeli dışarıda tutamaz. (Hedef, adres) kovası onlara da uygulanır. Veritabanı sorgusu yalnız kova boşken koşar; sayısını da (hedef, adres) kovası sınırlar.
- `auth_account` grubunun route'u yoktur; ADR-0021 §5'in tablosuna bu ADR'nin satırı eklenir (`TestRateLimitGroups` iki tabloyu birlikte okur):

<!-- rate-limit-groups: internal/app TestRateLimitGroups bu tabloyu ADR-0021'inkiyle birlikte okur; ilk dört sütunun biçimi "dk / burst" ya da "—". -->
| Grup | Adres başına (dk / burst) | Kimlik başına (dk / burst) | Özel anahtar (dk / burst) | Java karşılığı (IP başına) | Java'ya oran |
|---|---|---|---|---|---|
| `auth_account` | — | — | 30 / 10 | yok (Java'da hedef geneli kova yoktu) | yeni |

- Kimliği doğrulanmış her istek `user:<id>` ya da `staff:<id>` adıyla kimlik başına sınıra girer. Token yanıtları `Cache-Control: no-store` taşır (RFC 6749 §5.1). Kimlik doğrulama gövdeleri 4 KiB'la sınırlıdır (`x-body-limit`).

### 9. Middleware ve sözleşme

- API grubu sırası: **authn → kimlik başına oran sınırı → doğrulama** (ADR-0019, ADR-0021). Authn, route'un operasyonunun `security`'sini spec'ten okur: `[]` public, `marketplaceBearer`, `adminBearer`. Spec'te olmayan route reddedilir. 401'ler `WWW-Authenticate: Bearer` taşır (RFC 6750).
- **`/v1/admin` istisnaları (ADR-0005 madde 1'in ve plan §4.1-1 ile §5.5'in genişletmesi):** ADR-0005 ve plan, `/v1/admin` altındaki her operasyonun bir personel izni (`x-authz-permission`) taşımasını ister. İki dar istisna bu ADR ile gelir. Lint bunları üç kuralla zorlar; kurallar `head`, `options` ve `trace` dahil her metoda uygulanır:
  - `koala-admin-public`: kimliksiz personel uçları yalnız `^/v1/admin/auth/(login|refresh)$`'tir; `security: []`, izin ve muafiyet yok.
  - `koala-admin-self-service`: personelin yalnız kendi hesabına dokunan uçlar açık bir izin listesidir: `^/v1/admin/(me|auth/(logout|password))$`. Bunlar adminBearer + `x-authz-exempt` taşır, izin taşımaz.
  - `koala-admin-permission`: bu beş yolun dışındaki her `/v1/admin` operasyonu adminBearer + `x-authz-permission` taşır. `/v1/admin/auth/` altına eklenen yeni bir uç da buna dahildir. Ne `x-authz-exempt` ne `security: []` izin yerine geçer.
  Prob, her kuralın vakasını sabitler: izin listesi dışında `security: []`, giriş ya da yenilemede muafiyet ya da izin, self-service yolunda `security: []`, `HEAD` operasyonu. Doğru biçimler de prob tarafından sabitlenir.
- **`/v1/meta/ping` kaldırıldı** (ADR-0019 §10'un yerine geçer): ilk gerçek operasyonlar geldi. Doğrulama zincirinin uçtan uca testi `POST /v1/auth/login`'e taşındı (`TestLoginRequestValidation`). Merge patch'in uçtan uca testi ilk gerçek PATCH operasyonuyla `internal/app`'e döner; o zamana kadar `internal/codegentest` üreticinin biçimini sabitler. `BenchmarkPing`'in ölçtüğü yol (ADR-0019 §2) aynı doğrulayıcıdır; yeni bir benchmark ilk DB'siz operasyonla eklenir.
- Yeni hata kodları `ErrorCode` listesindedir; `TestPlatformCodesAreInTheContract` `authn.Codes()`'u da denetler.

### 10. Realtime bağlantısı (ADR-0018, ADR-0022)

- `TicketStore` Postgres'tedir (`realtime_tickets`): `Put` bir `INSERT`, `Take` `DELETE … WHERE token_hash = $1 RETURNING …`. Bu, ADR-0018 §2'deki "Postgres deposu Faz 4'te" notunun yerine geçer.
- `Sessions.Active` `user_sessions` üzerindeki aynı kontroldür; ADR-0022 §2 sözleşmesine uyar.
- `Authenticator` bilet isteğini her marketplace isteği gibi doğrular. `POST /v1/realtime/tickets` ve `GET /v1/realtime` `internal/app`'te bağlanır; hub `httpx.ServerOptions.Streams`'e girer. Origin listesi `KOALA_REALTIME_ORIGINS`'ten gelir.
- AUTH-017'nin "başka kitle için verilmiş bilgi bağlanamaz" maddesi düşer: bilet opaktır, kitlesi yoktur; sınıf ayrımı depoyla sağlanır (ADR-0018 §3). Test erişim token'ını bilet yerine sunar ve reddedildiğini gösterir.

### 11. Senaryo bağlama kuralı ve Faz 1 çıkış kriterinden sapma

- **Sapma, adıyla:** plan §6.1 Faz 1'in çıkış kriteri "AUTH senaryolarının %100'ü yeşil"dir; §6.2-1 her P0'ın bir Go testine bağlanmasını ister. Bu PR'dan sonra AUTH-006, 007 ve 008 (parola sıfırlama ve kayıt) Faz 2'ye ertelenir (`deferred: {phase: 2}`). AUTH-012 Faz 1'de personel RBAC PR'ında kapanır (`partial: {phase: 1}`). AUTH-009 (`partial: {phase: 2}`), AUTH-011 ve AUTH-015 (`partial: {phase: 8}`, TOTP) kısmen bağlıdır. Yani Faz 1 bu PR'la %100'e ulaşmaz; çıkış kriteri, kalan kalemlerin fazlarına dağıtılmasıyla izlenir.
- **Makine kontrolü:** `test/scenarios/scenarios_test.go` her P0'ın ya `go_test`'e ya `deferred`'a bağlı olduğunu, `deferred` ve `partial`'ın yapılandırılmış (`phase`, `reason`) olduğunu ve hiçbirinin geçmiş bir fazı beklemediğini denetler. Faz numarası `currentPhase` sabitindedir. Fazdan çıkan PR sabiti artırır ve o fazın açık kalemi CI'ı kırar.
- **Bağlama:** bağlı her test fonksiyonu senaryo kimliğini gövdesindeki bir dizgede taşır (alt test adı ya da hata iletisi); yalnız doc yorumunda geçmesi yetmez. `go_test` bir alt testi adlandırıyorsa o adda bir `t.Run` bulunur.

## Sonuçlar

- AUTH kapsamı (18): 15 senaryo bağlı ve testli; 3'ü (AUTH-006, 007, 008) Faz 2'ye `deferred`. Bağlı olanlardan 4'ü `partial`: AUTH-009 (Faz 2), AUTH-011 ve AUTH-015 (Faz 8), AUTH-012 (Faz 1, RBAC PR'ı).
- Yeni bağımlılıklar (onaylı liste, plan §3.3, K19): `github.com/golang-jwt/jwt/v5` v5.3.1 (MIT, 28.01.2026), `golang.org/x/crypto` v0.58.0 (BSD-3, 09.10.2026; `argon2`).
- Yeni değişkenler: `KOALA_JWT_MARKETPLACE_KEYS`, `KOALA_JWT_STAFF_KEYS`, `KOALA_LOGIN_PEPPER` (üçü de staging ve production'da `serve` için zorunlu, sır) ve `KOALA_REALTIME_ORIGINS`. Değerleri `koala keygen` üretir; burada yazılmaz.
- Yeni metrikler: `koala_auth_lockouts_total{principal,scope}`, `koala_workers_up`, `koala_worker_start_failures_total`.
- Mobil ve web istemcileri refresh rotasyonuna uyarlanır: her yenilemede gelen yeni refresh token saklanır; tolerans penceresi ağ tekrarını karşılar ama yenileme yine tek uçuşta tutulur (plan §4.2).
- **Açık sorular:**
  1. Marketplace oturumu için 90 günlük mutlak tavan bir ürün kararıdır.
  2. Yeni bir adresten giren personel, saldırı sürdükçe 15 dakikalık adımlarla süresiz bekletilebilir; bildiği adreslerden beklemez. TOTP gelince bekleme kısaltılabilir ya da bilinmeyen adres için ikinci faktöre bağlanabilir.
  3. Personel bootstrap'i (ilk OWNER) bu PR'da yok. Bugün personel hesabı yalnız `koala_maint` ile elle açılabilir.
  4. Marketplace'te bilinen iyi cihaz muafiyeti cihaz kimliğine dayanır ve cihaz kimliği gizli değildir. Faz 8'de cihaz bağlama (cihaza özgü anahtar) gelirse muafiyet ona taşınabilir.
