# ADR-0022: Realtime düzeltmeleri: cihaz bağı, oturum kontrolü, origin ve kapanış payı

- Durum: Kabul edildi
- Tarih: 05.11.2026
- Karar kaydı: K4 (ayrıntı)
- Kaynak: [ADR-0018](0018-realtime-ticket-and-ws-protocol.md) §2, §4, §6, §8 ve Sonuçlar; [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md); issue #7 (PR #5 doğrulama incelemesinin kalanları)
- Kod: `internal/realtime` (`ticket.go`, `conn.go`, `hub.go`, `http.go`), `internal/platform/httpx` (`server.go`), `api/openapi.yaml`
- Yerine geçtiği maddeler:
  - ADR-0018 §2 "Bilet bağı" ve "Cihaz bağı ve istemci türü": bilet kullanıcıya ve oturuma bağlıdır; cihaz eşitliği güvence değil, tutarlılık kontrolüdür (Karar 1).
  - ADR-0018 Sonuçlar: "Bilet tekrar kullanılamaz, cihaz ve oturuma bağlıdır" cümlesi (Karar 1).
  - ADR-0018 §2: oturum kontrolünün hata davranışı tanımsızdı; `Sessions.Active` sözleşmesi ve ardışık hata sınırı (Karar 2).
  - ADR-0018 §4 "Origin kuralı": isteğin kendi host'u yalnız `https` ile kabul edilir (Karar 3). Aynı tablodaki cihaz adımının önüne router'ın istemci başlığı kapısı girer (ADR-0021).
  - ADR-0018 §8 kapanış: stream'lerin zorla kapatma payı `ShutdownTimeout`'un içindedir (Karar 4).
  - ADR-0011 madde 2 ve plan §4.1-8 "cihaz eşleşmesi yapılır" ifadesi (Plandan sapmalar).

## Bağlam

PR #5'in incelemesi ADR-0018'de ve realtime kodunda dört açık buldu. Kabul edilmiş ADR düzenlenmediği için düzeltmeler burada kayıtlıdır:
1. ADR-0018 bileti "kullanıcı, oturum ve cihaz üçlüsüne bağlı" diye anlatıyor; Sonuçlar da "cihaz ve oturuma bağlıdır" diyor. Oysa cihaz eşitliği yalnız handshake `X-Device-Id` taşıdığında denetleniyor. `Origin` ekleyen bir istemci bu denetimi atlıyor. `Origin` tarayıcı dışında serbestçe yazılabildiği için cihaz eşitliği bir güvence değil, istemcinin beyanıdır.
2. `Sessions.Active`'in sözleşmesi yazılı değildi. Periyodik kontrol hata alınca bağlantıyı açık tutuyordu; sınır yoktu, yani oturum deposu düştüğü sürece fail-open'dı.
3. Aynı host origin kuralı yalnız host'u karşılaştırıyordu; aynı host'taki düz `http` sayfası geçiyordu.
4. `Hub.Shutdown`, `ShutdownTimeout` dolduktan sonra 5 sn (`forceCloseWait`) daha bekleyebiliyordu. Bu süre README'deki `stop_grace_period` hesabında yoktu.

## Karar

### 1. Bilet kullanıcıya ve oturuma bağlıdır; cihaz eşitliği güvence değildir

- **Güvenceler şunlardır:**
  - bilet tek kullanımlıktır (atomik `Take`),
  - 60 saniyelik ömrü vardır,
  - opaktır ve deposunda yalnız SHA-256'sı tutulur,
  - oturum üç kez kontrol edilir: kullanımda, kayıttan (`join`) sonra ve bağlantı açıkken dakikada bir.

  Oturum iptal edilince bağlantı en geç bir kontrol aralığında kapanır.
- **Cihaz bağı oturumdan gelir.** Faz 1'in authn'i her REST isteğinde `X-Device-Id`'yi oturumun cihazıyla eşler. Bilet de bu kimlikli REST çağrısıyla, oturumun cihazına verilir.
- **Handshake'teki cihaz denetimi bir tutarlılık kontrolüdür.** Yerel istemci (`Origin` yok) `X-Device-Id` göndermek zorundadır ve gönderdiği değer biletin cihazına eşit olmalıdır. Bu kontrol yanlış cihazdaki hatalı bir istemciyi yakalar. Bileti çalan birine karşı korumaz: o kişi `Origin` ekleyip başlığı göndermeyebilir.
- **Çalınan bilete karşı koruma** tek kullanımlık olmak, kısa ömür ve URL'de bulunmamaktır (ADR-0018 §1).
- **Router'ın istemci başlığı kapısı (ADR-0021):** `Origin`'li WebSocket upgrade'ini web istemcisi sayar ve cihaz istemez. Yerel handshake'ten ise REST'teki gibi platform, sürüm ve cihaz başlığını ister. Bu durumda handler'daki `REALTIME_DEVICE_REQUIRED` router üzerinden ulaşılamaz olur. Handler'ın kendi denetimi, kapının dışında bağlanan bir handler için ikinci savunma olarak kalır ve testi vardır.

ADR-0018'in "Java'daki 'istemci sahte olay basar' açığı sınıf olarak kapanır" sonucu geçerlidir. Değişen tek şey cihaza bağlılık iddiasının kapsamıdır.

### 2. `Sessions.Active` sözleşmesi ve ardışık hata sınırı

- **`(false, nil)`:** oturum yok, iptal edilmiş, süresi geçmiş, soft-delete edilmiş ya da başka kullanıcının veya cihazın. Bunlar ayrı ayrı bildirilmez.
- **Hata:** yalnız "cevap bilinmiyor" demektir (depo ulaşılamaz). Hiçbir zaman "etkin değil" yerine kullanılmaz.
- **Handshake** hatada fail-closed'dır (500, bilet harcanmış).
- **Açık bağlantı** tek bir hatayı tolere eder; bir DB kesintisi bütün istemcileri aynı anda düşürmemelidir. `MaxSessionCheckFailures` (varsayılan 3) hata art arda gelirse bağlantı 1013 (try again later) ile kapanır. İstemci yeniden bağlanır; yeni bilet etkin bir oturum ister. Başarılı bir kontrol sayacı sıfırlar.
- Bu sayede iptal edilmiş bir oturum, depo kesintisi boyunca en fazla 3 kontrol aralığı (3 dk) açık kalabilir.

### 3. Aynı host origin'i yalnız https'le

- `Origin` isteğin kendi host'una eşitse yalnız `https` şemasıyla kabul edilir.
- İzinli origin listesi zaten `http`'yi reddediyordu (ADR-0018 §4): aktarımda değiştirilebilen bir sayfa yetkili sayılmaz.
- İsteğin kendi şeması TLS'i sonlandıran proxy'nin arkasında bilinmez. `X-Forwarded-Proto`'ya güvenmek yerine `https` doğrudan istenir.
- Bu kural `websocket.Accept`'in kendi denetiminden sıkıdır (o yalnız host'a bakar). Ön kontrol her zaman en az onun kadar sıkı olmalıdır; bilet harcanmadan önce koşan odur.

### 4. Zorla kapatma payı `ShutdownTimeout`'un içindedir

- `httpx.Server` stream'lere `ShutdownTimeout` eksi `StreamForceCloseMargin` (5 sn) süren bir context verir.
- Bir stream'in `Shutdown`'ı, context'i bittikten sonra en geç bu pay içinde döner. Realtime'ın `forceCloseWait`'i bu paydır.
- Böylece toplam kapanış `KOALA_SHUTDOWN_DRAIN_DELAY + KOALA_SHUTDOWN_TIMEOUT` olarak kalır. README'deki formül değişmez ve gerçeği anlatır.
- Bunun bedeli, WebSocket'lerin kapanış el sıkışmasına ayrılan sürenin 5 sn kısalmasıdır (varsayılanla 30 sn). İstemci 1001'i alınca zaten kendisi bağlantıyı bırakır.

### 5. Sözleşmede realtime

- `POST /v1/realtime/tickets` ve `GET /v1/realtime` `api/openapi.yaml`'dadır ve `x-authz-exempt` ile belgeli istisnadır (ADR-0021 Madde 8). Gerekçeleri:
  - bilet çağıranın kendi oturumu içindir,
  - bağlantı nesne seçmez; alıcıya sunucu authz üzerinden karar verir.
- WebSocket ucunun kimliği `realtimeTicket` güvenlik şemasıyla anlatılır (`Sec-WebSocket-Protocol`'de bilet).
- İki uç da elle bağlanır; üretilen strict sunucunun parçası değildir (`exclude-tags: [realtime]`).
- `RealtimeTicket` şeması ile `realtime.TicketResponse`'u `api.TestRealtimeTicketMatchesContract` karşılaştırır.
- Realtime kodları `ErrorCode` listesindedir.

## Plandan sapmalar

- **Plan §4.1-8 ve ADR-0011 madde 2:** "Kimlik ... 60 sn'lik tek kullanımlık biletle sağlanır; cihaz eşleşmesi yapılır." Cihaz eşleşmesi yalnız handshake cihaz beyan ettiğinde yapılır ve bir güvence değildir. Güvence biletin tek kullanımlık ve kısa ömürlü olması ile oturum kontrolüdür; cihaz bağı oturumdan gelir (Karar 1).

## Sonuçlar

- Testler:
  - `TestSessionCheckFailuresCloseTheConnection`: iki hata, sıfırlayan bir başarı, üç hata → 1013,
  - `TestSameHostOriginNeedsHTTPS`,
  - `TestHandlerRequiresDeviceOfNativeClients`,
  - `TestWriterSendsNothingAfterCloseRequest`: kuyruğu dolu, kapanması istenmiş bağlantıya olay yazılmaz,
  - `TestServerStreamsKeepTheForceCloseMargin`,
  - `TestDefaultLimits`'in yeni değerleri.
- Realtime hâlâ `internal/app`'e bağlı değildir; kimlik (`Authenticator`, `Sessions`) Faz 1'in authn PR'ıyla, Postgres `TicketStore` Faz 4'te gelir (ADR-0018 Sonuçlar).
