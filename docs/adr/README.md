# Mimari karar kayıtları (ADR)

Her ADR tek bir kararı kaydeder: **Bağlam / Karar / Sonuçlar**. "Neden" kısmı plan belgesinden gelir; ADR'ler o belgeye bölüm numarasıyla atıf yapar.

**Plan belgesi:** [`docs/plan/go-gecis-degerlendirme.md`](../plan/go-gecis-degerlendirme.md) (04.11.2026). Belgenin atıf yaptığı N01–N08 notları repo dışındadır. Belgenin "0.0 Karar kaydı" bölümü bağlayıcıdır, §0.1 tablosu yalnız tarihseldir. İkisi çeliştiğinde karar kaydı geçerlidir. `K<n>` ve `S<n>` kısaltmaları o kayıttaki satırlardır. `N0x` ise plan belgesinin atıf yaptığı analiz notlarıdır.

| No | Başlık | Karar kaydı |
|---|---|---|
| [0001](0001-go-rewrite-and-repository.md) | Go'da baştan yazım ve yeni repo | K1, K16, K18 |
| [0002](0002-package-layout-and-layers.md) | Paket düzeni ve katman kuralları | §3.1, §3.2 |
| [0003](0003-dependency-policy.md) | Bağımlılık politikası | K19 |
| [0004](0004-quality-gate-and-tool-pinning.md) | Kalite kapısı (CI), Go sürümü ve araç sabitleme | §6.1 Faz 0–1 |
| [0005](0005-api-contract-principles.md) | API sözleşmesi ilkeleri | K10, K11 |
| [0006](0006-router-and-code-generation.md) | Router ve sözleşmeden kod üretimi: chi + oapi-codegen | K8, K9 |
| [0007](0007-error-model.md) | Hata modeli: `apperr` + RFC 9457 | §3.4 |
| [0008](0008-money-type.md) | Para tipi: `int64` kuruş + para birimi | K5 |
| [0009](0009-payout-state-model.md) | Payout durum modeli | K2 |
| [0010](0010-job-queue-river.md) | İş kuyruğu: River | K3 |
| [0011](0011-realtime-websocket-json.md) | Realtime: düz WebSocket + JSON olaylar | K4 |
| [0012](0012-data-access-and-migrations.md) | Veri erişimi ve migration: pgx + sqlc + goose | K6, K7 |
| [0013](0013-soft-delete.md) | Soft-delete tasarımı ve istisnaları | K14, K19, S4 |
| [0014](0014-authorization.md) | Authz: uygulama içi, OpenFGA DSL alt kümesi | K17, §5 |
| [0015](0015-sessions-and-staff-mfa.md) | Oturum modeli ve personel MFA | K12, K13 |
| [0016](0016-nfc-chip-verification-in-go.md) | NFC çip doğrulaması Go'da | K15, K19 |
| [0017](0017-river-goose-db-roles-and-test-db.md) | River + goose entegrasyonu, DB rol modeli ve test DB altyapısı | K3, K6, K7, K14 (ADR-0003 madde 6'nın, ADR-0004'ün "tek adım" kaleminin, ADR-0012 Karar 3'teki migration adlarının ve Karar 4'teki `InTx` imzasının yerine geçer) |
| [0018](0018-realtime-ticket-and-ws-protocol.md) | Realtime bileti ve WS protokol ayrıntıları | K4, K12 (ADR-0015 §1'in bir kaleminin yerine geçer) |
| [0019](0019-openapi-31-codegen-and-request-validation.md) | OpenAPI 3.1 spike sonucu, kod üretimi ve istek doğrulaması | K9, K5, K10, K11 (ADR-0005 madde 7'deki 426'nın yerine geçer) |
| [0020](0020-sqlc-toolchain-and-soft-delete-platform.md) | sqlc araç zinciri ve soft-delete platformu | K7, K14, K19 (plan §3.3'teki sqlc için "`go tool`" notunun yerine geçer) |
| [0021](0021-http-edge-clients-rate-limit-metrics.md) | HTTP kenarı: istemci başlıkları, oran sınırı, yol normalizasyonu ve metrikler | K12, §3.10 |
| [0022](0022-realtime-binding-and-session-check-corrections.md) | Realtime düzeltmeleri: cihaz bağı, oturum kontrolü, origin ve kapanış payı | K4 (ADR-0018'deki "bilet cihaza bağlıdır" iddiasının yerine geçer) |
| [0023](0023-image-release-pipeline-and-test-fakes.md) | İmaj kapısı, yayın hattı ve test sahteleri | §3.11, §6.1 Faz 1 ve 9 |

**Not:** Plandaki ADR-0013, 0014, 0016 atıfları koala-backend'e aittir; bu dizindeki aynı numaralı ADR'lerle ilgisi yoktur.

Bir ADR'de "bu PR'ın önerisi" diye işaretli kural plan belgesinde yoktur; iskelet PR'ında (#1) önerildi ve incelemeye açıktır. "(plan: doğrulanmadı)" işareti, planın kod okumasından çıkardığı ama doğrulamadığı bir bulguyu gösterir.

Yeni ADR: sıradaki numarayı al, bu tabloya ekle. Kabul edilmiş bir ADR değiştirilmez. Kararı değiştirmek için yeni ADR yazılır, eskisinin durumu "Yerine geçildi: ADR-NNNN" olur.
