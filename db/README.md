# db

Veritabanı kaynakları (ADR-0012, ADR-0013, ADR-0017). İşletim: [runbook](../docs/runbook/veritabani.md).

- `bootstrap/roles.sql`: cluster düzeyindeki grup rolleri: `koala_owner`, `koala_app`, `koala_maint`. `koala bootstrap` bunu her yayında `koala migrate up`'tan önce çalıştırır (`scripts/release-db.sh`); idempotenttir. DB `OWNER koala_owner` ile açılır. Giriş kullanıcılarını parolalarıyla operatör açar ve her birini tek bir grup rolüne üye yapar: migrate girişi `koala_owner`'a, sunucu girişi `koala_app`'e, bakım girişi `koala_maint`'e.
- `migrations/*.sql`: goose migration'ları. İkiliye `go:embed` ile gömülür, `koala migrate up|down|status` ile `koala_owner` rolünde uygulanır. Adlar `NNNN_<ad>.sql` biçimindedir ve sırayla numaralanır.
  - `0001_roles_and_schema.sql`: rolleri denetler; DB'de `PUBLIC`'in `CONNECT`/`TEMPORARY` hakkını, `public` şemasında ve yeni fonksiyonlarda `PUBLIC` haklarını kaldırır.
  - `NNNN_river_main_vVVV.sql`: River şemasının her sürümü ayrı bir dosyadır ve `river migrate-get` çıktısıdır; elle düzenlenmez. `migrations_test.go` bu dosyaları `go.mod`'daki River sürümüyle karşılaştırır.
  - `0009_river_grants.sql`: `koala_app`'e River tablolarında DML, dizilerde `USAGE, SELECT`, River fonksiyonunda `EXECUTE` verir.
  - Faz 1'in temiz baseline'ı (hedef şema), seed, `deletions` ve view/GRANT yardımcıları `0010` ve sonrasıdır.
- `queries/<alan>/*.sql` (Faz 1): sqlc girdisi. Uygulama sorguları temel tablolardan değil `active_<t>` view'larından okur.
- `softdelete.yaml` (Faz 1): her tablonun soft-delete sınıfı (view'lı ya da M1–M6 muaf) ve soft-delete'li tabloya giden her FK'nin politikası (`cascade` / `restrict` / `detach`).

Migration yazarken:
- Her dosya varsayılan olarak kendi transaction'ında koşar. `$$` gövdeli ifadeler `-- +goose StatementBegin` / `-- +goose StatementEnd` arasına yazılır.
- `CREATE INDEX CONCURRENTLY` gibi transaction'da çalışamayan işler `-- +goose NO TRANSACTION` işaretli, tek ifadelik ayrı bir dosyaya yazılır. Örnek: `internal/platform/dbtest/testdata/migrations/9002_*`.
- Yeni tablo, view, dizi ya da fonksiyon kendi GRANT'iyle gelir. `TestRoleGrants` yetkisi eksik ya da fazla olan nesneyi kırmızıya çevirir.
- Oturum düzeyinde `SET` yazılmaz; yalnız `InTx` içinde `SET LOCAL`.
- `koala migrate down` yalnız geliştirme içindir (ADR-0017 §3).

Entegrasyon testleri (`//go:build integration`, `make test-integration`) `internal/platform/dbtest` ile test başına template'ten klonlanmış bir DB alır. Postgres'i libpq ortam değişkenleri gösterir; CI'da bu `verify.yml`'deki service container'dır.
