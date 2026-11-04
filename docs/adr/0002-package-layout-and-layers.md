# ADR-0002: Paket düzeni ve katman kuralları

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Kaynak: plan §3.1, §3.2; N05 A1, A3, A5

## Bağlam

Referans mimari olan sky lab core-backend'de (N05) iyi kalıplar var:
- tek yetki karar noktası,
- fail-closed config,
- sıralı graceful shutdown,
- `healthcheck` alt komutu,
- problem+json ile stabil `code`.

Aynı repoda Koala'ya taşınmaması gereken alışkanlıklar da var (plan §3.2-5, N05 A5):
- 1.443 satırlık `main.go`,
- tek pakette 245 route,
- 14 pakette tekrarlanan sentinel hatalar ve 16 ayrı hata eşleyici,
- sağlayıcı tarafında tanımlanmış `Store` arayüzleri ve bellek içi store'lar,
- stdlib `log`,
- root kullanıcıyla çalışan `alpine:latest` imajı.

## Karar

1. **Düzen (§3.1).**
   - `cmd/koala`: ince `main`. Alt komutları `serve`, `migrate`, `healthcheck`, `version`.
   - `internal/app`: wiring; modül başına bir `wire<Modül>()`.
   - `internal/platform/*`: alan bilgisi taşımayan altyapı (config, log, httpx, apperr, ileride db, clock, money, jobs…).
   - `internal/<alan>/`: her alan `domain.go`, `service.go`, `store.go`, `http.go`.
   - `db/migrations`, `db/queries/<alan>`: SQL kaynakları.
   - `api/`: OpenAPI sözleşmesi.
2. **Bağımlılık yönü `http → service → store`.** `domain.go` içinde `net/http`, `pgx` ya da JSON etiketi bulunmaz. `internal/platform` alan paketlerini import etmez.
3. **Arayüzler kullanan tarafta tanımlanır.** Servis yalnız ihtiyaç duyduğu 2–3 metotluk arayüzü tanımlar. Bellek içi store yazılmaz; servis testleri gerçek PG'de, test başına ayrı DB ile koşar. Sahte implementasyon yalnız dış entegrasyonlar (Iyzico, FCM, SMTP…) için yazılır.
4. **İş kuralları tek yerde durur.**
   - Para hesabı `money` ve `payment/domain.go` içinde.
   - FSM'ler tablo olarak `domain.go` içinde.
   - Yetki kararı yalnız `authz` içinde.
   - "Yanlış soyutlama yerine biraz tekrar" ilkesi yalnız teknik kodda (handler, eşleyici) geçerli.
5. **Alanlar arası çağrı arayüz üzerinden yapılır.** Atomiklik gerektiğinde tx'e bağlı `db.Querier` parametre olarak geçirilir (core-backend'in tx enjeksiyon kalıbı, N05 A3).
6. **Wiring elle yapılır.** DI kütüphanesi (wire, fx, dig) kullanılmaz.

## Sonuçlar

- Madde 2'nin `domain.go` ve `platform` kısımları depguard'la zorlanır (`.golangci.yml` → `depguard.rules.domain`, `platform`). Madde 6 bağımlılık politikasıyla zorlanır (ADR-0003).
- "Bellek içi store yok" kararı servis testlerini gerçek PG'ye bağlar. Bunun için Faz 1'de template DB ile hızlı izole DB altyapısı kurulur (§6.1 Faz 1, §9 R10).
- `cmd/koala/main.go` sinyal yakalama ve `app.Run` çağrısından ibarettir. Büyüdüğü görülürse wiring yanlış yerdedir.
