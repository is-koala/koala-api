#!/usr/bin/env python3
"""authz-ciftleri.md tablosu ve internal/authz/testdata/matrix.yaml üreticisi.

Kaynak: docs/faz0/authz-envanteri.csv + aşağıdaki çift tanımları (Java kuralı, karar
kaydı D1-D5 ile hedef, §5.4 ve önerilen DSL karşılığı, matris satırları).

Kullanım (depo kökünden):  python3 -I docs/faz0/tarama/pairs.py
"""
import csv
import json
import sys
from collections import defaultdict

INV = "docs/faz0/authz-envanteri.csv"
MD = "docs/faz0/authz-ciftleri.md"
YAML_OUT = "internal/authz/testdata/matrix.yaml"
BEGIN, END = "<!-- tablo:başla (pairs.py üretir) -->", "<!-- tablo:bitir -->"

FGA = ["go", "openfga"]
GO = "go_only"
SKIP = "skip"

BASIS = {
    "worker": "atamanın işçisi (job_assignment#worker)",
    "individual_owner": "bireysel ilanın sahibi (job#individual_owner)",
    "company_member": "ilanın şirketinin üyesi, ADMIN ya da STAFF (company#member)",
    "company_admin": "şirket ADMIN'i (company#admin)",
    "applicant": "başvuru sahibi (job_application#applicant)",
    "applicant_pool": "açık ilana başvurabilecek herkes (job#applicant_pool)",
    "participant": "sohbet katılımcısı (conversation#participant)",
    "invitee": "davet edilen (company_invitation#invitee)",
    "inviter": "daveti gönderen (company_invitation#inviter)",
    "seat_holder": "serviste koltuğu olan işçi (shuttle#seat_holder)",
    "active_worker": "adresin göründüğü atama durumundaki işçi (job#active_worker)",
    "reviewer": "başvuru almış işveren tarafı (private_file#reviewer)",
    "owner": "kaydın sahibi (private_file#owner; model dışı tiplerde Scope SelfUserID)",
    "self": "kendisi (user#itself)",  # OpenFGA'da self ayrılmış sözcük
    "contact": "ortak sohbeti olan kullanıcı (user#contact)",
    "public": "herkese açık ([user:*] ya da kimliksiz)",
    "attribute_is_employer": "öznitelik: kullanıcı işveren ve kimliği doğrulanmış",
    "capability_url": "tahmin edilemez kimlik taşıyan URL",
    "provider_token": "sağlayıcı ödeme token'ı",
    "user_destination": "kullanıcının kendi WS kuyruğu",
}

# ----------------------------------------------------------------------------------
# Çiftler. Alanlar:
#   java: Java kuralı · target: Go hedefi (karar) · dec: dayanak karar
#   rel54: §5.4'teki karşılık · fit: §5.4'ün hedefi ifade etmesi (✓ ≈ ✗ dış)
#   rel: önerilen DSL relation'ı ya da go:<kural> · engine · jdiff: Java hedeften farklı mı
#   cases: (özne, eylem nesnesi, java, expect, basis|conceal, not, [motor])
#     allow satırında 5. öğe basis; deny satırında 5. öğe conceal (True/False).
#     java alanı: "allow" | "deny" | "deny!" (Java reddediyor ama ayırt edilebilir biçimde).
# ----------------------------------------------------------------------------------
P = []


def pair(obj, act, java, target, dec, rel54, fit, rel, engine, jdiff, cases, note=""):
    P.append(dict(obj=obj, act=act, java=java, target=target, dec=dec, rel54=rel54, fit=fit, rel=rel,
                  engine=engine, jdiff=jdiff, cases=cases, note=note))


A, D = "allow", "deny"
ED = "editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi"
FORMER = ("user:u_co_former", "Java JobEditors/partyOf creator'ı her zaman içeriyor; D1 şirket ilanında yalnız üyelik")

# ---- job_assignment
pair("job_assignment", "read", "işçi ∪ creator ∪ şirket yetkilisi (partyOf)", "işçi ∪ editor from job", "D1",
     "job_assignment#can_read", "✓", "job_assignment#can_read", FGA, "eski creator (D1)",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_co_staff", "job_assignment:asg_co", A, A, "company_member"),
      ("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True, {"jrow": "AZ-001"}),
      ("user:u_other_co_admin", "job_assignment:asg_co", D, D, True, {"jrow": "AZ-001"}),
      (FORMER[0], "job_assignment:asg_orphan", A, D, True, FORMER[1]),
      ("user:u_worker", "job_assignment:asg_deleted", D, D, True, {"note": "soft-delete: tuple'lar silinmiş, Check false", "jrow": "missing"})])
pair("job_assignment", "respond", "yalnız işçi", "= Java", "", "—", "✗", "job_assignment#can_respond", FGA, "",
     [("user:u_worker", "job_assignment:asg_pending", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_pending", D, D, True),
      ("user:u_stranger", "job_assignment:asg_pending", D, D, True)])
pair("job_assignment", "complete", "partyOf == EMPLOYER (creator ∪ yetkili)", "editor from job; açık uyuşmazlıkta ön süzgeç reddeder (D4)",
     "D1, D4", "job_assignment#can_complete", "✓", "job_assignment#can_complete", FGA, "eski creator (D1); uyuşmazlık (D4, oracle)",
     [("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_co_admin", "job_assignment:asg_co", A, A, "company_member"),
      ("user:u_worker", "job_assignment:asg_ind", D, D, True),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True),
      (FORMER[0], "job_assignment:asg_orphan", A, D, True, FORMER[1])],
     "D4 ilişki değil durum kuralıdır: testdata/oracle/assignment-transitions.csv AT-09, AT-14.")
pair("job_assignment", "pay", "BİREYSEL: creator; ŞİRKET: güncel yetkili; durum kontrolü yetkiden önce",
     "editor from job; yetki kontrolü durumdan önce", "D1, Java hatası", "job_assignment#can_pay (editor from job)", "≈",
     "job_assignment#can_pay", FGA, "ret biçimi (403) ve kontrol sırası",
     [("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_co_staff", "job_assignment:asg_co", A, A, "company_member"),
      ("user:u_worker", "job_assignment:asg_ind", "deny!", D, True),
      (FORMER[0], "job_assignment:asg_orphan", "deny!", D, True, "D1 ile Java aynı karar"),
      ("user:u_stranger", "job_assignment:asg_ind", "deny!", D, True)],
     "§5.4'teki editor creator'ı her zaman içerdiği için ≈; önerilen DSL'de editor D1'e göre düzeltildi.")
pair("job_assignment", "cancel", "taraf (partyOf)", "party", "D1", "job_assignment#can_cancel", "✓", "job_assignment#can_cancel", FGA,
     "eski creator (D1)",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True),
      (FORMER[0], "job_assignment:asg_orphan", A, D, True, FORMER[1])])
pair("job_assignment", "record_work", "yalnız işçi (check-in/out)", "= Java", "", "job_assignment#can_check_in", "✓",
     "job_assignment#can_record_work", FGA, "",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_ind", D, D, True),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True)])
pair("job_assignment", "open_dispute", "taraf; emanet ön süzgeç", "party", "D1", "—", "✗", "job_assignment#can_open_dispute", FGA,
     "ret biçimi (farklı 404)",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_co_admin", "job_assignment:asg_co", A, A, "company_member"),
      ("user:u_stranger", "job_assignment:asg_ind", "deny!", D, True)])
pair("job_assignment", "review", "taraf; COMPLETED ön süzgeç", "party", "D1", "—", "✗", "job_assignment#can_review", FGA, "",
     [("user:u_worker", "job_assignment:asg_done", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_done", A, A, "individual_owner"),
      ("user:u_stranger", "job_assignment:asg_done", D, D, True)])
pair("job_assignment", "reveal_worker_identity", "işveren tarafı + HIRED + oran sınırı", "editor from job + HIRED ön süzgeç",
     "D1", "—", "✗", "job_assignment#can_reveal_worker_identity", FGA, "eski creator (D1)",
     [("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_worker", "job_assignment:asg_ind", D, D, True),
      ("user:u_other_co_admin", "job_assignment:asg_co", D, D, True),
      ("user:u_creator", "job_assignment:asg_pending", D, D, True, "HIRED değil: ön süzgeç", GO)])
pair("job_assignment", "select_seat", "taraf (işveren işçi adına seçebilir)", "party", "D1", "—", "✗",
     "job_assignment#can_select_seat", FGA, "",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True)])
pair("job_assignment", "issue_boarding_qr", "taraf VE işçi", "worker", "", "—", "✗", "job_assignment#can_board", FGA, "",
     [("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_creator", "job_assignment:asg_ind", D, D, True),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, True)])
pair("job_assignment", "list", "işçi → worker_id; işveren → yalnız creator", "Scope(can_read): liste tekil okumayla aynı",
     "D2", "Scope(job_assignment#can_read)", "✓", "Scope(job_assignment#can_read)", GO, "şirket yetkilisi listede görmüyor (D2)",
     [("user:u_creator", "job_assignment:asg_ind", A, A, "individual_owner"),
      ("user:u_worker", "job_assignment:asg_ind", A, A, "worker"),
      ("user:u_co_admin", "job_assignment:asg_co", D, A, "company_member", "D2: tekil okuyabildiğini listede de görür"),
      ("user:u_stranger", "job_assignment:asg_ind", D, D, False, "liste süzgeci: satır listede yok")])
# ---- job
pair("job", "read", "herkes, her durumda", "herkes, her durumda (kapanmış/iptal ilan 'süresi doldu' gösterilir); soft-delete'te yok",
     "D3", "job#can_view (public_viewer yalnız OPEN)", "≈", "job#can_view", FGA, "",
     [("anonymous", "job:job_ind", A, A, "public", "", GO),
      ("user:u_stranger", "job:job_ind", A, A, "public"),
      ("user:u_stranger", "job:job_closed", A, A, "public", "D3"),
      ("user:u_stranger", "job:job_deleted", D, D, True, {"note": "soft-delete: public_viewer tuple'ı silinmiş", "jrow": "missing"})])
pair("job", "read_employer_view", "detayda editor, aramada yalnız creator", "her yerde editor", "D2, D1", "job#editor", "✓",
     "job#can_edit", FGA, "aramada şirket yetkilisi (D2)",
     [("user:u_co_admin", "job:job_co", A, A, "company_member", "detay"),
      ("user:u_co_admin", "job:job_co", D, A, "company_member", "D2: arama listesinde de editor görünümü"),
      ("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_stranger", "job:job_ind", D, D, False, "alan düzeyi: herkese açık görünüm")])
pair("job", "read_exact_address", "creator ∪ atanmış işçi ∪ şirket adresi açık",
     "individual_owner ∪ can_manage_jobs from owner_company ∪ aktif atamadaki işçi ∪ adres açık (şirket adresi iş yeri bilgisi)", "D1",
     "—", "✗", "job#can_read_exact_address", FGA, "şirket üyeleri ve eski creator (D1)",
     [("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_worker", "job:job_ind", A, A, "active_worker"),
      ("user:u_stranger", "job:job_co_public_addr", A, A, "public"),
      ("user:u_co_admin", "job:job_co", D, A, "company_member", "D1 (karar 05.11): şirket ilanının adresi iş yeri bilgisidir, can_manage_jobs üyeleri görür. Java yalnız ilanı açana gösteriyor: CompanyJobListingRulesTest#theSameJobIsExactForTheContactWhoOpenedItAndApproximateForTheOther"),
      ("user:u_stranger", "job:job_ind", D, D, False, "alan düzeyi: maskeli adres"),
      (FORMER[0], "job:job_co_orphan", A, D, False, FORMER[1])])
pair("job", "edit", "editor (JobEditors)", ED, "D1", "job#can_edit", "✓", "job#can_edit", FGA, "eski creator (D1)",
     [("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_co_admin", "job:job_co", A, A, "company_member"),
      ("user:u_worker", "job:job_ind", D, D, True, {"jrow": "AZ-022"}),
      ("user:u_stranger", "job:job_ind", D, D, True, {"jrow": "AZ-022"}),
      (FORMER[0], "job:job_co_orphan", A, D, True, FORMER[1])])
pair("job", "read_private", "editor", ED, "D1", "job#editor", "✓", "job#can_read_private", FGA, "eski creator (D1)",
     [("user:u_co_staff", "job:job_co", A, A, "company_member"),
      ("user:u_worker", "job:job_co", D, D, True),
      ("user:u_stranger", "job:job_co", D, D, True),
      (FORMER[0], "job:job_co_orphan", A, D, True, FORMER[1])])
pair("job", "list_applications", "BİREYSEL: creator (404); ŞİRKET: güncel yetkili (400)", ED, "D1",
     "job_application#can_read (editor from job)", "≈", "job#can_list_applications", FGA, "ret biçimi (400 / farklı 404)",
     [("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_co_admin", "job:job_co", A, A, "company_member"),
      (FORMER[0], "job:job_co_orphan", "deny!", D, True, "D1 ile Java aynı karar"),
      ("user:u_stranger", "job:job_ind", "deny!", D, True)])
pair("job", "cancel", "editor", ED, "D1", "job#can_edit", "✓", "job#can_edit", FGA, "eski creator (D1)",
     [("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_worker", "job:job_ind", D, D, True),
      (FORMER[0], "job:job_co_orphan", A, D, True, FORMER[1])])
pair("job", "close", "editor", ED, "D1", "job#can_edit", "✓", "job#can_edit", FGA, "eski creator (D1)",
     [("user:u_co_staff", "job:job_co", A, A, "company_member"),
      ("user:u_stranger", "job:job_co", D, D, True),
      (FORMER[0], "job:job_co_orphan", A, D, True, FORMER[1])])
pair("job", "apply", "editor olmayan herkes; OPEN; hazır olma", "applicant_pool but not editor (applicant_pool yalnız OPEN iken)",
     "D3", "—", "✗", "job#can_apply", FGA, "",
     [("user:u_applicant", "job:job_ind", A, A, "applicant_pool"),
      ("user:u_creator", "job:job_ind", D, D, False),
      ("user:u_co_staff", "job:job_co", D, D, False),
      ("user:u_applicant", "job:job_closed", D, D, False, "D3: kapanmış ilan başvuruya kapalı")])
pair("job", "open_conversation", "creator ∪ yetkili herhangi bir işçiyle; işçi kendisi için; ödeme ve engelleme ön süzgeç",
     "editor (+ ödeme/engelleme ön süzgeci)", "D1", "—", "✗", "job#can_open_conversation", FGA, "eski creator (D1)",
     [("user:u_creator", "job:job_ind", A, A, "individual_owner"),
      ("user:u_stranger", "job:job_ind", "deny!", D, False),
      ("user:u_blocked", "job:job_ind", D, D, False, "engelleme ön süzgeci", GO),
      (FORMER[0], "job:job_co_orphan", A, D, False, FORMER[1])])
pair("job", "copy_as_template", "editor (JobEditors kopyası)", ED, "D1", "job#can_edit", "✓", "job#can_edit", FGA, "eski creator (D1)",
     [("user:u_co_staff", "job:job_co", A, A, "company_member"),
      ("user:u_stranger", "job:job_co", D, D, True),
      (FORMER[0], "job:job_co_orphan", A, D, True, FORMER[1])])
# ---- company
pair("company", "read", "kimlikli her kullanıcı; galeri herkese", "= Java", "", "—", "✗", "company#viewer", FGA, "",
     [("user:u_stranger", "company:co_1", A, A, "public"),
      ("anonymous", "company:co_1", D, D, False, {"note": "kimlik doğrulaması yok", "jrow": "authn"}, GO)])
pair("company", "read_member_view", "üye: onay notu, kişi listesi, şirket ilanları", "= Java", "", "company#member", "✓",
     "company#can_read_internal", FGA, "ret biçimi (404/403/maske)",
     [("user:u_co_staff", "company:co_1", A, A, "company_member"),
      ("user:u_co_admin", "company:co_1", A, A, "company_admin"),
      ("user:u_stranger", "company:co_1", "deny!", D, False),
      ("user:u_other_co_admin", "company:co_1", "deny!", D, False)])
pair("company", "manage", "yalnız ADMIN", "= Java", "", "company#can_manage", "✓", "company#can_manage", FGA, "",
     [("user:u_co_admin", "company:co_1", A, A, "company_admin"),
      ("user:u_co_staff", "company:co_1", "deny!", D, False),
      ("user:u_stranger", "company:co_1", "deny!", D, False)])
pair("company", "create_job", "üye", "= Java", "", "company#can_manage_jobs", "✓", "company#can_manage_jobs", FGA, "ret biçimi (400)",
     [("user:u_co_staff", "company:co_1", A, A, "company_member"),
      ("user:u_stranger", "company:co_1", "deny!", D, False)])
pair("company", "tag_talent_pool_entry", "KONTROL YOK", "üye", "", "—", "✗", "company#can_manage_jobs", FGA, "kontrol yok",
     [("user:u_co_admin", "company:co_1", A, A, "company_member"),
      ("user:u_stranger", "company:co_1", A, D, False, "Java açığı")])
# ---- company_invitation
pair("company_invitation", "read", "davet eden ∪ davet edilen ∪ şirket ADMIN'i", "= Java", "", "—", "✗",
     "company_invitation#can_read", FGA, "",
     [("user:u_invitee", "company_invitation:inv_1", A, A, "invitee"),
      ("user:u_co_admin", "company_invitation:inv_1", A, A, "inviter"),  # u_co_admin daveti de gönderdi; model önce inviter'ı bulur
      ("user:u_co_admin2", "company_invitation:inv_1", A, A, "company_admin"),  # admin from company dalı: daveti göndermemiş ADMIN
      ("user:u_co_staff", "company_invitation:inv_1", D, D, True),
      ("user:u_stranger", "company_invitation:inv_1", D, D, True)])
pair("company_invitation", "answer", "yalnız davet edilen", "= Java", "", "—", "✗", "company_invitation#can_answer", FGA, "",
     [("user:u_invitee", "company_invitation:inv_1", A, A, "invitee"),
      ("user:u_co_admin", "company_invitation:inv_1", D, D, True),
      ("user:u_stranger", "company_invitation:inv_1", D, D, True)])
# ---- job_application
pair("job_application", "decide", "BİREYSEL: creator; ŞİRKET: güncel yetkili", "editor from job", "D1",
     "job_application#can_decide (editor from job)", "≈", "job_application#can_decide", FGA, "",
     [("user:u_creator", "job_application:app_ind", A, A, "individual_owner"),
      ("user:u_co_admin", "job_application:app_co", A, A, "company_member"),
      ("user:u_applicant", "job_application:app_ind", D, D, True),
      (FORMER[0], "job_application:app_orphan", D, D, True, "D1 ile Java aynı karar"),
      ("user:u_stranger", "job_application:app_co", D, D, True)],
     "§5.4 editor'ı creator'ı içerdiği için ≈; önerilen DSL'de düzeldi.")
pair("job_application", "withdraw", "yalnız başvuran", "= Java", "", "job_application#can_withdraw", "✓",
     "job_application#can_withdraw", FGA, "",
     [("user:u_applicant", "job_application:app_ind", A, A, "applicant"),
      ("user:u_creator", "job_application:app_ind", D, D, True),
      ("user:u_stranger", "job_application:app_ind", D, D, True)])
pair("job_application", "list", "creator → bireysel ilanlar; yetkili → şirket ilanları", "Scope(can_read)", "D1",
     "Scope(job_application#can_read)", "≈", "Scope(job_application#can_read)", GO, "",
     [("user:u_creator", "job_application:app_ind", A, A, "individual_owner"),
      ("user:u_co_admin", "job_application:app_co", A, A, "company_member"),
      (FORMER[0], "job_application:app_orphan", D, D, False, "liste süzgeci"),
      ("user:u_stranger", "job_application:app_ind", D, D, False, "liste süzgeci")])
# ---- conversation / message
pair("conversation", "read", "katılımcı", "= Java", "", "conversation#can_read", "✓", "conversation#can_read", FGA, "",
     [("user:u_worker", "conversation:conv_ind", A, A, "participant"),
      ("user:u_stranger", "conversation:conv_ind", D, D, True, {"jrow": "AZ-064"})])
pair("conversation", "post", "katılımcı + engelleme + ödeme/pencere ön süzgeçleri", "= Java", "", "conversation#can_post", "✓",
     "conversation#can_post", FGA, "",
     [("user:u_creator", "conversation:conv_ind", A, A, "participant"),
      ("user:u_stranger", "conversation:conv_ind", D, D, True, {"jrow": "AZ-068"}),
      ("user:u_blocked", "conversation:conv_blocked", D, D, False, {"note": "engelleme ön süzgeci", "jrow": "AZ-072"}, GO)])
pair("conversation", "subscribe", "katılımcı (STOMP)", "katılımcı (K4 düz WS)", "", "conversation#can_read", "✓",
     "conversation#can_read", FGA, "",
     [("user:u_worker", "conversation:conv_ind", A, A, "participant"),
      ("user:u_stranger", "conversation:conv_ind", D, D, False, "WS abonelik reddi")])
pair("conversation", "publish_to_topic", "KONTROL YOK (doğrulanmadı)", "istemci konu yayını yapamaz (K4 sunucu-itmeli)", "K4",
     "—", "dış", "go:protocol_no_client_publish", SKIP, "kontrol yok",
     [("user:u_worker", "conversation:conv_ind", A, D, False, "K4: istemciden yayın yok"),
      ("user:u_stranger", "conversation:conv_ind", A, D, False, "Java açığı (doğrulanmadı)")],
     "Relation gerekmez; WS protokol testi kapatır. Bu çiftte izinli hedef satırı yoktur.")
pair("conversation", "list", "katılımcı", "Scope(can_read)", "", "Scope(conversation#can_read)", "✓", "Scope(conversation#can_read)", GO, "",
     [("user:u_worker", "conversation:conv_ind", A, A, "participant"),
      ("user:u_stranger", "conversation:conv_ind", D, D, False, "liste süzgeci")])
pair("message", "report", "mesajın sohbetinde katılımcı; gönderen kendi mesajını raporlayamaz", "= Java", "", "—", "✗",
     "message#can_report", FGA, "",
     [("user:u_worker", "message:msg_ind", A, A, "participant"),
      ("user:u_stranger", "message:msg_ind", D, D, True),
      ("user:u_creator", "message:msg_ind", D, D, False, {"note": "gönderen kendi mesajını raporlayamaz (REPORT_CANNOT_REPORT_SELF; envanterde iş kuralı, ayrı satır yok)", "jrow": "distinct"})])
# ---- dispute
pair("dispute", "read", "atamanın tarafı", "party from assignment", "D1", "dispute#can_read", "✓", "dispute#can_read", FGA, "",
     [("user:u_worker", "dispute:dsp_ind", A, A, "worker"),
      ("user:u_creator", "dispute:dsp_ind", A, A, "individual_owner"),
      ("user:u_stranger", "dispute:dsp_ind", D, D, True, {"jrow": "AZ-082"})])
pair("dispute", "add_evidence", "atamanın tarafı; RESOLVED ise 409", "party from assignment", "D1", "dispute#can_add_evidence", "✓",
     "dispute#can_add_evidence", FGA, "",
     [("user:u_worker", "dispute:dsp_ind", A, A, "worker"),
      ("user:u_stranger", "dispute:dsp_ind", D, D, True)])
# ---- transport
pair("shuttle", "manage", "ilanın editor'ı", "editor from job", "D1", "—", "✗", "shuttle#can_manage", FGA, "",
     [("user:u_creator", "shuttle:shuttle_ind", A, A, "individual_owner"),
      ("user:u_worker", "shuttle:shuttle_ind", D, D, True),
      ("user:u_stranger", "shuttle:shuttle_ind", D, D, True)])
pair("shuttle", "board", "koltuk sahibi; koltuksuz taraf 409, yabancı 404", "= Java", "", "—", "✗", "shuttle#can_board", FGA, "",
     [("user:u_worker", "shuttle:shuttle_ind", A, A, "seat_holder"),
      ("user:u_stranger", "shuttle:shuttle_ind", D, D, True)])
pair("shuttle", "read_details", "işveren tarafı tümü; işçi yalnız koltuğu olan servis (VIEW_STATUSES)",
     "can_manage ∪ seat_holder (+ atama durumu ön süzgeci)", "", "—", "✗", "shuttle#can_read_details", FGA, "",
     [("user:u_creator", "shuttle:shuttle_ind", A, A, "individual_owner"),
      ("user:u_worker", "shuttle:shuttle_ind", A, A, "seat_holder"),
      ("user:u_worker", "shuttle:shuttle_other", D, D, False, "alan düzeyi: plaka/şoför gizli")])
pair("seat_assignment", "mark_boarding", "koltuğun ilanının editor'ı", "can_manage from shuttle", "D1", "—", "✗",
     "seat_assignment#can_mark_boarding", FGA, "",
     [("user:u_creator", "seat_assignment:seat_w", A, A, "individual_owner"),
      ("user:u_stranger", "seat_assignment:seat_w", D, D, True),
      ("user:u_worker", "seat_assignment:seat_w", D, D, True)])
# ---- user / dosya
pair("user", "read_public", "kimliksiz dahil herkes: deneyim, müsaitlik, rozet, yorum, puan, işveren istatistiği", "= Java", "",
     "—", "✗", "user#public_viewer", FGA, "",
     [("user:u_stranger", "user:u_worker", A, A, "public"),
      ("anonymous", "user:u_worker", A, A, "public", "kimliksiz okuma", GO),
      ("user:u_stranger", "user:u_deleted", D, D, True, {"note": "soft-delete: public_viewer tuple'ı silinmiş", "jrow": "missing"})])
pair("user", "read_public_profile", "kimlikli her kullanıcı (profil)", "= Java", "", "—", "✗", "user#public_viewer", FGA, "",
     [("user:u_stranger", "user:u_worker", A, A, "public"),
      ("anonymous", "user:u_worker", D, D, False, {"note": "profil kimlik ister", "jrow": "authn"}, GO),
      ("user:u_stranger", "user:u_deleted", D, D, True, {"note": "soft-delete", "jrow": "missing"})])
pair("user", "read_cv_url", "kendisi ∪ başvuru almış işveren (projeksiyon)", "private_file#can_download", "D5", "—", "✗",
     "private_file#can_download", FGA, "",
     [("user:u_applicant", "private_file:cv_applicant", A, A, "owner"),  # model CV'yi private_file#owner ile verir
      ("user:u_creator", "private_file:cv_applicant", A, A, "reviewer"),
      ("user:u_stranger", "private_file:cv_applicant", D, D, False, "alan düzeyi: CV bağlantısı yok")])
pair("private_file", "download", "sahip ∪ ROLE_ADMIN ∪ başvuru almış işveren", "sahip ∪ reviewer (ROLE_ADMIN yok)", "D5", "—", "✗",
     "private_file#can_download", FGA, "ROLE_ADMIN baypası (D5)",
     [("user:u_applicant", "private_file:cv_applicant", A, A, "owner"),
      ("user:u_co_staff", "private_file:cv_applicant", A, A, "reviewer"),
      ("user:u_stranger", "private_file:cv_applicant", D, D, True),
      ("user:u_mkt_admin", "private_file:cv_applicant", A, D, True, "D5: pazaryeri ROLE_ADMIN baypası kalkar")])
pair("user", "read_presence", "REST'te KONTROL YOK", "self ∪ contact", "", "—", "✗", "user#can_see_presence", FGA, "kontrol yok",
     [("user:u_creator", "user:u_worker", A, A, "contact"),
      ("user:u_stranger", "user:u_worker", A, D, False, "Java açığı"),
      ("anonymous", "user:u_worker", D, D, False, {"note": "kimlik doğrulaması yok", "jrow": "authn"}, GO)])
pair("user", "watch_presence", "kendisi ∪ ortak sohbet (STOMP)", "self ∪ contact", "", "—", "✗", "user#can_see_presence", FGA, "",
     [("user:u_worker", "user:u_worker", A, A, "self"),
      ("user:u_creator", "user:u_worker", A, A, "contact"),
      ("user:u_stranger", "user:u_worker", D, D, False)])
pair("user", "create_individual_job", "öznitelik: isEmployer + kimlik doğrulanmış", "= Java", "", "—", "dış",
     "go:attribute_is_employer", GO, "",
     [("user:u_creator", "user:u_creator", A, A, "attribute_is_employer"),
      ("user:u_worker", "user:u_worker", D, D, False)])
pair("image", "delete", "yükleyen ∪ ROLE_ADMIN", "yalnız yükleyen", "D5", "owner (Scope)", "dış", "go:owner", GO,
     "ROLE_ADMIN baypası (D5); ret biçimi (400)",
     [("user:u_stranger", "image:img_x", A, A, "owner"),
      ("user:u_worker", "image:img_x", "deny!", D, True),
      ("user:u_mkt_admin", "image:img_x", A, D, True, "D5")])
pair("platform", "read_actuator", "pazaryeri ROLE_ADMIN", "pazaryeri öznesine kapalı (operasyon uçları dış yüzeyde yok)", "D5",
     "—", "dış", "go:no_marketplace_admin", GO, "ROLE_ADMIN baypası (D5)",
     [("user:u_mkt_admin", "platform:koala", A, D, False, "D5"),
      ("user:u_worker", "platform:koala", D, D, False)],
     "Bu çiftte izinli hedef satırı yoktur; personel yüzeyi kapsam dışı.")
# ---- kendi verin
for obj, fix in [("experience", "exp_w"), ("education", "edu_w"), ("job_alert", "alert_w"), ("bank_account", "bank_w"),
                 ("device", "dev_w"), ("availability_exception", "avex_w")]:
    pair(obj, "write", "yalnız sahibi", "= Java", "", "owner (model dışı)", "dış", "go:owner", GO, "",
         [("user:u_worker", f"{obj}:{fix}", A, A, "owner"), ("user:u_stranger", f"{obj}:{fix}", D, D, True)])
pair("job_template", "read", "yalnız sahibi", "= Java", "", "owner", "dış", "go:owner", GO, "",
     [("user:u_creator", "job_template:tmpl_c", A, A, "owner"), ("user:u_stranger", "job_template:tmpl_c", D, D, True)])
pair("job_template", "write", "yalnız sahibi", "= Java", "", "owner", "dış", "go:owner", GO, "",
     [("user:u_creator", "job_template:tmpl_c", A, A, "owner"), ("user:u_stranger", "job_template:tmpl_c", D, D, True)])
pair("session", "read", "yalnız sahibi", "= Java", "", "owner", "dış", "go:owner", GO, "",
     [("user:u_worker", "session:ses_w", A, A, "owner"), ("user:u_stranger", "session:ses_w", D, D, True)])
pair("session", "write", "yalnız sahibi", "= Java", "", "owner", "dış", "go:owner", GO, "",
     [("user:u_worker", "session:ses_w", A, A, "owner"), ("user:u_stranger", "session:ses_w", D, D, True)])
pair("user_verification", "confirm", "yalnız sahibi; ret 403", "yalnız sahibi; gizli ret", "", "owner", "dış", "go:owner", GO,
     "ret biçimi (403)",
     [("user:u_worker", "user_verification:ver_w", A, A, "owner"), ("user:u_stranger", "user_verification:ver_w", "deny!", D, True)])
pair("notification", "mark_read", "yalnız alıcı; başkasınınki sessiz 200", "yalnız alıcı; gizli ret", "", "owner", "dış", "go:owner",
     GO, "sessiz kabul",
     [("user:u_worker", "notification:notif_w", A, A, "owner"),
      ("user:u_stranger", "notification:notif_w", A, D, True, "Java 200 döner ama değişiklik yapmaz")])
pair("notification", "mark_opened", "kimliksiz; bildirim UUID'si", "= Java", "", "—", "dış", "go:capability_url", GO, "",
     [("anonymous", "notification:notif_w", A, A, "capability_url"),
      ("anonymous", "notification:notif_missing", D, D, False, {"note": "var olmayan bildirim; yetki değil", "jrow": "missing_distinct"})])
pair("address_search_session", "use", "bellek içi oturumun sahibi; ret 400", "= Java", "", "owner", "dış", "go:owner", GO, "",
     [("user:u_worker", "address_search_session:addr_w", A, A, "owner"),
      ("user:u_stranger", "address_search_session:addr_w", "deny!", D, False)])
pair("payment_attempt", "complete_callback", "kimliksiz; sağlayıcı token'ı + sunucudan sorgu", "= Java", "", "—", "dış",
     "go:provider_token", SKIP, "",
     [("anonymous", "payment_attempt:pay_valid", A, A, "provider_token"),
      ("anonymous", "payment_attempt:pay_unknown", D, D, False, {"note": "bilinmeyen token", "jrow": "missing_distinct"})],
     "Yetki değil sağlayıcı doğrulaması: testdata/oracle/reconcile.csv RC-01.")
pair("payment_attempt", "read_outcome", "kimliksiz; kayıtlı sonucu okur", "= Java", "", "—", "dış", "go:provider_token", SKIP, "",
     [("anonymous", "payment_attempt:pay_valid", A, A, "provider_token"),
      ("anonymous", "payment_attempt:pay_unknown", D, D, False, {"note": "bilinmeyen token başarısız sonuç sayfasına yönlenir", "jrow": "missing_distinct"})])
pair("realtime_destination", "subscribe", "bilinen hedef dışı SUBSCRIBE reddedilir", "= Java (K4 protokolünde)", "K4", "—", "dış",
     "go:ws_destination", GO, "",
     [("user:u_worker", "realtime_destination:queue_own", A, A, "user_destination"),
      ("user:u_worker", "realtime_destination:topic_unknown", D, D, False)])

# ----------------------------------------------------------------------------------
USERS = {
    "u_worker": dict(is_employer=False, note="asg_ind, asg_co, asg_orphan, asg_pending, asg_done işçisi; conv_ind katılımcısı; seat_w"),
    "u_creator": dict(is_employer=True, note="bireysel işveren; job_ind ve job_closed sahibi; tmpl_c sahibi"),
    "u_co_admin": dict(is_employer=True, note="co_1 ADMIN"),
    "u_co_admin2": dict(is_employer=True, note="co_1'in ikinci ADMIN'i; inv_1'i göndermedi"),
    "u_co_staff": dict(is_employer=True, note="co_1 STAFF; job_co'yu açan"),
    "u_co_former": dict(is_employer=True, note="co_1'den çıkarılmış eski yetkili; job_co_orphan'ı açan"),
    "u_other_co_admin": dict(is_employer=True, note="co_2 ADMIN; co_1 ile ilişkisi yok"),
    "u_applicant": dict(is_employer=False, note="app_ind, app_co, app_orphan sahibi; cv_applicant sahibi"),
    "u_invitee": dict(is_employer=False, note="inv_1 ile co_1'e davet edilen"),
    "u_blocked": dict(is_employer=False, note="u_worker'ın engellediği kullanıcı; conv_blocked katılımcısı"),
    "u_stranger": dict(is_employer=False, note="hiçbir nesneyle ilişkisi yok; img_x'i yükleyen"),
    "u_mkt_admin": dict(is_employer=False, marketplace_role="ROLE_ADMIN", note="pazaryeri ROLE_ADMIN (D5: yetki vermez)"),
    "u_deleted": dict(is_employer=False, deleted=True, note="soft-delete edilmiş kullanıcı; tuple'ı yok"),
}
OBJECTS = {
    "platform:koala": {},
    "company:co_1": dict(approval="APPROVED"),
    "company:co_2": dict(approval="APPROVED"),
    "job:job_ind": dict(employer_type="INDIVIDUAL", status="OPEN"),
    "job:job_closed": dict(employer_type="INDIVIDUAL", status="CLOSED"),
    "job:job_co": dict(employer_type="COMPANY", status="OPEN", address_public=False),
    "job:job_co_public_addr": dict(employer_type="COMPANY", status="OPEN", address_public=True),
    "job:job_co_orphan": dict(employer_type="COMPANY", status="OPEN", address_public=False),
    "job:job_deleted": dict(employer_type="INDIVIDUAL", status="OPEN", deleted=True),
    "job_application:app_ind": dict(status="APPLIED"),
    "job_application:app_co": dict(status="APPLIED"),
    "job_application:app_orphan": dict(status="APPLIED"),
    "job_assignment:asg_ind": dict(status="CONFIRMED", payment_status="HELD_IN_ESCROW"),
    "job_assignment:asg_pending": dict(status="PENDING_CONFIRMATION", payment_status=None),
    "job_assignment:asg_done": dict(status="COMPLETED", payment_status="RELEASED_TO_WORKER"),
    "job_assignment:asg_co": dict(status="CONFIRMED", payment_status="HELD_IN_ESCROW"),
    "job_assignment:asg_orphan": dict(status="CONFIRMED", payment_status="PENDING_COLLECTION"),
    "job_assignment:asg_deleted": dict(status="CONFIRMED", payment_status="HELD_IN_ESCROW", deleted=True),
    "dispute:dsp_ind": dict(status="OPEN"),
    "conversation:conv_ind": dict(kind="DIRECT"),
    "conversation:conv_blocked": dict(kind="DIRECT", blocked_pair=["user:u_worker", "user:u_blocked"]),
    "message:msg_ind": {},
    "company_invitation:inv_1": dict(status="PENDING"),
    "shuttle:shuttle_ind": {},
    "shuttle:shuttle_other": {},
    "seat_assignment:seat_w": {},
    "private_file:cv_applicant": dict(kind="CV"),
    "image:img_x": dict(owner="user:u_stranger"),
    "experience:exp_w": dict(owner="user:u_worker"),
    "education:edu_w": dict(owner="user:u_worker"),
    "job_alert:alert_w": dict(owner="user:u_worker"),
    "bank_account:bank_w": dict(owner="user:u_worker"),
    "device:dev_w": dict(owner="user:u_worker"),
    "availability_exception:avex_w": dict(owner="user:u_worker"),
    "job_template:tmpl_c": dict(owner="user:u_creator"),
    "session:ses_w": dict(owner="user:u_worker", active=True),
    "user_verification:ver_w": dict(owner="user:u_worker"),
    "notification:notif_w": dict(owner="user:u_worker", channel="EMAIL", delivered=True),
    "notification:notif_missing": dict(exists=False),
    "address_search_session:addr_w": dict(owner="user:u_worker"),
    "payment_attempt:pay_valid": dict(provider_token="known"),
    "payment_attempt:pay_unknown": dict(provider_token="unknown"),
    "realtime_destination:queue_own": dict(destination="/user/queue/notifications"),
    "realtime_destination:topic_unknown": dict(destination="/topic/admin/secrets"),
}
for u, ua in USERS.items():                    # kullanıcılar nesne olarak da çözülür (başlık sözleşmesi)
    OBJECTS[f"user:{u}"] = {k: v for k, v in ua.items() if k != "note"}
TUPLES = [
    "company:co_1#admin@user:u_co_admin", "company:co_1#admin@user:u_co_admin2", "company:co_1#staff_member@user:u_co_staff",
    "company:co_2#admin@user:u_other_co_admin",
    "company:co_1#viewer@user:*", "company:co_2#viewer@user:*",          # C24: her şirkette yazılır
    "job:job_ind#individual_owner@user:u_creator", "job:job_ind#creator@user:u_creator",
    "job:job_ind#public_viewer@user:*", "job:job_ind#applicant_pool@user:*", "job:job_ind#active_worker@user:u_worker",
    "job:job_closed#individual_owner@user:u_creator", "job:job_closed#creator@user:u_creator",
    "job:job_closed#public_viewer@user:*",
    "job:job_co#owner_company@company:co_1", "job:job_co#creator@user:u_co_staff", "job:job_co#public_viewer@user:*",
    "job:job_co#applicant_pool@user:*", "job:job_co#active_worker@user:u_worker",
    "job:job_co_public_addr#owner_company@company:co_1", "job:job_co_public_addr#public_viewer@user:*",
    "job:job_co_public_addr#public_address_viewer@user:*",
    "job:job_co_orphan#owner_company@company:co_1", "job:job_co_orphan#creator@user:u_co_former",
    "job:job_co_orphan#public_viewer@user:*", "job:job_co_orphan#applicant_pool@user:*",
    "job_application:app_ind#job@job:job_ind", "job_application:app_ind#applicant@user:u_applicant",
    "job_application:app_co#job@job:job_co", "job_application:app_co#applicant@user:u_applicant",
    "job_application:app_orphan#job@job:job_co_orphan", "job_application:app_orphan#applicant@user:u_applicant",
    "job_assignment:asg_ind#job@job:job_ind", "job_assignment:asg_ind#worker@user:u_worker",
    "job_assignment:asg_pending#job@job:job_ind", "job_assignment:asg_pending#worker@user:u_worker",
    "job_assignment:asg_done#job@job:job_ind", "job_assignment:asg_done#worker@user:u_worker",
    "job_assignment:asg_co#job@job:job_co", "job_assignment:asg_co#worker@user:u_worker",
    "job_assignment:asg_orphan#job@job:job_co_orphan", "job_assignment:asg_orphan#worker@user:u_worker",
    "dispute:dsp_ind#assignment@job_assignment:asg_ind",
    "conversation:conv_ind#participant@user:u_creator", "conversation:conv_ind#participant@user:u_worker",
    "conversation:conv_blocked#participant@user:u_worker", "conversation:conv_blocked#participant@user:u_blocked",
    "message:msg_ind#conversation@conversation:conv_ind", "message:msg_ind#sender@user:u_creator",
    "company_invitation:inv_1#company@company:co_1", "company_invitation:inv_1#inviter@user:u_co_admin",
    "company_invitation:inv_1#invitee@user:u_invitee",
    "shuttle:shuttle_ind#job@job:job_ind", "shuttle:shuttle_ind#seat_holder@user:u_worker",
    "shuttle:shuttle_other#job@job:job_ind",
    "seat_assignment:seat_w#shuttle@shuttle:shuttle_ind",
    "private_file:cv_applicant#owner@user:u_applicant", "private_file:cv_applicant#reviewer@user:u_creator",
    "private_file:cv_applicant#reviewer@company:co_1#member",
    "user:u_worker#public_viewer@user:*", "user:u_applicant#public_viewer@user:*",
    "user:u_worker#itself@user:u_worker", "user:u_creator#itself@user:u_creator",
    "user:u_worker#contact@user:u_creator", "user:u_creator#contact@user:u_worker",
]


def q(v):
    return json.dumps(v, ensure_ascii=False)


CONCEALED = {"evet", "evet-yardımcısız"}


def norm_case(c, pair_engine):
    """(özne, nesne, java, expect, basis|conceal, [not|{seçenek}], [motor]) -> dict"""
    d = dict(subj=c[0], obj=c[1], java=c[2], expect=c[3], bc=c[4], note="", jrow=None, engine=pair_engine)
    for extra in c[5:]:
        if isinstance(extra, dict):
            d["note"] = extra.get("note", d["note"])
            d["jrow"] = extra.get("jrow")
        elif isinstance(extra, str) and extra in (GO, SKIP):
            d["engine"] = extra
        elif isinstance(extra, list):
            d["engine"] = extra
        elif isinstance(extra, str):
            d["note"] = extra
    return d


def java_conceal(case, rows, by_id):
    """Java'nın ret biçimi envanterdeki gizleme_404 kolonundan türetilir; varsayılan basılmaz."""
    j = case["jrow"]
    if j == "authn":            # kimlik doğrulaması yok: 401, kaynak ilişkisi değil
        return False
    if j == "missing":          # kaynak gerçekten yok / silinmiş: var olmayanla aynı 404
        return True
    if j in ("missing_distinct", "distinct"):  # ayırt edilebilir yanıt (yönlendirme, farklı 404, iş kuralı reddi)
        return False
    if j:
        return by_id[j]["gizleme_404"] in CONCEALED
    refusing = {r["gizleme_404"] in CONCEALED for r in rows if r["gizleme_404"] != "yok"}
    if len(refusing) > 1:
        raise ValueError("karışık gizleme; jrow gerekli")
    return refusing.pop() if refusing else False


def main():
    rows = list(csv.DictReader(open(INV, encoding="utf-8")))
    by_id = {r["id"]: r for r in rows}
    by = defaultdict(list)
    for r in rows:
        by[(r["nesne_tipi"], r["eylem"])].append(r)
    keys = {(p["obj"], p["act"]) for p in P}
    if set(by) != keys:
        print("eşleşmeyen çiftler:", set(by) - keys, keys - set(by), file=sys.stderr)
        sys.exit(1)

    subjects = {"anonymous"} | {f"user:{u}" for u in USERS}
    errs = []
    used = set()
    for i, p in enumerate(P, 1):
        p["ncases"] = []
        for c in p["cases"]:
            d = norm_case(c, p["engine"])
            if d["subj"] not in subjects:
                errs.append(f"C{i:02d}: fixtures.subjects içinde yok: {d['subj']}")
            if d["obj"] not in OBJECTS:
                errs.append(f"C{i:02d}: fixtures.objects içinde yok: {d['obj']}")
            used.add(d["obj"])
            if d["expect"] == A and d["bc"] not in BASIS:
                errs.append(f"C{i:02d}: sözlük dışı basis {d['bc']}")
            if d["expect"] == D and not isinstance(d["bc"], bool):
                errs.append(f"C{i:02d}: ret satırında conceal yok")
            if d["java"].startswith("deny"):
                try:
                    d["java_conceal"] = java_conceal(d, by[(p["obj"], p["act"])], by_id)
                except ValueError as e:
                    errs.append(f"C{i:02d} {d['subj']} {d['obj']}: {e}")
            p["ncases"].append(d)
    for t in TUPLES:                 # tuple: <nesne>#<relation>@<özne>; özne bir nesne, user:* ya da <nesne>#<relation>
        o, rest = t.split("#", 1)
        subj = rest.split("@", 1)[1]
        used.add(o)
        if o not in OBJECTS:
            errs.append(f"tuple nesnesi fixtures.objects içinde yok: {t}")
        if subj != "user:*" and subj.split("#", 1)[0] not in OBJECTS:
            errs.append(f"tuple öznesi fixtures.objects içinde yok: {t}")
    unused = {o for o in OBJECTS if not o.startswith("user:")} - used
    if unused:
        errs.append(f"kullanılmayan fikstür: {sorted(unused)}")
    if errs:
        print("\n".join(errs), file=sys.stderr)
        sys.exit(1)

    # --- markdown tablo
    esc = lambda x: x.replace("|", "\\|")
    lines = [BEGIN, "",
             "| # | Nesne | Eylem | Satırlar | Java kuralı | Hedef (karar) | §5.4 | Uyum | Önerilen relation | Motor | Java hedeften farklı |",
             "|---|---|---|---|---|---|---|---|---|---|---|"]
    for i, p in enumerate(P, 1):
        ids = ", ".join(r["id"] for r in by[(p["obj"], p["act"])])
        eng = "go + openfga" if p["engine"] == FGA else p["engine"]
        tgt = p["target"] + (f" ({p['dec']})" if p["dec"] else "")
        lines.append(f"| C{i:02d} | `{p['obj']}` | `{p['act']}` | {ids} | {esc(p['java'])} | {esc(tgt)} | {esc(p['rel54'])} | "
                     f"{p['fit']} | `{p['rel']}` | {eng} | {esc(p['jdiff']) or '—'} |")
    lines += ["", END]
    md = open(MD, encoding="utf-8").read()
    a, b = md.index(BEGIN), md.index(END) + len(END)
    open(MD, "w", encoding="utf-8").write(md[:a] + "\n".join(lines) + md[b:])

    # --- matris
    out = [HEADER, "basis:"]
    out += [f"  {k}: {q(v)}" for k, v in BASIS.items()]
    out += ["", "actions:"]
    for p in P:
        out.append(f"  {p['obj']}.{p['act']}: {q(p['rel'])}")
    out += ["", "fixtures:", "  subjects:", "    anonymous: {type: anonymous}"]
    for u, ua in USERS.items():
        out.append(f"    user:{u}: {{type: user, note: {q(ua['note'])}}}")
    out.append("  objects:")
    for o, attrs in OBJECTS.items():
        out.append(f"    {o}: {{type: {o.split(':')[0]}, attrs: {q(attrs)}}}")
    out.append("  tuples:")
    out += [f"    - {q(t)}" for t in TUPLES]
    out += ["", "cases:"]
    n = 0
    diff = 0
    for i, p in enumerate(P, 1):
        rs = by[(p["obj"], p["act"])]
        src = sorted({t.strip().split(" (")[0] for r in rs for t in r["java_testi"].split(";") if t.strip()})
        for d in p["ncases"]:
            n += 1
            jd = "deny" if d["java"].startswith("deny") else d["java"]
            if jd != d["expect"]:
                diff += 1
            out.append(f"  - id: AZM-{n:03d}")
            out.append(f"    pair: C{i:02d}")
            out.append(f"    action: {p['obj']}.{p['act']}")
            out.append(f"    subject: {q(d['subj'])}")
            out.append(f"    object: {q(d['obj'])}")
            out.append(f"    expect: {d['expect']}")
            if d["expect"] == A:
                out.append(f"    basis: {d['bc']}")
            else:
                out.append(f"    conceal: {'true' if d['bc'] else 'false'}")
            out.append(f"    java: {jd}")
            if jd == D:
                out.append(f"    java_conceal: {'true' if d['java_conceal'] else 'false'}")
            eng = d["engine"]
            out.append(f"    engine: {'[go, openfga]' if isinstance(eng, list) else eng}")
            if d["note"]:
                out.append(f"    note: {q(d['note'])}")
            out.append(f"    inventory: [{', '.join(r['id'] for r in rs)}]")
            out.append(f"    source: [{', '.join(q(x) for x in src)}]")
    open(YAML_OUT, "w", encoding="utf-8").write("\n".join(out) + "\n")
    print(f"{len(P)} çift, {n} matris satırı, {diff} satırda Java hedeften farklı", file=sys.stderr)


HEADER = '''# TASLAK — internal/authz/testdata/matrix.yaml (plan §5.6-3, ADR-0014)
#
# Faz 0 başlangıç taslağı. Koşucular (§5.6-5, ADR-0024): Go değerlendiricisi
# internal/authz/matrix_test.go, gerçek OpenFGA internal/authz/openfga_conformance_test.go.
# Üretici: docs/faz0/tarama/pairs.py; elle düzenlemeyin, betiği güncelleyin.
#
# Alanlar
#   action   : <nesne_tipi>.<karar eylemi>; relation karşılığı `actions:` haritasında.
#              `go:<kural>` = model dışı Go kuralı (sahiplik, öznitelik, protokol).
#   subject  : "anonymous" ya da "user:<id>"; fixtures.subjects içinde çözülür.
#   object   : "<tip>:<id>"; fixtures.objects içinde çözülür. Kullanıcılar (user:<id>)
#              hem subjects hem objects altında listelenir.
#   tuples   : "<nesne>#<relation>@<özne>"; özne fixtures.objects'teki bir nesne,
#              "<nesne>#<relation>" kullanıcı kümesi ya da "user:*" jokeri (her kimlikli
#              kullanıcı; fikstür değildir, anonymous'u kapsamaz).
#   expect   : Go HEDEF kararı (allow | deny); karar kaydı D1-D5 uygulanmış.
#   basis    : allow satırında kararın dayanağı; değer `basis:` sözlüğünden.
#   conceal  : deny satırında ZORUNLU; true = var olmayan kaynakla bayt bayt aynı 404.
#   java     : Java'nın bugünkü kararı (origin/main). java_conceal Java'nın ret biçimi;
#              docs/faz0/authz-envanteri.csv `gizleme_404` kolonundan türetilir.
#   engine   : [go, openfga] iki değerlendiricide de koşar · go_only yalnız Go
#              (ön süzgeç, öznitelik, sahiplik, Scope) · skip koşmaz (gerekçe çiftte).
#   source   : bağlayan Java testi (src/test/java/...#metot); boşsa bulunamadı.
#
# Kapsam: pazaryeri tarafı. Personel RBAC (uç izni + 18 dinamik kontrol) kapsam dışı;
# kaynağı docs/faz0/authz-personel-izinleri.csv (docs/faz0/README.md).
# OpenFGA'ya yüklenecek tuple'lar `fixtures.tuples` listesindedir; soft-delete'li
# nesnelerin (deleted: true) tuple'ı yoktur, Check false döner (§5.7).

version: 0
status: draft
engines:          # izin verilen engine değerleri
  - [go, openfga]
  - go_only
  - skip
'''

if __name__ == "__main__":
    main()
