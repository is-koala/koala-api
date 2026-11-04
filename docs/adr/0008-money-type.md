# ADR-0008: Para tipi: `int64` kuruş + para birimi

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K5
- Kaynak: plan §0.0 K5, §3.7, §6.3; N03 §2.3, §7.2-2

## Bağlam

Java'da para `BigDecimal` olarak tutuluyor. JSON'a string ondalık olarak gidiyor. Iyzico sınırında `"100.00"` → `"100"` gibi ölçek tuzakları var (N03 §7.2-2).

Komisyon ve iptal hesaplarında HALF_UP yuvarlama kuralı birden çok yerde uygulanıyor (N03 §2.3).

İstemcilerden Dart ve JS'de float kullanmak kuruş hatası üretir.

§0.1'de `shopspring/decimal` ile `int64` kuruş karşılaştırılmış, JSON'da string ondalık önerilmişti.

## Karar

1. **İçeride `int64` kuruş + para birimi.**
   - `money.Amount` tutarı en küçük birimde tutar.
   - Oranlar baz puandır: `money.Rate`, 1250 = %12,5.
   - Yuvarlama tek fonksiyondadır (`MulRateHalfUp`). Komisyon ve iptal kuralları bunun üzerine kurulur.
2. **API'de Stripe/Adyen biçimi** kullanılır: `{"amount": 15000, "currency": "TRY"}`. Gerekçe: Stripe, Adyen ve Square tutarı en küçük birimde tam sayı olarak taşıyor; Dart/JS float tuzağı kalkıyor.
3. **`"150.00"` biçimine çeviri yalnız Iyzico sınırında yapılır.**
   - İstek gövdesinde ölçek korunarak `json.Number` yazılır.
   - Null alan politikası Java ile aynıdır.
   - Yanıt imzası normalizasyonu (`stripTrailingZeros().toPlainString()`) birebir taklit edilir ve altın vektörlerle test edilir (§3.7).
4. **DB'de `NUMERIC(19,2)` kalır**, runbook SQL'leri okunur kalsın diye (§3.7). pgx kodlayıcısı tek yerde yazılır. K5 bu maddeyi değiştirmiyor.
5. **Bağımlılık yok.** `shopspring/decimal` depguard ile yasaktır (ADR-0003).

## Sonuçlar

- **K5, plan §0.1'deki "JSON'da string ondalık" önerisinin yerine geçer.** Plan belgesinin §3.7 ("JSON: `"150.00"` biçiminde string") ve §4.1-5 ("Para string ondalık") maddeleri bu karardan önce yazıldı ve geçersizdir.
- OpenAPI ortak bileşenlerindeki para şeması bu biçimle yazılır (ayrı PR).
- Vergi (KDV, stopaj) v1'de yok; ayrı bir ürün ve hukuk kararı (§3.7, §9 S3).
- Property testleri: `total = payout + commission`, `refund + fee = collected` (§6.3).
