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
| [0021](0021-http-edge-clients-rate-limit-metrics.md) | HTTP kenarı: istemci başlıkları, oran sınırı, yol normalizasyonu ve metrikler | K12, §3.10 (ADR-0005 madde 1'i ve plan §4.1-1/§6.3'ü `x-authz-exempt` ile genişletir; ADR-0005 madde 7 ve ADR-0015 madde 3'e cihaz istisnaları ekler; ADR-0018 §4'teki cihaz adımının ve §8'deki kök zincirin yerine geçer) |
| [0022](0022-realtime-binding-and-session-check-corrections.md) | Realtime düzeltmeleri: cihaz bağı, oturum kontrolü, origin ve kapanış payı | K4 (ADR-0018 §2'deki bilet ve cihaz bağının, Sonuçlar'daki "cihaz ve oturuma bağlıdır" cümlesinin, §4'teki aynı host origin kuralının ve §8'deki zorla kapatma beklemesinin yerine geçer; ADR-0011 madde 2 ve plan §4.1-8'deki cihaz eşleşmesini düzeltir) |
| [0023](0023-image-release-pipeline-and-test-fakes.md) | İmaj kapısı, yayın hattı ve test sahteleri | §3.11, §6.1 Faz 1 ve 9 |
| [0024](0024-authz-core-evaluator-and-conformance.md) | Authz çekirdeği: yorumlayıcı değerlendirici, model.json ve OpenFGA uygunluk koşucusu | ADR-0014'ün uygulaması; K17 (ADR-0014 Karar 2'deki alt kümeden `with condition`'ın ve ADR-0017'deki "tek gerekli kontrol" kaleminin yerine geçer) |
| [0025](0025-authentication-tokens-sessions-and-staff-sign-in.md) | Kimlik doğrulama: token sınıfları, oturumlar, refresh rotasyonu ve personel girişi | K12, K13 (hazırlık), D5 (ADR-0018 §2'deki "Postgres bilet deposu Faz 4'te" notunun ve ADR-0019 §10'un yerine geçer; ADR-0005 madde 1'i, plan §4.1-1 ve §5.5'i `/v1/admin` için genişletir; plan §6.1 Faz 1 ve §6.2-1'den sapmayı kaydeder) |
| [0026](0026-staff-rbac-audit-trail-and-pii-gate.md) | Personel RBAC, denetim kaydı çekirdeği ve PII kapısı | K13 (hazırlık), K14, S4, D5 (plan §5.5'teki "eşleşmeyen yol 403"ün yerine geçer; ADR-0013 ve S4'ün M1 "silinmez"ini ve ADR-0020 §8 (f)'yi saklama süreli kanıt tabloları için daraltır; ADR-0020 §8 (a)'ya `authz_tuples` istisnasını ekler ve §11'in `x-resource` kuralını getirir; ADR-0017'ye `KOALA_MAINT_DATABASE_URL`'i ekler ve saklama temizliğini Faz 1'e çeker; ADR-0025 §9'un self-servis listesini ve ADR-0005 madde 1'in biçimlerini genişletir) |
| [0027](0027-soft-delete-platform-hardening.md) | Soft-delete platformunun sertleştirilmesi: lint biçimleri, üretilmiş JOIN testi, kilit sırası | K14, S4 (ADR-0020 Karar 3'ün desen ve sınır listesinin, Karar 10'un son cümlesinin, "Sınır" listesinin ve "nullable tarafta" maddesinin, Karar 6'daki kilit ifadesinin ve test listesinin yerine geçer; "Bu PR'da kalanlar"daki üretilmiş JOIN testini kapatır) |
| [0028](0028-recursion-rules-body-allowlist-and-gate-completeness.md) | Model özyineleme kuralları, gövde maskesinde izin listesi ve kapının tamlığı | K17, K14, K9, K7 (ADR-0024 Karar 3'ün alt kümesini iki özyineleme kuralıyla daraltır ve §4'ün iki fark kaleminin gerekçesinin yerine geçer; ADR-0026 Karar 7'deki gövde maskesi tanımının yerine geçer ve açık soru 2'yi kapatır; ADR-0007 madde 5'i listelerin tamlığıyla, ADR-0004'ün `gen-check`'ini `pairs.py` çıktılarıyla genişletir) |
| [0029](0029-identity-user-company-and-media-contract.md) | Faz 2 sözleşmesi: kayıt, kimlik, kullanıcı, şirket ve medya | K10, K11, K14, K15 (hazırlık), K17, K19, S4, D5 (ADR-0020 §11 ve ADR-0026'nın `x-resource` denetimini `x-not-implemented` taşıyan operasyonda erteler; ADR-0019 §4'ün presigned yükleme kararını karantina öneğiyle uygular; plan §6.1 Faz 2'nin "P0'ların %100'ü yeşil" kriterini uygulama PR'larına bırakır; ADR-0027'den `Keeps` kalemini devralır) |
| [0030](0030-object-storage-media-pipeline-and-exif.md) | Nesne deposu istemcisi, yükleme hattı ve EXIF temizliği | K19, S4 (ADR-0029'un *Sonuçlar* bölümündeki, `FuzzStorageKey`'i `internal/media`'ya koyan cümlesinin yerine geçer; aynı kararın tamamlama adımındaki doğrulamasını tek savunma olarak kayda geçirir) |
| [0031](0031-gallery-cover-flag-and-cv-document-types.md) | Galeri kapağı bağımsız bir işaret, özgeçmiş üç belge türü | K10, K11, K19 (Yusuf'un 08.11.2026 kararları: ADR-0029 Karar 7'nin türetilmiş kapak daralmasının ve ADR-0030 açık soru 1'in "PDF-only" kararının yerine geçer; ADR-0029 Karar 5–6'nın ve ADR-0030 Karar 3'ün tip ve uzantı izin listelerini genişletir, Karar 4'ün "üstveri sökülmez" sınırını DOC ve DOCX'e taşır; plan §6.1 Faz 2'nin `owner` kriterini ve eğitim kayıtları boşluğunu düzeltir) |

**Not:** Plandaki ADR-0013, 0014, 0016 atıfları koala-backend'e aittir; bu dizindeki aynı numaralı ADR'lerle ilgisi yoktur.

Bir ADR'de "bu PR'ın önerisi" diye işaretli kural plan belgesinde yoktur; iskelet PR'ında (#1) önerildi ve incelemeye açıktır. "(plan: doğrulanmadı)" işareti, planın kod okumasından çıkardığı ama doğrulamadığı bir bulguyu gösterir.

Yeni ADR: sıradaki numarayı al, bu tabloya ekle. Kabul edilmiş bir ADR değiştirilmez. Kararı değiştirmek için yeni ADR yazılır, eskisinin durumu "Yerine geçildi: ADR-NNNN" olur.
