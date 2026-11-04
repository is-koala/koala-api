# ADR-0013: Soft-delete tasarımı ve istisnaları

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K14, K19 (muafiyetler), S4
- Kaynak: plan §0.0 K14, K19; §3.6-3; §5.7; §6.1 Faz 1 ve 8; §9 R16 ve S4; N08 §B

## Bağlam

Java'da soft-delete 66 tabloda Hibernate'in global filtresiyle uygulanıyor. Bu filtre #309 ve #321'deki join tuzağı sınıfını üretti: silinmiş ebeveyne INNER JOIN, aktif çocuk satırı da gizliyor (N02 §1.3). EF Core, Hibernate ve Rails'teki global filtrelerin bu tuzağı belgeli (N08 §B1).

§0.1'de "varsayılan yok" önerilmişti. Yusuf 04.11'de varsayılan soft-delete'i seçti (K14).

Go'da örtük filtre yok. Bu yüzden unutulan bir `WHERE deleted_at IS NULL`, ancak açık korumalarla yakalanabilir.

## Karar

1. **Varsayılan soft-delete VAR (K14).** Yeni tablo varsayılan olarak soft-delete'lidir. Muafiyet yalnız altı gerekçeli sınıfta vardır (K19 ile onaylı):

   | Sınıf | Kapsam | Davranış |
   |---|---|---|
   | M1 defter ve kanıt | ödeme, iade, payout, iptal, atama, puantaj, biniş, uyuşmazlık, rıza, sözleşme, denetim | **Silinmez.** `deleted_at` yok, uygulama rolünde DELETE yetkisi yok |
   | M2 kısa ömürlü | token, oturum | TTL ile hard delete |
   | M3 kuyruk | River tabloları | River yönetir |
   | M4 referans | referans verisi | `retired_at` |
   | M5 türetilmiş | türetilmiş tablolar | yeniden kurulur |
   | M6 bağlantı ve tekil ayar | bağlantı ve ayar tabloları | hard delete ya da upsert; katılımcıda `left_at` |

2. **Kolonlar.**
   - `deleted_at timestamptz` + `deletion_id uuid → deletions(id)`. CHECK: ikisi birlikte NULL ya da birlikte dolu.
   - `deletions` tablosu aktörü, nedeni ve kökü tutar.
   - `deletion_id` bir kaskad işleminin satırlarını gruplar; geri alma yalnız onları açar.
3. **Unutmayı imkânsızlaştıran mekanizma.**
   - Uygulama `active_<t>` view'larından okur ve yazar.
   - Uygulama rolü `koala_app`'in soft-delete'li temel tablolarda yetkisi yoktur. Unutulan filtre ilk testte `permission denied` verir.
   - Temel tablolara yalnız ayrı havuzdaki `koala_maint` rolü erişir: geri alma, temizlik, anonimleştirme. Bu rol depguard ile `privacy/`, `staff/` ve temizlik işleriyle sınırlanır.
   - `sqlc vet` kuralı, uygulama sorgularında temel tablo adını reddeder.
   - RLS seçilmedi. Filtre görünmez kalıyor, tek `USING` politikası soft-delete UPDATE'ini reddediyor ve tablo sahibi politikayı atlıyor.
4. **View #309'u tek başına çözmez.** Soft-delete'li tabloya giden her FK, `db/softdelete.yaml`'da bir politika taşır:
   - `cascade`: çocuk aynı `deletion_id` ile silinir.
   - `restrict`: aktif çocuk varken silme 409 döner.
   - `detach`: çocuk kalır; bu kenarda yalnız `LEFT JOIN active_*` kullanılır. Lint, `detach` kenarında INNER JOIN'i reddeder.
5. **Unique:** Her iş anahtarı kısmi indekstir (`WHERE deleted_at IS NULL`). Geri almada çakışma 409 `RESTORE_CONFLICT` döner.
6. **KVKK:** Soft-delete "silme" değildir.
   - Hesap silme: soft-delete → en geç 30 gün içinde anonimleştirme (K19).
   - Tablo başına `retention` vardır; aylık temizlik işi çalışır. Anonimleştirmeden sonra geri alma kapanır.
7. **S4: M1 silinmez** (sektör standardı, 04.11).
   - Defter kayıtları değişmez ve eklemeli tutulur; düzeltme ters kayıtla yapılır (muhasebe pratiği, VUK saklama süreleri).
   - Silinen hesabın TCKN'si yeniden kaydı engellemez.
   - Yalnız yasaklanmış hesaplar için TCKN'nin HMAC özeti dolandırıcılık önleme amacıyla saklanır.

## Sonuçlar

- Faz 1'in en büyük ek işi bu (+1–1,5 kh). Gerekenler: `deletions` tablosu, view ve GRANT goose yardımcıları, iki rol ve iki havuz, `softdelete` paketi, `db/softdelete.yaml`, katalog testleri, `sqlc vet` kuralı, JOIN lint'i, üretilmiş okuma ve JOIN testleri (§6.1 Faz 1).
- Katalog testi her tabloyu "view'lı" ya da "gerekçeli muaf" olarak sınıflandırmak zorundadır. Yeni tablo eklenince test kırmızı olur.
- Authz tuple VIEW'ı `active_*`'den kurulur; silinen nesnenin tuple'ları kendiliğinden düşer. OpenFGA'ya geçişte her silme ve geri alma, tuple olayı üretmelidir (§5.7, ADR-0014).
- **Açık:** S4'teki "yasaklı hesabın TCKN HMAC'i" kısmı KVKK açısından hukukçuya teyit ettirilecek.
