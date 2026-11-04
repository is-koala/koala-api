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
| [`payment.yaml`](payment.yaml) | Ödeme, emanet, payout, iade, iptal cezası | 20 |
| [`auth.yaml`](auth.yaml) | Kimlik doğrulama, oturum, parola, personel girişi | 18 |

Diğer alanlar (atama, ilan, mesajlaşma, KVKK, ulaşım…) ilgili fazda eklenir.

## Biçim

```yaml
domain: payment          # dosyanın alanı
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
    decision: K2         # isteğe bağlı: senaryoyu etkileyen karar kaydı maddesi
    target: >            # isteğe bağlı: Go hedefi Java'dan farklıysa hedef davranış
      …
    oracle: test/oracle/<tablo>.csv   # isteğe bağlı: kuralı satır satır bağlayan karar tablosu
    source:              # kaynak Java testleri: <src/test/java'dan göreli yol>#<metot>
      - "com/exskylab/koala/…/XxxIT.java#metot"
    go_test: null        # Go testinin tam adı: "internal/payment.TestReconcile/PAY-001"
```

### Alan kuralları

- **`given`, `when`, `then` iş dilindedir.** Örnek: "işveren ödeme oturumu başlatır", "atamanın ödeme durumu EMANETTE olur".
- **Durum adları Java enum adlarıyla yazılır.** Örnek: `HELD_IN_ESCROW`, `VERIFY_PENDING`.
  - Bu bir API bağımlılığı değildir, iş sözlüğüdür. Go'da ad değişirse eşleme `README`'ye eklenir.
- **Tutarlar TL ve kuruşla yazılır** (`1.075,00 TL`). İçeride `int64` kuruş tutulur (K5); senaryo bunu varsaymaz.
- **`then` yalnız gözlenebilir sonuçları yazar:** durum, tutar, kayıt, olay, dış çağrı. "Olmaması gerekenler" de buraya girer (`"sağlayıcıya ikinci çağrı yapılmaz"`).
- **`target`**, Go'nun kasıtlı olarak farklı davranacağı durumlarda yazılır. Bunun dayanağı bir karar kaydı maddesidir (K2, K12…). Bu durumda `then` Java'nın bugünkü davranışını anlatır, `target` ise Go'nun hedefini.
- **`source`** boş bırakılamaz. Senaryo bir Java testinden ya da Java kodundan çıkmadıysa kataloğa girmez.
  - Kod kaynağı yazılabilir: `src/main/java/…/X.java:satır`.
- **`go_test`**, Go testi yazılana kadar `null` kalır.

### Öncelik

| Öncelik | Anlam | Faz çıkışı |
|---|---|---|
| P0 | Para, güvenlik, KVKK ya da veri bütünlüğü; kırılırsa para kaybı, yetkisiz erişim ya da geri alınamaz durum | Alanın fazında %100 yeşil |
| P1 | İş kuralı; kırılırsa yanlış davranış ama düzeltilebilir | ≥ %90 yeşil, Faz 9'da %100 |
| P2 | Kullanıcı deneyimi, kozmetik, gözlemlenebilirlik | En iyi çaba |

Dosya içinde P0'lar önce gelir.

## CI kuralı (Faz 1'de kurulacak)

Bir Go testi her `test/scenarios/*.yaml` dosyasını okur ve şunları doğrular:

1. `id`'ler tekil ve `<ALAN>-<nnn>` biçiminde.
2. `priority` ∈ {P0, P1, P2}; `source` boş değil.
3. Alanın fazı başladıktan sonra her P0 satırının `go_test`'i dolu, test var ve geçiyor. Eksik olan kırmızıdır.

Kapsama oranı (P0/P1 için `go_test` dolu ve yeşil olanlar / toplam) faz çıkışında ölçülür.

## Senaryoların çıkarıldığı yer

- Bu ilk iki dosya Java IT'lerinden ve kural testlerinden çıkarıldı. Kaynak `origin/main` anlık görüntüsü; testler okundu, koşturulmadı.
- Bazı senaryolar birkaç testi tek iş kuralında birleştirir. Bu durumda `source` hepsini listeler.
- Tutar ve karar tablosu gerektiren kurallar (`reconcile` zinciri, komisyon, iptal cezası, durum geçişleri) [`test/oracle/`](../oracle/) tablolarına bağlıdır. Senaryo kuralı anlatır, tablo her satırı.
