# Referans verisi

M4 referans tablolarının satırları (ADR-0029 Karar 8, ADR-0036 Karar 7, ADR-0037). Dosyalar ikiliye gömülür; `koala migrate up` migration'lardan sonra tabloları bu dosyalara eşitler (`Sync`), `koala reference sync --dry-run` yazacağı her satırı gösterir ve hiçbir şeyi değiştirmez. Bu düzen ADR-0012 Karar 3'ün `0002_seed.sql`'inin yerine geçer.

| Dosya | Tablo | Ebeveyn | Satır |
|---|---|---|---|
| `countries.csv` | `countries` | — | 1 |
| `cities.csv` | `cities` | `country_id` | 81 |
| `districts.csv` | `districts` | `city_id` | 973 |
| `universities.csv` | `universities` | — | 223 |
| `faculties.csv` | `faculties` | `university_id` | 2.043 |
| `departments.csv` | `departments` | `faculty_id` | 7.567 |
| `VERSION` | `reference_data_version` | — | sürüm 1 |

Sayılar testle bağlıdır (`TestDatasetCounts`, `TestTemplateHoldsTheReferenceData`, `TestMigrateCommand`); bir dosyayı değiştiren PR bu tabloyu, `VERSION`'ı ve testleri birlikte günceller.

## Kurallar

- Biçim RFC 4180 CSV, UTF-8, ilk satır başlıktır. Başlık ve kolon sırası dosyaya göre sabittir: `id,name`; `id,<ebeveyn>_id,name`; ülkelerde `id,code,name`.
- Dosya **kullanımdaki** satırları listeler. Dosyadan düşen satır silinmez, emekliye ayrılır (`retired_at`); geri gelen satır yeniden etkinleşir.
- **`VERSION` her değişiklikte artar.** Veritabanında daha yeni bir sürüm varsa (eski imaja geri alınmış bir yayın) eşitleme hiçbir şey yazmaz ve bir uyarı basar; veriyi geri götürmez. Aynı sürüm yeniden eşitlenebilir (elle yapılmış bir sapmayı onarır).
- **Kimlik hiç değişmez.** Adı değişen satır aynı kimlikle kalır. Başka bir ebeveynin altına taşınan satır yeni bir satırdır ve yeni kimlik alır; araç eski kimliği başka ebeveynle görürse reddeder (eğitim kaydı (fakülte, üniversite) çiftini tutar, ADR-0036 Karar 6). İlk yükleme bunun tek istisnasıydı: hiçbir veritabanı satırları tutmuyordu (aşağıda İYTE ve fakülte birleştirmesi).
- Yeni satırın kimliği UUIDv7'dir (ADR-0012 Karar 5); başka bir kayıttan türetilmez.
- Ebeveyni dosyada olmayan satır reddedilir: bir üniversiteyi düşürmek fakültelerini ve bölümlerini de düşürmeyi gerektirir.
- Ad 1–200 karakterdir; baştaki ve sondaki boşluk, kontrol karakteri ve görünmez biçim karakteri (Cf: U+200B, U+202E, U+FEFF…) reddedilir. Aynı ebeveyn altında aynı ad kısıtla engellenmez; temizlenmiş veride yoktur.
- **Emeklilik sınırı:** bir tablonun kullanımdaki satırlarının %5'inden (en az 10) fazlasını emekliye ayıracak eşitleme hiçbir şey yazmadan durur. Yalnız başlık satırı kalmış bir dosya böyle yakalanır. Bilinçli bir büyük emeklilik `--allow-retire=<tablo>:<n>@<sürüm>` ya da `KOALA_REFERENCE_ALLOW_RETIRE` ister; izin yalnız adlandırdığı veri sürümüne eşitlemede geçerlidir (ADR-0037 Karar 3a).

## Kaynak

- **Anlık görüntü:** Java `is-koala/koala-backend` `origin/main` 0cbbce69, `src/main/resources/db/changelog/data/*.csv`. Kimlikler Java'nınkilerle aynıdır.
- **Akademik veri** (üniversite, fakülte, bölüm): Yükseköğretim Kurulu'nun (YÖK) kamuya açık lisans programı listelerinden; Java'daki satırların tarihi 22.08.2025. Önlisans programları (meslek yüksekokulları) **yoktur**; ayrı bir PR'da eklenecek (Yusuf onayladı, 23.11.2026).
- **Yer verisi** (ülke, il, ilçe): Türkiye'nin 81 ili ve ilçeleri; Java'daki satırların tarihi 23.08.2025. Java'nın kaynağı belgelenmemiş.
- **Lisans:** Kamu kurumlarının yayımladığı olgusal listelerdir; YÖK'ün bu listeler için açık bir veri lisansı yayımladığı doğrulanamadı (ADR-0037, açık soru). Dosyalar yalnız Koala'nın referans tablolarını doldurmak için tutulur.

## Dönüşüm ve temizlik (ADR-0037 Karar 4a)

Dosyalar iki betikle Java'nın anlık görüntüsünden bayt bayt yeniden üretilir; ikisi de yalnız standart kütüphane kullanır:

```sh
python3 -I db/referencedata/tools/from_java.py <java-csv-dizini> db/referencedata
python3 -I db/referencedata/tools/clean.py db/referencedata
```

`from_java.py` Java'nın dört denetim kolonunu (`created_at`, `updated_at`, `created_by_id`, `updated_by_id`) atar ve Eminönü'nü (`9b43a8a6-f165-460d-80b1-e711a2db5714`; 2008'de Fatih'e katıldı, Java 2026-09-05-04 changelog'unda emekliye ayırdı) almaz. `clean.py` idempotenttir: temizlenmiş dosyalarda koşunca her kuralın sayısı 0'dır. Kurallar ve 23.11.2026'daki etkileri:

| # | Kural | Etki |
|---|---|---|
| 1 | NBSP → boşluk, ardışık boşluk → tek, uçlar kırpılır | 61 ad |
| 2 | 30 büyükşehirdeki "MERKEZ" ilçesi çıkar (6360 sayılı Kanun; büyükşehir olmayan 51 ilin MERKEZ'i kalır) | −23 ilçe |
| 2b | DERECİK (Hakkari) eklenir: 2018'de Şemdinli'den ayrıldı, Java verisinde yok; kimliği tek seferlik bir UUIDv7 (`01a2077d-d01b-7bb5-8d9b-9c94173f5c34`). Bununla her ilin ilçe sayısı resmî sayıyla aynıdır | +1 ilçe |
| 3 | YÖK'ün yurt dışı üniversite notu fakülte satırı olarak çıkar (Hoca Ahmet Yesevi, Manas) | −2 fakülte |
| 4 | İzmir Yüksek Teknoloji Enstitüsü, İzmir Katip Çelebi'nin fakültesi değil üniversitedir; `universities.csv`'ye **aynı kimlikle** (`f458e88f-1201-4231-b4f7-7ede110e9c05`) taşındı. YÖK listesinde hemen ardından gelen Fen, Mimarlık ve Mühendislik fakülteleri (18 bölüm: Fotonik, Biyomühendislik, Şehir ve Bölge Planlama…) de İYTE'nindir ve kimlikleriyle İYTE'ye taşındı. İlk yükleme olduğu için mümkündü: taşıma reddi yalnız satırı zaten tutan bir veritabanında geçerlidir | −1 fakülte, +1 üniversite, 3 fakülte taşındı |
| 4b | "İÇİŞLERİ BAKANLIĞI VE MİLLİ SAVUNMA BAKANLIĞI ADINA SAĞLIK BİLİMLERİ ÜNİVERSİTESİNDE EĞİTİM ALACAKLAR" bir kontenjan notudur; altındaki her fakülte ve bölüm, ek ve yerleşke parantezi atılınca Sağlık Bilimleri Üniversitesi'nin bir birimidir. Kopya oldukları için çıkarıldı (taşımak kopya üretirdi); betik karşılıklarını denetler | −1 üniversite, −6 fakülte, −9 bölüm |
| 5 | Bölümü olan bir kardeşiyle aynı adı taşıyan bölümsüz fakülte çıkar | −5 fakülte |
| 6 | Fakültelerde kontenjan ve ücret eki ("(Ücretli)", "(Burslu)", "(%50 İndirimli)", "(İÖ)", …; liste `clean.py`'de `QUOTA`): her (üniversite, eksiz ad) için tek fakülte kalır (eksiz ad, sonra bölümü olan, sonra en küçük kimlik), diğerlerinin bölümleri ona taşınır (ilk yükleme) | −269 fakülte, 167 ad, 283 bölüm taşındı |
| 7 | Bölümlerde aynı ekler: her (fakülte, eksiz ad) için tek bölüm kalır (eksiz satır, yoksa en küçük kimlik) | −3.674 bölüm, 2.357 ad |

Öğretim dili ("(İngilizce)"), uzaktan öğretim ve açıköğretim, ortak lisans programları ("(UOLP-…)") ve yerleşke ("(Kahta)") programın parçası olarak kalır: YÖK onları ayrı program olarak listeler. İkinci öğretim (İÖ) ise aynı programın akşam öğretimidir ve ek sayılır.

## Yıllık güncelleme

1. Yeni YÖK listesini mevcut satırlarla eşleştir: aynı ebeveyn altında aynı ad aynı kimliği korur, yeni satır yeni kimlik (UUIDv7) alır, kapanan program dosyadan çıkar. **Bu kural tek başına yetmez:** bir ad değiştiyse (yeni adla eski satır), bir program başka fakülteye geçtiyse ya da aynı ebeveyn altında aynı adı taşıyan iki satır oluştuysa eşleştirme belirsizdir; bu gruplar elle, kimlik kimlik eşleştirilir ve PR'da listelenir. Kararlı bir eşleştirme YÖK program kodunu ister (ADR-0037, açık soru).
2. Kontenjan ekleri ve benzeri yeni bozukluklar için `clean.py`'yi koş (gerekirse kuralı genişlet).
3. `VERSION`'ı artır ve `referencedata_test.go`'daki `published()`'a yeni sürümün özet satırını ekle (eski satırlara dokunma; `TestVersionNamesTheData` doğru özeti hata iletisinde yazar); README'deki sayı tablosunu ve sayı testlerini aynı PR'da değiştir.
4. Yayından önce `koala reference sync --dry-run` eklenecek, yeniden adlandırılacak, geri gelecek ve emekliye ayrılacak satırları kimlik ve adla yazar. Emeklilik sınırı aşılıyorsa ve bu bilinçliyse yayında `KOALA_REFERENCE_ALLOW_RETIRE=<tablo>:<n>@<yeni sürüm>` verilir; `koala migrate up` uygular. İzin yalnız o sürüme eşitlemede geçerlidir; sonraki yayında unutulursa yok sayılır ve uyarı basılır.
