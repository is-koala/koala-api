# ADR-0016: NFC çip doğrulaması (PA/AA) Go'da

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K15, K19 (NFC bağımlılıkları)
- Kaynak: plan §0.0 K15, K19; §3.3 (NFC tabloları); §6.1 Faz 2b; §9 R13–R15; N08 §A

## Bağlam

Java'da NFC çip doğrulaması BouncyCastle ile yapılıyor ve prodda kapalı. Go'da hazır bir karşılığı yok:
- `crypto/x509` explicit EC parametreli sertifikayı ayrıştıramıyor (golang/go#16660).
- Mevcut pkcs7 paketleri explicit EC'yi ve PSS SignerInfo'yu desteklemiyor (§3.3, N08 §A2).

§0.1'de ertelenmesi önerilmişti. Yusuf 04.11'de NFC'nin v1 içinde olmasına karar verdi (K15).

Sektör kalıbı: mobil kartı okur, sunucu PA/AA doğrular (ReadID, Regula; N08 §A3).

## Karar

1. **Saf Go uygulaması, yan servis yok.** `internal/identity/chip` paketi şunlardan oluşur:
   - stdlib `crypto/*` + `encoding/asn1` + `golang.org/x/crypto/cryptobyte`,
   - kendi CMS/SOD ayrıştırıcısı,
   - explicit EC → adlı eğri eşlemesi,
   - elle yazılmış ISO 9796-2 şema 1.
2. **Kapsam ve politika** (§6.1 Faz 2b):
   - TCKK zinciri RSA-PSS köklü.
   - Gömülü 15 CSCA, parmak izleri sabit.
   - "SOD DG15 listeliyorsa AA zorunlu."
   - Brainpool ve RIPEMD-160 fail-closed. Brainpool TCKK yolunda görülmedi.
3. **Bağımlılıklar (K19, §3.3):**
   - `x/crypto/cryptobyte`: onaylandı.
   - `github.com/gmrtd/gmrtd`: yalnız imaja girmeyen ayrı bir test modülünde ve lisans koşuluyla onaylandı. Transitive `osanderson/brainpool` lisanssız; bu çözülmezse gmrtd eklenmez ve oracle olarak yalnız Java altın vektörleri kullanılır.
   - `ProtonMail/go-crypto/brainpool`: gerekmiyor; yabancı belge kapsama alınırsa **ayrıca onay bekler**.
4. **Bilinçli olarak alınmayanlar:**
   - `go.mozilla.org/pkcs7`, `smallstep/pkcs7`, `digitorus/pkcs7`: explicit EC ve RSA-PSS SignerInfo desteklemiyorlar.
   - `ebfe/brainpool`: lisans dosyası yok.
   - `x/crypto/ripemd160`: deprecated; ICAO 9303 ECDSA AA'da yasak.
   - `gmrtd`'nin üretimde kullanımı: cgo ve `x/mobile` bağımlılıkları var.

## Sonuçlar

- Faz 2b, 2–3 kh sürer ve Faz 3 ile paralel yürür.
- Çıkış kriterleri (§6.1):
  - Java PA, AA, `LdsFilesTest` ve challenge senaryoları Go'da yeşil.
  - Altın vektörlerde karar ve hata kodu eşitliği %100.
  - Diferansiyel fuzz'da beklenmeyen uyuşmazlık 0.
  - Kripto PR'larında ikinci göz.
- Riskler (§9):
  - R13: elle yazılmış kripto kodunda hata.
  - R14: gerçek TCKK'nın algoritması beklenenden farklı çıkabilir.
  - R15: NVİ CRL yayımlamadığı için iptal kontrolü yok.
- Reddedilen pkcs7 paketleri ve `ripemd160` depguard ile yasaktır.
- Gerçek TCKK dökümü repoya ve CI'a girmez.
