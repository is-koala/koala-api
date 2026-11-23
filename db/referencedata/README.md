# Referans verisi

M4 referans tablolarının satırları (ADR-0029 Karar 8, ADR-0036 Karar 7, ADR-0037). Dosyalar ikiliye gömülür; `koala migrate up` migration'lardan sonra tabloları bu dosyalara eşitler (`Sync`), `koala reference sync --dry-run` ne yapacağını yazar ve hiçbir şeyi değiştirmez.

| Dosya | Tablo | Ebeveyn | Satır |
|---|---|---|---|
| `countries.csv` | `countries` | — | 1 |
| `cities.csv` | `cities` | `country_id` | 81 |
| `districts.csv` | `districts` | `city_id` | 995 |
| `universities.csv` | `universities` | — | 223 |
| `faculties.csv` | `faculties` | `university_id` | 2.326 |
| `departments.csv` | `departments` | `faculty_id` | 11.250 |

Sayılar testle bağlıdır (`TestDatasetCounts`, `TestTemplateHoldsTheReferenceData`); bir dosyayı değiştiren PR bu tabloyu ve testleri birlikte günceller.

## Kurallar

- Biçim RFC 4180 CSV, UTF-8, ilk satır başlıktır. Başlık ve kolon sırası dosyaya göre sabittir: `id,name`; `id,<ebeveyn>_id,name`; ülkelerde `id,code,name`.
- Dosya **kullanımdaki** satırları listeler. Dosyadan düşen satır silinmez, emekliye ayrılır (`retired_at`); geri gelen satır yeniden etkinleşir.
- **Kimlik hiç değişmez.** Adı değişen satır aynı kimlikle kalır. Başka bir ebeveynin altına taşınan satır yeni bir satırdır ve yeni kimlik alır; araç eski kimliği başka ebeveynle görürse reddeder (eğitim kaydı (fakülte, üniversite) çiftini tutar, ADR-0036 Karar 6).
- Yeni satırın kimliği rastgele UUID'dir (v4 ya da v7); başka bir kayıttan türetilmez.
- Ebeveyni dosyada olmayan satır reddedilir: bir üniversiteyi düşürmek fakültelerini ve bölümlerini de düşürmeyi gerektirir.
- Ad 1–200 karakterdir, baştaki ve sondaki boşluk ve kontrol karakteri reddedilir. Aynı ebeveyn altında aynı ad **serbesttir** (kaynakta 5 fakülte ve 17 bölüm böyle; ADR-0037).

## Kaynak

- **Anlık görüntü:** Java `is-koala/koala-backend` `origin/main` 0cbbce69, `src/main/resources/db/changelog/data/*.csv`. Kimlikler Java'nınkilerle aynıdır.
- **Akademik veri** (üniversite, fakülte, bölüm): Yükseköğretim Kurulu'nun (YÖK) kamuya açık lisans programı listelerinden; Java'daki satırların tarihi 22.08.2025. Önlisans programları (meslek yüksekokulları) **yoktur** (ADR-0037, açık soru).
- **Yer verisi** (ülke, il, ilçe): Türkiye'nin 81 ili ve ilçeleri; Java'daki satırların tarihi 23.08.2025. Java'nın kaynağı belgelenmemiş.
- **Java'dan dönüşüm:** `created_at`, `updated_at`, `created_by_id`, `updated_by_id` kolonları atıldı; adlar olduğu gibi alındı. Eminönü ilçesi (`9b43a8a6-f165-460d-80b1-e711a2db5714`) alınmadı: 2008'de Fatih'e katıldı ve Java onu 2026-09-05-04 changelog'unda emekliye ayırdı.
- **Lisans:** Kamu kurumlarının yayımladığı olgusal listelerdir; YÖK'ün bu listeler için açık bir veri lisansı yayımladığı doğrulanamadı (ADR-0037, açık soru). Dosyalar yalnız Koala'nın referans tablolarını doldurmak için tutulur.

## Yıllık güncelleme

1. Yeni YÖK listesini mevcut satırlarla eşleştir: aynı ebeveyn altında aynı ad aynı kimliği korur, adı değişen satır elle eşleştirilir, yeni satır yeni kimlik alır, kapanan program dosyadan çıkar.
2. Dosyaları güncelle, `README`'deki sayı tablosunu ve sayı testlerini aynı PR'da değiştir.
3. Yayında `koala reference sync --dry-run` eklenecek, güncellenecek ve emekliye ayrılacak satır sayılarını yazar; `koala migrate up` uygular.
