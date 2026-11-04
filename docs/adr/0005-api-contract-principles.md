# ADR-0005: API sözleşmesi ilkeleri

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K10, K11 (K5 para biçimi için ADR-0008)
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §4.1, §3.8, §0.0 K10–K11; N06

## Bağlam

API değişiyor: Java'nın sözleşmesi korunmuyor (ADR-0001). Üç istemci (web, admin, mobil) zaten uyarlanacak (§4.2), bu yüzden sözleşme bir kez ve doğru kurulmalı.

Bugünkü sorunlar şunlar:
- Yanıtlar zarflı (`success`, `message`, `httpStatus`).
- Admin'de 54 dosya numaralı sayfa kullanıyor (N06 §3.2).
- İş saatleri dilimsiz duvar saati olarak taşınıyor. Bu, mobilde 56, web'de 51 referansla sessiz hatanın en olası kaynağı (§0.1 K11).
- Hata kodu listesi istemcide Java kaynağı taranarak çıkarılıyor (§3.4).

## Karar

1. **Sözleşme önce.**
   - `api/openapi.yaml` (OpenAPI 3.1) tek doğruluk kaynağıdır; bileşenler `api/components/*.yaml`'dadır (§3.1). Akış: spec PR'ı → lint → kod üretimi → uygulama.
   - Her operasyonda `operationId` ve `x-authz` ya da açık `security: []` bulunur.
   - Personel operasyonlarında `x-authz-permission` bulunur. Eşlemesi eksikse sunucu açılmaz.
2. **Yollar.**
   - Taban yol `/v1`. Kaynak adları çoğul ve kebab-case. Kendi verin `/v1/me/...` altında.
   - Durum geçişleri alt eylem POST'udur (`…/complete`), Stripe'ın `…/confirm` kalıbı gibi.
   - Alan adları camelCase. Enum değerleri UPPER_SNAKE.
3. **Zarf yok.** Başarılı yanıt çıplak gövdedir. Hata yanıtı RFC 9457'dir (ADR-0007). Mesaj metinleri istemcidedir; sunucu yalnız kod döner. E-posta, SMS ve push metinleri sunucuda kalır.
4. **Sayfalama her yerde cursor'la yapılır (K10).**
   - İstek `?limit=&cursor=`, yanıt `{items, nextCursor, prevCursor, total?}`.
   - `total` yalnız `?includeTotal=true` ile gelir ve yalnız admin listelerinde ve SEO'lu aramada sunulur (§4.1-4).
   - Sıralama alanları allowlist'tedir; dışındaki alan 400 `SORT_FIELD_NOT_ALLOWED` döner.
   - Gerekçe (K10): Stripe, Shopify Admin API, GitHub ve Slack cursor kullanıyor; Stripe Dashboard ve Shopify Admin arayüzü de önceki/sonraki ile çalışıyor.
5. **Zaman (K11, §3.8).**
   - Anlık zamanlar `timestamptz` olarak tutulur, API'de RFC 3339 UTC `Z` ile döner.
   - İlan saatleri `starts_at timestamptz` + `time_zone` (varsayılan `Europe/Istanbul`) olarak tutulur, API'de ofsetli döner (`…T09:30:00+03:00`).
6. **Idempotency-Key** para ve yan etki doğuran POST'larda zorunludur.
   - Aynı anahtarla farklı gövde gelirse 422, işlem sürerken gelirse 409 döner.
   - Dayanak: IETF `draft-ietf-httpapi-idempotency-key-header`.
7. **Başlıklar.**
   - `X-Request-Id` yanıtta döner.
   - `X-App-Version` ve 426 + `X-Minimum-App-Version` korunur; `/v1` dışında da çalışır.
   - `X-Device-Id` zorunludur (admin hariç).
   - Netleştirilen: `X-Client-Platform`. Mobil bugün göndermiyor (N06 §1.2).
   - `Accept-Language`'e göre mesaj çözümü kalkar.

## Sonuçlar

- OpenAPI stil rehberi, ortak bileşenler (problem, sayfa, para, tarih) ve spec lint'i ayrı bir PR'da `api/openapi.yaml` ve `api/components/` olarak gelir (§6.1 Faz 0).
- Bu iskelette `X-Request-Id` uygulandı (`internal/platform/httpx.RequestID`); her durumda yanıtta döner. İyi biçimli gelen değerin kabul edilmesi (en fazla 64 karakter, `[A-Za-z0-9._-]`), gelmezse üretilmesi bu PR'ın önerisi.
- Plan §4.1-5 ve §3.7'deki "para string ondalık" ifadesi K5 ile geçersizdir; para biçimi ADR-0008'dedir.
- 422 ve 426 için `apperr`'e yeni `Kind` eklenmesi gerekecek. Bu, idempotency ve sürüm kapısı PR'larında yapılır (ADR-0007).
