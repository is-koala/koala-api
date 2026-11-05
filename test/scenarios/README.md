# Senaryo kataloğu

Plan §6.2-1. Java HTTP testleri yeni API'ye karşı koşamaz, çünkü sözleşme değişiyor. Eşitlik bu yüzden **iş kuralı** düzeyinde kanıtlanır. Bu klasördeki her senaryo, Go'da en az bir testle (`go_test`) bağlanmak zorunda olan bir iş kuralıdır.

Senaryolar **API'den bağımsız iş dilinde** yazılır:

- Uç adı, HTTP metodu, JSON alan adı ve statü kodu senaryonun parçası değildir.
- Hata, iş hata kodu olarak yazılır (`PAYMENT_PROVIDER_UNAVAILABLE` gibi). Bu kodların API'deki karşılığı OpenAPI sözleşmesinde tanımlanır.
- Ret biçimi bir iş kuralıysa senaryoda yer alır. Örnek: "var olmayan kaynaktan ayırt edilemez".

## Dosyalar

Her alan için bir dosya: `test/scenarios/<alan>.yaml`.

| Dosya | Alan | Senaryo |
|---|---|---|
| [`payment.yaml`](payment.yaml) | Ödeme, emanet, payout, iade, iptal cezası, uyuşmazlık çözümü | 23 |
| [`auth.yaml`](auth.yaml) | Kimlik doğrulama, oturum, parola, personel girişi | 18 |

Diğer alanlar (atama, ilan, mesajlaşma, KVKK, ulaşım…) ilgili fazda eklenir.

## Biçim

```yaml
domain: payment          # dosyanın alanı
id_prefix: PAY           # bu dosyadaki bütün id'lerin öneki
version: 0               # biçim sürümü
scenarios:
  - id: PAY-001          # <ALAN>-<nnn>; bir kez verilir, yeniden kullanılmaz
    priority: P0         # P0 | P1 | P2 (aşağıda)
    title: "Kısa başlık"
    rule: >              # tek cümlelik iş kuralı: "… ise … olur"
      …
    given:               # ön koşul: dünyanın durumu (aktörler, kayıtlar, ayarlar)
      - "…"
    when:                # adımlar: aktörün ya da sistemin yaptığı iş eylemleri, sırayla
      - "…"
    then:                # beklenen iş sonucu: durumlar, tutarlar, yan etkiler, olmayanlar
      - "…"
    error: AUTH_…        # isteğe bağlı: iş hata kodu (ret senaryolarında)
    decision: K2         # isteğe bağlı: senaryoyu etkileyen karar (K1-K19, D1-D5 ya da "Java hatası (...)")
    target: >            # isteğe bağlı: Go hedefi Java'dan farklıysa hedef davranış
      …
    oracle: testdata/oracle/<tablo>.csv   # isteğe bağlı: kuralı satır satır bağlayan karar tablosu
    source:              # Java deposuna göreli tam yol
      - "src/test/java/com/exskylab/koala/…/XxxIT.java#metot"
      - "src/main/java/com/exskylab/koala/…/Xxx.java:120-135"
    go_test: null        # Go testinin tam adı ya da adlarının listesi: "internal/payment.TestReconcile/PAY-001"
    deferred: "…"        # isteğe bağlı: fazı başlamış alanda bağlanmayan P0'ın beklediği faz ve gerekçe
    partial: "…"         # isteğe bağlı: bağlı testlerin kuralın hangi kısmını sonraya bıraktığı
```

### Alan kuralları

- **`given`, `when`, `then` iş dilindedir.** Örnek: "işveren ödeme oturumu başlatır", "atamanın ödeme durumu EMANETTE olur".
- **Durum adları Java enum adlarıyla yazılır.** Örnek: `HELD_IN_ESCROW`, `VERIFY_PENDING`.
  - Bu bir API bağımlılığı değildir, iş sözlüğüdür. Go'da ad değişirse eşleme `README`'ye eklenir.
- **Tutarlar TL ve kuruşla yazılır** (`1.075,00 TL`). İçeride `int64` kuruş tutulur (K5); senaryo bunu varsaymaz.
- **`then` yalnız gözlenebilir sonuçları yazar:** durum, tutar, kayıt, olay, dış çağrı. "Olmaması gerekenler" de buraya girer (`"sağlayıcıya ikinci çağrı yapılmaz"`).
- **`target`**, Go'nun kasıtlı olarak farklı davranacağı durumlarda yazılır. Bunun dayanağı bir karar kaydı maddesidir (K2, K12…). Bu durumda `then` Java'nın bugünkü davranışını anlatır, `target` ise Go'nun hedefini.
- **`source`** boş bırakılamaz. Senaryo bir Java testinden ya da Java kodundan çıkmadıysa kataloğa girmez.
  - Biçim tektir ve envanter, matris ve oracle ile aynıdır: test için `src/test/java/…/X.java#metot`, kod için `src/main/java/…/X.java:satır`.
- **`id_prefix`** dosya başında bir kez yazılır; her `id` bu önekle başlar.
- **API sızıntısı yok:** HTTP statüsü (403…), yol (`/admin/…`), metot (GET/POST) ve "uç" sözcüğü senaryoda geçmez. Ret iş diliyle yazılır ("izin yok hatası", "var olmayan kaynaktan ayırt edilemez").
- **`go_test`**, Go testi yazılana kadar `null` kalır. Kural birden çok testle kanıtlanıyorsa liste yazılır. Bağlı her test fonksiyonu, doc yorumunda ya da gövdesinde senaryo kimliğini (`AUTH-001`) taşır.
- **`deferred`**, fazı başlamış bir alanda henüz bağlanamayan P0 için yazılır: beklediği faz ve gerekçe. `go_test` ile birlikte bulunmaz.
- **`partial`**, bağlı testler kuralın bir kısmını sonraki bir PR'a ya da faza bırakıyorsa yazılır; kapsama raporunda ayrıca sayılır.

### Öncelik

| Öncelik | Anlam | Faz çıkışı |
|---|---|---|
| P0 | Para, güvenlik, KVKK ya da veri bütünlüğü; kırılırsa para kaybı, yetkisiz erişim ya da geri alınamaz durum | Alanın fazında %100 yeşil |
| P1 | İş kuralı; kırılırsa yanlış davranış ama düzeltilebilir | ≥ %90 yeşil, Faz 9'da %100 |
| P2 | Kullanıcı deneyimi, kozmetik, gözlemlenebilirlik | En iyi çaba |

Dosya içinde P0'lar önce gelir.

## CI kuralı

`scenarios_test.go` her `test/scenarios/*.yaml` dosyasını katı okur (bilinmeyen alan hatadır) ve şunları doğrular:

1. `id`'ler tekil ve `<id_prefix>-<nnn>` biçiminde; `id_prefix` dosya başında var (`TestCatalogsAreWellFormed`).
2. `priority` ∈ {P0, P1, P2}; `source` boş değil.
3. Fazı başlamış alanda (`activeDomains`; Faz 1'den beri `auth`) her P0'ın ya `go_test`'i ya `deferred`'ı var, ikisi birden değil. Bağlı her test, hangi öncelikte olursa olsun, paketinin test dosyalarında var ve senaryo kimliğini taşıyor (`TestScenariosAreBoundToTests`). Testin geçtiğini kapı kanıtlar: birim testleri `make verify`, DB'li olanları `make test-integration` koşar.

Kapsama oranı (P0/P1 için `go_test` dolu ve yeşil olanlar / toplam) faz çıkışında ölçülür.

## Senaryoların çıkarıldığı yer

- Bu ilk iki dosya Java IT'lerinden ve kural testlerinden çıkarıldı. Kaynak `origin/main` anlık görüntüsü; testler okundu, koşturulmadı.
- Bazı senaryolar birkaç testi tek iş kuralında birleştirir. Bu durumda `source` hepsini listeler.
- Tutar ve karar tablosu gerektiren kurallar (`reconcile` zinciri, komisyon, iptal cezası, durum geçişleri) [`testdata/oracle/`](../../testdata/oracle/) tablolarına bağlıdır. Senaryo kuralı anlatır, tablo her satırı.
