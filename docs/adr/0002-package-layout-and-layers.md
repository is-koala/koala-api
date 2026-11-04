# ADR-0002: Paket düzeni ve katman kuralları

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.1, §3.2; N05 A1, A3, A5

## Bağlam

Referans mimari olan sky lab core-backend'de (N05) iyi kalıplar var:
- tek yetki karar noktası,
- fail-closed config,
- sıralı graceful shutdown,
- `healthcheck` alt komutu,
- problem+json ile stabil `code`.

Aynı repoda Koala'ya taşınmaması gereken alışkanlıklar da var:
- 1.443 satırlık `main.go`, tek pakette 245 route, 14 pakette tekrarlanan sentinel hatalar ve 16 ayrı hata eşleyici, stdlib `log`, root kullanıcıyla çalışan `alpine:latest` imajı (plan §3.2-5, N05 A5);
- sağlayıcı tarafında tanımlanmış `Store` arayüzleri ve bellek içi store'lar (plan §3.2-2, N05 A1).

## Karar

1. **Düzen (§3.1).**
   - `cmd/koala`: ince `main`. Alt komutları `serve`, `migrate`, `healthcheck`, `version`.
   - `internal/app`: wiring; modül başına bir `wire<Modül>()`.
   - `internal/platform/*`: alan bilgisi taşımayan altyapı (config, log, httpx, apperr, ileride db, clock, money, jobs…).
   - `internal/<alan>/`: her alan `domain.go`, `service.go`, `store.go`, `http.go`.
   - `db/migrations`, `db/queries/<alan>`: SQL kaynakları.
   - `api/`: OpenAPI sözleşmesi.
2. **Bağımlılık yönü `http → service → store`** (§3.2-1). `domain.go` içinde `net/http`, `pgx` ya da JSON etiketi bulunmaz. `internal/platform` alan paketlerini import etmez (§3.1: "alan bilgisi taşımayan ortak altyapı").
3. **Arayüzler kullanan tarafta tanımlanır** (§3.2-2). Servis yalnız ihtiyaç duyduğu 2–3 metotluk arayüzü tanımlar. Bellek içi store yazılmaz; servis testleri gerçek PG'de, test başına ayrı DB ile koşar. Sahte implementasyon yalnız dış entegrasyonlar (Iyzico, FCM, SMTP…) için yazılır.
4. **İş kuralları tek yerde durur** (§3.2-3).
   - Para hesabı `money` ve `payment/domain.go` içinde.
   - FSM'ler tablo olarak `domain.go` içinde.
   - Yetki kararı yalnız `authz` içinde.
   - "Yanlış soyutlama yerine biraz tekrar" ilkesi yalnız teknik kodda (handler, eşleyici) geçerli.
5. **Alanlar arası çağrı arayüz üzerinden yapılır** (§3.2-4). Atomiklik gerektiğinde tx'e bağlı `db.Querier` parametre olarak geçirilir (core-backend'in tx enjeksiyon kalıbı, N05 A3).
6. **Wiring elle yapılır** (§3.2-5: `main.go` ince, wiring `internal/app/`'te modül başına bir dosyada). DI kütüphanesi (wire, fx, dig) kullanılmaz; bu kısım bu PR'ın önerisi.

## Sonuçlar

- Madde 2 makineyle zorlanır:
  - `domain.go`'da import yasakları ve `platform` sınırı depguard'la (`.golangci.yml` → `depguard.rules.domain`, `platform`). Kuralların gerçekten tetiklendiğini CI'daki `make lint-probe` kanıtlar.
  - `domain.go`'da JSON struct tag'i import gerektirmediği için depguard'ın göremediği bir durum; `internal/archtest` içindeki AST testi yakalar.
- Madde 6'daki DI kütüphaneleri (fx, dig, wire) depguard'ın `rejected` listesinde. Madde 3'ün "küçük, tüketen tarafta arayüz" kısmı lint'le zorlanmıyor; inceleme kuralıdır.
- "Bellek içi store yok" kararı servis testlerini gerçek PG'ye bağlar. Bunun için Faz 1'de template DB ile hızlı izole DB altyapısı kurulur (§6.1 Faz 1, §9 R10).
- `cmd/koala/main.go` sinyal yakalama ve `app.Run` çağrısından ibarettir. Büyüdüğü görülürse wiring yanlış yerdedir.
