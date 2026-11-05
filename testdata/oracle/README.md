# Oracle: Java'dan çıkarılmış karar tabloları

Plan §6.2-2 ve §6.3. Go'nun para ve durum kuralları bu tabloları satır satır üretmek zorunda. Tablo-güdümlü bir Go testi CSV'yi okur, her satırın girdisini Go fonksiyonuna verir ve çıktıyı karşılaştırır.

**Java çalıştırılmadı.** Satırlar Java kodu ve Java testlerinin iddiaları okunarak yazıldı; tutar tabloları Java formülünün Python `Decimal` ile birebir yeniden uygulanmasıyla hesaplandı. Bütün CSV'ler tek betikten üretilir ve üretim sırasında sözlüğe karşı doğrulanır:

```sh
python3 -I docs/faz0/tarama/oracle_uret.py      # depo kökünden
```

Elle düzenlemeyin; betiği güncelleyin.

## Değer sözlüğü (bütün tablolar)

| Değer | Anlam |
|---|---|
| `*` | girdi kolonunda: herhangi bir değer |
| `=` | çıktı kolonunda: değişmez |
| boş | uygulanmaz |
| `A\|B` | küme: A ya da B |
| `A+B` | birlikte: A ve B (ör. `REFUND+DISAPPROVE`) |
| `>=N`, `<N`, `<=N` | karşılaştırma (sayısal kolonlarda ve `kosul` içinde) |
| `null` | açık null değer |
| `yok` | kayıt yok |
| `key=değer;key=değer` | `kosul` kolonu: koşulların hepsi |
| `now`, `backoff` | çıktı zamanı: işlem anı; `backoff.csv`'ye göre ileri zaman |
| Durum adları | Java enum adı (`HELD_IN_ESCROW`); Go adı farklıysa `go_*` kolonunda |
| Hata kodları | Java `errorCode` (`{a.b.c}` anahtarından türetilir: `A_B_C`); `DB_CHECK_VIOLATION` veritabanı kısıtı |
| Süreler | `_sn` ile biten kolonlarda saniye |
| Tutarlar | `_kurus` kolonlarında int64 kuruş (K5); ondalık kolonlar Java `BigDecimal` |
| Oranlar | ondalık (Java) ve `_bps` (K5, 1250 = %12,5) |

Açıklama yalnız `not` kolonundadır. Kaynak kolonları tam yol taşır: `src/main/java/…/X.java:satır` ve `src/test/java/…/XTest.java#metot`, birden çoksa `; ` ile.

## `dogrulama` ve `test_kolonlari`

`test_kolonlari`, bağlanan Java testlerinin **gerçekten iddia ettiği** çıktı kolonlarını `|` ile listeler. `dogrulama` bundan hesaplanır:

| Değer | Anlam |
|---|---|
| `test` | Test, satırın bütün boş olmayan çıktı kolonlarını iddia ediyor. |
| `kısmi` | Test çıktı kolonlarının bir kısmını iddia ediyor; kalanlar koddan. |
| `kod` | Test yok; değer Java kodu okunarak yazıldı. **Doğrulanmadı.** |
| `hesap` | Test yok; değer formülün Python ile yeniden uygulanmasından. **Doğrulanmadı.** |
| `karar` | Java'da karşılığı yok; değer karar kaydından (K2). |

## Tablolar

| Tablo | Satır | test | kısmi | kod | hesap | karar |
|---|---|---|---|---|---|---|
| [`reconcile.csv`](reconcile.csv): ödeme sonucu karar zinciri | 38 | 0 | 24 | 14 | 0 | 0 |
| [`commission.csv`](commission.csv): komisyon / toplam | 24 | 4 | 3 | 0 | 17 | 0 |
| [`cancellation-fee.csv`](cancellation-fee.csv): iptal cezası | 21 | 0 | 9 | 0 | 12 | 0 |
| [`backoff.csv`](backoff.csv): doğrulama ve settlement geri çekilmesi | 17 | 4 | 2 | 0 | 11 | 0 |
| [`escrow-transitions.csv`](escrow-transitions.csv): atama ödeme durumu FSM'i, 6×5 tam matris | 30 | 6 | 0 | 24 | 0 | 0 |
| [`escrow-predicates.csv`](escrow-predicates.csv): `acceptsPayment` / `canRelease` / `canRefund` | 11 | 7 | 0 | 4 | 0 | 0 |
| [`assignment-transitions.csv`](assignment-transitions.csv): atama durum geçişleri | 29 | 5 | 10 | 14 | 0 | 0 |
| [`payout-transitions.csv`](payout-transitions.csv): payout, iade, settlement görevi | 30 | 20 | 2 | 6 | 0 | 2 |
| **Toplam** | **200** | **46** | **50** | **62** | **40** | **2** |

Doğrulanmamış (`kod` + `hesap`): **102 satır**; ayrıca 50 satır kısmi. Iyzico imza vektörleri ayrı: [`../iyzico/vectors.json`](../iyzico/vectors.json).

`reconcile.csv` hiç `test` satırı taşımaz: Java testleri her dalda deneme ve atama durumunu iddia ediyor ama `failure_reason`, sayaç, sonraki doğrulama zamanı gibi kolonların çoğunu iddia etmiyor.

## Go testi hangi kolona göre koşar

| Tablo | Go beklentisi |
|---|---|
| `payout-transitions.csv` | **Her zaman `go_beklenen` ve `go_hata`.** `go_beklenen` her satırda dolu ve makinece okunur (durum adı ya da `=`). `go_onceki` Go'daki başlangıç durumudur. |
| `assignment-transitions.csv` | `go_durum`, `go_odeme`, `go_hata` (D4 ve Java hataları burada ayrışır). |
| `cancellation-fee.csv` | Tutar kolonları (`_kurus`, `_bps`) ve `go_saglayici_islemi`; `go_uygulanabilir = hayir` satırları atlanır. |
| `commission.csv` | `_kurus` ve `oran_bps` kolonları; `go_uygulanabilir = hayir` atlanır. |
| Diğerleri | Java kolonları (Go hedefi aynı). |

## K2: payout eşlemesi (ADR-0009)

Java'da payout APPROVE başarılı olunca doğrudan `COMPLETED` oluyor (`SettlementManager.java:199-219`). CLAUDE.md §2y3 ise COMPLETED'ın tek yolunun ekstre mutabakatı olduğunu söylüyor. K2 bu çelişkiyi Stripe Connect modeliyle kapattı. Oracle Java'yı `java_sonraki`'de olduğu gibi kaydeder; Go `go_beklenen`'e göre koşar.

| Java durumu | Nasıl ulaşılır (Java) | K2 / Go durumu |
|---|---|---|
| `PENDING` | emanet çıkışı | `PENDING` |
| `COMPLETED` | APPROVE başarılı (PO-03, PO-04) | `IN_TRANSIT` |
| `PROCESSING` | APPROVE başarılı, referans yok (PO-05; pratikte ulaşılamaz) | `IN_TRANSIT` |
| `COMPLETED` | `PROCESSING`'den ekstre mutabakatı (PO-07) | `PAID` (`IN_TRANSIT`'ten) |
| `FAILED` | APPROVE 8 denemede başarısız (PO-06) | `FAILED` |
| — | yok | `RETURNED` (`PAID`'den sonra iade, PO-16) |
| — | yok | `CANCELED` (PO-17; kaynak durumlar Faz 6'da) |

`chk_payouts_completed_requires_evidence` kısıtı Go'da `PAID`'e taşınır (PO-14). Iyzico'da `PAID`'i hangi raporun kanıtladığı sandbox'ta doğrulanacak.

## K5: tutar ve oran

- Go'da tutar int64 kuruş, oran baz puandır (ADR-0008). Tablolardaki `_kurus` ve `_bps` kolonları bu temsildir.
- Java girdisi kuruş altı (`100.005`) ya da oranı 4'ten fazla ondalık basamaklı (`0.33333`) olan satırlar Go'da ifade edilemez: `go_uygulanabilir = hayir` (CM-18, CM-19, CF-18, CF-20). Java bu girdileri önce yuvarlıyor; Go'da bu yuvarlama sınırda (API ve Iyzico dönüşümü) olur.
- Oranı 4 basamağa kadar olan her satırda `komisyon_kurus = HALF_UP(pay_kurus × bps / 10000)` Java'nın `round(round(pay, 2) × oran, 2)` sonucuyla aynıdır. Bu yüzden `go_uygulanabilir = evet` satırlarda kuruş kolonları doğrudan beklentidir.

## Karar kaydına bağlı Go farkları

| Kaynak | Java | Go |
|---|---|---|
| D4 (AT-09, AT-14) | Uyuşmazlık açıkken işveren tamamlaması atamayı COMPLETED yapar (para emanette); otomatik tamamlama 409 atıp geri alınır | İkisi de reddedilir / atlanır; atama durumu değişmez |
| Java hatası (AT-15b, CF-05, CF-14) | İş başladıktan sonra işveren iptalinde iade 0, `refundEmployer` çağrılmaz, para emanette kalır | Ceza işçiye geçer (`APPROVE`), emanet kapanır |
| Java hatası (CF-10) | Yuvarlama iadeyi 0 yapınca aynı durum | `karar_bekliyor`: pencere içi cezanın alıcısı karar verilmedi |
| Java hatası (envanter AZ-004/005) | Ödeme oturumunda durum kontrolü yetkiden önce | Yetki önce |

## Okurken çıkan bulgular (Java'da)

1. Uyuşmazlık `RELEASE_TO_WORKER` ile çözülürken atama COMPLETED değilse çözüm geri alınır (AT-20).
2. Uyuşmazlık `REFUND_EMPLOYER` atamayı iptal etmez (AT-21).
3. `SettlementRetryTest#aSuccessfulDisapprovalClosesTheRefundInsteadOfLeavingItPending` adı yanıltıcı: görev REFUND (RF-03).
4. Check-in tekrar oynatması (`clientEventId`) durum kontrolünden önce çalışır; atama artık aktif değilse de kayıtlı oturum döner (AT-07b).
5. Iyzico istek gövdelerinin çoğu `HashMap`'ten serileşiyor (`IyzicoPaymentService.java:56, :100, :151, :205, :234, :258`); bayt-eşit kayıt-oynatma için Java'nın gerçek alan sırası kaydedilmeli (§6.2-3).

## Java ile doğrulanacak tablolar (ayrı iş, CI'da)

Yerelde ağır iş yapılmaz (CLAUDE.md). Plan §6.2-2'deki küçük JUnit üreticisi Java deposunun CI'ında ya da ayrı bir işte koşmalı:

| Tablo | Ne koşturulacak |
|---|---|
| `reconcile.csv` | `PaymentManager.reconcile`'ı mock'lu sağlayıcı sonucuyla tam çarpımda koştur; bütün çıktı kolonlarını kaydet (14 `kod`, 24 `kısmi`). |
| `commission.csv`, `cancellation-fee.csv` | `CommissionCalculator` / `CancellationPolicy`'yi tablodaki girdiler ve rastgele HALF_UP sınırlarıyla koştur (29 `hesap`). |
| `backoff.csv` | `verificationBackoffFor` ve `backoffFor` için n = 1…8 (11 `hesap`). |
| `escrow-transitions.csv`, `escrow-predicates.csv` | 30 çift ve yüklemler (28 `kod`). |
| `assignment-transitions.csv` | AT-04, AT-05, AT-06, AT-06b, AT-07, AT-07b, AT-07c, AT-11, AT-13, AT-14, AT-15b, AT-16, AT-16b, AT-20 için durum kolonlarını iddia eden manager testi (14 `kod`). |
| `payout-transitions.csv` | PO-02, PO-05, PO-12, RF-01b, RF-02, ST-01 (6 `kod`). |
| `../iyzico/vectors.json` | Sabit `randomKey` ile LOOPBACK modunda ham istek gövdeleri; `stripTrailingZeros` örnekleri. |

Üretici hazır olunca CSV'ler onun çıktısıyla değiştirilir ve `dogrulama` `java-uretildi` olur.
