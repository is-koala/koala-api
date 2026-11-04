# Authz çiftleri: envanter → §5.4 modeli

Kaynak: [`authz-envanteri.csv`](authz-envanteri.csv) (132 satır). Plan: `GO-GECIS-DEGERLENDIRME.md` §5.4 (OpenFGA modeli) ve §5.6-2 (tekilleştirme).

## Özet

| Ölçü | Değer |
|---|---|
| Envanter satırı | 132 |
| Tekil `(nesne tipi, karar eylemi)` çifti | **68** |
| Modelde aynen karşılığı olan (✓) | 19 |
| Modelde var ama Java'dan farklı karar veren (≈) | 8 |
| Modelde karşılığı olmayan (✗) | 23 |
| Bilinçli olarak model dışı (dış: kendi verin, öznitelik, kimliksiz yetenek URL'si) | 18 |
| Matris taslağı ([`test/authz/matrix.yaml`](../../test/authz/matrix.yaml)) | 186 satır, 68 çiftin hepsinde ≥1 izin + ≥1 ret |

Plan 40–60 çift tahmin etmişti. 68 çıktı. Fark iki yerden geliyor: §5.4'te tipi olmayan nesneler (ulaşım, davet, mesaj, CV, kullanıcı profili) ve kimliksiz ya da tasarım gereği açık uçların da satır olarak sayılması.

## Tekilleştirme kuralı

- Satırın `alt_eylem`'i uca özgüdür, örneğin `read_work_sessions`. `eylem` ise karar eylemidir, örneğin `read`.
- Aynı nesne tipinde aynı kuralla karar verilen alt eylemler tek karar eylemine iner. Kuralı aynı olan altı okuma ucu bu yüzden `job_assignment.read` olarak tek çifttir.
- Kural farklıysa çift ayrı kalır, uç benzer olsa bile. Örneğin `job.list_applications` "işveren" kavramını kullanır, `job.read_private` ise "editor" kavramını kullanır. Bu ikisi ayrı çifttir.
- Eşleme betikte durur: `tarama/build_authz.py` → `NORMALIZE`.

## Tablo

Uyum: ✓ aynen · ≈ model farklı karar verir · ✗ modelde yok · dış = model dışı (bilinçli)

| # | Nesne | Eylem | Satırlar | Java kuralı | §5.4 relation | Uyum | Not |
|---|---|---|---|---|---|---|---|
| C01 | `job_assignment` | `read` | AZ-001, AZ-006, AZ-009, AZ-012, AZ-014, AZ-018 | işçi VEYA ilan sahibi VEYA şirket yetkilisi (partyOf) | job_assignment#can_read | ✓ |  |
| C02 | `job_assignment` | `respond` | AZ-002 | yalnız işçi | — | ✗ | Modelde yok. Öneri: `define can_respond: worker`. |
| C03 | `job_assignment` | `complete` | AZ-003 | işveren tarafı (partyOf == EMPLOYER: creator ∪ şirket yetkilisi) | job_assignment#can_complete | ✓ |  |
| C04 | `job_assignment` | `pay` | AZ-004, AZ-005 | BİREYSEL: yalnız creator; ŞİRKET: yalnız güncel yetkili (creator dahil değil) | job_assignment#can_pay | ≈ | Model `editor from job` diyor; editor creator'ı her zaman içerir. Java şirket ilanında şirketten ayrılmış creator'ı reddeder. Ayrıca durum (409) yetkiden önce bakılıyor. |
| C05 | `job_assignment` | `cancel` | AZ-007, AZ-008 | taraf (partyOf); taraf ceza politikasını seçer | job_assignment#can_cancel | ✓ |  |
| C06 | `job_assignment` | `record_work` | AZ-010, AZ-011 | yalnız işçi (check-in ve check-out) | job_assignment#can_check_in | ✓ | check-out da aynı kural; öneri: relation adını `can_record_work` yap. Emanet (HELD_IN_ESCROW) ön süzgeç. |
| C07 | `job_assignment` | `open_dispute` | AZ-013 | taraf; emanet şartı ön süzgeç | — | ✗ | Modelde yok (dispute yalnız var olan uyuşmazlık için). Öneri: `define can_open_dispute: party`. Ret mesajı NOT_A_PARTICIPANT (var olmayandan farklı). |
| C08 | `job_assignment` | `review` | AZ-015 | taraf; COMPLETED ön süzgeç | — | ✗ | Öneri: `define can_review: party`. |
| C09 | `job_assignment` | `reveal_worker_identity` | AZ-016, AZ-017 | işveren tarafı + atama HIRED (ön süzgeç) + saatlik oran sınırı | — | ✗ | Öneri: `define can_reveal_worker_identity: editor from job`; HIRED koşulu prefilter, oran sınırı ayrı. İşçinin kendisi bu uçtan okuyamaz. |
| C10 | `job_assignment` | `select_seat` | AZ-019, AZ-091, AZ-092 | taraf (işveren işçi adına seçebilir); servis/durak aynı planda olmalı | — | ✗ | Öneri: `define can_select_seat: party`. Kapsama (servis ilan planında) ayrı kontrol. |
| C11 | `job_assignment` | `issue_boarding_qr` | AZ-020, AZ-021 | taraf VE işçi (iki kat) | — | ✗ | Öneri: `define can_board: worker`. |
| C12 | `job_assignment` | `list` | AZ-117 | kapsam: işçi → worker_id; işveren → yalnız creator | Scope(job_assignment#can_read) | ≈ | Tekil okuma (can_read) şirket yetkilisine açık ama 'işveren olarak atamalarım' listesi yalnız creator'ı süzüyor. Scope ve Check aynı relation'dan türetilirse liste genişler. |
| C13 | `job` | `read` | AZ-119, AZ-120 | herkes, her durumda (permitAll) | job#can_view | ≈ | Model `public_viewer` tuple'ını yalnız OPEN iken yazar; Java CLOSED/CANCELLED ilanı da kimliksiz gösterir. Ürün kararı gerekir. |
| C14 | `job` | `read_employer_view` | AZ-041, AZ-042 | detayda editor, aramada yalnız creator | job#editor | ≈ | Java'nın iki yolu tutarsız; model tek kural (editor) verir. |
| C15 | `job` | `read_exact_address` | AZ-043 | creator VEYA atanmış işçi (ADDRESS_VISIBLE durumları) VEYA şirket adresi açık | — | ✗ | Modelde yok. Şirket yetkilisi (creator değilse) tam adresi GÖRMEZ: editor'dan dar. Öneri: alan düzeyi kural, `can_read_exact_address: creator or worker from <aktif atama>` + öznitelik (addressPublic). PII/konum maskesi (#349) ile birlikte. |
| C16 | `job` | `edit` | AZ-022, AZ-023, AZ-024, AZ-027, AZ-029, AZ-031, AZ-032 | editor (JobEditors): galeri, ulaşım planı yazımı | job#can_edit | ✓ |  |
| C17 | `job` | `read_private` | AZ-028, AZ-030 | editor: ulaşım planı okuma, ilanın atama listesi | job#editor | ✓ | Öneri: açık relation `can_read_private: editor`. |
| C18 | `job` | `list_applications` | AZ-033, AZ-034 | BİREYSEL: creator (404); ŞİRKET: yalnız güncel yetkili (400) | job_application#can_read (editor from job) | ≈ | Şirket dalında creator dahil değil ve hata 400 (ValidationException). Bireysel dal 404 ama farklı mesaj. |
| C19 | `job` | `cancel` | AZ-025 | editor | job#can_edit | ✓ | Öneri: denetim için ayrı `can_cancel: editor`. |
| C20 | `job` | `close` | AZ-026 | editor | job#can_edit | ✓ |  |
| C21 | `job` | `apply` | AZ-035, AZ-036 | editor OLMAYAN herkes (creator ve şirket yetkilisi başvuramaz) + OPEN + hazır olma | — | ✗ | Negatif ilişki. Öneri: `define can_apply: [user:*] but not editor` (+ OPEN prefilter). Ret 400. |
| C22 | `job` | `open_conversation` | AZ-037, AZ-038, AZ-039 | işveren tarafı (creator ∪ yetkili) herhangi bir işçiyle; işçi yalnız kendisi için; ödeme ön süzgeci ve engelleme | — | ✗ | conversation tipi var ama açma kuralı yok. Öneri: `can_open_conversation: editor` (+ işçi için atama/ödeme prefilter'ı). Yabancı 404 NOT_A_PARTICIPANT. |
| C23 | `job` | `copy_as_template` | AZ-040 | editor (JobEditors'un ayrı kopyası) | job#can_edit | ✓ | Kural kopyası birleştirilmeli. |
| C24 | `company` | `read` | AZ-121, AZ-122 | kimlikli her kullanıcı; galeri herkese | — | ✗ | Öneri: `define viewer: [user:*]`. |
| C25 | `company` | `read_member_view` | AZ-057, AZ-058, AZ-060 | üye (ADMIN\|STAFF): onay notu, kişi listesi, şirket ilanları | company#member | ✓ | Kişi listesi 404, ilan listesi 403, onay notu alan düzeyi: aynı kural, üç farklı ret biçimi. |
| C26 | `company` | `manage` | AZ-048, AZ-049, AZ-050, AZ-051, AZ-052, AZ-053, AZ-054, AZ-055, AZ-056, AZ-061 | yalnız ADMIN: güncelleme, logo, galeri, davet, kişi çıkarma, davet listesi | company#can_manage | ✓ | Ret 404 USER_NOT_ASSOCIATED_WITH_COMPANY: var olmayan şirketten farklı mesaj (ayırt edilebilir). |
| C27 | `company` | `create_job` | AZ-059 | üye | company#can_manage_jobs | ✓ | Ret 400 (ValidationException). |
| C28 | `company` | `tag_talent_pool_entry` | AZ-113 | KONTROL YOK | company#member (önerilen) | ✗ | Java açığı; Go'da `member` şartı önerilir. |
| C29 | `company_invitation` | `read` | AZ-063 | davet eden VEYA davet edilen VEYA şirket ADMIN'i | — | ✗ | Tip yok. Öneri: `type company_invitation { company: [company]; inviter: [user]; invitee: [user]; can_read: inviter or invitee or admin from company; can_answer: invitee }`. |
| C30 | `company_invitation` | `answer` | AZ-062 | yalnız davet edilen | — | ✗ | Bkz. üst satır. |
| C31 | `job_application` | `decide` | AZ-045, AZ-046 | BİREYSEL: creator; ŞİRKET: yalnız güncel yetkili | job_application#can_decide | ≈ | Model `editor from job` (creator dahil). Java şirket ilanında creator'ı dışarıda bırakır (job.pay ve list_applications ile aynı 'işveren' kavramı). |
| C32 | `job_application` | `withdraw` | AZ-047 | yalnız başvuran | job_application#can_withdraw | ✓ |  |
| C33 | `job_application` | `list` | AZ-116 | kapsam: creator → yalnız BİREYSEL ilanları; yetkili → şirket ilanları | Scope(job_application#can_read) | ≈ | Java'nın 'işveren' kavramı (creator yalnız bireysel ilanda) burada da geçerli; model editor'ı kullanırsa liste genişler. |
| C34 | `conversation` | `read` | AZ-064, AZ-065, AZ-066, AZ-067, AZ-069 | katılımcı (okuma, mesajlar, okundu işareti) | conversation#can_read | ✓ |  |
| C35 | `conversation` | `post` | AZ-068, AZ-070, AZ-071, AZ-072, AZ-073 | katılımcı + engelleme + ödeme/pencere ön süzgeçleri (+ STOMP typing) | conversation#can_post | ✓ | Ön süzgeçler (MessagingAvailability, user_blocks) ReBAC dışı; §5.2-3'e uygun. |
| C36 | `conversation` | `subscribe` | AZ-074 | katılımcı (STOMP SUBSCRIBE) | conversation#can_read | ✓ | K4 ile WS protokolü değişir; kural aynı kalır. |
| C37 | `conversation` | `publish_to_topic` | AZ-077 | KONTROL YOK (simple broker /topic SEND) — doğrulanmadı | — | ✗ | K4 sunucu-itmeli modelde istemci yayın yapamaz; Go'da relation gerekmez, protokol reddeder. |
| C38 | `conversation` | `list` | AZ-118 | kapsam: katılımcı | Scope(conversation#can_read) | ✓ |  |
| C39 | `message` | `report` | AZ-080 | mesajın sohbetinde katılımcı (DAO WHERE); gönderen kendi mesajını raporlayamaz | — | ✗ | Öneri: `type message { conversation: [conversation]; sender: [user]; can_report: participant from conversation but not sender }`. |
| C40 | `dispute` | `read` | AZ-082, AZ-083, AZ-084 | atamanın tarafı (kanıt listesi ve indirme); kanıt uyuşmazlığa ait olmalı | dispute#can_read | ✓ |  |
| C41 | `dispute` | `add_evidence` | AZ-081 | atamanın tarafı; RESOLVED ise 409 | dispute#can_add_evidence | ✓ |  |
| C42 | `shuttle` | `manage` | AZ-085, AZ-086, AZ-087 | ilanın editor'ı (QR üret/döndür, biniş listesi) | — | ✗ | Öneri: `type shuttle { job: [job]; can_manage: editor from job; can_board: seat_holder }` (seat_holder tuple'ı koltuk seçiminde yazılır). |
| C43 | `shuttle` | `board` | AZ-088, AZ-089 | bu serviste koltuğu olan işçi; koltuksuz taraf 409, yabancı 404 | — | ✗ | Bkz. üst satır. |
| C44 | `seat_assignment` | `mark_boarding` | AZ-090 | koltuğun ilanının editor'ı | — | ✗ | Öneri: `type seat_assignment { shuttle: [shuttle]; can_mark: can_manage from shuttle }`. |
| C45 | `user` | `read_public` | AZ-123, AZ-124, AZ-125, AZ-126, AZ-127, AZ-128, AZ-129 | tasarım gereği açık: profil, deneyim, müsaitlik, rozet, yorum, puan, işveren istatistiği | — | ✗ | Öneri: `type user { define public_viewer: [user:*] }` (N01 §9.d). Müsaitlik takviminin açık olması ürün kararı. |
| C46 | `user` | `read_cv_url` | AZ-094 | kendisi VEYA ona başvuru almış işveren (projeksiyon) | — | ✗ | private_file.download ile aynı kural, ROLE_ADMIN baypası hariç. |
| C47 | `private_file` | `download` | AZ-093 | sahip VEYA marketplace ROLE_ADMIN VEYA sahibinden başvuru almış işveren (kalıcı) | — | ✗ | Öneri: `type cv { owner: [user]; reviewer: [user, company#member]; can_download: owner or reviewer }`; reviewer tuple'ı başvuru anında yazılır. ROLE_ADMIN baypası Go'da kalkar (personel `user.read_pii`). |
| C48 | `user` | `read_presence` | AZ-078, AZ-079 | REST'te KONTROL YOK | — | ✗ | §5.4'teki bilinen `contact` kalemi; REST ve WS aynı kurala bağlanmalı. |
| C49 | `user` | `watch_presence` | AZ-075 | kendisi VEYA ortak sohbet (STOMP) | — | ✗ | §5.4: türetilmiş `contact` ilişkisi gerekir. |
| C50 | `user` | `create_individual_job` | AZ-044 | öznitelik: isEmployer + kimlik doğrulanmış | — | dış | ReBAC dışı ön süzgeç (öznitelik). |
| C51 | `image` | `delete` | AZ-095 | yükleyen VEYA marketplace ROLE_ADMIN | owner (Scope) | ≈ | ROLE_ADMIN baypası Go'da kalkar; ret 400 ve var olma sızar. |
| C52 | `experience` | `write` | AZ-096, AZ-097 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C53 | `education` | `write` | AZ-098, AZ-099 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C54 | `job_alert` | `write` | AZ-100, AZ-101 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C55 | `bank_account` | `write` | AZ-102, AZ-103, AZ-104 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C56 | `device` | `write` | AZ-107 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C57 | `availability_exception` | `write` | AZ-110 | yalnız sahibi | owner (model dışı, Scope SelfUserID) | dış |  |
| C58 | `job_template` | `read` | AZ-105 | yalnız sahibi | owner | dış |  |
| C59 | `job_template` | `write` | AZ-106 | yalnız sahibi | owner | dış |  |
| C60 | `session` | `read` | AZ-108 | yalnız sahibi | owner | dış |  |
| C61 | `session` | `write` | AZ-109 | yalnız sahibi (iptal) | owner | dış |  |
| C62 | `user_verification` | `confirm` | AZ-111 | yalnız sahibi; ret 403 (var olma sızar) | owner | dış |  |
| C63 | `notification` | `mark_read` | AZ-112 | yalnız alıcı; başkasınınki sessizce yok sayılır (200) | owner | dış |  |
| C64 | `notification` | `mark_opened` | AZ-130 | kimliksiz; bildirim UUID'si yetenek URL'si | — | dış |  |
| C65 | `address_search_session` | `use` | AZ-114, AZ-115 | bellek içi oturumun sahibi; ret 400 | owner | dış |  |
| C66 | `payment_attempt` | `complete_callback` | AZ-131 | kimliksiz; sağlayıcı token'ı + sunucudan sorgu (reconcile) | — | dış | Yetki değil sağlayıcı doğrulaması: test/oracle/reconcile. |
| C67 | `payment_attempt` | `read_outcome` | AZ-132 | kimliksiz; kayıtlı sonucu okur | — | dış |  |
| C68 | `realtime_destination` | `subscribe` | AZ-076 | bilinen hedef dışı her SUBSCRIBE reddedilir | — | dış | Varsayılan ret; K4 ile protokol değişir. |

## Modelin yanlış ifade ettiği çiftler (≈)

| Çift | Java | §5.4 modeli | Sonuç |
|---|---|---|---|
| C04 `job_assignment.pay`, C18 `job.list_applications`, C31 `job_application.decide`, C33 `job_application.list` | Bireysel ilanda yalnız creator; şirket ilanında yalnız şirketin **güncel** yetkilisi. Creator şirketten ayrıldıysa erişimi biter. | `editor from job` = creator ∪ şirket üyesi; creator her zaman içeride | Java'da iki ayrı "işveren" kavramı var. **JobEditors/partyOf** (düzenleme, atama okuma, tamamlama, iptal) creator'ı her zaman içerir. **Başvuru/ödeme yolu** ise creator'ı yalnız bireysel ilanda tanır. Model yalnız ilkini ifade ediyor. |
| C12 `job_assignment.list` | "İşveren olarak atamalarım" yalnız `creator_id` ile süzer | `Scope(can_read)` = party | Tekil okuma şirket yetkilisine açık ama liste kapalı. Scope ile Check aynı relation'dan türetilirse liste genişler. Bu bir ürün kararı. |
| C13 `job.read` | CLOSED/CANCELLED dahil her ilan kimliksiz okunur | `public_viewer` yalnız OPEN iken yazılır | Model daha kısıtlayıcı. Kapanmış ilanın paylaşılan bağlantısı Go'da 404 olur. |
| C14 `job.read_employer_view` | Detayda editor, arama listesinde yalnız creator | `editor` | Java kendi içinde tutarsız. Model tek kural verir. |
| C51 `image.delete` | Yükleyen **ya da marketplace `ROLE_ADMIN`** | sahip (Scope) | `ROLE_ADMIN` baypası personel modeline taşınmamış. Go'da kalkar (§5.5). |

## Modelde karşılığı olmayan çiftler (✗)

- **Atama alt eylemleri:** C02 `respond` (yalnız işçi), C07 `open_dispute` (taraf), C08 `review` (taraf), C09 `reveal_worker_identity` (işveren tarafı), C10 `select_seat` (taraf), C11 `issue_boarding_qr` (yalnız işçi).
- **İlan:** C15 `read_exact_address`. Alan düzeyi bir kural. Şirket yetkilisini dışarıda bırakır, editor'dan dardır. C21 `apply` negatif bir ilişki. C22 `open_conversation`.
- **Şirket:** C24 `read`: kimlikli herkes okur. C28 `tag_talent_pool_entry`: **Java'da kontrol yok**.
- **Tipi olmayan nesneler:**
  - C29, C30 `company_invitation`
  - C39 `message.report`
  - C42, C43 `shuttle`; C44 `seat_assignment`
  - C46, C47 CV dosyası
  - C45 `user.read_public`
- **Presence:** C48 REST'te **kontrol yok**. C49 STOMP'ta "kendisi ya da ortak sohbet" kuralı var. §5.4 bunu zaten "bilinen kalem" olarak not ediyor. REST ile WS'nin aynı kurala bağlanması gerekir.
- **C37 `conversation.publish_to_topic`:** STOMP SEND katılımcı kontrolünden geçmiyor (**doğrulanmadı**). K4 ile istemci yayın yapamayacağından Go'da relation gerekmez. Protokol bu yayını reddeder.

## Model düzeltme önerisi

Aşağıdaki ekler §5.4'ün alt kümesinde kalır (`[type]`, `X from Y`, `or`, `but not`). İşaretli satırlar Java'nın bugünkü davranışını korur. Hangisinin hedef olduğu ürün kararıdır.

```
type job
  relations
    define owner_company: [company]
    define creator: [user]
    define individual_owner: [user]          # YENİ: tuple yalnız employerType = INDIVIDUAL iken yazılır
    define public_viewer: [user:*]
    define editor: creator or can_manage_jobs from owner_company
    define employer: individual_owner or member from owner_company   # YENİ: başvuru/ödeme yolunun "işveren"i (Java C04/C18/C31/C33)
    define can_view: public_viewer or editor
    define can_edit: editor
    define can_apply: [user:*] but not editor                         # YENİ (C21); OPEN + hazır olma prefilter
    define can_list_applications: employer                            # YENİ (C18)

type job_application
  relations
    define job: [job]
    define applicant: [user]
    define can_read: applicant or employer from job                   # DEĞİŞİR: editor → employer
    define can_decide: employer from job                              # DEĞİŞİR (C31)
    define can_withdraw: applicant

type job_assignment
  relations
    define job: [job]
    define worker: [user]
    define party: worker or editor from job
    define can_read: party
    define can_respond: worker                                        # YENİ (C02)
    define can_cancel: party
    define can_complete: editor from job
    define can_pay: employer from job                                 # DEĞİŞİR (C04)
    define can_record_work: worker                                    # ad değişikliği: can_check_in (check-in + check-out)
    define can_open_dispute: party                                    # YENİ (C07); emanet prefilter
    define can_review: party                                          # YENİ (C08); COMPLETED prefilter
    define can_reveal_worker_identity: editor from job                # YENİ (C09); HIRED prefilter + oran sınırı
    define can_select_seat: party                                     # YENİ (C10)
    define can_board: worker                                          # YENİ (C11)

type company
  relations
    define viewer: [user:*]                                           # YENİ (C24)
    # admin, staff_member, member, can_manage, can_manage_jobs aynen

type company_invitation                                               # YENİ (C29, C30)
  relations
    define company: [company]
    define inviter: [user]
    define invitee: [user]
    define can_read: inviter or invitee or admin from company
    define can_answer: invitee

type message                                                          # YENİ (C39)
  relations
    define conversation: [conversation]
    define sender: [user]
    define can_report: participant from conversation but not sender

type shuttle                                                          # YENİ (C42, C43)
  relations
    define job: [job]
    define seat_holder: [user]                                        # koltuk seçiminde yazılır
    define can_manage: editor from job
    define can_board: seat_holder

type seat_assignment                                                  # YENİ (C44)
  relations
    define shuttle: [shuttle]
    define can_mark_boarding: can_manage from shuttle

type cv                                                               # YENİ (C46, C47)
  relations
    define owner: [user]
    define reviewer: [user, company#member]                           # başvuru anında yazılır (Java: kalıcı)
    define can_download: owner or reviewer

type user
  relations
    define public_viewer: [user:*]                                    # YENİ (C45): deneyim, müsaitlik, rozet, yorum
    define contact: [user]                                            # §5.4'teki bilinen kalem (C48, C49)
    define can_see_presence: contact
```

**Not:** `employer` ilişkisi Java'nın bugünkü davranışını korur. Ürün tarafı "ilan sahibi şirketten ayrılsa da kendi açtığı ilanın başvurularını yönetsin" derse `employer` kaldırılır ve yerine `editor` kullanılır. O durumda C04, C18, C31 ve C33 ✓ olur ve Java'dan bilinçli olarak ayrılınır. Karar verilmeden matrisin bu satırları "karar bekliyor" kalır.

## Kontrol eksik ya da şüpheli noktalar (envanterden)

| Satır | Bulgu |
|---|---|
| AZ-078, AZ-079 | `GET /users/{userId}/presence` ve `POST /presence/lookup` herkesin çevrimiçi/son görülme bilgisini döndürüyor. STOMP'taki kural REST'te yok. |
| AZ-077 | STOMP SEND `/topic/conversations/{id}`: yalnız oran sınırı var. Simple broker bunu abonelere iletebilir. **Doğrulanmadı.** Testi yok. |
| AZ-113 | `PUT /users/me/talent-pool/{workerId}`: `companyId` için üyelik kontrolü yok. |
| AZ-004 | `POST /job-assignments/{id}/payment-session`: durum (409) yetkiden (403) önce bakılıyor. Yabancı, atamanın ödeme durumunu öğrenebiliyor. |
| AZ-033, AZ-034 | `GET /jobs/{jobId}/applications`: şirket dalı 400, bireysel dal 404 dönüyor. Aynı uçta iki farklı ret biçimi var. |
| AZ-095 | `DELETE /images/{id}`: var olmayan görsel ile yetkisiz istek farklı 400 mesajları alıyor, yani var olma sızıyor. Ayrıca `ROLE_ADMIN` baypası var. |
| AZ-111 | `POST /verifications/{id}/confirm`: 403 ile 404 ayrımı var olmayı sızdırıyor (N01 §9.d). |
| AZ-112 | `POST /users/me/notifications/{id}/read`: başkasının bildirimi sessizce 200 alıyor. |
| AZ-093 | CV indirme: işçi bir kez başvurduysa, o işverenin bütün güncel yetkilileri CV'yi süresiz indirebiliyor. |
| AZ-043 | Tam adres, şirket ilanında şirket yetkilisine (creator değilse) gösterilmiyor. Bu editor kuralından dar. Kasıtlı görünüyor: `CompanyJobListingRulesTest:177#theSameJobIsExactForTheContactWhoOpenedItAndApproximateForTheOther` bu davranışı bağlıyor. |
