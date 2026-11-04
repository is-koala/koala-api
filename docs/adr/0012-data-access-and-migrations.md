# ADR-0012: Veri erişimi ve migration: pgx + sqlc + goose

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K6, K7
- Kaynak: plan §0.0 K6–K7, §0.1 K6–K7, §3.5, §3.6-1, §3.6-4; N02 §2, §5, §7; N05 A2, A5-5, A5-6

## Bağlam

Java'da JPA/Hibernate ve Spring Data kullanılıyor. Bunların örtük davranışları ayrı bir kusur sınıfı üretiyor (N02 §2):
- dirty checking,
- tam satır UPDATE,
- OSIV + LAZY,
- 14 varsayılan EAGER ilişki,
- global soft-delete filtresi.

Liquibase'de 317 changeset var.

core-backend'in durumu:
- 1,8 K satırlık kendi migration motorunu yazmış (N05 A5-5).
- sqlc kullanmıyor, 1,2 K satırlık elle SQL dosyaları var (N05 A5-6).

## Karar

1. **DB erişimi: `pgx/v5` + `sqlc` (K7).**
   - Statik sorgular sqlc ile tipli koda üretilir. sqlc, sorgu ile kolon uyumsuzluğunu derleme zamanında yakalar (N02 §5).
   - Dinamik admin aramaları (~10 sorgu) elle pgx ile yazılır.
   - ORM (GORM, ent) alınmaz; Hibernate'in örtük davranışlarını geri getirir.

   Gerekçe: Go'da yerleşik yaklaşım ORM değil, "SQL yaz, tipli kod üret".
2. **Migration: `goose` v3 (K6).**
   - `go:embed` ile gömülür, `koala migrate` alt komutuyla çalışır.
   - `$$` bloklarını ve `NO TRANSACTION`'ı destekliyor.
   - Kendi motorumuz gereksiz bir yük olur.
3. **Temiz baseline.** Liquibase taşınmaz (§3.6-1).
   - Hedef şema elle yazılır: `0001_baseline.sql`.
   - Referans verisi `0002_seed.sql`'e konur.
   - JSON alanları `jsonb` olur.
   - Enum'lar için CHECK kısıtı eklenir.
4. **Transaction (§3.5).**
   - `db.InTx(ctx, func(q Querier) error)` kullanılır. Tx servis metodu sınırında açılır, store `Querier` alır.
   - Dış HTTP çağrısı sırasında hiçbir tx açık kalmaz. Ödeme akışı üç kısa tx'e bölünür: rezerve et ve commit et → sağlayıcıyı çağır → sonucu yaz.
   - `pg_advisory_xact_lock` yalnız `Querier` (tx) alan fonksiyonlarda çağrılabilir; tip sistemi bunu zorlar.
   - Kilit gereken yerde açık `SELECT … FOR UPDATE` yazılır.
5. **Kimlikler:** Uygulama UUIDv7 üretir (§3.6-4).

## Sonuçlar

- `db/migrations/*.sql` (goose) ve `db/queries/<alan>/*.sql` (sqlc girdisi) Faz 1'de dolar. Bu PR'da `db/` yalnız düzeni anlatan bir README içerir.
- `koala migrate` bu PR'da yer tutucudur. "Not implemented" ile ve sıfırdan farklı çıkış koduyla döner.
- Soft-delete korumaları sqlc ile birlikte tasarlanır: `active_<t>` view'ları ve `sqlc vet` kuralı (ADR-0013).
- GORM, ent, sqlx ve `lib/pq` depguard ile yasaktır.
- Kısıt adı → hata kodu eşlemesi tek yerde yapılır (ADR-0007).
