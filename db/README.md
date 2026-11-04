# db

Veritabanı kaynakları (ADR-0012, ADR-0013). Faz 1'de dolar:

- `migrations/*.sql`: goose migration'ları, ikiliye `go:embed` ile gömülür, `koala migrate` ile uygulanır. `0001_baseline.sql` temiz hedef şemadır, `0002_seed.sql` referans verisidir.
- `queries/<alan>/*.sql`: sqlc girdisi. Uygulama sorguları temel tablolardan değil `active_<t>` view'larından okur.
- `softdelete.yaml`: her tablonun soft-delete sınıfı (view'lı ya da M1–M6 muaf) ve soft-delete'li tabloya giden her FK'nin politikası (`cascade` / `restrict` / `detach`).
