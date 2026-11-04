# ADR-0006: Router ve sözleşmeden kod üretimi: chi + oapi-codegen

- Durum: Kabul edildi (oapi-codegen kısmı Faz 0 spike'ına bağlı)
- Tarih: 04.11.2026
- Karar kaydı: K8, K9
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0.0 K8–K9, §3.3, §9 R4; N05 A5-7

## Bağlam

§0.1'deki ilk öneri stdlib `ServeMux`'tu. core-backend fiber kullanıyor; fiber fasthttp tabanlı ve `net/http` ekosisteminden (otel, standart middleware, `httptest`) kopuk (N05 A5-7).

Koala'nın iki ayrı middleware zinciri var: marketplace ve `/v1/admin`. Bu zincirleri route gruplarıyla ayırmak gerekiyor.

Sözleşme önce yazılacağı için (ADR-0005) handler imzaları spec'ten üretilmeli. Ancak oapi-codegen'in OpenAPI 3.1 desteği doğrulanmadı (§9 R4).

## Karar

1. **Router: `github.com/go-chi/chi/v5` (K8).** Gerekçe:
   - `net/http` ile uyumlu.
   - Go üretim servislerinde en yerleşik router.
   - Route grupları `/v1/admin` zincirini ayırmaya yetiyor.

   §0.1'deki stdlib önerisinin yerine geçer.
2. **Kod üretimi: oapi-codegen, `chi-server` hedefi, strict server (K9).** §3.3'teki `std-http` ifadesi K8 ile geçersizdir. Biçim doğrulaması spec'ten kin-openapi middleware'iyle yapılır. Etiketlerle ikinci bir kopya tutulmaz.
3. **3.1 desteği Faz 0 spike'ında doğrulanır.** Eksikse sırayla şu alternatiflere bakılır: ogen; spec'i 3.0.3 uyumlu alt kümede tutmak; istemci tarafında 3.1 araçları (§9 R4).

## Sonuçlar

- `internal/platform/httpx.NewRouter` bir `*chi.Mux` döner (somut tip; arayüzü tüketen taraf tanımlar). Platform middleware zinciri ve sağlık uçları üzerinde kuruludur. Modüller route'larını buna bağlar. Gövde sınırını yükseltmesi gereken route `r.With(httpx.BodyLimit(n))` kullanır.
- chi'nin özel 405 işleyicisi `Allow` başlığını yazmıyor. `httpx` bunu `Routes.Match` ile hesaplıyor.
- Erişim log'unda `route` olarak chi'nin route şablonu (`/v1/jobs/{id}`) kullanılır. HTTP metriklerinin `http_route` etiketi de aynı değeri alacak (§3.10).
- Spike sonucu bu ADR'ye "çalışıyor" ya da "alternatif seçildi: ADR-NNNN" notuyla işlenir (§6.1 Faz 0 çıkış kriteri).
