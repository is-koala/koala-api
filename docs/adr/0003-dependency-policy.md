# ADR-0003: Bağımlılık politikası

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K19
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §3.3, §0.0 K19

## Bağlam

Çalışma anlaşmasına göre yeni bağımlılık onaysız eklenmez (K19 gerekçesi). Plan §3.3, Spring parçalarının Go karşılıklarını ve onaya sunulan bağımlılık listesini veriyor. Yusuf 04.11'de bu listeyi chi dahil onayladı (K19).

## Karar

1. **Onaylı liste** plan §3.3'teki "Onaya sunulan bağımlılıklar" tablosudur. Çekirdek kalemler: pgx/v5, chi/v5, sqlc (araç), goose/v3, oapi-codegen/v2, kin-openapi, golang-jwt/v5, `golang.org/x/{crypto,time,text,oauth2}`, google/uuid, River + riverpgxv5, coder/websocket, prometheus/client_golang, go-mail, aws-sdk-go-v2/s3, geoip2-golang, sentry-go (isteğe bağlı), openfga/language, testcontainers-go (yalnız test).
2. **Bir bağımlılık yalnız onu gerçekten kullanan PR'da eklenir.** "Nasılsa lazım olacak" diye eklenmez. `make verify` içindeki `go mod tidy -diff`, kullanılmayan modülü kırmızı yapar.
3. **Listede olmayan her bağımlılık ayrı onay ister.** PR gövdesinde lisans, gerekçe ve alternatif yazılır.
   - NFC bağımlılıklarının ayrı onay durumu ADR-0016'dadır.
   - `ProtonMail/go-crypto/brainpool`: gerekmiyor; yabancı belge kapsama alınırsa **ayrıca onay bekler** (ADR-0016 ile aynı ifade).
4. **Bilinçli olarak alınmayanlar** (§3.3):
   - GORM, ent;
   - fiber, echo;
   - viper;
   - zap, zerolog;
   - Casbin;
   - shopspring/decimal;
   - go-playground/validator;
   - amqp091-go;
   - robfig/cron;
   - firebase-admin-go;
   - UA ayrıştırıcıları.

   UA ayrıştırıcıları dışındakiler (belirli bir paket adı yok) `.golangci.yml`'de depguard'ın `rejected` kuralıyla import edilemez hâle getirildi. Stdlib `log` da yasak; plan §3.2-5 onu core-backend'den alınmayacaklar arasında sayıyor.
5. **Bu PR'ın önerisi olan yasaklar** (planda yok; depguard açıklamalarında "(PR #1 proposal)" diye işaretli):
   - `pkg/errors`: stdlib `errors` ve `apperr` yeterli.
   - `sqlx`, `lib/pq`: veri erişimi pgx + sqlc (ADR-0012).
   - `logrus`: slog (plan zap ve zerolog'u sayıyor, logrus'u değil).
   - `gorilla/websocket`: tek WebSocket kütüphanesi `coder/websocket` (ADR-0011).
   - fx, dig, wire: wiring `internal/app`'te elle yapılır (ADR-0002).
   - testify: testler yalnız stdlib `testing` ile yazılır. Dayanak core-backend kalıbı (N05 A2); plan bunu kural olarak koymuyor.
6. **Test DB'si:** testcontainers-go onaylı; alternatifi docker CLI (§3.3).

## Sonuçlar

- Bu PR'da yalnız `github.com/go-chi/chi/v5` var.
- Lisanslar plan yazarının bilgisine dayanıyor. Her ekleme anında doğrulanmalı (§3.3 notu).
- golangci-lint ve govulncheck `go.mod`'a girmez (ADR-0004). Plan §3.3 sqlc için `go tool` diyor. sqlc ve oapi-codegen'in nasıl sabitleneceğine, onları ilk kullanan PR'da ADR-0004'teki ölçütle karar verilir: araç bağımlılıkları uygulamanın modül grafiğine karışıyor mu?
