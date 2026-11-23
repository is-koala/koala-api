# ADR-0039: Adresler, adres arama ve Google Maps entegrasyonu

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, K19
- Kaynak: [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8, 11 ve açık soru 4, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1, [ADR-0033](0033-email-and-phone-verification-and-sms.md) Karar 4 ve 8, [ADR-0034](0034-idempotency-key-store.md), [ADR-0035](0035-media-upload-handlers-jobs-and-budget.md) Karar 6 ve 8, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 5, 8 ve 10, [ADR-0037](0037-reference-data-sync-and-lists.md); Java `origin/main`: `AddressManager`, `AddressSearchManager`, `GooglePlacesAddressProvider`, `GoogleGeocodingAddressProvider`, `GoogleAddressComponents`, `IstanbulServiceArea`, `DistrictDao#findByNameAndCityAllIgnoreCase`, `UserManager.java:536`, `docs/adr/0007-use-provider-independent-confirmed-addresses.md` (okundu, koşturulmadı)
- Dış kaynaklar: Google Maps Platform belgeleri: Places API (New) [Autocomplete](https://developers.google.com/maps/documentation/places/web-service/place-autocomplete), [Place Details](https://developers.google.com/maps/documentation/places/web-service/place-details), [Session tokens](https://developers.google.com/maps/documentation/places/web-service/place-session-tokens), [Policies](https://developers.google.com/maps/documentation/places/web-service/policies); Geocoding API [reverse geocoding](https://developers.google.com/maps/documentation/geocoding/requests-reverse-geocoding). Google'a gerçek istek atılmadı; biçim belgeden alındı.
- Kod: `internal/address` (`domain.go`, `places.go`, `service.go`, `search.go`, `http.go`, `metrics.go`), `internal/integrations/googlemaps` (`googlemaps.go`, `canned.go`), `internal/platform/fakes/googlemaps.go`, `internal/platform/config` (`Maps`, `ServeMapsErrors`), `internal/users` (`addressLinks`), `internal/app/address.go`, `db/migrations/0049_addresses.sql`, `db/queries/address`

## Bağlam

Faz 2'nin adres PR'ı dokuz operasyonu getirir: `listMyAddresses`, `createMyAddress`, `getMyAddress`, `replaceMyAddress`, `deleteMyAddress`, `startAddressSearchSession`, `listAddressSuggestions`, `resolveAddressCandidate`, `reverseLookupAddress`; ve profilin `addressId` bağını (ADR-0036 Karar 5'in alan düzeyindeki 501'i). ADR-0029 Karar 8 neyin saklanacağını söyledi; bu ADR nasıl saklandığını, sağlayıcının nasıl çağrıldığını ve her çağrının nasıl bir bütçeye bağlandığını yazar.

Deponun baskın kusur sınıfı burada iki biçimde tehdit eder: (a) profil yamasındaki adres bağı #23'ün kurallarını (parola, bütçe, atomiklik) atlatan ikinci bir yol olabilir; (b) Google'a giden ikinci bir yol bütçeyi ve tavanı atlatabilir. Kararların çoğu bunları yapısal olarak imkânsız kılmak içindir.

## Karar

### 1. Google entegrasyonu

**Uçlar.** Sektör standardı Places API (New) ve Geocoding API'dir:

| İş | Uç | Faturalama |
|---|---|---|
| Öneri | `POST https://places.googleapis.com/v1/places:autocomplete` | Oturum belirteciyle: Place Details'le biten oturumdaki öneriler oturuma katlanır; terk edilen oturumda her istek ayrı |
| Çözme | `GET https://places.googleapis.com/v1/places/{placeId}?sessionToken=…` | Place Details Essentials; oturumu bitirir |
| Ters geocoding | `GET https://maps.googleapis.com/maps/api/geocode/json?latlng=…` | Geocoding, istek başına |
| Hizmet alanı sınırı | `GET …/geocode/json?components=country:TR\|administrative_area:<il>` | Geocoding; il başına günde bir (Karar 3) |

Places'in eski (Legacy) uçları seçilmedi: yeni projeye kapalıdır ve oturum belirteçleri ikisi arasında paylaşılamaz. Geocoding'in Places (New) hizalı v4 sürümü belgede bulunamadı; kararlı v3 JSON ucu kullanıldı.

**Alan maskesi (`X-Goog-FieldMask`).** Maske hem faturayı (SKU) hem gelen veriyi sınırlar:

- Autocomplete: `suggestions.placePrediction.placeId,suggestions.placePrediction.structuredFormat.mainText.text,suggestions.placePrediction.structuredFormat.secondaryText.text`. Tam açıklama (`text`), `types` ve `distanceMeters` istenmez.
- Place Details: `id,addressComponents,location`. Üçü de Essentials SKU'sundadır. **`formattedAddress` hiç istenmez**: istenmeyen alan gelmez, gelmeyen alan saklanamaz (MED-008). Java'nın maskesiyle aynıdır.
- Geocoding'de maske yoktur; istemci yanıtı yalnız `place_id`, `types`, `address_components` ve `geometry.bounds/viewport` taşıyan Go tiplerine çözer. `formatted_address` hiçbir tipe düşmez.

Dil `tr`, bölge `tr`, `includedRegionCodes: ["tr"]`, `includeQueryPredictions: false` (yalnız yer önerisi; sorgu önerisi çözülemez).

**İstemci (`internal/integrations/googlemaps`).** Düz `net/http`, SDK yok (İleti Merkezi kalıbı, ADR-0033):

- Zaman aşımı 8 saniye; isteğin kendi süre sınırı (`httpx.RequestTimeout`) daha kısaysa o kazanır.
- Okuma sınırı 256 KiB.
- Yönlendirme reddedilir: bir yönlendirme anahtarı gittiği yere taşırdı.
- **Anahtar:** Places'ta `X-Goog-Api-Key` başlığında. Geocoding belgesi yalnız `key` sorgu parametresini tanımlıyor; bu yüzden istemcinin döndürdüğü hiçbir hata istek URL'sini taşımaz: `*url.Error` nedenine indirgenir, istek kurma hatası URL'siz yeni bir hatadır, mesajda anahtar geçerse `[key]` olur (`TestFailures`, sekiz vaka).
- Her hata `ErrUnavailable`'dır ve sonlu bir nedenler kümesinden biri taşır: `quota` (429 `RESOURCE_EXHAUSTED`, `OVER_QUERY_LIMIT`), `denied` (403 `PERMISSION_DENIED`, `REQUEST_DENIED`), `invalid`, `upstream`, `transport`, `unreadable`. Sonuçsuz başarı (`ZERO_RESULTS`) `ErrNoResult`'tur.

**Sahte (`internal/platform/fakes.GoogleMaps`).** İki servisi tek sunucuda, belgedeki biçimiyle taklit eder: Places hataları Google API hata gövdesiyle (`{"error": {code, message, status}}`), Geocoding hataları HTTP 200 altında `status` ile; Place Details maskesiz istekte 400 `INVALID_ARGUMENT`; `formattedAddress` yalnız maske isterse; ters geocoding önce bir plus code sonucu, sonra sokak adresi döner (gerçek servis gibi), istemcinin ilk sonucu körü körüne almadığı böylece sınanır. Kota, ret, kesinti, bağlantı kopması ve askıda kalma enqueue edilebilir.

**Sürücü ve yapılandırma.**

| Değişken | Anlam |
|---|---|
| `KOALA_GOOGLE_MAPS_API_KEY` | Sunucu anahtarı, `config.Secret`; log'a ve hataya düşmez |
| `KOALA_GOOGLE_MAPS_DRIVER` | `google` (öntanım) ya da `fake` |
| `KOALA_SERVICE_AREA_CITIES` | Hizmet alanının il kimlikleri (Karar 3) |
| `KOALA_ADDRESS_LOOKUPS_PER_USER_DAY`, `_PER_IP_HOUR`, `_PER_DAY` | Arama bütçesi (Karar 7) |

`fake` sürücüsü (`googlemaps.Canned`) üç ilde birkaç sabit yerden yanıt verir ve hiçbir şey çağırmaz: Kadıköy/İstanbul, Çankaya/Ankara ve "Merkez"/Adıyaman. SMS'in `log` sürücüsünün emsalidir: `local`, `test` ve anahtarı olmayan bir staging bütün akışı onunla koşar; **production'da `Load` reddeder ve `ServeMapsErrors` ayrıca reddeder.** `google` sürücüsünde staging ve production anahtarsız açılmaz; `local` ve `test` açılır, arama `502 ADDRESS_PROVIDER_UNAVAILABLE` döner ve açılış log'u nedenini yazar (`TestServeMapsErrors`).

### 2. İl ve ilçe eşleştirmesi: Go'da, indeks yok

Google "Kadıköy", "İstanbul" yazar; referans verisi "KADIKÖY", "İSTANBUL". Java `findByNameAndCityAllIgnoreCase` ile SQL'de `upper()` karşılaştırması yapıyor; varsayılan collation'da `upper('i')` `I`'dır, `İ` değil, yani "Şişli" "ŞIŞLI"ya döner ve "ŞİŞLİ" ile eşleşmez. Bu bir Java hatasıdır; Go tekrarlamaz.

**Seçim: eşleştirme Go'da, ikilinin taşıdığı referans verisi üzerinde (`address.Places`).** `db/referencedata`'nın CSV'leri ikiliye gömülüdür ve `koala migrate up` tabloları onlara eşitler (ADR-0037); 81 il, 973 ilçe, haritalar kablolamada bir kez kurulur. Seçilmeyenler:

- `turkish` collation'lı kolona indeks: her ICU yükseltmesinde `REINDEX` ister (`TestNoIndexUsesTheTurkishCollation`). Bu PR indeks eklemiyor; test ve runbook değişmedi.
- Normalleştirilmiş ad kolonu: tabloya ve eşitlemeye yeni bir türetilmiş alan ve onu tutarlı tutan ikinci bir kural eklerdi.
- `COLLATE "C"` + `lower` ifade indeksi: `lower` Türkçe değildir (I/ı sorunu aynen kalır).

Sorguya hiç gidilmez; bir istekte yük yoktur. İkili veritabanından eskiyse (geri dönüşte veri sürümü geri alınmaz) bilmediği satır eşleşmez; sonuç güvenlidir (aşağıda ilçe seçimi).

**Anahtar:** Türkçe büyük harf (`unicode.TurkishCase`: i→İ, ı→I), sonra ASCII'ye katlama (Ç→C, Ğ→G, İ→I, Ö→O, Ş→S, Ü→U, şapkalılar), boşluklar tekleştirilir. "Kadıköy", "KADIKÖY", "Kadikoy" tek anahtardır. Bir ebeveynde aynı anahtarı paylaşan iki satır belirsizdir ve hiçbir şeye eşleşmez.

**Vakalar:**

- İl: "İstanbul", "Istanbul", "İstanbul İli", "Istanbul Province" → İSTANBUL.
- İlçe sonekleri: "Şişli İlçesi" → ŞİŞLİ.
- **Merkez:** büyükşehir olmayan ilin merkez ilçesi referans verisinde `MERKEZ`'dir; Google onu "Merkez", ilin adıyla ("Adıyaman") ya da "Adıyaman Merkez" diye yazar. Üçü de o ilin `MERKEZ`'ine eşleşir. Büyükşehirde `MERKEZ` yoktur; oradaki "Merkez" eşleşmez.
- **Eski adlar:** küçük bir tarih tablosu (`districtRenames`): Eminönü → Fatih (2008, Java'nın da tek alias'ı), Kazan → Kahramankazan (2016). Yapılandırma değil, tarih; eksik bir ad kullanıcının seçimine düşer.
- Bileşen seçimi Java'nınkidir (`GoogleAddressComponents`): il `administrative_area_level_1` ya da `locality`; ilçe `administrative_area_level_2` ya da `sublocality_level_1`; mahalle `neighborhood`, `administrative_area_level_4`, `sublocality_level_2/3`, yoksa ilçeden farklıysa `sublocality_level_1`. Mahallenin " Mahallesi/Mah./Mh." soneki atılır ("Caferağa", sözleşmenin örneği). Posta kodu yalnız beş rakamsa alınır. Serbest satırlar sözleşmenin sınırlarına kesilir: çözülen değer olduğu gibi `POST /v1/me/addresses`'e verilebilir.

**Eşleşmeyen il ya da ilçe: ilçe kullanıcıya, il 422.**

- İlçe eşleşmezse `ResolvedAddress.districtId` `null` döner; il, satırlar, koordinat ve yer kimliği gelir. İstemci ilçeyi `/v1/reference/cities/{cityId}/districts` listesinden seçtirir. Sektördeki yerleşik akış budur: otomatik tamamlama formu doldurur, kullanıcı eksikleri düzeltir. 422 seçilmedi: Google'ın bir yazım farkı kullanıcıyı çıkmaza sokar ve elle girişe geri götürürdü. Sözleşmede `districtId` nullable oldu (uç hiçbir istemcide yok).
- İl eşleşmez, il hizmet alanında değil ya da ülke alanın ülkesi değilse `422 ADDRESS_OUTSIDE_SERVICE_AREA`. Kod daha önce 400 yazılmıştı; istek biçimce doğrudur ve bir alan kuralına uymaz, bu yüzden 422 (ADR-0007; `KindUnprocessable`, ADR-0034).

### 3. Hizmet alanı: yapılandırılabilir il listesi

Java'da hizmet alanı yalnız İstanbul'dur (`IstanbulServiceArea`): öneriler İstanbul dikdörtgeniyle sınırlı, reverse kutu dışında ret, elle adres ekleme bile İstanbul dışını reddediyor. Ürün kararı (Yusuf, 23.11.2026): "şu anlık İstanbul, ama istediğimiz zaman artırabilmeliyiz."

- `KOALA_SERVICE_AREA_CITIES`: virgülle ayrılmış referans il kimlikleri. Ayarsızsa `config.DefaultServiceAreaCities` (İstanbul'un kimliği). **Kodda İstanbul'a özgü hiçbir şey yoktur**: dikdörtgen, ad ya da test varsayımı. Öntanım bir yapılandırma değeridir.
- Kimlikler kablolamada ikilinin referans verisine karşı denetlenir: bilinmeyen bir il ve iki ülkeye yayılan liste sunucuyu açmaz (fail-closed, `TestServiceArea`).
- **Google sınırlaması listeden türer.** Autocomplete'in `locationRestriction` dikdörtgeni, listedeki her ilin sınırlarının birleşimidir. Sınırlar Google'dan alınır (Geocoding, `components=country:TR|administrative_area:<il adı>`, sonucun `bounds`'u yoksa `viewport`'u), bellekte bir gün tutulur. Gerekçe: 81 ilin dikdörtgenini elle yazmak doğrulanamayan veri olurdu ve "il eklemek tek adım" olmazdı; Google'ın koşulları koordinatın 30 güne kadar önbelleklenmesine izin verir, bir gün içinde kalır. Her sınır sorgusu servisin kendi birimidir (`BOUNDS`) ve küresel tavana sayılır (Karar 7). Sorgu başarısızsa sınırlama düşer (yalnız Türkiye) ve on dakika sonra yeniden denenir; çözmedeki il denetimi her durumda nihai karardır.
- Birleşim dikdörtgeni uzak iki ilde aradaki illeri de kapsar; oradan gelen bir öneri çözmede 422 alır. Kabul edildi: daraltmak öneri metninden il ayıklamayı gerektirirdi, bu da Google'ın biçimine bağlı ikinci bir kural olurdu.
- **Genişletme tek adımdır:** listeye il kimliği eklenir ve servis yeniden başlatılır (runbook "Hizmet alanına il ekleme"). İki illi test bunu bağlar (`TestServiceAreaIsTheConfiguredCities`: İstanbul'la sınırlıyken Ankara'nın yeri önerilmez ve reverse 422; İstanbul+Ankara'da önerilir, Çankaya'ya çözülür, iki sınır sorgusu yapılır).
- **Kullanıcının kendi adresi hizmet alanından bağımsızdır.** Elle adres ekleme sözleşmeye uyar ve il kısıtı yoktur (Java'dan bilinçli ayrılık): bir işçi Ankara'da oturup İstanbul'da çalışabilir. İlan konumunun hizmet alanına bağlanması Faz 3'ün işidir.
- Yeniden başlatmadan değiştirmek (admin paneli ya da DB ayarı) istenirse ayrı bir iştir: sınır önbelleğinin geçersiz kılınmasını ve değişikliğin denetim kaydını getirir.

### 4. Adres modeli ve koordinat

- `addresses`: soft-delete'li, `pii: true` (security barrier), kullanıcıyla birlikte silinir (`cascade`). Sahiplik `(id, user_id)` ile aranır; başkasının adresi hiç olmamış adresin 404'ünü bayt bayt alır (ADR-0014).
- İl ve ilçe referans satırıdır; **ilçe bileşik FK'yle ilinin ilçesidir** (`(district_id, city_id) → districts (id, city_id)`). Başka ilin ilçesi, olmayan ve emekliye ayrılmış satır `404 REFERENCE_ITEM_NOT_FOUND`; adresin zaten taşıdığı emekli satır değiştirmede kalabilir (ADR-0036'nın kuralı).
- Serbest satırlar (mahalle, cadde/sokak, bina no, bina adı, kat, daire, tarif) sözleşmenin sınırlarında; DB `CHECK`'leri aynısını tutar. Kırpılır, boşsa `null`. Posta kodu sözleşmeye `pattern: ^[0-9]{5}$` olarak eklendi (Türkiye'nin posta kodu biçimi; uç hiçbir istemcide yok).
- `provider_place_id` 255'e kadar; kalıcı değil, yenilenebilir bir işaretçidir.
- **Koordinat tam hassasiyetle saklanır** (`double precision`), ikisi birlikte ya da hiç. Görünen hassasiyet tek yerde, `Point.Render(precision)`'da: `EXACT` saklanan değer; `APPROXIMATE` 0,01°'lik ızgaraya yuvarlanır (kuzey-güney ~1,1 km, doğu-batı Türkiye'de ~0,85 km). Izgara, rastgele kaydırma değil: aynı adres hep aynı hücreye düşer, çok sayıda yanıtın ortalaması fazlasını öğretmez. Bugün tek okuyucu sahibidir ve `EXACT` görür; şirket adresi (tam, gizlenebilir), bireysel ilan ve harita (yaklaşık) kendi modülleriyle gelir ve `Render`'a kendi hassasiyetlerini verir ([[job-location-rule]], MED-009 `partial: phase 3`).
- Kullanıcı başına **en çok 10 adres** (`AddressesPerUser`, ürün kararı 23.11.2026); aşımı `409 ADDRESS_LIMIT_REACHED`. Sayım ve ekleme kullanıcının satır kilidi (`FOR UPDATE`) altındadır: eşzamanlı eklemeler sınırı birlikte aşamaz.
- Liste yeniden eskiye (UUIDv7 kimlik), en çok on kayıt olduğu için serviste kesilir; cursor `adr1|yön|kimlik` (base64url), bozuksa `400 CURSOR_INVALID`.

### 5. Birincil adres ve profil bağı

- **Birincil adres `users.address_id`'dir** (sözleşme: profilin `addressId`'si "birincil adresin kimliği"). Tek kolon olduğu için en çok bir birincil vardır, yapısal olarak.
- `users.address_id → addresses` `restrict` kenarıdır (medya bağı kalıbı, ADR-0036 Karar 10). Adresin kullanıcıya ait olduğunu bileşik FK değil kod tutar: soft-delete kataloğu kenar başına tek kolon okur. Kod bunu kullanıcının satır kilidi altında, adresi `(id, user_id)` ile `FOR SHARE` kilitleyerek yapar.
- **Bağın tek yazıcısı adres modülüdür** (`setPrimary`): bağı ve profil senkronu niyetini (`profilesync.Record`, Java'nın `UserManager.java:536` yayını, ADR-0036 Karar 8) aynı transaction'da yazar. Üç yerden çağrılır:
  1. **Profil yaması** (`users.Service.Update`): `addressId` değişirse yama `addressLinks.LinkPrimary`'yi kendi transaction'ında, kullanıcının satır kilidi altında çağırır. Başkasının ya da olmayan adres `404 ADDRESS_NOT_FOUND`; `null` bağı kaldırır, adresi silmez. ADR-0036 Karar 5'in alan düzeyindeki 501'i kalktı, `ADDRESS_NOT_FOUND` `updateMyProfile`'ın listesine döndü.
  2. **İlk adres:** birincil adresi olmayan kullanıcının eklediği adres birincil olur (Java'da kullanıcının tek adresi vardı; sektörde "ilk adres varsayılan olur"). Ürün kararı 23.11.2026.
  3. **Birincil adresin silinmesi:** bağ aynı transaction'da önce `null` olur ve niyet yazılır, sonra adres silinir. Başka bir adres kendiliğinden birincil olmaz (Amazon modeli; ürün kararı 23.11.2026). Sıra ADR-0035 Karar 8'inkidir: önce satır adresten ayrılır, sonra adres silinir; ters sırada platform `SOFT_DELETE_RESTRICTED` döner.
- **Kilit sırası** her yolda aynıdır: kullanıcının satırı, sonra adres. Yama `LockProfile` (FOR UPDATE) → `OwnAddress` (FOR SHARE); silme `LockOwner` (FOR UPDATE) → `LockAddress`; ekleme `LockOwner` → `LockActiveParent(users)` (aynı satır, güncelleme kilidi paylaşım kilidini kapsar). Değiştirme yalnız adresi kilitler. Döngü kurulamaz.
- **Hesap silme:** `addresses.user_id → users` `cascade`, `users.address_id → addresses` `restrict`. Platform önce kullanıcıyı işaretler, kaskadla adresleri, sonra `restrict` kenarını denetler; tüketici aynı silmede işaretlendiği için engellenmez (`TestAccountDeletionTakesItsAddresses`). Karşılıklı FK'ler ikisi de `NOT DEFERRABLE`; fiziksel silme ya da anonimleştirme yapan `privacy` PR'ı önce `users.address_id`'yi `NULL` yapmak zorundadır (medya bağının notu).
- **#23'ün kuralları atlatılamaz:** adres bağı e-posta/telefon değişikliği değildir, parola istemez; ama aynı yamada e-posta varsa parola yine istenir (`dependentRequired`) ve yanlış parola bütün yamayı, adres bağı dahil, reddeder (`TestProfilePrimaryAddressLink`). Bağ yamanın transaction'ındadır; doğrulama başlatılamazsa bağ da yazılmaz (ADR-0036 Karar 9).

### 6. Arama oturumu ve lisans: ne, nerede, ne kadar

Google Maps Platform politikası: yer kimliği önbellekleme kısıtından muaftır ve süresiz saklanabilir; enlem/boylam en çok 30 ardışık gün önbelleklenebilir; diğer içerik (ad, adres, öneri listesi) yalnız koşulların açıkça izin verdiği yerde. Ayrıca son kullanıcının Autocomplete'te seçtiği adres içerik kısıtlarına tabi değildir; istisna öneri listesine uygulanmaz.

| Veri | Nerede | Ne kadar | Dayanak |
|---|---|---|---|
| Önerilerin metni (`primaryText`, `secondaryText`) | Yalnız yanıtta; hiçbir yere yazılmaz | İstek süresi | Öneri listesi istisnanın dışında |
| Önerilerin yer kimlikleri | `address_search_sessions.candidates` | Oturum (15 dk) + temizlik (saatlik) | Yer kimliği muaf |
| Oturum belirteci | Aynı satırda; rastgele UUIDv4 | Oturum | Google içeriği değil |
| Çözülen bileşenler | Yalnız yanıtta; istemci kaydederse adres olur | — | Seçilen adres istisnası |
| `formatted_address`, ham yanıt | Hiçbir yerde; istenmez, decode edilmez | — | ADR-0029 Karar 8 |
| Adresin yer kimliği | `addresses.provider_place_id` | Süresiz, yenilenebilir | Yer kimliği muaf |
| Adresin koordinatı | `addresses.latitude/longitude` | Süresiz | Aşağıya bakın |
| İl sınırları | Süreç belleği | 1 gün | Koordinat ≤ 30 gün |

**Koordinatın süresiz saklanması:** saklanan koordinat iki kaynaktan gelir. Reverse'te çağıranın kendi pinidir (yanıt Google'ın geometrisini değil pini döner); bu birinci taraf veridir. Çözmede Google'ın `location`'ıdır ve kullanıcının Autocomplete'te seçtiği adrese aittir: politika seçilen adresi içerik kısıtlarının dışında tutuyor. Bu yorumun hukukçu teyidi ADR-0029 açık soru 4'le birlikte bekliyor (Açık sorular 1).

**Oturum:** çağırana bağlı, 15 dakika. Bir çözmeyle biter (Google'ın oturumu Place Details'le biter); bitmiş oturum, başkasının oturumu, süresi dolmuş ve hiç olmamış oturum aynı `404 ADDRESS_SEARCH_SESSION_NOT_FOUND`'u bayt bayt alır (MED-010). Aday kimliği `c_<n>`, oturumun yer kimliği dizisindeki indekstir; oturumun hiç önermediği aday oturumun 404'üdür. Sağlayıcı çağrısı başarısız olursa bitiş geri alınır (birim harcanmış kalır). Oturum bellekte değil tablodadır: birden çok örnek ve yeniden başlatma oturumu düşürmez (Java bellekte tutuyordu).

### 7. Maliyet ve kötüye kullanım

**Sayım birimi faturalanan birimdir, istek değil** (ürün kararı 23.11.2026): `SESSION` (oturum açılışı), `DETAILS` (çözme), `REVERSE` (ters geocoding), `BOUNDS` (servisin il sınırı sorgusu). Oturumlu Autocomplete istekleri Place Details'le biten oturumda oturuma katlandığı için oturum tek birimdir; meşru bir yazım akışı bütçeyi tuş başına yemez. Terk edilen oturumun istekleri ayrı ayrı faturalanır; bu yüzden **bir oturum en çok 20 öneri isteği** yapar (`SuggestionRequestsPerSession`), sonrası `429 ADDRESS_LOOKUP_LIMIT_REACHED` ve yeni oturum. Böylece bir oturumun en kötü maliyeti sınırlıdır.

**Defter ve sıra.** Her birim `address_lookups` defterine bir satırdır (M2, bir gün). Rezervasyon tek fonksiyondur (`search.reserve`), sağlayıcı çağrısından önce kısa bir transaction'da, tek bir advisory kilit altında sayar ve yazar; sıra #23'ünkidir:

1. **Kullanıcı:** son 24 saatte en çok `KOALA_ADDRESS_LOOKUPS_PER_USER_DAY` (öntanım **100**). Ret `429` + `Retry-After` (en eski birimin pencereden çıkışı). İlk sırada: kendi bütçesini aşan bir çağıranın istekleri adresin bütçesinden hiçbir şey harcamaz.
2. **İstemci adresi** (IPv4 ya da IPv6 /64, `authn.ClientKey`): son bir saatte en çok `KOALA_ADDRESS_LOOKUPS_PER_IP_HOUR` (öntanım **60**). Ret `429` + `Retry-After`. ADR-0033'ün `verification_address` değeriyle aynı gerekçe: Türkiye'de mobil hatlar CGNAT arkasında; kova çağıranınkinin arkasında olduğu için tek bir çağıran adresi yakamaz, ancak çok sayıda ayrı hesap birlikte tüketebilir.
3. **Küresel tavan:** son 24 saatte en çok `KOALA_ADDRESS_LOOKUPS_PER_DAY` (öntanım **2000**). Her birim, hangi dal olursa olsun, aynı biçimde sayılır (#21'in dersi: tavan iki dalda aynı tüketilir). Ret `502 ADDRESS_PROVIDER_UNAVAILABLE`: istemci açısından sağlayıcı o gün yoktur ve çare aynıdır, adres elle girilir.

Ret hiçbir şey yazmaz. Değerler ölçülmedi (gerçek trafik yok): 100 birim, öneri + çözmeyle bir kullanıcının günde ~50 adres aramasıdır; 2000 birim, Place Details Essentials ve Geocoding fiyatıyla (1000 başına 5 $ düzeyi) günde ~10 $'lık bir tavandır.

**Bellekte HTTP kovası eklenmedi.** Defter kullanıcı ve adres bütçesini birden çok örnekte de tam tutar ve ret hiçbir şey harcamaz; bellek kovası örnek başınadır ve sırayı defterle ikiye bölerdi. Kenarın genel kovaları (`read`/`write`, ADR-0021) her isteğe uygulanmaya devam eder. Öneri isteğinde debounce istemcinindir; sunucuda sözleşmenin `minLength: 3`'ü ve kırpıldıktan sonra üç karakterden kısa sorgunun sağlayıcıya gitmemesi (boş liste, maliyetsiz) vardır.

**"Tek aktör herkesi durdurabilir mi?"** Kullanıcı ve adres bütçesi için hayır: ilk sırada kullanıcının kendi bütçesi durur. Küresel tavan için: tek bir hesap en çok 100, tek bir adres saatte 60 birim harcar; tavanı 20 hesapla ya da dağıtık adreslerle doldurmak mümkündür. **Kabul edilen risk:** o gün meşru kullanıcılar adresi elle girer; tavanın varlık nedeni faturadır. Hesap açmak kayıt bütçelerine takılır (ADR-0032 Karar 6). Azaltma: metrik ve uyarılar, yeniden başlatmayla ayarlanan değerler.

**Numaralandırma.** Arama uçları hesap varlığı sızdırmaz: hiçbiri bir hedef (e-posta, numara) almaz. Oturumun sahipliği her bütçe denetiminden **önce** denetlenir: başkasının oturum kimliği, tavan doluyken de, hiç olmamış oturumun 404'ünü alır; bütçe yanıtı bir oturumu var olmayandan ayırt ettirmez.

**Metrik ve uyarı.**

- `koala_address_lookups_total{unit, outcome}` (`reserved`, `user_limited`, `client_limited`, `capped`), `koala_address_lookup_cap` (tavanın değeri), `koala_address_provider_calls_total{call, outcome}` (`ok`, `no_result`, `not_configured` ve istemcinin nedenleri). Etiket değerleri sonlu kümelerdir.
- `KoalaAddressLookupCapNear`: son 24 saatte rezerve edilen birim tavanın %80'ini geçti.
- `KoalaAddressLookupCapReached`: tavan bir isteği reddetti.
- `KoalaAddressProviderFailing`: sağlayıcı çağrıları kota, ret, kesinti ya da bağlantı hatasıyla düşüyor.

### 8. Hata kodları

| Kod | Durum | Kim döner |
|---|---|---|
| `ADDRESS_NOT_FOUND` | 404 | adres uçları, profil yaması |
| `REFERENCE_ITEM_NOT_FOUND` | 404 | ekleme, değiştirme |
| `ADDRESS_LIMIT_REACHED` (yeni) | 409 | ekleme |
| `SOFT_DELETE_RESTRICTED` | 409 | silme (gelecekteki şirket/ilan tüketicisi) |
| `ADDRESS_SEARCH_SESSION_NOT_FOUND` | 404 | öneri, çözme |
| `ADDRESS_OUTSIDE_SERVICE_AREA` | **422** (400'dü) | çözme, reverse |
| `ADDRESS_LOOKUP_LIMIT_REACHED` (yeni) | 429 | oturum, öneri, çözme, reverse |
| `ADDRESS_PROVIDER_UNAVAILABLE` | 502 | oturum, öneri, çözme, reverse |
| `CURSOR_INVALID` | 400 | liste |

### 9. Veri en aza indirme

KVKK'nın yurt dışı aktarım kararı hukuk konusu ve açık (Açık sorular 2). Bekleyen bir karar yüzünden Google'a gerekenden fazla veri gitmez:

- **Reverse'te yalnız koordinat** gider (`latlng`, dil, bölge, anahtar); kullanıcı kimliği, cihaz, istemci adresi gitmez (`TestReverse` sorgu parametrelerini sayar).
- **Oturum belirteci rastgele bir UUIDv4'tür** ve kullanıcıya bağlanmaz; oturumun kimliğinden de ayrıdır.
- Autocomplete gövdesi yalnız girdi, belirteç, dil/bölge ve dikdörtgen taşır (`TestAutocomplete` gövdenin anahtarlarını sayar). Çağıranın isteğinin hiçbir başlığı Google'a iletilmez.
- **Log'a sorgu metni, yer kimliği, koordinat ve adres yazılmaz;** sağlayıcı hatasında yalnız çağrı adı ve nedeni yazılır.

## "Silinirse kırmızı olur mu?"

| Kural | Silinirse | Test |
|---|---|---|
| Kullanıcı bütçesi | Kullanıcı 3. oturumunu açar | `TestLookupBudgetOrder` |
| Sıra: kullanıcı → adres | Kullanıcının reddedilen istekleri adresin birimlerini yer, ikinci kullanıcı 429 alır | `TestLookupBudgetOrder` (u2'nin iki oturumu) |
| İstemci adresi bütçesi | Aynı adresteki üçüncü kullanıcı geçer | `TestLookupBudgetOrder` (u3, nat) |
| Küresel tavan, her dalda | Oturum ya da reverse tavanı aşar | `TestLookupBudgetOrder` (oturum ve reverse 502, defter 6) |
| Sağlayıcıya bütçesiz yol yok | — | Yapısal: `metered`'in her metodu bir `spent` ister; `spent`'i yalnız `reserve` ve açık oturum kurar. Bu bağ bir testle değil derleyiciyle durur: `spent`'i yok saymak için imzayı değiştirmek gerekir |
| Oturum başına öneri sınırı | 21. istek geçer | `TestSessionSuggestionBound` |
| Kısa sorgu sağlayıcıya gitmez | Kırpılmış iki harf öneri alır | `TestAddressSearchOverHTTP` |
| Oturum sahipliği (MED-010) | Başkası oturumu kullanır | `TestAddressSearchOverHTTP` |
| Çözme oturumu bitirir | Aynı oturumda öneri sürer | `TestAddressSearchOverHTTP` |
| `formattedAddress` istenmez (MED-008) | Maskeye eklenirse | `TestDetails` (maske denetimi) |
| Yer kimliği dışında öneri saklanmaz | `candidates` metin taşır | `TestAddressSearchOverHTTP` (`{canned-moda-caddesi-12}`) |
| Koordinat tam saklanır (MED-009) | Yuvarlanarak yazılır | `TestAddressesOverHTTP` (DB'de 9 basamak), `TestRender` |
| İlk adres birincil | Profil `null` kalır | `TestAddressesOverHTTP` |
| Birincil silinince bağ ve niyet | Silme 409 alır ya da niyet yazılmaz | `TestAddressesOverHTTP` (`updated_at`) |
| Bağın değişikliği niyet yazar | Niyet yazılmaz | `TestProfilePrimaryAddressLink` |
| Başkasının adresine bağ | Bağ kurulur | `TestProfilePrimaryAddressLink` |
| Yanlış parola bağı da geri alır | Bağ yazılır | `TestProfilePrimaryAddressLink` |
| Adres sınırı (10) | 11. eklenir | `TestAddressRulesOverHTTP` |
| Sınır eşzamanlılıkta (kullanıcının satır kilidi) | Yarışan eklemelerden birden çoğu geçer | `TestAddressLimitHoldsUnderConcurrency` |
| Tavan eşzamanlılıkta (`LockLookups`) | Yarışan başlatmalar tavanı aşar | `TestLookupCapHoldsUnderConcurrency` |
| İlçe ilinin ilçesi | Çankaya İstanbul'a eklenir | `TestAddressRulesOverHTTP` |
| Hizmet alanı listeden | Ankara İstanbul alanında çözülür | `TestServiceAreaIsTheConfiguredCities`, `TestResolvedServiceArea` |
| Sınırlama listeden türer, çok il | Ankara iki illi alanda önerilmez | `TestServiceAreaIsTheConfiguredCities` |
| Bilinmeyen il açılışı durdurur | Sunucu açılır | `TestServiceArea` |
| Merkez eşleşmesi | Adıyaman'ın Merkez'i `null` | `TestCentralDistrictMatches`, `TestPlacesMatchTheProvidersNames` |
| Hesap silmeyi `restrict` engellemez | Hesap silinemez | `TestAccountDeletionTakesItsAddresses` |
| Anahtar hataya düşmez | Hata URL taşır | `TestFailures` |
| Production'da sahte sürücü | Açılır | `TestLoadRejectsBadMapsValues`, `TestServeMapsErrors` |

**Denenmeyenler:** profil yaması ile adres silmenin eşzamanlı yarışı (kilit sırası kod incelemesiyle); `Restore` bu kenarlarla; eşzamanlı rezervasyonda kullanıcı ve adres bütçesi (tavanla aynı kilit, yalnız tavan eşzamanlı sınandı). Sınır önbelleğinin 24 saatlik süresi ve 10 dakikalık yeniden deneme saatle sınanmadı; `fake` sürücüsü sınır sorgusunun başarısızlığını üretemez. CI'da koşmadan önce hiçbir test yerelde koşturulmadı (yerelde ağır iş yok).

## Java paritesi

| Konu | Java | Go | Not |
|---|---|---|---|
| Adres sayısı | kullanıcı başına tek (`PUT /users/me/address`) | liste, en çok 10 | ADR-0029 kapsam defteri |
| Hizmet alanı | İstanbul, kodda sabit | yapılandırılabilir il listesi, öntanım İstanbul | Karar 3 |
| Elle adres il kısıtı | İstanbul dışı ret | yok | Karar 3 |
| Oturum deposu | süreç belleği | tablo | Karar 6 |
| Öneri sayısı | en çok 5 | sağlayıcının verdiği (≤ 5), sözleşme ≤ 10 | — |
| Ad eşleşmesi | `upper()` SQL'de, İ/ı hatalı; ilçe eşleşmezse ret | Türkçe anahtar Go'da; ilçe kullanıcıya | Karar 2 |
| Eminönü | Fatih'e alias | aynı, Kazan da | Karar 2 |
| Reverse | iki çağrı (bina, sonra alan) | tek çağrı, en ayrıntılı sonuç | maliyet |
| Reverse koordinatı | kullanıcının pini | aynı | — |
| Aday kanıtı (`candidateToken`) | ekleme kanıt ister | yok; sözleşmede yok | ADR-0029'un donmuş sözleşmesi |
| Bütçe | yok | birim defteri, üç sınır | Karar 7 |
| Birincil adres yayını | `UserManager.java:536` | `setPrimary` → `profilesync.Record` | ADR-0036 Karar 8 |

## Sözleşme değişiklikleri

Dokuz operasyonun `x-not-implemented` işareti kalktı, iskele metotları silindi. Uçlar hiçbir istemcide yok.

- `listMyAddresses`: `x-error-codes: [CURSOR_INVALID]`.
- `createMyAddress`: `ADDRESS_LIMIT_REACHED`; `201` `Idempotent-Replayed`; açıklama ilk adres ve hizmet alanı bağımsızlığını yazar.
- `startAddressSearchSession`, `listAddressSuggestions`, `resolveAddressCandidate`, `reverseLookupAddress`: `ADDRESS_LOOKUP_LIMIT_REACHED`; çözme ve reverse'e `ADDRESS_OUTSIDE_SERVICE_AREA` ve `422` yanıtı; açıklamalar bütçeyi, oturumun bitişini ve veri en aza indirmeyi yazar.
- `deleteMyAddress`, `replaceMyAddress`: açıklama.
- `updateMyProfile`: `ADDRESS_NOT_FOUND` listeye döndü; açıklamadaki 501 cümlesi kalktı.
- `ResolvedAddress.districtId`: nullable (Karar 2).
- `AddressInput.postalCode`: `pattern: ^[0-9]{5}$`.
- `UnprocessableContent`: açıklama uca özgü alan kuralını anar.
- `ErrorCode`: `ADDRESS_LIMIT_REACHED` (409), `ADDRESS_LOOKUP_LIMIT_REACHED` (429) eklendi; `ADDRESS_OUTSIDE_SERVICE_AREA` 400'den 422'ye; `ADDRESS_PROVIDER_UNAVAILABLE`'ın düzyazısı tavanı anar.

## Yerine geçtiği

1. **ADR-0036 Karar 5'in `addressId` maddeleri** (alan düzeyinde 501, `ADDRESS_NOT_FOUND`'un listeden çıkması, tetik testinin `addresses` satırı) → Karar 5: bağ canlı, kod listede, tetik testinden `addresses` çıktı.
2. **ADR-0036 Karar 8'in "henüz yazmayanlar"ındaki birincil adres yayını** → Karar 5: `setPrimary`.
3. **ADR-0029 Karar 8'in "Adres arama oturumu … ömrü 15 dakika"** → değişmedi; oturumun çözmeyle bitmesi ve aday kimliğinin biçimi eklendi (Karar 6).
4. **`ADDRESS_OUTSIDE_SERVICE_AREA`'nın 400'ü** (ADR-0029'un donmuş sözleşmesi) → 422 (Karar 2).
5. **`test/scenarios/contract_test.go`'nun `boundAhead`'indeki MED satırları** → alanın son donmuş operasyonu indi; MED P0'ları artık doğrudan bağlı.

ADR-0013, ADR-0014, ADR-0021, ADR-0026, ADR-0029, ADR-0033, ADR-0035 ve ADR-0036'nın kararları başka yerde değişmiyor.

## Sonuçlar

- Bağlanan P0'lar: MED-008, MED-009 (`partial: phase 3`: şirket, ilan ve harita görünümleri), MED-010. P1 MED-013 bağlandı. `media` alanında donmuş operasyon kalmadı.
- Şirket PR'ı şirket adresini bu tabloya bağlar: `addresses`'e şirket sahipliği (ya `company_id` ya ayrı bir bağ tablosu) ve `SOFT_DELETE_RESTRICTED`'in ilk gerçek tüketicisi o PR'dadır. Şirket adresinin görünürlüğü `Render(PrecisionExact)` ya da gizlenmiş haliyle gelir.
- İlan (Faz 3) konumu hizmet alanına bağlar ve haritayı `Render(PrecisionApproximate)` ile çizer.
- Hizmet alanını yeniden başlatmadan değiştirmek ayrı bir iştir (Karar 3).
- İstemciler (web, mobil) önerileri harita olmadan gösterirken Google Maps atfını göstermek zorundadır (politika: "Google Maps" logosu ya da metni). Sunucu bunu sağlayamaz; istemci işine not edildi.
- `privacy` PR'ı hesabı fiziksel olarak silmeden önce `users.address_id`'yi `NULL` yapar (Karar 5).

## Açık sorular

1. **Çözülen koordinatın süresiz saklanması:** politika seçilen adresi içerik kısıtının dışında tutuyor ve koordinatı 30 günle sınırlıyor; seçilen adresin koordinatının istisnaya girdiği yorumu hukukçu teyidi bekliyor (ADR-0029 açık soru 4 ile birlikte). Teyit "girmez" derse çözmeden gelen koordinat ya saklanmaz ya 30 günde silinir; pin koordinatı birinci taraf veridir ve etkilenmez.
2. **KVKK yurt dışı aktarım:** kullanıcının yazdığı metin ve pini Google'a (ABD) gider. Java'nın ADR-0007'si Google kaynaklı verinin kalıcı kullanımını bu karara bağlamıştı. Hukuk konusu; Karar 9 aktarılanı en aza indirir, kararın kendisi bekliyor. Prod yok, gerçek kullanıcı yok.
3. **Değerler ölçülmedi:** 100/gün, 60/saat, 2000/gün ve oturum başına 20 istek. İlk trafikte `koala_address_lookups_total` ile görülür; yeniden başlatmayla ayarlanır.
