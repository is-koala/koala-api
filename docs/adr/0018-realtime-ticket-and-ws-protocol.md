# ADR-0018: Realtime bileti ve WS protokol ayrıntıları

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K4 (ayrıntı), K12 (bir kalemin yerine geçer)
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §1.7, §3.1, §3.9, §4.1-8, §6.1 (Faz 0 ve Faz 4), §9 R9; N01 §5, §8.3; [ADR-0011](0011-realtime-websocket-json.md), [ADR-0015](0015-sessions-and-staff-mfa.md)
- Kod: `internal/realtime`, `internal/platform/httpx` (`Router.Stream`, `ServerOptions.Streams`)

## Bağlam

ADR-0011 düz WebSocket + JSON olay protokolünü seçti. Bilet akışını Faz 0 spike'ına bıraktı. Spike şunları kanıtlamalıydı:
- kısa ömürlü, tek kullanımlık, cihaza bağlı bilet,
- sürümlü olay zarfı,
- sunucunun karar verdiği abonelik,
- sıralı kapanış.

Java'daki açıklar (N01 §5, §8.3):
- STOMP CONNECT cihaz eşleşmesi yapmıyor.
- İstemci `/topic/**`'ya sahte olay basabiliyor.
- 90 sn'lik credential tekrar kullanılabiliyor.

Bu ADR, spike'ta verilen kararları kaydeder. Bunlar ADR-0011'i ayrıntılandırır; ADR-0011'in kararlarını değiştirmez. Kabul edilmiş bir ADR düzenlenmediği için ayrıntılar ADR-0011'e ek olarak değil, burada tutulur.

## Karar

### 1. Bilet nasıl taşınır

Bilet, `Sec-WebSocket-Protocol` listesinde ikinci bir girdi olarak taşınır:

```
Sec-WebSocket-Protocol: koala.v1, koala.ticket.<bilet>
```

Sunucu yalnız `koala.v1`'i seçer ve bileti yanıtta geri yazmaz. Erişim token'ı handshake'e hiç girmez.

Gerekçe:
- **URL'de bilet yok.** Slack `rtm.connect` bileti URL'ye koyar. Bizim erişim log'umuz query yazmaz ama proxy (Traefik/Dokploy), CDN ve tarayıcı geçmişi yazar.
- **Uzun ömürlü token yok.** 15 dk'lık erişim token'ını alt protokole koymak mümkün. Ama başlığı loglayan bir ara katman token'ı tam ömrüyle sızdırır. Bilet 60 sn geçerli ve tek kullanımlık olduğu için sızsa da iş görmez.
- **İlk mesajla kimlik (Discord IDENTIFY) yok.** O modelde kimliği doğrulanmamış soket önce açılır, goroutine ve bellek tutar, ayrı bir zaman aşımı ister. Bilet upgrade'den **önce** doğrulanır; ret düz bir HTTP problem yanıtıdır.
- **Tarayıcıda çalışır.** Tarayıcının WebSocket API'si özel başlık gönderemez, alt protokol listesi gönderebilir. Kubernetes API sunucusu da tarayıcıdan gelen kimliği bu yolla taşır.

### 2. Bilet

- `POST /v1/realtime/tickets` → `201 {"ticket","expiresAt"}`, `Cache-Control: no-store`. Kimlikli REST çağrısıdır ve istek süresi sınırlı route'tadır.
- Değer 32 rastgele bayttır, base64url ile 43 karakter. **Opaktır, durumludur**: imzalı bir JWT değildir.
- Depoda değer değil, SHA-256'sı tutulur. Gerekçe refresh token'la aynıdır (plan §3.9).
- `TicketStore.Take` atomiktir; tek kullanım buna dayanır. Postgres'te `Put` bir `INSERT`, `Take` ise `DELETE … WHERE hash = $1 RETURNING …` olur. Süresi geçen satırları River periyodik işi siler. Üretimde henüz uygulama yok; testler bellek içi bir sahte kullanır (`_test.go`). Postgres deposu Faz 4'te gelir.
- **Bilet bağı:** Bilet, kullanıcı, oturum ve cihaz üçlüsüne bağlıdır. Kullanımda sırasıyla şunlar kontrol edilir:
  1. biçim,
  2. atomik `Take`,
  3. süre,
  4. handshake `X-Device-Id` taşıyorsa biletin cihazıyla eşitliği,
  5. oturumun hâlâ etkin olması (`Sessions` arayüzü).

  Hepsinin reddi aynı yanıttır: `401 REALTIME_TICKET_INVALID`.
- **Cihaz bağı ve istemci türü:**
  - `Origin` başlığı olmayan handshake yerel (mobil) istemcidir. `X-Device-Id` göndermek zorundadır; göndermezse `401 REALTIME_DEVICE_REQUIRED`.
  - Tarayıcı (`Origin` var) bu başlığı gönderemez. Onun için bağ, bilet verilirken (oturumun cihazından) ve kullanımdaki oturum kontrolüyle sağlanır.
- **Bilet basma oranı:** kullanıcı başına 5'lik burst, sonra 6 sn'de bir (dakikada 10). Aşımda `429 REALTIME_TICKET_RATE_LIMITED` ve `Retry-After` döner. Sınır `golang.org/x/time/rate` ile uygulanır. Dolu kovalar budanır, böylece bellek aktif kullanıcı sayısını izler. Tek örnek varsayımıyla bellek içindedir (§3.9).

### 3. ADR-0015 §1 "realtime bileti ayrı anahtar" kaleminin yerine geçer

ADR-0015 §1 her token sınıfına (marketplace, personel, realtime bileti) ayrı imza anahtarı öngörür. Realtime bileti imzalı olmadığı için bu kalem bu ADR ile **yerine geçilmiştir**:
- Bilet opak ve durumludur. Tek kullanımlık olmak zaten sunucu tarafında durum gerektiriyor; durum varken imzaya ve anahtar yönetimine gerek yoktur.
- Sınıf ayrımı anahtarla değil depoyla sağlanır. Bilet yalnız `TicketStore`'da aranır, erişim ya da refresh token'ı orada bulunmaz. Tersine, bilet bir JWT doğrulayıcısından geçemez.

ADR-0015'in diğer kalemleri (marketplace ve personel anahtarları dahil) geçerlidir.

### 4. Bağlantı sırası

`GET /v1/realtime` şu sırayla ilerler:

| Adım | Ret yanıtı |
|---|---|
| Upgrade başlıkları (`Connection`, `Upgrade`, sürüm 13, tek ve geçerli `Sec-WebSocket-Key`) ve `koala.v1` | 400 `REALTIME_PROTOCOL_UNSUPPORTED` |
| Origin | 403 `REALTIME_ORIGIN_FORBIDDEN` |
| Cihaz | 401 `REALTIME_DEVICE_REQUIRED` |
| Drain | 503 `REALTIME_DRAINING`, `Retry-After: 5` |
| Bilet | 401 |
| Kullanıcı başına bağlantı sınırı | 429 `REALTIME_TOO_MANY_CONNECTIONS`, `Retry-After: 30` |
| Oturumun ikinci kontrolü | 401 |
| Upgrade | — |

- Bilet harcanmadan önceki her şey, bilet harcanmadan kontrol edilir.
- Bilet kullanımı ve oturum kontrolü `HandshakeTimeout` (5 sn) içinde kalır.
- Oturum, kayıttan (`join`) sonra bir kez daha kontrol edilir. Bilet kullanımı ile kayıt arasında iptal edilen bir oturumu `DisconnectSession` henüz göremez; bu kontrol o pencereyi kapatır.
- Origin kuralı: `Origin` yoksa yerel istemcidir. Varsa ya isteğin kendi host'u ya da listedeki tam bir origin olmalıdır. Liste yalnız `https://host[:port]` kabul eder; `http://`, açık varsayılan port (`:443`), yol ve glob karakterleri reddedilir. Bilet ortam (ambient) kimlik bilgisi olmadığı için klasik CSWSH zaten kapalıdır; Origin kontrolü ek bir savunmadır.

### 5. Zarf ve protokol sürümü

- **Sürüm** alt protokoldür. Kırıcı değişiklik `koala.v2` olarak yan yana gelir. `v1` içinde yalnız ekleme yapılır; istemci bilmediği olay tipini ve alanı yok sayar. Mesaj başına sürüm alanı yoktur.
- **Sunucu → istemci:** `{type, id, occurredAt, data}`. `id` her olayda benzersizdir; `occurredAt` RFC 3339 UTC'dir.
- **İstemci → sunucu:** `{type, id?, data}`. `id` istemcinin seçtiği korelasyon kimliğidir; hata olayında `commandId` olarak geri döner. Zaman alanı yoktur.
- **Tipler:** Olay tipleri (`EventType`) ile komut tipleri (`CommandType`) ayrı Go tipleridir.
  - Olaylar: `hello`, `error`, `conversation.typing`.
  - Komut: yalnız `typing`. `presence.watch` Faz 4'te gelir; o zamana kadar bilinmeyen komuttur.
- **Katı çözme:** Komut katı çözülür. Bilinmeyen alan (ör. `userId`, `occurredAt`) ve fazladan veri reddedilir. Bilinmeyen komut tipi `error` olayıyla (`REALTIME_COMMAND_UNKNOWN`) reddedilir; bağlantı açık kalır.
- **Komut süresi:** Her komut `CommandTimeout` (5 sn) içinde çalışır.
- **Aktarma yok:** Hiçbir istemci mesajı aktarılmaz. `typing` komutundan sunucu yeni bir `conversation.typing` olayı kurar; gönderen kimliği bağlantının kimliğidir.

### 6. Sınırlar ve kapanış kodları

| Konu | Değer | Aşılınca |
|---|---|---|
| Mesaj boyutu | 64 KiB | 1009 |
| Komut oranı (bağlantı başına) | 5/sn, burst 10 (`x/time/rate`) | İlk aşımda `REALTIME_RATE_LIMITED` hata olayı. Art arda 20 aşımda 1008. |
| Gönderim kuyruğu | 64 olay | 1013 (yavaş istemci). Yeniden bağlanıp `?since=` ile tamamlar. |
| Ping | 20 sn | — |
| Boşta kalma | 60 sn, yani art arda 3 ping aralığı. Bu sürede hiçbir çerçeve (pong dahil) gelmezse bağlantı kapanır; kapanış son çerçeveden 60–80 sn sonradır. | 1008 |
| Oturum yeniden kontrolü | 1 dk | 4401 |
| Kullanıcı başına bağlantı | 10 | 429 (handshake) |

Diğer kapanış kodları:
- 1001: sunucu kapanıyor; istemci backoff ile yeniden bağlanır.
- 1003: ikili (binary) mesaj.
- 4401: oturum sona erdi (`Hub.DisconnectSession` ya da periyodik kontrol); yeniden giriş gerekir.

Kapanması istenen bağlantıya artık olay yazılmaz:
- Bağlantı fanout dizininden hemen çıkar.
- Yazıcı her mesajdan önce kapanış isteğine bakar.

### 7. Abonelik ve yetki

İstemci konu seçmez. Alıcıyı `Hub` belirler:
- `PublishUser` kullanıcının bütün bağlantılarına gönderir.
- `PublishConversation`, `Conversations.Participants`'ın döndürdüğü kullanıcılara gönderir.

`typing` önce `Conversations.IsParticipant` ile kontrol edilir. Katılımcı olmayan ile var olmayan konuşma aynı `CONVERSATION_NOT_FOUND` hatasını alır (ADR-0014). `Conversations` arayüzü Faz 4'te authz üzerinde uygulanır.

### 8. Router zinciri ve kapanış

- chi'de bir route, ebeveyninin middleware'inden çıkamaz. Bu yüzden `httpx.Router` zinciri ikiye ayırır:
  - kök: RequestID → SecurityHeaders → AccessLog → Recover,
  - sınırlı grup: + Timeout → BodyLimit.
- Modüller `Router` üzerine her zamanki chi metotlarıyla bağlanır ve sınırlı grubu alır. Yalnız `Router.Stream` ile bağlanan uç Timeout ve BodyLimit'i atlar.
- Stream deseni her metoda yanıt verir: GET'ten başkasına `405 Allow: GET`. Böylece `/v1` gibi bir öneke bağlanmış alt router'ın 404'üne düşmez.
- net/http hijack sırasında bağlantı deadline'larını temizler.
- `http.Server.Shutdown` hijack edilmiş bağlantıları ne kapatır ne bekler. Bu yüzden `httpx.Server`, `ServerOptions.Streams` alır:
  - Drain başlarken `StartDraining` çağrılır: yeni WS'ye 503.
  - Drain gecikmesinden sonra, HTTP kapanışıyla birlikte `Shutdown` çağrılır: her bağlantıya 1001 kapanış çerçevesi gider, goroutine'ler `ShutdownTimeout` içinde beklenir. Süre biterse bağlantılar çerçevesiz kesilir ve hata döner.
- Drain sırasındaki 503 log'a WARN olarak yazılır; diğer 5xx'ler ERROR kalır.

### 9. Çok örnek: bilinçli sınırlar

Fanout ve `DisconnectSession` bellek içidir; yalnız aynı süreçteki bağlantılara ulaşır (§9 R9). İkinci örnek açılana kadar bu bilinçli bir sınırdır:
- **Olaylar** yalnız yayımlayan örneğin bağlantılarına gider. Çözüm: `Hub.fanout`'u bir yayın kanalının arkasına almak. Postgres `LISTEN/NOTIFY`'da yük 8000 baytla sınırlı olduğu için bildirim yalnız olay kimliğini ve alıcıları taşır, gövde tablodan okunur.
- **Oturum iptali** başka bir örnekteki bağlantıya hemen ulaşmaz. O bağlantı en geç bir sonraki oturum kontrolünde (1 dk) kapanır. Aynı `LISTEN/NOTIFY` kanalı iptali de taşıyınca sınır kalkar.
- **Bilet** deposu Postgres'e geçince örnekler arasında çalışır.

### 10. Plandan sapmalar

- **Paket yeri:** Plan §3.1 realtime'ı `internal/messaging/` altında (`messaging/ (+realtime)`) gösteriyor. Kod `internal/realtime` altında. Bu katman konuşmalara bağlı değil: bilet, bağlantı, zarf ve fanout taşıyor; konuşma bilgisini `Conversations` arayüzünden alıyor. Bildirimler, ödeme durumu ve atama olayları da aynı kanalı kullanacak. Messaging, Faz 4'te bu paketi kullanan taraf olur.
- **`apperr.KindUnavailable` (503):** Drain sırasındaki handshake reddi için eklendi; plan §3.4'teki listede yok. ADR-0007'deki Kind listesinin yanında bu kayıt geçerlidir.

## Sonuçlar

- Java'daki "istemci sahte olay basar" açığı sınıf olarak kapanır. Bilet tekrar kullanılamaz, cihaz ve oturuma bağlıdır.
- Testler: `internal/realtime/*_test.go`. Bunlara yetkisiz olay, sahte olay, bilet tekrarı, çerçeve sınırı, oran sınırı, boşta kalma, yavaş istemci, oturum iptali, kapanış ve goroutine sızıntısı dahil.
- `internal/app` wiring'i şunlarla birlikte gelir:
  - Faz 1: kimlik (`Authenticator`, `Sessions`) ve origin listesi yapılandırması,
  - Faz 4: konuşma yetkisi (`Conversations`) ve Postgres `TicketStore`.
- İstemcilerde STOMP → WS geçişi mobilin en riskli kalemlerinden biri olmaya devam ediyor (§4.2).
