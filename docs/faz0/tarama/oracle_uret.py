#!/usr/bin/env python3
"""testdata/oracle/*.csv üreticisi ve sözlük doğrulayıcısı.

Java ÇALIŞTIRILMAZ. Satırlar Java kodu ve Java testlerinin iddiaları okunarak yazıldı;
tutar tabloları Java formülünün Python Decimal ile birebir yeniden uygulanmasıyla
hesaplandı. Her satırın `dogrulama` değeri `test_kolonlari`'ndan hesaplanır:

  test   : testin iddia ettiği kolonlar satırın bütün (boş olmayan) çıktı kolonlarını kapsıyor
  kısmi  : test çıktı kolonlarının bir kısmını iddia ediyor (hangileri: test_kolonlari)
  kod    : test yok; değer Java kodu okunarak yazıldı (doğrulanmadı)
  hesap  : test yok; değer formülün Python ile yeniden uygulanmasından (doğrulanmadı)
  karar  : Java'da karşılığı yok; değer karar kaydından (K2)

Değer sözlüğü (testdata/oracle/README.md): `*` herhangi, `=` değişmez, boş = uygulanmaz,
`A|B` küme, `A+B` birlikte, `>=N` / `<N` / `<=N` karşılaştırma, `null`, `yok` (kayıt yok).
Açıklama yalnız `not` kolonundadır.

Kullanım (depo kökünden):  python3 -I docs/faz0/tarama/oracle_uret.py
"""
import csv
import os
import re
import sys
from decimal import Decimal, ROUND_HALF_UP

OUT = "testdata/oracle"
C2 = Decimal("0.01")
C4 = Decimal("0.0001")

M = "src/main/java/com/exskylab/koala/"
T = "src/test/java/com/exskylab/koala/"
PATHS = {
    "PaymentManager": M + "business/concretes/PaymentManager",
    "PaymentStateMachine": M + "core/helpers/PaymentStateMachine",
    "CommissionCalculator": M + "core/helpers/CommissionCalculator",
    "CancellationPolicy": M + "core/helpers/CancellationPolicy",
    "CancellationManager": M + "business/concretes/CancellationManager",
    "PayoutManager": M + "business/concretes/PayoutManager",
    "SettlementManager": M + "business/concretes/SettlementManager",
    "RefundManager": M + "business/concretes/RefundManager",
    "JobAssignmentManager": M + "business/concretes/JobAssignmentManager",
    "JobApplicationManager": M + "business/concretes/JobApplicationManager",
    "TimesheetManager": M + "business/concretes/TimesheetManager",
    "DisputeManager": M + "business/concretes/DisputeManager",
    "PaymentDao": M + "dataAccess/PaymentDao",
    "JobAssignmentDao": M + "dataAccess/JobAssignmentDao",
    "PaymentVerificationProperties": M + "core/properties/PaymentVerificationProperties",
    "SettlementRetryProperties": M + "core/properties/SettlementRetryProperties",
    "CommissionProperties": M + "core/properties/CommissionProperties",
    "CancellationProperties": M + "core/properties/CancellationProperties",
    "PaymentCallbackTest": T + "business/concretes/PaymentCallbackTest",
    "PaymentMonitoringTest": T + "business/concretes/PaymentMonitoringTest",
    "PaymentStateMachineTest": T + "core/helpers/PaymentStateMachineTest",
    "CommissionCalculatorTest": T + "core/helpers/CommissionCalculatorTest",
    "CancellationPolicyTest": T + "core/helpers/CancellationPolicyTest",
    "AssignmentOperationsAuthorizationTest": T + "business/concretes/AssignmentOperationsAuthorizationTest",
    "ApplicationDecisionIT": T + "webAPI/ApplicationDecisionIT",
    "AssignmentReadIT": T + "webAPI/AssignmentReadIT",
    "MessagingPaymentGateIT": T + "webAPI/MessagingPaymentGateIT",
    "AssignmentCompletionIT": T + "webAPI/AssignmentCompletionIT",
    "UnpaidExpiryRulesTest": T + "business/concretes/UnpaidExpiryRulesTest",
    "DisputeResolutionAuthorizationTest": T + "business/concretes/DisputeResolutionAuthorizationTest",
    "PayoutReleaseTest": T + "business/concretes/PayoutReleaseTest",
    "SettlementRetryTest": T + "business/concretes/SettlementRetryTest",
    "PayoutReconciliationIT": T + "webAPI/PayoutReconciliationIT",
    "PayoutSettlementReconciliationIT": T + "dataAccess/PayoutSettlementReconciliationIT",
    "TimesheetManagerOfflineEventTest": T + "business/concretes/TimesheetManagerOfflineEventTest",
}


def src(spec):
    """'PaymentManager:304-309' -> tam yol; 'PaymentCallbackTest#m' -> tam yol#m. ';' ile çoklu."""
    out = []
    for part in [p.strip() for p in spec.split(";") if p.strip()]:
        m = re.match(r"^(\w+)([:#].*)$", part)
        if not m or m.group(1) not in PATHS:
            out.append(part)
            continue
        out.append(PATHS[m.group(1)] + ".java" + m.group(2))
    return "; ".join(out)


def q2(x):
    return x.quantize(C2, rounding=ROUND_HALF_UP)


def kurus(x):
    return str(int(x * 100)) if x == x.quantize(C2) else ""


def bps(rate):
    v = rate * 10000
    return str(int(v)) if v == v.to_integral_value() else ""


def verdict(row, outputs, base):
    tested = {c for c in row.get("test_kolonlari", "").split("|") if c}
    if not tested:
        return base
    need = {c for c in outputs if row.get(c, "") != ""}  # boş (uygulanmaz) kolon iddia gerektirmez
    return "test" if need <= tested else "kısmi"


# ======================================================================
# reconcile.csv
# ======================================================================
RC_OUT = ["sonuc_deneme", "sonuc_failure_reason", "sonuc_next_verification", "sonuc_completed_at",
          "sonuc_atama_odeme", "sonuc_saglayici_kimlik", "sonuc_odendi_olayi", "sonuc_dogrulama_sayaci",
          "saglayici_sorgusu", "hata"]
RC_IN = ["adim", "olay", "on_deneme", "on_token", "on_yas_sn", "on_dogrulama_sayaci", "retrieve", "kimlik",
         "paymentStatus", "snapshot", "mdStatus", "fraudStatus", "atama_tahsil"]
AKTIF = "INITIATED|VERIFY_PENDING|IN_REVIEW"
VP, FR, TDS = "{payment.verification.pending}", "{payment.fraud.review}", "{payment.3ds.verification.failed}"
DEFER = dict(sonuc_deneme="VERIFY_PENDING", sonuc_failure_reason=VP, sonuc_next_verification="backoff",
             sonuc_completed_at="null", sonuc_atama_odeme="=", sonuc_saglayici_kimlik="=", sonuc_odendi_olayi="hayir",
             sonuc_dogrulama_sayaci="+1", saglayici_sorgusu="1", hata="")
BASE_IN = dict(adim="", olay="callback", on_deneme=AKTIF, on_token="var", on_yas_sn="*", on_dogrulama_sayaci="<7",
               retrieve="ok", kimlik="yok", paymentStatus="SUCCESS", snapshot="yok", mdStatus="1", fraudStatus="1",
               atama_tahsil="evet")


def rc(i, inp, out, kod, test="", cols="", note=""):
    r = dict(BASE_IN)
    r.update(inp)
    o = dict(DEFER)
    o.update(out)
    r.update(o)
    r.update(id=f"RC-{i:02d}", kaynak_kod=src(kod), kaynak_test=src(test), test_kolonlari=cols, not_=note)
    return r


def reconcile():
    S = "sonuc_deneme"
    A = "sonuc_atama_odeme"
    rows = [
        rc(1, dict(adim="0_giris", on_deneme="yok", on_token="*", retrieve="", kimlik="*", paymentStatus="*",
                   snapshot="*", mdStatus="*", fraudStatus="*", atama_tahsil="*", on_dogrulama_sayaci="*"),
           dict(sonuc_deneme="yok", sonuc_failure_reason="=", sonuc_next_verification="=", sonuc_completed_at="=",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0", hata="PAYMENT_NOT_FOUND"),
           "PaymentManager:185-195", "PaymentCallbackTest#anUnknownTokenIsRejected", "hata|saglayici_sorgusu",
           "Token'a ait deneme yok; bilinmeyen token metriği artar."),
        rc(2, dict(adim="0_giris", on_deneme="SUCCEEDED", retrieve="", kimlik="*", paymentStatus="*", snapshot="*",
                   mdStatus="*", fraudStatus="*", atama_tahsil="*", on_dogrulama_sayaci="*"),
           dict(sonuc_deneme="=", sonuc_failure_reason="=", sonuc_next_verification="=", sonuc_completed_at="=",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0"),
           "PaymentManager:197-204",
           "PaymentCallbackTest#aVerifiedPaymentThatAlreadySettledIsStillShortCircuited; PaymentCallbackTest#theSameCallbackArrivingTwiceIsProcessedOnlyOnce",
           f"{S}|{A}|saglayici_sorgusu", "Son durumdaki deneme sağlayıcıya sorulmaz."),
        rc(3, dict(adim="0_giris", on_deneme="FAILED", retrieve="", kimlik="*", paymentStatus="*", snapshot="*",
                   mdStatus="*", fraudStatus="*", atama_tahsil="*", on_dogrulama_sayaci="*"),
           dict(sonuc_deneme="=", sonuc_failure_reason="=", sonuc_next_verification="=", sonuc_completed_at="=",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0"),
           "PaymentManager:197-204", "PaymentCallbackTest#aTerminallyFailedAttemptCannotLaterOpenEscrow", f"{S}|{A}"),
        rc(4, dict(adim="1_retrieve", retrieve="hata", kimlik="*", paymentStatus="*", snapshot="*", mdStatus="*",
                   fraudStatus="*", atama_tahsil="*"), {},
           "PaymentManager:286-293", "PaymentCallbackTest#aProviderVerificationErrorRemainsActiveUntilReconciled",
           f"{S}|{A}|sonuc_dogrulama_sayaci|sonuc_next_verification"),
        rc(5, dict(adim="2_kimlik", kimlik="conversationId", paymentStatus="*", snapshot="*", mdStatus="*",
                   fraudStatus="*", atama_tahsil="*"), {},
           "PaymentManager:296-301; PaymentManager:407",
           "PaymentCallbackTest#aMismatchedProviderConversationStaysBlockedForReconciliation", f"{S}|{A}"),
        rc(6, dict(adim="2_kimlik", kimlik="conversationId", paymentStatus="FAILURE", snapshot="*", mdStatus="*",
                   fraudStatus="*", atama_tahsil="*"), {},
           "PaymentManager:296-301; PaymentManager:407",
           "PaymentCallbackTest#aDeclineForAnotherConversationCannotTerminallyFailThisAttempt", f"{S}|{A}",
           "Kimlik kontrolü paymentStatus'tan önce: başka denemenin FAILURE'ı bu denemeyi sonlandıramaz."),
        rc(7, dict(adim="2_kimlik", kimlik="token", paymentStatus="*", snapshot="*", mdStatus="*", fraudStatus="*",
                   atama_tahsil="*"), {}, "PaymentManager:408"),
        rc(8, dict(adim="2_kimlik", kimlik="basketId", paymentStatus="*", snapshot="*", mdStatus="*", fraudStatus="*",
                   atama_tahsil="*"), {}, "PaymentManager:409", note="basketId atama kimliğiyle karşılaştırılır."),
        rc(9, dict(adim="2_kimlik", kimlik="currency", paymentStatus="*", snapshot="*", mdStatus="*", fraudStatus="*",
                   atama_tahsil="*"), {}, "PaymentManager:410", "PaymentCallbackTest#aPaymentInAnotherCurrencyIsRefused", S,
           "Para birimi kimlik adımında (2) kontrol edilir, snapshot adımında değil."),
        rc(10, dict(adim="2_kimlik", kimlik="paymentId_bos", paymentStatus="*", snapshot="*", mdStatus="*",
                    fraudStatus="*", atama_tahsil="*"), {}, "PaymentManager:411"),
        rc(11, dict(adim="2_kimlik", on_deneme="IN_REVIEW", kimlik="paymentId_farkli"), {},
           "PaymentManager:412-413", "PaymentCallbackTest#providerIdentifiersCannotChangeAfterFraudReview",
           f"{S}|sonuc_saglayici_kimlik|{A}", "IN_REVIEW'da kaydedilmiş paymentId sonraki sorguda değişemez."),
        rc(12, dict(adim="3_paymentStatus", paymentStatus="FAILURE", snapshot="*", mdStatus="*", fraudStatus="*",
                    atama_tahsil="*"),
           dict(sonuc_deneme="FAILED", sonuc_failure_reason="paymentStatus=FAILURE", sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="FAILED", sonuc_dogrulama_sayaci="="),
           "PaymentManager:304-309; PaymentManager:516-529",
           "PaymentCallbackTest#aDeclinedPaymentIsRecordedAsFailedWithoutOpeningEscrow", f"{S}|{A}",
           "Atama PENDING_COLLECTION ya da FAILED iken FAILED olur (canTransition)."),
        rc(13, dict(adim="3_paymentStatus", paymentStatus="failure", snapshot="*", mdStatus="*", fraudStatus="*",
                    atama_tahsil="*"),
           dict(sonuc_deneme="FAILED", sonuc_failure_reason="paymentStatus=failure", sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="FAILED", sonuc_dogrulama_sayaci="="),
           "PaymentManager:304", note="equalsIgnoreCase."),
        rc(14, dict(adim="3_paymentStatus", paymentStatus="diger|null", snapshot="*", mdStatus="*", fraudStatus="*",
                    atama_tahsil="*"), {}, "PaymentManager:311-315"),
    ]
    snaps = [("price", "PaymentManager:419", "", ""), ("paidPrice", "PaymentManager:420",
             "PaymentCallbackTest#aShortPaymentIsRefusedInsteadOfSettled", S),
             ("itemCount", "PaymentManager:421", "", ""), ("itemId", "PaymentManager:422-423", "", ""),
             ("itemPrice", "PaymentManager:424", "", ""), ("itemPaidPrice", "PaymentManager:425", "", ""),
             ("subMerchantPrice", "PaymentManager:426", "", ""),
             ("subMerchantKey", "PaymentManager:427-429",
              "PaymentCallbackTest#aSuccessfulPaymentForAnotherSubmerchantNeverOpensEscrow", f"{S}|{A}"),
             ("paymentTransactionId", "PaymentManager:430-436", "", "")]
    i = 15
    for name, kod, test, cols in snaps:
        rows.append(rc(i, dict(adim="4_snapshot", snapshot=name, mdStatus="*", fraudStatus="*", atama_tahsil="*"), {},
                       "PaymentManager:317-322; " + kod, test, cols))
        i += 1
    rows += [
        rc(i, dict(adim="4_snapshot", snapshot="yok"),
           dict(sonuc_deneme="SUCCEEDED", sonuc_failure_reason="null", sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="HELD_IN_ESCROW", sonuc_saglayici_kimlik="yaz",
                sonuc_odendi_olayi="evet", sonuc_dogrulama_sayaci="="),
           "PaymentManager:440-442", note="Tutarlar compareTo ile karşılaştırılır: 1075 = 1075.00 eşleşme sayılır."),
        rc(i + 1, dict(adim="5_3ds", mdStatus="-1|0|2|3|4|5|6|7|8", fraudStatus="*", atama_tahsil="*"),
           dict(sonuc_failure_reason=TDS), "PaymentManager:325-331",
           "PaymentCallbackTest#everyMdStatusOtherThanOneRemainsPendingVerification", S),
        rc(i + 2, dict(adim="5_3ds", mdStatus="null", fraudStatus="*", atama_tahsil="*"),
           dict(sonuc_failure_reason=TDS), "PaymentManager:325-331",
           "PaymentCallbackTest#anAbsentMdStatusRemainsPendingRatherThanBeingAssumedVerified", f"{S}|sonuc_failure_reason"),
        rc(i + 3, dict(adim="5_3ds", mdStatus="0", fraudStatus="0", atama_tahsil="*"),
           dict(sonuc_failure_reason=TDS), "PaymentManager:325",
           "PaymentCallbackTest#aFailedVerificationOutranksTheFraudReviewHold", S, "3DS kontrolü fraud'dan önce."),
        rc(i + 4, dict(adim="6_fraud", fraudStatus="-1", atama_tahsil="*"),
           dict(sonuc_deneme="FAILED", sonuc_failure_reason=FR, sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="FAILED", sonuc_dogrulama_sayaci="="),
           "PaymentManager:333-337", "PaymentCallbackTest#aPaymentRejectedByFraudFiltersFails", S),
        rc(i + 5, dict(adim="6_fraud", fraudStatus="0", atama_tahsil="*"),
           dict(sonuc_deneme="IN_REVIEW", sonuc_failure_reason=FR, sonuc_saglayici_kimlik="yaz"),
           "PaymentManager:339-343; PaymentManager:449-453",
           "PaymentCallbackTest#aPaymentUnderFraudReviewDoesNotOpenEscrowYet", f"{S}|{A}|sonuc_saglayici_kimlik"),
        rc(i + 6, dict(adim="6_fraud", fraudStatus="null", atama_tahsil="*"), {}, "PaymentManager:345-349",
           "PaymentCallbackTest#anAbsentFraudDecisionRemainsPendingInsteadOfOpeningEscrow", f"{S}|{A}"),
        rc(i + 7, dict(adim="6_fraud", fraudStatus="2", atama_tahsil="*"), {}, "PaymentManager:345-349",
           "PaymentCallbackTest#anUnknownFraudDecisionRemainsPendingInsteadOfOpeningEscrow", f"{S}|{A}",
           "-1, 0 ve 1 dışındaki her değer."),
        rc(i + 8, dict(adim="7_atama", atama_tahsil="hayir"),
           dict(sonuc_next_verification="null", sonuc_dogrulama_sayaci="="), "PaymentManager:351-355; PaymentManager:472-478",
           "PaymentCallbackTest#aSuccessfulProviderResultForACancelledAssignmentRequiresManualReview",
           f"{S}|{A}|sonuc_next_verification",
           "Tahsil edilemez: atama CONFIRMED değil ya da ödeme durumu PENDING_COLLECTION|FAILED değil. Manuel inceleme."),
        rc(i + 9, dict(adim="8_settle"),
           dict(sonuc_deneme="SUCCEEDED", sonuc_failure_reason="null", sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="HELD_IN_ESCROW", sonuc_saglayici_kimlik="yaz",
                sonuc_odendi_olayi="evet", sonuc_dogrulama_sayaci="="),
           "PaymentManager:357-359; PaymentManager:492-514",
           "PaymentCallbackTest#aSuccessfulCallbackMovesTheAssignmentIntoEscrowAndRecordsTheBreakdown; PaymentCallbackTest#thePaidEventIsPublishedOnlyAfterThePaymentRowIsSaved",
           f"{S}|sonuc_saglayici_kimlik|sonuc_completed_at|{A}|sonuc_odendi_olayi",
           "Ücret kırılımı atamaya yazılır; ödendi olayı kayıt yazıldıktan sonra yayımlanır. Atama ödeme durumu PENDING_COLLECTION."),
        rc(i + 10, dict(adim="8_settle", atama_tahsil="evet"),
           dict(sonuc_deneme="SUCCEEDED", sonuc_failure_reason="null", sonuc_next_verification="null",
                sonuc_completed_at="now", sonuc_atama_odeme="HELD_IN_ESCROW", sonuc_saglayici_kimlik="yaz",
                sonuc_odendi_olayi="evet", sonuc_dogrulama_sayaci="="),
           "PaymentStateMachine:16-17; PaymentStateMachine:22-26",
           note="Atama ödeme durumu FAILED iken (önceki deneme başarısız) FAILED -> HELD_IN_ESCROW."),
        rc(i + 11, dict(adim="tavan", on_dogrulama_sayaci="7", retrieve="hata", kimlik="*", paymentStatus="*",
                        snapshot="*", mdStatus="*", fraudStatus="*", atama_tahsil="*"),
           dict(sonuc_next_verification="null"), "PaymentManager:455-470; PaymentVerificationProperties:13",
           "PaymentCallbackTest#automaticReconciliationStopsAfterTheConfiguredBoundButRemainsActiveForManualReview",
           f"{S}|sonuc_dogrulama_sayaci|sonuc_next_verification|{A}",
           "maxAttempts=8: sayaç 8'e ulaşınca otomatik mutabakat durur, manuel inceleme metriği artar. Test maxAttempts=2 ile koşuyor. Erteleyen her dal için geçerli."),
    ]
    sw = dict(olay="supurme", on_deneme="INITIATED", on_token="yok", on_dogrulama_sayaci="*", retrieve="", kimlik="",
              paymentStatus="", snapshot="", mdStatus="", fraudStatus="", atama_tahsil="")
    rows += [
        rc(i + 12, dict(sw, adim="supurme", on_yas_sn=">=900"),
           dict(sonuc_failure_reason="{payment.initiation.abandoned}", sonuc_next_verification="null",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0"),
           "PaymentManager:237-260; PaymentDao:86; PaymentVerificationProperties:15",
           "PaymentMonitoringTest#anInitiationAbandonedBeforeTheProviderCallIsSweptIntoManualReconciliation; PaymentMonitoringTest#theSweepDoesNotClaimAProviderRetrieveThatNeverHappened",
           f"{S}|sonuc_failure_reason|sonuc_next_verification|sonuc_dogrulama_sayaci",
           "Eşitlik süpürülür: sorgu created_at <= şimdi-15dk, yeniden kontrol isAfter. Token yok; otomatik mutabakat token ister, satır manuel kalır."),
        rc(i + 13, dict(sw, adim="supurme", on_yas_sn="<900"),
           dict(sonuc_deneme="=", sonuc_failure_reason="=", sonuc_next_verification="=", sonuc_completed_at="=",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0"),
           "PaymentDao:86; PaymentManager:244-249",
           "PaymentMonitoringTest#anInitiationStillInsideTheCheckoutWindowIsLeftAlone", S),
        rc(i + 14, dict(sw, adim="supurme", on_token="var", on_yas_sn="*"),
           dict(sonuc_deneme="=", sonuc_failure_reason="=", sonuc_next_verification="=", sonuc_completed_at="=",
                sonuc_dogrulama_sayaci="=", saglayici_sorgusu="0"),
           "PaymentDao:85; PaymentManager:244-249", "PaymentMonitoringTest#anInitiationThatReachedTheProviderIsNeverSwept", S),
    ]
    for r in rows:
        r["dogrulama"] = verdict(r, RC_OUT, "kod")
    return ["id"] + RC_IN + RC_OUT + ["kaynak_kod", "kaynak_test", "test_kolonlari", "dogrulama", "not"], rows


# ======================================================================
# commission.csv
# ======================================================================
CM_OUT = ["sonuc_isci_payi", "sonuc_komisyon", "sonuc_toplam"]
COMMISSION = [
    ("1000.00", "0.075", "CommissionCalculatorTest#theBreakdownAddsUpToTheChargedTotal", "sonuc_isci_payi|sonuc_komisyon|sonuc_toplam", ""),
    ("0.05", "0.10", "CommissionCalculatorTest#halfKurusRoundsUpNotDown", "sonuc_komisyon|sonuc_toplam", "0,005 yukarı yuvarlanır."),
    ("500", "0.12", "CommissionCalculatorTest#aDifferentRateProducesADifferentCommission", "sonuc_komisyon", ""),
    ("500", "0.075", "CommissionCalculatorTest#theAppliedRateIsReportedSoItCanBeStored", "", "Test yalnız uygulanan oranı (0.075) iddia ediyor."),
    ("0.30", "0.075", "CommissionCalculatorTest#floatingPointDriftIsGone", "sonuc_isci_payi", "Girdi 0.10 + 0.20."),
    ("0.01", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek=2 ve toplam=pay+komisyon iddia ediyor."),
    ("3.33", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek ve toplam değişmezini iddia ediyor."),
    ("99.99", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek ve toplam değişmezini iddia ediyor."),
    ("1234.56", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek ve toplam değişmezini iddia ediyor."),
    ("7777.77", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek ve toplam değişmezini iddia ediyor."),
    ("100000.05", "0.075", "CommissionCalculatorTest#roundingNeverLosesOrInventsKurus", "", "Test yalnız ölçek ve toplam değişmezini iddia ediyor."),
    ("1.00", "0.125", "", "", "HALF_UP sınırı: 0,125 -> 0,13."),
    ("6.20", "0.075", "", "", "HALF_UP sınırı: 0,465 -> 0,47."),
    ("2.20", "0.075", "", "", "HALF_UP sınırı: 0,165 -> 0,17."),
    ("0.07", "0.075", "", "", "0,00525 -> 0,01."),
    ("0.06", "0.075", "", "", "0,0045 -> 0,00: komisyon sıfır."),
    ("0.01", "0.10", "", "", "0,001 -> 0,00."),
    ("100.005", "0.075", "", "", "Kuruş altı girdi: pay önce HALF_UP. Go'da girdi int64 kuruş olduğundan ifade edilemez."),
    ("100.004", "0.075", "", "", "Kuruş altı girdi; Go'da ifade edilemez."),
    ("1075.00", "0.0755", "", "", "4 basamaklı oran = 755 bps."),
    ("99999999.99", "0.075", "", "", "NUMERIC(19,2) içinde büyük tutar."),
]


def commission():
    rows = []
    for n, (amount, rate, test, cols, note) in enumerate(COMMISSION, start=1):
        a, r = Decimal(amount), Decimal(rate)
        payout = q2(a)
        comm = q2(payout * r)
        total = payout + comm
        ok = a == a.quantize(C2) and bps(r) != ""
        row = dict(id=f"CM-{n:02d}", girdi_isci_payi=amount, girdi_isci_payi_kurus=kurus(a), oran=rate, oran_bps=bps(r),
                   sonuc_isci_payi=str(payout), sonuc_komisyon=str(comm), sonuc_toplam=str(total),
                   sonuc_isci_payi_kurus=kurus(payout), sonuc_komisyon_kurus=kurus(comm), sonuc_toplam_kurus=kurus(total),
                   hata="", go_uygulanabilir="evet" if ok else "hayir",
                   kaynak_kod=src("CommissionCalculator:31-34; CommissionProperties:12"), kaynak_test=src(test),
                   test_kolonlari=cols, not_=note)
        row["dogrulama"] = verdict(row, CM_OUT, "hesap")
        rows.append(row)
    for amount in ("0", "-1", "null"):
        n = len(rows) + 1
        row = dict(id=f"CM-{n:02d}", girdi_isci_payi=amount, girdi_isci_payi_kurus=amount if amount != "-1" else "-100",
                   oran="0.075", oran_bps="750", sonuc_isci_payi="", sonuc_komisyon="", sonuc_toplam="",
                   sonuc_isci_payi_kurus="", sonuc_komisyon_kurus="", sonuc_toplam_kurus="", hata="JOB_SALARY_MIN_VALUE",
                   go_uygulanabilir="evet", kaynak_kod=src("CommissionCalculator:26-27"),
                   kaynak_test=src("CommissionCalculatorTest#nonPositiveAmountsAreRejected"), test_kolonlari="hata",
                   not_="ValidationException {job.salary.min.value}.")
        row["dogrulama"] = verdict(row, ["hata"], "hesap")
        rows.append(row)
    cols = ["id", "girdi_isci_payi", "girdi_isci_payi_kurus", "oran", "oran_bps", "sonuc_isci_payi", "sonuc_komisyon",
            "sonuc_toplam", "sonuc_isci_payi_kurus", "sonuc_komisyon_kurus", "sonuc_toplam_kurus", "hata",
            "go_uygulanabilir", "kaynak_kod", "kaynak_test", "test_kolonlari", "dogrulama", "not"]
    return cols, rows


# ======================================================================
# cancellation-fee.csv
# ======================================================================
H = 3600
CF_OUT = ["sonuc_ceza_orani", "sonuc_ceza", "sonuc_iade"]
# (taraf, tahsilat|None, bildirim_sn, pencere_sa, gec_oran, kod satırları, test, test kolonları, not)
CANCEL = [
    ("EMPLOYER", "1075.00", 48 * H, 24, "0.50", "CancellationPolicy:57", "CancellationPolicyTest#anEmployerCancellingWellAheadPaysNoFee", "sonuc_ceza|sonuc_iade", ""),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.50", "CancellationPolicy:60; CancellationPolicy:45-47", "CancellationPolicyTest#anEmployerCancellingInsideTheWindowPaysHalf", "sonuc_ceza|sonuc_iade", ""),
    ("EMPLOYER", "1075.00", 2 * H, 24, "0.50", "CancellationPolicy:60; CancellationManager:120-122", "AssignmentOperationsAuthorizationTest#aLateEmployerCancellationRefundsOnlyThePartLeftAfterTheFee", "sonuc_ceza|sonuc_iade", ""),
    ("EMPLOYER", "1075.00", 24 * H, 24, "0.50", "CancellationPolicy:57", "CancellationPolicyTest#exactlyAtTheWindowBoundaryTheCancellationIsStillFree", "sonuc_ceza", "Pencere sınırı dahil serbest."),
    ("EMPLOYER", "1075.00", -600, 24, "0.50", "CancellationPolicy:54", "CancellationPolicyTest#anEmployerCancellingAfterTheStartForfeitsEverything", "sonuc_ceza|sonuc_iade", "İş başladıktan sonra: Java'da iade yok ve para emanette kalır (hata)."),
    ("WORKER", "1075.00", 48 * H, 24, "0.50", "CancellationPolicy:51", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "sonuc_ceza|sonuc_iade", ""),
    ("WORKER", "1075.00", 30 * 60, 24, "0.50", "CancellationPolicy:51", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "sonuc_ceza|sonuc_iade", ""),
    ("WORKER", "1075.00", -5 * H, 24, "0.50", "CancellationPolicy:51", "CancellationPolicyTest#aWorkerCancellingNeverCostsTheEmployerAnything", "sonuc_ceza|sonuc_iade", ""),
    ("EMPLOYER", None, 300, 24, "0.50", "CancellationPolicy:36-41", "CancellationPolicyTest#whenNothingWasCollectedThereIsNothingToRefund", "sonuc_ceza|sonuc_iade", "Emanette para yok."),
    ("EMPLOYER", "0.01", 2 * H, 24, "0.50", "CancellationPolicy:45-47", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "", "Test yalnız ceza+iade=tahsilat değişmezini iddia ediyor. Yuvarlama iadeyi 0 yapıyor: Java'da para emanette kalır."),
    ("EMPLOYER", "33.33", 2 * H, 24, "0.50", "CancellationPolicy:45-47", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "", "Test yalnız toplam değişmezini iddia ediyor."),
    ("EMPLOYER", "99999.99", 2 * H, 24, "0.50", "CancellationPolicy:45-47", "CancellationPolicyTest#feeAndRefundAlwaysSumToTheCollectedAmount", "", "Test yalnız toplam değişmezini iddia ediyor."),
    ("EMPLOYER", "1075.00", 24 * H - 1, 24, "0.50", "CancellationPolicy:57", "", "", "toHours kesirli saati atar: 23 sa 59 dk 59 sn -> 23 -> geç."),
    ("EMPLOYER", "1075.00", 0, 24, "0.50", "CancellationPolicy:54", "", "", "Tam başlangıç anı: isPositive false -> %100."),
    ("EMPLOYER", "1075.00", 1, 24, "0.50", "CancellationPolicy:54-60", "", "", "1 sn kala -> geç oran."),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.25", "CancellationPolicy:60", "", "", "Geç iptal oranı ayardan (CANCELLATION_LATE_FEE_RATE)."),
    ("EMPLOYER", "1075.00", 30 * H, 48, "0.50", "CancellationPolicy:57", "", "", "Serbest pencere ayardan (CANCELLATION_FREE_WINDOW_HOURS)."),
    ("EMPLOYER", "1075.00", 3 * H, 24, "0.33333", "CancellationPolicy:60", "", "", "Oran 4 basamağa HALF_UP: 0.33333 -> 0.3333. Girdi bps'e sığmaz, Go'da ifade edilemez."),
    ("ADMIN", "1075.00", 3 * H, 24, "0.50", "CancellationPolicy:51", "", "", "EMPLOYER dışı her taraf cezasız."),
    ("EMPLOYER", "100.005", 3 * H, 24, "0.50", "CancellationPolicy:36-38", "", "", "Tahsilat önce 2 basamağa HALF_UP. Kuruş altı girdi Go'da ifade edilemez."),
    ("EMPLOYER", "0.00", 3 * H, 24, "0.50", "CancellationPolicy:40", "", "", ""),
]


def cancellation():
    rows = []
    for n, (party, coll, sec, window, late, kod, test, cols, note) in enumerate(CANCEL, start=1):
        collected = Decimal("0") if coll is None else q2(Decimal(coll))
        if collected <= 0:
            rate, fee, refund = Decimal("0.0000"), Decimal("0.00"), Decimal("0.00")
        else:
            if party != "EMPLOYER":
                rate = Decimal("0.0000")
            elif sec <= 0:
                rate = Decimal("1.0000")
            elif int(sec // H) >= window:
                rate = Decimal("0.0000")
            else:
                rate = Decimal(late).quantize(C4, rounding=ROUND_HALF_UP)
            fee = q2(collected * rate)
            refund = collected - fee
        if coll is None or collected <= 0:
            java_op = "yok"
        elif refund <= 0:
            java_op = "yok"
        elif refund == collected:
            java_op = "REFUND"
        else:
            java_op = "REFUND+DISAPPROVE"
        go_op = java_op
        go_note = ""
        if coll is not None and collected > 0 and refund <= 0:
            if party == "EMPLOYER" and sec <= 0:
                go_op = "APPROVE"
                go_note = " Go (karar): iş başladıktan sonraki işveren iptalinde ceza işçiye geçer, emanet kapanır; komisyon payının dağılımı Faz 6'da."
            else:
                go_op = "karar_bekliyor"
                go_note = " Go: para emanette kalmamalı ilkesi; geç-pencere cezasının alıcısı karar bekliyor."
        coll_ok = coll is None or Decimal(coll) == Decimal(coll).quantize(C2)
        go_ok = coll_ok and bps(Decimal(late)) != ""
        row = dict(id=f"CF-{n:02d}", taraf=party, tahsilat="null" if coll is None else coll,
                   tahsilat_kurus="null" if coll is None else kurus(Decimal(coll)),
                   bildirim_sn=str(sec), serbest_pencere_saat=str(window), gec_iptal_orani=late,
                   gec_iptal_orani_bps=bps(Decimal(late)), sonuc_ceza_orani=str(rate), sonuc_ceza_orani_bps=bps(rate),
                   sonuc_ceza=str(fee), sonuc_iade=str(refund), sonuc_ceza_kurus=kurus(fee), sonuc_iade_kurus=kurus(refund),
                   java_saglayici_islemi=java_op, go_saglayici_islemi=go_op, go_uygulanabilir="evet" if go_ok else "hayir",
                   kaynak_kod=src(kod + "; CancellationManager:101-122"), kaynak_test=src(test), test_kolonlari=cols,
                   not_=(note + go_note).strip())
        row["dogrulama"] = verdict(row, CF_OUT, "hesap")
        rows.append(row)
    cols = ["id", "taraf", "tahsilat", "tahsilat_kurus", "bildirim_sn", "serbest_pencere_saat", "gec_iptal_orani",
            "gec_iptal_orani_bps", "sonuc_ceza_orani", "sonuc_ceza_orani_bps", "sonuc_ceza", "sonuc_iade",
            "sonuc_ceza_kurus", "sonuc_iade_kurus", "java_saglayici_islemi", "go_saglayici_islemi", "go_uygulanabilir",
            "kaynak_kod", "kaynak_test", "test_kolonlari", "dogrulama", "not"]
    return cols, rows


# ======================================================================
# backoff.csv
# ======================================================================
def backoff():
    rows = []
    for kind, kod, ceiling in (("odeme_dogrulama", "PaymentManager:455-470; PaymentManager:485-490; PaymentVerificationProperties:13-17", "manuel_inceleme"),
                               ("settlement_gorevi", "SettlementManager:148-157; SettlementManager:275-280; SettlementRetryProperties:14-18", "gorev_FAILED")):
        for n in range(1, 9):
            sec = "null" if n >= 8 else str(min(60 * (1 << min(n - 1, 20)), 21600))
            test, cols, note = "", "", ""
            if kind == "settlement_gorevi" and n in (1, 2, 5):
                test, cols = "SettlementRetryTest#theBackoffGrowsButIsCapped", "sonraki_deneme_sn"
            if kind == "settlement_gorevi" and n == 8:
                test, cols, note = "SettlementRetryTest#theTaskGivesUpAfterTheAttemptLimit", "tavan_eylemi", "Test maxAttempts=3 ile koşuyor."
            if kind == "odeme_dogrulama" and n == 8:
                test, cols, note = ("PaymentCallbackTest#automaticReconciliationStopsAfterTheConfiguredBoundButRemainsActiveForManualReview",
                                    "sonraki_deneme_sn", "Test maxAttempts=2 ile koşuyor.")
            row = dict(id=f"BO-{len(rows) + 1:02d}", tur=kind, basarisiz_deneme_sayisi=str(n), sonraki_deneme_sn=sec,
                       tavan_eylemi=ceiling if n >= 8 else "", kaynak_kod=src(kod), kaynak_test=src(test),
                       test_kolonlari=cols, not_=note)
            row["dogrulama"] = verdict(row, ["sonraki_deneme_sn", "tavan_eylemi"], "hesap")
            rows.append(row)
    row = dict(id=f"BO-{len(rows) + 1:02d}", tur="settlement_gorevi", basarisiz_deneme_sayisi="20", sonraki_deneme_sn="21600",
               tavan_eylemi="", kaynak_kod=src("SettlementManager:275-280"),
               kaynak_test=src("SettlementRetryTest#theBackoffGrowsButIsCapped"), test_kolonlari="sonraki_deneme_sn|tavan_eylemi",
               not_="Formül satırı: maxAttempts > 20 yapılandırılırsa; tavan 6 sa.")
    row["dogrulama"] = verdict(row, ["sonraki_deneme_sn", "tavan_eylemi"], "hesap")
    rows.append(row)
    return ["id", "tur", "basarisiz_deneme_sayisi", "sonraki_deneme_sn", "tavan_eylemi", "kaynak_kod", "kaynak_test",
            "test_kolonlari", "dogrulama", "not"], rows


# ======================================================================
# escrow-transitions.csv / escrow-predicates.csv
# ======================================================================
ALLOWED = {"PENDING_COLLECTION": {"HELD_IN_ESCROW", "FAILED"}, "FAILED": {"HELD_IN_ESCROW", "FAILED"},
           "HELD_IN_ESCROW": {"RELEASED_TO_WORKER", "REFUNDED"}, "RELEASED_TO_WORKER": set(), "REFUNDED": set()}
ES_TESTED = {
    ("PENDING_COLLECTION", "HELD_IN_ESCROW"): "PaymentStateMachineTest#moneyOnlyMovesForward; PaymentStateMachineTest#aLegalTransitionIsApplied",
    ("HELD_IN_ESCROW", "REFUNDED"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("REFUNDED", "HELD_IN_ESCROW"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("HELD_IN_ESCROW", "PENDING_COLLECTION"): "PaymentStateMachineTest#moneyOnlyMovesForward; PaymentStateMachineTest#anIllegalTransitionThrowsInsteadOfSilentlyApplying",
    ("REFUNDED", "PENDING_COLLECTION"): "PaymentStateMachineTest#moneyOnlyMovesForward",
    ("HELD_IN_ESCROW", "HELD_IN_ESCROW"): "PaymentStateMachineTest#anAlreadyPaidAssignmentCannotBeChargedAgain",
}
ES_EVENT = {
    (None, "PENDING_COLLECTION"): ("isci_kabul", "JobAssignmentManager:209-212"),
    ("PENDING_COLLECTION", "HELD_IN_ESCROW"): ("odeme_dogrulandi", "PaymentManager:492-514"),
    ("FAILED", "HELD_IN_ESCROW"): ("odeme_dogrulandi", "PaymentManager:492-514"),
    ("PENDING_COLLECTION", "FAILED"): ("deneme_basarisiz", "PaymentManager:516-529"),
    ("FAILED", "FAILED"): ("deneme_basarisiz", "PaymentManager:516-529"),
    ("HELD_IN_ESCROW", "RELEASED_TO_WORKER"): ("emanet_cikisi", "PayoutManager:150"),
    ("HELD_IN_ESCROW", "REFUNDED"): ("iade", "PayoutManager:206"),
}


def escrow():
    states = ["null", "PENDING_COLLECTION", "FAILED", "HELD_IN_ESCROW", "RELEASED_TO_WORKER", "REFUNDED"]
    targets = ["PENDING_COLLECTION", "FAILED", "HELD_IN_ESCROW", "RELEASED_TO_WORKER", "REFUNDED"]
    rows = []
    for fr in states:
        for to in targets:
            frk = None if fr == "null" else fr
            ok = (to == "PENDING_COLLECTION") if frk is None else (to in ALLOWED[frk])
            test = ES_TESTED.get((frk, to), "")
            ev, evkod = ES_EVENT.get((frk, to), ("", ""))
            note = ""
            if frk is None:
                note = "Atama kabulünde durum doğrudan yazılır; transition() kullanılmaz."
            row = dict(id=f"ES-{len(rows) + 1:02d}", onceki=fr, sonraki=to, izinli="evet" if ok else "hayir",
                       hata="" if ok else "PAYMENT_INVALID_STATUS_TRANSITION", olay=ev,
                       kaynak_kod=src("PaymentStateMachine:15-20; PaymentStateMachine:37-50" + (f"; {evkod}" if evkod else "")),
                       kaynak_test=src(test), test_kolonlari="izinli" if test else "", not_=note)
            row["dogrulama"] = verdict(row, ["izinli"], "kod")
            rows.append(row)
    return ["id", "onceki", "sonraki", "izinli", "hata", "olay", "kaynak_kod", "kaynak_test", "test_kolonlari",
            "dogrulama", "not"], rows


def escrow_predicates():
    preds = [
        ("acceptsPayment", "CONFIRMED", "PENDING_COLLECTION|FAILED", "evet", "PaymentStateMachineTest#onlyAConfirmedAssignmentAwaitingCollectionCanStartAPayment"),
        ("acceptsPayment", "PENDING_CONFIRMATION|REJECTED", "PENDING_COLLECTION", "hayir", "PaymentStateMachineTest#anUnconfirmedAssignmentCannotStartAPayment"),
        ("acceptsPayment", "CONFIRMED", "HELD_IN_ESCROW", "hayir", "PaymentStateMachineTest#anAlreadyPaidAssignmentCannotBeChargedAgain"),
        ("acceptsPayment", "IN_PROGRESS|COMPLETED", "*", "hayir", ""),
        ("acceptsPayment", "CANCELLED", "*", "hayir", ""),
        ("canRelease", "COMPLETED", "HELD_IN_ESCROW", "evet", "PayoutReleaseTest#aCompletedAssignmentInEscrowCanBeReleased"),
        ("canRelease", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "hayir", "PayoutReleaseTest#moneyIsNotReleasedBeforeTheWorkIsCompleted"),
        ("canRelease", "COMPLETED", "PENDING_COLLECTION|FAILED|RELEASED_TO_WORKER|REFUNDED", "hayir", ""),
        ("canRefund", "*", "HELD_IN_ESCROW", "evet", "PayoutReleaseTest#escrowedMoneyCanBeRefundedToTheEmployer"),
        ("canRefund", "*", "RELEASED_TO_WORKER", "hayir", "PayoutReleaseTest#alreadyReleasedMoneyCannotBeRefunded"),
        ("canRefund", "*", "PENDING_COLLECTION|FAILED|REFUNDED", "hayir", ""),
    ]
    rows = []
    for name, a, p, res, test in preds:
        row = dict(id=f"EP-{len(rows) + 1:02d}", yuklem=name, atama_durumu=a, odeme_durumu=p, sonuc=res,
                   kaynak_kod=src("PaymentStateMachine:22-35"), kaynak_test=src(test),
                   test_kolonlari="sonuc" if test else "", not_="")
        row["dogrulama"] = verdict(row, ["sonuc"], "kod")
        rows.append(row)
    return ["id", "yuklem", "atama_durumu", "odeme_durumu", "sonuc", "kaynak_kod", "kaynak_test", "test_kolonlari",
            "dogrulama", "not"], rows


# ======================================================================
# assignment-transitions.csv
# ======================================================================
AT_OUT = ["java_durum", "java_odeme", "java_hata"]
LIVE = "PENDING_CONFIRMATION|CONFIRMED|IN_PROGRESS"


def at(i, onceki, odeme, olay, aktor, kosul, jd, jo, jh, gd=None, go=None, gh=None, kod="", test="", cols="", note=""):
    r = dict(id=f"AT-{i}", onceki_durum=onceki, onceki_odeme=odeme, olay=olay, aktor=aktor, kosul=kosul,
             java_durum=jd, java_odeme=jo, java_hata=jh,
             go_durum=jd if gd is None else gd, go_odeme=jo if go is None else go, go_hata=jh if gh is None else gh,
             kaynak_kod=src(kod), kaynak_test=src(test), test_kolonlari=cols, not_=note)
    r["dogrulama"] = verdict(r, AT_OUT, "kod")
    return r


def assignment():
    rows = [
        at("01", "yok", "yok", "basvuru_kabul", "isveren", "basvuru=APPLIED;kontenjan=bos", "PENDING_CONFIRMATION", "null", "",
           kod="JobApplicationManager:172-209", test="ApplicationDecisionIT#anOfferStillWaitingForItsWorkerHoldsTheOnlyOpening",
           cols="java_durum", note="İşveren: bireysel ilanda sahibi, şirket ilanında güncel yetkili (D1)."),
        at("01b", "yok", "yok", "basvuru_kabul", "isveren", "basvuru=APPLIED;kontenjan=dolu", "yok", "yok",
           "JOB_APPLICATION_JOB_HEADCOUNT_FULL", kod="JobApplicationManager:295-300",
           test="ApplicationDecisionIT#anOfferStillWaitingForItsWorkerHoldsTheOnlyOpening", cols="java_durum|java_odeme|java_hata"),
        at("02", "PENDING_CONFIRMATION", "null", "isci_kabul", "isci", "", "CONFIRMED", "PENDING_COLLECTION", "",
           kod="JobAssignmentManager:209-212",
           test="AssignmentReadIT#acceptingTheAssignmentAnswersWithTheTimelineDtoAndStampsConfirmedAt", cols="java_durum",
           note="confirmedAt damgalanır; ödeme durumu transition() kullanmadan yazılır."),
        at("03", "PENDING_CONFIRMATION", "null", "isci_ret", "isci", "", "REJECTED", "=", "",
           kod="JobAssignmentManager:214-226",
           test="MessagingPaymentGateIT#decliningTheOfferOverHttpClosesTheDirectChatLeftOverForThatWorker", cols="java_durum",
           note="AssignmentClosedEvent: sohbet kapanır."),
        at("04", "CONFIRMED|REJECTED|IN_PROGRESS|COMPLETED|CANCELLED", "*", "isci_yanit", "isci", "", "=", "=",
           "JOB_ASSIGNMENT_ALREADY_ANSWERED", kod="JobAssignmentManager:204-207"),
        at("05", "CONFIRMED", "HELD_IN_ESCROW", "check_in", "isci", "acik_oturum=yok;client_event_id=yeni", "IN_PROGRESS", "=", "",
           kod="TimesheetManager:79-110", note="İş oturumu açılır."),
        at("05b", "CONFIRMED", "PENDING_COLLECTION|FAILED", "check_in", "isci", "client_event_id=yeni", "=", "=",
           "TIMESHEET_PAYMENT_NOT_SECURED", kod="TimesheetManager:83-87",
           test="AssignmentOperationsAuthorizationTest#workCannotStartBeforeTheEmployerHasPaid", cols="java_durum",
           note="Test hata kodunu değil ConflictException türünü iddia ediyor."),
        at("06", "IN_PROGRESS", "HELD_IN_ESCROW", "check_in", "isci", "acik_oturum=yok;client_event_id=yeni", "=", "=", "",
           kod="TimesheetManager:79-110", note="Sonraki gün yeni iş oturumu."),
        at("06b", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "check_in", "isci", "acik_oturum=var;client_event_id=yeni", "=", "=",
           "TIMESHEET_ALREADY_CHECKED_IN", kod="TimesheetManager:89-90"),
        at("07", "PENDING_CONFIRMATION|REJECTED|COMPLETED|CANCELLED", "*", "check_in", "isci", "client_event_id=yeni", "=", "=",
           "TIMESHEET_ASSIGNMENT_NOT_ACTIVE", kod="TimesheetManager:79-81"),
        at("07b", "*", "*", "check_in", "isci", "client_event_id=kayitli;yuk=ayni", "=", "=", "",
           kod="TimesheetManager:67-76", test="TimesheetManagerOfflineEventTest#retryingTheSameNormalizedCheckInReturnsTheOriginalResult",
           note="Tekrar oynatma durum kontrolünden ÖNCE: kayıtlı oturum döner, atama artık aktif olmasa da. Test dönen sonucu iddia ediyor, atama durumunu değil."),
        at("07c", "*", "*", "check_in", "isci", "client_event_id=kayitli;yuk=farkli", "=", "=", "TIMESHEET_IDEMPOTENCY_CONFLICT",
           kod="TimesheetManager:67-76; TimesheetManager:198-205",
           test="TimesheetManagerOfflineEventTest#reusingACheckInIdentityWithDifferentPayloadIsRejected",
           note="Test hata türünü iddia ediyor; kolon karşılığı doğrulanmadı."),
        at("08", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "isveren_tamamla", "isveren", "uyusmazlik=yok;birincil_iban=var",
           "COMPLETED", "RELEASED_TO_WORKER", "", kod="JobAssignmentManager:596-628; JobAssignmentManager:680-687",
           test="AssignmentCompletionIT#theEmployerCompletingTheWorkReleasesTheEscrowToTheWorker", cols="java_durum|java_odeme|java_hata",
           note="Payout PENDING + APPROVE görevi."),
        at("09", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "isveren_tamamla", "isveren", "uyusmazlik=acik;birincil_iban=var",
           "COMPLETED", "=", "", "=", "=", "ASSIGNMENT_DISPUTE_OPEN",
           kod="JobAssignmentManager:619-620; PayoutManager:160-169",
           test="AssignmentCompletionIT#anOpenDisputeLeavesTheMoneyInEscrowButStillCompletesTheWork", cols="java_durum|java_odeme|java_hata",
           note="D4: açık uyuşmazlık varken atama tamamlanamaz. Go hata kodu adı önerilir."),
        at("10", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "isveren_tamamla", "isveren", "uyusmazlik=yok;birincil_iban=yok",
           "=", "=", "PAYOUT_WORKER_IBAN_NOT_VERIFIED", kod="PayoutManager:129-135",
           test="AssignmentCompletionIT#aWorkerWithoutAPrimaryIbanBlocksTheCompletionLoudly", cols="java_durum|java_odeme|java_hata",
           note="İşlem geri alınır."),
        at("11", "CONFIRMED|IN_PROGRESS", "PENDING_COLLECTION|FAILED", "isveren_tamamla", "isveren", "", "=", "=",
           "ASSIGNMENT_PAYMENT_NOT_SECURED", kod="JobAssignmentManager:612-616",
           test="AssignmentOperationsAuthorizationTest#anUnpaidAssignmentCannotBeMarkedComplete",
           note="Test yalnız ConflictException türünü ve COMPLETED olmadığını iddia ediyor."),
        at("12", "PENDING_CONFIRMATION|REJECTED|COMPLETED|CANCELLED", "*", "isveren_tamamla", "isveren", "", "=", "=",
           "JOB_ASSIGNMENT_NOT_COMPLETABLE", kod="JobAssignmentManager:606-610",
           test="AssignmentCompletionIT#aSecondCompletionIsRefusedAndDoesNotTouchThePayout", cols="java_hata"),
        at("13", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "otomatik_tamamla", "sistem", "is_bitisi_gecen_sn=>=259200;uyusmazlik=yok",
           "COMPLETED", "RELEASED_TO_WORKER", "", kod="JobAssignmentManager:630-644; JobAssignmentManager:662-678",
           note="Varsayılan ek süre 3 gün (AUTO_COMPLETION_GRACE_DAYS)."),
        at("14", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "otomatik_tamamla", "sistem", "is_bitisi_gecen_sn=>=259200;uyusmazlik=acik",
           "=", "=", "DISPUTE_BLOCKS_RELEASE", "=", "=", "", kod="JobAssignmentManager:674; PayoutManager:124-126",
           note="Java: releaseToWorker 409 atar, işlem geri alınır, iş satırı her taramada tekrar dener. Go (D4): sessizce atlanır."),
        at("15", LIVE, "HELD_IN_ESCROW", "taraf_iptal", "taraf", "uyusmazlik=yok;iade=>0", "CANCELLED", "REFUNDED", "",
           kod="CancellationManager:81-135",
           test="AssignmentOperationsAuthorizationTest#aLateEmployerCancellationRefundsOnlyThePartLeftAfterTheFee", cols="java_odeme",
           note="İade tutarı ve ceza: cancellation-fee.csv."),
        at("15b", "IN_PROGRESS|CONFIRMED", "HELD_IN_ESCROW", "taraf_iptal", "isveren", "uyusmazlik=yok;bildirim_sn=<=0",
           "CANCELLED", "=", "", "CANCELLED", "RELEASED_TO_WORKER", "", kod="CancellationManager:101-122",
           note="Java hatası: ceza %100, iade 0 olduğundan refundEmployer çağrılmaz, para emanette kalır. Go (karar): ceza işçiye geçer."),
        at("15c", LIVE, "null|PENDING_COLLECTION|FAILED", "taraf_iptal", "taraf", "uyusmazlik=yok", "CANCELLED", "=", "",
           kod="CancellationManager:81-135",
           test="AssignmentOperationsAuthorizationTest#whenNoMoneyWasCollectedNoRefundIsAttempted", cols="java_odeme"),
        at("16", "REJECTED|COMPLETED|CANCELLED", "*", "taraf_iptal", "taraf", "", "=", "=", "CANCELLATION_NOT_CANCELLABLE",
           kod="CancellationManager:88-90",
           test="AssignmentOperationsAuthorizationTest#aCompletedAssignmentCannotBeCancelled; AssignmentOperationsAuthorizationTest#anAssignmentCannotBeCancelledTwice",
           note="Testler yalnız ConflictException türünü iddia ediyor."),
        at("16b", LIVE, "HELD_IN_ESCROW", "taraf_iptal", "taraf", "uyusmazlik=acik", "=", "=", "DISPUTE_BLOCKS_RELEASE",
           kod="CancellationManager:93-95"),
        at("17", "CONFIRMED", "PENDING_COLLECTION|FAILED", "odenmemis_sure_doldu", "sistem",
           "aktif_deneme=yok;baslangica_kalan_sn=<=21600", "CANCELLED", "=", "",
           kod="CancellationManager:161-205; JobAssignmentDao:155-156; JobAssignmentManager:646-658",
           test="UnpaidExpiryRulesTest#anUnpaidAssignmentIsCancelledWhenTheJobIsAboutToStart; UnpaidExpiryRulesTest#theExpiryIsRecordedAgainstTheEmployerWithNoMoneyMoving",
           cols="java_durum|java_odeme|java_hata",
           note="Eşitlik dahil (<=). İptal kaydı: EMPLOYER, oran 0, ceza 0, iade 0, sebep {cancellation.payment.not.collected}."),
        at("18", "CONFIRMED", "PENDING_COLLECTION|FAILED", "odenmemis_sure_doldu", "sistem", "aktif_deneme=var", "=", "=", "",
           kod="CancellationManager:178-182",
           test="UnpaidExpiryRulesTest#anAssignmentWithAnOpenPaymentAttemptIsNeverBlamedOnTheEmployerWithZeroRefund; UnpaidExpiryRulesTest#aSweptInitiationStillCountsAsAnOpenAttemptForTheExpiryJob",
           cols="java_durum", note="Süpürülmüş deneme de aktif sayılır."),
        at("19", "COMPLETED", "HELD_IN_ESCROW", "uyusmazlik_release", "personel", "uyusmazlik=acik", "=", "RELEASED_TO_WORKER", "",
           kod="DisputeManager:133-166",
           test="DisputeResolutionAuthorizationTest#staffHoldingTheReleasePermissionResolvesAndMovesTheMoney", cols="java_odeme",
           note="Personel dispute.resolve + payout.release ister."),
        at("20", "CONFIRMED|IN_PROGRESS", "HELD_IN_ESCROW", "uyusmazlik_release", "personel", "uyusmazlik=acik", "=", "=",
           "PAYOUT_NOT_READY_FOR_RELEASE", kod="DisputeManager:165-166; PayoutManager:120-122",
           note="canRelease COMPLETED ister; çözüm de geri alınır. Go hedefi karar bekliyor (D4 ile birlikte ele alınacak)."),
        at("21", "*", "HELD_IN_ESCROW", "uyusmazlik_refund", "personel", "uyusmazlik=acik", "=", "REFUNDED", "",
           kod="DisputeManager:168; PayoutManager:172-215",
           test="DisputeResolutionAuthorizationTest#staffHoldingTheRefundPermissionResolvesAndRefundsTheEmployer", cols="java_odeme",
           note="Atama iptal edilmez; tam tutar iade edilir."),
    ]
    return ["id", "onceki_durum", "onceki_odeme", "olay", "aktor", "kosul", "java_durum", "java_odeme", "java_hata",
            "go_durum", "go_odeme", "go_hata", "kaynak_kod", "kaynak_test", "test_kolonlari", "dogrulama", "not"], rows


# ======================================================================
# payout-transitions.csv
# ======================================================================
PO_OUT = ["java_sonraki", "java_hata"]


def po(rid, varlik, onceki, go_onceki, olay, kosul, js, jh, gb, gh=None, kod="", test="", cols="", note="", karar=False):
    r = dict(id=rid, varlik=varlik, onceki_durum=onceki, go_onceki=go_onceki, olay=olay, kosul=kosul, java_sonraki=js,
             java_hata=jh, go_beklenen=gb, go_hata=jh if gh is None else gh, kaynak_kod=src(kod), kaynak_test=src(test),
             test_kolonlari=cols, not_=note)
    r["dogrulama"] = "karar" if karar else verdict(r, PO_OUT, "kod")
    return r


def payout():
    rows = [
        po("PO-01", "payout", "yok", "yok", "emanet_cikisi", "atama=COMPLETED;odeme=HELD_IN_ESCROW;uyusmazlik=yok;birincil_iban=var",
           "PENDING", "", "PENDING", kod="PayoutManager:110-156",
           test="PayoutReleaseTest#aPayoutStartsPendingUntilTheProviderConfirms; PayoutReleaseTest#aCompletedAssignmentInEscrowCanBeReleased",
           cols="java_sonraki|java_hata", note="APPROVE görevi kuyruğa girer; atama RELEASED_TO_WORKER."),
        po("PO-02", "payout", "*", "*", "emanet_cikisi", "payout=var", "=", "", "=", kod="PayoutManager:114-118",
           test="PayoutReleaseTest#theSameAssignmentIsNeverPaidOutTwice", note="Mevcut payout döner; test yalnız yeni kayıt yazılmadığını iddia ediyor."),
        po("PO-03", "payout", "PENDING", "PENDING", "approve_basarili", "yanit_islem_kimligi=var", "COMPLETED", "", "IN_TRANSIT",
           kod="SettlementManager:136-137; SettlementManager:199-219",
           test="SettlementRetryTest#aSuccessfulRunClosesTheTaskAndSettlesThePayoutFromTheProviderApproval", cols="java_sonraki|java_hata",
           note="K2: sağlayıcı onayı IN_TRANSIT'tir, PAID değil."),
        po("PO-04", "payout", "PENDING", "PENDING", "approve_basarili", "yanit_islem_kimligi=yok;kuyruk_islem_kimligi=var",
           "COMPLETED", "", "IN_TRANSIT", kod="SettlementManager:206",
           test="SettlementRetryTest#anApprovalWithoutAResponseIdStillSettlesFromTheQueuedTransaction", cols="java_sonraki|java_hata",
           note="Referans = kuyruğa alınmış işlem kimliği."),
        po("PO-05", "payout", "PENDING", "PENDING", "approve_basarili", "yanit_islem_kimligi=yok;kuyruk_islem_kimligi=yok",
           "PROCESSING", "", "IN_TRANSIT", kod="SettlementManager:207-209; SettlementManager:73-77",
           note="Pratikte ulaşılamaz: boş işlem kimliğiyle görev açılmıyor."),
        po("PO-06", "payout", "PENDING", "PENDING", "approve_tavan", "basarisiz_deneme=8", "FAILED", "", "FAILED",
           kod="SettlementManager:148-150; SettlementManager:241-258",
           test="SettlementRetryTest#givingUpOnAnApprovalMarksThePayoutFailed", cols="java_sonraki|java_hata",
           note="failure_reason {payout.settlement.failed}."),
        po("PO-07", "payout", "PROCESSING", "IN_TRANSIT", "ekstre_bildir", "kanit=eslesir", "COMPLETED", "", "PAID",
           kod="PayoutManager:217-273; PayoutManager:391-405",
           test="PayoutReconciliationIT#theWorkerSeesTheirPayoutAsCompletedOnceTheStatementLineIsReported; PayoutSettlementReconciliationIT#matchingStatementEvidenceCompletesThePayoutInPostgres",
           cols="java_sonraki|java_hata",
           note="Kanıt: sağlayıcı referansı, tutar, para birimi eşit; ödeme tarihi >= payout oluşturma. Java'da COMPLETED çoğunlukla APPROVE'dan gelir (PO-03); K2'de PAID yalnız bu yoldan."),
        po("PO-08", "payout", "COMPLETED", "PAID", "ekstre_bildir", "satir=ayni", "=", "", "=", kod="PayoutManager:225-230",
           test="PayoutReconciliationIT#reportingTheSameStatementLineTwiceStillLeavesOneCompletedPayout; PayoutSettlementReconciliationIT#theSameStatementLineReportedTwiceProducesOneFinancialOutcome",
           cols="java_sonraki|java_hata"),
        po("PO-09", "payout", "COMPLETED", "PAID", "ekstre_bildir", "satir=farkli", "=", "PAYOUT_SETTLEMENT_REFERENCE_CONFLICT", "=",
           kod="PayoutManager:232", test="PayoutReconciliationIT#aSecondStatementLineOnACompletedPayoutIsRefused",
           cols="java_hata", note="Java'da APPROVE ile COMPLETED olmuş payout'ta da aynı ret (settlement_reference APPROVE referansıdır)."),
        po("PO-10", "payout", "PROCESSING", "IN_TRANSIT", "ekstre_bildir", "kanit=eslesmez", "=", "PAYOUT_SETTLEMENT_EVIDENCE_MISMATCH",
           "=", kod="PayoutManager:246-252",
           test="PayoutReconciliationIT#evidenceThatDoesNotMatchTheSnapshotLeavesThePayoutProcessing; PayoutSettlementReconciliationIT#mismatchedEvidenceLeavesThePayoutAwaitingSettlementInsteadOfFailingIt",
           cols="java_sonraki|java_hata"),
        po("PO-11", "payout", "PROCESSING", "IN_TRANSIT", "ekstre_bildir", "saglayici_referansi=yok", "=", "PAYOUT_NO_PROVIDER_REFERENCE",
           "=", kod="PayoutManager:240-244",
           test="PayoutSettlementReconciliationIT#aPayoutWithoutAProviderReferenceIsEscalatedRatherThanSettled", cols="java_sonraki|java_hata",
           note="Yükseltme gerekir."),
        po("PO-12", "payout", "PENDING|FAILED", "PENDING|FAILED", "ekstre_bildir", "", "=", "PAYOUT_NOT_AWAITING_SETTLEMENT", "=",
           kod="PayoutManager:235-238"),
        po("PO-13", "payout", "PROCESSING", "IN_TRANSIT", "ekstre_bildir", "satir=baska_payoutta", "=",
           "PAYOUT_SETTLEMENT_REFERENCE_TAKEN", "=", kod="PayoutManager:260-268",
           test="PayoutSettlementReconciliationIT#oneStatementLineCannotSettleTwoPayoutsEvenWhenTheWritesTrulyOverlap", cols="java_hata",
           note="uk_payouts_settlement_reference."),
        po("PO-14", "payout", "*", "*", "db_yazimi", "durum=COMPLETED;kanit=yok", "=", "DB_CHECK_VIOLATION", "=",
           test="PayoutSettlementReconciliationIT#theDatabaseRefusesACompletedPayoutThatCarriesNoSettlementEvidence",
           cols="java_sonraki|java_hata", note="chk_payouts_completed_requires_evidence; Go'da kısıt PAID'e taşınır (ADR-0009)."),
        po("PO-15", "payout", "*", "*", "db_yazimi", "durum=PROCESSING;processing_at=null", "=", "DB_CHECK_VIOLATION", "=",
           test="PayoutSettlementReconciliationIT#aPayoutCannotEnterProcessingWithoutTheClockThatStartsTheWait",
           cols="java_sonraki|java_hata", note="Go'da IN_TRANSIT için aynı kısıt."),
        po("PO-16", "payout", "", "PAID", "banka_iadesi", "", "", "", "RETURNED", "", note="K2/ADR-0009; Java karşılığı yok.", karar=True),
        po("PO-17", "payout", "", "PENDING", "cikis_iptali", "", "", "", "CANCELED", "",
           note="K2/ADR-0009; hangi durumlardan geçileceği Faz 6'da FSM tablosuyla belirlenir.", karar=True),
        po("RF-01", "refund", "yok", "yok", "iade_iste", "odeme=HELD_IN_ESCROW;uyusmazlik=yok;tutar=>0", "PENDING", "", "PENDING",
           kod="PayoutManager:172-215", test="PayoutReleaseTest#escrowedMoneyCanBeRefundedToTheEmployer", cols="java_sonraki|java_hata",
           note="REFUND görevi; tutar < tahsilat ise DISAPPROVE da; atama REFUNDED."),
        po("RF-01b", "refund", "yok", "yok", "iade_iste", "tutar=>tahsilat", "yok", "PAYOUT_INVALID_REFUND_AMOUNT",
           "yok", kod="PayoutManager:187-188", test="PayoutReleaseTest#aRefundCannotExceedWhatWasCharged",
           note="Test ValidationException türünü iddia ediyor."),
        po("RF-02", "refund", "PENDING", "PENDING", "refund_gorevi_basladi", "", "PROCESSING", "", "PROCESSING",
           kod="SettlementManager:125-127; RefundManager:25", test="SettlementRetryTest#aRefundBecomesProcessingBeforeTheProviderCall",
           note="Ayrı işlem, sağlayıcı çağrısından önce. Test çağrı sırasını iddia ediyor, durumu değil."),
        po("RF-03", "refund", "PROCESSING", "PROCESSING", "refund_basarili", "", "COMPLETED", "", "COMPLETED",
           kod="SettlementManager:138-139; SettlementManager:230-239",
           test="SettlementRetryTest#aSuccessfulDisapprovalClosesTheRefundInsteadOfLeavingItPending", cols="java_sonraki|java_hata",
           note="Test adı yanıltıcı: görev REFUND."),
        po("RF-04", "refund", "PENDING|PROCESSING", "PENDING|PROCESSING", "refund_tavan", "basarisiz_deneme=8", "FAILED", "", "FAILED",
           kod="SettlementManager:148-150; SettlementManager:260-264", test="SettlementRetryTest#givingUpOnARefundMarksTheRefundFailed",
           cols="java_sonraki|java_hata"),
        po("RF-05", "refund", "*", "*", "disapprove_sonucu", "", "=", "", "=", kod="SettlementManager:136-140; SettlementManager:242-244",
           test="SettlementRetryTest#aDisapprovalNeverTouchesThePayoutOrTheRefund", cols="java_sonraki|java_hata"),
        po("ST-01", "settlement_task", "yok", "yok", "kuyruga_al", "settlement=acik;islem_kimligi=var;ayni_gorev=yok", "PENDING", "",
           "PENDING", kod="SettlementManager:65-98", note="attempts 0, next_attempt_at = şimdi."),
        po("ST-01b", "settlement_task", "yok", "yok", "kuyruga_al", "settlement=kapali|islem_kimligi=yok", "yok", "", "yok",
           kod="SettlementManager:68-77",
           test="SettlementRetryTest#nothingIsQueuedWhenSettlementIsSwitchedOff; SettlementRetryTest#nothingIsQueuedWithoutAProviderTransactionId",
           cols="java_sonraki|java_hata", note="Çağıran PAYOUT_SETTLEMENT_NOT_QUEUED atar ve işlemi geri alır."),
        po("ST-02", "settlement_task", "*", "*", "kuyruga_al", "ayni_gorev=var", "=", "", "=", kod="SettlementManager:79-84",
           test="SettlementRetryTest#queueingTwiceForTheSamePaymentReusesTheTask", cols="java_sonraki|java_hata"),
        po("ST-03", "settlement_task", "PENDING", "PENDING", "calistir_basarili", "", "SUCCEEDED", "", "SUCCEEDED",
           kod="SettlementManager:116-145",
           test="SettlementRetryTest#aSuccessfulRunClosesTheTaskAndSettlesThePayoutFromTheProviderApproval", cols="java_sonraki|java_hata"),
        po("ST-04", "settlement_task", "PENDING", "PENDING", "calistir_basarisiz", "basarisiz_deneme=<8", "PENDING", "", "PENDING",
           kod="SettlementManager:153-157", test="SettlementRetryTest#aFailedRunStaysPendingAndBacksOff", cols="java_sonraki|java_hata",
           note="Geri çekilme: backoff.csv."),
        po("ST-05", "settlement_task", "PENDING", "PENDING", "calistir_basarisiz", "basarisiz_deneme=8", "FAILED", "", "FAILED",
           kod="SettlementManager:148-152", test="SettlementRetryTest#theTaskGivesUpAfterTheAttemptLimit", cols="java_sonraki|java_hata"),
        po("ST-06", "settlement_task", "SUCCEEDED|FAILED", "SUCCEEDED|FAILED", "calistir", "", "=", "", "=", kod="SettlementManager:118-121",
           test="SettlementRetryTest#anAlreadySucceededTaskIsNotRunAgain", cols="java_sonraki|java_hata"),
    ]
    return ["id", "varlik", "onceki_durum", "go_onceki", "olay", "kosul", "java_sonraki", "java_hata", "go_beklenen",
            "go_hata", "kaynak_kod", "kaynak_test", "test_kolonlari", "dogrulama", "not"], rows


# ======================================================================
FREE_TEXT = {"not", "kaynak_kod", "kaynak_test", "dogrulama"}
TOKEN = re.compile(r"^[A-Za-z0-9_.:{}+|<>=*\-/;]*$")


def validate(name, cols, rows):
    errors = []
    ids = set()
    for r in rows:
        if r["id"] in ids:
            errors.append(f"{name}: tekrar eden id {r['id']}")
        ids.add(r["id"])
        for c in cols:
            v = r.get(c, r.get("not_") if c == "not" else None)
            if v is None:
                errors.append(f"{name} {r['id']}: kolon yok {c}")
                continue
            if c not in FREE_TEXT and not TOKEN.match(v.replace("{payment.", "{payment.")):
                errors.append(f"{name} {r['id']}.{c}: sözlük dışı değer {v!r}")
        if r.get("dogrulama") not in ("test", "kısmi", "kod", "hesap", "karar"):
            errors.append(f"{name} {r['id']}: dogrulama {r.get('dogrulama')}")
    if name == "payout-transitions.csv":
        for r in rows:
            if not r["go_beklenen"]:
                errors.append(f"{name} {r['id']}: go_beklenen boş")
    return errors


def write(name, cols, rows):
    with open(os.path.join(OUT, name), "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(cols)
        for r in rows:
            w.writerow([r["not_"] if c == "not" else r.get(c, "") for c in cols])


def main():
    tables = {
        "reconcile.csv": reconcile(),
        "commission.csv": commission(),
        "cancellation-fee.csv": cancellation(),
        "backoff.csv": backoff(),
        "escrow-transitions.csv": escrow(),
        "escrow-predicates.csv": escrow_predicates(),
        "assignment-transitions.csv": assignment(),
        "payout-transitions.csv": payout(),
    }
    errors = []
    for name, (cols, rows) in tables.items():
        errors += validate(name, cols, rows)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        sys.exit(1)
    total = {}
    for name, (cols, rows) in tables.items():
        write(name, cols, rows)
        counts = {k: sum(1 for r in rows if r["dogrulama"] == k) for k in ("test", "kısmi", "kod", "hesap", "karar")}
        for k, v in counts.items():
            total[k] = total.get(k, 0) + v
        print(f"| [`{name}`]({name}) | {len(rows)} | " + " | ".join(str(counts[k]) for k in counts) + " |")
    print(f"| **Toplam** | **{sum(total.values())}** | " + " | ".join(f"**{total[k]}**" for k in total) + " |")


if __name__ == "__main__":
    main()
