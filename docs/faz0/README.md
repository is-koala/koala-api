# Faz 0: Java authz envanteri

Bu klasörde Java `koala-backend`'deki dağınık yetki kontrollerinin envanteri var (plan §5.6-1, §5.6-2; ADR-0014). Kaynak `origin/main` anlık görüntüsüdür. Satır numaraları o görüntüye aittir. Java çalıştırılmadı; envanter kod ve test okunarak çıkarıldı.

| Dosya | İçerik |
|---|---|
| [`authz-envanteri.csv`](authz-envanteri.csv) | Pazaryeri tarafı, kaynak düzeyi yetki noktaları: **134 satır** (`AZ-nnn`) |
| [`authz-personel-izinleri.csv`](authz-personel-izinleri.csv) | Personel tarafı: 83 `/admin` ucunun izni ve 18 servis içi dinamik kontrol, **101 satır** (`PI-nnn`) |
| [`authz-ciftleri.md`](authz-ciftleri.md) | Satırların `(nesne tipi, eylem)` çiftlerine indirgenmesi (**71 çift**), §5.4 modeline eşleme, ürün kararları D1–D5, önerilen DSL |
| [`../../internal/authz/testdata/matrix.yaml`](../../internal/authz/testdata/matrix.yaml) | Matris **taslağı**: 214 satır; fikstürler, tuple'lar, `actions:` haritası, `basis` sözlüğü, `java` / `expect` ayrımı, `engine` |
| [`tarama/`](tarama/) | Envanteri, matrisi ve oracle tablolarını üreten betikler |

İlgili teslimler: senaryo kataloğu [`test/scenarios/`](../../test/scenarios/), oracle [`testdata/oracle/`](../../testdata/oracle/), Iyzico vektörleri [`testdata/iyzico/`](../../testdata/iyzico/).

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
   - Sonra test gövdeleri okunarak elle seçildi: `test_eslesmesi = elle`, **73 satır**.
   - Bulunamayanlar `bulunamadı` olarak işaretli: **61 satır**. Bunların bir kısmı bilinçli olarak testi olmayan noktalar: tasarım gereği açık uçlar, kimliksiz uçlar ve kontrolü olmayan noktalar.

Yeniden üretmek için (Python 3, bağımlılık yok, Java gerekmez). **Bütün betikler depo kökünden çalışır**; `<backend-src>` Java deposunun yerel kopyasıdır, ara dosyalar için geçici bir dizin kullanın:

```sh
python3 -I docs/faz0/tarama/endpoints.py <backend-src> > /tmp/eps.json
python3 -I docs/faz0/tarama/tests.py     <backend-src> > /tmp/tests.json
python3 -I docs/faz0/tarama/build_authz.py <backend-src> /tmp/eps.json /tmp/tests.json   # → docs/faz0/*.csv
python3 -I docs/faz0/tarama/pairs.py                                                     # → authz-ciftleri.md tablosu + matrix.yaml
python3 -I docs/faz0/tarama/oracle_uret.py                                               # → testdata/oracle/*.csv
```

`pairs.py` tek başına `make generate`'in bir adımıdır ve çıktısı `make
gen-check`'te denetlenir (ADR-0028): matris bayat kalırsa kapı kırmızıdır.
Öteki betikler Java deposunu ister, kapıda koşmazlar.

## Satır kuralı

Bir satır, **(guard'ın konumu, korunan public metot)** ikilisidir.

- `requireCompanyAdmin` 8 yerden çağrılır. Bu yüzden 8 satırdır ve her satır kendi ucunu taşır.
- `MessagingManager.loadParticipating` 5 public metodu korur. Bu yüzden aynı konumdan 5 satır çıkar.
- `requireOwned` gibi özel yardımcılarda `konum` karar satırıdır (`ExperienceManager.java:79`); çağrı satırı `desen`'de yazılıdır.
- Toplam 134 satır, 109 farklı konumdan ve 40 dosyadan geliyor.

Envantere girenler:

- Tekil kaynak kontrolleri.
- Alt nesnenin ebeveyne aitliği (kapsama). Örnekler: servis plana ait mi, kanıt uyuşmazlığa ait mi.
- Alan düzeyi projeksiyonlar. Bunlarda ret yoktur, alan maskelenir (ör. ulaşımda plaka/şoför, `TransportManager.assignmentView`).
- URL düzeyi rol kuralı: `/actuator/**` pazaryeri `ROLE_ADMIN` (D5 ile kalkar).
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
| `java_testi` | Bağlayan test (`src/test/java/…/Dosya.java#test`; biçim senaryo ve oracle ile aynı) |
| `test_eslesmesi` | `elle` / `bulunamadı` |
| `uc` | HTTP metot + yol (STOMP için `STOMP SUBSCRIBE/SEND …`) |
| `not` | Bulgu ya da açıklama |

## Personel tarafı (`authz-personel-izinleri.csv`)

**Uç izinleri matristedir (ADR-0026).** Personel tarafı uç başına izin tablosudur (RBAC + rol DAG'ı, §5.5). 25 izin `platform:koala`'nın relation'larıdır; `pairs.py` Java seed'indeki 10 rolü ve DAG'ı fikstür tuple'ı olarak yazar ve her izin için doğrudan sahibi, OWNER'ı (kapsamayla), karşı daldan bir personeli ve rolsüz personeli satır olarak üretir (çift `S01`–`S26`, 98 satır, hepsi `[go, openfga]`). Aynı satırlar veritabanında da koşar (`internal/authz/authzsql` `TestStaffMatrixOnTheDatabase`). `/v1/admin/**` operasyonlarının `x-authz-permission` eşlemesi açılışta denetlenir (`staff.NewGate`). Servis içi dinamik kontrollerin (PI-084–101) 14'ü (PI-088–101) matris satırlarının `inventory` etiketlerindedir; geri kalan dördü (PI-084–087) ve eşlenmeyen beş kimlik/self-servis satırı (PI-058–062) matriste değildir, karşılıkları aşağıdaki eşleme tablosundadır. Kaynağı bu CSV'dir; satır kimlikleri `PI-nnn` (oracle'daki `ST-` settlement görevi satırlarıyla çakışmasın diye).

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

**Matriste eşlenmeyen dokuz satır (ADR-0026).** `pairs.py` 101 satırın 92'sini `inventory` etiketinde anar. Kalan dokuzu izin sahipliği sorusu değildir (kimlik doğrulama ucu ya da kapı davranışı); karşılıkları şunlardır:

| Satır | Karşılığı |
|---|---|
| PI-058, PI-059 (`POST /admin/auth/logout`, `…/password`) | AUTH-013 (çıkış ve parola değişimi token sürümünü artırır) |
| PI-060, PI-061 (`POST /admin/auth/login`, `…/refresh`) | AUTH-010 (ayırt edilemez ret), AUTH-011 (kilit), AUTH-018 (oran sınırı); ikisi de kimlik ucudur, izin istemez |
| PI-062 (`GET /admin/me`, izin listesi boş) | Self-servis muafiyeti; kapıda kodla sabittir (`staff.SelfService`), AUTH-012 izinleri DB'den okur |
| PI-084 (eşleşmeyen `/admin/**` fail-closed 403) | ADR-0026 Karar 4 ile 404 `ROUTE_NOT_FOUND`; AUTH-015 `target`'ı ve `go_test`'i (`unmapped staff path`) |
| PI-085 (kayıtsız personel ya da `mustChangePassword` → ret) | AUTH-014 (`TestStaffMustChangePassword`, `TestStaffMustChangeFlag`) |
| PI-086 (köprü: pazaryeri `ROLE_ADMIN`) | D5: köprü yok; AUTH-015 (`TestTokenClassesOverHTTP`) |
| PI-087 (`GET /admin/me`: kayıtlı personel mi) | `getStaffMe` işleyicisi; kayıtsız ya da kapalı personel authn'de reddedilir (ADR-0025 §6) |

**Verilebilirlik (PI-096–101) ölçülmüyor.** Bu altı satır `inventory` etiketlerinde görünür, ama matris yalnız `role.manage`/`staff.manage` sahipliğini ölçer; "verenin kendisinde olmayan izni verememesi" (`requireGrantable`) kuralının kendisi sınanmaz. Rol ve izin verme uçları Faz 8'in personel yönetimiyle gelir; kural AUTH-019 senaryosunda `deferred: phase 8` olarak izlenir.

## Doğrulanmamış noktalar

- **AZ-077 (öncelik yüksek).** STOMP SEND ile `/topic/conversations/{id}`'ye yayın gerçekten abonelere ulaşıyor mu? Simple broker davranışı canlı denenmedi. Go'da K4 sunucu-itmeli protokolle kapanır.
- **AZ-123.** Onaylanmamış şirketin detayı herkese açık görünüyor. Onay durumu süzgeci bulunamadı ama tam taranmadı.
- **AZ-133, AZ-134.** Ödeme callback'inin bilinmeyen token'la davranışı matriste `skip` motoruyla duruyor; oracle `reconcile.csv` RC-01 bağlar.
- **Personel CSV'sinde ret testi eşleşmesi otomatik.** 47 satırın testleri elle okunmadı.
- **`bulunamadı` olan 61 satır.** Testin gerçekten olmadığı kesin değil. Tarama yöntemi, yolu değişkenle kuran testleri kaçırır.
