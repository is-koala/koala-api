# ADR-0011: Realtime: düz WebSocket + JSON olaylar

- Durum: Kabul edildi (bilet akışı Faz 0 spike'ına bağlı)
- Tarih: 04.11.2026
- Karar kaydı: K4
- Kaynak: plan §0.0 K4, §0.1 K4, §4.1-8, §8 madde 1 ve 3, §9 R9; N01 §8.1, §8.3

## Bağlam

Java'da realtime, Spring'in STOMP simple broker'ıyla çalışıyor. Bununla ilgili sorunlar:
- Go'da olgun bir STOMP sunucusu yok (N01 §8.1).
- İstemci `/topic/**`'ya SEND ile sahte olay basabiliyor (§8 madde 3).
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
- WebSocket ucu, `httpx.Server`'ın genel `WriteTimeout`'undan muaf tutulmalı. Kalıcı bağlantı için kendi deadline yönetimi gerekir; bu Faz 4'te ele alınır.
- `gorilla/websocket` depguard ile yasaktır; tek WebSocket kütüphanesi kullanılır.
- İstemcilerde STOMP → WS geçişi mobilin en riskli kalemlerinden biri (§4.2).
