# Faz 0: Java authz envanteri

Bu klasörde Java `koala-backend`'deki dağınık yetki kontrollerinin envanteri var (plan §5.6-1, §5.6-2). Kaynak `origin/main` anlık görüntüsüdür. Satır numaraları o görüntüye aittir. Java çalıştırılmadı; envanter kod ve test okunarak çıkarıldı.

| Dosya | İçerik |
|---|---|
| [`authz-envanteri.csv`](authz-envanteri.csv) | Marketplace tarafı, kaynak düzeyi yetki noktaları: **132 satır** |
| [`authz-personel-izinleri.csv`](authz-personel-izinleri.csv) | Personel tarafı: 83 `/admin` ucunun izni ve 18 servis içi dinamik kontrol, **101 satır** |
| [`authz-ciftleri.md`](authz-ciftleri.md) | Satırların `(nesne tipi, eylem)` çiftlerine indirgenmesi (**68 çift**), §5.4 modeline eşleme, uyumsuzluklar ve model önerisi |
| [`../../test/authz/matrix.yaml`](../../test/authz/matrix.yaml) | Matris **taslağı**: 186 satır, her çiftte en az bir izin ve bir ret |
| [`tarama/`](tarama/) | Envanteri üreten betikler |

## Nasıl üretildi

1. **Tarama.** N01 §9.b'deki desenler `business/`, `core/` ve `webAPI/` altında grep'lendi. Bulunan ham eşleşmeler:

   | Desen | Eşleşme |
   |---|---|
   | `Concealment.notFound` | 34 |
   | `new ForbiddenException` | 19 |
   | `participants.partyOf\|isParticipant` / `assignmentParticipants.` | 12 |
   | `jobEditors.` | 7 |
   | `isUserAContactOfCompany\|isUserAdminOfCompany` | 21 |
   | `requireOwned(` | 13 |
   | `requireCompanyAdmin(` | 9 |
   | `requireJobEditor(` | 6 |
   | `requireShuttleEditor(` | 4 |
   | `requireParty(` | 4 |
   | `getId().equals(` | 68 |

   Ek desenler de tarandı: DAO'da `findByIdAnd…User` gibi WHERE ile sahiplik, `assertCanDownload`, STOMP interceptor, `ROLE_ADMIN` baypası.

2. **Elle doğrulama.** Ham eşleşmelerin her biri okundu. Gürültü elendi: bildirim alıcı süzgeçleri, eşleme ve gösterim mantığı, referans verisi tutarlılığı (ülke → il), kimlik doğrulama filtreleri.

   Kalan guard'lar `tarama/build_authz.py` içindeki `ROWS` listesine sınıflandırılarak yazıldı. Her guard satırı alıntılanan koda karşı denetlendi.

3. **Uç eşleme.**
   - `tarama/endpoints.py` 265 uç çıkarıyor (182 marketplace, 83 `/admin`). Her uç için controller'ın çağırdığı manager metotları da çıkıyor.
   - `tarama/resolve.py` guard'ı içeren metottan dosya içi çağıranlara çıkıp public kök metotları buluyor.
   - Bu iki çıktı birleşince `uc` kolonu oluşuyor.

4. **Test bağlama.**
   - `tarama/tests.py` 2 752 test metodunu indeksliyor: HTTP istekleri, beklenen statüler, çağrılan metotlar ve beklenen istisnalar.
   - Aday testler uç ve ret statüsü eşleşmesiyle önerildi.
   - Sonra test gövdeleri okunarak elle seçildi: `test_eslesmesi = elle`, **72 satır**.
   - Bulunamayanlar `bulunamadı` olarak işaretli: **60 satır**. Bunların bir kısmı bilinçli olarak testi olmayan noktalar: tasarım gereği açık uçlar, kimliksiz uçlar ve kontrolü olmayan noktalar.

Yeniden üretmek için (Python 3, bağımlılık yok, Java gerekmez):

```sh
python3 -I tarama/endpoints.py <backend-src> > eps.json
python3 -I tarama/tests.py     <backend-src> > tests.json
python3 -I tarama/build_authz.py <backend-src> eps.json tests.json <çıktı-dizini>
python3 -I tarama/pairs.py <çıktı-dizini>/authz-envanteri.csv ciftler-tablo.md ../../test/authz/matrix.yaml
```

## Satır kuralı

Bir satır, **(guard'ın konumu, korunan public metot)** ikilisidir.

- `requireCompanyAdmin` 8 yerden çağrılır. Bu yüzden 8 satırdır ve her satır kendi ucunu taşır.
- `MessagingManager.loadParticipating` 5 public metodu korur. Bu yüzden aynı konumdan 5 satır çıkar.
- Toplam 132 satır, 112 farklı konumdan ve 39 dosyadan geliyor.

Envantere girenler:

- Tekil kaynak kontrolleri.
- Alt nesnenin ebeveyne aitliği (kapsama). Örnekler: servis plana ait mi, kanıt uyuşmazlığa ait mi.
- Alan düzeyi projeksiyonlar. Bunlarda ret yoktur, alan maskelenir.
- İlişki taşıyan liste kapsamları. Bunlar "kendi" listeleri dışındaki 3 liste.
- Tasarım gereği açık uçlar: 11 satır, ileride `public` ilişkisi olarak modellenecek.
- Kimliksiz uçlar: yetenek URL'si ya da sağlayıcı token'ı ile çalışanlar.
- Kontrolü olmayan noktalar: 4 satır, `desen = YOK`.

Envantere girmeyenler:

- Kimlik doğrulama uçları.
- `/users/me/*` altındaki "kendi listem / kendi kaydım" uçları. Bunlar `Scope(SelfUserID)` olarak çözülür (§5.4, "Modelin dışında kalanlar"). Yaklaşık 47 uç.
- Referans ve herkese açık statik uçlar.
- `POST /companies`, `POST /images` ve şirket takip uçları. Bunlarda kaynak ilişkisi yok.

## Kolonlar (`authz-envanteri.csv`)

| Kolon | Anlam |
|---|---|
| `id` | `AZ-nnn` |
| `konum` | `dosya:satır` (guard kararının satırı) |
| `korunan_metot` | `Manager.publicMetot` |
| `desen` | Guard ya da desen (ör. `requireCompanyAdmin`, `AssignmentParticipants.partyOf`, `DAO WHERE`, `kapsama`, `YOK`) |
| `nesne_tipi`, `eylem` | Karar çifti (tekilleştirilmiş, `authz-ciftleri.md`) |
| `alt_eylem` | Uca özgü eylem |
| `gizleme_404` | `evet` = `Concealment.notFound` · `evet-yardımcısız` = var olmayanla aynı 404 mesajı, yardımcı yok · `kısmi` = 404 ama mesaj farklı (ayırt edilebilir) · `hayır` = 403/400/409 · `sessiz` = 200, iş yok · `yok` = ret yok |
| `hata_kodu` | HTTP statüsü ve mesaj sabiti |
| `java_testi` | Bağlayan test (`Dosya.java:satır#test`) |
| `test_eslesmesi` | `elle` / `bulunamadı` |
| `uc` | HTTP metot + yol (STOMP için `STOMP SUBSCRIBE/SEND …`) |
| `not` | Bulgu ya da açıklama |

## Personel tarafı (`authz-personel-izinleri.csv`)

- 79 uç `@RequiresPermission` ile korunuyor, 4'ü kimlik doğrulama ucu.
- Birden çok izin yazılmışsa hepsi gerekir (`containsAll`). Boş izin "kayıtlı personel yeter" demektir. Annotation yoksa istek fail-closed 403 alır.
- 79 ucun **47'sinde** ret (403) bekleyen bir Java testi bulundu, 32'sinde bulunamadı. Bu eşleşme otomatik yapıldı: uç ve 403 statüsü. Elle doğrulanmadı.
- 18 servis içi dinamik kontrol var:
  - `user.read_pii` projeksiyonu
  - uyuşmazlık sonucuna göre para izni
  - not yazarı
  - verilebilirlik (`requireGrantable`)
  - `StaffPayoutManager`'da ikinci kat kontrol
  - köprü (`ROLE_ADMIN`)

## Doğrulanmamış noktalar

- **AZ-077.** STOMP SEND ile `/topic/conversations/{id}`'ye yayın gerçekten abonelere ulaşıyor mu? Simple broker davranışı canlı denenmedi.
- **AZ-121.** Onaylanmamış şirketin detayı herkese açık görünüyor. Onay durumu süzgeci bulunamadı ama tam taranmadı.
- **AZ-131, AZ-132.** Ödeme callback'inin uydurma ya da bilinmeyen token'la davranışı matriste "doğrulanmadı" olarak duruyor.
- **Personel CSV'sinde ret testi eşleşmesi otomatik.** 47 satırın testleri elle okunmadı.
- **`bulunamadı` olan 60 satır.** Testin gerçekten olmadığı kesin değil. Tarama yöntemi, yolu değişkenle kuran testleri kaçırır.
