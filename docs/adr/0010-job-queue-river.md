# ADR-0010: İş kuyruğu: River

- Durum: Kabul edildi (goose entegrasyonu Faz 0 spike'ına bağlı)
- Tarih: 04.11.2026
- Karar kaydı: K3
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K3, §3.5, §9 R7; N02 §4, N03 §2.2, §3.3, §7.1; N05 A3

## Bağlam

Java'da 5 Rabbit kuyruğu ve 17 `@Scheduled` iş var. Bütün üreticiler ve tüketiciler aynı süreçte.

Outbox'lar zaten DB'de; Rabbit yalnız bir atlama ve ayrı bir konteyner ekliyor (§3.5). Bugün 5 ayrı claim/fencing uygulaması var (N03 §7.1).

Settlement'ta satır kilidi yok ve HTTP çağrısı tx içinde yapılıyor. Çok örnekte aynı APPROVE iki kez gidebilir (§8 madde 14; plan: doğrulanmadı, kod okuması).

`@Scheduled` işlerinde ShedLock yok.

## Karar

1. **River** (Postgres tabanlı iş kuyruğu) kullanılır. RabbitMQ kaldırılır. İkinci bir servis çıkarsa broker o gün eklenir.

   Gerekçe: tek uygulama + tek DB'de Postgres tabanlı kuyruk varsayılan yaklaşım; Rails 8 Solid Queue, Elixir Oban ve Go River bu kalıbı izliyor. Rabbit ve Kafka servisler arası olay için.
2. **Garantili işler** iş verisiyle aynı tx'te `InsertTx` ile kuyruğa girer (outbox semantiği). Realtime yayını gibi best-effort işler tx'e bağlı hook listesindedir ve yalnız commit başarılıysa çalışır (§3.5).
3. **`UniqueOpts`** (ör. `(payment_id, operation)`) çift kuyruklamayı engeller. Sağlayıcı çağrısı tx dışında yapılır, sonuç kısa bir tx'te yazılır.
4. **Periyodik işler** River'ın lider seçimli periodic job'larıyla çalışır. `robfig/cron` alınmaz.
5. **River'ın şeması goose'a SQL olarak alınır** (§9 R7). Entegrasyon Faz 0 spike'ında doğrulanır.

## Sonuçlar

- 5 sn'lik polling döngüleri ve beş ayrı claim uygulaması kalkar.
- River MPL-2.0 lisanslı; onaylı listede (ADR-0003).
- River K3 ve K19 ile onaylı. River'ın goose ile şema entegrasyonu Faz 0 spike'ında başarısız olursa core-backend'deki `FOR UPDATE SKIP LOCKED` + lease token kalıbı tek bir `platform/jobs` paketinde yazılır. Tahmini büyüklük ~400–600 satır (§3.5, K3-c).
- `amqp091-go` ve `robfig/cron` depguard ile yasaktır.
- Sıralı kapanışta worker context'i HTTP drain'inden sonra iptal edilir (§3.10). `internal/app/serve.go`'da bunun yeri işaretli.
