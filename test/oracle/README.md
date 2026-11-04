# Oracle: Java'dan çıkarılmış karar tabloları

Plan §6.2-2 ve §6.3. Go'nun para ve durum kuralları bu tabloları satır satır üretmek zorunda. Tablo-güdümlü bir Go testi CSV'yi okur, her satırın girdisini Go fonksiyonuna verir ve çıktıyı karşılaştırır.

**Java çalıştırılmadı.** Satırlar şu yollardan biriyle çıkarıldı:

- Java kodu okundu: `origin/main` anlık görüntüsü; satır numaraları ona aittir.
- Mevcut Java testlerinin beklenen değerleri okundu.
- Formül Python `Decimal` ile yeniden uygulandı.

Her satırın `kaynak_kod` / `kaynak_test` ya da `kaynak` kolonunda dosya:satır ya da `TestSınıfı#metot` var.

## `dogrulama` kolonu

| Değer | Anlam |
|---|---|
| `test` | Satırdaki çıktı, adı geçen Java testinin iddiasıyla birebir aynı. |
| `test (yalnız …)` / `test (… koddan)` | **Kısmi.** Test satırın bir kısmını bağlıyor (ör. yalnız `ceza + iade = tahsilat`). Kalan değerler koddan hesaplandı. |
| `doğrulanmadı (yalnız kod)` / `doğrulanmadı (koddan hesaplandı)` | Hiçbir Java testi bu satırı bağlamıyor. Değer kod okunarak ya da formül yeniden uygulanarak bulundu. |
| `karar (K2); Java'da yok` | Satır Java davranışı değil, karar kaydından gelen hedef durum. |

## Tablolar

| Tablo | Satır | test | kısmi | doğrulanmadı | karar |
|---|---|---|---|---|---|
| [`reconcile.csv`](reconcile.csv): ödeme sonucu karar zinciri | 38 | 24 | 0 | 14 | 0 |
| [`commission.csv`](commission.csv): komisyon / toplam | 24 | 5 | 9 | 10 | 0 |
| [`cancellation-fee.csv`](cancellation-fee.csv): iptal cezası | 21 | 7 | 5 | 9 | 0 |
| [`escrow-transitions.csv`](escrow-transitions.csv): atama ödeme durumu (emanet) FSM, 6×5 tam matris | 30 | 6 | 0 | 24 | 0 |
| [`escrow-predicates.csv`](escrow-predicates.csv): `acceptsPayment` / `canRelease` / `canRefund` | 13 | 11 | 0 | 2 | 0 |
| [`assignment-transitions.csv`](assignment-transitions.csv): atama durum geçişleri | 21 | 13 | 2 | 6 | 0 |
| [`payout-transitions.csv`](payout-transitions.csv): payout, iade, settlement görevi + K2 eşlemesi | 28 | 24 | 0 | 2 | 2 |
| [`backoff.csv`](backoff.csv): doğrulama ve settlement geri çekilmesi | 17 | 6 | 0 | 11 | 0 |
| **Toplam** | **192** | **96** | **16** | **78** | **2** |
| [`iyzico-vectors.json`](iyzico-vectors.json): IYZWSv2 ve yanıt imzası | 5 Java vektörü + 2 türetilmiş istek vektörü + 5 normalizasyon örneği | | | | |

### Notlar

- **`reconcile.csv` zinciri sıralıdır** (`PaymentManager.java:279-360`). İlk eşleşen dal kazanır. `*` "önemsiz" demektir.
  - Satırlar tam kartezyen çarpım değildir. Her dalın ilk eşleştiği durumu ve öncelik çakışmalarını gösterir.
  - Öncelik örneği RC-27: 3DS, fraud'dan önce gelir.
  - Ondalık karşılaştırması `compareTo` ile yapılır, `1075 = 1075.00` (RC-24).
  - Plan §6.2'deki "paymentStatus × fraudStatus × mdStatus × snapshot × atama" çarpımı Java'da koşturularak üretilmeli (aşağıda).
- **`commission.csv` ve `cancellation-fee.csv` K5 için kuruş kolonları taşır.**
  - `commission.csv` ayrıca `oran_bps` kolonu taşır (1250 = %12,5).
  - Java oranı `BigDecimal` olarak tutar. Oranın ondalık basamağı ≤ 4 ise baz puan (bps) karşılığı kayıpsızdır, ve `komisyon_kurus = HALF_UP(pay_kurus × bps / 10000)` Java ile aynı sonucu verir. 4'ten fazla basamaklı oran bps'e sığmaz; tabloda bu yüzden işaretli.
  - Varsayılanlar `CommissionProperties.rate = 0.075` ve `CancellationProperties` (24 sa, 0.50, Europe/Istanbul). Çalışma zamanı ayarı (`SettingKeys.*`) bunları ezer.
- **`cancellation-fee.csv` yalnız politikadır.** `saglayici_islemi` kolonu tahsilatın emanette olduğunu varsayar (`CancellationManager.java:102-122`).

## K2: payout COMPLETED çelişkisi

| Kaynak | Ne diyor |
|---|---|
| CLAUDE.md §2y / §2y3 (Java repo) | `PayoutStatus.COMPLETED`'a giden tek yol banka/ekstre mutabakatıdır. |
| `SettlementManager.java:199-219` (kod) | APPROVE başarılıysa ve referans varsa payout **doğrudan COMPLETED** olur. Javadoc'taki gerekçe: "APPROVE yanıtı tek elimizdeki gerçekleşme kanıtı". Referans `firstNonBlank(yanıt kimliği, kuyruk işlem kimliği)` olduğundan, ve kuyruk işlem kimliği boş olamadığından (`SettlementManager.java:73-77`), PROCESSING dalı pratikte ulaşılamaz (PO-05). |
| Testler | `SettlementRetryTest#aSuccessfulRunClosesTheTaskAndSettlesThePayoutFromTheProviderApproval` ve `#anApprovalWithoutAResponseIdStillSettlesFromTheQueuedTransaction` kodun davranışını bağlıyor. |
| **K2 (karar kaydı)** | Stripe Connect modeli: `PENDING → IN_TRANSIT` (Iyzico APPROVE) `→ PAID` (banka ekstresi mutabakatı). Ek durumlar: `FAILED`, `RETURNED` (PAID'den sonra iade), `CANCELED`. |

Oracle **Java davranışını olduğu gibi** kaydeder (`payout-transitions.csv`, `sonraki_durum_java`). K2 hedefi `k2_hedef_durum` ve `k2_farki` kolonlarında. Go'nun bu tablodaki eşitlik testi:

- `PO-03`, `PO-04`, `PO-07` satırlarında `k2_hedef_durum`'a göre koşar, `sonraki_durum_java`'ya göre değil. Bu fark bilinçlidir.
- `PO-16`, `PO-17` satırları Java'da olmayan K2 durumlarıdır.
- Kalan satırlar Java ile birebir aynı olmalıdır.
- Iyzico'da PAID'i hangi raporun kanıtladığı sandbox'ta doğrulanacak (K2).

## Okurken çıkan bulgular (Java'da, doğrulanmadı)

1. **İş başladıktan sonra işveren iptal ederse emanet açık kalıyor.** Ceza %100 ve iade 0 olur. `CancellationManager.java:120` yalnız `iade > 0` ise `refundEmployer` çağırır. Atama `CANCELLED` olur ama ödeme durumu `HELD_IN_ESCROW` kalır. Paranın kime ve nasıl çıkacağı kodda yok (AT-15, CF-05, CF-14).
2. **Otomatik tamamlama, açık uyuşmazlıkta işveren tamamlamasından farklı davranıyor.**
   - Manuel tamamlama atamayı `COMPLETED` yapar, para emanette kalır (AT-09).
   - Otomatik tamamlama `releaseToWorker` çağırır. Bu çağrı uyuşmazlıkta 409 atar ve aynı işlem geri alınır, atama `COMPLETED` olmaz (AT-14).
3. **Uyuşmazlık `RELEASE_TO_WORKER` ile çözülürken atama henüz COMPLETED değilse çözüm başarısız oluyor.** Sebep `canRelease` koşulu; tüm işlem geri alınır (AT-20).
4. **Uyuşmazlık `REFUND_EMPLOYER` atamayı iptal etmiyor.** Atama durumu korunur, ödeme durumu `REFUNDED` olur (AT-21).
5. **`aSuccessfulDisapprovalClosesTheRefundInsteadOfLeavingItPending` testinin adı yanıltıcı.** Test bir REFUND görevi çalıştırıyor. DISAPPROVE iade kaydına hiç dokunmaz (RF-03, RF-05).
6. **Iyzico istek gövdelerinin çoğu `HashMap`'ten serileşiyor.** Retrieve, approve/disapprove, refund, cancel ve alt üye oluşturma/güncelleme gövdeleri böyle (`IyzicoPaymentService.java:56, :100, :151, :205, :234, :258`). Alan sırası ekleme sırası değil. Checkout initialize gövdesi DTO'dan serileşiyor.
   - Bu imza doğruluğunu etkilemez, çünkü imza gönderilen baytların üzerindedir.
   - Ama §6.2-3'teki "bayt bayt eşit gövde" kayıt-oynatma testi Java'nın gerçek sırasını kaydetmeden kurulamaz.

## Java ile doğrulanacak tablolar (ayrı iş)

Bu adım **ağır iş** sayılır: mvn, JUnit, Testcontainers. Mac'te ortak kilitle (`/tmp/claude-agir-is-slotu`) ya da CI'da tek seferlik koşmalı. Plan §6.2-2'deki "küçük JUnit üreticisi" şunları yapmalı:

| Tablo | Ne koşturulacak | Neden |
|---|---|---|
| `reconcile.csv` | `PaymentManager.reconcile`'ı mock'lu sağlayıcı sonucuyla tam çarpım üzerinde koştur: paymentStatus × fraudStatus × mdStatus × 10 snapshot alanı × 5 kimlik alanı × atama durumu. Ayrıca `dogrulama_sayaci` tavanını koştur. | 14 satır yalnız kod. Zincirin tam çarpımı hiç üretilmedi. |
| `commission.csv` | `CommissionCalculator.calculate`'i tablodaki girdilerle ve rastgele HALF_UP sınırlarıyla (`x.xx5`) koştur. | 10 satır koddan hesaplandı, 9 satırda test yalnız toplamı bağlıyor. |
| `cancellation-fee.csv` | `CancellationPolicy.evaluate` + `noticeBefore`'u ayar varyasyonlarıyla (pencere, oran) ve saniye sınırlarıyla koştur. | 9 satır koddan, 5 satır kısmi. |
| `backoff.csv` | `PaymentManager.verificationBackoffFor` ve `SettlementManager.backoffFor` için n = 1…8 değerlerini al. | 11 satır koddan. |
| `escrow-transitions.csv` | `PaymentStateMachine.canTransition`'ı 30 çiftin hepsinde koştur. | 24 satırda test yok; tablo okundu. |
| `assignment-transitions.csv` | AT-04, AT-06, AT-07, AT-13, AT-14, AT-20 için IT ya da manager testi yaz. | Davranış yalnız koddan okundu. AT-14 ve AT-20 yukarıdaki bulgulara bağlı. |
| `payout-transitions.csv` | PO-05 (ulaşılamaz dal) ve PO-12'yi koştur. | Kod okuması. |
| `iyzico-vectors.json` | Sabit `randomKey` ile LOOPBACK modunda ham istek gövdelerini kaydet (§6.2-3). `stripTrailingZeros` örneklerini koştur. | IR-01 ve IR-02 türetilmiş. Gövde alan sırası bilinmiyor (bulgu 6). |

## Yeniden üretme

`commission.csv`, `cancellation-fee.csv`, `backoff.csv`, `escrow-transitions.csv` ve `escrow-predicates.csv` şu betikle üretilir:

```sh
python3 -I docs/faz0/tarama/oracle_hesapla.py test/oracle
```

`reconcile.csv`, `assignment-transitions.csv` ve `payout-transitions.csv` kod okunarak elle yazıldı. Java'daki JUnit üreticisi hazır olunca hepsi onun çıktısıyla değiştirilmeli. O zaman `dogrulama` kolonu `java-üretildi` olur.
