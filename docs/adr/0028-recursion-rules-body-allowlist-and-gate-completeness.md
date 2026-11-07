# ADR-0028: Model özyineleme kuralları, gövde maskesinde izin listesi ve kapının tamlığı

- Durum: Kabul edildi
- Tarih: 07.11.2026
- Karar kaydı: K17 (ADR-0014'ün uygulaması), K14 ve S4 (denetim kaydı), K9 (sözleşme), K7 (üretilen kod)
- Kaynak: issue #14 "Faz 2 ön koşulları" (Authz maddesi, "Yarım kapanmış korumalar"ın iki maddesi, "Kapının görmediği yerler"in iki maddesi); [ADR-0024](0024-authz-core-evaluator-and-conformance.md) Karar 3 ve §4; [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md) Karar 7 ve açık soru 2; [ADR-0007](0007-error-model.md); [ADR-0004](0004-quality-gate-and-tool-pinning.md); PR #13'ün iki incelemesi ve doğrulaması; OpenFGA v1.22.0 davranışı (uygunluk koşucusunun ölçtüğü)
- Kod: `internal/authz/model.go`, `internal/authz/doc.go`, `internal/audit/body.go`, `internal/audit/domain.go`, `internal/archtest/errorcode_test.go`, `Makefile`
- Yerine geçtiği:
  - **ADR-0024 Karar 3**'teki alt küme tanımını iki kuralla daraltır. "`and` ve `but not` yalnız ilişkinin en üstünde, yalın işlenenlerle" koşulu özyinelemeyi engellemiyordu: yalın bir relation da özyinelemeli olabilir, tek sınır çalışma anındaki `MaxResolutionDepth`'ti (Karar 1).
  - **ADR-0024 §4**'ün iki fark kaleminin ("`but not`'un çıkarılan tarafında döngü", "25 seviyelik zincir") "Bugünkü modelde ikisi de kararı etkilemez" gerekçesinin yerine geçer: artık modelin bugünkü hâline bakmıyoruz, iki uç duruma ulaşan bir model **yüklenemiyor**. Aynı §'in "Geçiş günü dark mode bu farkları sayaçta gösterir. Hangisinin doğru kaynak olacağına o gün karar verilir" kalemi bu iki fark için gereksizleşir; farklar yalnız koşucunun kendi klasör modelinde ölçülmeye devam eder (Karar 1).
  - **ADR-0026 Karar 7**'deki gövde maskesi tanımının ("iki katmandır: anahtar adı kişisel bir parça içeriyorsa … İstisnalar da sonek mantığıyla eşleşir") yerine geçer ve **açık soru 2**'yi kapatır (Karar 2).
  - **ADR-0007 madde 5**'i ("Servisin döndürdüğü her hata kodu `ErrorCode` listesindedir") listelerin tamlığıyla genişletir: liste dışı bırakılmış bir kod sabiti artık kapıda kırmızıdır (Karar 3).
  - **ADR-0004**'ün `gen-check` kapsamını `docs/faz0/tarama/pairs.py`'nin çıktılarına genişletir; `python3` kapının bir aracı olur (Karar 4).

## Bağlam

Faz 1 bitti (main `55181d3`): platform, kimlik doğrulama, authz çekirdeği, personel RBAC, denetim kaydı ve PII kapısı. Faz 2 kimlik, kullanıcı, şirket, referans ve medyayı getiriyor — yani **ilk gerçek PII alanlarını**, çok daha fazla hata kodunu, izni ve matris satırını.

Issue #14'teki dört madde, Faz 1'in ölçülebilir çıkış kriterlerinin dışında kalmış ama o yükten **önce** kapanması gereken yerlerdir. Dördünün ortak özelliği: bugün bir şey kırmıyorlar, çünkü deponun bugünkü içeriği onlara denk gelmiyor. Faz 2'nin getirdiği içerik denk gelecek.

1. **Authz.** Yerel değerlendirici ile OpenFGA v1.22.0 tam iki uç durumda ayrışıyor (ADR-0024 §4, koşucuyla ölçüldü): `but not`'un çıkarılan tarafındaki döngüde Go **ret** verirken OpenFGA **izin** veriyor, ve 25 seviyeyi geçen zincirde Go `resolution_too_complex` derken OpenFGA çözmeye devam ediyor. `LoadModel` bu iki uç duruma varan bir modeli kabul ediyordu; ayrımı yalnız bugünkü `model.fga`'nın içeriği engelliyordu. OpenFGA'ya geçiş (plan §5.7) böyle bir modelle sessizce karar değiştirir.
2. **Denetim gövdesi.** Maske bir **red listesiydi**: anahtar parçası + sonek + istisna + değer düzeyinde desen. Red listesi tanımı gereği eksiktir, ve bu ölçüldü: PR #13'ün incelemesinde beş anahtar kaçak çıktı, doğrulamasında JSON sayı dalında 17-19 haneli kart kaçağı bulundu. Üstelik istisna listesi parça listesinden **önce** kısa devre yapıyordu ve sonek mantığıyla eşleştiği için `passwordFieldName`, `emailHostname` ve `secretTokenVersion` muaf oluyordu.
3. **Hata kodları.** `httpx.PlatformCodes()`, `softdelete.Codes()`, `realtime.Codes()`, `authn.Codes()` ve `staff.Codes()` elle tutuluyor. `TestPlatformCodesAreInTheContract` listedekilerin sözleşmede olduğunu denetliyor; listelerin **tam** olduğunu kimse denetlemiyordu. Listeye konmamış bir kod, hiçbir üretilmiş istemcinin bilmediği bir kod olarak kullanıcıya gider.
4. **Matris.** `internal/authz/testdata/matrix.yaml` ve `docs/faz0/authz-ciftleri.md`'yi `pairs.py` üretiyor, ama `gen-check` onları görmüyordu. Bayat bir matris, `authz-conformance` job'ının yeşiline bakanlara **yanlış kanıt** üretir: koşu yeşildir, ölçtüğü şey eski kurallardır.

## Karar

### 1. `LoadModel` iki özyineleme kuralını yükleme anında uygular, fail-closed

Modelin ilişki grafiği üzerinde çalışılır: düğümler `<tip>#<relation>`, kenarlar bir çözümün bir sonraki adımda sorabileceği relation'lardır (doğrudan atamanın userset kısıtı `[company#member]`, computed userset, `from` zincirinin her ebeveyn tipi, küme işlemlerinin işlenenleri). Bir relation **özyinelemelidir** eğer kendi kenarlarından kendisine ulaşılıyorsa.

- **Kural 1 — `but not`'un çıkarılan tarafı.** Çıkarılan taraftan ulaşılan hiçbir relation özyinelemeli olamaz. Gerekçe: oradaki bir döngü dışlamayı yanlışlanamaz yapar; yerel karar bilinçli olarak **ret**tir (ADR-0024 §4), OpenFGA **izin** verir.
- **Kural 2 — özyinelemeli `from` zincirleri.** Böyle bir zincir yalnız **sınırlı DAG tiplerinde** kullanılabilir (`boundedDAGTypes()`, bugün yalnız `staff_role`). Gerekçe: sınırsız bir zinciri yalnız `MaxResolutionDepth` durdurur ve OpenFGA'da o sınır yoktur.
- İki kural da ret yönünde yanılır: kuşkulu model **yüklenmez**, servis açılmaz. Hata mesajı hangi relation'ın hangi kuralı çiğnediğini ve nedenini yazar.

**`staff_role` neden listede.** Rol kapsama grafiği veritabanında DAG olarak zorlanır: `staff_role_includes_no_self_edge` CHECK'i ve `staff_role_includes_acyclic` tetikleyicisi (0040) döngü kuran her yazımı reddeder; derinlik rol sayısıyla sınırlıdır. Yani `assignee from includer` sonlanır ve döngüye giremez — iki motorun ayrışabileceği durum oluşmaz. Listeye yeni bir tip, **ancak aynı güvenceyle** ve onu getiren PR'da girer.

**Kural 1 kuralda 2'nin kapsamadığı bir şeyi yakalar.** Özyineleme `from` zinciriyle değil, relation'ın kendi userset kısıtıyla da kurulabilir (`doc#viewer: [user, doc#viewer]`). Kural 2 böyle bir modeli görmez; kural 1 onu çıkarılan tarafta yakalar. Bu yüzden iki kural ayrı ayrı yazıldı ve ayrı ayrı test edildi.

**Koşucunun klasör modeli ret yemiyor.** `folderModel` (`evaluator_test.go`) tam bu iki uç durumdan kurulu, çünkü ölçtüğü şey onlar. Test tarafı modeli `authz.LoadModelAllowingRecursionOn(data, []string{"folder"})` ile yükler; bu fonksiyon `export_test.go`'dadır, yani **yalnız test ikilisinde vardır**. Üretim yolunda (`DefaultModel`, `LoadModel`) gevşetme kapısı yoktur. `TestLoadModelRefusesTheFolderModel` aynı modelin `LoadModel`'den geçmediğini kanıtlar.

**Değerlendirici değişmedi.** `MaxResolutionDepth` ve döngü davranışı aynı kaldı: bunlar artık modelin ulaşamayacağı durumlar için ikinci savunmadır, çünkü tuple verisi (soft-delete, eşzamanlı yazım) değil model şekli kısıtlanıyor.

### 2. Gövde maskesi izin listesine geçti

`MaskBody` artık **izin listesiyle** çalışır: bir skaler değer, onu adlandıran anahtar `allowedKeys()`'te (tam ad) ya da `allowedKeyClasses()`'tan bir sınıf sonekiyle eşleşiyorsa açık kalır; **geri kalan her şey `***` olur**, ne taşıdığına bakılmaz. Karar tek sıralı ve tamdır:

1. **Tam ad** izin listesinde mi → açık. (İncelenmiş, adıyla yazılmış anahtar.)
2. **Sınıf soneki** eşleşiyor ve anahtar `piiKeyParts()`'tan bir parça **içermiyor** mu → açık.
3. Yoksa → `***`.

Sonra, açık kalan her skalerde **değer katmanı** son savunma olarak koşmaya devam eder: e-posta içeren, telefon olan ya da Luhn'dan geçen 13-19 haneli kart olan değer (JSON sayısı olarak gelse de, `UseNumber` ile hane hane) maskelenir.

**İstisnanın kısa devresi yapısal olarak kapandı.** Eski istisna listesi **sonek** olarak eşleşiyor ve parça listesinden önce dönüyordu; `passwordFieldName`, `emailHostname`, `secretTokenVersion` böyle muaf oluyordu. İzin listesi **tam adla** eşleşir: bir bileşik anahtar, bittiği anahtar değildir. Üstüne, sınıf soneki eşleşmesi `piiKeyParts()` ile daraltılır, yani `nationalId`, `emailId`, `latitudeType`, `secretTokenVersion` sınıfa uysa bile maskelenir. Sıra artık belirsiz değil: izin → daraltma → maske.

**İzin listesinde ne var.** Üç sınıf ve bir istisna:
- **Enum, kod ve ad (nesnenin adı, kişinin değil):** `action`, `code`, `decision`, `outcome`, `status`, `kind`, `type`, `sensitivity`, rol/izin/kapsam adları (`role`, `roles`, `rolecode`, `rolename`, `permission`, `permissions`, `scope`…), şema ve yönlendirme nesnelerinin adları (`tablename`, `columnname`, `routename`, `queuename`, `triggername`…), personelin kendi kurduğu taksonomiler (`categoryname`, `badgename`, `tagname`).
- **Sayaç, boyut, sayfalama ve para:** `count`, `total`, `page`, `limit`, `offset`, `cursor`, `bytes`, `quantity`, `amount`, `currency`, `rate`, `fee` (ADR-0008: kuruş ve para birimi, kart değil).
- **Bayrak, sürüm ve zaman:** `enabled`, `active`, `approved`, `required`, `force`, `dryrun`, `version`, `createdat`, `expiresat`, `duration`, `ttl`, `retention`…
- **Adıyla iki istisna:** `mustchangepassword` ve `tokenversion`. İkisi kişisel bir parça (`password`, `token`) içeriyor ama biri bayrak, öteki sayaçtır. `TestOnlyTwoAllowedKeysHoldAPersonalPart` listedeki kişisel parça taşıyan anahtarların **tam olarak bu ikisi** olduğunu bağlar: üçüncüsü eklenirse testi değiştirmek gerekir, yani karar görünür olur.

**Sınıf sonekleri** (`id`, `ids`, `uuid`, `count`, `total`, `version`, `status`, `state`, `kind`, `type`, `code`, `codes`, `enabled`, `flag`) tek bir anahtar için **eklenmez**: bir sınıf genellemedir ve o sonekle biten her gelecek anahtarı da satın alır. Tek anahtar `allowedKeys()`'e yazılır.

**Denetim kaydı bağlamını ne kadar koruyor.** Maske **biçimi bozmaz**: anahtarlar, iç içelik ve her dizinin uzunluğu kalır; kapsayıcılar her zaman gezilir ve her skaler yaprak onu adlandıran anahtara göre yargılanır (dizi öğeleri dizinin anahtarına göre). Yani `{"address":{"street":"***","city":"***","count":1},"phones":["***","***"]}` okuyan biri, operasyonun hangi alanları gönderdiğini ve kaç telefon geldiğini görür; göremediği şey değerlerdir. Denetim satırının asıl bilgisi (aktör, eylem, izin, basis, hedef, sonuç, request id) ayrıntı satırında değil, zaten maskesiz **entry** satırındadır (ADR-0026 Karar 7); gövde oradaki bilgiyi tamamlar, taşımaz.

Fiyatı açıkça: personelin yazdığı serbest metin (`reason`, `note`, `comment`) ve kişiyi anan adlar (`companyName`, `fileName`, `targetRoleName`) artık maskelenir. Üçünün de gerekçesi aynı: serbest metin KVKK açısından her şeyi içerebilir, şahıs şirketinin adı kişinin adıdır, ve `targetRoleName` tam ad olarak listede değildir. Gerçekten gereken biri çıkarsa **genişletme yolu** şudur:

> Anahtar, onu getiren operasyonun PR'ında, tek normalize edilmiş satır olarak `allowedKeys()`'e yazılır; satırın üstündeki yorum hangi sınıf değer olduğunu söyler. İncelemenin sorusu tek: "bu anahtar bir gün bir kişinin yazdığı ya da bir kişiye dair bir şey tutabilir mi?" Evetse anahtar listeye girmez; kayıt gövdenin biçimini değerleri olmadan tutmaya devam eder. Bir testi yeşile çevirmek için hiçbir şey eklenmez.

**Anahtarın kendisi de maskelenir.** Bir harita e-posta adresiyle anahtarlanmışsa (`{"ayse@example.com": {...}}`) yalnız değerleri maskelemek adresi basardı; anahtar da değer katmanından geçer. İki maskelenmiş anahtar çakışırsa sayı kaybolur, değerler zaten maskelidir.

### 3. Hata kodu listelerinin tamlığı mimari testte

`internal/archtest/errorcode_test.go`, modülün tamamını AST ile tarar ve iki kuralı zorlar:

- **`TestEveryErrorCodeIsInItsPackageList`:** bir pakette tanımlı her hata kodu sabiti, o paketin `[]apperr.Code` döndüren liste fonksiyonunda (`Codes()`, `PlatformCodes()`) anılmış olmalı. Anılmayan sabit ve liste fonksiyonu olmayan paket kırmızıdır.
- **`TestCodeListsReachTheContractTest`:** liste tam olsa da onu kimse okumuyorsa işe yaramaz; her liste fonksiyonu `internal/app/api_test.go`'da çağrılmış olmalı, çünkü kodların `api/components/problem.yaml` ile buluştuğu tek yer orasıdır.

**Kod sabiti nasıl tanınıyor.** İki işaretin birleşimi: (a) tipi `apperr.Code` olan sabit, (b) adı `Code…` ile başlayan ve değeri UPPER_SNAKE dize olan sabit. (b) gerekli, çünkü `internal/platform/httpx`'in kodları **tipsiz** dize sabitidir ve `apperr.Code`'a yalnız `PlatformCodes`'un dilim literalinde dönüşür: yalnız tipe bakan bir kural o paketi bütünüyle kaçırırdı. İkisinin birleşimi, başka türden dize sabitlerini (`ActionCreate`, `ownerRoleCode`, `ExtPermission`) dışarıda bırakır. Dedektörün kendi kırmızı/yeşil vakaları (`TestErrorCodeDetector`, `TestUpperSnakeShape`) her biçimi beklenen bulgu **sayısıyla** sınar; tarama 40'tan az sabit ya da 5'ten az liste bulursa bu da kırmızıdır (bugün 49 sabit, 5 liste).

**Tek muafiyet gerekçeli ve denetleniyor:** `internal/platform/apperr` kendi `CodeInternal`'ını listelemez, çünkü onu istemciye yazan paket `httpx`'tir ve `httpx.PlatformCodes()` onu listeler. Muafiyet listesi, muaf paketin gerçekten kod tanımlamaya devam ettiğini denetler: bayat muafiyet kırmızıdır.

### 4. `pairs.py` çıktıları `gen-check`'in içinde

`make generate` artık `python3 -I docs/faz0/tarama/pairs.py`'yi de koşar ve `ALL_GENERATED` `internal/authz/testdata/matrix.yaml` ile `docs/faz0/authz-ciftleri.md`'yi kapsar. Bayat matris `make verify`'ın `gen-check` adımında kırmızıdır.

- `python3` kapının bir aracı olur. `ubuntu-latest` imajında ve Mac'te var, bu yüzden `verify.yml`'ye kurulum adımı eklenmedi; hedef aracı bulamazsa ne gerektiğini söyleyerek düşer. Sürümü sabitlenmedi: betik yalnız stdlib kullanıyor ve çıktısını belirleyici (sözlük sırası değil, kendi tanım sırası) yazıyor — Faz 0'dan beri üç farklı Python sürümünde aynı baytları üretti. Betik `-I` ile koşar, yani yanındaki hiçbir dosyayı import edemez.
- Betik kendi tutarsızlığını (tanımsız basis, eksik fikstür) `sys.exit(1)` ile bildirir, yani `gen-check` bayatlığı değil, bozulmayı da yakalar.
- Alternatif — çıktıyı bir Go testinin golden'ı yapmak — reddedildi: üretici Python'dur, Go tarafı onu çağırmak için `os/exec` ister ve kapıda ikinci bir "üret ve karşılaştır" mekanizması doğar. `gen-check` zaten tam bu iş için var.

## Ertelenenler

- Issue #14'ün öteki maddeleri bu ADR'nin kapsamında değil: üretilmiş JOIN testi, join-lint'in yordam/predicate yanlış negatifleri, `no-soft-delete-columns` atlatmaları, `TestRetentionMatchesCatalog`'un ad bağı, kimlik taklidi fensinin ad tabanlı olması, sözleşme lint'i ve belge maddeleri, `PrepareConn` ölçümü, soft-delete eşzamanlılığı.
- Değer düzeyinde serbest metin analizi (ada, adrese ya da TCKN'ye benzeyen metni izinli bir anahtarın içinde yakalamak) yapılmadı: izin listesi serbest metin anahtarlarını zaten maskeliyor, kalan risk izinli bir anahtarın serbest metin taşımasıdır ve bu incelemenin sorusudur.
- `MaxResolutionDepth`'in OpenFGA'nınkiyle eşitlenmesi (ya da farkın dark mode sayacında izlenmesi) ADR-0024 §4'te kaldığı gibi geçiş gününün konusu; bu ADR yalnız modelin o farka ulaşmasını engeller.

## Sonuçlar

- `LoadModel` iki kural daha uygular; `model.fga`'nın bugünkü hâli ve 14 tipli uygunluk koşusu değişmeden geçer. Tek özyinelemeli relation `staff_role#assignee`'dir ve `TestModelRecursionIsBounded` bunu bağlar.
- Maskeleme davranışı **değişti**: eskiden açık kalan anahtarlar (`city`, `note`, `reason`, `companyName`, `fileName`, `targetRoleName`, `memberTokenVersion`) artık maskelenir. Depodaki tek gerçek gövde (`staff create`: `{"email", "roles"}`) ve `internal/app/staff_integration_test.go`'nun beklentisi (`body ->> 'email' = '***'`) etkilenmez.
- Kapıya yeni iş girdi: iki mimari test ve `gen-check`'in iki dosyası daha. Yeni bağımlılık yok; yeni araç `python3` (sürümü sabitlenmedi, gerekçesi Karar 4'te).
- `internal/audit`'ten iki fonksiyon silindi (`piiKeySuffixes`, `piiKeyExceptions`), biri rolünü değiştirdi (`piiKeyParts` artık yalnız sınıf soneklerini daraltır).
- **Açık sorular:**
  1. İzinli bir anahtarın serbest metin taşıması (örneğin bir `statusName`'in enum yerine cümle tutması) incelemeyle yakalanır; makineyle değil. Faz 8'in not ve bayrak uçları bunu yeniden değerlendirir.
  2. `companyName` izin listesine alınmadı. Şirket onayı denetimi (Faz 2) adı açık görmek isteyebilir; şahıs şirketinin adı kişinin adı olduğu için karar, uçlar gelince hukuki görüşle birlikte verilecek.
  3. Kural 2'nin sınırlı DAG listesi tip adına bakar, tuple verisine bakmaz. `staff_role` için güvence veritabanındadır (0040); listeye giren bir tipin güvencesinin **kodda** denetlenmesi (katalogla eşleme gibi) ikinci bir tip gelince değerlendirilecek.
