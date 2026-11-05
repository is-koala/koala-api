# ADR-0024: Authz çekirdeği: yorumlayıcı değerlendirici, model.json ve OpenFGA uygunluk koşucusu

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: ADR-0014'ün uygulaması; şirket rolleri K17.
  - Atıf notu: plan §5'in başlığındaki "karar 5" hiçbir karar kaydı satırına karşılık gelmiyor. K5 para kararıdır (ADR-0008); §0.1 tablosunda da 5. satır para tipidir.
  - Authz tasarımının dayanağı ADR-0014 ve plan §5.1'deki koşuldur ("sektörle tam uyumlu, geçiş zor olmasın").
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §5.2–5.7, §6.1 Faz 1; [ADR-0014](0014-authorization.md); [Faz 0 çiftleri](../faz0/authz-ciftleri.md); sektör araştırması N07 §6–8
- Yerine geçtiği:
  - ADR-0014 Karar 2'deki alt kümeden `with condition` (Karar 3).
  - ADR-0017'deki "Kapı iki adımlıdır … `verify` işinin adımıdır ve PR'ın tek gerekli kontrolü olarak kalır" kalemi. Kapı artık dört job'dur ve hepsi gerekli kontroldür (Sonuçlar).

## Bağlam

ADR-0014 kararı verdi: uygulama içinde tek karar noktası, kurallar OpenFGA DSL alt kümesinde, uygunluk matrisi iki motorda. Faz 1'in çıkış kriteri: "Uygunluk koşucusu OpenFGA konteyneriyle en az 1 tipte yeşil".

ADR-0014'ün açık bıraktığı sorular:
1. `model.fga`'dan Go'ya nasıl geçilecek: yorumlayıcı mı, kod üretimi mi?
2. `openfga/language` yalnız test/CI için onaylı (ADR-0003). Servis modeli neyle okuyacak?
3. Alt küme nasıl sınırlanacak ve zorlanacak?
4. `Decision`'ın `Basis` ve `Conceal` alanları neye göre hesaplanacak?
5. Koşucu CI'da nerede ve hangi etiketle koşacak?

## Karar

### 1. Yorumlayıcı, kod üretimi değil; sabitler üretilir

`Evaluator`, modeli çalışma anında yorumlar: doğrudan tuple, computed userset, tuple-to-userset, union, intersection, exclusion.

**N07 §6.2-1'den sapma.** Araştırma, `.fga`'nın derleme zamanında ayrıştırılıp (a) Check değerlendiricisinin ve (b) Scope SQL'inin kod üretimiyle çıkarılmasını önerdi (Meta PrivacyLib emsali). Check için bu öneri uygulanmadı. Gerekçe:
- **Sektör pratiği.** OpenFGA, SpiceDB ve Zanzibar modeli yorumlar: kural kümesi veridir, değerlendirici geneldir. Kod üretimi (PrivacyLib), kuralları dile gömen sistemlerde kullanılır. Bizim kurallarımız zaten OpenFGA JSON'unda veri olarak duruyor.
- **Bakım.** Şablon, üretici ve üretilmiş mantık kodu yoktur. Model değişikliği tek dosyada (`model.fga`) ve onun JSON'unda kalır; incelemede okunan şey DSL'dir.
- **Uygunluk iddiası.** Servisin yorumladığı dosya, OpenFGA'ya yüklenen dosyanın ta kendisidir (`model.json`). İki motor aynı girdiyi görür.
- **Maliyet.** Matristeki en derin yol 6 adımdır. Faz 2'de yorumlama maliyeti SQL okumasının yanında ihmal edilir.

Scope'un SQL'e çevrilmesi (N07'nin (b) kalemi) ayrı bir sorudur; domain tablolarıyla (Faz 2–3) değerlendirilecek.

**Kod üretiminin kazancı korunur.** Kod üretiminin asıl kazancı, yanlış yazılmış bir ilişki adının derleme zamanında yakalanmasıdır. Bu kazanç tipli sabitlerle alınır:
- `actions_gen.go`, `model.json`'dan üretilir: her tip için `TypeJob`, her tip ve ilişki için `ActionJobCanEdit`.
- `TestGeneratedConstants` gen-check'tir: dosya modelle uyuşmazsa `make verify` kırmızıdır.
- Servisler ilişkiyi bu sabitlerle anar. Yorumlayıcı kalır, yazım hatası derleme hatası olur.

### 2. `model.fga` kaynak, `model.json` resmi ayrıştırıcının çıktısı

- `internal/authz/model.fga` tek doğruluk kaynağıdır.
- `internal/authz/model.json`, `openfga/language`'in (`transformer.TransformDSLToJSON`) `model.fga`'dan ürettiği JSON'dur. Alan adları `openfga/api`'nin protobuf JSON adlarıdır: `computedUserset` ve `tupleToUserset` dışında snake_case. OpenFGA'nın HTTP API'si de bu adları okur; dosya olduğu gibi yüklenir.
- Servis dosyayı `//go:embed` ile gömer ve stdlib `encoding/json` ile okur. `openfga/language` servis ikilisine girmez; yalnız testler onu import eder.
- `TestModelJSONMatchesDSL` iki dosyanın aynı modeli taşıdığını doğrular.
- Güncelleme tek koşudur: `go test ./internal/authz -run 'TestModelJSONMatchesDSL|TestGeneratedConstants' -update`. Bu koşu `model.json`'ı ve `actions_gen.go`'yu birlikte yazar. `-update`'te `TestGeneratedConstants`, gömülü (eski) kopyayı değil, `model.fga`'nın resmi ayrıştırmasını okur.
- Resmi Go ayrıştırıcısı yalnız sözdizimini denetler. Anlamsal denetimi OpenFGA sunucusu model yazımında yapar: ilk koşuda `self` adını ayrılmış sözcük diye reddetti. Uygunluk job'u bu denetimi her PR'da koşar. `LoadModel` de aynı ayrılmış sözcükleri reddeder.
- Kendi DSL ayrıştırıcımız yazılmadı.

### 3. Alt küme: `with condition` çıkarıldı

ADR-0014 Karar 2'deki alt küme şöyle daraltılır: `[type]`, `[type:*]`, `[type#relation]`, `X from Y`, `or` ve sınırlı `and` / `but not`. **`with condition` (CEL) alt kümede değildir.**
- CEL bir bağımlılık ister. İlk gerçek ihtiyaçta ayrı onayla gelir.
- O güne kadar öznitelik ve durum koşulları ön süzgeçte kalır (plan §5.2-3) ya da tuple varlığına çevrilir (`job#applicant_pool` yalnız OPEN iken yazılır).

`LoadModel`, OpenFGA'nın model yazımındaki denetimleri ve alt küme kurallarını uygular. Model yüklenemezse servis açılmaz.
- Adlar OpenFGA'nın kurallarına uyar; `self` ve `this` hem tip hem ilişki adı olarak ayrılmıştır.
- Her ilişkinin bir giriş noktası olmalıdır (OpenFGA'nın "no entrypoint" kuralı): bir rewrite yolu doğrudan atamaya varmalıdır. Yalnız birbirine işaret eden computed ilişkiler (`a: b`, `b: a`) ya da tabansız özyineleme reddedilir.
- Şema 1.1 zorunludur. Modül, koşul ve bilinmeyen JSON alanı reddedilir.
- `and` ve `but not` yalnız ilişkinin en üstünde, yalın işlenenlerle (doğrudan, computed, `from`) kullanılır. İç içe küme işlemi reddedilir.
- Bunları kullanan ilişkiler `model_test.go`'daki açık listededir: bugün `job#can_apply` ve `message#can_report`. Yeni bir kullanım, listenin incelemeden geçen değişikliğidir. Gerekçe: dış motorda liste sorgularını pahalılaştırırlar (N07 §6.2-2).
- Denetim `openfga/language`'in kendi ayrıştırmasının üzerinde koşar (`TestModelSubset`).

### 4. Değerlendirme kuralları

- **Derinlik.** Sorulan ilişkinin altındaki her yeni `object#relation` araması bir seviyedir. 24 seviye çözülür, 25. seviye `ErrResolutionTooComplex` ile reddedilir. Sınırın değeri OpenFGA'nın varsayılanından (25) alındı ve yerelde testle sabittir.
  - **Bu sınır OpenFGA'nınkiyle tam eşdeğer değildir.** OpenFGA her dispatch'te bir seviye düşer, ama özyinelemeli `from` için seviye düşürmeyen hızlı yolları var.
  - Önbellekten (aşağıda) gelen bir sonuç için derinlik yeniden denetlenmez: aynı `object#relation`'a daha derinden ulaşan ikinci yol, sınırı aşsa da saklı sonucu alır. Bu kararı değiştirmez. Önbelleğe yalnız hatasız sonuç girer ve aynı Check içindeki ilk çözüm zaten sınırın içindedir; sınır aşımı her iki tarafta da ret verir.
  - Sınır vakaları gerçek OpenFGA'ya karşı da koşar (aşağıda, "Sunucuya karşı").
- **Döngü.** Kendi yolunda tekrar karşılaşılan bir ilişki izin vermez. Döngü, sonuçta ayrı bir işaretle taşınır.
  - `but not`'un çıkarılan tarafında döngü varsa dışlama kanıtlanamaz; yerel karar **ret**tir. OpenFGA'nın CycleDetected kuralı da budur.
  - `or`'da başka bir dal izin veriyorsa döngü önemsizdir.
- **Sunucuya karşı.** Döngü ve derinlik vakaları (klasör modeli: yalın döngü, tabanda döngü, `but not` altında döngü, en derin zincir, bir seviye fazlası) `TestConformanceFolderModel`'de gerçek OpenFGA'ya karşı da koşar.
  - Davranışı belgelenmiş vakalarda iki motorun aynı sonucu vermesi beklenir.
  - Sunucunun davranışı belirsiz olan vakalarda (`but not` altında döngü, zincir sınırı) Go'nun sonucu doğrulanır; OpenFGA'nın sonucu log'a yazılır ve aşağıdaki tabloda tutulur. Bu vakalar için "OpenFGA gibi" iddiası yoktur.

  | Vaka | Go | OpenFGA v1.22.0 |
  |---|---|---|
  | `but not` altında döngü | ret | (gözlem) |
  | 24 seviyelik zincir | izin | (gözlem) |
  | 25 seviyelik zincir | `resolution_too_complex` | (gözlem) |
- **Tekrar çözüm yok.** Bir Check boyunca her `object#relation` en fazla bir kez çözülür; döngüsüz ve hatasız sonuçlar saklanır. Elmas biçimli rol grafiği (iki rolün içerdiği bir rol) üstel değil doğrusal okuma yapar. Bu da testle sabittir.
- **Tip kısıtları.** Değerlendirici, ilişkinin tip kısıtına uymayan tuple'ı yok sayar (OpenFGA gibi). Eski bir modelden kalan tuple yetki vermez.
- **Kimlikler.** Nesne kimlikleri, tip ve ilişki adları OpenFGA'nın desenlerine uyar (`openfga/language` doğrulama kuralları). Uymayan nesneyle Check reddedilir; uymayan tuple ayrıştırılmaz.

### 5. `Basis`: yapılandırılmış ve belirleyici

- `Decision.Basis` bir `Basis`'tir: `[]Path`, her `Path` bir `[]Step` ile bir `Grant`'tan oluşur. `Step`, nesnenin kendisini taşır (`staff_role:finance_officer#assignee`), yalnız ilişki adını değil. Böylece personel denetimi hangi rolden geçildiğini görür.
- Son adım ile `Grant` birlikte yetkiyi veren tuple'dır.
- Denetim kaydına `Basis.String()` yazılır: adımlar `>` ile, yollar `&` ile birleşir.
- **Belirleyicilik.** Değerlendirici rewrite'ları model sırasıyla, okunan tuple'ları dize sırasıyla gezer. Sıralama değerlendiricinin içindedir; kaynağın döndürdüğü sıra önemli değildir. Aynı veri aynı `Basis`'i verir.
- `or` ilk izin veren dalı raporlar. `and` her işlenenin yolunu raporlar (hepsi geçerlidir). `but not` tabanın yolunu raporlar.
- Matriste AZM-106 ve AZM-161'in (önceki AZM-160) `basis`'i modele göre düzeltildi (sırasıyla `inviter`, `owner`); test istisnası kalmadı. `admin from company` dalı AZM-107'yle ayrıca kanıtlanır.

### 6. `TupleReader`

`TupleReader` iki metottur: `Contains` (tam tuple var mı; SQL'de `EXISTS`) ve `Read` (`object#relation`'ın kullanıcıları). Bunlar OpenFGA'nın depolama okumalarının karşılığıdır.
- Bu PR'da yalnız bellek içi kaynak var (`authz/authztest`). OpenFGA'nın yazım denetimini uygular.
- Domain tablolarına giden tipli SQL kaynakları Faz 2–3'te gelir ve çağıranın tx'inde okur.

### 7. Gizleme politikası

Ret varsayılan olarak gizlenir: kaynağın kendi 404'ü. Nesneyi zaten görebilen özneye karşı verilen retlerde gizleme yalnız özne nesneyi göremiyorsa yapılır. Bu retler: başvuru, sohbet açma, tam adres, şirket yönetimi, presence ve mesaj raporu. Görünürlüğü belirleyen ilişki politikada yazılıdır (`conceal.go`).

Politika matrisin `conceal` kolonuna karşı test edilir. Dört satır istisnadır, gerekçeleri testte yazılıdır:
- üçü alan düzeyi projeksiyon (çağıran reddetmez),
- biri WS abonelik reddi (soket üzerinde 404 yok).

### 8. `Scope`

- Alanlar dışa kapalıdır. Kurucular: `Never()`, `Always()`, `Conditional(Predicate)`, `ConditionalIDs([]string)`. Sıfır değer `Never`'dır.
- Boş bir Conditional `Normalize` ile `Never`'a döner; kurucular bunu kendileri yapar. `Validate` değişmezleri denetler.
- **Birleşim anlamı.** `Predicate`'in alanları VEYA ile bağlanır: nesne, öznenin kendi kullanıcı kimliğiyle (`SelfUserID`) ya da `CompanyIDs`'teki bir şirketle ilişkiliyse görünür.
- Bir Conditional ya predicate ya ID kümesi taşır, ikisini birden taşımaz. İkisi birlikte gerekirse bu, örtük bir birleşim değil yeni bir türdür.

### 9. Uygunluk koşucusu

- **Go tarafı** (`matrix_test.go`, etiketsiz, `make verify`): bütün `[go, openfga]` satırlarında karar, `Basis` ve `Conceal` doğrulanır.
- **OpenFGA tarafı** (`openfga_conformance_test.go`, `//go:build conformance`): `model.json` ve matris tuple'ları gerçek OpenFGA'ya yüklenir. Aynı satırlar iki motorda koşar.
  - İki motor farklıysa ya da biri matristen farklıysa test kırmızıdır.
  - Yanıtta `allowed` alanı yoksa da test kırmızıdır.
  - Tip başına tablo ve koşmayan satırların gerekçeli listesi log'a yazılır.
- **Matris okuyucusu:** `go.yaml.in/yaml/v3` (Yusuf'un 05.11 onayı). Matris `KnownFields(true)` ile tipli yapıya çözülür.
  - Bilinmeyen alan, yinelenen anahtar, ikinci YAML belgesi hata verir.
  - Fikstürlerde bildirilmemiş bir özne, nesne ya da tuple referansı da hata verir.
- **CI:** `verify.yml`'de ayrı `authz-conformance` job'u var.
  - `openfga/openfga` service container olarak çalışır; digest'le sabitlenmiştir (v1.22.0, 14 gün kuralına uyan en yeni sürüm).
  - Bellek içi depo kullanılır, playground kapalıdır, port yalnız loopback'e açıktır.
  - Koşu `make test-conformance` ile yapılır. Adres `FGA_API_URL`'den gelir (OpenFGA CLI'nin adı).
- **Etiket:** `integration` değil, `conformance`; çünkü entegrasyon job'unda OpenFGA yok. `scripts/test-integration.sh`'nin etiket koruması `*_conformance_test.go`'yu tanıyor; golangci-lint iki etiketle koşuyor.
- OpenFGA'ya stdlib HTTP istemcisiyle bağlanılır; `openfga/go-sdk` onaylı listede değil.
- **§5.6-5'in DB/VIEW ayağı.** Plan, OpenFGA'ya yüklenen tuple'ların `authz_tuples` VIEW'ından gelmesini ve Go tarafının test başına DB'de koşmasını istiyor. Bu ayak domain tablolarıyla (Faz 2–3) gelir. Bugün iki motor da matrisin fikstür tuple'larını okur.

### 10. Geçiş iskeleti

- **`RelationSink` ve `RelationChange`:** bir ilişkiyi değiştiren her domain yazımı, aynı tx'te tek bir olay üretir (§5.2-5). İlk uygulama ilk ilişki tablosuyla gelir.
- **Zorlama planı (§5.2-5).** İlk ilişki tablosunu getiren PR şunları kurar:
  - ilişki tablolarına yazan sqlc sorgularını `authz.RelationChange` üreten store fonksiyonlarına bağlayan bir mimari test (`internal/archtest`),
  - bu sorguların üretilmiş paketlerini yalnız o store dosyalarından import ettiren bir depguard kuralı,
  - yetki amaçlı ilişki tablosu okumasının `authz` dışından yapılmasını yasaklayan bir depguard kuralı (§5.6-4).

  Bugün ilişki tablosu olmadığı için kurallar boş kalır; yazılmadılar.
- **`ShadowChecker` (dark mode, §5.7-2):** kararı birincil verir, gölgeye aynı soruyu sorar. `Allowed` ya da `Conceal` farkını ve gölge hatasını `MismatchCounter`'a ve log'a yazar.
  - Gölge kendi bağlamında koşar: isteğin iptalinden kopuk, kendi zaman aşımıyla.
  - `koala_authz_mismatch_total` sayacı `MismatchCounter` arayüzünün arkasındadır; Prometheus uygulaması metrik paketiyle gelir.
- **`AtLeastAsFresh`:** bugün no-op. Yerel okuma çağıranın tx'indedir, kendi yazdığını zaten görür.

### 11. Arayüzler tüketici tarafında

`Checker`, `Scoper`, `CapabilityReader` ve `RelationSink` §5.3'ün sözleşmesidir. Servisler bunları bağımlılık tipi olarak import etmez; ihtiyaç duydukları tek metodu kendi paketlerinde tanımlar ve `*Evaluator` ile bağlanır (CLAUDE.md "Arayüz kullanan tarafta tanımlanır").

## §5.3 taslağından sapmalar

| Yer | §5.3 | Bu karar | Gerekçe |
|---|---|---|---|
| `RelationChange` | `Object`, `Relation`, `Subject string`, `Actor` | `Op`, `Tuple`, `Actor` | Tuple, OpenFGA `Write` gövdesiyle birebir aynı tiptir (N07 §6.2-3) ve ayrıştırılmış, doğrulanmış hâldedir. Kullanıcı tarafı dize değil `TupleUser`'dır; userset ve joker tip düzeyinde ayrılır. |
| `Decision.Basis` | `string` ("worker", "editor from job", "staff_role:FINANCE_OFFICER") | `Basis` = `[]Path{Steps []Step, Grant}`, `String()` ile serileşir | Denetim, nesne kimliğini (hangi rol, hangi şirket) ve `and`'in bütün yollarını taşımalı. Dize hem bilgi kaybeder hem testte ayrıştırma ister. |
| `SubjectKind` | `User \| Staff \| System` | Sıfır değer `SubjectInvalid` | Unutulmuş bir `Subject{}` geçerli bir özne olmamalı; Check onu reddeder. `System`'in model tipi yok, o da reddedilir. |
| `Scope` | Dışa açık alanlar | Kapalı alanlar, kurucular, `Normalize`/`Validate` | Boş Conditional ve predicate+ID karışımı gibi anlamsız durumlar kurulamaz (Karar 8). |
| `Action` | Dize | Dize + üretilmiş sabitler | Karar 1. |

## Faz 0 önerisinden sapmalar (`model.fga`)

| Yer | Faz 0 önerisi | Bu karar | Gerekçe |
|---|---|---|---|
| `user` | `self: [user]`, `can_see_presence: self or contact` | `itself: [user]`, `can_see_presence: itself or contact` | `self` OpenFGA'da ayrılmış sözcük; sunucu modeli reddediyor. Matris tuple'ları da `itself` oldu. |
| `message` | `can_report: participant from conversation but not sender` | `can_read: participant from conversation`, `can_report: can_read but not sender` | Aynı karar. `can_read` gizleme politikasının görünürlük ilişkisidir (C39: gönderen 403, yabancı 404). |
| `platform` | `user_read_pii` + "… §5.4'teki gibi" | 25 iznin hepsi, `[staff_role#assignee]` | Personel CSV'sindeki 25 izin, nokta yerine alt çizgiyle (§5.4). |
| Yorumlar | Satır sonu `#` yorumları | Yorumlar kendi satırında, `define`/`type` öncesinde | OpenFGA dilbilgisi yorumu yalnız bu konumlarda kabul ediyor. |

Matris (`pairs.py` üzerinden yeniden üretildi):
- `company:co_{1,2}#viewer@user:*` eklendi. Eksik olduğu için AZM-092 (C24) hiçbir motorda geçemiyordu.
- `self` → `itself` oldu.
- AZM-106'nın `basis`'i `inviter`, AZM-161'inki (önceki AZM-160) `owner` oldu; `basis` sözlüğüne `inviter` eklendi.
- `company_invitation#can_read`'in `admin from company` dalı için ayrı bir izin satırı eklendi (AZM-107): daveti göndermemiş ikinci co_1 ADMIN'i (`u_co_admin2`). Sonraki satırların numaraları bir kaydı.

## Ertelenenler

- **PII kapısı** (`internal/pii`, §5.5): personel RBAC, denetim çekirdeği ve PII kapısını birlikte getiren PR'da, Faz 1 içinde gelir. Bu PR yalnız authz çekirdeğidir.
- **Personel tipleri matriste** (`staff`, `staff_role`, `platform`): personel RBAC PR'ında girer. Satır kaynağı `docs/faz0/authz-personel-izinleri.csv`. Bugün `assignee from includer` özyinelemesi bir Go testiyle sabittir (`TestStaffRoleInclusion`): rol zinciri, döngü, atanmamış personel.
- **Ön süzgeçler ve `Scope` uygulaması:** domain tablolarıyla gelir. `go_only` satırları o zaman koşar.

## Sonuçlar

- Çıkış kriteri "≥1 tip yeşil"; sonuç bütün `[go, openfga]` satırları: 153 satır, 12 tip. 55 `go_only` ve 6 `skip` satır koşmaz; gerekçeleri raporda satır satır yazılır.
- Yeni bağımlılıklar: `openfga/language` v0.3.2 (yalnız testte import edilir) ve `go.yaml.in/yaml/v3` v3.0.6 (yalnız matris okuyucusunda).
- Kapı dört job'dur: `verify`, `integration`, `image`, `authz-conformance`. Merge için hepsi yeşil olmalıdır (CLAUDE.md). Branch protection açıldığında dördü de zorunlu kontroldür (`docs/runbook/yayin.md`). Yayın, `main` koşusunun bütün job'larının yeşilini ister.
