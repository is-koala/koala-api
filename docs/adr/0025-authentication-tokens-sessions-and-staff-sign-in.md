# ADR-0025: Kimlik doğrulama: token sınıfları, oturumlar, refresh rotasyonu ve personel girişi

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K12, K13 (hazırlık), D5
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K12–K13, §3.3, §3.9, §6.1 Faz 1, §6.3; [ADR-0015](0015-sessions-and-staff-mfa.md), [ADR-0018](0018-realtime-ticket-and-ws-protocol.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0022](0022-realtime-binding-and-session-check-corrections.md); N01 §3; `test/scenarios/auth.yaml`; RFC 8725 (JWT BCP), RFC 9068 (access token JWT profili), RFC 9106 (Argon2), RFC 6749 §5.1, RFC 6750, OAuth 2.0 Security BCP (RFC 9700) §4.14, OWASP Password Storage ve Authentication Cheat Sheet'leri, OWASP ASVS 5.0 V6–V7, NIST SP 800-63B
- Kod: `internal/authn`, `internal/platform/tokenkeys`, `internal/app/authn.go`, `internal/realtime/store.go`, `db/migrations/0020`–`0022`, `db/queries/authn`, `db/queries/realtime`, `test/scenarios/scenarios_test.go`

## Bağlam

ADR-0015 oturum modelini seçti (15 dk erişim token'ı, refresh rotasyonu, yeniden kullanım tespiti, personel kilidi, `token_version`) ama algoritmayı, anahtar yönetimini, süreleri, şemayı ve akışların ayrıntısını Faz 1'in authn PR'ına bıraktı. ADR-0018 ve ADR-0022 realtime'ın `Sessions` ve `Authenticator` arayüzlerini bu PR'a, ADR-0021 de `WithRateLimitIdentity`, cihaz eşlemesi ve `x-rate-limit: auth` işlerini bıraktı.

Java'da (N01 §3): tek HS256 anahtarı bütün sınıfları imzalıyor, `iss` doğrulanmıyor, marketplace erişim token'ı 12 saat, refresh token BCrypt'li ve rotasyonsuz, personel refresh'i 12 saatlik bir JWT, parola hash'i BCrypt cost 10. Proje prodda olmadığı için eski token ya da hash uyumluluğu gerekmiyor.

## Karar

### 1. Erişim token'ı: EdDSA (Ed25519), sınıf başına ayrı anahtar halkası

Seçenekler:

| | HS256, sınıf başına ayrı anahtar | **EdDSA (Ed25519)** | RS256 / ES256 |
|---|---|---|---|
| Güvenlik | Yeterli (RFC 8725). Doğrulayan her yer imzalayabilir. | Doğrulama anahtarı imzalayamaz. Deterministik imza, nonce hatası yok. | RSA büyük anahtar ve imza; ECDSA nonce'a duyarlı. |
| Maliyet | ~1 µs | İmza ~20 µs, doğrulama ~50 µs | RSA doğrulama hızlı, imza yavaş |
| Bağımlılık | golang-jwt + stdlib | golang-jwt + stdlib `crypto/ed25519` | aynı |
| İleride | İkinci doğrulayıcı (admin BFF, ayrı realtime süreci) sırrı paylaşmak zorunda | Yalnız açık anahtar dağıtılır (JWKS'e hazır) | aynı |

**Karar: EdDSA.** Tek servis için HS256 de sektörde kabul görüyor; ama erişim token'ını doğrulayacak ikinci bir bileşen (admin paneli BFF'i, ayrı çalışan bir realtime örneği) çıktığında HMAC sırrı dağıtılmak zorunda kalır ve her doğrulayıcı token basabilir. Ed25519 bu seçeneği bedavaya açık tutar; istek başına ~50 µs doğrulama, her istekteki oturum sorgusunun yanında ihmal edilir. Auth0, Okta ve Cognito gibi sağlayıcılar da asimetrik imzayı varsayılan yapıyor.

- **İki sınıf:** marketplace (`aud: koala-marketplace`) ve personel (`aud: koala-staff`). Her sınıfın kendi anahtar halkası vardır; halkalar anahtar paylaşamaz (yapılandırma açılışta reddeder). Realtime bileti imzalı değildir (ADR-0018 §3).
- **Doğrulama sabit sırayla ve eksiksizdir:** `alg` yalnız `EdDSA` (`none` ve HS256 karışıklığı reddedilir, RFC 8725 §3.1); `typ: at+jwt` (RFC 9068 §2.1; servisin imzalayabileceği başka JWT erişim token'ı yerine geçemez); `kid` sınıfın halkasında olmalı; imza; `iss = https://api.iskoala.com`; `aud`; `exp` zorunlu; `nbf` ve `iat` zorunlu, gelecekte olamaz; `jti` UUID; `sub` ve `sid` UUID. Süre aşımı dışındaki her ret aynı `AUTH_TOKEN_INVALID`'dır; imzası geçerli ama süresi dolmuş token `AUTH_TOKEN_EXPIRED` alır (istemci yeniler). Saat payı yoktur: token'ı aynı servis üretip doğruluyor ve saat `clock.Clock`'tan gelir.
- **İçerik:** `sub` (kullanıcı ya da personel), `sid` (oturum), personelde `stv` (`token_version`) ve `amr: ["pwd"]` (RFC 8176). Rol ve izin token'da yoktur (AUTH-012); izin her istekte DB'den çözülür (personel RBAC PR'ı).
- **Ömür:** 15 dakika (K12).

### 2. Anahtar halkası ve rotasyon

- `KOALA_JWT_MARKETPLACE_KEYS` ve `KOALA_JWT_STAFF_KEYS`: `kid:tohum[,kid:tohum…]`, tohum 32 baytlık Ed25519 seed'i (unpadded base64url). İlk anahtar imzalar, hepsi doğrular; en çok 4 anahtar (`internal/platform/tokenkeys`).
- Rotasyon: yeni anahtarı ikinci sıraya ekle → yayımla → birinci sıraya al → eski anahtarın son token'ı öldükten (15 dk) sonra çıkar. Refresh token'lar opak olduğu için rotasyon kimseyi çıkış yaptırmaz.
- Staging ve production'da iki değişken de `serve` için zorunludur (`config.ServeAuthErrors`). Local ve test ortamında ayarlı değilse süreç başına rastgele anahtar üretilir ve WARN yazılır; token'lar süreçle ölür. Değerler hiçbir dosyada yoktur; test anahtarları yalnız testte üretilir.

### 3. Refresh token: opak, rotasyonlu, aileye göre yeniden kullanım tespiti

- 32 rastgele bayt, istemciye 43 karakter base64url; DB'de yalnız SHA-256'sı (`user_refresh_tokens`, `staff_refresh_tokens`). Arama hash'le DB'de yapılır; Go'da sır karşılaştırılmaz.
- **Aile oturumdur.** Her yenileme sunulan token'ı tüketir (`consumed_at`) ve yenisini verir. Tüketilmiş token yeniden gelirse oturumun tamamı kapanır (`AUTH_REFRESH_TOKEN_REUSED`, OAuth 2.0 Security BCP §4.14.2). Tüketilmiş satırlar oturumla birlikte temizlenene kadar kalır, böylece ailenin her eski token'ı tespit edilir.
- **Grace süresi yoktur.** Aynı token'la eşzamanlı iki yenileme `FOR UPDATE` ile sıralanır; biri başarılı, diğeri yeniden kullanım olur ve oturum kapanır. Ağ tekrarında oturum kaybı riski bilinçli olarak kabul edildi (Auth0'ın "reuse interval" varsayılanı da 0). Mobil istemci yenilemeyi tek uçuşta tutar (§4.2).
- **Oturum token'ın kendisinden bulunur.** Yenileme erişim token'ı istemez; süresi günlerce önce dolmuş olsa da fark etmez (AUTH-003 hedefi). Bilinmeyen token hiçbir oturuma dokunmaz (`AUTH_REFRESH_TOKEN_INVALID`); yanlış token'ın oturumu kapatması K12'de istenmişti ama oturum bilinmediği için mümkün değil ve gerekmiyor.
- **Süreler:**

| | Boşta (kayan) | Mutlak | Gerekçe |
|---|---|---|---|
| Marketplace | 30 gün | 90 gün | Java'nın 30 günlük kayan penceresi (AUTH-005) korunur; rotasyonla birlikte bir tavan eklenir. Mobil uygulamalarda yaygın aralık. |
| Personel | 30 dakika | 12 saat | ASVS 5.0 V7.3 (L2): 30 dk hareketsizlik ya da 12 saatte bir yeniden kimlik doğrulama. Java'da personel refresh'i 12 saatti. |

  Yeni token'ın süresi `min(şimdi + boşta, oturum sonu)`'dur. Süresi dolan token ya da oturum `AUTH_REFRESH_TOKEN_EXPIRED` alır ve oturum kapanır.

### 4. Kullanıcı oturumu ve cihaz

- Oturum bir cihaza bağlıdır (`devices`: kullanıcı + `X-Device-Id`). Aynı cihazda yeni giriş o cihazın açık oturumlarını kapatır; farklı cihazlar bağımsızdır (AUTH-002). Cihaz kimliği kimlik bilgisine bakılmadan önce denetlenir.
- **Her istekte oturum kontrolü** (ADR-0015 §2): erişim token'ı doğrulanır, sonra tek sorgu (`GetUserSessionState`: oturumun PK'si + cihaz ve kullanıcı PK'leri) oturumun açık, süresi dolmamış, token'ın kullanıcısına ait, kullanıcısının ve cihazının aktif olduğunu ve isteğin oturumun cihazından geldiğini denetler. Başka cihazdan gelen istek ya da yenileme oturumu kapatır ve WARN yazar (AUTH-004). `last_seen_at` en çok 5 dakikada bir yazılır.
- Kullanıcı ya da cihaz soft-delete edilince oturumları kendiliğinden ölür: `user_sessions` → `users`/`devices` kenarları `detach`'tır ve kontrol iki ebeveyne `LEFT JOIN active_*` ile ulaşıp aktif olmalarını ister.
- Oturum kapanınca (çıkış, yerine geçme, cihaz uyuşmazlığı, yeniden kullanım, iptal) commit'ten sonra realtime hub'ı o oturumun bağlantılarını kapatır (`RevokeHook` → `Hub.DisconnectSession`).
- Kullanıcı uçları: giriş, yenileme, çıkış, `/v1/me`, oturum listesi, oturum ayrıntısı, tek oturum iptali ve "diğerlerini kapat". Başkasının oturumu var olmayanla aynı 404'ü alır (AUTH-016). Liste cihaz başına bir oturum olduğu için tek sayfadır (cursor'lar `null`, en çok 100).

### 5. Parola: argon2id, PHC biçimi, parametre sürümleme

- `x/crypto/argon2` IDKey; varsayılan **m = 19 MiB, t = 2, p = 1, 16 bayt tuz, 32 bayt anahtar** (OWASP Password Storage Cheat Sheet'in argon2id yapılandırması; 46 MiB/t=1 seçeneğiyle eşdeğer güçte, eşzamanlı giriş başına daha az bellek).
- Hash PHC string'idir: `$argon2id$v=19$m=19456,t=2,p=1$<tuz>$<anahtar>`. Parametreler hash'te taşındığı için varsayılanlar ileride değişebilir: eski parametreli hash doğrulanmaya devam eder ve `Verify` "yeniden hash'le" der; başarılı girişte yeni parametreyle yazılır.
- DB'den okunan hash'in parametreleri sınırlanır (bellek ≤ 256 MiB, t ≤ 10, p ≤ 16); bozuk ya da aşırı pahalı değer hash'lenmeden reddedilir. Anahtar karşılaştırması `subtle.ConstantTimeCompare`'dır; bir test paketin değişken zamanlı karşılaştırma kullanmadığını denetler.
- Aynı anda en çok `GOMAXPROCS` hash koşar; her biri 19 MiB tutar. Bekleme isteğin context'ine bağlıdır.
- **Numaralandırma direnci:** kayıtsız adres ya da parolası olmayan hesap, açılışta rastgele bir parolayla güncel parametrelerle üretilmiş bir hash'e karşı doğrulanır; iki dal da bir doğrulama ve bir `login_attempts` satırı öder, hata ve gövde aynıdır (AUTH-001, AUTH-010).
- **Parola kuralı** Java'dan aynen: en az 12 karakter, UTF-8'de en çok 72 bayt, bileşim şartı yok (AUTH-009). Tek yerdedir (`authn.CheckPassword`). Girişte uygulanmaz. Argon2id'de 72 bayt sınırının teknik gerekçesi kalmadı; kural iş kuralı olarak korunuyor (açık soru 1).

### 6. Personel girişi

- Ayrı token sınıfı, ayrı uçlar (`/v1/admin/auth/*`, `/v1/admin/me`). Personel token'ı pazaryerinde, pazaryeri token'ı personel yüzeyinde geçmez (D5, AUTH-015). Oturum cihaza bağlı değildir (panel cihaz göndermez, ADR-0021 §2); `staff_sessions` + rotasyonlu refresh token.
- **`token_version`:** erişim token'ı `stv` taşır, oturum açıldığı andaki sürümü saklar. Her istekte hesabın güncel sürümü, oturumunki ve token'ınki eşit olmalıdır. Çıkış ve parola değişimi sürümü artırır: hesabın bütün cihazlardaki erişim ve refresh token'ları, isteği yapan dahil, ölür (AUTH-013; Java'daki "çıkış her yerden çıkar" davranışı korunur).
- **Kilit:** art arda 5 yanlış parola 15 dakika kilitler. Kilitliyken doğru ya da yanlış hiçbir deneme sayacı ya da kilidi değiştirmez; yanıt yanlış parolayla aynıdır; başarılı giriş sayacı sıfırlar (AUTH-011). Sayaç ve kilit satır kilidi altında okunup yazılır. Sektörle karşılaştırma: OWASP sabit bir eşik önermiyor, kilitlemenin DoS'a dönüşmesine dikkat çekiyor; NIST 800-63B en çok 100 ardışık hata istiyor; Cognito 5 hatadan sonra artan süreli kilit; Entra ID ve Okta ~10 hata. PCI DSS 4.0 8.3.4 ≤ 10 deneme ve ≥ 30 dk istiyor (kart verisi Iyzico'da, kapsam dışıyız). Personel hesabı az ve K13 ile TOTP gelecek; Java'nın 5/15 değeri senaryoda bağlayıcı olduğu için korundu (açık soru 2).
- **`must_change_password`:** işaretli hesap yalnız parola değişimine ve çıkışa erişir; başka her personel ucu `403 STAFF_PASSWORD_CHANGE_REQUIRED` döner (AUTH-014). Yeni parola AUTH-009 kuralına uyar; mevcut parola istenir.
- Hesap yok, parola yanlış, hesap devre dışı ve hesap kilitli aynı `401 STAFF_LOGIN_FAILED`'dır.
- **TOTP'ye hazırlık (K13, Faz 8):** `staff_sessions.mfa_verified_at` kolonu ve token'daki `amr` bugün "yalnız parola"yı anlatır. Faz 8'de giriş, parola doğrulandıktan sonra ikinci adım ister; `amr`'a `otp` eklenir ve personel uçları `mfa_verified_at` dolu oturum ister. Şemada yeni tablo ya da token sınıfı gerekmez.

### 7. Şema ve soft-delete sınıfları

| Tablo | Sınıf | Not |
|---|---|---|
| `users` | soft_delete, pii | Kimlik çekirdeği: e-posta, parola hash'i (NULL olabilir: Faz 2'nin OTP'li hesabı), durum. Profil ve TCKN Faz 2. |
| `devices` | soft_delete, pii | Kullanıcı + `X-Device-Id`; `users`'a `cascade`. |
| `staff_members` | soft_delete, pii | Kilit, `must_change_password`, `token_version`. Roller 0040+'da. |
| `user_sessions`, `user_refresh_tokens`, `staff_sessions`, `staff_refresh_tokens`, `login_attempts`, `realtime_tickets` | M2 | Süresi dolunca hard delete. `koala_app` doğrudan yazar ve temizler. |

- Oturumların soft-delete'li ebeveynlere kenarları `detach`'tır (M2 çocuğa `cascade` yasak; `restrict` hesap silmeyi gereksiz yere tıkardı).
- Oturum açan ve cihaz ekleyen akış kullanıcı satırını `FOR UPDATE` ile kilitler. Bu, `softdelete.LockActiveParent`'ın `FOR SHARE` güvencesini (eşzamanlı silme ile "silinmiş ebeveynin aktif çocuğu" olmaz) sağlar ve aynı kullanıcının iki girişini sıralar. İkisi birden alınmaz: aynı satırda paylaşımlı kilitten özel kilide yükselme iki işlem arasında kilitlenme üretir. Personel girişi de personel satırını aynı nedenle kilitler.
- `login_attempts` adresi değil, normalize edilmiş adresin SHA-256'sını tutar; olmayan bir hesaba yapılan deneme adres saklamaz. Saklama: biten oturumlar 30 gün, denemeler 90 gün (`Service.Purge`, `StaffService.Purge`, `PostgresTickets.Purge`). Periyodik iş River worker'ları sunucuya bağlandığında eklenir.

### 8. Oran sınırı (ADR-0021 §5)

- Giriş, yenileme, personel girişi/yenilemesi ve personel parola değişimi `x-rate-limit: auth` grubundadır. Kök zincir adres başına sınırı uygular (dakikada 30, burst 10).
- Ek olarak her istek hedefinin kovasından jeton alır (`KeyCustom`, dakikada 10, burst 5): giriş için normalize adresin SHA-256'sı, yenileme için refresh token'ın SHA-256'sı, parola değişimi için personel kimliği. Böylece aynı NAT arkasındaki farklı hesaplar birbirini kilitlemez (AUTH-018). Strict server middleware'i (`authn.StrictMiddleware`) uygular; token yanıtlarına `Cache-Control: no-store` yazar (RFC 6749 §5.1).
- Kimliği doğrulanmış her istek `user:<id>` ya da `staff:<id>` adıyla kimlik başına sınıra girer.
- Kimlik doğrulama gövdeleri 4 KiB'la sınırlıdır (`x-body-limit`); aşan gövde tamponlanmadan 413 alır.

### 9. Middleware ve sözleşme

- API grubu sırası: **authn → kimlik başına oran sınırı → doğrulama** (ADR-0019, ADR-0021). Authn, route'un operasyonunun `security`'sini spec'ten okur: `[]` public, `marketplaceBearer`, `adminBearer`. Spec'te olmayan route reddedilir. 401'ler `WWW-Authenticate: Bearer` taşır (RFC 6750).
- `koala-admin-permission` lint kuralı genişler: `/v1/admin` altında adminBearer + `x-authz-permission`'ın yanında iki biçim kabul edilir: personelin yalnız kendi hesabına dokunan uç için adminBearer + `x-authz-exempt` (izin yok) ve personel girişi/yenilemesi için `security: []`. İkisi de `x-authz-permission` taşıyamaz. Prob ve geçen örnekler güncellendi.
- Yeni hata kodları `ErrorCode` listesindedir; `TestPlatformCodesAreInTheContract` `authn.Codes()`'u da denetler.

### 10. Realtime bağlantısı (ADR-0018, ADR-0022)

- `TicketStore` Postgres'tedir (`realtime_tickets`): `Put` bir `INSERT`, `Take` `DELETE … WHERE token_hash = $1 RETURNING …`. Bu, ADR-0018 §2'deki "Postgres deposu Faz 4'te" notunun yerine geçer: bilet örnekler arasında tek kullanımlıktır.
- `Sessions.Active` `user_sessions` üzerindeki aynı kontroldür; ADR-0022 §2 sözleşmesine uyar (her "hayır" `(false, nil)`, hata yalnız "bilinmiyor").
- `Authenticator` bilet isteğini her marketplace isteği gibi doğrular. `POST /v1/realtime/tickets` ve `GET /v1/realtime` `internal/app`'te bağlanır; hub `httpx.ServerOptions.Streams`'e girer. Origin listesi `KOALA_REALTIME_ORIGINS`'ten gelir. Konuşma yetkisi Faz 4'e kadar "hiç kimse katılımcı değil"dir.

### 11. Senaryo bağlama kuralı

- `test/scenarios/scenarios_test.go` README'deki CI kuralını kurar. Fazı başlamış alanda (`auth`) her P0 ya `go_test`'e ya da gerekçeli `deferred`'a bağlıdır; bağlı her test fonksiyonu var ve doc yorumunda ya da gövdesinde senaryo kimliğini taşır.
- `go_test` liste olabilir. Kuralın bir kısmını sonraya bırakan bağlar `partial` alanıyla işaretlenir.

## Sonuçlar

- AUTH kapsamı (18): 15 senaryo bağlı ve testli; 3'ü (AUTH-006, 007, 008: parola sıfırlama ve kayıt) Faz 2'ye `deferred`. Bağlı olanlardan 4'ü `partial`: AUTH-009 (kayıt, sıfırlama ve oturumda değiştirme yüzeyleri Faz 2), AUTH-011 ve AUTH-015 (TOTP Faz 8), AUTH-012 (rol iptali personel RBAC PR'ı).
- Yeni bağımlılıklar (onaylı liste, plan §3.3, K19): `github.com/golang-jwt/jwt/v5` v5.3.1 (MIT, 28.01.2026), `golang.org/x/crypto` v0.58.0 (BSD-3, 09.10.2026; `argon2`).
- Yeni değişkenler: `KOALA_JWT_MARKETPLACE_KEYS`, `KOALA_JWT_STAFF_KEYS` (staging ve production'da `serve` için zorunlu, sır), `KOALA_REALTIME_ORIGINS`. Yayın hattı (Dokploy) bu adları tanımlamalıdır; değerleri burada yazılmaz.
- Mobil ve web istemcileri refresh rotasyonuna uyarlanır: her yenilemede gelen yeni refresh token saklanır, yenileme tek uçuşta tutulur (plan §4.2).
- Personel bootstrap'i (ilk OWNER) ve personel yönetimi uçları RBAC ve Faz 8 işleriyle gelir; bugün personel satırı yalnız `InsertStaffMember` ile (test ve bootstrap için) eklenebilir.
- **Açık sorular:**
  1. Argon2id'de 72 bayt üst sınırının teknik gerekçesi yok. NIST 800-63B en az 64 karakterlik üst sınır istiyor; çok baytlı karakterlerde 72 bayt bunun altında kalabiliyor. Sınır 128 bayta (ya da 64 karaktere) çıkarılsın mı? AUTH-009 değişir.
  2. Personel kilidi PCI DSS 4.0'ın ≥ 30 dk önerisinin altında (15 dk). TOTP gelince 5/15 yeterli mi, yoksa artan süreli kilide mi geçilsin?
  3. Refresh rotasyonunda grace süresi: mobilde yenilemenin ağ tekrarı oturumu kapatıyor. Sorun çıkarsa birkaç saniyelik bir "aynı token'ın tekrarı aynı yanıtı alır" penceresi eklenebilir.
  4. Marketplace oturumu için 90 günlük mutlak tavan bir ürün kararıdır; mobilde 90 günde bir yeniden giriş istenmiyorsa artırılabilir.
