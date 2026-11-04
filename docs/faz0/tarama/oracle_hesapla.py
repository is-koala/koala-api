#!/usr/bin/env python3
"""test/oracle/{commission,cancellation-fee,backoff,escrow-transitions,escrow-predicates}.csv üreticisi.

Java ÇALIŞTIRILMAZ. Satırlar iki kaynaktan gelir:
  - "test": değer bir Java testinin beklenen değeridir (testte aynen yazılı).
  - "doğrulanmadı (koddan hesaplandı)": değer, Java kodundaki formülün
    (CommissionCalculator.java:25-35, CancellationPolicy.java:35-61,
    PaymentManager.java:485-490, SettlementManager.java:275-280) Python Decimal ile
    birebir yeniden uygulanmasıyla hesaplandı. Java'da koşturularak doğrulanmalı
    (test/oracle/README.md, "Java ile doğrulanacak tablolar").

Kullanım: python3 -I oracle_hesapla.py <test/oracle dizini>
"""
import csv
import os
import sys
from decimal import Decimal, ROUND_HALF_UP

OUT = sys.argv[1]
C2 = Decimal("0.01")
C4 = Decimal("0.0001")


def q2(x):
    return x.quantize(C2, rounding=ROUND_HALF_UP)


def bps(rate):
    v = rate * 10000
    return str(int(v)) if v == v.to_integral_value() else f"— ({v} bps tam sayı değil)"


def kurus(x):
    return str(int(x * 100))


# ---------------- komisyon ----------------
# (girdi işçi payı, oran, kaynak, doğrulama notu)
COMMISSION = [
    ("1000.00", "0.075", "CommissionCalculatorTest#theBreakdownAddsUpToTheChargedTotal", "test"),
    ("0.05", "0.10", "CommissionCalculatorTest#halfKurusRoundsUpNotDown", "test"),
    ("500", "0.12", "CommissionCalculatorTest#aDifferentRateProducesADifferentCommission", "test (yalnız komisyon 60.00 ve oran; pay/toplam koddan)"),
    ("500", "0.075", "CommissionCalculatorTest#theAppliedRateIsReportedSoItCanBeStored", "test (yalnız oran; tutarlar koddan)"),
    ("0.30", "0.075", "CommissionCalculatorTest#floatingPointDriftIsGone (0.10 + 0.20)", "test (yalnız pay 0.30; komisyon/toplam koddan)"),
    ("0.01", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek=2 ve toplam=pay+komisyon; değerler koddan)"),
    ("3.33", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek ve toplam; değerler koddan)"),
    ("99.99", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek ve toplam; değerler koddan)"),
    ("1234.56", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek ve toplam; değerler koddan)"),
    ("7777.77", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek ve toplam; değerler koddan)"),
    ("100000.05", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "test (yalnız ölçek ve toplam; değerler koddan)"),
    # HALF_UP sınırları (x.xx5) — koddan
    ("1.00", "0.125", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı)"),
    ("6.20", "0.075", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı)"),
    ("2.20", "0.075", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı)"),
    ("0.07", "0.075", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı)"),
    ("0.06", "0.075", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı; komisyon 0,00)"),
    ("0.01", "0.10", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı; 0,001 → 0,00)"),
    # girdi ölçeği > 2 — önce pay yuvarlanır
    ("100.005", "0.075", "CommissionCalculator.java:28", "doğrulanmadı (koddan hesaplandı; pay önce HALF_UP)"),
    ("100.004", "0.075", "CommissionCalculator.java:28", "doğrulanmadı (koddan hesaplandı)"),
    ("1075.00", "0.0755", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı; 4 basamaklı oran)"),
    ("99999999.99", "0.075", "CommissionCalculator.java:28-30", "doğrulanmadı (koddan hesaplandı; NUMERIC(19,2) içinde büyük tutar)"),
]
COMMISSION_REJECT = [
    ("0", "CommissionCalculatorTest#nonPositiveAmountsAreRejected"),
    ("-1", "CommissionCalculatorTest#nonPositiveAmountsAreRejected"),
    ("null", "CommissionCalculatorTest#nonPositiveAmountsAreRejected"),
]


def commission():
    rows = []
    n = 0
    for amount, rate, src, note in COMMISSION:
        n += 1
        a, r = Decimal(amount), Decimal(rate)
        payout = q2(a)
        comm = q2(payout * r)
        total = payout + comm
        rows.append(dict(id=f"CM-{n:02d}", girdi_isci_payi=amount, oran=rate, oran_bps=bps(r),
                         sonuc_isci_payi=str(payout), sonuc_komisyon=str(comm), sonuc_toplam=str(total),
                         isci_payi_kurus=kurus(payout), komisyon_kurus=kurus(comm), toplam_kurus=kurus(total),
                         hata="", kaynak=src, dogrulama=note))
    for amount, src in COMMISSION_REJECT:
        n += 1
        rows.append(dict(id=f"CM-{n:02d}", girdi_isci_payi=amount, oran="0.075", oran_bps="750",
                         sonuc_isci_payi="", sonuc_komisyon="", sonuc_toplam="",
                         isci_payi_kurus="", komisyon_kurus="", toplam_kurus="",
                         hata="ValidationException {job.salary.min.value} → JOB_SALARY_MIN_VALUE", kaynak=src, dogrulama="test"))
    return rows


# ---------------- iptal cezası ----------------
# (taraf, tahsilat (None = emanette para yok), bildirim (sn), serbest pencere sa, geç oran, kaynak, doğrulama)
H = 3600
CANCEL = [
    ("EMPLOYER", "1075.00", 48 * H, 24, "0.50", "CancellationPolicyTest#anEmployerCancellingWellAheadPaysNoFee", "test"),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.50", "CancellationPolicyTest#anEmployerCancellingInsideTheWindowPaysHalf", "test"),
    ("EMPLOYER", "1075.00", 2 * H, 24, "0.50", "AssignmentOperationsAuthorizationTest#aLateEmployerCancellationRefundsOnlyThePartLeftAfterTheFee", "test (ceza ve iade 537.50)"),
    ("EMPLOYER", "1075.00", 24 * H, 24, "0.50", "CancellationPolicyTest#exactlyAtTheWindowBoundaryTheCancellationIsStillFree", "test (yalnız ceza 0)"),
    ("EMPLOYER", "1075.00", -10 * 60, 24, "0.50", "CancellationPolicyTest#anEmployerCancellingAfterTheStartForfeitsEverything", "test"),
    ("WORKER", "1075.00", 48 * H, 24, "0.50", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "test"),
    ("WORKER", "1075.00", 30 * 60, 24, "0.50", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "test"),
    ("WORKER", "1075.00", -5 * H, 24, "0.50", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "test"),
    ("EMPLOYER", None, 5 * 60, 24, "0.50", "CancellationPolicyTest#whenNothingWasCollectedThereIsNothingToRefund", "test (ceza ve iade 0; oran koddan 0.0000)"),
    ("EMPLOYER", "0.01", 2 * H, 24, "0.50", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "test (yalnız ceza+iade=tahsilat; değerler koddan)"),
    ("EMPLOYER", "33.33", 2 * H, 24, "0.50", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "test (yalnız toplam; değerler koddan)"),
    ("EMPLOYER", "99999.99", 2 * H, 24, "0.50", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "test (yalnız toplam; değerler koddan)"),
    # sınırlar — koddan
    ("EMPLOYER", "1075.00", 24 * H - 1, 24, "0.50", "CancellationPolicy.java:53 (toHours kesirli saati atar)", "doğrulanmadı (koddan hesaplandı; 23 sa 59 dk 59 sn → 23 → geç)"),
    ("EMPLOYER", "1075.00", 0, 24, "0.50", "CancellationPolicy.java:50 (!isPositive)", "doğrulanmadı (koddan hesaplandı; tam başlangıç anı → %100)"),
    ("EMPLOYER", "1075.00", 1, 24, "0.50", "CancellationPolicy.java:50-56", "doğrulanmadı (koddan hesaplandı; 1 sn kala → geç oran)"),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.25", "CancellationPolicy.java:56; SettingKeys.CANCELLATION_LATE_FEE_RATE", "doğrulanmadı (koddan hesaplandı; ayardan oran 0.25)"),
    ("EMPLOYER", "1075.00", 30 * H, 48, "0.50", "CancellationPolicy.java:53; SettingKeys.CANCELLATION_FREE_WINDOW_HOURS", "doğrulanmadı (koddan hesaplandı; pencere 48 sa)"),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.33333", "CancellationPolicy.java:56 (oran 4 basamağa HALF_UP)", "doğrulanmadı (koddan hesaplandı; 0.33333 → 0.3333)"),
    ("ADMIN", "1075.00", 3 * H, 24, "0.50", "CancellationPolicy.java:47 (party != EMPLOYER)", "doğrulanmadı (koddan hesaplandı; ADMIN tarafı cezasız)"),
    ("EMPLOYER", "100.005", 3 * H, 24, "0.50", "CancellationPolicy.java:37-39 (tahsilat önce 2 basamağa HALF_UP)", "doğrulanmadı (koddan hesaplandı)"),
    ("EMPLOYER", "0.00", 3 * H, 24, "0.50", "CancellationPolicy.java:41", "doğrulanmadı (koddan hesaplandı)"),
]


def cancellation():
    rows = []
    for n, (party, coll, sec, window, late, src, note) in enumerate(CANCEL, start=1):
        collected = Decimal("0") if coll is None else q2(Decimal(coll))
        if collected <= 0:
            rate, fee, refund = Decimal("0.0000"), Decimal("0.00"), Decimal("0.00")
        else:
            if party != "EMPLOYER":
                rate = Decimal("0.0000")
            elif sec <= 0:
                rate = Decimal("1.0000")
            elif int(sec // H) >= window:  # Duration.toHours() pozitifte kesme
                rate = Decimal("0.0000")
            else:
                rate = Decimal(late).quantize(C4, rounding=ROUND_HALF_UP)
            fee = q2(collected * rate)
            refund = collected - fee
        if sec % H == 0:
            notice = f"{sec // H} sa"
        elif abs(sec) % 60 == 0:
            notice = f"{sec // 60} dk"
        else:
            notice = f"{sec} sn"
        refund_action = ("iade yok" if refund <= 0 else
                         ("REFUND (tam)" if refund == collected else "REFUND + DISAPPROVE (kısmi)"))
        if coll is None:
            refund_action = "iade yok (emanette para yok)"
        rows.append(dict(id=f"CF-{n:02d}", taraf=party, tahsilat="null" if coll is None else coll,
                         bildirim_suresi=notice, bildirim_saniye=str(sec), serbest_pencere_saat=str(window),
                         gec_iptal_orani=late, sonuc_ceza_orani=str(rate), sonuc_ceza=str(fee), sonuc_iade=str(refund),
                         sonuc_ceza_kurus=kurus(fee), sonuc_iade_kurus=kurus(refund), saglayici_islemi=refund_action,
                         kaynak=src, dogrulama=note))
    return rows


# ---------------- geri çekilme ----------------
def backoff():
    rows = []
    for kind, src in (("odeme_dogrulama", "PaymentManager.java:455-470; :485-490; PaymentVerificationProperties (1 dk, 6 sa, 8)"),
                      ("settlement_gorevi", "SettlementManager.java:150-162; :275-280; SettlementRetryProperties (1 dk, 6 sa, 8)")):
        for attempts in range(1, 9):
            mins = min(1 * (1 << min(attempts - 1, 20)), 360)
            if attempts >= 8:
                nxt = "null (tavan: ödeme → manuel inceleme; settlement → görev FAILED)"
            else:
                nxt = f"şimdi + {mins} dk"
            note = "doğrulanmadı (koddan hesaplandı)"
            if kind == "settlement_gorevi" and attempts == 8:
                note = "test (SettlementRetryTest#theTaskGivesUpAfterTheAttemptLimit)"
            if kind == "settlement_gorevi" and attempts in (1, 2, 5):
                note = "test (SettlementRetryTest#theBackoffGrowsButIsCapped: backoffFor(n))"
            if kind == "odeme_dogrulama" and attempts == 8:
                note = "test (PaymentCallbackTest#automaticReconciliationStopsAfterTheConfiguredBoundButRemainsActiveForManualReview, maxAttempts=2 ile)"
            rows.append(dict(id=f"BO-{len(rows) + 1:02d}", tur=kind, basarisiz_deneme_sayisi=str(attempts),
                             sonraki_deneme=nxt, kaynak=src, dogrulama=note))
    rows.append(dict(id=f"BO-{len(rows) + 1:02d}", tur="formul (maxAttempts > 8 yapılandırılırsa)",
                     basarisiz_deneme_sayisi="20", sonraki_deneme="şimdi + 360 dk (tavan 6 sa)",
                     kaynak="SettlementManager.java:275-280",
                     dogrulama="test (SettlementRetryTest#theBackoffGrowsButIsCapped: backoffFor(20) = 6 sa)"))
    return rows


def write(name, rows):
    with open(os.path.join(OUT, name), "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), lineterminator="\n")
        w.writeheader()
        w.writerows(rows)
    print(name, len(rows), file=sys.stderr)




# ---------------- emanet (atama ödeme durumu) geçişleri ----------------
ALLOWED = {
    "PENDING_COLLECTION": {"HELD_IN_ESCROW", "FAILED"},
    "FAILED": {"HELD_IN_ESCROW", "FAILED"},
    "HELD_IN_ESCROW": {"RELEASED_TO_WORKER", "REFUNDED"},
    "RELEASED_TO_WORKER": set(),
    "REFUNDED": set(),
}
TESTED = {
    ("PENDING_COLLECTION", "HELD_IN_ESCROW"): "PaymentStateMachineTest#moneyOnlyMovesForward; #aLegalTransitionIsApplied",
    ("HELD_IN_ESCROW", "REFUNDED"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("REFUNDED", "HELD_IN_ESCROW"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("HELD_IN_ESCROW", "PENDING_COLLECTION"): "PaymentStateMachineTest#moneyOnlyMovesForward; #anIllegalTransitionThrowsInsteadOfSilentlyApplying",
    ("REFUNDED", "PENDING_COLLECTION"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("HELD_IN_ESCROW", "HELD_IN_ESCROW"): "PaymentStateMachineTest#anAlreadyPaidAssignmentCannotBeChargedAgain",
}
TRIGGER = {
    (None, "PENDING_COLLECTION"): "işçi teklifi kabul eder (JobAssignmentManager.java:209-211, durum doğrudan yazılır)",
    ("PENDING_COLLECTION", "HELD_IN_ESCROW"): "ödeme doğrulanır: reconcile → settle (PaymentManager.java:492-514)",
    ("FAILED", "HELD_IN_ESCROW"): "başarısız denemeden sonra yeni ödeme doğrulanır (settle)",
    ("PENDING_COLLECTION", "FAILED"): "deneme FAILED: paymentStatus=FAILURE ya da fraud=-1 (PaymentManager.java:516-529)",
    ("FAILED", "FAILED"): "ikinci deneme de FAILED (canTransition FAILED→FAILED izinli)",
    ("HELD_IN_ESCROW", "RELEASED_TO_WORKER"): "emanet çıkışı: tamamlama, otomatik tamamlama, uyuşmazlık kararı, personel (PayoutManager.java:150)",
    ("HELD_IN_ESCROW", "REFUNDED"): "iade: iptal (iade > 0), uyuşmazlık REFUND_EMPLOYER, personel (PayoutManager.java:206)",
}


def escrow():
    states = ["null", "PENDING_COLLECTION", "FAILED", "HELD_IN_ESCROW", "RELEASED_TO_WORKER", "REFUNDED"]
    targets = ["PENDING_COLLECTION", "FAILED", "HELD_IN_ESCROW", "RELEASED_TO_WORKER", "REFUNDED"]
    rows = []
    for fr in states:
        for to in targets:
            frk = None if fr == "null" else fr
            ok = (to == "PENDING_COLLECTION") if frk is None else (to in ALLOWED[frk])
            src = TESTED.get((frk, to))
            rows.append(dict(
                id=f"ES-{len(rows) + 1:02d}", onceki=fr, sonraki=to, izinli="evet" if ok else "hayır",
                ret_hatasi="" if ok else "ConflictException {payment.invalid.status.transition} → PAYMENT_INVALID_STATUS_TRANSITION",
                tetikleyen=TRIGGER.get((frk, to), ""),
                kaynak="PaymentStateMachine.java:15-20; :37-42" + (f"; {src}" if src else ""),
                dogrulama="test" if src else "doğrulanmadı (yalnız kod; tablo okundu)"))
    return rows


def escrow_predicates():
    rows = []
    preds = [
        ("acceptsPayment", "CONFIRMED", "PENDING_COLLECTION", "evet", "PaymentStateMachineTest#onlyAConfirmedAssignmentAwaitingCollectionCanStartAPayment"),
        ("acceptsPayment", "CONFIRMED", "FAILED", "evet", "PaymentStateMachineTest#onlyAConfirmedAssignmentAwaitingCollectionCanStartAPayment"),
        ("acceptsPayment", "PENDING_CONFIRMATION", "PENDING_COLLECTION", "hayır", "PaymentStateMachineTest#anUnconfirmedAssignmentCannotStartAPayment"),
        ("acceptsPayment", "REJECTED", "PENDING_COLLECTION", "hayır", "PaymentStateMachineTest#anUnconfirmedAssignmentCannotStartAPayment"),
        ("acceptsPayment", "CONFIRMED", "HELD_IN_ESCROW", "hayır", "PaymentStateMachineTest#anAlreadyPaidAssignmentCannotBeChargedAgain"),
        ("acceptsPayment", "IN_PROGRESS", "*", "hayır", ""),
        ("acceptsPayment", "CANCELLED", "*", "hayır", "PaymentCallbackTest#aSuccessfulProviderResultForACancelledAssignmentRequiresManualReview"),
        ("canRelease", "COMPLETED", "HELD_IN_ESCROW", "evet", "PayoutReleaseTest#aCompletedAssignmentInEscrowCanBeReleased"),
        ("canRelease", "CONFIRMED | IN_PROGRESS", "HELD_IN_ESCROW", "hayır", "PayoutReleaseTest#moneyIsNotReleasedBeforeTheWorkIsCompleted"),
        ("canRelease", "COMPLETED", "PENDING_COLLECTION | FAILED", "hayır", "PayoutReleaseTest#releasingWithoutASettledPaymentIsRejected"),
        ("canRefund", "*", "HELD_IN_ESCROW", "evet", "PayoutReleaseTest#escrowedMoneyCanBeRefundedToTheEmployer"),
        ("canRefund", "*", "RELEASED_TO_WORKER", "hayır", "PayoutReleaseTest#alreadyReleasedMoneyCannotBeRefunded"),
        ("canRefund", "*", "PENDING_COLLECTION | FAILED | REFUNDED", "hayır", ""),
    ]
    for name, a, p, res, src in preds:
        rows.append(dict(id=f"EP-{len(rows) + 1:02d}", yuklem=name, atama_durumu=a, odeme_durumu=p, sonuc=res,
                         kaynak="PaymentStateMachine.java:22-35" + (f"; {src}" if src else ""),
                         dogrulama="test" if src else "doğrulanmadı (yalnız kod)"))
    return rows


if __name__ == "__main__":
    write("commission.csv", commission())
    write("cancellation-fee.csv", cancellation())
    write("backoff.csv", backoff())
    write("escrow-transitions.csv", escrow())
    write("escrow-predicates.csv", escrow_predicates())
