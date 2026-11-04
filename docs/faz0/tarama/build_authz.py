#!/usr/bin/env python3
"""authz envanteri üreticisi.

1) endpoints.py ve tests.py çıktılarını okur,
2) aşağıdaki ELLE DOĞRULANMIŞ guard listesindeki her satırı, guard'ı içeren
   metottan public kök metotlara (resolve.py) ve oradan HTTP uçlarına çözer,
3) her (guard, kök metot) için bir CSV satırı yazar ve bağlayan Java testini
   (uç + red statüsü ya da manager + istisna eşleşmesi) önerir.

Kullanım:
  python3 -I build_authz.py <backend-src> <eps.json> <tests.json> <çıktı dizini>

Guard listesi (ROWS) betikle taranan aday satırların (grep desenleri README'de)
elle okunup sınıflandırılmış halidir. Satır numaraları origin/main anlık
görüntüsüne aittir.
"""
import csv
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import resolve  # noqa: E402

ROOT, EPS, TESTS, OUT = sys.argv[1:5]
JAVA_PREFIX = "src/main/java/com/exskylab/koala/"
TEST_PREFIX = "src/test/java/"

C = "business/concretes/"

# Gizleme değerleri:
#   evet          : Concealment.notFound (log + kaynağın kendi 404'ü, var olmayanla aynı mesaj)
#   evet-yardımcısız : var olmayanla aynı 404 mesajı, ama Concealment yardımcısı yok (log yok)
#   kısmi         : 404 ama var olmayan kaynaktan farklı mesaj (ayırt edilebilir)
#   hayır         : 403 / 400 / 409 (var olma sızar)
#   sessiz        : 200 döner, hiçbir şey yapmaz
#   yok           : kontrol yok
#
# Satır: (konum, desen, nesne, eylem, gizleme, hata, kökler, not)
#   kökler: guard'ı içeren dosyadaki public metot adları; "!" ile başlayan
#   öğe doğrudan uç ("!GET /x") ya da "STOMP ..." olarak yazılır.
ROWS = [
    # ---------------- job_assignment ----------------
    (C + "JobAssignmentManager.java:505", "AssignmentParticipants.partyOf", "job_assignment", "read", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["getMyAssignment"], ""),
    (C + "JobAssignmentManager.java:200", "doğrudan getId().equals (işçi)", "job_assignment", "respond", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["answerToAssignment"], ""),
    (C + "JobAssignmentManager.java:601", "AssignmentParticipants.partyOf == EMPLOYER", "job_assignment", "complete", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["completeAssignment"], ""),
    (C + "JobAssignmentManager.java:261", "doğrudan getId().equals (ilan sahibi, INDIVIDUAL)", "job_assignment", "pay", "hayır",
     "403 JobAssignmentMessages.NOT_CREATOR_OF_JOB", ["initiatePaymentSession"],
     "Durum kontrolü (409 ASSIGNMENT_NOT_READY_FOR_PAYMENT, :249) yetkiden ÖNCE: taraf olmayan atamanın ödeme durumunu öğrenebilir."),
    (C + "JobAssignmentManager.java:274", "isUserAContactOfCompany (COMPANY)", "job_assignment", "pay", "hayır",
     "403 JobAssignmentMessages.USER_IS_NOT_A_CONTACT_OF_COMPANY", ["initiatePaymentSession"],
     "Şirket ilanında ilan sahibi (creator) şirketten ayrıldıysa ödeyemez; JobEditors'tan farklı (creator dahil değil)."),
    (C + "PaymentManager.java:368", "AssignmentParticipants.isParticipant", "job_assignment", "read_payments", "evet-yardımcısız",
     "404 PaymentMessages.ASSIGNMENT_NOT_FOUND", ["getAttemptsForAssignment"], ""),
    (C + "CancellationManager.java:85", "AssignmentParticipants.partyOf", "job_assignment", "cancel", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["cancelAssignment"], "Taraf (WORKER/EMPLOYER) ceza politikasını da seçer."),
    (C + "CancellationManager.java:142", "AssignmentParticipants.partyOf", "job_assignment", "preview_cancellation", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["previewCancellation"], ""),
    (C + "CancellationManager.java:215", "AssignmentParticipants.isParticipant", "job_assignment", "read_cancellation", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["getForAssignment"], ""),
    (C + "TimesheetManager.java:191", "doğrudan getId().equals (requireWorker)", "job_assignment", "check_in", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["checkIn"], "Emanet kapısı (HELD_IN_ESCROW) ayrıca :83'te."),
    (C + "TimesheetManager.java:191", "doğrudan getId().equals (requireWorker)", "job_assignment", "check_out", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["checkOut"], ""),
    (C + "TimesheetManager.java:173", "AssignmentParticipants.isParticipant", "job_assignment", "read_work_sessions", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["getSessions"], "Konum alanlarının maskesi ayrı (#349/#352)."),
    (C + "DisputeManager.java:106", "AssignmentParticipants.isParticipant", "job_assignment", "open_dispute", "kısmi",
     "404 DisputeMessages.NOT_A_PARTICIPANT", ["openDispute"], "Var olmayan atama 404 JobAssignment NOT_FOUND; taraf olmayan farklı mesaj."),
    (C + "DisputeManager.java:183", "AssignmentParticipants.isParticipant", "job_assignment", "read_disputes", "kısmi",
     "404 DisputeMessages.NOT_A_PARTICIPANT", ["getForAssignment"], "Aynı mesaj farkı."),
    (C + "ReviewManager.java:125", "doğrudan getId().equals + isUserAContactOfCompany (resolveSubject)", "job_assignment", "review", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["leaveReview"], "Taraf, değerlendirilen kişiyi de belirler (işçi → ilan sahibi; işveren tarafı → işçi)."),
    (C + "IdentityRevealManager.java:93", "AssignmentParticipants.partyOf == EMPLOYER", "job_assignment", "reveal_worker_identity", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["revealWorkerIdentity"], "Saatlik oran sınırı ayrıca (:105)."),
    (C + "IdentityRevealManager.java:97", "durum ön süzgeci (HIRED)", "job_assignment", "reveal_worker_identity", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["revealWorkerIdentity"], "ReBAC dışı ön süzgeç: atama HIRED değilse gizli 404."),
    (C + "TransportManager.java:1239", "AssignmentParticipants.isParticipant (requireParty)", "job_assignment", "read_transport", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["getAssignmentTransport"], ""),
    (C + "TransportManager.java:1239", "AssignmentParticipants.isParticipant (requireParty)", "job_assignment", "select_seat", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["selectSeat"], "İşveren tarafı da işçi adına koltuk seçebilir; kasıtlı (TransportPlanIT#theEmployerMayPickTheSeatOnBehalfOfTheWorker…)."),
    (C + "TransportManager.java:1239", "AssignmentParticipants.isParticipant (requireParty)", "job_assignment", "issue_boarding_qr", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["issueWorkerQr"], "İlk kat; ikinci kat :549 yalnız işçi."),
    (C + "TransportManager.java:549", "doğrudan getId().equals (işçi)", "job_assignment", "issue_boarding_qr", "evet",
     "404 JobAssignmentMessages.NOT_FOUND", ["issueWorkerQr"], ""),
    # ---------------- job ----------------
    (C + "JobManager.java:493", "JobEditors.isEditor (requireJobEditor)", "job", "manage_gallery", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["addJobImage"], ""),
    (C + "JobManager.java:493", "JobEditors.isEditor (requireJobEditor)", "job", "manage_gallery", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["replaceJobGallery"], ""),
    (C + "JobManager.java:493", "JobEditors.isEditor (requireJobEditor)", "job", "manage_gallery", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["removeJobImage"], ""),
    (C + "JobManager.java:493", "JobEditors.isEditor (requireJobEditor)", "job", "cancel", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["cancelJob"], ""),
    (C + "JobManager.java:493", "JobEditors.isEditor (requireJobEditor)", "job", "close", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["closeJob"], ""),
    (C + "GalleryManager.java:209", "kapsama (görsel ilanın galerisinde mi)", "job", "manage_gallery", "kısmi",
     "404 GalleryMessages.IMAGE_NOT_IN_GALLERY", ["!DELETE /jobs/{jobId}/images/{imageId}"], "Alt nesne ebeveyne ait mi; requireJobEditor'dan sonra."),
    (C + "JobAssignmentManager.java:493", "JobEditors.isEditor", "job", "list_assignments", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["getAssignmentsForJob"], ""),
    (C + "TransportManager.java:1229", "JobEditors.isEditor (requireEditor)", "job", "write_transport_plan", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["writePlan"], ""),
    (C + "TransportManager.java:1229", "JobEditors.isEditor (requireEditor)", "job", "read_transport_plan", "evet",
     "404 JobMessages.JOB_NOT_FOUND", ["getPlan"], ""),
    (C + "TransportManager.java:212", "kapsama (servis plana ait mi)", "job", "write_transport_plan", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["writePlan"], "Gövdedeki shuttle id başka ilanın planındaysa gizli 404."),
    (C + "TransportManager.java:268", "kapsama (durak servise ait mi)", "job", "write_transport_plan", "evet",
     "404 TransportMessages.STOP_NOT_FOUND", ["writePlan"], ""),
    (C + "JobApplicationManager.java:139", "isUserAContactOfCompany (COMPANY)", "job", "list_applications", "hayır",
     "400 JobApplicationMessages.USER_NOT_CONTACT_OF_COMPANY_TO_VIEW_APPLICATIONS", ["getJobApplicationsByIdJobId"],
     "ValidationException → 400; şirket ilanında creator dahil değil (JobEditors'tan farklı)."),
    (C + "JobApplicationManager.java:143", "doğrudan getId().equals (INDIVIDUAL)", "job", "list_applications", "kısmi",
     "404 JobApplicationMessages.USER_NOT_CREATOR_OF_INDIVIDUAL_JOB_TO_VIEW_APPLICATIONS", ["getJobApplicationsByIdJobId"],
     "Aynı uçta şirket dalı 400, bireysel dal 404: tutarsız."),
    (C + "JobApplicationManager.java:100", "doğrudan getId().equals (kendi ilanı) — negatif ilişki", "job", "apply", "hayır",
     "400 JobApplicationMessages.USER_CANNOT_APPLY_TO_OWN_JOB", ["applyToJob"], "editor başvuramaz."),
    (C + "JobApplicationManager.java:110", "isUserAContactOfCompany — negatif ilişki", "job", "apply", "hayır",
     "400 JobApplicationMessages.CONTACT_USER_CANNOT_APPLY_TO_COMPANY_JOB", ["applyToJob"], ""),
    (C + "MessagingManager.java:456", "doğrudan getId().equals + isUserAContactOfCompany (assertMayConverse)", "job", "open_conversation", "kısmi",
     "404 MessagingMessages.NOT_A_PARTICIPANT", ["openConversation"],
     "İşveren tarafı herhangi bir workerId ile sohbet açabilir; ödeme ön süzgeci ayrıca."),
    (C + "MessagingManager.java:463", "assertNotBlocked (engelleme)", "job", "open_conversation", "hayır",
     "403 MessagingMessages.BLOCKED", ["openConversation"], "ReBAC dışı ön süzgeç (user_blocks, iki yönlü)."),
    (C + "MessagingManager.java:421", "MessagingAvailability ön süzgeci (refuseUnlessAvailable)", "job", "open_conversation", "hayır",
     "403 MessagingMessages.PAYMENT_REQUIRED / 409 WINDOW_CLOSED", ["openConversation"], "Durum ön süzgeci: atama emanette değilse 403."),
    (C + "JobTemplateManager.java:103", "doğrudan getId().equals + isUserAContactOfCompany (assertCanCopy)", "job", "copy_as_template", "evet-yardımcısız",
     "404 JobTemplateMessages.JOB_NOT_FOUND", ["createFromJob"], "JobEditors kopyası (aynı kural, ayrı kod)."),
    (C + "JobManager.java:624", "JobEditors.isEditor (projeksiyon)", "job", "read_employer_view", "yok",
     "— (alan düzeyi)", ["getJobDetail"], "Ret yok; EMPLOYER görünümü yalnız editor'a. GET /jobs/{id} her durumda (CLOSED/CANCELLED dahil) herkese açık."),
    (C + "JobManager.java:628", "doğrudan getId().equals (projeksiyon, yalnız creator)", "job", "read_employer_view", "yok",
     "— (alan düzeyi)", ["search"], "Arama listesinde EMPLOYER görünümü yalnız creator'a; detayda editor'a. Tutarsız."),
    ("core/helpers/JobAddressVisibility.java:40", "JobAddressVisibility.precision (projeksiyon)", "job", "read_exact_address", "yok",
     "— (alan düzeyi: maskeli adres)",
     ["!GET /jobs/{jobId}", "!GET /jobs", "!GET /job-assignments/{assignmentId}", "!GET /users/me/assignments",
      "!GET /users/me/applications"],
     "Tam adres: creator VEYA atanmış işçi (ADDRESS_VISIBLE durumları) VEYA şirket adresi herkese açık. Şirket yetkilisi (contact) dahil DEĞİL."),
    (C + "JobManager.java:131", "kullanıcı özniteliği (isEmployer)", "user", "create_individual_job", "hayır",
     "403 JobMessages.USER_IS_NOT_EMPLOYER", ["createIndividualJob"], "Öznitelik kapısı; ayrıca kimlik doğrulanmış olmalı (:135)."),
    # ---------------- job_application ----------------
    (C + "JobApplicationManager.java:307", "isUserAContactOfCompany (assertCurrentUserIsEmployerOf, COMPANY)", "job_application", "decide", "evet-yardımcısız",
     "404 JobApplicationMessages.JOB_APPLICATION_NOT_FOUND", ["respondToApplication"], "Şirket ilanında creator dahil değil."),
    (C + "JobApplicationManager.java:315", "doğrudan getId().equals (assertCurrentUserIsEmployerOf, INDIVIDUAL)", "job_application", "decide", "evet-yardımcısız",
     "404 JobApplicationMessages.JOB_APPLICATION_NOT_FOUND", ["respondToApplication"], ""),
    (C + "JobApplicationManager.java:247", "doğrudan getId().equals (başvuran)", "job_application", "withdraw", "evet-yardımcısız",
     "404 JobApplicationMessages.JOB_APPLICATION_NOT_FOUND", ["withdrawApplication"], ""),
    # ---------------- company ----------------
    (C + "CompanyManager.java:160", "requireCompanyAdmin", "company", "invite_contact", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["inviteContactToCompany"], "Var olmayan şirket 404 COMPANY_NOT_FOUND; farklı mesaj."),
    (C + "CompanyManager.java:267", "requireCompanyAdmin", "company", "update", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["updateCompany"], ""),
    (C + "CompanyManager.java:308", "requireCompanyAdmin", "company", "remove_contact", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["removeContact"], "Son ADMIN çıkarılamaz (409)."),
    (C + "CompanyManager.java:385", "requireCompanyAdmin", "company", "manage_logo", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["updateLogo"], ""),
    (C + "CompanyManager.java:398", "requireCompanyAdmin", "company", "manage_logo", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["removeLogo"], ""),
    (C + "CompanyManager.java:436", "requireCompanyAdmin", "company", "manage_gallery", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["addCompanyImage"], ""),
    (C + "CompanyManager.java:443", "requireCompanyAdmin", "company", "manage_gallery", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["replaceCompanyGallery"], ""),
    (C + "CompanyManager.java:450", "requireCompanyAdmin", "company", "manage_gallery", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["removeCompanyImage"], ""),
    (C + "GalleryManager.java:121", "kapsama (görsel şirket galerisinde mi)", "company", "manage_gallery", "kısmi",
     "404 GalleryMessages.IMAGE_NOT_IN_GALLERY", ["!DELETE /companies/{companyId}/images/{imageId}"], ""),
    (C + "CompanyManager.java:301", "isUserAContactOfCompany (requireCompanyMember)", "company", "list_contacts", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["getContacts"], ""),
    (C + "CompanyManager.java:258", "isUserAContactOfCompany (projeksiyon)", "company", "read_approval_note", "yok",
     "— (alan düzeyi)", ["getCompanyDetail"], "approvalNote yalnız üyeye; şirket detayı her kimlikli kullanıcıya açık."),
    (C + "JobManager.java:239", "doğrudan getId().equals (contacts akışı)", "company", "create_job", "hayır",
     "400 JobMessages.USER_IS_NOT_A_CONTACT_OF_COMPANY", ["createCorporateJob"], "ValidationException → 400; isUserAContactOfCompany'nin kopyası."),
    (C + "JobManager.java:402", "isUserAContactOfCompany", "company", "list_jobs", "hayır",
     "403 JobMessages.USER_IS_NOT_A_CONTACT_OF_COMPANY", ["getJobsOfCompany"], ""),
    (C + "CompanyContactInvitationManager.java:217", "isUserAdminOfCompany", "company", "list_invitations", "kısmi",
     "404 CompanyMessages.USER_NOT_ASSOCIATED_WITH_COMPANY", ["getCompanyContactInvitationsByCompanyIdDto"], ""),
    # ---------------- company_invitation ----------------
    (C + "CompanyContactInvitationManager.java:127", "CompanyContactInvitation.wasSentTo", "company_invitation", "answer", "evet-yardımcısız",
     "404 CompanyContactInvitationMessages.COMPANY_CONTACT_INVITATION_NOT_FOUND", ["answerToInvitation"], "Yalnız davet edilen."),
    (C + "CompanyContactInvitationManager.java:198", "isPartyTo || isUserAdminOfCompany", "company_invitation", "read", "evet-yardımcısız",
     "404 CompanyContactInvitationMessages.COMPANY_CONTACT_INVITATION_NOT_FOUND", ["getInvitationById"], "Davet eden, davet edilen ya da şirket ADMIN'i."),
    # ---------------- conversation / message ----------------
    (C + "MessagingManager.java:440", "Conversation.hasParticipant (loadParticipating)", "conversation", "read", "evet",
     "404 MessagingMessages.CONVERSATION_NOT_FOUND", ["getConversation"], ""),
    (C + "MessagingManager.java:440", "Conversation.hasParticipant (loadParticipating)", "conversation", "read", "evet",
     "404 MessagingMessages.CONVERSATION_NOT_FOUND", ["getMessages"], ""),
    (C + "MessagingManager.java:440", "Conversation.hasParticipant (loadParticipating)", "conversation", "read", "evet",
     "404 MessagingMessages.CONVERSATION_NOT_FOUND", ["getMessagesAfter"], ""),
    (C + "MessagingManager.java:440", "Conversation.hasParticipant (loadParticipating)", "conversation", "mark_read", "evet",
     "404 MessagingMessages.CONVERSATION_NOT_FOUND", ["markRead"], ""),
    (C + "MessagingManager.java:440", "Conversation.hasParticipant (loadParticipating)", "conversation", "post", "evet",
     "404 MessagingMessages.CONVERSATION_NOT_FOUND", ["sendMessage"], ""),
    (C + "MessagingManager.java:247", "kapsama (anchor mesaj sohbete ait mi)", "conversation", "read", "hayır",
     "400 MessagingMessages.AFTER_UNKNOWN", ["getMessagesAfter"], "Başka sohbetin mesaj id'si 400 (ValidationException)."),
    (C + "MessagingManager.java:404", "assertCrewMemberMayMessage (GROUP)", "conversation", "post", "hayır",
     "403 MessagingMessages.PAYMENT_REQUIRED / 409 WINDOW_CLOSED", ["sendMessage"],
     "Grup: işveren her zaman; şirket yetkilisi ataması yoksa; işçi yalnız ödeme ön süzgeci geçerse."),
    (C + "MessagingManager.java:421", "MessagingAvailability ön süzgeci (DIRECT)", "conversation", "post", "hayır",
     "403 MessagingMessages.PAYMENT_REQUIRED / 409 WINDOW_CLOSED", ["sendMessage"], ""),
    (C + "MessagingManager.java:463", "assertNotBlocked (DIRECT)", "conversation", "post", "hayır",
     "403 MessagingMessages.BLOCKED", ["sendMessage"], "ReBAC dışı ön süzgeç."),
    (C + "MessagingManager.java:485", "Conversation.hasParticipant (assertParticipant)", "conversation", "typing", "evet",
     "STOMP ERROR (404 CONVERSATION_NOT_FOUND)", ["!STOMP SEND /app/conversations/{conversationId}/typing"], "RealtimeController.typing."),
    ("core/realtime/StompAuthChannelInterceptor.java:151", "Conversation.hasParticipant (SUBSCRIBE)", "conversation", "subscribe", "hayır",
     "STOMP ERROR (403 SUBSCRIPTION_NOT_ALLOWED)", ["!STOMP SUBSCRIBE /topic/conversations/{conversationId}"],
     "MessagingManager kuralının ayrı kopyası (doğrudan DAO)."),
    ("core/realtime/StompAuthChannelInterceptor.java:162", "kendisi || ortak sohbet (existsBetweenParticipants)", "user", "watch_presence", "hayır",
     "STOMP ERROR (403 SUBSCRIPTION_NOT_ALLOWED)", ["!STOMP SUBSCRIBE /topic/presence/{userId}"], "Modelde contact ilişkisi yok (§5.4 notu)."),
    ("core/realtime/StompAuthChannelInterceptor.java:146", "varsayılan ret (bilinmeyen hedef)", "realtime_destination", "subscribe", "hayır",
     "STOMP ERROR (403 SUBSCRIPTION_NOT_ALLOWED)", ["!STOMP SUBSCRIBE /topic/**"], "/user/** kuyrukları broker'ın kullanıcı kapsamına bırakılıyor (:127)."),
    ("core/realtime/StompAuthChannelInterceptor.java:78", "YOK (yalnız oran sınırı)", "conversation", "publish_to_topic", "yok",
     "—", ["!STOMP SEND /topic/conversations/{conversationId}"],
     "DOĞRULANMADI: simple broker '/topic' SEND'i aboneye iletebilir; katılımcı kontrolü yok. Test yok (StompSubscriptionSecurityTest SEND'i yalnız 'dokunulmadan geçer' diye bağlıyor)."),
    ("webAPI/RealtimeController.java:82", "YOK", "user", "read_presence", "yok",
     "—", ["!GET /users/{userId}/presence"], "STOMP'taki 'kendisi ya da ortak sohbet' kuralı REST'te yok: herkesin çevrimiçi/son görülme bilgisi okunabiliyor."),
    ("webAPI/RealtimeController.java:95", "YOK", "user", "read_presence", "yok",
     "—", ["!POST /presence/lookup"], "Toplu; liste boyutu sınırsız."),
    (C + "MessagingManager.java:494", "DAO WHERE (findReportableByIdForUpdate: mesajın sohbetine katılımcı)", "message", "report", "evet-yardımcısız",
     "404 MessagingMessages.MESSAGE_NOT_FOUND", ["!POST /reports"], "ReportManager.createReport → getMessageForReport."),
    # ---------------- dispute ----------------
    (C + "DisputeManager.java:237", "AssignmentParticipants.isParticipant (isParticipantOf)", "dispute", "add_evidence", "evet",
     "404 DisputeMessages.NOT_FOUND", ["attachEvidence"], "RESOLVED ise 409."),
    (C + "DisputeManager.java:294", "AssignmentParticipants.isParticipant (readableDispute)", "dispute", "read_evidence", "evet",
     "404 DisputeMessages.NOT_FOUND", ["getEvidence"], ""),
    (C + "DisputeManager.java:294", "AssignmentParticipants.isParticipant (readableDispute)", "dispute", "download_evidence", "evet",
     "404 DisputeMessages.NOT_FOUND", ["getEvidenceForDownload"], ""),
    (C + "DisputeManager.java:284", "kapsama (kanıt uyuşmazlığa ait mi, DAO WHERE)", "dispute", "download_evidence", "kısmi",
     "404 DisputeMessages.EVIDENCE_NOT_FOUND", ["getEvidenceForDownload"], ""),
    # ---------------- transport ----------------
    (C + "TransportManager.java:1249", "JobEditors.isEditor (requireShuttleEditor)", "shuttle", "issue_qr", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["issueShuttleQr"], ""),
    (C + "TransportManager.java:1249", "JobEditors.isEditor (requireShuttleEditor)", "shuttle", "rotate_qr", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["rotateShuttleQr"], ""),
    (C + "TransportManager.java:1249", "JobEditors.isEditor (requireShuttleEditor)", "shuttle", "list_boardings", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["listBoardings"], ""),
    (C + "TransportManager.java:579", "koltuk sahibi (findWorkerSeatsOnJob, işçi)", "shuttle", "board", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND (yabancı) / 409 TRANSPORT_SEAT_NOT_SELECTED (koltuksuz taraf)", ["boardOnShuttle"],
     "refusalWithoutSeat: ilanın tarafı olan koltuksuz işçi 409 ile sebebi öğrenir; yabancı gizli 404 alır."),
    (C + "TransportManager.java:584", "kapsama (koltuk bu servise mi ait)", "shuttle", "board", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["boardOnShuttle"], ""),
    (C + "TransportManager.java:606", "JobEditors.isEditor (koltuğun ilanı)", "seat_assignment", "mark_boarding", "evet",
     "404 TransportMessages.SEAT_ASSIGNMENT_NOT_FOUND", ["markSeatBoarding"], ""),
    (C + "TransportManager.java:1213", "kapsama (servis atamanın ilan planında mı)", "job_assignment", "select_seat", "evet",
     "404 TransportMessages.SHUTTLE_NOT_FOUND", ["selectSeat"], ""),
    (C + "TransportManager.java:1221", "kapsama (durak seçilen serviste mi)", "job_assignment", "select_seat", "evet",
     "404 TransportMessages.STOP_NOT_FOUND", ["selectSeat"], ""),
    # ---------------- dosya / görsel ----------------
    (C + "CvManager.java:63", "sahip || ROLE_ADMIN || başvuru almış işveren (hasApplicationForEmployer)", "private_file", "download", "evet-yardımcısız",
     "404 StorageMessages.FILE_NOT_FOUND", ["assertCanDownload"],
     "Başvuru ilişkisi kalıcı: herhangi bir zamanda herhangi bir ilanına başvurmuş işçinin CV'sini indirir. Marketplace ROLE_ADMIN baypası personel modeline taşınmamış."),
    (C + "PublicProfileManager.java:81", "kendisi || hasApplicationForEmployer (projeksiyon)", "user", "read_cv_url", "yok",
     "— (alan düzeyi)", ["getPublicProfile"], "CvManager kuralının ikinci kopyası (ROLE_ADMIN yok)."),
    (C + "ImageManager.java:107", "doğrudan getId().equals || ROLE_ADMIN", "image", "delete", "hayır",
     "400 ImageMessages.IMAGE_PERMISSION_ERROR", ["deleteImage"], "Var olmayan görsel 400 IMAGE_NOT_FOUND_ERROR, yetkisiz 400 farklı mesaj: var olma sızar. ROLE_ADMIN baypası."),
    # ---------------- sahip (owner) ----------------
    (C + "ExperienceManager.java:59", "requireOwned", "experience", "update", "evet",
     "404 ExperienceMessages.NOT_FOUND", ["updateExperience"], ""),
    (C + "ExperienceManager.java:70", "requireOwned", "experience", "delete", "evet",
     "404 ExperienceMessages.NOT_FOUND", ["deleteExperience"], ""),
    (C + "EducationManager.java:57", "requireOwned", "education", "update", "evet",
     "404 EducationMessages.NOT_FOUND", ["updateEducation"], ""),
    (C + "EducationManager.java:68", "requireOwned", "education", "delete", "evet",
     "404 EducationMessages.NOT_FOUND", ["deleteEducation"], ""),
    (C + "JobAlertManager.java:72", "requireOwned", "job_alert", "update", "evet-yardımcısız",
     "404 JobAlertMessages.NOT_FOUND", ["updateAlert"], ""),
    (C + "JobAlertManager.java:83", "requireOwned", "job_alert", "delete", "evet-yardımcısız",
     "404 JobAlertMessages.NOT_FOUND", ["deleteAlert"], ""),
    (C + "BankAccountManager.java:100", "requireOwned (DAO WHERE findByIdAndUserIdAndActiveTrue)", "bank_account", "select_primary", "evet-yardımcısız",
     "404 BankAccountMessages.NOT_FOUND", ["selectMyPrimaryAccount"], ""),
    (C + "BankAccountManager.java:112", "requireOwned (DAO WHERE)", "bank_account", "delete", "evet-yardımcısız",
     "404 BankAccountMessages.NOT_FOUND", ["deleteMyAccount"], ""),
    (C + "BankAccountManager.java:174", "requireOwned (DAO WHERE)", "bank_account", "start_verification", "evet-yardımcısız",
     "404 BankAccountMessages.NOT_FOUND", ["startMyVerification"], ""),
    (C + "JobTemplateManager.java:84", "doğrudan getId().equals (getMyTemplate)", "job_template", "read", "evet",
     "404 JobTemplateMessages.NOT_FOUND", ["getMyTemplate"], ""),
    (C + "JobTemplateManager.java:84", "doğrudan getId().equals (getMyTemplate)", "job_template", "delete", "evet",
     "404 JobTemplateMessages.NOT_FOUND", ["deleteTemplate"], ""),
    (C + "DeviceManager.java:122", "DAO WHERE (findByIdAndUser_Id)", "device", "rename", "evet-yardımcısız",
     "404 DeviceMessages.DEVICE_NOT_FOUND", ["renameMyDevice"], ""),
    (C + "SessionManager.java:145", "doğrudan getId().equals (myActiveSession)", "session", "read", "evet",
     "404 MyWorkMessages.SESSION_NOT_FOUND", ["getMySessionDetail"], ""),
    (C + "SessionManager.java:145", "doğrudan getId().equals (myActiveSession)", "session", "revoke", "evet",
     "404 MyWorkMessages.SESSION_NOT_FOUND", ["revokeSession"], ""),
    (C + "AvailabilityManager.java:102", "doğrudan getId().equals", "availability_exception", "delete", "evet-yardımcısız",
     "404 AvailabilityMessages.NOT_FOUND", ["deleteMyException"], ""),
    (C + "UserVerificationManager.java:49", "doğrudan getId().equals", "user_verification", "confirm", "hayır",
     "403 UserVerificationMessages.VERIFICATION_NOT_ASSOCIATED_WITH_USER", ["verify"], "403/404 ayrımı var olmayı sızdırıyor (N01 §9.d)."),
    (C + "NotificationManager.java:483", "doğrudan getId().equals (filter)", "notification", "mark_read", "sessiz",
     "200 (no-op)", ["markRead"], "Başkasının bildirimi sessizce yok sayılıyor."),
    # ---------------- kontrol eksik ----------------
    (C + "TalentPoolManager.java:80", "YOK (companyId üyelik kontrolü yok)", "company", "tag_talent_pool_entry", "yok",
     "—", ["addWorker"], "Kişi kendi yetenek havuzu kaydını herhangi bir şirketle etiketleyebiliyor; üyelik kontrolü yok."),
    (C + "AddressSearchManager.java:258", "bellek içi oturum sahibi (requireSession)", "address_search_session", "use", "hayır",
     "400 AddressMessages.SEARCH_SESSION_INVALID", ["search"], "Başkasının oturumu, süresi dolmuş ve tamamlanmış oturum aynı 400."),
    (C + "AddressSearchManager.java:258", "bellek içi oturum sahibi (requireSession)", "address_search_session", "use", "hayır",
     "400 AddressMessages.SEARCH_SESSION_INVALID", ["resolve"], ""),
    # ---------------- kapsam (liste) — ilişki taşıyan, "kendi" dışı ----------------
    (C + "JobApplicationManager.java:365", "kapsam: creator = viewer VEYA şirket ∈ üyelikler (DAO RECEIVED_SCOPE)", "job_application", "list_received", "yok",
     "— (liste süzgeci)", ["getReceivedApplications"], "Scope karşılığı: job_application#can_read (işveren dalı)."),
    (C + "JobAssignmentManager.java:477", "kapsam: asEmployer → yalnız creator (findAllByJobCreatorId)", "job_assignment", "list_mine", "yok",
     "— (liste süzgeci)", ["getMyAssignments"],
     "Şirket yetkilisi 'işveren olarak atamalarım' listesinde şirket ilanlarının atamalarını GÖRMEZ; tekil okumada (partyOf) görür. Tutarsız."),
    (C + "MessagingManager.java:284", "kapsam: katılımcı (findAllForParticipant)", "conversation", "list_mine", "yok",
     "— (liste süzgeci)", ["getMyConversations"], "Scope karşılığı: conversation#can_read."),
    # ---------------- tasarım gereği açık / kimliksiz ----------------
    ("webAPI/JobsController.java:104", "tasarım gereği açık (permitAll)", "job", "read", "yok", "—",
     ["!GET /jobs/{jobId}"], "Her durumdaki ilan (CLOSED/CANCELLED dahil) kimliksiz okunur. §5.4'teki public_viewer 'yalnız OPEN iken' der."),
    ("webAPI/JobsController.java:153", "tasarım gereği açık (permitAll)", "job", "read_gallery", "yok", "—",
     ["!GET /jobs/{jobId}/images"], ""),
    ("webAPI/CompaniesController.java:194", "tasarım gereği açık (kimlikli her kullanıcı)", "company", "read", "yok", "—",
     ["!GET /companies/{companyId}"], "Onaylanmamış şirket de okunur (doğrulanmadı: onay durumu süzgeci yok görünüyor)."),
    ("webAPI/CompaniesController.java:304", "tasarım gereği açık", "company", "read_gallery", "yok", "—",
     ["!GET /companies/{companyId}/images"], ""),
    ("webAPI/UsersController.java:94", "tasarım gereği açık (kimlikli her kullanıcı)", "user", "read_public_profile", "yok", "—",
     ["!GET /users/{userId}"], "CV URL'si ayrıca projeksiyonla süzülür (PublicProfileManager:81)."),
    ("webAPI/UsersController.java:109", "tasarım gereği açık (permitAll)", "user", "read_employer_stats", "yok", "—",
     ["!GET /users/{userId}/employer-stats"], ""),
    ("webAPI/WorkerProfileController.java:86", "tasarım gereği açık (permitAll)", "user", "read_experiences", "yok", "—",
     ["!GET /users/{userId}/experiences"], ""),
    ("webAPI/WorkerProfileController.java:147", "tasarım gereği açık (permitAll)", "user", "read_availability", "yok", "—",
     ["!GET /users/{userId}/availability"], "İşçinin müsaitlik takvimi herkese açık (N01 §9.d)."),
    ("webAPI/BadgesController.java:53", "tasarım gereği açık (permitAll)", "user", "read_badges", "yok", "—",
     ["!GET /users/{userId}/badges"], ""),
    ("webAPI/ReviewsController.java:79", "tasarım gereği açık (permitAll)", "user", "read_reviews", "yok", "—",
     ["!GET /users/{userId}/reviews"], ""),
    ("webAPI/ReviewsController.java:97", "tasarım gereği açık (permitAll)", "user", "read_rating_summary", "yok", "—",
     ["!GET /users/{userId}/rating-summary"], ""),
    ("webAPI/NotificationTrackingController.java:40", "yetenek URL'si (bildirim UUID'si, kimliksiz)", "notification", "mark_opened", "kısmi",
     "404 NotificationMessages.NOTIFICATION_NOT_FOUND_WITH_ID", ["!GET /notification-tracking/{id}.png"],
     "Uygun olmayan bildirim sessizce geçilir; var olmayan 404 (var olma sızar, UUID tahmin edilemez varsayımı)."),
    (C + "PaymentManager.java:288", "sağlayıcı token'ı (kimliksiz; gövdeye güvenilmez, sunucudan sorgu)", "payment_attempt", "complete_callback", "yok",
     "—", ["!POST /payments/callback"], "Yetki yerine sağlayıcı doğrulaması: reconcile karar zinciri (test/oracle)."),
    (C + "PaymentManager.java:211", "sağlayıcı token'ı (tüketmeyen GET karşılığı)", "payment_attempt", "read_outcome", "yok",
     "—", ["!GET /payments/callback"], "#252: kaydedilmiş sonucu okur, durum değiştirmez."),
]


# Elle doğrulanmış bağlayan testler: (dosya:satır, kök ya da uç) -> test etiketleri.
# Test gövdesi okunarak seçildi; yoksa otomatik eşleşme süzülerek kullanılır.
EC = "ExistenceConcealmentIT.java"
AOA = "AssignmentOperationsAuthorizationTest.java"
OVERRIDES = {
    ("JobAssignmentManager.java:505", "getMyAssignment"): [f"{EC}:148#readingSomeoneElsesAssignmentLooksExactlyLikeReadingOneThatDoesNotExist", "AssignmentReadIT.java:602#anOutsiderGetsTheConcealedNotFoundResponse"],
    ("JobAssignmentManager.java:200", "answerToAssignment"): [f"{EC}:162#respondingToSomeoneElsesPendingAssignmentLooksExactlyLikeRespondingToOneThatDoesNotExist", f"{EC}:170#respondingToSomeoneElsesTerminalAssignmentLooksExactlyLikeRespondingToOneThatDoesNotExist"],
    ("JobAssignmentManager.java:601", "completeAssignment"): [f"{EC}:222#completingSomeoneElsesAssignmentLooksExactlyLikeCompletingOneThatDoesNotExist", "AssignmentCompletionIT.java:181#theWorkerCannotCompleteTheirOwnAssignment", f"{AOA}:431#onlyTheEmployerCanMarkTheWorkComplete"],
    ("JobAssignmentManager.java:261", "initiatePaymentSession"): ["PaymentSessionAuthorizationTest.java:104#aStrangerCannotInitiatePaymentForAnIndividualJob", "PaymentSessionAuthorizationTest.java:116#theWorkerCannotInitiatePaymentForTheirOwnAssignment"],
    ("JobAssignmentManager.java:274", "initiatePaymentSession"): ["PaymentSessionAuthorizationTest.java:143#aUserWhoIsNotAContactCannotInitiatePaymentForACompanyJob"],
    ("PaymentManager.java:368", "getAttemptsForAssignment"): ["PaymentAccessRulesTest.java:91#aStrangerCannotSeeTheAttempts"],
    ("CancellationManager.java:85", "cancelAssignment"): [f"{EC}:199#cancellingSomeoneElsesAssignmentLooksExactlyLikeCancellingOneThatDoesNotExist", f"{AOA}:170#aStrangerCannotCancelSomeoneElsesAssignment"],
    ("CancellationManager.java:142", "previewCancellation"): [f"{EC}:141#previewingSomeoneElsesCancellationLooksExactlyLikePreviewingOneThatDoesNotExist", "CancellationPreviewTest.java:163#someoneWhoIsNotAPartyGetsNotFound"],
    ("CancellationManager.java:215", "getForAssignment"): [f"{EC}:192#readingSomeoneElsesCancellationLooksExactlyLikeReadingOneThatDoesNotExist"],
    ("TimesheetManager.java:191", "checkIn"): [f"{EC}:215#checkingIntoSomeoneElsesAssignmentLooksExactlyLikeCheckingIntoOneThatDoesNotExist", f"{AOA}:343#onlyTheWorkerCanCheckIn"],
    ("TimesheetManager.java:173", "getSessions"): [f"{EC}:208#readingSomeoneElsesTimesheetLooksExactlyLikeReadingOneThatDoesNotExist", f"{AOA}:423#aStrangerCannotReadTheTimesheet"],
    ("DisputeManager.java:106", "openDispute"): [f"{AOA}:257#aStrangerCannotOpenADispute"],
    ("ReviewManager.java:125", "leaveReview"): [f"{EC}:229#reviewingSomeoneElsesAssignmentLooksExactlyLikeReviewingOneThatDoesNotExist", "ReviewRulesTest.java:109#aStrangerCannotReviewSomeoneElsesJob"],
    ("IdentityRevealManager.java:93", "revealWorkerIdentity"): ["WorkerIdentityRevealIT.java:181#anOutsiderSeesTheAssignmentAsMissing", "WorkerIdentityRevealIT.java:199#aContactOfAnotherCompanySeesTheAssignmentAsMissing", "WorkerIdentityRevealIT.java:176#theWorkerCannotReadTheirOwnIdentityThroughTheEmployerEndpoint"],
    ("IdentityRevealManager.java:97", "revealWorkerIdentity"): [],
    ("TransportManager.java:1239", "selectSeat"): ["TransportPlanIT.java:367#theEmployerMayPickTheSeatOnBehalfOfTheWorkerAndCapacityCannotDropBelowSeats (olumlu: işveren adına seçim)"],
    ("TransportManager.java:1213", "selectSeat"): ["TransportPlanIT.java:339#aFullShuttleRefusesTheNextSeatAndAForeignShuttleIsConcealed"],
    ("JobAssignmentManager.java:493", "getAssignmentsForJob"): [f"{EC}:155#listingSomeoneElsesJobAssignmentsLooksExactlyLikeListingOnesOfAJobThatDoesNotExist", "AssignmentReadIT.java:265#theWorkerAndAnOutsiderGetTheConcealedNotFoundForTheJobsAssignments"],
    ("TransportManager.java:1229", "writePlan"): ["TransportPlanIT.java:245#aStrangerCannotTellTheJobFromOneThatDoesNotExist"],
    ("TransportManager.java:1229", "getPlan"): ["TransportPlanIT.java:245#aStrangerCannotTellTheJobFromOneThatDoesNotExist"],
    ("TransportManager.java:212", "writePlan"): [],
    ("TransportManager.java:268", "writePlan"): [],
    ("MessagingManager.java:456", "openConversation"): ["MessagingAuthorizationTest.java:448#aStrangerCannotOpenAConversationOnSomeoneElsesJob", "MessagingPaymentGateIT.java:325#anOutsiderStillGetsTheSameNotFoundBeforeAndAfterPayment"],
    ("MessagingManager.java:463", "openConversation"): ["MessagingAuthorizationTest.java:526#aBlockedUserCannotSendAMessageInEitherDirection (yalnız gönderim yönü; açma doğrulanmadı)"],
    ("MessagingManager.java:463", "sendMessage"): ["MessagingAuthorizationTest.java:526#aBlockedUserCannotSendAMessageInEitherDirection"],
    ("MessagingManager.java:421", "openConversation"): ["MessagingPaymentGateIT.java:145#nobodyCanOpenTheConversationBeforeTheEmployerPays"],
    ("MessagingManager.java:421", "sendMessage"): ["MessagingAuthorizationTest.java:436#aLegacyDirectConversationWithoutAnAssignmentIsReadOnly"],
    ("MessagingManager.java:404", "sendMessage"): ["MessagingAuthorizationTest.java:422#aFormerOfficerWithoutAnAssignmentCannotWriteInTheCrew", "MessagingAuthorizationTest.java:360#aRefundedWorkerStillListedInTheCrewCannotWriteThereAnyMore", "MessagingPaymentGateIT.java:398#anUnpaidWorkerSittingInTheCrewIsRefusedWithPaymentRequired"],
    ("MessagingManager.java:440", "getConversation"): [f"{EC}:129#readingSomeoneElsesSingleConversationLooksExactlyLikeReadingOneThatDoesNotExist"],
    ("MessagingManager.java:440", "getMessages"): [f"{EC}:123#readingSomeoneElsesConversationLooksExactlyLikeReadingOneThatDoesNotExist", "MessagingAuthorizationTest.java:456#aStrangerCannotReadOrWriteInAConversation"],
    ("MessagingManager.java:440", "getMessagesAfter"): [],
    ("MessagingManager.java:440", "markRead"): [f"{EC}:135#markingSomeoneElsesConversationReadLooksExactlyLikeMarkingOneThatDoesNotExist", "ConversationMessagingIT.java:436#anOutsiderMarkingReadGetsTheSameNotFoundAndLeavesNoStamp"],
    ("MessagingManager.java:440", "sendMessage"): ["MessagingAuthorizationTest.java:456#aStrangerCannotReadOrWriteInAConversation"],
    ("MessagingManager.java:247", "getMessagesAfter"): ["ConversationMessagingIT.java:241#anAfterFromAnotherConversationLooksExactlyLikeAnUnknownOne"],
    ("MessagingManager.java:485", "!STOMP SEND /app/conversations/{conversationId}/typing"): [],
    ("core/realtime/StompAuthChannelInterceptor.java:151", "!STOMP SUBSCRIBE /topic/conversations/{conversationId}"): ["StompSubscriptionSecurityTest.java:87#anOutsiderCannotSubscribeToSomeoneElsesConversation", "StompSubscriptionSecurityTest.java:93#subscribingToAConversationThatDoesNotExistIsRefused"],
    ("core/realtime/StompAuthChannelInterceptor.java:162", "!STOMP SUBSCRIBE /topic/presence/{userId}"): ["StompSubscriptionSecurityTest.java:136#presenceOfAStrangerCannotBeWatched", "StompSubscriptionSecurityTest.java:128#presenceOfSomeoneYouTalkToMayBeWatched"],
    ("core/realtime/StompAuthChannelInterceptor.java:146", "!STOMP SUBSCRIBE /topic/**"): ["StompSubscriptionSecurityTest.java:102#anUnknownDestinationIsRefusedRatherThanSilentlyAllowed"],
    ("core/realtime/StompAuthChannelInterceptor.java:78", "!STOMP SEND /topic/conversations/{conversationId}"): ["StompSubscriptionSecurityTest.java:149#framesOtherThanConnectAndSubscribePassThroughUntouched (açığı bağlıyor, kapatmıyor)"],
    ("DisputeManager.java:237", "attachEvidence"): [f"{EC}:244#attachingEvidenceToSomeoneElsesDisputeLooksExactlyLikeAttachingToOneThatDoesNotExist", "DisputeEvidenceRulesTest.java:147#aStrangerCannotAttachEvidence"],
    ("DisputeManager.java:294", "getEvidence"): [f"{EC}:252#listingSomeoneElsesDisputeEvidenceLooksExactlyLikeListingOneThatDoesNotExist", "DisputeEvidenceRulesTest.java:159#aStrangerCannotListEvidence"],
    ("DisputeManager.java:294", "getEvidenceForDownload"): [f"{EC}:258#downloadingSomeoneElsesDisputeEvidenceLooksExactlyLikeDownloadingOneThatDoesNotExist", "DisputeEvidenceRulesTest.java:169#aStrangerCannotDownloadEvidence"],
    ("DisputeManager.java:284", "getEvidenceForDownload"): [],
    ("CvManager.java:63", "assertCanDownload"): ["CvAccessRulesTest.java:81#anEmployerWithNoApplicationFromThatWorkerCannot", "StorageDownloadRulesTest.java:75#aPrivateFileIsRefusedWhenItBelongsToSomeoneElse"],
    ("PublicProfileManager.java:81", "getPublicProfile"): ["EmployerSurfaceRulesTest.java:93#aStrangerDoesNotSeeTheCvLink"],
    ("ExperienceManager.java:70", "deleteExperience"): ["WorkerToolsRulesTest.java:135#nobodyCanEditSomeoneElsesExperience (silmeyi deniyor)"],
    ("ExperienceManager.java:59", "updateExperience"): [],  # WorkerToolsRulesTest:135 yalnız silmeyi deniyor
    ("EducationManager.java:57", "updateEducation"): ["ProfileEndpointRulesTest.java:134#nobodyCanEditSomeoneElsesEducation"],
    ("EducationManager.java:68", "deleteEducation"): ["ProfileEndpointRulesTest.java:134#nobodyCanEditSomeoneElsesEducation"],
    ("JobTemplateManager.java:84", "getMyTemplate"): [f"{EC}:238#readingSomeoneElsesJobTemplateLooksExactlyLikeReadingOneThatDoesNotExist"],
    ("JobTemplateManager.java:103", "createFromJob"): ["WorkerToolsRulesTest.java:238#aTemplateCannotBeMadeFromSomeoneElsesJob"],
    ("DeviceManager.java:122", "renameMyDevice"): ["SessionDetailIT.java:300#renamingADeviceShowsUpInTheListAndTheDetailButNotForStrangers"],
    ("SessionManager.java:145", "getMySessionDetail"): ["SessionDetailIT.java:226#someoneElsesSessionLooksExactlyLikeAMissingOne"],
    ("SessionManager.java:145", "revokeSession"): ["MyWorkRulesTest.java:75#aSessionBelongingToSomeoneElseCannotBeRevoked"],
    ("NotificationManager.java:483", "markRead"): ["NotificationFeedRulesTest.java:153#someoneElsesNotificationIsNotMarkedRead"],
    ("AddressSearchManager.java:258", "resolve"): ["AddressSearchManagerTest.java:144#providerProofCannotBeUsedByAnotherUser"],
    ("AddressSearchManager.java:258", "search"): [],
    ("JobApplicationManager.java:365", "getReceivedApplications"): ["ReceivedApplicationsAuthorizationTest.java:91#theScopePredicateGrantsCreatorsOnlyIndividualJobsAndContactsOnlyCompanyJobs", "ReceivedApplicationsAuthorizationTest.java:112#aStrangerWithNoMembershipIsScopedToNoCompanyAtAll", "ReceivedApplicationsIT.java:173#theCreatorSeesEveryApplicationOnHisIndividualAndCompanyJobsAndNothingElse"],
    ("JobApplicationManager.java:307", "respondToApplication"): ["JobApplicationAuthorizationTest.java:114#aUserWhoIsNotAContactCannotRespondToACompanyJobApplication", "JobApplicationAuthorizationTest.java:161#aStrangerLearnsNothingFromTheStateOfAnAnsweredCompanyJobApplication"],
    ("JobApplicationManager.java:315", "respondToApplication"): ["JobApplicationAuthorizationTest.java:73#aStrangerCannotAcceptAnApplicationOnSomeoneElsesJob", "JobApplicationAuthorizationTest.java:95#theApplicantCannotAcceptTheirOwnApplication", "ApplicationDecisionIT.java:255#anOutsiderCannotTellAnAnsweredApplicationFromAMissingOne"],
    ("JobApplicationManager.java:247", "withdrawApplication"): [],
    ("JobApplicationManager.java:100", "applyToJob"): [],
    ("JobApplicationManager.java:110", "applyToJob"): [],
    ("CompanyManager.java:160", "inviteContactToCompany"): ["CompanyInvitationSendingRulesTest.java:157#aNonAdminCannotTellTheCompanyFromAMissingOne"],
    ("CompanyManager.java:267", "updateCompany"): ["CompanyAdministrationRulesTest.java:78#onlyACompanyAdminCanEditTheCompany", "CompanyAddressUpdateIT.java:141#aStaffContactGetsTheSameNotFoundAsBeforeAndTheAddressStays"],
    ("CompanyManager.java:308", "removeContact"): [],
    ("CompanyManager.java:301", "getContacts"): ["CompanyAdministrationRulesTest.java:139#anOutsiderCannotReadTheContactList"],
    ("CompanyManager.java:258", "getCompanyDetail"): ["CompanyAdministrationRulesTest.java:193#onlyTheCompanysOwnPeopleReadWhyItWasRejected"],
    ("JobManager.java:402", "getJobsOfCompany"): ["CompanyJobListingIT.java:114#aUserWhoIsNotAContactOfTheCompanyIsForbidden"],
    ("CompanyContactInvitationManager.java:217", "getCompanyContactInvitationsByCompanyIdDto"): ["CompanyContactInvitationAccessIT.java:92#aStaffContactCannotTellTheInvitationListFromAMissingCompany", "CompanyContactInvitationRulesTest.java:94#anOutsiderCannotListTheInvitations"],
    ("CompanyContactInvitationManager.java:127", "answerToInvitation"): ["CompanyContactInvitationAccessIT.java:142#answeringSomeoneElsesInvitationLooksExactlyLikeAMissingOne"],
    ("CompanyContactInvitationManager.java:198", "getInvitationById"): ["CompanyContactInvitationAccessIT.java:124#aStrangerCannotTellTheInvitationFromAMissingOne", "CompanyContactInvitationRulesTest.java:123#aStrangerHoldingTheIdCannotReadTheInvitation"],
    ("core/helpers/JobAddressVisibility.java:40", "!GET /jobs/{jobId}"): ["JobAddressVisibilityTest.java:53#anUnrelatedViewerReceivesOnlyAnApproximateAddress", "JobAddressVisibilityTest.java:96#anIndividualJobCannotRevealItsExactAddressToEveryone", "JobAddressVisibilityIT.java:163#anUnrelatedUserReceivesOnlyTheApproximateAddress"],
    ("JobManager.java:131", "createIndividualJob"): [],
    ("GalleryManager.java:121", "!DELETE /companies/{companyId}/images/{imageId}"): ["GalleryRulesTest.java:164#removingAnImageThatIsNotInTheGalleryIsNotFound"],
    ("GalleryManager.java:209", "!DELETE /jobs/{jobId}/images/{imageId}"): [],
    ("CompanyManager.java:450", "removeCompanyImage"): [],
    ("MessagingManager.java:494", "!POST /reports"): ["MessageReportIT.java:121#aNonParticipantCannotReportAnExistingMessage", "MessageReportIT.java:337#membershipInAnotherConversationDoesNotAuthorizeTheMessage"],
    ("TransportManager.java:549", "issueWorkerQr"): ["TransportBoardingIT.java:460#theEmployerScansTheWorkerQrOfEitherDirection (işveren GET /transport/qr → 404)"],
    ("TransportManager.java:1239", "issueWorkerQr"): [],
    ("TransportManager.java:1239", "getAssignmentTransport"): ["TransportPlanIT.java:339#aFullShuttleRefusesTheNextSeatAndAForeignShuttleIsConcealed (yabancı GET /transport → 404)"],
    ("TransportManager.java:1221", "selectSeat"): [],
    ("TransportManager.java:1249", "issueShuttleQr"): ["TransportBoardingIT.java:508#aStaleTokenIs400AndOnlyJobPartiesLearnWhyTheyCannotBoard (yabancı GET /shuttles/{id}/qr → 404)"],
    ("TransportManager.java:1249", "rotateShuttleQr"): ["TransportBoardingIT.java:756#aStaticQrIsFlaggedAndRotatingTheKeyRevokesEveryEarlierQr"],
    ("TransportManager.java:1249", "listBoardings"): ["TransportBoardingIT.java:688#theBoardingListCarriesLiveSeatsOnlyWithTheirDecidingEvent"],
    ("TransportManager.java:579", "boardOnShuttle"): ["TransportBoardingIT.java:508#aStaleTokenIs400AndOnlyJobPartiesLearnWhyTheyCannotBoard (yabancı 404, koltuksuz taraf 409)"],
    ("TransportManager.java:584", "boardOnShuttle"): ["TransportBoardingIT.java:385#aSeatMovedOffTheScannedShuttleWhileTheScanWaitsForItsLockIsConcealed"],
    ("TransportManager.java:606", "markSeatBoarding"): ["TransportBoardingIT.java:839#theMarkingRulesAreValidatedAtTheBoundary (yabancı mark → 404)"],
    ("BankAccountManager.java:100", "selectMyPrimaryAccount"): [],
    ("BankAccountManager.java:112", "deleteMyAccount"): ["BankAccountIT.java:147#deletingAnotherUsersAccountLooksExactlyLikeAMissingAccount"],
}

# Tekilleştirme: (nesne, alt eylem) -> karar eylemi. Aynı nesnede AYNI kuralla
# karar verilen alt eylemler tek karar eylemine iner; kural farklıysa ayrı kalır.
NORMALIZE = {
    ("job_assignment", "read_payments"): "read", ("job_assignment", "read_cancellation"): "read",
    ("job_assignment", "read_work_sessions"): "read", ("job_assignment", "read_disputes"): "read",
    ("job_assignment", "read_transport"): "read",
    ("job_assignment", "preview_cancellation"): "cancel",
    ("job_assignment", "check_in"): "record_work", ("job_assignment", "check_out"): "record_work",
    ("job_assignment", "list_mine"): "list",
    ("job", "read_gallery"): "read",
    ("job", "manage_gallery"): "edit", ("job", "write_transport_plan"): "edit",
    ("job", "read_transport_plan"): "read_private", ("job", "list_assignments"): "read_private",
    ("company", "read_gallery"): "read",
    ("company", "read_approval_note"): "read_member_view", ("company", "list_contacts"): "read_member_view",
    ("company", "list_jobs"): "read_member_view",
    ("company", "update"): "manage", ("company", "manage_logo"): "manage", ("company", "manage_gallery"): "manage",
    ("company", "invite_contact"): "manage", ("company", "remove_contact"): "manage",
    ("company", "list_invitations"): "manage",
    ("job_application", "list_received"): "list",
    ("conversation", "mark_read"): "read", ("conversation", "typing"): "post", ("conversation", "list_mine"): "list",
    ("dispute", "read_evidence"): "read", ("dispute", "download_evidence"): "read",
    ("shuttle", "issue_qr"): "manage", ("shuttle", "rotate_qr"): "manage", ("shuttle", "list_boardings"): "manage",
    ("user", "read_public_profile"): "read_public", ("user", "read_employer_stats"): "read_public",
    ("user", "read_experiences"): "read_public", ("user", "read_availability"): "read_public",
    ("user", "read_badges"): "read_public", ("user", "read_reviews"): "read_public",
    ("user", "read_rating_summary"): "read_public",
    ("experience", "update"): "write", ("experience", "delete"): "write",
    ("education", "update"): "write", ("education", "delete"): "write",
    ("job_alert", "update"): "write", ("job_alert", "delete"): "write",
    ("bank_account", "select_primary"): "write", ("bank_account", "delete"): "write",
    ("bank_account", "start_verification"): "write",
    ("job_template", "delete"): "write",
    ("device", "rename"): "write",
    ("availability_exception", "delete"): "write",
    ("session", "revoke"): "write",
}

REFUSAL_NAME = re.compile(r'(?i)stranger|outsider|cannot|conceal|someone|foreign|nobody|another|notapart|nonadmin|notthe|looksexactly|tell|refus|forbidden|only|missing|notfound|isnot|unknown')


def norm_segments(path):
    return [s for s in path.split("/") if s]


def path_match(ep_path, test_path):
    a, b = norm_segments(ep_path), norm_segments(test_path.split("?")[0])
    if len(a) != len(b):
        return False
    for x, y in zip(a, b):
        if x.startswith("{") or y.startswith("{"):
            continue
        if x != y:
            return False
    return True


def expected_statuses(err):
    if err.startswith("404"):
        return {"isNotFound", "is404"}
    if err.startswith("403"):
        return {"isForbidden", "is403"}
    if err.startswith("400"):
        return {"isBadRequest", "is400"}
    return set()


def find_tests(tests, eps, cls, roots, err):
    want = expected_statuses(err)
    hits = []
    for t in tests:
        reqs = t["requests"]
        st = set(t["statuses"])
        for ep in eps:
            if not ep or ep.startswith("STOMP"):
                continue
            m, p = ep.split(" ", 1)
            if any(r.split(" ", 1)[0] == m and path_match(p, r.split(" ", 1)[1]) for r in reqs) and (st & want):
                hits.append(t)
                break
    # birim testleri: manager'ın kök metodu çağrılıp istisna bekleniyor
    exc = {"404": "ResourceNotFoundException", "403": "ForbiddenException", "400": "ValidationException"}.get(err[:3])
    if exc:
        for t in tests:
            if t in hits:
                continue
            if exc in t["exceptions"] and any(c.split(".")[-1] in roots for c in t["calls"]) \
                    and (cls.lower() in t["file"].lower() or any(c.split(".")[0].lower().startswith(cls[:-7].lower()) for c in t["calls"])):
                hits.append(t)
    return hits


def main():
    m2e = json.load(open(EPS))["method_to_endpoints"]
    tests = json.load(open(TESTS))
    rows = []
    for idx, (site, guard, obj, action, conceal, err, roots, note) in enumerate(ROWS, start=1):
        path, line = site.rsplit(":", 1)
        cls = os.path.basename(path)[:-5]
        eps = []
        method_roots = []
        for r in roots:
            if r.startswith("!"):
                eps.append(r[1:])
            else:
                method_roots.append(r)
        if method_roots:
            res = resolve.resolve(site)
            # resolve: guard'ın kök kümesi; satırın kökü bu kümede olmalı (doğrulama)
            for r in method_roots:
                if res["method"] and r not in res["roots"] and r != res["method"]:
                    print(f"UYARI {site}: kök {r} çözümlenen kümede yok {res['roots']}", file=sys.stderr)
                for e in m2e.get(f"{cls}.{r}", []):
                    eps.append(e)
        eps = sorted(set(eps))
        if not eps:
            print(f"UYARI {site} {method_roots}: uç bulunamadı", file=sys.stderr)
        key = (site.replace(C, ""), roots[0])
        if key in OVERRIDES:
            tlist = OVERRIDES[key]
            tsrc = "elle" if tlist else "bulunamadı"
        else:
            hits = [h for h in find_tests(tests, eps, cls, method_roots, err) if REFUSAL_NAME.search(h["test"])]
            tlist = [f'{h["file"].split("/")[-1]}:{h["line"]}#{h["test"]}' for h in hits[:3]]
            tsrc = "otomatik (doğrulanmadı)" if tlist else "bulunamadı"
        tlabel = "; ".join(tlist)
        rows.append({
            "id": f"AZ-{idx:03d}",
            "konum": JAVA_PREFIX + site,
            "korunan_metot": f"{cls}." + ",".join(method_roots) if method_roots else "",
            "desen": guard,
            "nesne_tipi": obj,
            "eylem": NORMALIZE.get((obj, action), action),
            "alt_eylem": action,
            "gizleme_404": conceal,
            "hata_kodu": err,
            "java_testi": tlabel,
            "test_eslesmesi": tsrc,
            "uc": " | ".join(eps),
            "not": note,
        })
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, "authz-envanteri.csv"), "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), lineterminator="\n")
        w.writeheader()
        w.writerows(rows)
    print(f"{len(rows)} satır", file=sys.stderr)
    staff(tests)


# Personel tarafı: servis içi dinamik / ek kontroller (uç izninden ayrı)
STAFF_DYNAMIC = [
    ("core/security/StaffPermissionAuthorizationManager.java:47", "@RequiresPermission yoksa fail-closed",
     "-", "403", ["!ANY /admin/**"], "Eşlenmeyen /admin yolu da 403 (404 değil)."),
    ("core/security/StaffPermissionAuthorizationManager.java:82", "kayıtsız personel ya da mustChangePassword → ret",
     "-", "403", ["!ANY /admin/**"], "Parola değişimi zorunluyken yalnız /admin/auth/password açık."),
    ("core/security/StaffPermissionAuthorizationManager.java:71", "köprü: marketplace ROLE_ADMIN (koala.staff.bridge.open)",
     "-", "403 (kapalıyken)", ["!ANY /admin/**"], "Açıkken ROLE_ADMIN her /admin ucuna GRANTED (izin bakılmaz). Go'da kaldırılır (§5.5)."),
    ("webAPI/StaffController.java:48", "kayıtlı personel mi (izin listesi boş)", "-", "403 StaffMessages.RECORD_NOT_FOUND",
     ["!GET /admin/me"], ""),
    (C + "AdminDetailManager.java:210", "gövdeye bağlı dinamik izin: user.read_pii varsa PII'li yanıt + denetim reclassify",
     "USER_READ_PII", "— (maskeli yanıt)", ["!GET /admin/users/{userId}"], "Ret yok; izin yoksa PII alanları maskeli."),
    (C + "DisputeManager.java:144", "sonuca göre para izni: RELEASE_TO_WORKER→payout.release, REFUND_EMPLOYER→payment.refund",
     "PAYOUT_RELEASE | PAYMENT_REFUND", "403 StaffMessages.PERMISSION_DENIED", ["!POST /admin/disputes/{disputeId}/resolve"],
     "Uç izni dispute.resolve; para hareketi ek izin ister (StaffAuditContext.reclassify)."),
    (C + "StaffPayoutManager.java:39", "servis içi ikinci kontrol (savunma derinliği)", "PAYOUT_RELEASE",
     "403 StaffMessages.PERMISSION_DENIED", ["!POST /admin/job-assignments/{assignmentId}/release"], ""),
    (C + "StaffPayoutManager.java:45", "servis içi ikinci kontrol", "PAYMENT_REFUND",
     "403 StaffMessages.PERMISSION_DENIED", ["!POST /admin/job-assignments/{assignmentId}/refund"], ""),
    (C + "StaffPayoutManager.java:51", "servis içi ikinci kontrol", "PAYOUT_RECONCILE",
     "403 StaffMessages.PERMISSION_DENIED", ["!POST /admin/payouts/{payoutId}/reconcile"], ""),
    (C + "StaffPayoutManager.java:58", "servis içi ikinci kontrol", "PAYOUT_RECONCILE",
     "403 StaffMessages.PERMISSION_DENIED", ["!GET /admin/payouts"], ""),
    (C + "StaffPayoutManager.java:68", "servis içi ikinci kontrol", "PAYOUT_RECONCILE",
     "403 StaffMessages.PERMISSION_DENIED", ["!GET /admin/payouts/{payoutId}"], ""),
    (C + "StaffNoteManager.java:155", "yazar VEYA staff.manage (editableNote)", "NOTE_WRITE + (yazar | STAFF_MANAGE)",
     "404 StaffMessages.NOTE_NOT_FOUND (Concealment)", ["!PATCH /admin/notes/{noteId}", "!DELETE /admin/notes/{noteId}"], "Kaynak düzeyi personel kontrolü."),
    (C + "StaffAuthorizationManager.java:364", "verilebilirlik: aktör role.manage + izni tutmalı (requireGrantable)",
     "ROLE_MANAGE + verilen izin", "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!POST /admin/roles/{roleId}/permissions"], ""),
    (C + "StaffAuthorizationManager.java:386", "verilebilirlik (requireGrantable)", "ROLE_MANAGE + izin",
     "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!DELETE /admin/roles/{roleId}/permissions/{permission}"], ""),
    (C + "StaffAuthorizationManager.java:413", "verilebilirlik: kapsanan rolün tüm izinleri (includeRole)", "ROLE_MANAGE + kapanış izinleri",
     "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!POST /admin/roles/{roleId}/includes"], "Döngü kontrolü :409 (409)."),
    (C + "StaffAuthorizationManager.java:440", "verilebilirlik (excludeRole)", "ROLE_MANAGE + kapanış izinleri",
     "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!DELETE /admin/roles/{roleId}/includes/{includedRoleId}"], ""),
    (C + "StaffAccountManager.java:316", "aktör verilen rolün tüm izinlerine ulaşmalı", "STAFF_MANAGE + rol izinleri",
     "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!POST /admin/staff/{staffId}/roles/{roleCode}"], "Görev ayrılığı ayrıca (StaffSeparationOfDuties)."),
    (C + "StaffAccountManager.java:341", "aktör geri alınan rolün tüm izinlerine ulaşmalı", "STAFF_MANAGE + rol izinleri",
     "403 StaffMessages.PERMISSION_NOT_GRANTABLE", ["!DELETE /admin/staff/{staffId}/roles/{roleCode}"], "Kendini kilitleme 409 (:439)."),
]


def staff(tests):
    eps = json.load(open(EPS))["endpoints"]
    rows = []
    n = 0
    for e in eps:
        if not e["path"].startswith("/admin"):
            continue
        n += 1
        req = e.get("requires")
        if req is None:
            perms, act, kind = "", "", "kimlik doğrulama ucu (izin yok)"
        else:
            perms = " + ".join(req["permissions"]) or "(boş: kayıtlı personel yeter)"
            act = req["action"]
            kind = "uç izni (@RequiresPermission)"
        forb = []
        for t in tests:
            if any(r.split(" ", 1)[0] == e["http"] and path_match(e["path"], r.split(" ", 1)[1]) for r in t["requests"]) \
                    and {"isForbidden", "is403"} & set(t["statuses"]):
                forb.append(t)
        ok = []
        for t in tests:
            if any(r.split(" ", 1)[0] == e["http"] and path_match(e["path"], r.split(" ", 1)[1]) for r in t["requests"]) \
                    and {"isOk", "isCreated", "isNoContent", "isFound"} & set(t["statuses"]):
                ok.append(t)
        lab = lambda ts: "; ".join(f'{h["file"].split("/")[-1]}:{h["line"]}#{h["test"]}' for h in ts[:2])
        rows.append({
            "id": f"ST-{n:03d}",
            "tur": kind,
            "konum": f"{JAVA_PREFIX}webAPI/{e['controller']}:{e['line']}",
            "uc": f"{e['http']} {e['path']}",
            "izin": perms,
            "denetim_eylemi": act,
            "degistirir": "evet" if e["http"] != "GET" else "hayır",
            "ret_kodu": "403" if req is not None else "401",
            "java_testi_ret": lab(forb),
            "java_testi_izinli": lab(ok),
            "not": "",
        })
    for site, guard, perm, err, roots, note in STAFF_DYNAMIC:
        n += 1
        rows.append({
            "id": f"ST-{n:03d}",
            "tur": "servis içi dinamik kontrol",
            "konum": JAVA_PREFIX + site,
            "uc": " | ".join(r[1:] for r in roots),
            "izin": perm,
            "denetim_eylemi": guard,
            "degistirir": "",
            "ret_kodu": err,
            "java_testi_ret": "",
            "java_testi_izinli": "",
            "not": note,
        })
    with open(os.path.join(OUT, "authz-personel-izinleri.csv"), "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), lineterminator="\n")
        w.writeheader()
        w.writerows(rows)
    print(f"personel: {len(rows)} satır", file=sys.stderr)


if __name__ == "__main__":
    resolve.M2E = json.load(open(EPS))["method_to_endpoints"]
    resolve.JAVA = os.path.join(ROOT, JAVA_PREFIX)
    main()
