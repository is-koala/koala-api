# ADR-0014: Authz: uygulama içi tek karar noktası, OpenFGA DSL alt kümesi

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K17 (ve plan §5, "karar 5")
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §5.1–5.7, §0-5, §3.4; N05 B, N07 §1–8

## Bağlam

Java'da yetki kontrolleri dağınık; 43 dosyada tahminen 130–150 kontrol var (§5.6).

core-backend'in tek karar noktası, capabilities ucu ve `both` kipi iyi kalıplar (N05 B6). Ancak core-backend'de şu eksikler var:
- ilişkiler JWT'deki string grup yollarından okunuyor,
- karar gerekçesiz bir `bool`,
- liste süzme bellekte yapılıyor,
- okuma denetimi yok.

Sektör pratiği (N07): Zanzibar tuple'ları, Meta'nın süreç içi gizlilik kontrolü, Oso, OPA ve Cerbos'un "yerel veriden SQL'e süzme" kalıbı. Monolitte ayrı bir authz servisi kurmamak Slack'in de ilk adımıydı.

Yusuf'un koşulu: sektörle tam uyumlu olsun ve ileride geçiş zor olmasın (§5.1).

## Karar

1. **Ayrı servis yok.** Uygulama içinde tek bir `authz` paketi var. Arayüzleri:
   - `Checker` (`Check`, `CheckBatch`),
   - `Scoper` (`Scope`: Never / Always / Conditional),
   - `CapabilityReader`,
   - `RelationSink`.

   `Decision`, `Basis` (denetim için) ve `Conceal` (ret → kaynağın kendi 404'ü) taşır (§5.3).
2. **Kurallar OpenFGA DSL'inde yazılır** (`internal/authz/model.fga`, schema 1.1). Kullanılan alt küme: `[type]`, `X from Y`, `or`, sınırlı `and`/`but not`, `with condition`.
3. **N07'nin 7 değişikliği** tasarıma dahildir (§5.2):
   - VIEW sıcak yolda kullanılmaz; ilişki başına tipli SQL kullanılır.
   - Durum koşulları "ReBAC dışı ön süzgeç" olarak adlandırılır.
   - `AtLeastAsFresh` tutarlılık parametresi var.
   - İlişki yazımları tek domain olayından geçer.
   - PII denetimi authz'dan ayrıdır (ayrı `pii` paketi).
   - Uygunluk test takımı var.
4. **Liste süzme SQL'de yapılır.** `Scope`, store'da tipli WHERE'e çevrilir. SQL parçası üretilmez. `ScopeNever` gelirse sorgu atılmaz.
5. **Personel RBAC** (25 izin, 10 rol, DAG ve görev ayrılığı) aynen taşınır.
   - Her istekte tek bir özyinelemeli CTE ile çözülür.
   - `/v1/admin/**` operasyonunun izni spec'teki `x-authz-permission`'dan gelir. Eşlemesi olmayan operasyon varsa sunucu açılmaz.
6. **Şirket rolleri v1'de ADMIN/STAFF'tır (K17).** Gerekçe iş kuralı eşitliği; yeni roller ürün kararıdır. Genişleme, `company` tipine relation eklenerek yapılır (§9 S2).
7. **Capabilities ucu** (`/v1/me/capabilities`, `/v1/admin/me/capabilities`) aynı `Check`'ten hesaplanır. İstemci JWT ayrıştırmaz. Uç okunamazsa UI kapalı başlar.

## Sonuçlar

- Uygunluk matrisi (`authz/testdata/matrix.yaml`) hem Go değerlendiricisine hem gerçek OpenFGA konteynerine karşı koşar. Fark varsa CI kırmızıdır. Böylece "kurallar OpenFGA alt kümesinde" iddiası test edilmiş olur (§5.6-5).
- Gizleme altın testleri: var olmayan kaynak ile yetkisiz erişilen kaynak için 404 gövdeleri bayt bayt eşittir (ADR-0007).
- OpenFGA/SpiceDB'ye geçiş: Check ve Capabilities kolay, tuple senkronu orta. `Scope` yerelde kalırsa toplam zorluk **kolay-orta**. Geçiş Slack'in dark/light mode sırasını izler (§5.7).
- Faz 0'da Java'daki kontrollerin envanteri çıkarılır (≥130 satır CSV). Bu ayrı bir iştir, bu repoda değil.
- Casbin depguard ile yasaktır (ReBAC ifade gücü yok, `Scope → WHERE` yapamıyor).
- `internal/authz` ve `internal/pii` bu PR'da oluşturulmadı. Faz 1'in işidir.
