> **Not:** N01–N08 notları repo dışında. Bu dosya `GO-GECIS-DEGERLENDIRME.md`'nin 04.11.2026 tarihli kopyasıdır.

# Koala backend: Java/Spring Boot → Go — değerlendirme ve aşamalı plan

Tarih: 04.11.2026. Hazırlayan: Claude (sentez). Kod yazılmadı, hiçbir repoya dokunulmadı.
Kaynak: yedi analiz notu ve `koala-backend` origin/main anlık görüntüsü (`2ca84245`, v0.9.31). Bu belgede sır, env değeri, anahtar ya da merchant id yok.

**Atıf kısaltmaları**

| Kısaltma | Dosya |
|---|---|
| N01 | `01-api-guvenlik-realtime.md`: 266 uç, sözleşme, JWT, personel izinleri, STOMP, kaynak düzeyi yetki envanteri |
| N02 | `02-veri-katmani-liquibase.md`: entity/tablo, soft-delete, kilitler, transaction, Liquibase |
| N03 | `03-is-mantigi-para-async-entegrasyon.md`: Iyzico, para durum makinesi, Rabbit, zamanlanmış işler, entegrasyonlar, NFC |
| N04 | `04-test-ops-belgeler.md`: testler, yayın, ops, performans kanıtı, git |
| N05 | `05-referans-core-backend-authz.md`: sky lab core-backend mimarisi |
| N06 | `06-istemci-etkisi.md`: mobil, web ve admin uyarlama maliyeti |
| N07 | `07-authz-sektor-kaynakli.md`: authz sektör araştırması; `[07-n]`, N07 kaynakçasındaki n numaralı kaynak |
| N08 | `08-nfc-go-ve-soft-delete.md`: NFC (PA/AA) Go fizibilitesi ve varsayılan soft-delete tasarımı (K14/K15 sonrası) |
| `k/` | `src/main/java/com/exskylab/koala/` |

Notlar `~/Library/Application Support/Claude/side-session-notes/local_797f2d35-…/` altında. "Doğrulandı" diye işaretlenen her iddiayı 04.11'de anlık görüntüden okuyarak kontrol ettim. "Doğrulanmadı" diye işaretlenenler kod okumasından çıkan çıkarımlardır.

---

## 0. Yönetici özeti

1. Java backend'in boyutu: 63.643 satır ana kod, 266 HTTP uç, 84 entity ve 86 tablo, 317 Liquibase changeset'i, 17 zamanlanmış iş, 5 Rabbit kuyruğu, 2.776 test metodu. Bunun 978'i entegrasyon testi [N03 §0, N04 §1.1].
2. Repoda Go'ya geçişi gerektiren bir performans ya da bellek sorunu kayıtlı değil. Ölçülmüş tek somut maliyet 84 sn'lik açılış süresi; bunun ~22 sn'si Yauaa'dan geliyor [N04 §4]. Bu yüzden Go'ya geçişin gerekçesi performans değil. Gerekçe şunlar: tek ikili, sky lab ile ortak yığın, daha hızlı test döngüsü, prodda olmadığımız için bedelin en düşük olduğu an olması ve API ile authz'ı baştan doğru kurma fırsatı.
3. Önerilen yol, Yusuf'un kararlarıyla aynı:
   - Go'da baştan yazım, hazır olunca Java'nın yerine tek seferde konur.
   - Temiz şema; goose ile baseline alınır.
   - OpenAPI 3.1 sözleşme-önce `/v1` API'si.
   - Eşitlik hedefi iş kuralı eşitliği: Java testleri senaryo kataloğuna dönüştürülür, para kuralları için Java'yı oracle alan karar tabloları ve Iyzico altın vektörleri kullanılır.
4. Altyapı sadeleşir:
   - RabbitMQ kaldırılır. Yerine Postgres tabanlı iş kuyruğu gelir (River ya da elle yazılmış `SKIP LOCKED` worker'ı).
   - STOMP yerine düz WebSocket ve JSON olaylar kullanılır.
   - Yauaa kaldırılır; cihaz kimliği yalnız `X-Device-Id` başlığından alınır.
5. Authz ayrı bir servis olmaz; uygulama içinde tek bir karar noktası kurulur (`Check/Scope/Capabilities` + ayrı `PIIGate`). Kurallar OpenFGA DSL alt kümesinde yazılır. N07'deki hüküme göre tasarım sektörle genel olarak uyumlu. İleride ayrı bir servise geçiş "orta" zorlukta; liste süzme (`Scope`) yerelde kalırsa "kolay-orta" [N07 §8].
6. Büyüklük, ajan desteği olmadan tek kişinin seri çalıştığı varsayımıyla:
   - Backend 46–62 kişi-hafta (NFC ve varsayılan soft-delete dahil, karar kaydı K14/K15).
   - İstemciler 14,5–22,5 kişi-hafta [N06 §0, N08].
   - Toplam ≈ 61–84 kişi-hafta.
   - Takvim, Eylül 2026'nın ajan temposuyla yaklaşık 4–6 ay olabilir. Bu tahmin yargıya dayanıyor (§6.4).
7. En riskli üç alan: para yolu (emanet ve hakediş), KVKK ve PII, kimlik doğrulama ile authz. Her birinin ayrı koruma yöntemi var (§6.3).
8. Go'ya geçişten önce bir iş kararı vermen gerekiyor: payout `COMPLETED` semantiğinde belge ile kod çelişiyor (§8, madde 12). Bu karar verilmeden para yolunun eşitlik hedefi tanımlanamaz.
9. Java tarafı için önerim: dondur. Karar içeren açık PR'ları (#341, #344, #350, #351, #352) yalnız referans olsun diye merge et. KVKK bulgularını Go'da tasarımla kapat (§7).

### 0.0 Karar kaydı (04.11.2026, Yusuf ile)

Aşağıdaki kayıt §0.1 tablosundan önce gelir; ikisi çeliştiğinde **bu kayıt geçerlidir**. Teknik seçimlerde ilke: "sektörde nasılsa öyle" (Yusuf).

| # | Karar | Gerekçe (sektör) |
|---|---|---|
| K1 | **Go'da baştan yazım.** Koşullar kabul edildi: lansman 5–6 aydan uzak, yazım süresince Java'da yeni özellik yok. | — |
| K2 | **Payout durumları Stripe Connect modelinde:** `PENDING → IN_TRANSIT` (Iyzico APPROVE) `→ PAID` (banka ekstresi mutabakatı); ayrıca `FAILED`, `RETURNED`, `CANCELED`. `PAID`'den sonra iade gelirse `RETURNED`. Iyzico'da `PAID`'i hangi raporun kanıtladığı sandbox'ta doğrulanacak. | Stripe payout: pending / in_transit / paid / failed / canceled; "paid" sonrası iade mümkün. Sağlayıcı onayı ile paranın hesaba geçmesi ayrı durumlardır. |
| K3 | **River** (Postgres iş kuyruğu). RabbitMQ kaldırılır. İkinci bir servis çıkarsa broker o gün eklenir. | Tek uygulama + tek DB'de Postgres tabanlı kuyruk varsayılan: Rails 8 Solid Queue, Elixir Oban, Go River. Rabbit/Kafka servisler arası olay için. |
| K4 | **Düz WebSocket + JSON olay protokolü.** | Slack, Discord, WhatsApp Web kendi olay protokollerini düz WebSocket üzerinde kullanıyor. |
| K5 | **İçeride `int64` kuruş + para birimi.** API'de de Stripe/Adyen biçimi: `{"amount": 15000, "currency": "TRY"}`. `"150.00"` biçimine çeviri yalnız Iyzico sınırında. Oranlar baz puan (1250 = %12,5). **§0.1'deki "JSON'da string ondalık" önerisinin yerine geçer.** | Stripe, Adyen, Square tutarı en küçük birimde tam sayı taşır; Dart/JS float tuzağı kalkar. |
| K6 | **goose.** | — |
| K7 | **pgx + sqlc**; dinamik admin aramaları elle pgx. | Go'da yerleşik yaklaşım ORM değil, "SQL yaz, tipli kod üret". |
| K8 | **chi** (net/http uyumlu). oapi-codegen `chi-server` hedefiyle. **§0.1'deki stdlib önerisinin yerine geçer.** | Go üretim servislerinde en yerleşik net/http router'ı; route grupları ile `/v1/admin` middleware zinciri ayrışır. |
| K9 | **oapi-codegen** (3.1 desteği Faz 0 spike'ında doğrulanır; eksikse ogen). | — |
| K10 | **Her yerde cursor** ("önceki/sonraki"); admin listelerinde isteğe bağlı `total`. | Stripe, Shopify Admin API, GitHub, Slack cursor kullanıyor; Stripe Dashboard ve Shopify Admin arayüzü de önceki/sonraki ile çalışıyor. |
| K11 | **RFC 3339 ofsetli zaman + `timestamptz` + ilanın saat dilimi.** | Standart. |
| K12 | **15 dk erişim token'ı + refresh rotasyonu + yeniden kullanım tespiti.** | OAuth 2.0 Security BCP. |
| K13 | **Personel için zorunlu MFA (TOTP) v1'de.** | Yönetim panellerinde MFA zorunluluğu yerleşik pratik; `read_pii` ve `payout.release` tek parolayla korunmamalı. |
| K14 | **Varsayılan soft-delete VAR** (Yusuf, 04.11). Bugünkü gibi iş tablolarında `deleted_at`; Go'da örtük filtre olmadığı için açık korumalarla (§3.6'ya bakın). **§0.1'deki "b" önerisinin yerine geçer.** *Tasarım özeti:* kolonlar `deleted_at` + `deletion_id` (kaskad grubu); `active_<t>` view'ları; uygulama DB rolünün temel tablolarda yetkisi yok; JOIN politika kaydı; şema güdümlü üretilmiş testler. Gerekçeli 6 muafiyet sınıfı var; para ve kanıt tabloları "silinmez". KVKK silmesi ayrı bir anonimleştirme ve temizlik adımıdır. Ayrıntı N08 §B. | Yusuf'un kararı. EF Core, Hibernate ve Rails'teki global filtrelerin INNER JOIN tuzağı belgeli (N08 §B1). |
| K15 | **NFC çip doğrulaması (PA/AA) v1 İÇİNDE** (Yusuf, 04.11). Go'da uygulanabilirliği ve büyüklüğü ayrıca değerlendiriliyor (§1.5, §6.1). **§0.1'deki "a" önerisinin yerine geçer.** *Tasarım özeti:* Go'da saf uygulama: stdlib + `x/crypto/cryptobyte`, kendi CMS/SOD ayrıştırıcısı, explicit EC → adlı eğri eşlemesi, elle ISO 9796-2. Yan servis yok. TCKK zinciri RSA-PSS kökte. Explicit parametreli 8 sertifika pasaport CSCA'sı ve P-521; Brainpool TCKK yolunda görülmedi. "SOD DG15 listeliyorsa AA zorunlu." Faz 2b, 2–3 kh. Ayrıntı N08 §A. | Yusuf'un kararı. Sektör kalıbı: mobil okur, sunucu PA/AA doğrular (ReadID, Regula; N08 §A3). |
| K16 | **Java dondurulur (yeni özellik yok); #341, #344, #350, #351, #352 `verify` yeşilse merge edilir. Güncelleme (Yusuf, 04.11): #352 merge olduktan sonra v0.9.32 yayımlanır** (#349 dahil olduğu için T2 konum sızıntısı riski kalkıyor; pre-prod Go hazır olana kadar güncel kalır). Zinciri üst orkestrasyon oturumu yürütüyor. | — |
| K17 | **Şirket rolleri v1'de ADMIN/STAFF.** | İş kuralı eşitliği; yeni roller ürün kararı. |
| K18 | **Go kodu yeni repoda**; Java repo geçişten sonra salt-okunur arşiv. | — |
| K19 | **§3.3 bağımlılık listesi onaylandı** (chi dahil; Yusuf, 04.11). NFC: `x/crypto/cryptobyte` onaylandı; `gmrtd` yalnız test için ve lisans koşuluyla onaylandı; `brainpool` gerekmiyor. Soft-delete istisnaları (M1 silinmez, teknik tablolar süreyle silinir, hesap silmede 30 gün içinde anonimleştirme) onaylandı. | Çalışma anlaşması: yeni bağımlılık onaysız eklenmez. |

### 0.1 Vermen gereken kararlar

| # | Karar | Seçenekler | Önerimiz | Neden |
|---|---|---|---|---|
| K1 | Yeniden yazım yapılsın mı? | A) Go yeniden yazım · B) Java'da kal, yalnız API'yi ve authz'ı yeniden tasarla · C) Java'yı olduğu gibi yayımla | **A**, iki koşulla: lansman en az 5–6 ay uzakta olmalı ve yazım süresince Java'da özellik dondurulmalı. Koşullar tutmuyorsa **B** | §2. Prodda olmadığımız için bedel bugün en düşük. Performans gerekçesi yok. |
| K2 | Payout `COMPLETED` ne zaman yazılır? | a) Iyzico APPROVE yanıtında referans varsa (bugünkü kod) · b) yalnız banka ekstresi mutabakatıyla (CLAUDE.md §2y/§2y3) · c) iki ayrı durum: `PROVIDER_APPROVED` ve `SETTLED` | **c** | APPROVE, talimatın kabul edildiğini söyler; paranın hesaba geçtiğini söylemez [N03 §2.4]. İki durum hem işçi ekranını hem kanıt ilkesini korur. |
| K3 | RabbitMQ kalsın mı? | a) Kalsın (`amqp091-go`) · b) Postgres iş kuyruğu: River · c) Elle yazılmış outbox ve `SKIP LOCKED` worker'ları (core-backend kalıbı) | **b**, olmazsa **c** | Bütün üreticiler ve tüketiciler aynı süreçte. Rabbit yalnız bir atlama ekliyor. Bugün 5 ayrı claim/fencing uygulaması var [N03 §7.1-6]. |
| K4 | Realtime protokolü ne olsun? | a) STOMP'u Go'da yeniden kurmak · b) Düz WebSocket ve JSON olaylar · c) SSE ve REST | **b** | Go'da olgun bir STOMP sunucusu yok [N01 §8.1-3]. Sözleşme zaten değişiyor. Sunucu-itmeli model, istemcinin sahte olay basabildiği açık sınıfını tümden ortadan kaldırır. |
| K5 | Para tipi ne olsun? | a) `shopspring/decimal` · b) `int64` kuruş + ölçek-4 oran tipi | **b** | Dış bağımlılık gerekmez. `"100.00"` → `"100"` tuzağı ortadan kalkar [N03 §7.2-2]. HALF_UP kuralı tek fonksiyonda kalır. |
| K6 | Migration aracı ne olsun? | goose · golang-migrate · atlas · core-backend'in kendi motoru | **goose** | `$$` blokları ve `NO TRANSACTION` desteği var, `go:embed` ile gömülür. Kendi motorumuz 1,8 K satırlık gereksiz bir yük olur [N05 A5-5]. |
| K7 | DB erişimi nasıl olsun? | pgx ve elle SQL · pgx + sqlc · ORM (GORM/ent) | **pgx + sqlc**. Dinamik admin aramaları elle pgx ile yazılır. | sqlc, sorgu ile kolon uyumsuzluğunu derleme zamanında yakalar [N02 §5]. ORM yeniden Hibernate'in örtük davranışlarını getirir. |
| K8 | Router ne olsun? | stdlib `ServeMux` · chi · fiber | **stdlib** (gerekirse chi) | Bağımlılık gerekmez; `r.Pattern` metrik etiketi için yeterli. fiber `net/http` ekosisteminden kopuk [N05 A5-7]. |
| K9 | Sözleşmeden Go kodu üretilsin mi? | oapi-codegen (strict server) · ogen · elle yazım | **oapi-codegen**, ama önce OpenAPI 3.1 desteği spike ile doğrulanacak (Faz 0) | Tarihsel olarak oapi-codegen ve kin-openapi 3.1'i tam desteklemiyordu. **Doğrulanmadı**, §9 R4. |
| K10 | Admin listelerinde sayfalama nasıl olsun? | a) Her yerde cursor + isteğe bağlı `total` · b) `/v1/admin/**`'da offset istisnası | **a**, admin UX'i "önceki/sonraki + toplam" olur | Admin'de 54 dosya numaralı sayfa kullanıyor [N06 §3.2]. Tek kural daha temiz. Numaralı sayfa şartsa b seçilir. |
| K11 | İş başlangıç/bitiş saatleri nasıl gönderilsin? | a) Dilimsiz duvar saati (bugünkü) · b) RFC 3339 ofsetli + `timestamptz` + ilanın saat dilimi | **b** | Mobilde 56, web'de 51 referans var. Sessiz hatanın en olası kaynağı burası, o yüzden tek seferde düzeltilmeli [N06 §4-6]. |
| K12 | Oturum modeli nasıl olsun? | a) Bugünkü gibi 12 sa erişim token'ı, rotasyonsuz refresh · b) 15 dk erişim token'ı + refresh rotasyonu + yeniden kullanım tespiti | **b** | İstemciler zaten uyarlanıyor. Bugünkü model OAuth güvenlik pratiğinin gerisinde [N01 §3.3]. |
| K13 | Personele MFA eklensin mi? | Yok (bugünkü) · TOTP · WebAuthn | **TOTP**, v1'de, Faz 8 | `user.read_pii` ve `payout.release` izinleri tek parolayla korunuyor [N01 §3.5]. Bu benim değerlendirmem, kapsamı genişletiyor. |
| K14 | Soft-delete politikası ne olsun? | a) Her tabloda (bugünkü, 66 tablo) · b) Varsayılan yok; yalnız listelenen tablolarda açık durum kolonu | **b** | #309 ve #321'deki join tuzağı sınıfı ortadan kalkar [N02 §1.3]. |
| K15 | NFC çip doğrulaması (PA/AA) ne olsun? | a) v1 dışı, ertele · b) Küçük bir Java yan servisi · c) Go'da elle yazım | **a** | Prodda kapalı. Go'da hazır karşılığı yok [N03 §5.1]. Ürün yolu fotoğraf ve personel onayı. |
| K16 | Java'daki açık işler ne olsun? | §7'deki A/B/C | **A**: dondur, karar içeren PR'ları merge et | Go için referans olur. |
| K17 | Şirket rolleri | Bugünkü `ADMIN/STAFF` · owner/admin/recruiter/viewer | **Bugünkü** (v1) | İş kuralı eşitliği. Yeni roller ürün kararıdır [N01 §9.c, N05 B6]. |
| K18 | Go kodu hangi repoda dursun? | Yeni repo (ör. `is-koala/koala-api`) · `koala-backend` içinde yeni kök | **Yeni repo**. Java repo, geçişten sonra salt-okunur arşiv olur. | Temiz git geçmişi, temiz CI. Java referans olarak yerinde kalır. |
| K19 | Yeni bağımlılık listesi | §3.3 | Onaya sunuldu | Her bağımlılık senin onayına bağlı. |

---

## 1. Envanter

### 1.1 Kod ve paketler

| Ölçü | Değer | Kaynak |
|---|---|---|
| Ana kod (`src/main/java`) | 1.119 dosya, 63.643 satır | N03 §0 |
| `business/` | 164 dosya (80 arayüz + 84 manager), 20.505 satır; concretes 18.353 | N03 §0 |
| `webAPI/` | 48 controller, 7.472 satır | N01 §1.1 |
| DTO | 248 dosya, 6.382 satır (91 istek, 155 yanıt) | N01 §7 |
| `core/` | utilities 3.768 · properties 2.987 · helpers 2.876 · configs 2.435 · security ~1.100 · schedulers 648 · consumers 464 · privacy 460 · realtime 412 · logging 328 · exceptions 299 · producers 219 · events 131 | N01 §7, N03 §0 |
| Mesaj kataloğu | `messages_tr` ve `messages_en`, her biri 842 anahtar | N01 §2.4 |
| E-posta şablonu | Thymeleaf, 17 dosya, 529 satır | N03 §0 |
| Git | 482 commit. Eylül 2026'da 373. Ekim başından beri main'e merge yok. | N04 §5 |

### 1.2 REST uçları (266)

| Kitle | Uç | Not |
|---|---|---|
| Anonim / public | 33 | `/auth/*` 7, `/reference/**` 8, ilan okuma, sitemap, public profil parçaları |
| Webhook / callback / token linki | 4 | Iyzico callback GET+POST, bildirim pikseli, abonelikten çıkma |
| Personel, anonim | 2 | `/admin/auth/login`, `/refresh` |
| Personel (`/admin/**`) | 81 | 79 `@RequiresPermission` |
| Kimlikli kullanıcı | 146 | İşçi ve işveren ayrımı yok, ikisi de `ROLE_USER`. Ayrım manager'daki sahiplik kontrolünde. |

Metot dağılımı: GET 123, POST 89, DELETE 27, PUT 22, PATCH 5. Diğer özellikler [N01 §1]:
- 10 multipart uç ve 8 indirme ucu var.
- SSE yok, `/v1` öneki yok.
- Admin dışı 183 ucun yaklaşık 41'i işverene, 42'si işçiye ait; ~100'ü ortak.
- En büyük controller'lar: `AdminController` 28 uç / 717 satır, `CompaniesController` 17.

### 1.3 Veri

| Ölçü | Değer | Kaynak |
|---|---|---|
| Entity / tablo | 84 `@Entity`, 86 tablo; 9 tablonun entity'si yok | N02 §0 |
| `BaseEntity` (soft-delete) | 66 tablo, hepsinde `@SQLDelete` + `@SQLRestriction` | N02 §1.3 |
| DAO | 82 DAO, 461 metot (311 türetilmiş, 94 JPQL, 56 native) | N02 §3 |
| `@EntityGraph` / LAZY | 95 / 142; OSIV açık | N02 §2 |
| `@Transactional` | 462; REQUIRES_NEW 12, MANDATORY 5, noRollbackFor 2, AFTER_COMMIT 5 + elle 3 | N02 §4 |
| Kilit | 10 `PESSIMISTIC_WRITE`, 6 advisory-lock yeri, havuz dışı bir session lock. `SKIP LOCKED` ve `@Version` hiç yok. | N02 §3 |
| Liquibase | 97 XML + 6 CSV, 317 changeset, 96 include; 4 trigger, 4 fonksiyon; ~41–45 kısmi unique indeks; yalnız `unaccent` extension | N02 §5 |
| JSONB / PostGIS / PG enum | Hiçbiri yok | N02 §0 |
| Hassas düz metin | TCKN `users.tc_identity_number` (`User.java:60`, doğrulandı) ve `user_verifications` | N02 §1.4 |

### 1.4 Asenkron işler ve zamanlanmış görevler

- **Rabbit kuyrukları** [N03 §3.1]: `email.queue` ve DLQ'su, `sms.queue`, `push.queue` ve DLQ'su, `user.profile.updated.for.sync.queue`, `notification.delivery.busy.parking.queue`.
  - Parking kuyruğunun tüketicisi yok. Doğrulandı: koddaki 4 `@RabbitListener`'ın hiçbiri onu dinlemiyor.
  - 4 mesaj tipi var. `UserUpdatedEventDto` TCKN, IBAN ve telefonu düz metin taşıyor (doğrulandı).
- **Outbox'lar**: `notifications`, `user_profile_sync_outbox`, `settlement_tasks`, `account_media_deletion_outbox`, `iys_consent_tasks`. Bunlar beş ayrı claim/fencing/backoff uygulaması; ortak bir soyutlama yok [N02 §4, N03 §7.1].
- **Zamanlanmış işler**: 17 `@Scheduled` [N03 §4]. Bunlardan 5'i para yolunda: ödeme mutabakatının 2 süpürmesi, settlement, otomatik tamamlama, ödenmemiş atamanın iptali. ShedLock yok; sistem tek örnek varsayıyor.

### 1.5 Dış entegrasyonlar

| Entegrasyon | Bugün | Go karşılığı | Zorluk |
|---|---|---|---|
| Iyzico: ödeme, emanet (approve/disapprove), iade, alt üye | Elle HTTP, `IyzicoPaymentService` 463 satır, 7 uç, IYZWSv2 imzası + yanıt imzası | Elle istemci (~500 satır) + altın vektörler | Orta; para |
| FCM push | `firebase-admin` 9.10.0, yalnız `send` | FCM HTTP v1 + `x/oauth2/google` (SDK gerekmez) | Düşük |
| E-posta | JavaMail SMTP + Thymeleaf 17 şablon, izleme pikseli | `go-mail` + `html/template` | Düşük-orta (HTML diff) |
| SMS / OTP | İleti Merkezi REST; OTP yerelde üretiliyor | Düz JSON POST | Düşük |
| İYS | İleti Merkezi proxy; alıcı AES-256-GCM `v1:` biçiminde | `crypto/aes` + GCM | Düşük |
| Google Places / Geocoding | REST, field mask, session token | Düz REST | Düşük |
| GeoIP | MaxMind City DB | `geoip2-golang` | Düşük |
| User-Agent | Yauaa; cihaz kimliğinin parçası, açılışa 22 sn ekliyor | **Kaldırılır** (§3) | — |
| Nesne depolama | AWS SDK S3 (R2), presign | `aws-sdk-go-v2` | Düşük |
| Görsel / EXIF | ImageIO ile yeniden encode; `ExifOrientation` 214 satır; webp izinli ama okunamıyor | `image/jpeg` ve `png` + EXIF yön | Düşük-orta |
| NFC (PA/AA) | BouncyCastle; 11 dosya, ~1.000 satır; prodda kapalı | **K15: v1'de.** Saf Go: stdlib `x509`, `rsa` (PKCS#1 + PSS), `ecdsa`, `encoding/asn1` + `x/crypto/cryptobyte`. Kendi SOD/CMS ayrıştırıcısı. Explicit EC → adlı eğri eşlemesi (8 pasaport CSCA'sı P-521). Elle ISO 9796-2 (~150 satır). Brainpool v1'de fail-closed. Toplam ~1.300–1.600 satır. Hazır Go paketleri (pkcs7 fork'ları, `gmrtd`) uygun değil [N08 §A2] | Yüksek; Faz 2b, 2–3 kh |
| Hata izleme / metrik | Sentry, Micrometer/Prometheus, 18 alarm kuralı | `sentry-go`, `client_golang` | Düşük |
| Yayın | `publish-ghcr-image.sh` sürümü pom'dan okuyor; tag ile Dokploy webhook | Sürüm git tag'den | Düşük |

Kaynaklar: N03 §5, §6; N04 §2.

### 1.6 Güvenlik

- **JWT** [N01 §3]: HS256, tek anahtar, 5 token türü (marketplace, STOMP, parola belirleme, personel erişimi, personel refresh).
  - `iss` doğrulanmıyor.
  - Token rol taşımıyor; roller her istekte DB'den okunuyor.
  - Her istekte oturum, cihaz eşleşmesi ve `last_accessed` kontrol ediliyor; bu istek başına ~2–4 sorgu demek.
- **Refresh** [N01 §3.3]: opak, BCrypt'li, rotasyonsuz. Süre 30 gün, 7 günlük kayan pencere. Cihaz başına tek oturum.
- **Personel** [N01 §3.6, §4.2]:
  - Ayrı tablo; `token_version` ile iptal; 5 hatalı denemede 15 dk kilit.
  - 25 izin ve 10 rol. Roller DAG ile birbirini kapsıyor. Görev ayrılığı uygulanıyor.
  - `@RequiresPermission` fail-closed.
- **KVKK** [N01 §4.3, §4.4]:
  - `user.read_pii` 4 uçta ve bir dinamik kapıda.
  - Maskeleme: IBAN, adres, `SafeUserDto`.
  - Log maskeleme var.
  - Denetim kaydı `admin_audit_entries`: 24 ay saklanıyor (finansal kayıtlar 10 yıl), ayrıntılar 6 ay.
- **Rate limit** [N01 §4.5]: 8 kova, bellek içi, IP'ye göre.
- **MFA ve hesap kilidi**: MFA yok. Kullanıcı hesabında kilit yok.

### 1.7 Realtime

STOMP `/ws`, bellek içi simple broker [N01 §5]:
- 3 hedef: `/topic/conversations/{id}`, `/user/queue/messages`, `/topic/presence/{id}`. Ayrıca 1 SEND handler: typing.
- CONNECT'te 90 sn'lik credential ya da normal token kabul ediliyor. Cihaz eşleşmesi yapılmıyor.
- Abonelik reddi `ERROR` karesiyle değil, `MESSAGE type:ERROR` olarak dönüyor.

### 1.8 Testler

| Küme | Sınıf | Test | Not |
|---|---|---|---|
| IT (Testcontainers PG) | 128 | 978 | 65 sınıf / 692 test HTTP üzerinden; 63 sınıf HTTP'siz [N04 §1.3] |
| Birim | ~253 | 1.798 | Mockito'ya ve Java'ya özgü |
| Yapısal koruma (kaynağı tarayan) | ~40–44 | — | `ApiConventionTest`, `LogPrivacyTest`, `SoftDeleteCoverageTest`… Go'da sıfırdan yazılır [N04 §1.4] |
| Runbook SQL IT'leri | 5 | — | Dile bağımsız, korunabilir |

IT'lerin alanlara dağılımı: admin 248 test, marketplace 210, ödeme 119, kimlik 104, platform 92, bildirim 89, mesajlaşma 68, KVKK 48 [N04 §1.5].

### 1.9 Karmaşıklık sıcak noktaları

| Dosya | Satır / dal | Neden |
|---|---|---|
| `TransportManager` | 1.358 / 125 | En büyük birim; koltuk kapasitesi kilidi, QR biniş |
| `JobAssignmentManager` | 801 / 52 | `initiatePaymentSession` ~230 satır, iç içe tx, 3 sağlayıcı çağrısı |
| `PaymentManager` | 591 / 62 | Deneme FSM'i, `reconcile` karar zinciri |
| `UserManager`, `JobManager` | 657'şer | Profil kilidi, ilan yaşam döngüsü |
| `IyzicoPaymentService` | 463 / 48 | Bayt düzeyinde imza |
| `SettlementManager` + `PayoutManager` | 288 + 416 | Outbox, kısmi iade sırası, belge ile kod çelişkisi |
| `PostgresAccountAnonymizer` | 366 | 38 tabloya dokunan ham SQL |
| Sahiplik kontrolleri | ~130–150 nokta / ~43 dosya | 50'den fazla farklı adlı guard, merkezi değil [N01 §9.b] |

Kaynaklar: N03 §1.1, N01 §7, N02 §3.

### 1.10 Notlar arası sayı çelişkileri

| Konu | Değerler | Yorum |
|---|---|---|
| Uç sayısı | 266 (N01, metot düzeyinde ölçülmüş) · ~311 `@*Mapping` (N05, kaba) · "214 uç" (N03 §6, açıklamasız) | **266 esas alındı.** N05 sınıf düzeyindeki `@RequestMapping`'leri de saymış görünüyor. |
| Entity sayısı | 84 (N02, ölçülmüş) · ~132 (N05, kaba) | **84 esas alındı.** |
| Ham SQL | 98/317 changeset (N02) · 193 `<sql>` bloğu (N04) | Birimler farklı (changeset ile blok); çelişki değil. |
| Outbox sayısı | 3 + 1 (N02) · 5 aile (N03) | Tanımlar farklı; bu belgede 5 kullanıldı. |
| Sahiplik kontrolü | ~130–150 nokta (N01) · ~35 `throw …Forbidden` (N05) | Ölçüler farklı; N01'in birleşik grep'i esas alındı. |
| IT sayısı | 128 (N02, N04) · CLAUDE.md'de "36" | CLAUDE.md bayat [N04 §1.2]. |

---

## 2. Neden Go? Dürüst değerlendirme

### 2.1 Kanıt ne diyor

- Repoda performans, gecikme ya da bellek sorunu belgelenmemiş [N04 §4]:
  - Yük testi, p95 ölçümü ve kutu boyutu kaydı yok.
  - Virtual threads, AppCDS ve native-image hiç denenmemiş.
- Ölçülmüş tek maliyet 84 sn'lik açılış süresi; 22 sn'si Yauaa'dan. Healthcheck `start-period` 150 sn. Bu sorun Java'da ucuz yollarla da çözülebilir: Yauaa'yı tembel yüklemek ya da AppCDS.
- Test takımı Pi'de bellek ve CPU baskısı yaratıyor; Failsafe bu yüzden 2 fork'la koşuyor [N04 §1.2]. Bu Mac'te de `mvn verify` ağır bir iş.

Sonuç: "Go daha hızlı" gerekçesi bu projede kanıtsız. Karar performansa dayanmamalı.

### 2.2 Go'nun gerçek kazanımları

| Kazanım | Ağırlık | Not |
|---|---|---|
| Sky lab core-backend ile ortak yığın | Yüksek | Aynı kalıplar ve aynı lint/test kültürü; ajanlar iki proje arasında aynı alışkanlıklarla çalışır [N05]. |
| Tek statik ikili, distroless, <1 sn açılış | Orta | Healthcheck basitleşir, deploy hızlanır. Açılış süresi tipik değerdir, Koala için ölçülmedi. |
| Bellek | Orta | JVM'nin 1.200 MiB sınırı [N04 §2.2]. Go servisleri tipik olarak onlarca MB'tır; Koala için ölçülmedi. Tek Hetzner kutusunda DB ve Rabbit'e yer açar. |
| Test döngüsü | Orta-yüksek | Test başına izole DB ve `-race` ile koşan Go testleri, Spring context açan IT'lerden çok daha ucuz. Bu Mac'in yük sorununa doğrudan iyi gelir. |
| Açıklık | Yüksek | Spring'in örtük davranışları (ResponseBodyAdvice, `@SQLRestriction`, REQUIRES_NEW, OSIV, dirty checking) Go'da açık kod olur. Repodaki baskın kusur sınıfı, yani sessiz geçersiz kılma, tam bu örtüklükten besleniyor [N02 §2, N03 §7.2]. |
| Temiz başlangıç | Yüksek | API, şema ve authz baştan doğru kurulur. Prodda olmadığımız için bu bedel bugün en düşük; lansmandan sonra katlanır. |

### 2.3 Maliyet ve riskler

1. **İş gücü**: ≈ 61–84 kişi-hafta, istemciler dahil (§6.4).
2. **Test ağının kaybı**:
   - 2.776 testin yaklaşık %75'i Go'da yeniden yazılmalı.
   - Mevcut HTTP IT'leri, yeni API başka olduğu için doğrudan koşturulamaz [N04 §6].
   - ~40 yapısal test ve CLAUDE.md'nin 5.600 satırlık kural korpusu Spring'e bağlı.
3. **Para yolunda sessiz davranış kayması** [N03 §7.2-1]:
   - Kurallar 6'dan fazla yere dağılmış.
   - Iyzico akışı sandbox'ta uçtan uca hiç tamamlanmadı; yani referans davranışın kendisi kesin değil.
4. **Özellik donması**: Yeniden yazım süresince Java'da yeni iş yapılırsa iki kod tabanı birbirinden uzaklaşır.

### 2.4 Hangi koşulda değmez

- Lansman 3–4 aydan yakınsa. Bu durumda K1-B ya da K1-C seçilmeli.
- Tek amaç açılış süresi ya da bellekse. Yauaa düzeltmesi ve AppCDS çok daha ucuz.
- Yazım süresince Java'da özellik geliştirmeyi durduramıyorsan.
- Asıl istenen yalnız API'yi yeniden tasarlamak ve authz'ı tek yerde toplamaksa. K1-B bunu Java'da da sağlar: backend tarafı kabaca yarı maliyetle (yargı), istemci maliyeti ise aynı 14–21 kişi-hafta.

Karar senin. Benim okumam: prod yok, sky lab'la ortak yığın değerli, ve örtük davranışların açık koda dönmesi kusur sınıfına doğrudan iyi geliyor. Bu yüzden K1-A'yı makul buluyorum. Yalnız iki koşula bağlı: takvim payı olmalı ve Java'da özellik donması kabul edilmeli.

---

## 3. Hedef mimari

### 3.1 Dizin iskeleti

```
koala-api/
  api/openapi.yaml               # tek doğruluk kaynağı (bileşenler api/components/*.yaml)
  cmd/koala/main.go              # ≤100 satır: alt komutlar serve | migrate | healthcheck
  internal/
    app/                         # wiring: modül başına wireIdentity(), wirePayment()...
    platform/                    # alan bilgisi taşımayan ortak altyapı
      config/  log/  db/ (pool, tx, querier)  httpx/ (middleware, problem, ratelimit)
      apperr/  clock/  ids/  money/  metrics/  jobs/ (River sarmalayıcısı)  storage/  mailer/
    authn/                       # JWT, oturum, cihaz, personel girişi, MFA
    authz/                       # model.fga, değerlendirici, Scope, Capabilities, uygunluk testleri
    pii/                         # PIIGate, şifreli alanlar, maske projeksiyonları
    identity/   company/   job/   hiring/ (başvuru, atama, puantaj, iptal, değerlendirme)
    transport/  messaging/ (+realtime)  notification/  payment/ (deneme, emanet, payout, iade, settlement)
    dispute/    privacy/ (rıza, silme, anonimleştirme, İYS)  staff/ (roller, denetim, notlar)
    reference/  media/  sitemap/
      └─ her alan: domain.go (tip, kural, FSM) · service.go (+ kullanan tarafta küçük arayüzler)
                   store.go (sqlc sarmalayıcısı) · http.go (üretilmiş strict arayüzün uygulaması)
    integrations/  iyzico/  fcm/  smtp/  iletimerkezi/ (sms, iys)  googlemaps/  geoip/
  db/migrations/*.sql            # goose; 0001 baseline, 0002 referans seed
  db/queries/<alan>/*.sql        # sqlc girdisi
  testdata/iyzico/*.json         # altın vektörler (Java ile ortak)
  test/scenarios/*.yaml          # senaryo kataloğu (§6.2)
```

### 3.2 Katmanlar ve kurallar

1. **Bağımlılık yönü**: `http → service → store`. `domain.go` içinde `net/http`, `pgx` ya da JSON etiketi bulunmaz. depguard bu kuralı zorlar.
2. **Arayüzler kullanan tarafta tanımlanır.** Servis yalnız ihtiyaç duyduğu 2–3 metotluk arayüzü tanımlar. Bu, core-backend'deki sağlayıcı-tarafı `Store` arayüzü alışkanlığının tersidir [N05 A1]. Bellek içi store yazılmaz; servis testleri gerçek PG'de, test başına ayrı DB ile koşar. Sahte implementasyon yalnız dış entegrasyonlar için yazılır.
3. **İş kuralları tek yerde durur**:
   - Para hesabı `money` ve `payment/domain.go` içinde.
   - FSM'ler tablo olarak `domain.go` içinde.
   - Yetki kararı yalnız `authz` içinde.
   - "Yanlış soyutlama yerine biraz tekrar" ilkesi yalnız teknik kodda (handler, mapper) geçerli; kurallar tekrar edilmez.
4. **Alanlar arası çağrı arayüz üzerinden** yapılır. Atomiklik gerekiyorsa `db.Querier` (tx'e bağlı) parametre olarak geçirilir; bu core-backend'in tx enjeksiyon kalıbı [N05 A3].
5. **`main.go` ince tutulur.** Wiring `internal/app/` altında, modül başına bir dosyada.
   - Bunlar core-backend'den alınmayacak: 1.443 satırlık `main.go`, tek pakette 245 route, 14 pakette tekrarlanan sentinel hatalar, 16 ayrı hata eşleyici, stdlib `log`, root kullanıcıyla çalışan `alpine:latest` imajı, bellekte liste süzme, OpenAPI yokluğu, zayıf lint [N05 A5].
   - Bunlar alınacak: tek karar noktası, capabilities ucu, fail-closed config, `both` kipi ve uyuşmazlık metriği, pending→sonuç denetim satırı, sıralı graceful shutdown, healthcheck alt komutu, lease'li worker'lar, problem+json ile stabil `code` alanı.

### 3.3 Kütüphane seçimleri (Spring parçası → Go)

| Spring parçası | Go karşılığı | Gerekçe |
|---|---|---|
| Web MVC + springdoc | stdlib `net/http` + **oapi-codegen** strict server (`std-http`) | Sözleşme önce yazılır; handler imzası spec'ten üretilir. |
| Bean Validation | **kin-openapi** istek doğrulama middleware'i + serviste açık domain kontrolleri | Biçim kuralları spec'te bir kez yazılır; etiketlerle ikinci kopya tutulmaz. `go-playground/validator` alınmaz. |
| Jackson | `encoding/json` + özel `money.Amount`, `civil` tipleri | Bilinmeyen alan reddi istekte açık. |
| Security (iki zincir) | `authn` middleware'leri: marketplace ve `/v1/admin` | Personel zinciri ayrı mux'a bağlanır. |
| JJWT | **golang-jwt/jwt/v5** | Standart. |
| BCrypt | `x/crypto/argon2` (argon2id) | Taşınacak hash yok. OWASP önerisi; 72 bayt sınırı sorunu kalkar. Değerlendirme. |
| JPA/Hibernate + Spring Data | **pgx/v5 + sqlc** | Statik ve tipli SQL. Dinamik admin aramaları ~10 elle yazılmış pgx sorgusu olur [N02 §7-2]. |
| Liquibase | **goose v3** (gömülü, `koala migrate`) | Temiz baseline (§3.6). |
| Spring AMQP / Rabbit | **River** (Postgres iş kuyruğu). Alternatifi elle `SKIP LOCKED` worker'ları | §3.5 |
| `@Scheduled` | River periodic jobs (lider seçimli) | Çok örnekte güvenli; ShedLock eksikliği kapanır. |
| `@TransactionalEventListener` | `InsertTx` ile iş kaydı (outbox) + tx-sonrası hook listesi | §3.5 |
| STOMP simple broker | **coder/websocket** + JSON olay protokolü | §0.1 K4 |
| Actuator health | `/livez`, `/readyz` + `koala healthcheck` alt komutu | Distroless imajda wget yok. |
| Micrometer/Prometheus | **prometheus/client_golang** (ayrı iç port) | §3.10 |
| Logback ECS | `log/slog` JSON | Bağımlılık gerekmez. |
| Sentry Java | **sentry-go** | İsteğe bağlı. |
| `@ConfigurationProperties` + `ProductionConfigurationGuard` | Kendi `config` paketi (env, fail-closed) | Bağımlılık gerekmez; core-backend kalıbı. |
| MapStruct/Lombok | Elle eşleme fonksiyonları | Gerekmez. |
| Yauaa | **Kaldırılır** | Cihaz kimliği yalnız `X-Device-Id`; ad `X-Koala-Device-Model` ipucundan. 22 sn açılış maliyeti ve toplu çıkış riski kalkar [N01 §8.1-1]. |
| Thymeleaf | `html/template` | 17 şablon. |
| BouncyCastle (NFC) | stdlib `crypto/*` + `encoding/asn1` + `x/crypto/cryptobyte`; kendi CMS ve ISO 9796-2 kodu (K15) | `crypto/x509` explicit EC parametreli sertifikayı ayrıştıramıyor (golang/go#16660). Mevcut pkcs7 paketleri explicit EC'yi ve PSS SignerInfo'yu desteklemiyor [N08 §A2]. |

**Onaya sunulan bağımlılıklar.** Lisanslar benim bilgime dayanıyor; ekleme anında doğrulanmalı.

| Bağımlılık | Lisans | Neden | Zorunlu mu |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | MIT | PG sürücüsü ve havuz | Evet |
| `github.com/go-chi/chi/v5` | MIT | Router (K8, karar kaydı) | Evet |
| `sqlc` (araç, `go tool`) | MIT | Sorgu kod üretimi | Evet |
| `github.com/pressly/goose/v3` | MIT | Migration | Evet |
| `github.com/oapi-codegen/oapi-codegen/v2` (araç + runtime) | Apache-2.0 | Sözleşmeden sunucu kodu | Evet (K9 spike'ına bağlı) |
| `github.com/getkin/kin-openapi` | MIT | İstek doğrulama | Evet |
| `github.com/golang-jwt/jwt/v5` | MIT | JWT | Evet |
| `golang.org/x/crypto`, `x/time`, `x/text`, `x/oauth2` | BSD-3 | argon2, rate limit, dil eşleme, FCM OAuth | Evet |
| `github.com/google/uuid` | BSD-3 | UUIDv7 | Evet |
| `github.com/riverqueue/river` + `riverpgxv5` | MPL-2.0 | İş kuyruğu ve periyodik işler | K3-b seçilirse |
| `github.com/coder/websocket` | ISC | WebSocket | Evet |
| `github.com/prometheus/client_golang` | Apache-2.0 | Metrik | Evet |
| `github.com/wneessen/go-mail` | MIT | SMTP | Evet |
| `github.com/aws/aws-sdk-go-v2/service/s3` | Apache-2.0 | R2/S3 | Evet |
| `github.com/oschwald/geoip2-golang` | ISC | GeoIP | Evet |
| `github.com/getsentry/sentry-go` | MIT | Hata izleme | İsteğe bağlı |
| `github.com/openfga/language/pkg/go` | Apache-2.0 [07-44] | `.fga` ayrıştırma (test ve CI) | Evet (authz) |
| `github.com/testcontainers/testcontainers-go` | MIT | Test PG ve OpenFGA konteyneri | Evet (yalnız test). Alternatif: core-backend gibi docker CLI. |
| Araçlar: `golangci-lint`, OpenAPI lint (Spectral ya da vacuum), `openfga/openfga` imajı | — | Kalite kapısı | Evet (CI) |

**NFC (K15) için gereken bağımlılıklar: AYRICA ONAY BEKLİYOR.** K19 onayı bunları kapsamıyor [N08 §A4.6]. Lisanslar 04.11'de GitHub API'si ve pkg.go.dev'den okundu.

| Bağımlılık | Lisans | Neden | Zorunlu mu | Durum |
|---|---|---|---|---|
| `golang.org/x/crypto/cryptobyte` | BSD-3 | Explicit EC parametreleri, SOD CMS ve sertifika ASN.1 ayrıştırması. Modül yukarıda onaylı; paket NFC için yeni | Evet | **Onaylandı** (Yusuf, 04.11: hazır resmi paket varsa onu kullan, yoksa kendimiz yazarız) |
| `github.com/ProtonMail/go-crypto/brainpool` | BSD-3 | Brainpool eğrileri; yalnız yabancı belge (Brainpool'lu DSC ya da AA anahtarı) kapsama alınırsa. TCKK yolunda gerekmiyor; v1'de Brainpool fail-closed | Hayır | **AYRICA ONAY BEKLİYOR** (gerekirse) |
| `github.com/gmrtd/gmrtd` (yalnız imaja girmeyen ayrı test modülünde) | MIT; transitive `osanderson/brainpool` **lisanssız** | NFC diferansiyel testinde ikinci oracle | Hayır | **Onaylandı, koşullu** (Yusuf, 04.11): yalnız ayrı test modülünde, üretim imajına asla girmez. Eklemeden önce `osanderson/brainpool`'un lisans durumu çözülür (bakımcıdan lisans eklemesi istenir ya da `replace` ile lisanslı bir Brainpool paketine yönlendirilir); çözülmezse gmrtd eklenmez ve oracle olarak yalnız Java altın vektörleri kullanılır |

NFC için bilinçli olarak alınmayanlar:
- `go.mozilla.org/pkcs7` (deprecated), `smallstep/pkcs7`, `digitorus/pkcs7`: explicit EC ve RSA-PSS SignerInfo desteklemiyorlar.
- `ebfe/brainpool`: lisans dosyası yok.
- `golang.org/x/crypto/ripemd160`: deprecated; ICAO 9303 ECDSA AA'da RIPEMD-160'ı yasaklıyor.
- `gmrtd`'nin üretimde kullanımı: PC/SC (cgo) ve `x/mobile` bağımlılıkları; explicit eğriyi yalnız asal p ile eşliyor; ECDSA'da alternatif eğri geri düşmesi var.

Bilinçli olarak alınmayanlar: GORM ve ent (örtük davranış), fiber ve echo, viper, zap ve zerolog (slog yeterli), Casbin (ReBAC ifade gücü yok, `Scope → WHERE` yapamıyor [N07 §7]), `shopspring/decimal` (K5), `go-playground/validator`, `amqp091-go` (K3), `robfig/cron` (River periyodik işleri karşılıyor), `firebase-admin-go` (ağır bağımlılık ağacı; FCM HTTP v1 yeterli), UA ayrıştırıcıları.

### 3.4 Hata modeli

- **Tek `apperr` paketi**: `Kind` alanı {Invalid, Unauthenticated, Forbidden, NotFound, Conflict, RateLimited, Upstream, Internal}, yanında stabil bir `Code` ve isteğe bağlı alan hataları. Tek eşleyici bunu RFC 9457 `application/problem+json`'a çevirir.
- **Örnek gövde**:

```json
{ "type": "https://api.iskoala.com/problems/payment-attempt-active",
  "title": "Conflict", "status": 409,
  "code": "PAYMENT_ATTEMPT_ACTIVE",
  "instance": "/v1/job-assignments/…/payment-session",
  "requestId": "…",
  "errors": [{ "pointer": "/iban", "code": "IBAN_INVALID" }] }
```

- **Kodların kaydı**: Kodlar OpenAPI'de enum olarak (ya da `x-error-codes`) listelenir. İstemcilerin hata kodu listesi buradan üretilir. Bu, bugün Java kaynağını tarayan mobil ve web sözleşme testlerinin yerini alır [N06 §1.2, §4-2].
- **Var olma gizleme**: authz kararının çıktısıdır. Okuma reddi, kaynağın kendi 404 gövdesiyle bayt bayt aynı döner [N01 §2.2, §9.d].
- **Örtük eşlemeler kaldırılır**: `IllegalStateException → 409` gibi eşlemeler gider. Bilinmeyen hata 500 `INTERNAL` döner, gerekçe yalnız log'da kalır.

### 3.5 Transaction yönetimi ve asenkron işler

| Spring davranışı | Go karşılığı |
|---|---|
| `@Transactional` (462) | `db.InTx(ctx, func(q Querier) error)`. Tx servis metodu sınırında açılır, store `Querier` alır. readOnly tx yalnız tutarlı anlık görüntü gerektiğinde açılır. |
| REQUIRES_NEW, ödeme rezervasyonu (6 yer) | Akış üç ayrı kısa tx'e bölünür: **(1)** denemeyi rezerve et ve commit et; **(2)** Iyzico çağrısını tx dışında yap; **(3)** sonucu kısa bir tx'te yaz. HTTP çağrısı sırasında hiçbir tx açık kalmaz. Bu, "dış tx Iyzico boyunca açık" sorununu ve submerchant anahtarı kaybını tasarımla kapatır [N02 §4, §8 madde 8]. |
| REQUIRES_NEW, AFTER_COMMIT içinden (crew konuşmaları, parola sıfırlama bildirimi) | Ya aynı tx'te doğrudan yapılır (crew üyeliği bir domain yazımıdır) ya da `InsertTx` ile iş olarak kuyruğa alınır. |
| AFTER_COMMIT dinleyicisi (5) | Garantili işler `InsertTx` ile aynı tx'te kuyruğa girer (outbox semantiği). Realtime yayını gibi best-effort işler tx'e bağlı hook listesinde tutulur ve yalnız commit başarılıysa çalışır. |
| `noRollbackFor` (2) | Açık yazılır. Başarısız giriş sayacı kendi kısa tx'inde commit edilir, sonra hata döner. |
| MANDATORY + advisory lock | `pg_advisory_xact_lock` yalnız `Querier` (tx) alan fonksiyonlarda çağrılabilir; tip sistemi bunu zorlar. |
| `PESSIMISTIC_WRITE` (10) | Açık `SELECT … FOR UPDATE`. Para işlemlerinde serileştirme noktası atama satırıdır. Kilit sırası "önce atama, sonra detay" olarak belgelenir ve test edilir. |
| Rollback'te depolama temizliği | Yükleme `pending/` önekine yapılır, DB commit'inden sonra yeri değiştirilir. Yetim nesneleri periyodik bir iş toplar. |
| 5 outbox ve 17 zamanlayıcı | River işleri. `UniqueOpts` (ör. `(payment_id, operation)`) çift kuyruklamayı engeller. Sağlayıcı çağrısı tx dışında yapılır; sonuç kısa tx'te yazılır. Böylece "aynı APPROVE iki kez gider" riski kalkar [N03 §2.2]. 5 sn'lik polling döngüleri ortadan kalkar. |

Rabbit gerekli mi? Hayır. Bütün üreticiler ve tüketiciler aynı süreçte çalışıyor. Bugünkü outbox'lar zaten DB'de; Rabbit yalnız bir atlama ve ayrı bir konteyner ekliyor [N02 §4, N03 §3.3]. Tek kutuda Postgres kuyruğu yeterli. İleride gerçekten ayrı bir tüketici süreç gerekirse (ör. medya işleme) iş tablosu yine yeterli olur.

River onaylanmazsa: core-backend'deki `FOR UPDATE SKIP LOCKED` + lease token kalıbı tek bir `platform/jobs` paketinde yazılır, beş ayrı kopya yerine tek uygulama olur [N05 A3]. Yaklaşık 400–600 satır; tahmin.

### 3.6 Şema, soft-delete, kimlikler

1. **Baseline**: Liquibase taşınmaz. Hedef şema elle ve temiz yazılır (goose `0001_baseline.sql`).
   - Bugünkü 4 trigger, 4 fonksiyon ve kısmi indeksler gözden geçirilip gerekli olanlar korunur.
   - `devices.push_token` trigger'ı tam satır UPDATE'i cezalandırıyor [N02 §2]. Bu trigger kaldırılır; push token kuralı açık bir store fonksiyonuna taşınır.
   - Referans verisi (6 CSV, sözleşme sürümleri, personel rol ve izin seed'i, sistem kullanıcısı) `0002_seed.sql`'e konur.
2. **Temizlik listesi** [N02 §1.2, §6]:
   - `deleted_by_id` (hiç yazılmıyor) kaldırılır. Silme aktörü `deletions` tablosunda tutulur (madde 3).
   - Genel `created_by_id` ve `updated_by_id` kaldırılır; aktör gereken yerde alana özgü kolon ve denetim tablosu kullanılır.
   - `Job.duties` ve `location` ad uyuşmazlığı giderilir.
   - `authorities` tablosu kaldırılır (marketplace rolü yok, §8 madde 5).
   - JSON alanları `TEXT` yerine `jsonb` olur.
   - Enum'lar için CHECK kısıtı eklenir.
   - Saat kaynağı tek olur: uygulama saati parametre olarak geçer.
3. **Soft-delete (K14: varsayılan VAR)** [N08 §B]:
   - **Kapsam.**
     - Yeni tablo varsayılan olarak soft-delete'li. Bugünkü tablolardan ~33 iş tablosu bu sınıfta: kullanıcı, şirket, ilan, başvuru, ulaşım, mesaj, bildirim, personel.
     - Muafiyet yalnız altı gerekçeli sınıfta:
       - **M1 defter ve kanıt: silinmez.** Ödeme, iade, payout, iptal, atama, puantaj, biniş, uyuşmazlık, rıza, sözleşme, denetim. `deleted_at` yok, uygulama rolünde DELETE yetkisi yok.
       - M2 kısa ömürlü token ve oturum: TTL ile hard delete.
       - M3 kuyruk: River.
       - M4 referans: `retired_at`.
       - M5 türetilmiş: yeniden kurulur.
       - M6 bağlantı ve tekil ayar: hard delete ya da upsert; katılımcıda `left_at`.
     - Liste `db/softdelete.yaml`'da. Katalog testi her tablonun ya view'lı ya muaf olduğunu zorlar. **M1'in "silinmez" olması S4'te sektör standardı olarak kararlaştırıldı.**
   - **Kolonlar.**
     - `deleted_at timestamptz` ve `deletion_id uuid → deletions(id)`. CHECK: ikisi birlikte NULL ya da birlikte dolu.
     - `deletions` tablosu aktörü, nedeni ve kökü tutar. Tablo başına `deleted_by` yok.
     - `deletion_id` bir kaskad işleminin satırlarını gruplar; geri alma yalnız onları açar.
   - **Unique.**
     - Her iş anahtarı kısmi indeks: `WHERE deleted_at IS NULL`. İstisnalar katalog testinde gerekçeli.
     - `devices.push_token` kısmi unique. Soft-delete aynı UPDATE'te token'ı NULL'lar; trigger kaldırılır (madde 1).
     - Geri almada çakışma → 409 `RESTORE_CONFLICT`.
   - **Unutmayı imkânsızlaştıran mekanizma.** Uygulama sorguları `active_<t>` view'larından (`SELECT * … WHERE deleted_at IS NULL`, `CHECK OPTION` yok) okur ve yazar. Uygulama DB rolü `koala_app`'in soft-delete'li **temel tablolarda yetkisi yok**. Temel tablolar yalnız `koala_maint` rolüyle açılır: geri alma, temizlik, anonimleştirme, personelin "silinmişler" ekranı. Bu rol ayrı bir havuzda durur ve depguard ile `privacy/`, `staff/` ve temizlik işleriyle sınırlanır. Unutulan filtre ilk testte `permission denied` verir. sqlc tipleri view'lardan üretir. `sqlc vet` CEL kuralı temel tablo adını uygulama sorgularında reddeder. RLS seçilmedi: filtre görünmez kalır; tek `USING` politikası soft-delete UPDATE'ini reddeder; tablo sahibi politikayı atlar.
   - **View, #309'u çözmez.** `active_*`'e INNER JOIN silinmiş ebeveynle çocuğu da düşürür; EF Core belgesindeki uyarının aynısı. Bu yüzden soft-delete'li tabloya giden her FK `softdelete.yaml`'da bir politika taşır:
     - `cascade`: çocuk aynı `deletion_id` ile silinir; INNER JOIN serbest.
     - `restrict`: aktif çocuk varken silme 409.
     - `detach`: çocuk kalır, ebeveyn mezar taşı olarak görünür; **yalnız `LEFT JOIN active_*`**.
     Lint, `detach` kenarında INNER JOIN'i reddeder. Üretilmiş test: ebeveyn silinince çocuğun uçları çocuğu hâlâ döndürür.
   - **KVKK.** Soft-delete "silme" değildir: KVKK Yönetmeliği m.8/1 "tekrar kullanılamaz" şartı arıyor. Hesap silme: soft-delete → en geç 30 günde anonimleştirme (m.12/1-a). Tablo başına `retention` dolunca hard delete ya da anonimleştirme; periyodik iş ayda bir koşar (m.11/2 altı ay tavanı). Anonimleştirmeden sonra geri alma kapanır.
   - **Authz.** Silinmiş nesne → gizleme 404'ü (§3.4). `authz_tuples` VIEW'ı `active_*`'den kurulur; silinen nesnenin tuple'ları kendiliğinden düşer.
   - Hibernate'in örtük davranışlarının açık karşılıkları:

| Örtük davranış [N02 §2] | Açık karşılık |
|---|---|
| Her sorguya ve join'e `is_deleted=false` | `active_<t>` view'ı + uygulama rolünde temel tablo yetkisi yok + `sqlc vet` kuralı. JOIN anlamı FK politikasından gelir: `detach` kenarında yalnız LEFT JOIN, lint ve üretilmiş testle zorlanır. |
| `delete()` → UPDATE | Tek `softdelete.Delete(ctx, q, kök, aktör, neden)`: `deletions` satırı + kök + kayıttaki `cascade` kenarları, aynı tx'te. `softdelete.Restore(deletionID)` yalnız `maint` rolüyle. |
| Kaskad / orphanRemoval (29/7) | `softdelete.yaml` kenar kaydı (`cascade` / `restrict` / `detach`). Katalog testi: her FK'nin politikası var. Değişmez sorgusu: "silinmiş ebeveynin aktif çocuğu = 0", testte ve izlemede. DB trigger'ı ile kaskad yapılmaz. |
| `created_at` / `updated_at` | DB default + `set_updated_at()` trigger'ı. Tek istisnasız kural olduğu için DB'de durur. |
| Dirty checking, tam satır UPDATE | Dar UPDATE: yalnız değişen kolonlar. Gerekli kaynaklarda `version` kolonu ve `If-Match` (iyimser kilit). |
| OSIV + LAZY, 14 varsayılan EAGER | Her uç için açık join ya da "id sayfası + `WHERE id = ANY($1)` ile toplu yükleme" kalıbı. N+1 korumasını sorgu sayısı testi sağlar. |
| STI (`user_verifications`, `media`) | Ayrı tablolar ya da `kind` kolonu + CHECK; tasarımda karar verilir. |

4. **Kimlikler**: Uygulamada UUIDv7 üretilir. Zamana göre sıralı olduğu için cursor sayfalama ve indeks yerleşimi için iyidir.
5. **Hassas alanlar**: TCKN ve IBAN AES-256-GCM ile şifrelenir. Anahtar env'den gelir, şifreli değer sürüm önekli tutulur. Benzersizlik için ayrı anahtarla HMAC-SHA256 kör indeks kullanılır. Bu alanlar hiçbir iş yükünde ya da log'da yer almaz; worker gerekirse kullanıcı id'siyle DB'den okur (§8 madde 6–7).

### 3.7 Para

- **Tipler**:
  - `money.Amount` int64 kuruş tutar; `money.Rate` ölçek-4 int64 tutar.
  - Tek bir `MulRateHalfUp` fonksiyonu var. `CommissionCalculator` ve `CancellationPolicy` kuralları bunun üzerine kurulur [N03 §2.3].
  - DB'de `NUMERIC(19,2)` kalır; runbook SQL'leri okunur kalsın diye. pgx kodlayıcısı tek yerde yazılır.
- **JSON**: `"150.00"` biçiminde string ve yanında `currency`.
  - Iyzico istek gövdesinde sayı, ölçek korunarak `json.Number` olarak yazılır. Null alan politikası Java ile aynı tutulur (`"iban": null` farkı) [N03 §2.3, §7.2-2].
  - Yanıt imzası için `stripTrailingZeros().toPlainString()` (ör. `1.0E7` → `10000000`) birebir taklit edilir ve altın vektörle test edilir.
- **Vergi**: KDV ve stopaj kodda yok. v1'de de yok; ayrı bir ürün kararı [N03 §2.3].

### 3.8 Zaman

- **Anlık zamanlar**: `timestamptz`; API'de RFC 3339 UTC `Z`.
- **İlan saatleri (K11-b)**: `starts_at timestamptz` + `time_zone text default 'Europe/Istanbul'`. API'de ofsetli değer döner: `2026-08-27T09:30:00+03:00`.
- **tzdata**: distroless static imajda zoneinfo yok, bu yüzden `import _ "time/tzdata"` gerekir.
- **Saat soyutlaması**: `clock.Clock` arayüzü. Testlerde sahte saat kullanılır; bugün 7 IT `Clock` mock'luyor [N04 §1.3].

### 3.9 Kimlik doğrulama tasarımı (K12)

- **Token'lar**:
  - Erişim JWT'si 15 dk.
  - Her token sınıfı (marketplace, personel, realtime bileti) ayrı anahtar kullanır.
  - `iss`, `aud` ve `alg` sıkı doğrulanır.
  - Refresh token opak, 32 bayt, DB'de SHA-256 ile saklanır. Yüksek entropili token için yavaş hash gereksiz; parola sıfırlama token'ı zaten böyle [N01 §3.5]. Her kullanımda rotasyon yapılır. Eski token tekrar kullanılırsa o oturum ailesi iptal edilir.
- **Oturum kontrolü**: Her istekte tek indeksli sorguyla oturum, cihaz ve kullanıcı durumu kontrol edilir; anlık iptal korunur.
- **Cihaz**: `X-Device-Id` zorunlu (admin hariç). UA'dan türetilen cihaz kimliği yok.
- **OTP**: Deneme sayacı (ör. 5), sabit zamanlı karşılaştırma, DB'de hash, tek tip 404.
- **Personel**: `token_version`, kilit, `must_change_password` ve TOTP (K13).
- **Hız sınırı**: `x/time/rate` ile bellek içi; tek örnek varsayımı açıkça belgelenir.
- **Korunan sözleşme**: 426 `X-Minimum-App-Version` ve `X-App-Version`, `/v1` dışında da çalışır. Bu, eski mobil derlemeyi güvenle kilitleyen tek mekanizma [N06 §1.4, §4-1].

### 3.10 Gözlemlenebilirlik

- **Log**: `slog` JSON. Her satırda `request_id`, `user_id` ya da `staff_id`, `session_id`, `route`, `status`, `duration_ms` bulunur; context'ten handler'a eklenir. core-backend'de request id log'a girmiyordu [N05 A3]. PII alanları için `LogValuer` maskeleri ve lint kuralı var; bugünkü `LogPrivacyTest`'in karşılığı.
- **Metrik**: Prod olmadığı için 18 alarm kuralı da yeniden yazılır [N04 §2.6].
  - HTTP için OTel semconv adları: `http_server_request_duration_seconds{http_request_method,http_route,http_response_status_code}`. `http_route` değeri `r.Pattern`'dan alınan şablondur.
  - Domain sayaçları adlarını korur: `koala_payment_operations_total{operation,outcome}`, `koala_payout_settlement_overdue`…
  - İş hataları için `koala_job_failures_total{kind}`. Bu, Spring'in otomatik ürettiği `tasks_scheduled_*` serisinin yerini alır.
  - `/metrics` ayrı bir iç portta yayımlanır, proxy'ye açılmaz. Bugünkü "`/actuator/prometheus` marketplace ADMIN token'ı istiyor" tuhaflığı kalkar [N01 §8.3].
- **Sağlık**: `/livez` ve `/readyz`. Drain başlayınca readiness 503 döner; sonra HTTP 35 sn içinde boşaltılır (PR #3 ile değişti: 20 sn idi; alt sınır `ReadHeaderTimeout` 5 sn + istek süre sınırı 25 sn + 5 sn yazma payı), ardından worker iptal edilir. core-backend'in `serve.go` sırası [N05 A1].

### 3.11 Yapılandırma, sırlar, Docker

- **Yapılandırma**: Yalnız env'den okunur. Açılışta fail-closed doğrulanır: bilinmeyen enum değeri, eksik zorunlu sır ya da ortam ile sağlayıcı uyuşmazlığı başlatmayı durdurur. Bu `ProductionConfigurationGuard`'ın karşılığı [N04 §2.8]. Sır adları `docs/secrets-inventory.md`'deki kategorilerle aynı kalır; değerler bu belgede yok. JWT anahtarları token sınıfına göre ayrılır.
- **Docker**:
  - Çok aşamalı build: digest'e sabitlenmiş `golang` imajı → digest'e sabitlenmiş `distroless/static:nonroot`.
  - `CGO_ENABLED=0`, `-trimpath`; sürüm `-ldflags` ile ve git tag'den gelir. `publish-ghcr-image.sh` artık pom okumaz [N04 §2.3].
  - `HEALTHCHECK` `koala healthcheck` alt komutunu çağırır.
  - Rabbit konteyneri kalkar. Compose'da yalnız app ve DB kalır.

---

## 4. API sözleşmesi ve istemci uyarlaması

### 4.1 Tasarım ilkeleri (karar 2)

1. **Sözleşme önce**: `api/openapi.yaml` (3.1) tek doğruluk kaynağı. Akış: spec PR'ı → lint (Spectral ya da vacuum) → sunucu kodu ve istemci tipleri üretilir → uygulama yazılır.
   - Her operasyonda `operationId` ve `x-authz` (eylem ve nesne tipi) ya da açık `security: []` (public) bulunur.
   - Personel operasyonlarında `x-authz-permission` bulunur. Bu eşleme eksikse **sunucu açılmaz**. Bu, `AdminPermissionMappingIT` ve `ApiDocumentationTest`'in yerini alır [N01 §7].
2. **Kaynak adları**: Taban yol `/v1`. Kaynak adları çoğul ve kebab-case (`/v1/job-assignments/{id}`). Kendi verin `/v1/me/...` altında.
   - Durum geçişleri alt eylem POST'u olarak kalır: `/v1/job-assignments/{id}/complete`. Bugünkü kural bu [N01 §1.3]; Stripe'ın `…/confirm` kalıbıyla da aynı.
   - Alan adları camelCase. Enum değerleri bugünkü gibi UPPER_SNAKE kalır; istemci değişikliği azalsın diye.
3. **Zarf yok**: Başarılı yanıtta çıplak gövde döner. `success`, `message` ve `httpStatus` alanları kalkar. Hata yanıtı RFC 9457 (§3.4). Mesaj metinleri istemcide; sunucu yalnız kod döner.
   - İstisna: e-posta, SMS ve push metinleri sunucuda kalır (tr/en). 842 anahtarlık katalog, bildirim şablonlarına kadar küçülür.
4. **Sayfalama**: `?limit=&cursor=`; yanıt `{ "items": [], "nextCursor": "…", "prevCursor": "…", "total": 57 }`.
   - `total` yalnız `?includeTotal=true` ile gelir; admin listeleri ve SEO'lu arama için [N06 §4-4].
   - Cursor, sıralama anahtarını kodlar. Sıralama alanları allowlist'te; dışındaki alan 400 `SORT_FIELD_NOT_ALLOWED`.
5. **Tarih ve para**: Tarihler RFC 3339 (§3.8). Para string ondalık + `currency` (§3.7).
6. **Idempotency-Key**: Para ve yan etki doğuran POST'larda zorunlu. Ödeme oturumu, iade ve çıkış (personel), başvuru, kayıt ve şikayet bunlara dahil.
   - Saklama: `idempotency_keys(key, actor, operation, request_hash, response, expires_at)`.
   - Aynı anahtarla farklı gövde gelirse 422; işlem sürerken gelirse 409. IETF taslağı `draft-ietf-httpapi-idempotency-key-header`.
7. **Başlıklar**:
   - Korunan: `X-Request-Id` (yanıtta), `X-App-Version`, 426.
   - Netleştirilen: `X-Client-Platform`. Mobil bugün göndermiyor [N06 §1.2].
   - Kaldırılan: `Accept-Language`'e göre mesaj çözümü.
8. **Realtime (K4-b)**:
   - Bağlantı: `GET /v1/realtime` WebSocket, alt protokol `koala.v1`. Kimlik, `POST /v1/realtime/tickets` ile alınan 60 sn'lik tek kullanımlık biletle sağlanır; cihaz eşleşmesi yapılır.
   - **Sunucu-itmeli model**: İstemci konu seçmez. Sunucu, authz'a göre kullanıcının konuşmalarına ait olayları ve ortak sohbeti olan kişilerin presence'ını iter.
   - İstemciden gelen mesajlar yalnız tipli komutlar: `typing`, `presence.watch` (en fazla 100 id, her biri authz'dan geçer).
   - Olay zarfı `{type, id, occurredAt, data}`. Hata olayları REST'teki `code` kümesini kullanır.
   - İstemci `id` ile tekilleştirir. Bağlantı koparsa REST'ten `?since=` ile eksikleri tamamlar.

### 4.2 İstemci uyarlama planı [N06]

| İstemci | Büyüklük | Araç | En riskli |
|---|---|---|---|
| **Web** (ilk) | M, 3–5 kişi-hafta | openapi-typescript + openapi-fetch; BFF'in tek kapısı `apiFetch`'e oturur, ~80 elle yazılmış tip silinir | Oturum yenileme ve çerez akışı; 8 numaralı-sayfa listesi (SEO'lu `job-search`); ödeme sonucu bekleme; realtime istemcisi (+0,5 hf) |
| **Admin** (ikinci) | M, 3,5–6 kişi-hafta (Orval ile alt sınır) | Orval zod ya da openapi-zod-client; 80 zod şeması üretilir, özel refinement'lar korunur | Emanet çıkışı ve iade formları; payout mutabakatı; mesaj kataloğu sıfırdan (~31 dosya); 54 dosyada sayfalama; `src/server/authz/*` (1,4 k satır) capabilities ucuyla değişir |
| **Mobil** (son) | L, 7–10 kişi-hafta | Elle istemci + OpenAPI şemasından fixture doğrulayan sözleşme testi; hata kodu listesi spec'ten üretilir | `auth_interceptor` (rotasyon); `startDateTime` (56 referans) ve offline mesai kuyruğu (sqflite'ta eski biçim); 119 `e.message` gösterim yeri; `FinancialFailureMessage`; STOMP → WS |

Sıra web → admin → mobil [N06 §5]. Gerekçe:
- Web en küçük istemci ve kataloğu hazır; sözleşmeyi ilk sınayan o olur.
- Admin zod üretimini ve sayfalama kararını zorlar.
- Mobil, web'de oturmuş kataloğu devralır.

Mobil, Go ile aynı gün yayımlanır ve `X-Minimum-App-Version` o derlemeye çekilir. Eski derleme her yanıtı hata sayar; sessiz değil, görünür bir bozulma olur [N06 §1.4].

Paralellik: Bir alanın spec'i dondurulunca (§6, her alan aşamasının ilk adımı) o alanın istemci işi başlayabilir. İstemciler Go sunucusuna karşı geliştirilir. O alan için Go hazır olana kadar spec'ten üretilmiş mock sunucu (Prism benzeri; ayrı onay) kullanılabilir.

---

## 5. Authz tasarımı (karar 5)

### 5.1 Sektör özeti ve hüküm

**Sektör pratiği [N07 §1–5]:**
- **Google Zanzibar** [07-2]: tuple'lar (`object#relation@user`), userset rewrite, zookie ile tutarlılık. Liste ve arama Check ile değil, ayrı bir indeksle yapılıyor.
- **GCP IAM ve Azure RBAC** [07-3][07-5]: principal → rol → kapsam; önce deny değerlendiriliyor.
- **Meta**: süreç içi, veri erişim katmanında gizlilik kontrolü. PrivacyLib [07-15] bir kütüphane, servis değil. entgo'nun privacy `FilterFunc`'ı [07-12] SQL WHERE ile süzüyor.
- **Merkezi servisler**: Airbnb Himeji [07-16] ve Carta [07-18]. Uber merkezi politika deposu ile yerel değerlendirmeyi birleştiriyor [07-22].
- **"Yerel veriden SQL'e süzme" kalıbı** ürünleşmiş durumda: Oso "Local Authorization / list_local" [07-24][07-25], OPA partial evaluation [07-27], Cerbos query plan [07-28].
- **Bizim VIEW fikrimiz** (domain tablolarını tuple biçimine projekte etmek) resmi bir üründe bulunamadı. Kavramsal olarak Oso'nun "fact → SELECT" eşlemesiyle aynı [N07 §5].

**Hüküm:**
- Tasarım sektörle genel olarak uyumlu: uygulama içi tek karar noktası ve veri katmanında süzme. Monolitte ayrı bir authz servisi kurmamak Slack'in de ilk adımıydı [07-21].
- İleride OpenFGA ya da SpiceDB'ye geçiş: **orta**.
  - `Check` ve `Capabilities` kolay.
  - Tuple senkronu orta.
  - `Scope` dışarı taşınırsa zor. Yerelde kalırsa (Oso hibrit modeli [07-26]) toplam zorluk **kolay-orta**.

Senin koşulun ("sektörle tam uyumluysa ve geçiş zor olmayacaksa") N07'deki 7 değişiklik işlenince karşılanıyor. Bu değişikliklerin hepsi aşağıda tasarıma dahil.

**Emsal: Slack'in loopback ve paralel karşılaştırması [07-21].** Slack önce monolitin içine bir loopback gRPC izin servisi yazdı. "Dark mode"da eski ve yeni kararları paralel hesaplayıp farkları Prometheus'a yazdı. "Light mode"da yeni servisi doğruluk kaynağı yaptı. İleride OpenFGA'ya geçiş planımız bu sırayı izler (§5.7). Aynı tekniğin küçük bir örneği core-backend'de var: `AUTHZ_ROLE_MODE=both` + uyuşmazlık metriği [N05 B2].

### 5.2 N07'nin 7 değişikliği tasarımda nereye işlendi

| # | Değişiklik [N07 §8] | Tasarımda |
|---|---|---|
| 1 | VIEW sıcak yolda kullanılmaz | `Check` ve `Scope` ilişki başına tipli SQL kullanır (domain tablolarına EXISTS ya da JOIN). `authz_tuples` VIEW'ı yalnız uygunluk testi, hata ayıklama ve ileride dışa aktarım için var. |
| 2 | Kurallar doğrudan OpenFGA DSL'inde yazılır | `internal/authz/model.fga` (schema 1.1). Alt küme: `[type]`, `X from Y`, `or`, sınırlı `and`/`but not`, `with condition`. |
| 3 | Öznitelik ve durum koşulları ayrılır | İlan durumu, atama durumu, engelleme gibi koşullar "ReBAC dışı ön süzgeç" olarak `authz/prefilters.go`'da adlandırılır ya da tuple varlığına çevrilir (ör. `public_viewer@user:*` yalnız ilan OPEN iken yazılır). |
| 4 | Tutarlılık parametresi ve üç durumlu Scope | `AtLeastAsFresh(token)` bugün no-op. `Scope` sonucu Never, Always ya da Conditional olur; Conditional ya tipli predicate ya da ID kümesi taşır. |
| 5 | İlişki yazımları tek domain olayından geçer | İlişki tablolarına yalnız `authz.RelationChange` üreten store fonksiyonları yazar; depguard ve lint bunu zorlar. Bugün bu olaylar yalnız `authz_change_log`'a (denetim) gider; geçiş günü outbox'a bağlanır. |
| 6 | PII denetimi authz'dan bağımsızdır | Ayrı `pii` paketi; denetim satırı ve okuma aynı tx'te. |
| 7 | Uygunluk test takımı | Aynı matris hem Go değerlendiricisine hem gerçek OpenFGA konteynerine karşı koşar (§5.6). |

### 5.3 Arayüz taslağı (yalnız imzalar)

```go
package authz

type SubjectKind uint8 // User | Staff | System
type Subject struct { Kind SubjectKind; ID uuid.UUID }
type Object  struct { Type string; ID string } // OpenFGA biçimi: "job_assignment", "<uuid>"
type Action  string                            // = model.fga'daki relation adı: "can_complete"

type ConsistencyToken string
type CheckOption func(*checkOptions)
func AtLeastAsFresh(t ConsistencyToken) CheckOption // bugün no-op

type Decision struct {
    Allowed bool
    Basis   string // "worker", "editor from job", "staff_role:FINANCE_OFFICER" → denetime
    Conceal bool   // ret → kaynağın kendi 404'ü
}

type Checker interface {
    Check(ctx context.Context, s Subject, a Action, o Object, opts ...CheckOption) (Decision, error)
    CheckBatch(ctx context.Context, s Subject, a Action, objs []Object, opts ...CheckOption) (map[Object]Decision, error)
}

type ScopeKind uint8 // ScopeNever | ScopeAlways | ScopeConditional
type Predicate struct { SelfUserID *uuid.UUID; CompanyIDs []uuid.UUID }
type Scope struct { Kind ScopeKind; Predicate *Predicate; IDs []string }
type Scoper interface {
    Scope(ctx context.Context, s Subject, a Action, objectType string) (Scope, error)
}

type Capabilities struct { Can map[string]bool; Companies []CompanyCapabilities; Permissions []string }
type CapabilityReader interface {
    Capabilities(ctx context.Context, s Subject) (Capabilities, error)
}

type RelationChange struct { Op ChangeOp; Object Object; Relation string; Subject string /* "user:<id>" | "staff_role:<id>#assignee" */; Actor Subject }
type RelationSink interface { Record(ctx context.Context, q db.Querier, ch ...RelationChange) error }

package pii
type Purpose string
type Field string
type Grant struct{ /* unexported */ }
type Gate interface {
    Open(ctx context.Context, q db.Querier, staff authz.Subject, dataSubject uuid.UUID, p Purpose, f []Field) (*Grant, error)
}
// func user.ToAdminDTO(u user.User, g *pii.Grant) AdminUserDTO // g nil → maskeli
```

Servisler bu arayüzlerin yalnız ihtiyaç duydukları kısmını kendi taraflarında tanımlar (ör. `type checker interface{ Check(...) }`). `Check`, mümkün olduğunda domain okumasıyla aynı `Querier` (tx) üzerinde koşar; böylece kendi yazdığını okuma bedavaya gelir [N07 §6.1-2].

### 5.4 Örnek model (OpenFGA DSL alt kümesi)

```
model
  schema 1.1

type user
type staff                                   # staff_members; users ile FK yok (ADR-0014)

type staff_role
  relations
    define includer: [staff_role]            # staff_role_includes DAG'ı
    define assignee: [staff] or assignee from includer

type platform                                # tek nesne: platform:koala
  relations
    define user_read: [staff_role#assignee]
    define user_read_pii: [staff_role#assignee]
    define payout_release: [staff_role#assignee]
    define payment_refund: [staff_role#assignee]
    # … 25 izin; her biri bir relation (nokta yerine alt çizgi)

type company
  relations
    define admin: [user]                     # company_contacts.role = ADMIN
    define staff_member: [user]              # company_contacts.role = STAFF
    define member: admin or staff_member
    define can_manage: admin                 # davet, güncelleme, logo, kişi çıkarma
    define can_manage_jobs: member

type job
  relations
    define owner_company: [company]
    define creator: [user]
    define public_viewer: [user:*]           # yalnız OPEN iken yazılır (durum → tuple varlığı)
    define editor: creator or can_manage_jobs from owner_company
    define can_view: public_viewer or editor
    define can_edit: editor

type job_application
  relations
    define job: [job]
    define applicant: [user]
    define can_read: applicant or editor from job
    define can_decide: editor from job
    define can_withdraw: applicant

type job_assignment
  relations
    define job: [job]
    define worker: [user]
    define party: worker or editor from job  # AssignmentParticipants'ın karşılığı
    define can_read: party
    define can_cancel: party
    define can_complete: editor from job
    define can_pay: editor from job
    define can_check_in: worker

type dispute
  relations
    define assignment: [job_assignment]
    define can_read: party from assignment
    define can_add_evidence: party from assignment

type conversation
  relations
    define participant: [user]
    define can_read: participant
    define can_post: participant             # engelleme: ReBAC dışı ön süzgeç (MessagingAvailability)
```

Modelin dışında kalanlar:
- **Kendi verin** (`bank_account`, `education`, `device`… `owner`): `Scope` ile `SelfUserID` WHERE'ine çevrilir.
- **Presence ("kendisi ya da ortak sohbet")**: userset rewrite ile ifade edilemez. Bugün tipli SQL kuralıdır. OpenFGA'da, konuşma kurulurken yazılan türetilmiş bir `contact` ilişkisi gerekir. Bu, geçişte modellenmesi gereken bilinen bir kalemdir.
- **Görev ayrılığı** (`StaffSeparationOfDuties`): yazım anında doğrulanan bir değişmez, karar kuralı değil.

### 5.5 Scope → SQL, personel RBAC, PII, capabilities

**Scope → SQL.** Store, tipli kapsamı WHERE'e çevirir; SQL parçası üretilmez [N05 B6].

```sql
-- name: ListAssignmentsForSubject :many
SELECT a.* FROM job_assignments a JOIN jobs j ON j.id = a.job_id
WHERE (@scope_all::bool
       OR a.worker_id = sqlc.narg(self_user_id)
       OR j.creator_id = sqlc.narg(self_user_id)
       OR j.company_id = ANY(@company_ids::uuid[]))
  AND (a.created_at, a.id) < (@cursor_ts, @cursor_id)
ORDER BY a.created_at DESC, a.id DESC
LIMIT @lim;
```

`ScopeNever` gelirse servis sorgu atmadan boş liste döner.

**Personel RBAC.**
- 25 izin ve 10 rol, DAG'ı ve görev ayrılığıyla birlikte aynen taşınır [N01 §4.2].
- Her istekte tek bir özyinelemeli CTE ile DB'den çözülür; rol iptali anında geçerli olur.
- `/v1/admin/**` operasyonunun izni spec'teki `x-authz-permission`'dan gelir. Eşlemesi olmayan operasyon varsa açılış başarısız olur (fail-closed). Eşleşmeyen yol 403 döner.
- Marketplace `ROLE_ADMIN` ve köprü bayrağı kalkar.

**Denetim kaydı.**
- `admin_audit_entries` tasarımı korunur: saklama 24 ay, finansal kayıtlarda 10 yıl, ayrıntılar 6 ay, gövde ≤8 KB [N01 §4.4].
- Mutasyonlarda pending→sonuç satırı kullanılır ve DB trigger'ı değişmezliği sağlar (core-backend kalıbı [N05 A5-5]). #341'in kararı (her kararın öznesi kaydedilir) baştan şemada yer alır.

**PII kapısı (fail-closed).**
- `pii.Gate.Open` aynı tx'te `pii_access_log` satırını yazar: aktör, temel, veri sahibi, amaç, alanlar, uç, request id.
- Satır yazılamazsa grant verilmez ve yanıt maskeli kalır.
- PII alanları (TCKN, tam IBAN, telefon, sokak ve kapı, hassas koordinat, iş oturumu konumu) yalnız `*pii.Grant` alan projeksiyon fonksiyonlarıyla doldurulabilir. Tip sistemi ve lint bunu zorlar. #349 ve #352'nin konum kuralı tasarımın parçası olur.

**Capabilities ucu.**
- Marketplace: `GET /v1/me/capabilities` → `{ can: {...}, companies: [{companyId, role, can: {...}}] }`.
- Personel: `GET /v1/admin/me/capabilities` → `{ permissions: [...], can: {...} }`.
- Her değer aynı `Check`'ten hesaplanır [N05 B4].
- İstemci JWT ayrıştırmaz. Uç okunamazsa UI kapalı başlar; core-frontend'deki "fallback kural kopyası" bilinçli olarak alınmaz [N05 B6-4].

### 5.6 Dağınık 130–150 kontrolü toplamak ve test stratejisi

1. **Envanter (Faz 0)**:
   - N01 §9.b'deki desenler betikle taranır: `Concealment.notFound` 34, `requireCompanyAdmin`, `requireJobEditor`, `AssignmentParticipants` 23, `requireOwned` 13, doğrudan `getId().equals` 18 ve diğerleri.
   - Çıktı bir CSV: `dosya:satır, guard, nesne tipi, eylem, gizleme var mı, hata kodu, bağlayan Java testi`. ~43 dosya, ~130–150 satır beklenir.
2. **Tekilleştirme**: Satırlar `(nesne tipi, eylem)` çiftlerine indirgenir. Tahminim 40–60 farklı çift. Bu sayı yargıya dayanıyor; envanter kesinleştirir.
3. **Matris**: `authz/testdata/matrix.yaml` dosyasında her çift için olumlu ve olumsuz satırlar yazılır (özne fikstürü, eylem, nesne fikstürü → beklenen sonuç ve temel). Kaynak Java testi etiketlenir.
4. **Uygulama**: Servisler yalnız `authz` arayüzlerini çağırır. depguard, ilişki tablolarına yetki amaçlı sorguyu `authz` dışından yasaklar. Her OpenAPI operasyonunda `x-authz` lint'le zorunlu.
5. **Uygunluk**: Matris iki yere karşı koşar. (a) Go değerlendiricisi, test başına ayrı DB'de. (b) `openfga/openfga` konteyneri: `model.fga` ve aynı DB'nin `authz_tuples` VIEW'ından alınan tuple'lar yüklenir. İki sonuç farklıysa CI kırmızı olur. Böylece "kurallar OpenFGA alt kümesinde" iddiası test edilmiş olur.
6. **Gizleme altın testleri**: Var olmayan kaynak ile yetkisiz erişilen kaynak için dönen 404 gövdeleri bayt bayt eşit olmalı.

### 5.7 Gelecekte OpenFGA/SpiceDB'ye geçiş yolu

1. `RelationSink`, outbox'a bağlanır. Bir aktarıcı işi idempotent `Write` ile OpenFGA'ya yazar. Başlangıç verisi VIEW'dan backfill edilir [07-37].
2. **Dark mode**: `Check` iki tarafta da hesaplanır, farklar `koala_authz_mismatch_total` sayacına yazılır. Bu Slack'in ve core-backend'in `both` kipi [07-21][N05 B2].
3. **Light mode**: `Check` ve `Capabilities` için doğruluk kaynağı OpenFGA olur. `Scope` yerelde kalır (Oso hibrit modeli [07-26]); ListObjects'in 1.000 sonuç / 3 sn sınırına takılmaz [07-29].
4. **Tutarlılık**: OpenFGA'da `HIGHER_CONSISTENCY` ya da önbellek kapalı [07-32]; SpiceDB'de kaynak tablosunda ZedToken sütunu [07-34].
5. Bant dışı periyodik karşılaştırma çalışır: VIEW anlık görüntüsü ile `ReadTuples` farkı.

**Zorluk hükmü: kolay-orta.** Asıl dikkat noktası, soft-delete ve durum değişikliklerinin tuple silme olaylarına doğru çevrilmesi. K14 (varsayılan soft-delete) bu yüzeyi **büyütür**: her silme ve geri alma, kaskadla etkilenen bütün nesneler için tuple silme ve yeniden yazma olayı üretmelidir. `deletion_id` etkilenen satırları sayılabilir kılar. Olaylar aynı tx'te outbox'a yazılır. Uygunluk matrisine "soft-delete → `Check` false, geri alma → true" senaryosu eklenir [N08 §B2.9].

---

## 6. Aşamalı plan

### 6.1 Aşamalar

Büyüklükler kişi-hafta (kh), ajan desteği olmadan tek kişi seri varsayımıyla. İstemci işi ayrı sayıldı (Faz C).

| Faz | Kapsam | Çıkış kriteri (ölçülebilir) | Büyüklük | Bağımlılık |
|---|---|---|---|---|
| **0: Kararlar, sözleşme kuralları, iskelet** | K1–K19 kararları ADR olarak yazılır. OpenAPI stil rehberi, ortak bileşenler (problem, sayfa, para, tarih) ve lint yapılandırması kurulur. Repo iskeleti ve `make verify` hazırlanır. Spike'lar: oapi-codegen + 3.1; River + goose şema entegrasyonu; WS bileti. Senaryo kataloğu biçimi belirlenir. Authz kontrol envanteri (§5.6-1) çıkarılır. Java oracle'ından karar tabloları üretilir (§6.2). | ADR'ler merge edildi. Spec lint yeşil. Spike'ların her biri "çalışıyor ya da alternatif seçildi" notuyla kapandı. Envanter CSV'si ≥130 satır. Oracle CSV'leri `testdata/` altında. | M, 2–3 kh | — |
| **1: Platform** | config, slog, pgx, `InTx`, goose baseline + seed, sqlc + `sqlc vet`, `apperr`/problem, middleware zinciri (request id, access log, 426, rate limit, gövde limitleri, path normalizasyonu), metrik, sağlık, graceful shutdown, River. Authn: kullanıcı oturumu, cihaz, refresh rotasyonu, personel girişi, kilit, `token_version`. Authz çekirdeği, uygunluk koşucusu, personel RBAC ve denetim çekirdeği, PII kapısı iskeleti. Test altyapısı: test başına DB (template DB), sahte saat, Iyzico ve FCM fake'leri. Docker, yayın betiği. **Soft-delete korumaları (K14, §3.6-3):** `deletions` tablosu, `active_<t>` view ve GRANT goose yardımcıları, `koala_app` / `koala_maint` rolleri ve iki havuz, `softdelete` paketi (Delete/Restore, kenar kaydı), `db/softdelete.yaml`, katalog testleri, `sqlc vet` temel tablo kuralı, JOIN lint'i, üretilmiş okuma ve JOIN test çatısı (OpenAPI `x-resource`). | `make verify` yeşil: `golangci-lint` 0 bulgu, `go test -race` yeşil. Boş DB'de `goose up` ve `down`. Problem+json altın testleri. AUTH senaryolarının %100'ü yeşil. Uygunluk koşucusu OpenFGA konteyneriyle ≥1 tipte yeşil. İmaj distroless nonroot, açılış <1 sn, `/readyz` drain davranışı test edildi. **Soft-delete:** katalog testi her tabloyu view'lı ya da muaf sınıflandırıyor; `koala_app`'in temel tablolarda yetkisi yok (test); view kolonları = tablo kolonları; her FK'nin politikası var; `detach` kenarında INNER JOIN lint'te kırmızı; üretilmiş okuma testi ≥1 kaynakta yeşil; kaskad ve geri alma testleri yeşil. | L, 7–9,5 kh (soft-delete +1–1,5) | 0 |
| **2: Kimlik, kullanıcı, şirket, referans, medya** | 3 adımlı kayıt, OTP, parola sıfırlama, profil, eğitim kayıtları, kimlik belgesi yükleme (NFC'siz), oturum listesi. Şirket, kişiler, davetler, takip. Referans ve adres verisi (Google). Yükleme, EXIF ve S3. Bu alanların admin uçları: kullanıcı detayı ve PII, şirket onayı, kimlik incelemesi. | Alanın spec'i donduruldu. Alanın P0 senaryolarının %100'ü, P1'in ≥%90'ı yeşil. Matriste `company` ve `user` tiplerinin satırları hem Go'da hem OpenFGA'da yeşil; sahipliğin model karşılığı `private_file#owner`'ın satırları iki motorda yeşil, kalan sahiplik kararları (`bank_account`, `education`, `device`, `session`…) `go_only` kalır ve store'un `Scope`'unda sınanır (§5.4 "Modelin dışında kalanlar": `owner` bir model tipi değildir). PII lint 0. Yol geçişi (`..`, `%2e`, `%2f`) fuzz testi yeşil. | L, 6–8 kh | 1 |
| **2b: NFC çip doğrulaması (K15)** | `internal/identity/chip`, saf Go [N08 §A4]: LDS TLV, DG1 TD1 MRZ, DG11, DG14, DG15. SOD CMS SignedData ayrıştırma ve imza (RSA PKCS#1, RSA-PSS parametreli, ECDSA). Explicit EC → adlı eğri eşlemesi (6 alan eşitliği; DSC ve link sertifikası için SPKI normalleştirme, imza orijinal TBS üzerinde). Gömülü 15 CSCA, parmak izleri sabit; DSC → CSCA (+ link) zinciri. AA: ISO 9796-2 şema 1 (kısmi ve tam kurtarma), ECDSA düz/DER yelpazesi. Challenge: 8 bayt, 10 dk, deneme sayacı. Politika: "SOD DG15 listeliyorsa AA zorunlu". Brainpool ve RIPEMD-160 fail-closed. Java'dan altın vektör üreticisi (`TestChip` tabanlı, sentetik, PII'siz), diferansiyel fuzz düzeneği. | Java'nın PA (30), AA (30/44 koşu), `LdsFilesTest` (15) ve challenge senaryoları katalogda ve Go'da yeşil. Altın vektörlerde karar ve hata kodu eşitliği %100. Diferansiyel korpusta beklenmeyen uyuşmazlık 0. Fuzz hedefleri CI'da panic'siz. 15 çıpanın 15'i yükleniyor. Gerçek TCKK ile PA+AA yerelde "doğrulandı" (döküm repoya ve CI'a girmez). Deneme sınırı testi yeşil. Kripto PR'larında ikinci göz. | M, 2–3 kh | 2. Faz 3 ile paralel yürür. |
| **3: İlan, başvuru, atama** | İlan yaşam döngüsü ve fiyat snapshot'ı, arama (haversine, koordinat yuvarlama), kategori, şablon, alarm, kayıtlı ilan. Başvuru FSM'i, atama FSM'i (tablo tabanlı), puantaj (ADR-0013 offline olayları), iptal politikası, değerlendirme, rozet, yetenek havuzu, sitemap. İlgili admin uçları. | P0 %100. İptal politikası karar tablosu Java oracle'ıyla %100 eşit. FSM geçiş tablosu testi tüm (durum, olay) çiftlerini kapsıyor. Matrisin `job`, `job_application` ve `job_assignment` satırları yeşil. Sorgu sayısı testi: liste uçlarında N+1 yok. | L, 7–9 kh | 2 |
| **4: Mesajlaşma ve realtime** | Konuşmalar (DIRECT/GROUP, crew), mesajlar, okundu bilgisi, engelleme, şikayet. WS protokolü, bilet, presence, sunucu-itmeli fanout. | P0 %100. WS sözleşme testleri: yetkisiz olay alınamıyor; istemci komutu sahte olay üretemiyor; presence yalnız ortak sohbette görünüyor. 64 KB çerçeve sınırı ve oran sınırları uygulanıyor. | M, 3–4 kh | 3 (konuşma ilan ve atamaya bağlı) |
| **5: Bildirim** | Bildirim kaydı, e-posta (17 şablon), SMS, FCM HTTP v1, tercihler, abonelikten çıkma, izleme pikseli, İYS (AES-GCM `v1:`), alıcı allowlist'i. | Şablon HTML diff testleri yeşil. İYS şifreli verisi Java'nın test vektörüyle çözülüyor. İş yeniden deneme ve terminalleştirme senaryoları yeşil. | M, 3–4 kh | 1. Faz 3 ile paralel yürür; üretici kancaları alan fazlarında bağlanır. |
| **6: Para** | Iyzico istemcisi (IYZWSv2, yanıt imzası, host allowlist'i, ortam kapısı), alt üye ve profil senkronu, ödeme oturumu (§3.5'teki üç tx), callback ve `reconcile` karar zinciri, emanet FSM'i, çıkış ve iade, settlement işleri (unique), mutabakat süpürmeleri, payout ve K2 durumları, IBAN, uyuşmazlık. Finans admin uçları. | §6.3'teki para kapıları tamamen yeşil. Karar tabloları (`reconcile`, komisyon, iptal) oracle'la %100 eşit. Altın vektörler ve kayıt-oynatma ile gövde eşitliği sağlandı. İki worker yarışında bir `(payment, operation)` için en fazla bir sağlayıcı çağrısı. `koala_payment_operations_total` etiket kümesi testi yeşil. | L, 7–9 kh | 3, 5 |
| **7: Ulaşım** | Plan, servis, durak, koltuk ataması (kapasite `FOR UPDATE`), QR biniş, binmedi kaydı, hatırlatma işleri. Plaka ve şoför görünürlüğü. | P0 %100. Kapasite yarış testi yeşil. Görünürlük matrisi yeşil. | M, 3–4 kh | 3. Faz 4–6 ile paralel yürür. |
| **8: KVKK ve personel yönetimi** | Rıza kayıtları, sözleşme sürümleri, hesap silme ve 30 gün sonra anonimleştirme (`PostgresAccountAnonymizer` karşılığı), kişisel medya silme, saklama işleri. Personel ve rol yönetimi, görev ayrılığı, denetim arama ve dışa aktarım (#344), notlar ve bayraklar, ayarlar, TOTP. **Soft-delete yaşam döngüsü (K14):** tablo başına `retention` ile temizlik işi (aylık, hard delete ya da anonimleştirme); kullanıcı (30 gün içinde) ve personel geri alma uçları; anonimleştirmeden sonra geri almanın kapanması. | **Şema güdümlü test**: kullanıcıya FK'sı olan her tablo, soft-delete'li satırlar dahil, ya anonimleştirme planında ya da gerekçeli muafiyet listesinde (yeni tablo eklenince test kırmızı). PII erişim satırı yazılamazsa grant yok testi. Saklama süresi testleri. Her soft-delete'li tablonun `retention`'ı var ve temizlik işi süresi dolan satırı bırakmıyor. Geri alma çakışma, süresi dolmuş ve ebeveyni silinmiş vakaları yeşil. KVKK senaryolarının %100'ü yeşil. | M, 3,5–5 kh (soft-delete +0,5–1) | 2–7 (anonimleştirici bütün tablolara dokunur) |
| **9: Sertleştirme ve devreye alma** | Bütün P1 senaryoları. Güvenlik incelemesi (authz matrisi tam, token testleri, fuzz). Sandbox'ta Iyzico uçtan uca senaryo seti (§6.3). Küçük yük smoke testi (k6, isteğe bağlı). Runbook'lar ve alarmlar yeniden yazılır. Cutover wizard güncellenir (Liquibase adımı yerine goose). | Iyzico sandbox'ta 6 senaryo kanıtlı. 18 alarm kuralının karşılığı var. Restore tatbikatı goose ile yapıldı. Mobil mağaza derlemesi hazır. Tek seferlik geçiş tamamlandı, Java arşivlendi. | M, 2–3 kh | Hepsi, C |
| **C: İstemciler** | Web → admin → mobil (§4.2). | Her istemci için tip üretimi derleniyor, test takımı yeşil, cihaz testi yapıldı. | 14–21 kh | Alan spec'i dondukça paralel |

### 6.2 Java testlerini senaryo kaynağına çevirmek (karar 3)

Java HTTP testleri yeni sözleşmeye karşı koşamaz, çünkü API değişiyor. Eşitlik bu yüzden iş kuralı düzeyinde kanıtlanır. Dört araç kullanılır:

1. **Senaryo kataloğu** (`test/scenarios/<alan>.yaml`). API'den bağımsız iş dilinde yazılır. Alanları:
   - `id` (ör. `PAY-017`), öncelik (P0/P1/P2), kural cümlesi;
   - `given` (durum), `when` (eylem), `then` (iş sonucu ve hata kodu);
   - `source` (Java `dosya#metot`), `go_test` (Go testinin adı).

   Kaynak olarak 978 IT, 1.798 birim testinin kural testleri (`business/concretes` 836 test), runbook SQL IT'leri ve CLAUDE.md'deki "neden" paragrafları kullanılır. Bir CI testi her P0 satırının `go_test`'inin var olduğunu ve geçtiğini doğrular; eksik olan kırmızıdır. Kataloğun kapsama oranı aşama çıkışında ölçülür.
2. **Karar tabloları (oracle)**:
   - Java'da bir kez, küçük bir JUnit üreticisiyle şu fonksiyonlar beslenir: `PaymentManager.reconcile` girdileri (paymentStatus × fraudStatus × mdStatus × snapshot uyuşmazlığı × atama durumu), `CommissionCalculator` (HALF_UP sınırları `x.xx5`), `CancellationPolicy` (süre × oran × tutar), `PaymentStateMachine`.
   - Çıktılar CSV olarak `testdata/oracle/` altına yazılır. Go, aynı CSV'yi tablo-güdümlü testte birebir üretmek zorundadır [N03 §7.3-3].
   - Bu üretici CI'da ya da kilitle tek seferlik koşar; Mac'te ağır iş kuralına uyulur.
3. **Iyzico altın vektörleri**:
   - `IyzicoProtocolTest`'teki sabit imza değerleri ve test sırrı `testdata/iyzico/*.json`'a taşınır.
   - Java'nın LOOPBACK modunda gönderdiği ham istek gövdeleri (`randomKey` sabitlenerek) kaydedilir. Go'nun aynı senaryoda **bayt bayt eşit** gövde ürettiği doğrulanır.
   - Yanıt imzası normalizasyonu (`stripTrailingZeros`) ayrı vektörlerle test edilir [N03 §7.3-1,2].
4. **Runbook SQL testleri**: Runbook'lar yeni şemaya göre yeniden yazılır. `RunbookSql` kalıbı korunur: test, SQL bloğunu belgeden okuyup gerçek PG'de koşar [N04 §1.4].

### 6.3 En riskli alanlar için koruma yöntemleri

**Para ve emanet:**
- Kurallar tek yerde: FSM tablosu, `money` tipi, tek `reconcile` fonksiyonu.
- DB kısıtları aynen taşınır: `uk_payments_one_active_attempt`, `(payment_id, operation)`, `uk_payouts_settlement_reference`, `chk_payouts_completed_requires_evidence`. Kısıt adı hata koduna `pgconn.PgError.ConstraintName` ile eşlenir [N02 §3].
- Sağlayıcı çağrısı sırasında tx açık kalmaz. Sağlayıcıdan önce rezervasyon commit edilir.
- River unique işleri. İki worker'ın yarıştığı eşzamanlılık testi `pg_sleep` tetikleyicisiyle kurulur.
- Oracle CSV'leri, altın vektörler, kayıt-oynatma.
- Property testleri: rastgele tutar ve oranla `total = payout + commission`; `refund + fee = collected`.
- Metrik etiket kümesi sözleşme testi.
- Para PR'larında ikinci göz: insan ya da ayrı bir inceleme ajanı.
- Sandbox senaryo seti: başarılı ödeme, `mdStatus≠1`, fraud 0, tam iade, kısmi iade + disapprove sırası, approve [N03 §7.3-7].
- K2 kararı kod yazılmadan önce verilir.

**KVKK ve PII:**
- PII tipleri dışa kapalı; yalnız `pii.Grant` ile açılır.
- TCKN ve IBAN şifreli, kör indeksli. Kuyruk yüklerinde PII yok.
- Log gizliliği için lint ve `LogValuer`.
- Şema güdümlü anonimleştirme kapsama testi.
- Konum, koordinat ve iş oturumu alanları için maske matrisi testi (#349 kuralı).
- Saklama işleri testi.
- Denetim satırı değişmezliği DB trigger'ıyla sağlanır.

**Kimlik doğrulama ve authz:**
- Varsayılan red: hiçbir operasyon `x-authz` ya da `security: []` olmadan derlenmez ve açılmaz.
- Uygunluk matrisi Go ve OpenFGA'ya karşı koşar. Gizleme altın testleri.
- Token testleri: `alg=none`, yanlış `aud`, yanlış `iss`, süre, anahtar karışması, refresh'in yeniden kullanımı → aile iptali.
- OTP deneme sınırı.
- Yol geçişi fuzz testi (`/uploads`) [N01 §8.2].
- Rate limit testleri.
- `must_change_password` ve kilit senaryoları.

### 6.4 Büyüklük özeti ve paralellik

| Kalem | Kişi-hafta |
|---|---|
| Backend (Faz 0–9 ve 2b) | **46–62**: 42–56 + NFC (Faz 2b) 2–3 + soft-delete (Faz 1 +1–1,5, Faz 8 +0,5–1) [N08] |
| İstemciler (Faz C: web 3–5, admin 3,5–6, mobil 7–10 + NFC 0,5–1,5) | **14,5–22,5** [N06]. Mobil NFC uyarlaması tahmin; N06'da yoktu, doğrulanmadı |
| **Toplam** | **≈ 61–84** |

**Paralellik:**
- Faz 0 ve 1 seri ilerler; kritik yol bunlar.
- Faz 1'den sonra:
  - 2 → 3 seri.
  - 2b (NFC), 2'den sonra 3 ile paralel; kritik yolu uzatmaz.
  - 5, 3 ile paralel.
  - 4, 6 ve 7, 3'ten sonra birbirine paralel; 6, 5'e de bağlı.
  - 8 en son.
- İstemciler, alan spec'i dondukça backend'le paralel ilerler.
- İki akış birden yürürse (backend ve istemci ya da iki alan ajanı) takvim kabaca %40 kısalır. Bu bir tahmin.

**Kalibrasyon uyarısı:**
- Java'nın büyük kısmı ~2 ayda yazıldı: Ağustos–Eylül 2026'da 428 commit, +157 K satır [N04 §5]. Aynı ajan temposuyla takvim 4–6 ay olabilir.
- Ama aynı tempo, CLAUDE.md'de kayıtlı "sessiz geçersiz kılma" kusur sınıfını da üretti. Bu planın kapıları (senaryo kataloğu, oracle, uygunluk testi) tempoyu bilinçli olarak yavaşlatıyor.

---

## 7. Java'daki açık işler

Durum [N04 §5, bellek notu 04.11]:
- Açık PR'lar: #341 (denetim öznesi), #344 (CSV dışa aktarım denetim olayı; #341'in üstüne yığılı), #350 (webp'yi reddet), #351 (kanıt EXIF temizliği), #352 (taslak; `read_pii` yoksa admin atama detayında konum boş, #349).
- `main`'de T2 (#335) ve #340 yayımlanmadı; son tag v0.9.31. **T2, #349 olmadan yayımlanırsa konum PII'ı sızar.**
- İstemci tarafında KVKK taslakları var: admin #136, web #203–206.

| Seçenek | Ne | Artı | Eksi |
|---|---|---|---|
| **A (öneri)** | Java'yı dondur: yeni özellik ve yayın yok. Karar içeren PR'ları (#341, #344, #350, #351, #352) `verify` yeşilse merge et. Karar ve gerekçeleri senaryo kataloğuna yaz. v0.9.32 yayımlanmaz. | Go için referans davranış tek bir dalda toplanır. KVKK kararları kayıtlı olur. T2 sızıntısı yayınsız kalır. | Bir `verify` koşusu gerekir (ağır iş, kilitle). #352 taslağının tamamlanması gerekir. |
| B | Java PR'larını kapat; kararları yalnız kataloğa ve ADR'lere yaz. | Sıfır Java işi. | Kararın çalışan kod kanıtı kaybolur. Mevcut pre-prod ortamı #349'suz kalır. |
| C | Java'yı paralel geliştirmeye devam et. | Kısa vadede özellik akışı sürer. | İki kod tabanı birbirinden uzaklaşır. **Önerilmez.** |

İstemci KVKK PR'ları (admin #136, web #203–206) UI kuralı taşıyor ve uyarlamadan sonra da geçerli kalacak. Bunları normal akışla ele al. Pre-prod Dokploy ortamı Java'da kalabilir; Go hazır olunca ADR-0016'ya göre sıfırdan kurulur [N04 §3.1].

---

## 8. Go'dan bağımsız, şimdi bilinmesi gereken bulgular

Prodda olmadığımız için Java'da düzeltmeye **değmez**; hepsi Go tasarımında kapanıyor ve senaryo kataloğuna **olumsuz test** olarak girer. İstisnalar: K2 (iş kararı) ve #349 (zaten PR'da, A seçeneği).

| # | Bulgu | Kaynak | Doğrulandı mı | Go'da nasıl kapanır |
|---|---|---|---|---|
| 1 | REST presence ilişki kontrolsüz: herkes herkesin online ve son görülme bilgisini görüyor; `lookup` sınırsız | `k/webAPI/RealtimeController.java:80-108` [N01 §8.3] | **Evet** (kod okundu) | Presence yalnız authz kuralıyla (kendisi ya da ortak sohbet); toplu sorgu en fazla 100 id |
| 2 | OTP: deneme sayacı yok, `String.equals`, 403 ile 404 ayrımı var-olmayı sızdırıyor | `UserVerificationManager.java:43-72` [N01 §3.5] | **Evet** | §3.9: sayaç, sabit zamanlı karşılaştırma, hash, tek tip 404 |
| 3 | STOMP: istemcinin `/topic/**`'ya SEND'iyle sahte olay basabilmesi | `StompAuthChannelInterceptor.java:77-81` [N01 §8.3] | **Kısmen**: SEND'de yalnız oran sınırı var (okundu). Simple broker'ın bunu iletmesi Spring davranışı; **testle doğrulanmadı** | Sunucu-itmeli protokol; istemci yalnız tipli komut gönderir (§4.1-8) |
| 4 | Var olmayı sızdıran hata kodları: görselde `IMAGE_NOT_FOUND` ile `IMAGE_PERMISSION` ayrımı (ikisi de 400); OTP 403/404 | `ImageManager.java:97-113` [N01 §8.3] | **Evet** | Gizleme authz'ın çıktısı (§3.4) |
| 5 | Eski marketplace admin rolü: `ImageManager`'da `ROLE_ADMIN` bypass'ı, `/actuator/**` `ROLE_ADMIN`, köprü bayrağı | `ImageManager.java:110`, `SecurityConfig.java:252-253` [N01 §4.1, §8.3] | **Evet** (ImageManager) | Marketplace admin rolü yok; yalnız personel kimliği; metrik iç portta |
| 6 | TCKN DB'de düz metin, profil senkron mesajında da düz | `User.java:60`, `IdentityVerification.java:23`, `UserUpdatedEventDto.java:20` [N02 §1.4, N03 §3.2] | **Evet** | Şifreli alan + kör indeks; iş yükünde PII yok (§3.6-5) |
| 7 | IBAN Rabbit mesajında düz metin | `UserUpdatedEventDto.java:19` | **Evet** | Aynı |
| 8 | Submerchant anahtarı kaybı: anahtar dış tx'te `users`'a yazılıyor; checkout istisnasında rollback'le kayboluyor | `JobAssignmentManager.java:311-314` [N02 §4] | **Kısmen**: kod yolu doğrulandı. Anahtar `attachProviderSubmerchantKey` (REQUIRES_NEW, :333) ile `payments` satırında kalıyor. Iyzico'nun ikinci `createSubMerchant`'a tepkisi **doğrulanmadı** | Alt üye oluşturma kendi kısa tx'inde, sağlayıcı yanıtından hemen sonra commit edilir (§3.5) |
| 9 | `CompanyDao.search` bellekte sayfalıyor (Page + koleksiyon graph'ı) | `CompanyDao.java:20-35` [N02 §2] | **Kısmen**: sorgu şekli doğrulandı. HHH90003004 log'u **doğrulanmadı** | Id sayfası + toplu yükleme kalıbı |
| 10 | Ölü kod: parking kuyruğu tüketicisiz; `DisputeStatus.UNDER_REVIEW` hiç yazılmıyor; `IyzicoPaymentService.cancel` çağrılmıyor | `RabbitMQConfig`, `DisputeManager.java:60`, `IyzicoPaymentService.java:231` [N03 §8] | **Evet** (üçü de grep'le) | Taşınmaz. UNDER_REVIEW ürün kararı; v1'de yok. |
| 11 | webp izin listesinde ama JDK okuyamıyor | `UploadProperties.java:28,34` [N03 §5] | **Kısmen**: izin listesi doğrulandı. Reddedildiği **doğrulanmadı**. #350 açık | v1'de webp yok. Go'da webp kodlayıcı olmadığından EXIF temizliği için yeniden encode edilemez. |
| 12 | **Payout COMPLETED çelişkisi**: belge "tek yol banka mutabakatı" diyor; kod APPROVE referansıyla doğrudan COMPLETED yazıyor | `CLAUDE.md:2408` (§2y), §2y3 ↔ `SettlementManager.java:199-219` [N03 §2.4] | **Evet** (ikisi de okundu) | **K2 kararı.** Önerim iki durum: `PROVIDER_APPROVED` ve `SETTLED` |
| 13 | Bayat belgeler: CLAUDE.md "36 IT" (gerçekte 128); `silent-diagnostics-sweep` "hiçbiri uygulanmadı" diyor ama bir kısmı kapandı; N05'te ~132 entity | [N04 §1.2, §3.4] | Evet (N04) | Yeni repoda kurallar ADR + lint olarak tutulur; düzyazı korpusu yok |
| 14 | Settlement: satır kilidi yok, HTTP çağrısı tx içinde → çok örnekte çift APPROVE/REFUND | `SettlementTaskDao.java:34-35` [N03 §2.2] | Doğrulanmadı (kod okuması) | River unique işi + tx dışı çağrı |
| 15 | Kısmi iade + disapprove sırası garanti değil | [N03 §2.4] | Doğrulanmadı | İşler arası açık bağımlılık: disapprove, refund başarısını bekler |
| 16 | Pratikte HSTS yok; JWT `iss` doğrulanmıyor; erişim token'ı 12 sa; STOMP CONNECT'te cihaz eşleşmesi yok; personelde MFA yok | [N01 §3, §4.6] | Doğrulanmadı (N01 kod okuması) | HSTS proxy'de ve uygulamada; §3.9; K13 |
| 17 | `/uploads/**` yol güvenliği Spring'in `StrictHttpFirewall`'ına dayanıyor | [N01 §8.2] | Doğrulanmadı | Açık normalizasyon + fuzz testi |
| 18 | Iyzico approve/disapprove/refund yanıtlarında imza doğrulaması yok | `IyzicoPaymentService.java` [N03 §2.2] | Doğrulanmadı | Iyzico bu yanıtları imzalıyorsa doğrula; imzalamıyorsa belgele (sandbox'ta bakılacak) |

---

## 9. Riskler ve açık sorular

| # | Risk / soru | Olasılık | Etki | Azaltma |
|---|---|---|---|---|
| R1 | Para yolunda sessiz davranış kayması | Orta | Çok yüksek | §6.3: oracle, vektörler, unique işler, sandbox seti; K2 kararı önce verilir |
| R2 | Iyzico referans davranışı kesin değil (sandbox uçtan uca hiç tamamlanmadı) | Yüksek | Yüksek | Faz 6'da Go ile sandbox seti koşulur. Ham sağlayıcı yanıtları fixture olarak kaydedilir. Belirsiz dallar fail-closed: VERIFY_PENDING |
| R3 | Özellik donması tutmaz; Java ve Go birbirinden uzaklaşır | Orta | Yüksek | §7-A. Java'ya giren her değişiklik için bir katalog satırı zorunlu |
| R4 | oapi-codegen ya da kin-openapi OpenAPI 3.1'i tam desteklemiyor | Orta (**doğrulanmadı**) | Orta | Faz 0 spike'ı. Alternatifler: ogen; spec'i 3.0.3 uyumlu alt kümede tutmak; istemci tarafında 3.1 araçları |
| R5 | İstemci maliyeti tahminin üstünde; mobilde saat ve offline kuyruk | Orta | Orta | Önce web. Mobilde `startsAt` göçü için sqflite kuyruğunu boşaltan ya da dönüştüren tek seferlik bir adım |
| R6 | Senaryo kataloğu eksik kalır; Java'daki örtük bir kural kaybolur | Orta | Yüksek | Kaynak taraması sistematik yapılır (IT ve birim kural testleri). P0 kapsaması aşama çıkışı. CLAUDE.md'nin "neden" paragrafları da kaynak sayılır |
| R7 | River'ın kendi şeması ve sürüm yükseltmeleri goose ile çakışır | Düşük | Düşük | River migration'ı goose'a SQL olarak alınır. K3-c alternatifi hazır |
| R8 | Authz kural envanteri 130–150'den büyük çıkar | Düşük | Orta | Faz 0 envanteri. Tekilleştirme 40–60 çifte iner (tahmin) |
| R9 | Tek örnek varsayımı (rate limit, presence, WS fanout) yatay ölçeği engeller | Düşük (bugün tek kutu) | Orta | Varsayım açıkça belgelenir. Fanout için ileride Postgres LISTEN/NOTIFY; rate limit için PG ya da Redis |
| R10 | Bu Mac'teki yük (çoklu oturum) testleri sahte kırmızıya çeviriyor | Yüksek | Düşük-orta | Ağır işlerde kilit ve `nice`; `go test -p` sınırı; template DB ile hızlı izole DB |
| R11 | Takvim tahmini ajan temposuna dayanıyor | Orta | Orta | Faz 0–1 sonunda gerçekleşen hızla yeniden tahmin |
| R12 | ~~NFC'nin ertelenmesi~~ K15 ile kapandı: NFC v1'de. Yerine R13–R16 | — | — | — |
| R13 | Elle yazılmış NFC kripto kodunda (CMS, explicit EC, ISO 9796-2) hata: sahte kabul ya da gerçek kartın reddi | Orta | Yüksek | Java `TestChip` tabanlı altın vektörler, gerçek ve PII'siz CSCA/DSC vektörleri, Java oracle'ına karşı diferansiyel fuzz, ikinci göz, fail-closed varsayılanlar [N08 §A4.3] |
| R14 | Gerçek TCKK'nın SOD/DSC algoritması (PSS salt'ı, SHA-224/SHA-1) ya da AA anahtar tipi beklenenden farklı; Brainpool çıkarsa ek bağımlılık gerekir | Orta (gerçek döküm yok, #70) | Orta | Kendi imza fonksiyonu `x509`'un PSS kovasına bağlı değil. Faz 2b'nin başında gerçek kartla yerel teşhis. Brainpool için ProtonMail paketi onaya hazır |
| R15 | NFC'de iptal kontrolü yok: NVİ CRL yayımlamıyor, iptal edilmiş DSC tespit edilemez | Düşük | Orta | Kalan risk olarak belgelenir. ICAO PKD üyeliği ileride değerlendirilir. Doğrulama kanıtı (`evidence_digest`, DSC subject) saklanır |
| R16 | Varsayılan soft-delete'in INNER JOIN tuzağı (#309/#321 sınıfı) view'larla da sürer; ayrıca temizlik ve anonimleştirme yapılmazsa KVKK "silme" yükümlülüğü karşılanmaz | Orta | Yüksek | FK politika kaydı (`cascade`/`restrict`/`detach`), JOIN lint'i, şema güdümlü üretilmiş testler, değişmez sorgusu. KVKK'da 30 gün içinde anonimleştirme ve aylık temizlik [§3.6-3, N08 §B] |
| S4 | Para ve kanıt tabloları (M1) "silinmez" mi kalsın? Silinmiş hesabın TCKN'si yeni hesabı engellesin mi? | — | — | **Karar (sektör standardı, 04.11):** M1 silinmez; defter kayıtları değişmez ve eklemeli tutulur, düzeltme ters kayıtla yapılır (muhasebe pratiği; VUK saklama süreleri). Silinen hesabın TCKN'si yeniden kaydı engellemez; yalnız yasaklanmış hesaplar için TCKN'nin anahtarlı özeti (HMAC) dolandırıcılık önleme amacıyla saklanır. Bu son kısım KVKK açısından hukukçuya teyit ettirilecek [N08 §B2.1, §B2.3] |
| S1 | Admin numaralı sayfa şart mı? | — | — | K10 |
| S2 | Şirket rolleri genişleyecek mi (recruiter/viewer)? | — | — | K17; model `company` tipine relation eklemekle genişler |
| S3 | Vergi (KDV/stopaj) v1'de gerekecek mi? | — | — | Kodda yok [N03 §2.3]; ayrı ürün ve hukuk kararı |

---

## Ek A: Kaynakça

**Belgede kullanılan sektör kaynakları.** Numaralar N07'deki numaralarla aynı.
- [07-2] Pang ve ark., "Zanzibar: Google's Consistent, Global Authorization System", USENIX ATC 2019. https://www.usenix.org/system/files/atc19-pang.pdf
- [07-3] Google Cloud — IAM overview. https://docs.cloud.google.com/iam/docs/overview
- [07-5] Microsoft Learn — What is Azure RBAC? https://learn.microsoft.com/en-us/azure/role-based-access-control/overview
- [07-12] entgo — Privacy. https://entgo.io/docs/privacy/
- [07-15] Meta Engineering — PrivacyLib, 27.10.2026. https://engineering.fb.com/2026/10/27/security/privacylib/
- [07-16] InfoQ — Airbnb Himeji, 12.05.2021. https://www.infoq.com/news/2021/05/airbnb-himeji
- [07-18] Building Carta — AuthZ (403; arama özetinden). https://medium.com/building-carta/authz-cartas-highly-scalable-permissions-system-782a7f2c840f
- [07-21] Slack Engineering — "Role Management at Slack", 24.05.2021. https://slack.engineering/role-management-at-slack/
- [07-22] Uber Engineering — "Attribute-Based Access Control at Uber", 13.07.2023. https://www.uber.com/blog/attribute-based-access-control-at-uber/
- [07-24] Oso — Local Authorization. https://www.osohq.com/docs/authorization-data/local-authorization
- [07-25] Oso — Filter lists. https://www.osohq.com/docs/app-integration/integrate-authorization/filter-lists
- [07-26] Oso — Authorization data. https://www.osohq.com/docs/authorization-data
- [07-27] OPA — Partial evaluation. https://www.openpolicyagent.org/docs/filtering/partial-evaluation
- [07-28] Cerbos — Resources query plan. https://docs.cerbos.dev/cerbos/latest/api/#resources-query-plan
- [07-29] OpenFGA — Relationship queries. https://openfga.dev/docs/interacting/relationship-queries
- [07-32] OpenFGA — Consistency. https://openfga.dev/docs/interacting/consistency
- [07-34] SpiceDB — Consistency / ZedTokens. https://authzed.com/docs/spicedb/concepts/consistency
- [07-37] AuthZed — "The Dual-Write Problem", 02.01.2025. https://authzed.com/blog/the-dual-write-problem
- [07-44] OpenFGA — language. https://github.com/openfga/language

**Standartlar:** RFC 9457 (Problem Details), RFC 3339 (tarih/saat), OpenAPI 3.1, `draft-ietf-httpapi-idempotency-key-header` (taslak), OWASP Password Storage Cheat Sheet (argon2id önerisi; kaynak notlarda değil, genel bilgi).

## Ek B: Not dosyaları

`~/Library/Application Support/Claude/side-session-notes/local_797f2d35-c0a1-4423-afd6-ba494719a028/`:

| Dosya | Satır | En çok kullanıldığı bölüm |
|---|---|---|
| `01-api-guvenlik-realtime.md` | 390 | §1.2, §1.6–1.7, §3.4, §3.9, §5.6, §8 |
| `02-veri-katmani-liquibase.md` | 247 | §1.3, §3.5–3.6 |
| `03-is-mantigi-para-async-entegrasyon.md` | 349 | §1.4–1.5, §3.5, §3.7, §6.2–6.3, §8 |
| `04-test-ops-belgeler.md` | 249 | §1.8, §2, §3.10–3.11, §7 |
| `05-referans-core-backend-authz.md` | 278 | §3.1–3.2, §5 |
| `06-istemci-etkisi.md` | 136 | §4 |
| `07-authz-sektor-kaynakli.md` | 219 | §5, Ek A |
| `08-nfc-go-ve-soft-delete.md` | 495 | §1.5, §3.3, §3.6, §6.1, §6.4, §9 |

Doğrulama için kullanılan anlık görüntü: `/private/tmp/claude-501/-Users-yusssss-Projects-koala/6b216409-e0f5-4128-a31a-b6731e380bdf/scratchpad/backend-src`, `koala-backend` origin/main `2ca84245`.
