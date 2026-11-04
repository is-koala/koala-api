# ADR-0003: Bağımlılık politikası

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K19
- Kaynak: plan §3.3, §0.0 K19

## Bağlam

Çalışma anlaşmasına göre yeni bağımlılık onaysız eklenmez (K19 gerekçesi). Plan §3.3, Spring parçalarının Go karşılıklarını ve onaya sunulan bağımlılık listesini veriyor. Yusuf 04.11'de bu listeyi chi dahil onayladı (K19).

## Karar

1. **Onaylı liste** plan §3.3'teki "Onaya sunulan bağımlılıklar" tablosudur. Çekirdek kalemler: pgx/v5, chi/v5, sqlc (araç), goose/v3, oapi-codegen/v2, kin-openapi, golang-jwt/v5, `golang.org/x/{crypto,time,text,oauth2}`, google/uuid, River + riverpgxv5, coder/websocket, prometheus/client_golang, go-mail, aws-sdk-go-v2/s3, geoip2-golang, sentry-go (isteğe bağlı), openfga/language, testcontainers-go (yalnız test).
2. **Bir bağımlılık yalnız onu gerçekten kullanan PR'da eklenir.** "Nasılsa lazım olacak" diye eklenmez. `make verify` içindeki `go mod tidy -diff`, kullanılmayan modülü kırmızı yapar.
3. **Listede olmayan her bağımlılık ayrı onay ister.** PR gövdesinde lisans, gerekçe ve alternatif yazılır.
   - NFC bağımlılıklarının ayrı onay durumu ADR-0016'dadır.
   - Brainpool paketi hâlâ onay bekliyor.
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

   Bunlar `.golangci.yml`'de depguard'ın `rejected` kuralıyla import edilemez hâle getirildi. Stdlib `log`, `pkg/errors` ve testify de aynı kuralla yasak.
5. **Test bağımlılığı:** Testler yalnız stdlib `testing` ile yazılır (core-backend kalıbı, N05 A2). Test DB'si için testcontainers-go onaylı; alternatifi docker CLI.

## Sonuçlar

- Bu PR'da yalnız `github.com/go-chi/chi/v5` var.
- Lisanslar plan yazarının bilgisine dayanıyor. Her ekleme anında doğrulanmalı (§3.3 notu).
- golangci-lint `go.mod`'a girmez (ADR-0004). Plan §3.3 sqlc için `go tool` diyor. sqlc ve oapi-codegen'in nasıl sabitleneceğine, onları ilk kullanan PR'da ADR-0004'teki ölçütle karar verilir: araç bağımlılıkları uygulamanın modül grafiğine karışıyor mu?
