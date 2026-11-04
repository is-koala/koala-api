#!/usr/bin/env python3
"""authz-ciftleri.md tablosunu ve test/authz/matrix.yaml taslağını aynı kaynaktan üretir.

Kullanım: python3 -I pairs.py <authz-envanteri.csv> <ciftler-tablo.md çıktısı> <matrix.yaml çıktısı>
"""
import csv
import sys
from collections import defaultdict

INV, MD_OUT, YAML_OUT = sys.argv[1:4]

# uyum: "✓" model aynen ifade ediyor · "≈" model ifade ediyor ama Java'dan farklı karar verir
#       "✗" modelde karşılık yok · "dış" bilinçli olarak model dışı (§5.4 "Modelin dışında kalanlar")
# pos / neg: (özne, nesne, temel/not)
P = []


def pair(obj, act, rule, rel, fit, note, pos, neg, conceal=None):
    P.append(dict(obj=obj, act=act, rule=rule, rel=rel, fit=fit, note=note, pos=pos, neg=neg, conceal=conceal))


# ---------------- job_assignment ----------------
pair("job_assignment", "read", "işçi VEYA ilan sahibi VEYA şirket yetkilisi (partyOf)", "job_assignment#can_read", "✓", "",
     [("u_worker", "asg_ind", "worker"), ("u_co_staff", "asg_co", "editor from job")],
     [("u_stranger", "asg_ind"), ("u_other_co_admin", "asg_co")], conceal=True)
pair("job_assignment", "respond", "yalnız işçi", "—", "✗", "Modelde yok. Öneri: `define can_respond: worker`.",
     [("u_worker", "asg_pending", "worker")], [("u_creator", "asg_pending"), ("u_stranger", "asg_pending")], conceal=True)
pair("job_assignment", "complete", "işveren tarafı (partyOf == EMPLOYER: creator ∪ şirket yetkilisi)", "job_assignment#can_complete", "✓", "",
     [("u_creator", "asg_ind", "editor from job"), ("u_co_admin", "asg_co", "editor from job")],
     [("u_worker", "asg_ind"), ("u_stranger", "asg_ind")], conceal=True)
pair("job_assignment", "pay", "BİREYSEL: yalnız creator; ŞİRKET: yalnız güncel yetkili (creator dahil değil)", "job_assignment#can_pay", "≈",
     "Model `editor from job` diyor; editor creator'ı her zaman içerir. Java şirket ilanında şirketten ayrılmış creator'ı reddeder. Ayrıca durum (409) yetkiden önce bakılıyor.",
     [("u_creator", "asg_ind", "creator (INDIVIDUAL)"), ("u_co_staff", "asg_co", "member from owner_company")],
     [("u_worker", "asg_ind"), ("u_co_former", "asg_orphan"), ("u_stranger", "asg_ind")], conceal=False)
pair("job_assignment", "cancel", "taraf (partyOf); taraf ceza politikasını seçer", "job_assignment#can_cancel", "✓", "",
     [("u_worker", "asg_ind", "worker"), ("u_creator", "asg_ind", "editor from job")], [("u_stranger", "asg_ind")], conceal=True)
pair("job_assignment", "record_work", "yalnız işçi (check-in ve check-out)", "job_assignment#can_check_in", "✓",
     "check-out da aynı kural; öneri: relation adını `can_record_work` yap. Emanet (HELD_IN_ESCROW) ön süzgeç.",
     [("u_worker", "asg_ind", "worker")], [("u_creator", "asg_ind"), ("u_stranger", "asg_ind")], conceal=True)
pair("job_assignment", "open_dispute", "taraf; emanet şartı ön süzgeç", "—", "✗",
     "Modelde yok (dispute yalnız var olan uyuşmazlık için). Öneri: `define can_open_dispute: party`. Ret mesajı NOT_A_PARTICIPANT (var olmayandan farklı).",
     [("u_worker", "asg_ind", "worker"), ("u_co_admin", "asg_co", "editor from job")], [("u_stranger", "asg_ind")], conceal=False)
pair("job_assignment", "review", "taraf; COMPLETED ön süzgeç", "—", "✗", "Öneri: `define can_review: party`.",
     [("u_worker", "asg_done", "worker"), ("u_creator", "asg_done", "editor from job")], [("u_stranger", "asg_done")], conceal=True)
pair("job_assignment", "reveal_worker_identity", "işveren tarafı + atama HIRED (ön süzgeç) + saatlik oran sınırı", "—", "✗",
     "Öneri: `define can_reveal_worker_identity: editor from job`; HIRED koşulu prefilter, oran sınırı ayrı. İşçinin kendisi bu uçtan okuyamaz.",
     [("u_creator", "asg_ind", "editor from job")], [("u_worker", "asg_ind"), ("u_other_co_admin", "asg_co"), ("u_creator", "asg_pending")], conceal=True)
pair("job_assignment", "select_seat", "taraf (işveren işçi adına seçebilir); servis/durak aynı planda olmalı", "—", "✗",
     "Öneri: `define can_select_seat: party`. Kapsama (servis ilan planında) ayrı kontrol.",
     [("u_worker", "asg_ind", "worker"), ("u_creator", "asg_ind", "editor from job")], [("u_stranger", "asg_ind")], conceal=True)
pair("job_assignment", "issue_boarding_qr", "taraf VE işçi (iki kat)", "—", "✗", "Öneri: `define can_board: worker`.",
     [("u_worker", "asg_ind", "worker")], [("u_creator", "asg_ind"), ("u_stranger", "asg_ind")], conceal=True)
pair("job_assignment", "list", "kapsam: işçi → worker_id; işveren → yalnız creator", "Scope(job_assignment#can_read)", "≈",
     "Tekil okuma (can_read) şirket yetkilisine açık ama 'işveren olarak atamalarım' listesi yalnız creator'ı süzüyor. Scope ve Check aynı relation'dan türetilirse liste genişler.",
     [("u_creator", "asg_ind", "creator"), ("u_worker", "asg_ind", "worker")], [("u_co_admin", "asg_co")], conceal=None)
# ---------------- job ----------------
pair("job", "read", "herkes, her durumda (permitAll)", "job#can_view", "≈",
     "Model `public_viewer` tuple'ını yalnız OPEN iken yazar; Java CLOSED/CANCELLED ilanı da kimliksiz gösterir. Ürün kararı gerekir.",
     [("anonymous", "job_ind", "public"), ("anonymous", "job_closed", "public (Java)")],
     [("anonymous", "job_closed", "JAVA_ALLOWS")], conceal=None)
pair("job", "read_employer_view", "detayda editor, aramada yalnız creator", "job#editor", "≈", "Java'nın iki yolu tutarsız; model tek kural (editor) verir.",
     [("u_co_admin", "job_co", "editor (detay)"), ("u_creator", "job_ind", "creator")], [("u_stranger", "job_ind")], conceal=None)
pair("job", "read_exact_address", "creator VEYA atanmış işçi (ADDRESS_VISIBLE durumları) VEYA şirket adresi açık", "—", "✗",
     "Modelde yok. Şirket yetkilisi (creator değilse) tam adresi GÖRMEZ: editor'dan dar. Öneri: alan düzeyi kural, `can_read_exact_address: creator or worker from <aktif atama>` + öznitelik (addressPublic). PII/konum maskesi (#349) ile birlikte.",
     [("u_creator", "job_ind", "creator"), ("u_worker", "job_ind", "assigned worker"), ("anonymous", "job_co_public_addr", "public address")],
     [("u_stranger", "job_ind"), ("u_co_admin", "job_co")], conceal=None)
pair("job", "edit", "editor (JobEditors): galeri, ulaşım planı yazımı", "job#can_edit", "✓", "",
     [("u_creator", "job_ind", "creator"), ("u_co_admin", "job_co", "member from owner_company")], [("u_worker", "job_ind"), ("u_stranger", "job_ind")], conceal=True)
pair("job", "read_private", "editor: ulaşım planı okuma, ilanın atama listesi", "job#editor", "✓", "Öneri: açık relation `can_read_private: editor`.",
     [("u_co_staff", "job_co", "member from owner_company")], [("u_worker", "job_co"), ("u_stranger", "job_co")], conceal=True)
pair("job", "list_applications", "BİREYSEL: creator (404); ŞİRKET: yalnız güncel yetkili (400)", "job_application#can_read (editor from job)", "≈",
     "Şirket dalında creator dahil değil ve hata 400 (ValidationException). Bireysel dal 404 ama farklı mesaj.",
     [("u_creator", "job_ind", "creator (INDIVIDUAL)"), ("u_co_admin", "job_co", "member")], [("u_co_former", "job_co_orphan"), ("u_stranger", "job_ind")], conceal=False)
pair("job", "cancel", "editor", "job#can_edit", "✓", "Öneri: denetim için ayrı `can_cancel: editor`.",
     [("u_creator", "job_ind", "creator")], [("u_worker", "job_ind")], conceal=True)
pair("job", "close", "editor", "job#can_edit", "✓", "",
     [("u_co_staff", "job_co", "member")], [("u_stranger", "job_co")], conceal=True)
pair("job", "apply", "editor OLMAYAN herkes (creator ve şirket yetkilisi başvuramaz) + OPEN + hazır olma", "—", "✗",
     "Negatif ilişki. Öneri: `define can_apply: [user:*] but not editor` (+ OPEN prefilter). Ret 400.",
     [("u_applicant", "job_ind", "[user:*] but not editor")], [("u_creator", "job_ind"), ("u_co_staff", "job_co")], conceal=False)
pair("job", "open_conversation", "işveren tarafı (creator ∪ yetkili) herhangi bir işçiyle; işçi yalnız kendisi için; ödeme ön süzgeci ve engelleme", "—", "✗",
     "conversation tipi var ama açma kuralı yok. Öneri: `can_open_conversation: editor` (+ işçi için atama/ödeme prefilter'ı). Yabancı 404 NOT_A_PARTICIPANT.",
     [("u_creator", "job_ind", "editor (asg_ind emanette)")], [("u_stranger", "job_ind"), ("u_blocked", "job_ind")], conceal=False)
pair("job", "copy_as_template", "editor (JobEditors'un ayrı kopyası)", "job#can_edit", "✓", "Kural kopyası birleştirilmeli.",
     [("u_co_staff", "job_co", "member")], [("u_stranger", "job_co")], conceal=True)
# ---------------- company ----------------
pair("company", "read", "kimlikli her kullanıcı; galeri herkese", "—", "✗", "Öneri: `define viewer: [user:*]`.",
     [("u_stranger", "co_1", "public")], [("anonymous", "co_1 (kimlik doğrulaması yok: 401)")], conceal=None)
pair("company", "read_member_view", "üye (ADMIN|STAFF): onay notu, kişi listesi, şirket ilanları", "company#member", "✓",
     "Kişi listesi 404, ilan listesi 403, onay notu alan düzeyi: aynı kural, üç farklı ret biçimi.",
     [("u_co_staff", "co_1", "staff_member"), ("u_co_admin", "co_1", "admin")], [("u_stranger", "co_1"), ("u_other_co_admin", "co_1")], conceal=False)
pair("company", "manage", "yalnız ADMIN: güncelleme, logo, galeri, davet, kişi çıkarma, davet listesi", "company#can_manage", "✓",
     "Ret 404 USER_NOT_ASSOCIATED_WITH_COMPANY: var olmayan şirketten farklı mesaj (ayırt edilebilir).",
     [("u_co_admin", "co_1", "admin")], [("u_co_staff", "co_1"), ("u_stranger", "co_1")], conceal=False)
pair("company", "create_job", "üye", "company#can_manage_jobs", "✓", "Ret 400 (ValidationException).",
     [("u_co_staff", "co_1", "staff_member")], [("u_stranger", "co_1")], conceal=False)
pair("company", "tag_talent_pool_entry", "KONTROL YOK", "company#member (önerilen)", "✗", "Java açığı; Go'da `member` şartı önerilir.",
     [("u_co_admin", "co_1", "member (önerilen)")], [("u_stranger", "co_1", "JAVA_ALLOWS")], conceal=None)
# ---------------- company_invitation ----------------
pair("company_invitation", "read", "davet eden VEYA davet edilen VEYA şirket ADMIN'i", "—", "✗",
     "Tip yok. Öneri: `type company_invitation { company: [company]; inviter: [user]; invitee: [user]; can_read: inviter or invitee or admin from company; can_answer: invitee }`.",
     [("u_invitee", "inv_1", "invitee"), ("u_co_admin", "inv_1", "admin from company")], [("u_co_staff", "inv_1"), ("u_stranger", "inv_1")], conceal=True)
pair("company_invitation", "answer", "yalnız davet edilen", "—", "✗", "Bkz. üst satır.",
     [("u_invitee", "inv_1", "invitee")], [("u_co_admin", "inv_1"), ("u_stranger", "inv_1")], conceal=True)
# ---------------- job_application ----------------
pair("job_application", "decide", "BİREYSEL: creator; ŞİRKET: yalnız güncel yetkili", "job_application#can_decide", "≈",
     "Model `editor from job` (creator dahil). Java şirket ilanında creator'ı dışarıda bırakır (job.pay ve list_applications ile aynı 'işveren' kavramı).",
     [("u_creator", "app_ind", "creator"), ("u_co_admin", "app_co", "member")], [("u_applicant", "app_ind"), ("u_co_former", "app_orphan"), ("u_stranger", "app_co")], conceal=True)
pair("job_application", "withdraw", "yalnız başvuran", "job_application#can_withdraw", "✓", "",
     [("u_applicant", "app_ind", "applicant")], [("u_creator", "app_ind"), ("u_stranger", "app_ind")], conceal=True)
pair("job_application", "list", "kapsam: creator → yalnız BİREYSEL ilanları; yetkili → şirket ilanları", "Scope(job_application#can_read)", "≈",
     "Java'nın 'işveren' kavramı (creator yalnız bireysel ilanda) burada da geçerli; model editor'ı kullanırsa liste genişler.",
     [("u_creator", "app_ind", "creator (INDIVIDUAL)"), ("u_co_admin", "app_co", "member")], [("u_co_former", "app_orphan"), ("u_stranger", "app_ind")], conceal=None)
# ---------------- conversation / message ----------------
pair("conversation", "read", "katılımcı (okuma, mesajlar, okundu işareti)", "conversation#can_read", "✓", "",
     [("u_worker", "conv_ind", "participant")], [("u_stranger", "conv_ind")], conceal=True)
pair("conversation", "post", "katılımcı + engelleme + ödeme/pencere ön süzgeçleri (+ STOMP typing)", "conversation#can_post", "✓",
     "Ön süzgeçler (MessagingAvailability, user_blocks) ReBAC dışı; §5.2-3'e uygun.",
     [("u_creator", "conv_ind", "participant")], [("u_stranger", "conv_ind"), ("u_blocked", "conv_blocked")], conceal=True)
pair("conversation", "subscribe", "katılımcı (STOMP SUBSCRIBE)", "conversation#can_read", "✓", "K4 ile WS protokolü değişir; kural aynı kalır.",
     [("u_worker", "conv_ind", "participant")], [("u_stranger", "conv_ind")], conceal=False)
pair("conversation", "publish_to_topic", "KONTROL YOK (simple broker /topic SEND) — doğrulanmadı", "—", "✗",
     "K4 sunucu-itmeli modelde istemci yayın yapamaz; Go'da relation gerekmez, protokol reddeder.",
     [("u_worker", "conv_ind", "Java: SEND yalnız oran sınırından geçer (doğrulanmadı)")], [("u_stranger", "conv_ind", "JAVA_ALLOWS_UNVERIFIED")], conceal=None)
pair("conversation", "list", "kapsam: katılımcı", "Scope(conversation#can_read)", "✓", "",
     [("u_worker", "conv_ind", "participant")], [("u_stranger", "conv_ind")], conceal=None)
pair("message", "report", "mesajın sohbetinde katılımcı (DAO WHERE); gönderen kendi mesajını raporlayamaz", "—", "✗",
     "Öneri: `type message { conversation: [conversation]; sender: [user]; can_report: participant from conversation but not sender }`.",
     [("u_worker", "msg_ind", "participant from conversation")], [("u_stranger", "msg_ind"), ("u_creator", "msg_ind")], conceal=True)
# ---------------- dispute ----------------
pair("dispute", "read", "atamanın tarafı (kanıt listesi ve indirme); kanıt uyuşmazlığa ait olmalı", "dispute#can_read", "✓", "",
     [("u_worker", "dsp_ind", "party from assignment"), ("u_creator", "dsp_ind", "party from assignment")], [("u_stranger", "dsp_ind")], conceal=True)
pair("dispute", "add_evidence", "atamanın tarafı; RESOLVED ise 409", "dispute#can_add_evidence", "✓", "",
     [("u_worker", "dsp_ind", "party from assignment")], [("u_stranger", "dsp_ind")], conceal=True)
# ---------------- transport ----------------
pair("shuttle", "manage", "ilanın editor'ı (QR üret/döndür, biniş listesi)", "—", "✗",
     "Öneri: `type shuttle { job: [job]; can_manage: editor from job; can_board: seat_holder }` (seat_holder tuple'ı koltuk seçiminde yazılır).",
     [("u_creator", "shuttle_ind", "editor from job")], [("u_worker", "shuttle_ind"), ("u_stranger", "shuttle_ind")], conceal=True)
pair("shuttle", "board", "bu serviste koltuğu olan işçi; koltuksuz taraf 409, yabancı 404", "—", "✗", "Bkz. üst satır.",
     [("u_worker", "shuttle_ind", "seat_holder")], [("u_stranger", "shuttle_ind")], conceal=True)
pair("seat_assignment", "mark_boarding", "koltuğun ilanının editor'ı", "—", "✗", "Öneri: `type seat_assignment { shuttle: [shuttle]; can_mark: can_manage from shuttle }`.",
     [("u_creator", "seat_w", "editor from job")], [("u_stranger", "seat_w"), ("u_worker", "seat_w")], conceal=True)
# ---------------- user / dosya ----------------
pair("user", "read_public", "tasarım gereği açık: profil, deneyim, müsaitlik, rozet, yorum, puan, işveren istatistiği", "—", "✗",
     "Öneri: `type user { define public_viewer: [user:*] }` (N01 §9.d). Müsaitlik takviminin açık olması ürün kararı.",
     [("anonymous", "u_worker", "public (deneyim, müsaitlik, rozet, yorum, puan, istatistik)")],
     [("anonymous", "u_worker (GET /users/{userId} profil kimlik ister: 401)")], conceal=None)
pair("user", "read_cv_url", "kendisi VEYA ona başvuru almış işveren (projeksiyon)", "—", "✗", "private_file.download ile aynı kural, ROLE_ADMIN baypası hariç.",
     [("u_applicant", "u_applicant", "self"), ("u_creator", "u_applicant", "received application")], [("u_stranger", "u_applicant")], conceal=None)
pair("private_file", "download", "sahip VEYA marketplace ROLE_ADMIN VEYA sahibinden başvuru almış işveren (kalıcı)", "—", "✗",
     "Öneri: `type cv { owner: [user]; reviewer: [user, company#member]; can_download: owner or reviewer }`; reviewer tuple'ı başvuru anında yazılır. ROLE_ADMIN baypası Go'da kalkar (personel `user.read_pii`).",
     [("u_applicant", "cv_applicant", "owner"), ("u_creator", "cv_applicant", "received application")], [("u_stranger", "cv_applicant")], conceal=True)
pair("user", "read_presence", "REST'te KONTROL YOK", "—", "✗", "§5.4'teki bilinen `contact` kalemi; REST ve WS aynı kurala bağlanmalı.",
     [("u_stranger", "u_worker", "Java: kontrol yok")], [("anonymous", "u_worker (401)"), ("u_stranger", "u_worker", "JAVA_ALLOWS")], conceal=None)
pair("user", "watch_presence", "kendisi VEYA ortak sohbet (STOMP)", "—", "✗", "§5.4: türetilmiş `contact` ilişkisi gerekir.",
     [("u_worker", "u_worker", "self"), ("u_creator", "u_worker", "contact")], [("u_stranger", "u_worker")], conceal=False)
pair("user", "create_individual_job", "öznitelik: isEmployer + kimlik doğrulanmış", "—", "dış", "ReBAC dışı ön süzgeç (öznitelik).",
     [("u_creator", "self", "isEmployer")], [("u_worker", "self")], conceal=False)
pair("image", "delete", "yükleyen VEYA marketplace ROLE_ADMIN", "owner (Scope)", "≈", "ROLE_ADMIN baypası Go'da kalkar; ret 400 ve var olma sızar.",
     [("u_stranger", "img_x", "owner")], [("u_worker", "img_x")], conceal=False)
# ---------------- kendi verin (owner) ----------------
for obj, fix in [("experience", "exp_w"), ("education", "edu_w"), ("job_alert", "alert_w"), ("bank_account", "bank_w"),
                 ("device", "dev_w"), ("availability_exception", "avex_w")]:
    pair(obj, "write", "yalnız sahibi", "owner (model dışı, Scope SelfUserID)", "dış", "",
         [("u_worker", fix, "owner")], [("u_stranger", fix)], conceal=True)
pair("job_template", "read", "yalnız sahibi", "owner", "dış", "", [("u_creator", "tmpl_c", "owner")], [("u_stranger", "tmpl_c")], conceal=True)
pair("job_template", "write", "yalnız sahibi", "owner", "dış", "", [("u_creator", "tmpl_c", "owner")], [("u_stranger", "tmpl_c")], conceal=True)
pair("session", "read", "yalnız sahibi", "owner", "dış", "", [("u_worker", "ses_w", "owner")], [("u_stranger", "ses_w")], conceal=True)
pair("session", "write", "yalnız sahibi (iptal)", "owner", "dış", "", [("u_worker", "ses_w", "owner")], [("u_stranger", "ses_w")], conceal=True)
pair("user_verification", "confirm", "yalnız sahibi; ret 403 (var olma sızar)", "owner", "dış", "", [("u_worker", "ver_w", "owner")], [("u_stranger", "ver_w")], conceal=False)
pair("notification", "mark_read", "yalnız alıcı; başkasınınki sessizce yok sayılır (200)", "owner", "dış", "",
     [("u_worker", "notif_w", "recipient")], [("u_stranger", "notif_w (200, değişiklik yok)")], conceal=None)
pair("notification", "mark_opened", "kimliksiz; bildirim UUID'si yetenek URL'si", "—", "dış", "", [("anonymous", "notif_w", "capability url")], [("anonymous", "notif_missing (404; yetki değil var olma)")], conceal=None)
pair("address_search_session", "use", "bellek içi oturumun sahibi; ret 400", "owner", "dış", "", [("u_worker", "addr_w", "owner")], [("u_stranger", "addr_w")], conceal=False)
pair("payment_attempt", "complete_callback", "kimliksiz; sağlayıcı token'ı + sunucudan sorgu (reconcile)", "—", "dış", "Yetki değil sağlayıcı doğrulaması: test/oracle/reconcile.", [("anonymous", "pay_token", "provider token")], [("anonymous", "pay_token_forged (sunucu sorgusu reddeder; doğrulanmadı)")], conceal=None)
pair("payment_attempt", "read_outcome", "kimliksiz; kayıtlı sonucu okur", "—", "dış", "", [("anonymous", "pay_token", "provider token")], [("anonymous", "pay_token_unknown (kayıt yok; doğrulanmadı)")], conceal=None)
pair("realtime_destination", "subscribe", "bilinen hedef dışı her SUBSCRIBE reddedilir", "—", "dış", "Varsayılan ret; K4 ile protokol değişir.", [("u_worker", "queue_own (/user/** broker kapsamı)", "user destination")], [("u_worker", "topic_unknown")], conceal=False)


def main():
    rows = list(csv.DictReader(open(INV, encoding="utf-8")))
    by = defaultdict(list)
    for r in rows:
        by[(r["nesne_tipi"], r["eylem"])].append(r)
    keys = {(p["obj"], p["act"]) for p in P}
    missing = set(by) - keys
    extra = keys - set(by)
    if missing or extra:
        print("eşleşmeyen çiftler:", missing, extra, file=sys.stderr)
        sys.exit(1)

    with open(MD_OUT, "w", encoding="utf-8") as f:
        f.write("| # | Nesne | Eylem | Satırlar | Java kuralı | §5.4 relation | Uyum | Not |\n|---|---|---|---|---|---|---|---|\n")
        for i, p in enumerate(P, 1):
            ids = ", ".join(r["id"] for r in by[(p["obj"], p["act"])])
            esc = lambda x: x.replace("|", "\\|")
            f.write(f'| C{i:02d} | `{p["obj"]}` | `{p["act"]}` | {ids} | {esc(p["rule"])} | {esc(p["rel"])} | {p["fit"]} | {esc(p["note"])} |\n')

    with open(YAML_OUT, "w", encoding="utf-8") as f:
        f.write(HEADER)
        n = 0
        for i, p in enumerate(P, 1):
            rs = by[(p["obj"], p["act"])]
            src = sorted({t.strip().split(" (")[0] for r in rs for t in r["java_testi"].split(";") if t.strip()})
            inv = [r["id"] for r in rs]
            for kind, lst in (("allow", p["pos"]), ("deny", p["neg"])):
                for case in lst:
                    n += 1
                    subj, objf = case[0], case[1]
                    f.write(f'  - id: AZM-{n:03d}\n')
                    f.write(f'    pair: C{i:02d}\n')
                    f.write(f'    subject: {subj}\n')
                    f.write(f'    action: {p["obj"]}.{p["act"]}\n')
                    f.write(f'    object: {objf.split(" (")[0]}\n')
                    f.write(f'    expect: {kind}\n')
                    marker = case[2] if len(case) > 2 else ""
                    if kind == "deny" and marker.startswith("JAVA_ALLOWS"):
                        f.write('    java_actual: allow   # hedef davranış (model ya da öneri); Java bugün izin veriyor\n')
                        if marker.endswith("UNVERIFIED"):
                            f.write('    verified: false\n')
                    elif kind == "allow":
                        f.write(f'    basis: "{case[2]}"\n')
                    else:
                        if p["conceal"] is True:
                            f.write('    conceal: true      # ret = kaynağın kendi 404 gövdesi\n')
                        elif p["conceal"] is False:
                            f.write('    conceal: false     # Java bugün ayırt edilebilir ret döndürüyor (403/400/farklı 404)\n')
                    if "(" in objf and objf.endswith(")"):
                        f.write(f'    note: "{objf[objf.index("(") + 1:-1]}"\n')
                    if p["fit"] in ("≈", "✗"):
                        f.write(f'    model_status: "{ {"≈": "model farklı karar verir", "✗": "modelde yok"}[p["fit"]] }"  # authz-ciftleri.md\n')
                    f.write(f'    inventory: [{", ".join(inv)}]\n')
                    f.write(f'    source: [{", ".join(chr(34) + s + chr(34) for s in src) if src else ""}]\n')
                    if not src:
                        f.write('    # source boş: bu çifti bağlayan Java testi bulunamadı\n')
        print(f"{len(P)} çift, {n} matris satırı", file=sys.stderr)


HEADER = '''# TASLAK — test/authz/matrix.yaml
#
# Bu dosya Faz 0 başlangıç taslağıdır; Go değerlendiricisi ve OpenFGA uygunluk
# koşucusu (§5.6-5) henüz yok. `expect` Java'nın BUGÜNKÜ davranışıdır (origin/main);
# tek istisna `java_actual: allow` taşıyan satırlar: orada `expect: deny` hedef
# davranıştır (kontrol eksik ya da model farkı), Java bugün izin veriyor.
# `model_status` taşıyan satırlarda §5.4
# modeli Java'dan farklı karar verir ya da karşılığı yoktur; bu satırlar ürün
# kararı verilene kadar "karar bekliyor" sayılır (docs/faz0/authz-ciftleri.md).
#
# Üretim: docs/faz0/tarama/pairs.py (elle düzenlemeden önce betiği güncelleyin).
#
# Biçim (§5.6-3): özne fikstürü, eylem, nesne fikstürü -> beklenen sonuç ve temel.
#   action  : <nesne_tipi>.<karar eylemi> (authz-envanteri.csv `eylem` kolonu)
#   expect  : allow | deny
#   basis   : allow için kararın dayanağı (Decision.Basis karşılığı)
#   conceal : deny için ret biçimi (true = var olmayan kaynakla bayt bayt aynı 404)
#   source  : bağlayan Java testi (dosya:satır#test); boşsa bulunamadı

version: 0
status: draft

fixtures:
  users:
    anonymous:        "kimliksiz istek"
    u_worker:         "işçi; asg_ind, asg_co, asg_orphan atamalarının işçisi; conv_ind katılımcısı; seat_w koltuğu"
    u_creator:        "bireysel işveren; job_ind ve job_closed'ın creator'ı; tmpl_c sahibi"
    u_co_admin:       "co_1 ADMIN"
    u_co_staff:       "co_1 STAFF; job_co'nun creator'ı"
    u_co_former:      "co_1'den çıkarılmış eski yetkili; job_co_orphan'ın creator'ı"
    u_other_co_admin: "co_2 ADMIN; co_1 ile ilişkisi yok"
    u_applicant:      "app_ind ve app_co başvurularının sahibi; cv_applicant sahibi"
    u_invitee:        "inv_1 ile co_1'e davet edilmiş kullanıcı"
    u_blocked:        "u_worker tarafından engellenmiş kullanıcı (conv_blocked'da katılımcı)"
    u_stranger:       "hiçbir nesneyle ilişkisi yok; img_x'in yükleyeni"
  objects:
    co_1:              "onaylı şirket (ADMIN u_co_admin, STAFF u_co_staff)"
    co_2:              "başka şirket"
    job_ind:           "BİREYSEL, OPEN, creator u_creator; ulaşım planı var"
    job_closed:        "BİREYSEL, CLOSED, creator u_creator"
    job_co:            "ŞİRKET co_1, OPEN, creator u_co_staff, adres gizli"
    job_co_public_addr: "ŞİRKET co_1, adres herkese açık"
    job_co_orphan:     "ŞİRKET co_1, creator u_co_former (şirketten ayrılmış)"
    app_ind:           "u_applicant'ın job_ind başvurusu (APPLIED)"
    app_co:            "u_applicant'ın job_co başvurusu (APPLIED)"
    app_orphan:        "u_applicant'ın job_co_orphan başvurusu"
    asg_ind:           "job_ind, işçi u_worker, CONFIRMED, HELD_IN_ESCROW"
    asg_pending:       "job_ind, işçi u_worker, PENDING_CONFIRMATION"
    asg_done:          "job_ind, işçi u_worker, COMPLETED"
    asg_co:            "job_co, işçi u_worker, HELD_IN_ESCROW"
    asg_orphan:        "job_co_orphan, işçi u_worker, CONFIRMED, PENDING_COLLECTION"
    dsp_ind:           "asg_ind üzerinde açık uyuşmazlık"
    conv_ind:          "job_ind DIRECT sohbeti (u_creator, u_worker)"
    conv_blocked:      "u_worker ile u_blocked arasında DIRECT sohbet"
    msg_ind:           "conv_ind içinde u_creator'ın mesajı"
    inv_1:             "co_1 daveti: inviter u_co_admin, invitee u_invitee, PENDING"
    shuttle_ind:       "job_ind ulaşım planındaki servis"
    seat_w:            "u_worker'ın shuttle_ind koltuğu"
    cv_applicant:      "u_applicant'ın CV dosyası"
    img_x:             "u_stranger'ın yüklediği görsel"
    exp_w:             "u_worker'ın deneyim kaydı"
    edu_w:             "u_worker'ın eğitim kaydı"
    alert_w:           "u_worker'ın iş alarmı"
    bank_w:            "u_worker'ın aktif banka hesabı"
    dev_w:             "u_worker'ın cihazı"
    avex_w:            "u_worker'ın müsaitlik istisnası"
    ses_w:             "u_worker'ın aktif oturumu"
    ver_w:             "u_worker'ın doğrulama kaydı (OTP)"
    notif_w:           "u_worker'ın bildirimi"
    addr_w:            "u_worker'ın adres arama oturumu (bellek içi)"
    tmpl_c:            "u_creator'ın ilan şablonu"
    pay_token:         "sağlayıcı ödeme token'ı"
    topic_unknown:     "/topic/admin/secrets gibi bilinmeyen hedef"

cases:
'''

if __name__ == "__main__":
    main()
