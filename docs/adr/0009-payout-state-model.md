# ADR-0009: Payout durum modeli

- Durum: Kabul edildi (`PAID`'in kanıt raporu sandbox'ta doğrulanacak)
- Tarih: 04.11.2026
- Karar kaydı: K2
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K2, §0.1 K2, §8 madde 12, §6.3; N03 §2.4

## Bağlam

Java'da payout `COMPLETED` semantiğinde belge ile kod çelişiyor (§8 madde 12):
- Belge "tek yol banka ekstresi mutabakatı" diyor.
- Kod ise Iyzico APPROVE yanıtında referans varsa doğrudan `COMPLETED` yazıyor (`SettlementManager`).

APPROVE, talimatın kabul edildiğini söyler; paranın hesaba geçtiğini söylemez (N03 §2.4). Plan bu kararı para yolunun eşitlik hedefinin ön koşulu sayıyordu (§0-8); karar 04.11'de K2 ile verildi. Plan §8 madde 12'deki "`PROVIDER_APPROVED` ve `SETTLED`" önerisi K2'den önce yazıldı.

## Karar

Payout durumları **Stripe Connect modelini** izler:

```
PENDING ──(Iyzico APPROVE)──▶ IN_TRANSIT ──(banka ekstresi mutabakatı)──▶ PAID ──(iade geldi)──▶ RETURNED
```

- `PENDING → IN_TRANSIT`: Iyzico APPROVE.
- `IN_TRANSIT → PAID`: banka ekstresi mutabakatı.
- `PAID → RETURNED`: `PAID`'den sonra gelen iade.
- Ayrıca `FAILED` ve `CANCELED` durumları var. Bunlara hangi durumlardan geçileceğini karar kaydı belirtmiyor; Faz 6'da FSM tablosuyla tanımlanır.

Gerekçe: Stripe payout'ları `pending / in_transit / paid / failed / canceled` durumlarını kullanıyor ve "paid" sonrası iade mümkün. Sağlayıcı onayı ile paranın hesaba geçmesi ayrı durumlardır.

Bu karar §0.1'deki `PROVIDER_APPROVED` / `SETTLED` önerisinin yerine geçer.

## Sonuçlar

- FSM, `payment/domain.go` içinde tablo olarak yazılır. Bütün (durum, olay) çiftlerini kapsayan geçiş tablosu testi zorunludur (§3.2-3, §6.1 Faz 3 ve 6).
- `PAID`'e geçiş için kanıt şarttır. Java'daki `chk_payouts_completed_requires_evidence` kısıtının karşılığı yeni durum adıyla taşınır (§6.3).
- Iyzico'da `PAID`'i hangi raporun kanıtladığı sandbox'ta doğrulanacak (K2). Bu, Faz 6'nın açık kalemidir.
- Ayrı iki durum hem işçi ekranını ("yolda" ile "hesaba geçti" ayrımı) hem de kanıt ilkesini korur (§0.1 K2 gerekçesi).
