# ADR-0015: Oturum modeli ve personel MFA

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K12, K13
- Kaynak: plan §0.0 K12–K13, §0.1 K12–K13, §3.9, §8 madde 2 ve 16, §6.3; N01 §3.3, §3.5, §4.6

## Bağlam

Java'daki oturum modeli şöyle:
- erişim token'ı 12 saat geçerli,
- refresh rotasyonu yok,
- JWT `iss` doğrulanmıyor (§8 madde 16).

OTP'de deneme sayacı yok, karşılaştırma `String.equals` ile yapılıyor ve 403/404 ayrımı kaydın var olup olmadığını sızdırıyor (§8 madde 2).

Personelin `user.read_pii` ve `payout.release` izinleri tek parolayla korunuyor (N01 §3.5).

İstemciler zaten uyarlanıyor (ADR-0005), bu yüzden oturum modelini değiştirmenin ek maliyeti düşük.

## Karar

1. **15 dk erişim token'ı + refresh rotasyonu + yeniden kullanım tespiti (K12).** Gerekçe: OAuth 2.0 Security BCP.
   - Her token sınıfı ayrı anahtar kullanır: marketplace, personel, realtime bileti.
   - `iss`, `aud` ve `alg` sıkı doğrulanır.
   - Refresh token opak ve 32 bayttır; DB'de SHA-256 ile saklanır.
   - Her kullanımda rotasyon yapılır. Eski token tekrar kullanılırsa o oturum ailesi iptal edilir.
2. **Her istekte oturum kontrolü** yapılır: oturum, cihaz ve kullanıcı durumu tek indeksli sorguyla kontrol edilir; iptal anında geçerli olur.
3. **Cihaz kimliği:** `X-Device-Id` zorunludur (admin hariç). UA'dan cihaz kimliği türetilmez; Yauaa kaldırılır.
4. **OTP:** deneme sayacı, sabit zamanlı karşılaştırma, DB'de hash ve tek tip 404.
5. **Personel için zorunlu MFA (TOTP) v1'de (K13).** Ayrıca `token_version`, hesap kilidi ve `must_change_password` var.

   Gerekçe: Yönetim panellerinde MFA zorunluluğu yerleşik pratik; `read_pii` ve `payout.release` tek parolayla korunmamalı.
6. **Parola hash'i:** argon2id (`x/crypto/argon2`). Taşınacak hash yok (§3.3).

## Sonuçlar

- Token testleri zorunludur (§6.3): `alg=none`, yanlış `aud` ya da `iss`, süre aşımı, anahtar karışması, refresh'in yeniden kullanımı → aile iptali.
- Mobilde `auth_interceptor` rotasyona uyarlanır. Mobilin en riskli kalemlerinden biri (§4.2).
- Bellek içi hız sınırı tek örnek varsayımına dayanır. Bu varsayım açıkça belgelenir (§3.9, §9 R9).
- TOTP Faz 8'de gelir; personel girişi, kilit ve `token_version` Faz 1'de (§6.1).
