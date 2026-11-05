# ADR-0024: Authz çekirdeği: yorumlayıcı değerlendirici, model.json ve OpenFGA uygunluk koşucusu

- Durum: Önerildi
- Tarih: 05.11.2026
- Karar kaydı: K17 (ADR-0014'ün uygulaması)
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §5.2–5.7, §6.1 Faz 1; [ADR-0014](0014-authorization.md); [Faz 0 çiftleri](../faz0/authz-ciftleri.md); sektör araştırması (N07) §6–8

## Bağlam

ADR-0014 kararı verdi: uygulama içinde tek karar noktası, kurallar OpenFGA DSL alt kümesinde, uygunluk matrisi iki motorda. Faz 1'in çıkış kriteri: "Uygunluk koşucusu OpenFGA konteyneriyle en az 1 tipte yeşil". Uygulamaya geçerken ADR-0014'ün açık bıraktığı beş soru var:

1. `model.fga`'dan Go'ya nasıl geçilecek: yorumlayıcı mı, kod üretimi mi?
2. `openfga/language` yalnız test/CI için onaylı (ADR-0003). Çalışan servis modeli neyle okuyacak?
3. "Sınırlı `and` / `but not`" ne demek, nasıl zorlanacak?
4. `Decision.Conceal` neye göre hesaplanacak?
5. Koşucu CI'da nerede ve hangi etiketle koşacak?

## Karar

### 1. Yorumlayıcı, kod üretimi değil

`Evaluator`, modeli çalışma anında yorumlar: doğrudan tuple, computed userset, tuple-to-userset, union, intersection, exclusion. Çözüm derinliği sınırı OpenFGA'nınkiyle aynıdır (25). Yol üzerindeki döngü izin vermez.

Gerekçe:
- **Sektör pratiği.** OpenFGA, SpiceDB ve Zanzibar modeli yorumlar; kural kümesi veri, değerlendirici geneldir. Kod üretimi (Meta PrivacyLib) kuralları dile gömmek için kullanılır. Bizim kurallarımız zaten OpenFGA'nın JSON'unda veri olarak duruyor.
- **Bakım.** Model değişikliği tek dosyada (`model.fga`) ve onun JSON'unda kalır. Şablon, üretici ve üretilmiş Go kodu yoktur. İncelemede okunan şey DSL'dir.
- **Uygunluk iddiası.** Servisin yorumladığı dosya, OpenFGA'ya yüklenen dosyanın ta kendisidir (`model.json`). İki motor aynı girdiyi görür.
- **Maliyet.** Matris boyutunda ilişki zincirleri kısadır (en derin yol 6 adım). Yorumlama maliyeti, tuple okumasının (Faz 2'de SQL) yanında ihmal edilebilir.

Kod üretimi, Scope'un SQL'e çevrilmesi için ayrıca değerlendirilecek (Faz 2–3). O soru Check'ten bağımsızdır.

### 2. `model.fga` kaynak, `model.json` resmi ayrıştırıcının çıktısı

- `internal/authz/model.fga` tek doğruluk kaynağıdır.
- `internal/authz/model.json`, `openfga/language`'in (`transformer.TransformDSLToJSON`) `model.fga`'dan ürettiği OpenFGA JSON'udur. Servis bunu `//go:embed` ile gömer ve stdlib `encoding/json` ile okur. Böylece `openfga/language` servis ikilisine girmez; yalnız testler onu import eder.
- `TestModelJSONMatchesDSL` iki dosyanın aynı modeli taşıdığını doğrular; farklıysa resmi çıktıyı log'a basar. Güncelleme: `go test ./internal/authz -run TestModelJSONMatchesDSL -update`.
- Kendi DSL ayrıştırıcımız yazılmadı: tek ayrıştırıcı resmi olandır.

### 3. Alt küme ve "sınırlı `and` / `but not`"

`LoadModel` OpenFGA'nın model yazımındaki denetimleri ve alt küme kurallarını uygular. Model yüklenemezse servis açılmaz.
- Şema 1.1. Modül, `extend` ve bilinmeyen JSON alanı reddedilir.
- `with condition` alt kümededir, ama yerel değerlendirici koşulları henüz değerlendirmiyor: koşul taşıyan model yüklenmez. CEL bağımlılığı ayrı bir onay ister.
- `and` ve `but not` yalnız ilişkinin en üstünde, yalın işlenenlerle (doğrudan, computed, `from`) kullanılır. İç içe küme işlemi reddedilir.
- Bunları kullanan ilişkiler `model_test.go`'daki açık listededir: bugün `job#can_apply` ve `message#can_report`. Yeni bir kullanım, listenin incelemeden geçen değişikliğidir. Gerekçe: dış motorda liste sorgularını pahalılaştırırlar (N07 §6.2-2).
- Alt küme denetimi `openfga/language`'in kendi ayrıştırmasının üzerinde koşar (`TestModelSubset`).

### 4. Tuple kaynağı arayüzdür

`TupleReader` iki metottur: `Contains` (tam tuple var mı; SQL'de `EXISTS`) ve `Read` (`object#relation`'ın kullanıcıları). Bunlar OpenFGA'nın depolama okumalarının karşılığıdır (`ReadUserTuple`, `Read`).
- Bu PR'da yalnız bellek içi kaynak var (`authz/authztest`). Tuple'ları yüklerken OpenFGA'nın yazım denetimini uygular.
- Domain tablolarına giden tipli SQL kaynakları Faz 2–3'te gelir ve çağıranın tx'inde okur. `authz_tuples` VIEW'ı sıcak yolda kullanılmaz (ADR-0014, §5.2-1).
- Değerlendirici, ilişkinin tip kısıtına uymayan tuple'ı yok sayar (OpenFGA gibi). Eski bir modelden kalan tuple yetki vermez.

### 5. Gizleme politikası

Ret varsayılan olarak gizlenir: kaynağın kendi 404'ü. Nesneyi zaten görebilen özneye karşı verilen retlerde (başvuru, sohbet açma, şirket yönetimi, presence, mesaj raporu) gizleme yalnız özne nesneyi göremiyorsa yapılır. Görünürlüğü belirleyen ilişki politikada yazılıdır (`conceal.go`).

Politika matrisin `conceal` kolonuna karşı test edilir. Dört satır istisnadır ve gerekçeleri testte yazılıdır: üçü alan düzeyi projeksiyon (çağıran reddetmez), biri WS abonelik reddi (soket üzerinde 404 yok).

### 6. Uygunluk koşucusu

- **Go tarafı** (`matrix_test.go`, etiketsiz, `make verify`): `[go, openfga]` satırlarının hepsi koşar. Karar, `Basis` ve `Conceal` doğrulanır. `Basis`'te iki satır istisnadır, gerekçeleri testte yazılıdır.
- **OpenFGA tarafı** (`openfga_conformance_test.go`, `//go:build conformance`): `model.json` ve matris tuple'ları gerçek OpenFGA'ya yüklenir. Aynı satırlar iki motorda koşar.
  - İki motor farklıysa test kırmızıdır.
  - Bir motor matrisin beklentisinden farklıysa da test kırmızıdır.
  - Tip başına yeşil/kırmızı tablosu ve koşmayan satırların gerekçeli listesi log'a yazılır.
- **CI:** `verify.yml`'de ayrı `authz-conformance` job'u var.
  - `openfga/openfga` service container olarak çalışır. Digest'le sabitlenmiştir: v1.22.0, 14 gün kuralına uyan en yeni sürüm. Bellek içi depo kullanılır, playground kapalıdır, port yalnız loopback'e açıktır.
  - Koşu `make test-conformance` ile yapılır. Adres `FGA_API_URL`'den gelir (OpenFGA CLI'nin adı).
  - Postgres gerekmez; verify job'uyla paralel koşar.
- **Etiket:** `integration` değil, `conformance`. `make test-integration`, etiketli her testi Postgres'e karşı koşar; orada OpenFGA yok. `scripts/test-integration.sh`'nin etiket koruması `*_conformance_test.go` dosyalarını tanıyacak şekilde genişletildi. golangci-lint iki etiketle koşar.
- OpenFGA'ya stdlib HTTP istemcisiyle bağlanılır; `openfga/go-sdk` onaylı listede değil.
- Kimlikler: `Subject.ID` UUID'dir (§5.3). Matrisin sembolik kullanıcı adları iki motor için de aynı deterministik UUID'lere çevrilir (SHA-1 ad alanı). Diğer nesnelerin kimlikleri olduğu gibi kalır.

### 7. Geçiş iskeleti

- `RelationSink` ve `RelationChange`: bir ilişkiyi değiştiren her domain yazımı, aynı tx'te tek bir olay üretir (§5.2-5). İlk uygulama ilk ilişki tablosuyla gelir.
- `ShadowChecker` dark mode'dur (§5.7-2). Kararı birincil verir, gölgeye aynı soruyu sorar. Farkı ya da gölge hatasını `MismatchCounter`'a ve log'a yazar.
- `koala_authz_mismatch_total` sayacı `MismatchCounter` arayüzünün arkasındadır. Prometheus uygulaması metrik paketiyle (paralel PR) gelir.
- `AtLeastAsFresh` bugün no-op'tur: yerel okuma çağıranın tx'indedir, kendi yazdığını zaten görür.

### 8. Arayüzler tüketici tarafında

`Checker`, `Scoper`, `CapabilityReader` ve `RelationSink` §5.3'ün sözleşmesidir. Servisler bunları bağımlılık tipi olarak import etmez; ihtiyaç duydukları tek metodu kendi paketlerinde tanımlar ve `*Evaluator` ile bağlanır (CLAUDE.md "Arayüz kullanan tarafta tanımlanır"). `Scoper` ve `CapabilityReader`'ın uygulaması domain tablolarıyla gelir (Faz 2).

## Faz 0 önerisinden sapmalar (`model.fga`)

| Yer | Faz 0 önerisi | Bu PR | Gerekçe |
|---|---|---|---|
| `message` | `can_report: participant from conversation but not sender` | `can_read: participant from conversation`, `can_report: can_read but not sender` | Aynı karar. `can_read` gizleme politikasının görünürlük ilişkisi (C39: gönderen 403, yabancı 404). |
| `platform` | `user_read_pii` + "… §5.4'teki gibi" | 25 izinin hepsi, `[staff_role#assignee]` | Personel CSV'sindeki 25 izin, nokta yerine alt çizgiyle (§5.4). |
| Yorumlar | Satır sonu `#` yorumları | Yorumlar kendi satırında, `define`/`type` öncesinde | OpenFGA dilbilgisi yorumu yalnız bu konumlarda kabul ediyor. |

Matris: `company:co_1#viewer@user:*` ve `company:co_2#viewer@user:*` tuple'ları eksikti. Bu yüzden AZM-092 (C24, `company.read`) iki motorda da yanlış sonuç veriyordu. Tuple'lar `pairs.py`'ye eklendi ve matris yeniden üretildi; diğer satırlar değişmedi.

## Sonuçlar

- Çıkış kriteri "≥1 tip yeşil" yerine hedef, bütün `[go, openfga]` satırlarıdır (152 satır, 12 tip; modeldeki personel tipleri `staff`, `staff_role` ve `platform` pazaryeri matrisinin kapsamı dışında). `go_only` (55) ve `skip` (6) satırları koşmaz. Gerekçeleri koşucunun raporunda satır satır yazılır: anonim özne, Scope, model dışı Go kuralı, ön süzgeç.
- `go_only` satırlarının ön süzgeçleri (`authz/prefilters.go`) ve Scope, domain tablolarıyla gelir.
- YAML kütüphanesi onaylı listede yok. Matris, `internal/authz/internal/matrix`'teki katı bir alt küme okuyucusuyla okunur: tanımadığı yapıda hata verir. `gopkg.in/yaml.v3` zaten `openfga/language` üzerinden dolaylı bağımlılık; onaylanırsa okuyucu kaldırılır.
- `openfga/language` v0.3.2, yalnız testlerde import edilir. Dolaylı bağımlılıkları (antlr, protobuf, grpc-gateway, grpc) `go.mod`'a girer ama servis ikilisine girmez.
