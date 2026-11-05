# Authz çiftleri: envanter → §5.4 modeli → karar

Kaynak: [`authz-envanteri.csv`](authz-envanteri.csv) (134 satır). Plan: `docs/plan/go-gecis-degerlendirme.md` §5.4 (OpenFGA modeli), §5.6-2 (tekilleştirme); ADR-0014.

## Ürün kararları (05.11.2026)

Sektör standardına göre verildi. Hedef (`expect`) bu kararlarla hesaplanır; Java'nın davranışı matriste ayrıca (`java`) tutulur.

| # | Karar | Etki |
|---|---|---|
| D1 | **Şirket ilanı şirketindir.** Şirket ilanında erişim yalnız şirket üyeliğinden gelir; ilanı açan kişi şirketten ayrılırsa erişimini kaybeder. Bireysel ilanda erişim sahibindedir. | `editor` = `employer` = `individual_owner or member from owner_company`. `creator` yetki vermez. Java'nın başvuru/ödeme yolu (C04, C18, C31, C33) zaten böyle; JobEditors/partyOf yolu eski creator'a izin veriyor, Go'da kapanır. |
| D2 | **Liste ile detay tutarlıdır.** | C12: şirket yetkilisi tek tek görebildiği atamayı listede de görür. C14: aramada ve detayda aynı kural (editor). |
| D3 | **Kapanmış ya da iptal edilmiş ilan herkese görünür kalır** ("süresi doldu" durumuyla) **ama başvuruya kapalıdır.** | `public_viewer` her durumda yazılır (soft-delete'te silinir); başvuru `applicant_pool` ile, o da yalnız OPEN iken. |
| D4 | **Açık uyuşmazlık varken atama, elle de otomatik de tamamlanamaz.** | Durum kuralı (ön süzgeç), relation değil: `testdata/oracle/assignment-transitions.csv` AT-09, AT-14. |
| D5 | **Pazaryeri ROLE_ADMIN baypası kalkar** (görsel silme, CV indirme, `/actuator`). | C47, C48 (CV), C52 (görsel), C53 (`/actuator`). |

Java hataları (oracle'da Java davranışı kalır, Go hedefi ayrı kolonda):
- Geç iptal cezası iptal edilen tarafa gider: işveren iptalinde ceza işçiye geçer, iade 0 ise tahsilatın tamamı ceza sayılır; para emanette kalmaz (`cancellation-fee.csv` `go_beklenen`, `go_ceza_alicisi`; AT-15, AT-15b).
- Uyuşmazlık atama tamamlanmadan çözülürse: işçi lehine emanet işçiye çıkar ve atama "çözümle tamamlandı" kapanır; işveren lehine iade yapılır ve atama iptal olur (AT-20, AT-21; Upwork pratiği).
- Ödeme oturumunda yetki kontrolü durum kontrolünden önce yapılır (C04, AZ-004/005).

## Özet

| Ölçü | Değer |
|---|---|
| Envanter satırı | 134 |
| Tekil `(nesne tipi, karar eylemi)` çifti | **70** |
| §5.4 hedefi aynen ifade ediyor (✓) | 21 |
| §5.4 ifade ediyor ama farklı karar veriyor (≈) | 5 |
| §5.4'te karşılığı yok (✗) | 23 |
| Bilinçli olarak model dışı (dış) | 21 |
| Önerilen DSL'de karşılığı olmayan çift | 0 (model dışı olanlar `go:<kural>`) |
| Matris ([`internal/authz/testdata/matrix.yaml`](../../internal/authz/testdata/matrix.yaml)) | 210 satır; 21'inde Java hedeften farklı. `skip` olmayan her çiftte ≥1 izin ve ≥1 ret; istisna C53 (hedefte izinli pazaryeri öznesi yok). C37 `skip`. |

"Uyum" kolonu §5.4'ün **mevcut** modelini hedefe göre değerlendirir. Önerilen DSL (aşağıda) bütün ReBAC çiftlerini karşılar.

## Tekilleştirme kuralı

- Satırın `alt_eylem`'i uca özgüdür (`read_work_sessions`); `eylem` karar eylemidir (`read`).
- Aynı nesne tipinde aynı kuralla karar verilen alt eylemler tek karar eylemine iner; kural farklıysa çift ayrı kalır.
- Eşleme: `tarama/build_authz.py` → `NORMALIZE`.

## Tablo

Uyum: ✓ aynen · ≈ §5.4 farklı karar verir · ✗ §5.4'te yok · dış = model dışı (bilinçli). Motor: `go + openfga` iki değerlendiricide koşar; `go_only` ön süzgeç/öznitelik/sahiplik/Scope; `skip` koşmaz.

<!-- tablo:başla (pairs.py üretir) -->

| # | Nesne | Eylem | Satırlar | Java kuralı | Hedef (karar) | §5.4 | Uyum | Önerilen relation | Motor | Java hedeften farklı |
|---|---|---|---|---|---|---|---|---|---|---|
| C01 | `job_assignment` | `read` | AZ-001, AZ-006, AZ-009, AZ-012, AZ-014, AZ-018 | işçi ∪ creator ∪ şirket yetkilisi (partyOf) | işçi ∪ editor from job (D1) | job_assignment#can_read | ✓ | `job_assignment#can_read` | go + openfga | eski creator (D1) |
| C02 | `job_assignment` | `respond` | AZ-002 | yalnız işçi | = Java | — | ✗ | `job_assignment#can_respond` | go + openfga | — |
| C03 | `job_assignment` | `complete` | AZ-003 | partyOf == EMPLOYER (creator ∪ yetkili) | editor from job; açık uyuşmazlıkta ön süzgeç reddeder (D4) (D1, D4) | job_assignment#can_complete | ✓ | `job_assignment#can_complete` | go + openfga | eski creator (D1); uyuşmazlık (D4, oracle) |
| C04 | `job_assignment` | `pay` | AZ-004, AZ-005 | BİREYSEL: creator; ŞİRKET: güncel yetkili; durum kontrolü yetkiden önce | editor from job; yetki kontrolü durumdan önce (D1, Java hatası) | job_assignment#can_pay (editor from job) | ≈ | `job_assignment#can_pay` | go + openfga | ret biçimi (403) ve kontrol sırası |
| C05 | `job_assignment` | `cancel` | AZ-007, AZ-008 | taraf (partyOf) | party (D1) | job_assignment#can_cancel | ✓ | `job_assignment#can_cancel` | go + openfga | eski creator (D1) |
| C06 | `job_assignment` | `record_work` | AZ-010, AZ-011 | yalnız işçi (check-in/out) | = Java | job_assignment#can_check_in | ✓ | `job_assignment#can_record_work` | go + openfga | — |
| C07 | `job_assignment` | `open_dispute` | AZ-013 | taraf; emanet ön süzgeç | party (D1) | — | ✗ | `job_assignment#can_open_dispute` | go + openfga | ret biçimi (farklı 404) |
| C08 | `job_assignment` | `review` | AZ-015 | taraf; COMPLETED ön süzgeç | party (D1) | — | ✗ | `job_assignment#can_review` | go + openfga | — |
| C09 | `job_assignment` | `reveal_worker_identity` | AZ-016, AZ-017 | işveren tarafı + HIRED + oran sınırı | editor from job + HIRED ön süzgeç (D1) | — | ✗ | `job_assignment#can_reveal_worker_identity` | go + openfga | eski creator (D1) |
| C10 | `job_assignment` | `select_seat` | AZ-019, AZ-091, AZ-092 | taraf (işveren işçi adına seçebilir) | party (D1) | — | ✗ | `job_assignment#can_select_seat` | go + openfga | — |
| C11 | `job_assignment` | `issue_boarding_qr` | AZ-020, AZ-021 | taraf VE işçi | worker | — | ✗ | `job_assignment#can_board` | go + openfga | — |
| C12 | `job_assignment` | `list` | AZ-119 | işçi → worker_id; işveren → yalnız creator | Scope(can_read): liste tekil okumayla aynı (D2) | Scope(job_assignment#can_read) | ✓ | `Scope(job_assignment#can_read)` | go_only | şirket yetkilisi listede görmüyor (D2) |
| C13 | `job` | `read` | AZ-121, AZ-122 | herkes, her durumda | herkes, her durumda (kapanmış/iptal ilan 'süresi doldu' gösterilir); soft-delete'te yok (D3) | job#can_view (public_viewer yalnız OPEN) | ≈ | `job#can_view` | go + openfga | — |
| C14 | `job` | `read_employer_view` | AZ-041, AZ-042 | detayda editor, aramada yalnız creator | her yerde editor (D2, D1) | job#editor | ✓ | `job#can_edit` | go + openfga | aramada şirket yetkilisi (D2) |
| C15 | `job` | `read_exact_address` | AZ-043 | creator ∪ atanmış işçi ∪ şirket adresi açık | editor ∪ aktif atamadaki işçi ∪ adres açık; şirket ilanında can_manage_jobs üyeleri (adres iş yeri bilgisi) (D1) | — | ✗ | `job#can_read_exact_address` | go + openfga | şirket üyeleri ve eski creator (D1) |
| C16 | `job` | `edit` | AZ-022, AZ-023, AZ-024, AZ-027, AZ-029, AZ-031, AZ-032 | editor (JobEditors) | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job#can_edit | ✓ | `job#can_edit` | go + openfga | eski creator (D1) |
| C17 | `job` | `read_private` | AZ-028, AZ-030 | editor | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job#editor | ✓ | `job#can_read_private` | go + openfga | eski creator (D1) |
| C18 | `job` | `list_applications` | AZ-033, AZ-034 | BİREYSEL: creator (404); ŞİRKET: güncel yetkili (400) | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job_application#can_read (editor from job) | ≈ | `job#can_list_applications` | go + openfga | ret biçimi (400 / farklı 404) |
| C19 | `job` | `cancel` | AZ-025 | editor | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job#can_edit | ✓ | `job#can_edit` | go + openfga | eski creator (D1) |
| C20 | `job` | `close` | AZ-026 | editor | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job#can_edit | ✓ | `job#can_edit` | go + openfga | eski creator (D1) |
| C21 | `job` | `apply` | AZ-035, AZ-036 | editor olmayan herkes; OPEN; hazır olma | applicant_pool but not editor (applicant_pool yalnız OPEN iken) (D3) | — | ✗ | `job#can_apply` | go + openfga | — |
| C22 | `job` | `open_conversation` | AZ-037, AZ-038, AZ-039 | creator ∪ yetkili herhangi bir işçiyle; işçi kendisi için; ödeme ve engelleme ön süzgeç | editor (+ ödeme/engelleme ön süzgeci) (D1) | — | ✗ | `job#can_open_conversation` | go + openfga | eski creator (D1) |
| C23 | `job` | `copy_as_template` | AZ-040 | editor (JobEditors kopyası) | editor (D1): bireysel ilanda sahibi, şirket ilanında güncel şirket üyesi (D1) | job#can_edit | ✓ | `job#can_edit` | go + openfga | eski creator (D1) |
| C24 | `company` | `read` | AZ-123, AZ-124 | kimlikli her kullanıcı; galeri herkese | = Java | — | ✗ | `company#viewer` | go + openfga | — |
| C25 | `company` | `read_member_view` | AZ-057, AZ-058, AZ-060 | üye: onay notu, kişi listesi, şirket ilanları | = Java | company#member | ✓ | `company#can_read_internal` | go + openfga | ret biçimi (404/403/maske) |
| C26 | `company` | `manage` | AZ-048, AZ-049, AZ-050, AZ-051, AZ-052, AZ-053, AZ-054, AZ-055, AZ-056, AZ-061 | yalnız ADMIN | = Java | company#can_manage | ✓ | `company#can_manage` | go + openfga | — |
| C27 | `company` | `create_job` | AZ-059 | üye | = Java | company#can_manage_jobs | ✓ | `company#can_manage_jobs` | go + openfga | ret biçimi (400) |
| C28 | `company` | `tag_talent_pool_entry` | AZ-113 | KONTROL YOK | üye | — | ✗ | `company#can_manage_jobs` | go + openfga | kontrol yok |
| C29 | `company_invitation` | `read` | AZ-063 | davet eden ∪ davet edilen ∪ şirket ADMIN'i | = Java | — | ✗ | `company_invitation#can_read` | go + openfga | — |
| C30 | `company_invitation` | `answer` | AZ-062 | yalnız davet edilen | = Java | — | ✗ | `company_invitation#can_answer` | go + openfga | — |
| C31 | `job_application` | `decide` | AZ-045, AZ-046 | BİREYSEL: creator; ŞİRKET: güncel yetkili | editor from job (D1) | job_application#can_decide (editor from job) | ≈ | `job_application#can_decide` | go + openfga | — |
| C32 | `job_application` | `withdraw` | AZ-047 | yalnız başvuran | = Java | job_application#can_withdraw | ✓ | `job_application#can_withdraw` | go + openfga | — |
| C33 | `job_application` | `list` | AZ-118 | creator → bireysel ilanlar; yetkili → şirket ilanları | Scope(can_read) (D1) | Scope(job_application#can_read) | ≈ | `Scope(job_application#can_read)` | go_only | — |
| C34 | `conversation` | `read` | AZ-064, AZ-065, AZ-066, AZ-067, AZ-069 | katılımcı | = Java | conversation#can_read | ✓ | `conversation#can_read` | go + openfga | — |
| C35 | `conversation` | `post` | AZ-068, AZ-070, AZ-071, AZ-072, AZ-073 | katılımcı + engelleme + ödeme/pencere ön süzgeçleri | = Java | conversation#can_post | ✓ | `conversation#can_post` | go + openfga | — |
| C36 | `conversation` | `subscribe` | AZ-074 | katılımcı (STOMP) | katılımcı (K4 düz WS) | conversation#can_read | ✓ | `conversation#can_read` | go + openfga | — |
| C37 | `conversation` | `publish_to_topic` | AZ-077 | KONTROL YOK (doğrulanmadı) | istemci konu yayını yapamaz (K4 sunucu-itmeli) (K4) | — | dış | `go:protocol_no_client_publish` | skip | kontrol yok |
| C38 | `conversation` | `list` | AZ-120 | katılımcı | Scope(can_read) | Scope(conversation#can_read) | ✓ | `Scope(conversation#can_read)` | go_only | — |
| C39 | `message` | `report` | AZ-080 | mesajın sohbetinde katılımcı; gönderen kendi mesajını raporlayamaz | = Java | — | ✗ | `message#can_report` | go + openfga | — |
| C40 | `dispute` | `read` | AZ-082, AZ-083, AZ-084 | atamanın tarafı | party from assignment (D1) | dispute#can_read | ✓ | `dispute#can_read` | go + openfga | — |
| C41 | `dispute` | `add_evidence` | AZ-081 | atamanın tarafı; RESOLVED ise 409 | party from assignment (D1) | dispute#can_add_evidence | ✓ | `dispute#can_add_evidence` | go + openfga | — |
| C42 | `shuttle` | `manage` | AZ-085, AZ-086, AZ-087 | ilanın editor'ı | editor from job (D1) | — | ✗ | `shuttle#can_manage` | go + openfga | — |
| C43 | `shuttle` | `board` | AZ-088, AZ-089 | koltuk sahibi; koltuksuz taraf 409, yabancı 404 | = Java | — | ✗ | `shuttle#can_board` | go + openfga | — |
| C44 | `shuttle` | `read_details` | AZ-116 | işveren tarafı tümü; işçi yalnız koltuğu olan servis (VIEW_STATUSES) | can_manage ∪ seat_holder (+ atama durumu ön süzgeci) | — | ✗ | `shuttle#can_read_details` | go + openfga | — |
| C45 | `seat_assignment` | `mark_boarding` | AZ-090 | koltuğun ilanının editor'ı | can_manage from shuttle (D1) | — | ✗ | `seat_assignment#can_mark_boarding` | go + openfga | — |
| C46 | `user` | `read_public` | AZ-125, AZ-126, AZ-127, AZ-128, AZ-129, AZ-130, AZ-131 | tasarım gereği açık | = Java | — | ✗ | `user#public_viewer` | go + openfga | — |
| C47 | `user` | `read_cv_url` | AZ-094 | kendisi ∪ başvuru almış işveren (projeksiyon) | private_file#can_download (D5) | — | ✗ | `private_file#can_download` | go + openfga | — |
| C48 | `private_file` | `download` | AZ-093 | sahip ∪ ROLE_ADMIN ∪ başvuru almış işveren | sahip ∪ reviewer (ROLE_ADMIN yok) (D5) | — | ✗ | `private_file#can_download` | go + openfga | ROLE_ADMIN baypası (D5) |
| C49 | `user` | `read_presence` | AZ-078, AZ-079 | REST'te KONTROL YOK | self ∪ contact | — | ✗ | `user#can_see_presence` | go + openfga | kontrol yok |
| C50 | `user` | `watch_presence` | AZ-075 | kendisi ∪ ortak sohbet (STOMP) | self ∪ contact | — | ✗ | `user#can_see_presence` | go + openfga | — |
| C51 | `user` | `create_individual_job` | AZ-044 | öznitelik: isEmployer + kimlik doğrulanmış | = Java | — | dış | `go:attribute_is_employer` | go_only | — |
| C52 | `image` | `delete` | AZ-095 | yükleyen ∪ ROLE_ADMIN | yalnız yükleyen (D5) | owner (Scope) | dış | `go:owner` | go_only | ROLE_ADMIN baypası (D5); ret biçimi (400) |
| C53 | `platform` | `read_actuator` | AZ-117 | pazaryeri ROLE_ADMIN | pazaryeri öznesine kapalı (operasyon uçları dış yüzeyde yok) (D5) | — | dış | `go:no_marketplace_admin` | go_only | ROLE_ADMIN baypası (D5) |
| C54 | `experience` | `write` | AZ-096, AZ-097 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C55 | `education` | `write` | AZ-098, AZ-099 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C56 | `job_alert` | `write` | AZ-100, AZ-101 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C57 | `bank_account` | `write` | AZ-102, AZ-103, AZ-104 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C58 | `device` | `write` | AZ-107 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C59 | `availability_exception` | `write` | AZ-110 | yalnız sahibi | = Java | owner (model dışı) | dış | `go:owner` | go_only | — |
| C60 | `job_template` | `read` | AZ-105 | yalnız sahibi | = Java | owner | dış | `go:owner` | go_only | — |
| C61 | `job_template` | `write` | AZ-106 | yalnız sahibi | = Java | owner | dış | `go:owner` | go_only | — |
| C62 | `session` | `read` | AZ-108 | yalnız sahibi | = Java | owner | dış | `go:owner` | go_only | — |
| C63 | `session` | `write` | AZ-109 | yalnız sahibi | = Java | owner | dış | `go:owner` | go_only | — |
| C64 | `user_verification` | `confirm` | AZ-111 | yalnız sahibi; ret 403 | yalnız sahibi; gizli ret | owner | dış | `go:owner` | go_only | ret biçimi (403) |
| C65 | `notification` | `mark_read` | AZ-112 | yalnız alıcı; başkasınınki sessiz 200 | yalnız alıcı; gizli ret | owner | dış | `go:owner` | go_only | sessiz kabul |
| C66 | `notification` | `mark_opened` | AZ-132 | kimliksiz; bildirim UUID'si | = Java | — | dış | `go:capability_url` | go_only | — |
| C67 | `address_search_session` | `use` | AZ-114, AZ-115 | bellek içi oturumun sahibi; ret 400 | = Java | owner | dış | `go:owner` | go_only | — |
| C68 | `payment_attempt` | `complete_callback` | AZ-133 | kimliksiz; sağlayıcı token'ı + sunucudan sorgu | = Java | — | dış | `go:provider_token` | skip | — |
| C69 | `payment_attempt` | `read_outcome` | AZ-134 | kimliksiz; kayıtlı sonucu okur | = Java | — | dış | `go:provider_token` | skip | — |
| C70 | `realtime_destination` | `subscribe` | AZ-076 | bilinen hedef dışı SUBSCRIBE reddedilir | = Java (K4 protokolünde) (K4) | — | dış | `go:ws_destination` | go_only | — |

<!-- tablo:bitir -->

## §5.4'ün yanlış ifade ettiği çiftler (≈)

| Çift | §5.4 | Hedef | Düzeltme |
|---|---|---|---|
| C04 `job_assignment.pay`, C18 `job.list_applications`, C31 `job_application.decide`, C33 `job_application.list` | `editor from job`; editor creator'ı her zaman içerir | D1: şirket ilanında yalnız güncel üyelik | `editor` D1'e göre yeniden tanımlandı; Java bu yolda zaten D1 gibi. |
| C13 `job.read` | `public_viewer` yalnız OPEN iken yazılır | D3: her durumda görünür, başvuru ayrı | `public_viewer` her durumda; `applicant_pool` yalnız OPEN. |

## Önerilen DSL (D1–D5 uygulanmış)

Alt küme: `[type]`, `X from Y`, `or`, `but not`. `with condition` kullanılmadı; durum koşulları tuple varlığına çevrildi ya da Go ön süzgecinde (§5.2-3).

```
model
  schema 1.1

type user
  relations
    define self: [user]                       # her kullanıcı için user:X#self@user:X
    define contact: [user]                    # sohbet kurulurken iki yönlü yazılır
    define public_viewer: [user:*]            # deneyim, müsaitlik, rozet, yorum, puan
    define can_see_presence: self or contact  # REST ve WS aynı kural (C49, C50)

type staff
type staff_role
  relations
    define includer: [staff_role]
    define assignee: [staff] or assignee from includer

type platform                                 # personel izinleri (25); pazaryeri öznesine relation yok (D5)
  relations
    define user_read_pii: [staff_role#assignee]
    # … §5.4'teki gibi

type company
  relations
    define admin: [user]
    define staff_member: [user]
    define member: admin or staff_member
    define viewer: [user:*]                   # C24
    define can_manage: admin
    define can_manage_jobs: member
    define can_read_internal: member          # onay notu, kişi listesi, şirket ilanları (C25)

type company_invitation                       # C29, C30
  relations
    define company: [company]
    define inviter: [user]
    define invitee: [user]
    define can_read: inviter or invitee or admin from company
    define can_answer: invitee

type job
  relations
    define owner_company: [company]
    define creator: [user]                    # denetim için; yetki vermez (D1)
    define individual_owner: [user]           # yalnız BİREYSEL ilanda yazılır (D1)
    define public_viewer: [user:*]            # her durumda; soft-delete'te silinir (D3)
    define applicant_pool: [user:*]           # yalnız OPEN iken (D3)
    define active_worker: [user]              # adresin göründüğü atama durumlarında
    define public_address_viewer: [user:*]    # şirket adresi herkese açıksa
    define employer: individual_owner or member from owner_company   # D1
    define editor: employer
    define can_view: public_viewer or editor
    define can_edit: editor
    define can_read_private: editor           # ulaşım planı, ilanın atama listesi (C17)
    define can_list_applications: editor      # C18
    define can_apply: applicant_pool but not editor   # C21
    define can_open_conversation: editor      # işçi tarafı ödeme ön süzgeciyle (C22)
    define can_read_exact_address: editor or active_worker or public_address_viewer   # C15

type job_application
  relations
    define job: [job]
    define applicant: [user]
    define can_read: applicant or editor from job
    define can_decide: editor from job
    define can_withdraw: applicant

type job_assignment
  relations
    define job: [job]
    define worker: [user]
    define party: worker or editor from job
    define can_read: party
    define can_respond: worker
    define can_cancel: party
    define can_complete: editor from job      # D4 ön süzgeç
    define can_pay: editor from job           # yetki kontrolü durumdan önce
    define can_record_work: worker            # check-in ve check-out
    define can_open_dispute: party
    define can_review: party                  # COMPLETED ön süzgeç
    define can_reveal_worker_identity: editor from job   # HIRED ön süzgeç + oran sınırı
    define can_select_seat: party
    define can_board: worker

type dispute
  relations
    define assignment: [job_assignment]
    define can_read: party from assignment
    define can_add_evidence: party from assignment

type conversation
  relations
    define participant: [user]
    define can_read: participant
    define can_post: participant              # engelleme ve ödeme ön süzgeç

type message                                  # C39
  relations
    define conversation: [conversation]
    define sender: [user]
    define can_report: participant from conversation but not sender

type shuttle                                  # C42, C43, C44
  relations
    define job: [job]
    define seat_holder: [user]                # koltuk seçiminde yazılır
    define can_manage: editor from job
    define can_board: seat_holder
    define can_read_details: can_manage or seat_holder   # plaka/şoför; atama durumu ön süzgeç

type seat_assignment                          # C45
  relations
    define shuttle: [shuttle]
    define can_mark_boarding: can_manage from shuttle

type private_file                             # CV (C47, C48); tek tip adı
  relations
    define owner: [user]
    define reviewer: [user, company#member]   # başvuru anında yazılır (Java'da kalıcı)
    define can_download: owner or reviewer
```

## Kontrol eksik ya da şüpheli noktalar (envanterden)

| Satır | Öncelik | Bulgu |
|---|---|---|
| AZ-077 | **yüksek** | STOMP SEND `/topic/conversations/{id}`: yalnız oran sınırı; simple broker abonelere iletebilir (sahte olay). **Doğrulanmadı**, testi yok. Go'da K4 sunucu-itmeli protokolle kapanır: istemci konu yayını yapamaz. |
| AZ-078, AZ-079 | yüksek | `GET /users/{userId}/presence`, `POST /presence/lookup` herkesin çevrimiçi/son görülme bilgisini döndürüyor. STOMP'taki kural REST'te yok. |
| AZ-004, AZ-005 | orta | Ödeme oturumunda durum (409) yetkiden önce; yabancı ödeme durumunu öğrenebiliyor. Go: yetki önce (karar). |
| AZ-113 | orta | Yetenek havuzunda `companyId` üyelik kontrolü yok. |
| AZ-093, AZ-095, AZ-117 | orta | Pazaryeri ROLE_ADMIN baypası (CV, görsel, `/actuator`). D5 ile kalkar. |
| AZ-033, AZ-034 | düşük | Aynı uçta şirket dalı 400, bireysel dal 404. |
| AZ-095 | düşük | Var olmayan görsel ile yetkisiz istek farklı 400 mesajı: var olma sızar. |
| AZ-111 | düşük | 403/404 ayrımı var olmayı sızdırıyor (N01 §9.d). |
| AZ-112 | düşük | Başkasının bildirimi sessizce 200. |
| AZ-043 | — | Tam adres şirket ilanında creator dışındaki yetkiliye gösterilmiyor; `CompanyJobListingRulesTest#theSameJobIsExactForTheContactWhoOpenedItAndApproximateForTheOther` bağlıyor. Karar (05.11, D1): şirket ilanının adresi iş yeri bilgisidir, `can_manage_jobs` üyelerinin hepsi görür; Java davranışı matriste `java` alanında. |
