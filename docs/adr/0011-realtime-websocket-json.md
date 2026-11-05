# ADR-0011: Realtime: düz WebSocket + JSON olaylar

- Durum: Kabul edildi. Bilet akışı Faz 0 spike'ıyla netleşti; aşağıdaki "Ek: WS bileti spike'ı" bölümüne bakın.
- Tarih: 04.11.2026
- Karar kaydı: K4
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K4, §0.1 K4, §4.1-8, §8 madde 1 ve 3, §9 R9; N01 §8.1, §8.3

## Bağlam

Java'da realtime, Spring'in STOMP simple broker'ıyla çalışıyor. Bununla ilgili sorunlar:
- Go'da olgun bir STOMP sunucusu yok (N01 §8.1).
- İstemci `/topic/**`'ya SEND ile sahte olay basabiliyor (§8 madde 3; plan: kısmen doğrulandı, testle doğrulanmadı).
- REST presence ucu ilişki kontrolü yapmıyor; herkes herkesin online bilgisini görüyor (§8 madde 1).

Sözleşme zaten değişiyor (ADR-0005).

## Karar

1. **Düz WebSocket + JSON olay protokolü** kullanılır. Kütüphane `github.com/coder/websocket`.

   Gerekçe: Slack, Discord ve WhatsApp Web kendi olay protokollerini düz WebSocket üzerinde kullanıyor.
2. **Bağlantı:**
   - Uç `GET /v1/realtime`, alt protokol `koala.v1`.
   - Kimlik, `POST /v1/realtime/tickets` ile alınan 60 sn'lik tek kullanımlık biletle sağlanır. Cihaz eşleşmesi yapılır.
3. **Sunucu-itmeli model.**
   - İstemci konu seçmez. Sunucu, authz'a göre kullanıcının konuşmalarına ait olayları iter.
   - Presence yalnız ortak sohbeti olan kişiler için itilir.
   - İstemciden yalnız tipli komutlar gelir: `typing` ve `presence.watch`. `presence.watch` en fazla 100 id alır, her biri authz'dan geçer.
4. **Olay zarfı** `{type, id, occurredAt, data}` biçimindedir.
   - Hata olayları REST'teki `code` kümesini kullanır.
   - İstemci `id` ile tekilleştirir.
   - Kopma sonrası eksikler REST'ten `?since=` ile tamamlanır.

## Sonuçlar

- İstemcinin sahte olay basabildiği açık sınıfı tümden kalkar.
- 64 KB çerçeve sınırı ve oran sınırları Faz 4 çıkış kriteridir (§6.1).
- Tek örnek varsayımı (fanout bellek içinde) açıkça belgelenir. İleride Postgres LISTEN/NOTIFY kullanılabilir (§9 R9).
- WebSocket ucu, `httpx.Server`'ın genel `WriteTimeout`'undan ve router'daki istek başına süre sınırından (`httpx.Timeout`, `RequestTimeout`) muaf tutulmalı. Kalıcı bağlantı için kendi deadline yönetimi gerekir; bu Faz 4'te ele alınır.
- `gorilla/websocket` depguard ile yasaktır; tek WebSocket kütüphanesi kullanılır.
- İstemcilerde STOMP → WS geçişi mobilin en riskli kalemlerinden biri (§4.2).

## Ek: WS bileti spike'ı (05.11.2026)

Bu ek, Durum satırında açık bırakılan bilet akışını kapatır. Yukarıdaki kararları değiştirmez, ayrıntılandırır. Kod: `internal/realtime`, `internal/platform/httpx` (`Router.Stream`, `ServerOptions.Streams`).

### Bilet nasıl taşınır

**Karar:** Bilet, `Sec-WebSocket-Protocol` listesinde ikinci bir girdi olarak taşınır:

```
Sec-WebSocket-Protocol: koala.v1, koala.ticket.<bilet>
```

Sunucu yalnız `koala.v1`'i seçer, bileti yanıtta geri yazmaz. Erişim token'ı hiçbir zaman handshake'e girmez.

Gerekçe:
- **URL'de bilet yok.** Slack `rtm.connect` bileti URL'ye koyar. Bizim istek log'umuz query yazmaz ama proxy (Traefik/Dokploy), CDN ve tarayıcı geçmişi yazar. "Token URL'ye konmaz" kuralı bu yüzden. Kubernetes API sunucusu da tarayıcıdan gelen WebSocket'te kimliği aynı yolla, alt protokol girdisi olarak taşır.
- **Uzun ömürlü token yok.** 15 dk'lık erişim token'ını alt protokole koymak mümkün, ama başlığı loglayan bir ara katman token'ı tam ömrüyle sızdırır. Bilet 60 sn geçerli ve tek kullanımlıktır; sızarsa iş görmez.
- **İlk mesajla kimlik (Discord IDENTIFY) yok.** O modelde kimliği doğrulanmamış soket önce açılır, goroutine ve bellek tutar, ayrı bir zaman aşımı ister. Bilet handshake'te, upgrade'den **önce** doğrulanır. Ret düz bir HTTP problem yanıtıdır, soket hiç açılmaz.
- **Tarayıcıda çalışır.** Tarayıcı WebSocket API'si özel başlık gönderemez, alt protokol listesi gönderebilir. Mobil ve web aynı yolu kullanır.

### Bilet

- `POST /v1/realtime/tickets` (kimlikli REST, istek süresi sınırlı route) → `201 {"ticket","expiresAt"}`, `Cache-Control: no-store`.
- Değer 32 rastgele bayt, base64url (43 karakter). **Opak**: imzalı JWT değil. Tek kullanımlık olmak zaten sunucu tarafında durum gerektiriyor; durum varken imzaya ve anahtar yönetimine gerek yok. Bu yüzden ADR-0015'teki "realtime bileti ayrı anahtar" kalemi gereksiz kalır (açık soru olarak PR'da).
- Depoda değer değil, SHA-256'sı tutulur (refresh token ile aynı gerekçe, plan §3.9).
- Bilet (kullanıcı, oturum, cihaz) üçlüsüne bağlıdır. Kullanımda sırasıyla şunlar kontrol edilir; hepsinin reddi aynı `401 REALTIME_TICKET_INVALID`'dir:
  1. biçim,
  2. atomik `Take` (bulunamadı ya da daha önce kullanıldı),
  3. süre (`ExpiresAt`, sahte saatle test edilir),
  4. handshake `X-Device-Id` taşıyorsa biletin cihazıyla eşitliği,
  5. oturumun hâlâ etkin olması (`Sessions` arayüzü; ADR-0015 §2'deki anlık iptal handshake'te de geçerli).
- **Cihaz bağı ve tarayıcı:** Tarayıcı handshake'te `X-Device-Id` gönderemez. Bu yüzden bağ, bilet verilirken (oturumun cihazından) ve kullanımda oturum kontrolüyle sağlanır. Başlığı gönderebilen yerel istemci için eşitlik ek bir savunmadır.
- `TicketStore` arayüzü DB'ye hazırdır: `Put` = `INSERT`, `Take` = `DELETE … WHERE hash = $1 RETURNING …`. Bu, çok örnekte de tek kullanımı korur; süresi geçen satırları River periyodik işi siler. Spike'taki `MemoryTicketStore` tek süreçliktir ve Faz 4'te kalkar.

### Bağlantı ve kontroller

`GET /v1/realtime` sırası: upgrade + `koala.v1` (yoksa 400 `REALTIME_PROTOCOL_UNSUPPORTED`) → Origin (403 `REALTIME_ORIGIN_FORBIDDEN`) → drain (503 `REALTIME_DRAINING`) → bilet (401) → kullanıcı başına bağlantı sınırı (429 `REALTIME_TOO_MANY_CONNECTIONS`) → upgrade.

- Origin ve drain kontrolü bilet harcanmadan önce yapılır.
- Origin: Origin başlığı yoksa (yerel istemci) kabul. Varsa ya isteğin kendi host'u ya da listedeki tam origin (`scheme://host[:port]`) olmalı. Bilet ortam (ambient) kimlik bilgisi olmadığı için klasik CSWSH zaten kapalı; Origin kontrolü ek savunmadır.
- 503 için `apperr.KindUnavailable` eklendi (bu spike'ın önerisi; ADR-0007'deki listeye eklenmeli).

### Zarf ve protokol sürümü

- Sürüm alt protokoldür: kırıcı değişiklik `koala.v2` olarak yan yana gelir. `v1` içinde yalnız ekleme yapılır; istemci bilmediği olay tipini ve alanı yok sayar. Mesaj başına sürüm alanı yoktur.
- Sunucu → istemci: `{type, id, occurredAt, data}`. `id` her olayda benzersizdir. `occurredAt` RFC 3339 UTC'dir.
- İstemci → sunucu: `{type, id?, data}`. `id` istemcinin seçtiği korelasyon kimliğidir, hata olayında `commandId` olarak geri döner. Zaman alanı yoktur.
- Olay tipleri (`EventType`) ve komut tipleri (`CommandType`) ayrı Go tipleridir. Bugün olaylar `hello`, `error`, `conversation.typing`; komut yalnız `typing`. `presence.watch` Faz 4'te gelir, o zamana kadar bilinmeyen komuttur.
- Komut katı çözülür: bilinmeyen alan (ör. `userId`, `occurredAt`) ve fazladan veri reddedilir. Bilinmeyen komut tipi `error` olayıyla (`REALTIME_COMMAND_UNKNOWN`) reddedilir, bağlantı açık kalır.
- Hiçbir istemci mesajı aktarılmaz. `typing` komutundan sunucu yeni bir `conversation.typing` olayı kurar; gönderen kimliği bağlantının kimliğidir.

### Sınırlar ve kapanış kodları

| Konu | Değer | Aşılınca |
|---|---|---|
| Mesaj boyutu | 64 KiB | 1009 |
| Komut oranı | 5/sn, 10 burst (bağlantı başına) | ilk aşımda `REALTIME_RATE_LIMITED`; art arda 20 aşımda 1008 |
| Gönderim kuyruğu | 64 olay | 1013 (yavaş istemci; yeniden bağlanıp `?since=` ile tamamlar) |
| Ping | 25 sn | — |
| Boşta kalma | 60 sn hiçbir çerçeve (pong dahil) gelmezse | 1008 |
| Kullanıcı başına bağlantı | 10 | 429 (handshake) |

Diğer kapanış kodları: 1001 sunucu kapanıyor (backoff ile yeniden bağlan), 1003 ikili (binary) mesaj, 4401 oturum sona erdi (yeniden giriş gerekir; `Hub.DisconnectSession`).

### Abonelik ve yetki

İstemci konu seçmez. `Hub` alıcıyı kendisi belirler: `PublishUser` kullanıcının bütün bağlantılarına, `PublishConversation` `Conversations.Participants`'ın döndürdüğü kullanıcılara gönderir. `typing` önce `Conversations.IsParticipant` ile kontrol edilir. Katılımcı olmayan ile var olmayan konuşma aynı `CONVERSATION_NOT_FOUND` hatasını alır (ADR-0014). `Conversations` arayüzü Faz 4'te authz üzerinde uygulanır.

### Router zinciri ve kapanış

- chi'de bir route ebeveyninin middleware'inden çıkamaz. Bu yüzden `httpx.Router` zinciri ikiye ayırır: kök (RequestID → SecurityHeaders → AccessLog → Recover) ve sınırlı grup (+ Timeout → BodyLimit). Modüller `Router` üzerine her zamanki chi metotlarıyla bağlanır ve sınırlı grubu alır. Yalnız `Router.Stream` ile bağlanan uç (WebSocket) Timeout ve BodyLimit'i atlar. Sunucunun bağlantı deadline'larını net/http hijack sırasında temizler.
- `http.Server.Shutdown` hijack edilmiş bağlantıları ne kapatır ne bekler. `httpx.Server` bu yüzden `ServerOptions.Streams` alır: drain başlarken `StartDraining` (yeni WS'ye 503), drain gecikmesinden sonra HTTP kapanışıyla birlikte `Shutdown` (her bağlantıya 1001 kapanış çerçevesi, goroutine'ler `ShutdownTimeout` içinde beklenir; süre biterse bağlantılar çerçevesiz kesilir).

### Çok örnek

Fanout bellek içidir; bir olay yalnız aynı süreçteki bağlantılara gider (§9 R9). İkinci örnek açıldığında `Hub.fanout` bir yayın kanalının arkasına alınır: Postgres `LISTEN/NOTIFY` (8000 baytlık yük sınırı nedeniyle bildirimde yalnız olay kimliği ve alıcılar, gövde tablodan okunur) ya da bir mesaj aracısı. Bilet deposu Postgres'e geçtiğinde bilet örnekler arasında zaten çalışır.
