# ADR-0001: Go'da baştan yazım ve yeni repo

- Durum: Kabul edildi
- Tarih: 04.11.2026
- Karar kaydı: K1, K16, K18
- Kaynak: [plan](../plan/go-gecis-degerlendirme.md) §0 (madde 2–3, 9), §0.0, §2, §7

## Bağlam

Java/Spring Boot backend'i (`is-koala/koala-backend`) 63,6 K satır, 266 uç, 84 entity ve 317 Liquibase changeset'inden oluşuyor (plan §0-1). Repoda Go'ya geçişi gerektiren bir performans ya da bellek sorunu kayıtlı değil. Ölçülmüş tek somut maliyet 84 sn'lik açılış süresi (§0-2).

Geçişin gerekçesi performans değil. Plan şu gerekçeleri sayıyor (§0-2, §2.2):
- tek ikili,
- sky lab ile ortak yığın,
- daha hızlı test döngüsü,
- API'yi ve authz'ı baştan doğru kurma fırsatı.

Proje prodda olmadığı için bu bedel bugün en düşük seviyesinde.

İki model düşünüldü: aynı repoda yeni bir kök ya da yeni repo (§0.1 K18).

## Karar

1. **Baştan yazım (K1).** Go uygulaması hazır olunca Java'nın yerine tek seferde konur. Koşullar kabul edildi: lansman 5–6 aydan uzak ve yazım süresince Java'da yeni özellik yok.
2. **Java dondurulur (K16).**
   - Yeni özellik yok.
   - Karar içeren açık PR'lar (#341, #344, #350, #351, #352) `verify` yeşilse merge edilir.
   - **#352 merge olduktan sonra v0.9.32 yayımlanır** (Yusuf, 04.11 güncellemesi). #349 dahil olduğu için T2 konum sızıntısı riski kalkıyor; pre-prod, Go hazır olana kadar güncel kalır.
   - Bu zinciri (merge'ler ve yayın) üst orkestrasyon oturumu yürütür; bu repo yürütmez.
   - Plan §7-A'daki "v0.9.32 yayımlanmaz" ifadesi K16'nın bu güncellemesiyle geçersizdir.
3. **Yeni repo (K18).** Go kodu `is-koala/koala-api`'de durur. Java repo, geçişten sonra salt-okunur arşiv olur.
4. **Eşitlik hedefi iş kuralı eşitliğidir; API eşitliği hedeflenmez** (§0-3, §6.2). Java testleri senaryo kataloğuna çevrilir. Para kuralları için Java oracle'ından karar tabloları ve Iyzico altın vektörleri kullanılır.

## Sonuçlar

- Temiz git geçmişi ve temiz CI. Java referans olarak yerinde kalır (§0.1 K18).
- Java'da açılan her yeni değişiklik iki kod tabanını ayrıştırır. Bunu önlemek için Java'ya giren her değişikliğe bir senaryo kataloğu satırı zorunludur (§9 R3).
- Takvim tahmini ajan temposuna dayanıyor. Faz 0–1 sonunda gerçekleşen hıza göre yeniden tahmin edilir (§6.4, §9 R11).
