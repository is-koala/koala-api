# ADR-0039: Adresler, adres arama ve Google Maps entegrasyonu

- Durum: Kabul edildi
- Tarih: 23.11.2026
- Karar kaydı: K10, K14, K19
- Kaynak: [ADR-0007](0007-error-model.md), [ADR-0013](0013-soft-delete.md), [ADR-0014](0014-authorization.md), [ADR-0021](0021-http-edge-clients-rate-limit-metrics.md), [ADR-0026](0026-staff-rbac-audit-trail-and-pii-gate.md), [ADR-0027](0027-soft-delete-platform-hardening.md), [ADR-0029](0029-identity-user-company-and-media-contract.md) Karar 8, 11 ve açık soru 4, [ADR-0032](0032-registration-password-reset-mail-and-error-code-binding.md) Karar 1, [ADR-0033](0033-email-and-phone-verification-and-sms.md) Karar 4 ve 8, [ADR-0034](0034-idempotency-key-store.md), [ADR-0035](0035-media-upload-handlers-jobs-and-budget.md) Karar 6 ve 8, [ADR-0036](0036-profile-educations-and-profile-sync-outbox.md) Karar 5, 8 ve 10, [ADR-0037](0037-reference-data-sync-and-lists.md); Java `origin/main`: `AddressManager`, `AddressSearchManager`, `GooglePlacesAddressProvider`, `GoogleGeocodingAddressProvider`, `GoogleAddressComponents`, `IstanbulServiceArea`, `UserManager.java:536`, `docs/adr/0007-use-provider-independent-confirmed-addresses.md` (okundu, koşturulmadı)
- Dış kaynaklar: Google Maps Platform belgeleri: Places API (New) [Autocomplete](https://developers.google.com/maps/documentation/places/web-service/place-autocomplete), [Place Details](https://developers.google.com/maps/documentation/places/web-service/place-details), [Session tokens](https://developers.google.com/maps/documentation/places/web-service/place-session-tokens), [Session pricing](https://developers.google.com/maps/documentation/places/web-service/session-pricing), [Policies](https://developers.google.com/maps/documentation/places/web-service/policies); Geocoding API [reverse geocoding](https://developers.google.com/maps/documentation/geocoding/requests-reverse-geocoding). Google'a gerçek istek atılmadı; biçim belgeden alındı.
- Kod: `internal/address` (`domain.go`, `places.go`, `service.go`, `search.go`, `http.go`, `metrics.go`), `internal/integrations/googlemaps` (`googlemaps.go`, `canned.go`), `internal/platform/fakes/googlemaps.go`, `internal/platform/config` (`Maps`, `ServeMapsErrors`), `internal/users` (`addressLinks`), `internal/app/address.go`, `db/migrations/0049_addresses.sql`, `db/queries/address`

## Bağlam

Faz 2'nin adres PR'ı dokuz operasyonu getirir: `listMyAddresses`, `createMyAddress`, `getMyAddress`, `replaceMyAddress`, `deleteMyAddress`, `startAddressSearchSession`, `listAddressSuggestions`, `resolveAddressCandidate`, `reverseLookupAddress`; ve profilin `addressId` bağını (ADR-0036 Karar 5'in alan düzeyindeki 501'i). ADR-0029 Karar 8 neyin saklanacağını söyledi; bu ADR nasıl saklandığını, sağlayıcının nasıl çağrıldığını ve her çağrının nasıl bir bütçeye bağlandığını yazar.

Deponun baskın kusur sınıfı burada iki biçimde tehdit eder: (a) profil yamasındaki adres bağı #23'ün kurallarını (parola, bütçe, atomiklik) atlatan ikinci bir yol olabilir; (b) Google'a giden ikinci bir yol bütçeyi ve tavanı atlatabilir. Kararların çoğu bunları yapısal olarak imkânsız kılmak içindir. İlk sürümün iki bağımsız incelemesi (FIX-FIRST) maliyet varsayımında, tavanın durum kodunda ve birkaç kör testte bulgu çıkardı; bu ADR düzeltilmiş hâlidir.

## Karar

### 1. Google entegrasyonu

**Uçlar.** Sektör standardı Places API (New) ve Geocoding API'dir:

| İş | Uç | Faturalama (Essentials maskesiyle) |
|---|---|---|
| Öneri | `POST https://places.googleapis.com/v1/places:autocomplete` | İstek başına. Place Details Essentials'la biten oturumda ilk 12 istek tek tek, 13. ve sonrası ücretsiz; terk edilen oturumda hepsi tek tek ([session pricing](https://developers.google.com/maps/documentation/places/web-service/session-pricing)) |
| Çözme | `GET https://places.googleapis.com/v1/places/{placeId}?sessionToken=…` | Place Details Essentials; oturumu bitirir |
| Ters geocoding | `GET https://maps.googleapis.com/maps/api/geocode/json?latlng=…` | Geocoding, istek başına |
| Hizmet alanı sınırı | `GET …/geocode/json?components=country:TR\|administrative_area:<il>` | Geocoding; il başına günde bir (Karar 3) |

Oturumdaki önerilerin oturuma katlanması (Session Usage SKU) yalnız Pro ve üstü alanlarla biten oturumlarda geçerlidir; bu ADR'nin ilk sürümü bunu Essentials için de varsaymıştı ve yanlıştı. Pro alan istemek önerileri ücretsiz yapar ama çözmeyi Enterprise + Atmosphere fiyatından faturalar ve istemediğimiz içeriği getirir; Essentials'ta kalındı, maliyet Karar 7'de istek başına sayılır.

Places'in eski (Legacy) uçları seçilmedi: yeni projeye kapalıdır ve oturum belirteçleri ikisi arasında paylaşılamaz. Geocoding'in Places (New) hizalı v4 sürümü belgede bulunamadı; kararlı v3 JSON ucu kullanıldı.

**Alan maskesi (`X-Goog-FieldMask`).** Maske hem faturayı (SKU) hem gelen veriyi sınırlar:

- Autocomplete: `suggestions.placePrediction.placeId,suggestions.placePrediction.structuredFormat.mainText.text,suggestions.placePrediction.structuredFormat.secondaryText.text`. Tam açıklama (`text`), `types` ve `distanceMeters` istenmez.
- Place Details: `id,addressComponents,location`. Üçü de Essentials SKU'sundadır. **`formattedAddress` hiç istenmez**: istenmeyen alan gelmez, gelmeyen alan saklanamaz (MED-008). Java'nın maskesiyle aynıdır.
- Geocoding'de maske yoktur; istemci yanıtı yalnız `place_id`, `types`, `address_components` ve `geometry.bounds/viewport` taşıyan Go tiplerine çözer. `formatted_address` hiçbir tipe düşmez.

Dil `tr`, bölge `tr`, `includedRegionCodes: ["tr"]`, `includeQueryPredictions: false` (yalnız yer önerisi; sorgu önerisi çözülemez).

**İstemci (`internal/integrations/googlemaps`).** Düz `net/http`, SDK yok (İleti Merkezi kalıbı, ADR-0033):

- Zaman aşımı 8 saniye; isteğin kendi süre sınırı (`httpx.RequestTimeout`) daha kısaysa o kazanır.
- Okuma sınırı 256 KiB.
- Yönlendirme reddedilir: Go aynı sunucuya yönlendirmede `X-Goog-Api-Key` başlığını taşır, bir yönlendirme anahtarı gittiği yere götürürdü (`TestRedirectIsNotFollowed`: sahte sunucunun başka bir yoluna yönlendirilen istek oraya hiç gitmez).
- **Anahtar:** Places'ta `X-Goog-Api-Key` başlığında. Geocoding belgesi yalnız `key` sorgu parametresini tanımlıyor; bu yüzden istemcinin döndürdüğü hiçbir hata istek URL'sini taşımaz: `*url.Error` nedenine indirgenir, istek kurma hatası URL'siz yeni bir hatadır, mesajda anahtar geçerse `[key]` olur (`TestFailures`).
- Her hata `ErrUnavailable`'dır ve sonlu bir nedenler kümesinden biri taşır: `quota` (429 `RESOURCE_EXHAUSTED`, `OVER_QUERY_LIMIT`), `denied` (403 `PERMISSION_DENIED`, `REQUEST_DENIED`), `invalid`, `upstream`, `transport`, `unreadable`. Sonuçsuz başarı (`ZERO_RESULTS`) `ErrNoResult`'tur.
- İl sınırı yalnız `administrative_area_level_1` tipli sonuçtan alınır; bileşen süzgeci bir kasabaya ya da ülkeye düşerse sorgu `ErrNoResult`'tur (`TestRegionBoundsNeedsTheProvince`).

**Sahte (`internal/platform/fakes.GoogleMaps`).** İki servisi tek sunucuda, belgedeki biçimiyle taklit eder: Places hataları Google API hata gövdesiyle (`{"error": {code, message, status}}`), Geocoding hataları HTTP 200 altında `status` ile; Place Details maskesiz istekte 400 `INVALID_ARGUMENT`; `formattedAddress` yalnız maske isterse; ters geocoding önce bir plus code sonucu, sonra sokak adresi döner (gerçek servis gibi). Kota, ret, kesinti, bağlantı kopması ve askıda kalma enqueue edilebilir.

**Sürücü ve yapılandırma.**

| Değişken | Anlam |
|---|---|
| `KOALA_GOOGLE_MAPS_API_KEY` | Sunucu anahtarı, `config.Secret`; log'a ve hataya düşmez |
| `KOALA_GOOGLE_MAPS_DRIVER` | `google` (öntanım) ya da `fake` |
| `KOALA_SERVICE_AREA_CITIES` | Hizmet alanının il kimlikleri (Karar 3) |
| `KOALA_ADDRESS_LOOKUPS_PER_USER_DAY`, `_PER_IP_HOUR`, `_PER_DAY` | Arama bütçesi (Karar 7) |

`fake` sürücüsü (`googlemaps.Canned`) üç ilde birkaç sabit yerden yanıt verir ve hiçbir şey çağırmaz: Kadıköy/İstanbul, Çankaya/Ankara ve "Merkez"/Adıyaman. SMS'in `log` sürücüsünün emsalidir: `local`, `test` ve anahtarı olmayan bir staging bütün akışı onunla koşar; **production'da `Load` reddeder ve `ServeMapsErrors` ayrıca reddeder.** `google` sürücüsünde staging ve production anahtarsız açılmaz; `local` ve `test` açılır, arama `502 ADDRESS_PROVIDER_UNAVAILABLE` döner ve açılış log'u nedenini yazar (`TestServeMapsErrors`).

### 2. İl ve ilçe eşleştirmesi: Go'da, indeks yok

Google "Kadıköy", "İstanbul" yazar; referans verisi "KADIKÖY", "İSTANBUL".

**Java ne yapıyor:** `DistrictDao#findByNameAndCityAllIgnoreCase` tanımlı ama çağıranı yok. Gerçek eşleştirme `AddressSearchManager#resolveReferences`'ta, `IstanbulServiceArea.canonicalName` ile yapılıyor: NFD'ye ayrıştırıp birleşik işaretleri atar, `Locale.ROOT` ile büyütür, `İ` ve `ı`'yı `I`'ya çevirir ve " ILCESI" sonekini atar. Doğru çalışır ("Şişli" ve "ŞİŞLİ" ikisi de `SISLI` olur). Go aynı fikri izler.

**Seçim: eşleştirme Go'da, ikilinin taşıdığı referans verisi üzerinde (`address.Places`).** `db/referencedata`'nın CSV'leri ikiliye gömülüdür ve `koala migrate up` tabloları onlara eşitler (ADR-0037); 81 il, 973 ilçe, haritalar kablolamada bir kez kurulur. Seçilmeyenler:

- `turkish` collation'lı kolona indeks: her ICU yükseltmesinde `REINDEX` ister (`TestNoIndexUsesTheTurkishCollation`). Bu PR indeks eklemiyor; test ve runbook değişmedi.
- Normalleştirilmiş ad kolonu: tabloya ve eşitlemeye yeni bir türetilmiş alan ve onu tutarlı tutan ikinci bir kural eklerdi.
- `COLLATE "C"` + `lower` ifade indeksi: `lower` Türkçe değildir (I/ı sorunu aynen kalır).

Sorguya hiç gidilmez. İkili veritabanından eskiyse (geri dönüşte veri sürümü geri alınmaz) bilmediği satır eşleşmez; sonuç güvenlidir (aşağıda ilçe seçimi).

**Anahtar:** Türkçe büyük harf (`unicode.TurkishCase`: i→İ, ı→I), sonra ASCII'ye katlama (Ç→C, Ğ→G, İ→I, Ö→O, Ş→S, Ü→U, şapkalılar), boşluklar tekleştirilir. "Kadıköy", "KADIKÖY", "Kadikoy" tek anahtardır. Bir ebeveynde aynı anahtarı paylaşan iki satır belirsizdir ve hiçbir şeye eşleşmez.

**Vakalar:**

- İl: "İstanbul", "Istanbul", "İstanbul İli", "Istanbul Province" → İSTANBUL.
- İlçe sonekleri: "Şişli İlçesi" → ŞİŞLİ.
- **Merkez:** büyükşehir olmayan ilin merkez ilçesi referans verisinde `MERKEZ`'dir; Google onu "Merkez", ilin adıyla ("Adıyaman") ya da "Adıyaman Merkez" diye yazar. Üçü de o ilin `MERKEZ`'ine eşleşir. Büyükşehirde `MERKEZ` yoktur; oradaki "Merkez" eşleşmez.
- **Eski adlar:** küçük bir tarih tablosu (`districtRenames`): Eminönü → Fatih (2008, Java'nın da tek alias'ı), Kazan → Kahramankazan (2016), Eyüp → Eyüpsultan (2017). Yapılandırma değil, tarih; eksik bir ad kullanıcının seçimine düşer.
- Bileşen seçimi Java'nınkidir (`GoogleAddressComponents`): il `administrative_area_level_1` ya da `locality`; ilçe `administrative_area_level_2` ya da `sublocality_level_1`; mahalle `neighborhood`, `administrative_area_level_4`, `sublocality_level_2/3`, yoksa ilçeden farklıysa `sublocality_level_1`; site adı `premise`. Mahallenin " Mahallesi/Mah./Mh." soneki atılır ("Caferağa", sözleşmenin örneği). Posta kodu yalnız beş rakamsa alınır. Serbest satırlar sözleşmenin sınırlarına kesilir.

**Eşleşmeyen il ya da ilçe: ilçe kullanıcıya, il 400.**

- İlçe eşleşmezse `ResolvedAddress.districtId` `null` döner; il, satırlar, koordinat ve yer kimliği gelir. İstemci ilçeyi `/v1/reference/cities/{cityId}/districts` listesinden seçtirir; ilçe seçilene kadar değer `POST /v1/me/addresses`'e verilemez (`districtId` zorunlu). Sektördeki yerleşik akış budur: otomatik tamamlama formu doldurur, kullanıcı eksikleri düzeltir. Bir hata kodu seçilmedi: Google'ın bir yazım farkı kullanıcıyı çıkmaza sokar ve elle girişe geri götürürdü. Sözleşmede `districtId` nullable oldu (uç hiçbir istemcide yok).
- İl eşleşmez, il hizmet alanında değil ya da ülke alanın ülkesi değilse `400 ADDRESS_OUTSIDE_SERVICE_AREA`. Donmuş sözleşme, stil rehberi ve ADR-0034 bir alan kuralının ihlalini 400 sayar (`EDUCATION_PERIOD_INVALID` emsali); 422 Idempotency-Key deposunundur. İlk sürüm 422'yi seçip ADR-0034'ün kuralını daraltmıştı; inceleme bunu geri aldı.

### 3. Hizmet alanı: yapılandırılabilir il listesi

Java'da hizmet alanı yalnız İstanbul'dur (`IstanbulServiceArea`): öneriler İstanbul dikdörtgeniyle sınırlı, reverse kutu dışında Google'a gitmeden ret, elle adres ekleme bile İstanbul dışını reddediyor. Ürün kararı (Yusuf, 23.11.2026): "şu anlık İstanbul, ama istediğimiz zaman artırabilmeliyiz."

- `KOALA_SERVICE_AREA_CITIES`: virgülle ayrılmış referans il kimlikleri. Ayarsızsa `config.DefaultServiceAreaCities` (İstanbul'un kimliği). **Kodda İstanbul'a özgü hiçbir şey yoktur**: dikdörtgen, ad ya da test varsayımı. Öntanım bir yapılandırma değeridir.
- Kimlikler kablolamada ikilinin referans verisine karşı denetlenir: bilinmeyen bir il ve iki ülkeye yayılan liste sunucuyu açmaz (fail-closed, `TestServiceArea`).
- **Google sınırlaması listeden türer.** Autocomplete'in `locationRestriction` dikdörtgeni, listedeki her ilin sınırlarının birleşimidir. Sınırlar Google'dan alınır (Geocoding, `components=country:TR|administrative_area:<il adı>`, yalnız `administrative_area_level_1` tipli sonuç; `bounds`'u yoksa `viewport`'u). Gerekçe: 81 ilin dikdörtgenini elle yazmak doğrulanamayan veri olurdu ve "il eklemek tek adım" olmazdı; Google'ın koşulları koordinatın 30 güne kadar önbelleklenmesine izin verir, bir gün içinde kalınır.
- **Önbellek il başınadır.** Başarılı sonuç bir gün tutulur; bir ilin sorgusu başarısızsa yalnız o il geri çekilir: on dakika, her ardışık başarısızlıkta iki katı, en çok bir gün. Bütün iller geçerli sınıra sahip değilse sınırlama düşer (yalnız Türkiye); çözmedeki il denetimi her durumda nihai karardır. Her sınır sorgusu servisin kendi birimidir (`BOUNDS`) ve küresel tavana sayılır; tavan doluysa sorgu da yapılmaz ve geri çekilme işler. Google'ın hiç yanıt vermediği bir il geri çekilme başına bir birim harcar, istek başına değil (`TestFailingCityBoundsBackOff`).
- **Yenileme tek uçuşludur (single-flight).** Süresi dolan bir ili ilk bulan istek yenilemeyi kendisi koşar, isteğin iptaliyle bitmeyen ayrı bir context'le (`context.WithoutCancel` + 20 sn); diğerleri onu ya da kendi süre sınırlarını bekler. Google çağrılırken hiçbir kilit tutulmaz.
- **Ters geocoding ön denetimi:** nokta hizmet alanının bilinen dikdörtgeni dışındaysa Google'a hiç gitmez ve birim harcamaz (Java paritesi, veri en aza indirme; `TestReverseOutsideTheBoundsSpendsNothing`). Sınır bilinmiyorsa istek gider ve il denetimi karar verir.
- Birleşim dikdörtgeni uzak iki ilde aradaki illeri de kapsar; oradan gelen bir öneri çözmede 400 alır. Kabul edildi: daraltmak öneri metninden il ayıklamayı gerektirirdi, bu da Google'ın biçimine bağlı ikinci bir kural olurdu.
- **Genişletme tek adımdır:** listeye il kimliği eklenir ve servis yeniden başlatılır (runbook "Hizmet alanına il ekleme"). İki illi test bunu bağlar (`TestServiceAreaIsTheConfiguredCities`).
- **Kullanıcının kendi adresi hizmet alanından bağımsızdır.** Elle adres ekleme sözleşmeye uyar ve il kısıtı yoktur (Java'dan bilinçli ayrılık): bir işçi Ankara'da oturup İstanbul'da çalışabilir. İlan konumunun hizmet alanına bağlanması Faz 3'ün işidir.
- Yeniden başlatmadan değiştirmek (admin paneli ya da DB ayarı) istenirse ayrı bir iştir: sınır önbelleğinin geçersiz kılınmasını ve değişikliğin denetim kaydını getirir.

### 4. Adres modeli ve koordinat

- `addresses`: soft-delete'li, `pii: true` (security barrier), kullanıcıyla birlikte silinir (`cascade`). Sahiplik `(id, user_id)` ile aranır; başkasının adresi hiç olmamış adresin 404'ünü bayt bayt alır (ADR-0014).
- İl ve ilçe referans satırıdır; **ilçe bileşik FK'yle ilinin ilçesidir** (`(district_id, city_id) → districts (id, city_id)`). Başka ilin ilçesi, olmayan ve emekliye ayrılmış satır `404 REFERENCE_ITEM_NOT_FOUND`; adresin zaten taşıdığı emekli satır değiştirmede kalabilir (ADR-0036'nın kuralı).
- Serbest satırlar: mahalle, cadde/sokak, bina no, bina adı, **site adı, blok** (Java'nın `siteName` ve `block`'u; ADR-0029 Karar 8 sayar; sözleşmeye isteğe bağlı alan olarak eklendi), kat, daire, tarif. Sözleşmenin sınırlarındadır; DB `CHECK`'leri aynısını tutar. Kırpılır, boşsa `null`. Posta kodu sözleşmeye `pattern: ^[0-9]{5}$` olarak eklendi. Görünen satır site ve bloğu da yazar ("Moda Caddesi 12/7, Moda Konutları B Blok, Caferağa, Kadıköy, İstanbul").
- `provider_place_id` 255'e kadar; kalıcı değil, yenilenebilir bir işaretçidir.
- **Koordinat tam hassasiyetle saklanır** (`double precision`), ikisi birlikte ya da hiç. Görünen hassasiyet tek yerde, `Point.Render(precision)`'da: `EXACT` saklanan değer; `APPROXIMATE` 0,01°'lik ızgaraya yuvarlanır (kuzey-güney ~1,1 km, doğu-batı Türkiye'de ~0,85 km). Izgara, rastgele kaydırma değil: aynı adres hep aynı hücreye düşer, çok sayıda yanıtın ortalaması fazlasını öğretmez. Bugün tek okuyucu sahibidir ve `EXACT` görür. Şirket adresi (tam, gizlenebilir), bireysel ilan ve harita (yaklaşık) kendi modülleriyle gelir ve `Render`'a kendi hassasiyetlerini verir ([[job-location-rule]], MED-009 `partial: phase 3`).
- `ResolvedAddress`'in koordinatı `Render`'dan geçmez: ters aramada çağıranın kendi pinidir, çözmede çağıranın seçtiği yerin konumudur; ikisinde de değeri gören, adresin sahibi olacak çağırandır.
- Kullanıcı başına **en çok 10 adres** (`AddressesPerUser`, ürün kararı 23.11.2026); aşımı `409 ADDRESS_LIMIT_REACHED`. Sayım ve ekleme kullanıcının satır kilidi (`FOR UPDATE`) altındadır (`TestAddressLimitHoldsUnderConcurrency`).
- Liste yeniden eskiye (UUIDv7 kimlik), en çok on kayıt olduğu için serviste kesilir; cursor `adr1|yön|kimlik` (base64url), bozuksa `400 CURSOR_INVALID`.

### 5. Birincil adres ve profil bağı

- **Birincil adres `users.address_id`'dir** (sözleşme: profilin `addressId`'si "birincil adresin kimliği"). Tek kolon olduğu için en çok bir birincil vardır, yapısal olarak.
- `users.address_id → addresses` `restrict` kenarıdır (medya bağı kalıbı, ADR-0036 Karar 10). **Adresin her tüketicisi** (bugün profil, yarın şirket ve ilan) `restrict` kenarıyla bağlanır; `TestAddressConsumersAreRestrictEdges` ebeveyni `addresses` olan her kenarı denetler. Adresin kullanıcıya ait olduğunu bileşik FK değil kod tutar: soft-delete kataloğu kenar başına tek kolon okur. Kod bunu kullanıcının satır kilidi altında, adresi `(id, user_id)` ile `FOR SHARE` kilitleyerek yapar.
- **Bağın tek yazıcısı adres modülüdür** (`setPrimary`): bağı ve profil senkronu niyetini (`profilesync.Record`, Java'nın `UserManager.java:536` yayını, ADR-0036 Karar 8) aynı transaction'da yazar. Üç yerden çağrılır:
  1. **Profil yaması** (`users.Service.Update`): `addressId` değişirse yama `addressLinks.LinkPrimary`'yi kendi transaction'ında, kullanıcının satır kilidi altında çağırır. Başkasının ya da olmayan adres `404 ADDRESS_NOT_FOUND`; `null` bağı kaldırır, adresi silmez.
  2. **İlk adres:** birincil adresi olmayan kullanıcının eklediği adres birincil olur (Java'da kullanıcının tek adresi vardı). Ürün kararı 23.11.2026.
  3. **Birincil adresin silinmesi:** bağ aynı transaction'da önce `null` olur ve niyet yazılır, sonra adres silinir. Başka bir adres kendiliğinden birincil olmaz (Amazon modeli; ürün kararı 23.11.2026).
- **Kilit sırası** her yolda aynıdır: kullanıcının satırı, sonra adres. Yama `LockProfile` (FOR UPDATE) → `OwnAddress` (FOR SHARE); silme `LockOwner` (FOR UPDATE) → `LockAddress`; ekleme `LockOwner` → `LockActiveParent(users)` (aynı satır). Değiştirme yalnız adresi kilitler. Döngü kurulamaz.
- **Hesap silme:** `addresses.user_id → users` `cascade`, `users.address_id → addresses` `restrict`. Platform önce kullanıcıyı işaretler, kaskadla adresleri, sonra `restrict` kenarını denetler; tüketici aynı silmede işaretlendiği için engellenmez (`TestAccountDeletionTakesItsAddresses`). Fiziksel silme ya da anonimleştirme yapan `privacy` PR'ı önce `users.address_id`'yi `NULL` yapmak zorundadır.
- **#23'ün kuralları atlatılamaz:** adres bağı parola istemez; ama aynı yamada e-posta varsa parola yine istenir (`dependentRequired`) ve yanlış parola bütün yamayı, adres bağı dahil, reddeder (`TestProfilePrimaryAddressLink`).

### 6. Arama oturumu ve lisans: ne, nerede, ne kadar

Google Maps Platform politikası: yer kimliği önbellekleme kısıtından muaftır ve süresiz saklanabilir; enlem/boylam en çok 30 ardışık gün önbelleklenebilir; diğer içerik yalnız koşulların açıkça izin verdiği yerde. Son kullanıcının Autocomplete'te seçtiği adres içerik kısıtlarına tabi değildir; istisna öneri listesine uygulanmaz ve metninde ters geocoding'i anmaz.

| Veri | Nerede | Ne kadar | Dayanak |
|---|---|---|---|
| Önerilerin metni (`primaryText`, `secondaryText`) | Yalnız yanıtta; hiçbir yere yazılmaz | İstek süresi | Öneri listesi istisnanın dışında |
| Önerilerin yer kimlikleri | `address_search_sessions.candidates` | Oturum (15 dk) + temizlik (saatlik) | Yer kimliği muaf |
| Oturum belirteci | Aynı satırda; rastgele UUIDv4 | Oturum | Google içeriği değil |
| Çözülen ve ters aranan bileşenler | Yalnız yanıtta; istemci kaydederse adres olur | — | Seçilen adres istisnası (çözme); ters arama için açık soru 1 |
| `formatted_address`, ham yanıt | Hiçbir yerde; istenmez, decode edilmez | — | ADR-0029 Karar 8 |
| Adresin yer kimliği | `addresses.provider_place_id` | Süresiz, yenilenebilir | Yer kimliği muaf |
| Adresin bileşenleri ve koordinatı | `addresses`, kaynağıyla (Karar 10) | Süresiz, temizlenebilir | Açık soru 1 |
| İl sınırları | Süreç belleği | 1 gün | Koordinat ≤ 30 gün |

**Oturum:** çağırana bağlı, 15 dakika. Bir çözmeyle biter (Google'ın oturumu Place Details'le biter); bitmiş oturum, başkasının oturumu, süresi dolmuş ve hiç olmamış oturum aynı `404 ADDRESS_SEARCH_SESSION_NOT_FOUND`'u bayt bayt alır (MED-010). `404` alan istemci yeni bir oturum açar. Aday kimliği `c_<n>`, oturumun yer kimliği dizisindeki indekstir; oturumun hiç önermediği aday oturumun 404'üdür. Sağlayıcı çağrısı başarısız olursa bitiş geri alınır (birim harcanmış kalır; `TestProviderFailureOverHTTP`). Oturum bellekte değil tablodadır: birden çok örnek ve yeniden başlatma oturumu düşürmez (Java bellekte tutuyordu).

**Idempotency-Key yok, gerekçeleri:** oturum açmanın tekrarı yeni bir oturum ve bir birim daha harcar, kaybolan oturum 15 dakikada ölür; çözmenin tekrarı bitmiş oturumda `404` alır ve istemci yeni oturumla yeniden arar (anahtar bir yanıtı geri verse de bitmiş oturumu açmazdı); ters aramanın tekrarı aynı noktayı yeniden çevirir ve bir birim daha harcar. Hiçbiri kayıt yazmaz; tekrarların bedeli bütçededir.

### 7. Maliyet ve kötüye kullanım

**Sayım birimi sağlayıcının faturaladığı çağrıdır**, ve oturum açılışı: `SESSION` (faturalanmaz, ama bir çağıranın kaç oturum çevirdiğini sınırlar), `AUTOCOMPLETE` (her öneri isteği; Essentials'ta faturalanır), `DETAILS` (çözme), `REVERSE` (ters geocoding), `BOUNDS` (servisin il sınırı sorgusu). Kırpıldıktan sonra üç karakterden kısa sorgu sağlayıcıya gitmez ve birim harcamaz.

**Bir oturum en çok 12 öneri isteği yapar** (`SuggestionRequestsPerSession`): Essentials'la biten oturumun faturalanan kısmı budur, ve debounce yapan bir istemci bir adres için 6–12 istek gönderir. Sonrası `429 ADDRESS_LOOKUP_LIMIT_REACHED` (`Retry-After` = `RetryFloor`, 1 sn; istemci hemen yeni oturum açabilir).

**Tipik bir adres girişi:** 1 oturum + 6–12 öneri + 1 çözme = **8–14 birim**. Değerler bunun üzerinden:

| Sınır | Öntanım | Hesap |
|---|---|---|
| Kullanıcı, 24 saat (`KOALA_ADDRESS_LOOKUPS_PER_USER_DAY`) | **150** | ~10 tam adres girişi (14 × 10 = 140) ve yeniden denemeler; meşru bir kullanıcı günde birkaç adres girer |
| İstemci adresi, 1 saat (`_PER_IP_HOUR`) | **600** | ~40 giriş/saat; mobil CGNAT bir çıkış adresini yüzlerce aboneye paylaştırır (ADR-0033'ün `verification_address` gerekçesi) |
| Küresel, 24 saat (`_PER_DAY`) | **3000** | ~210 giriş/gün. En kötü durumda hepsi Place Details/Geocoding fiyatında (liste fiyatı 1000 başına 5 $ düzeyi) günde ~15 $, tipik karışımda (çoğu birim Autocomplete, 1000 başına ~2,8 $) daha az. Google'ın aylık ücretsiz kotası hesaba katılmadı |

İlk sürüm oturumdaki önerileri ücretsiz sayıyordu; 2000 birimlik tavan en kötü durumda ~40.000 faturalı isteğe denk geliyordu. Değerler ölçülmedi (gerçek trafik yok), yapılandırılabilir ve yeniden başlatmayla değişir.

**Defter ve sıra.** Her birim `address_lookups` defterine bir satırdır (M2, bir gün). Rezervasyon tek fonksiyondur (`reserve[T]`), sağlayıcı çağrısından önce kısa bir transaction'da, tek bir advisory kilit altında sayar ve yazar; sıra #23'ünkidir:

1. **Kullanıcı:** son 24 saatte en çok 150. İlk sırada: kendi bütçesini aşan çağıranın istekleri adresin bütçesinden hiçbir şey harcamaz.
2. **İstemci adresi** (IPv4 ya da IPv6 /64, `authn.ClientKey`): son bir saatte en çok 600. Kova çağıranınkinin arkasında olduğu için tek bir çağıran adresi yakamaz.
3. **Küresel tavan:** son 24 saatte en çok 3000. Her birim, hangi dal olursa olsun, aynı biçimde sayılır (#21'in dersi).

Her ret `429 ADDRESS_LOOKUP_LIMIT_REACHED` ve `Retry-After`'dır: reddeden pencereden en eski birimin çıkışına kadar, en az `RetryFloor`. Ret hiçbir şey yazmaz (`TestLookupRefusalsWriteNothing`); sıranın kendisi `Retry-After`'la bağlıdır (`TestLookupBudgetOrder`: iki bütçesi de dolu çağıran kendi gününün beklemesini alır, adresin saatini değil).

**Tavan neden 429, neden tek kod:** tavan sağlayıcının hatası değil faturanın sınırıdır; 502 olsaydı tavan dolduğunda `KoalaHighHttp5xxRate` çalardı ve gerçek sağlayıcı hatası ondan ayırt edilemezdi. 502 yalnız gerçek sağlayıcı hatasına ve yapılandırılmamış sağlayıcıya kalır. Ayrı bir `ADDRESS_LOOKUP_CAP_REACHED` seçilmedi: istemcinin çaresi dört durumda da aynıdır (bekle ya da adresi elle gir), tek kod hangi bütçenin dolduğunu söylemez ve dört dalı tek davranışa indirir. Hangi sınırın reddettiği metrikte görülür (`outcome` etiketi).

**Yapısal bağ: sağlayıcıya ancak kendi türünden harcanmış birimle ulaşılır.** `spent[T]` birimin türünü tip parametresinde taşır (`autocompleteTag`, `detailsTag`, `reverseTag`, `boundsTag`, `sessionTag`); `metered`'in her çağrısı yalnız kendi türünün birimini alır, yani çözme bir oturumun birimiyle, öneri bir başkasıyla ödenemez. İlk sürümde `openSession` bir `spent` döndürüyordu ve çözmenin `DETAILS` birimi testsizdi; artık oturum hiç birim döndürmez. Paket içinde elle `spent` kurmayı tip engelleyemez; `TestOnlyReserveSpends` paketin kaynağını okur ve `reserve` dışındaki her `spent` literalini kırmızı sayar.

**"Tek aktör herkesi durdurabilir mi?"** Kullanıcı ve adres bütçesi için hayır. Küresel tavan için: tek bir hesap en çok 150, tek bir adres saatte 600 birim harcar; tavanı birkaç hesap ya da dağıtık adreslerle doldurmak mümkündür. **Kabul edilen risk:** o gün meşru kullanıcılar adresi elle girer; tavanın varlık nedeni faturadır. Azaltma: metrik ve uyarılar, yeniden başlatmayla ayarlanan değerler.

**Numaralandırma.** Arama uçları hesap varlığı sızdırmaz: hiçbiri bir hedef (e-posta, numara) almaz. Oturumun sahipliği her bütçe denetiminden **önce** denetlenir: başkasının oturum kimliği, tavan doluyken de, hiç olmamış oturumun 404'ünü alır.

**Metrik ve uyarı.**

- `koala_address_lookups_total{unit, outcome}` (`reserved`, `user_limited`, `client_limited`, `capped`), `koala_address_lookup_cap`, `koala_address_provider_calls_total{call, outcome}`.
- `KoalaAddressLookupCapNear`: son 24 saatte rezerve edilen birim tavanın %80'ini geçti.
- `KoalaAddressLookupCapReached`: tavan bir isteği reddetti.
- `KoalaAddressAutocompleteVolumeHigh`: bir saatte 1000'den fazla öneri isteği Google'a gitti (tavanın üçte biri); öneri pompalaması tavandan önce hacim olarak görünür.
- `KoalaAddressProviderFailing`: sağlayıcı çağrıları kota, ret, kesinti ya da bağlantı hatasıyla düşüyor.

### 8. Hata kodları

| Kod | Durum | Kim döner |
|---|---|---|
| `ADDRESS_NOT_FOUND` | 404 | adres uçları, profil yaması |
| `REFERENCE_ITEM_NOT_FOUND` | 404 | ekleme, değiştirme |
| `ADDRESS_LIMIT_REACHED` (yeni) | 409 | ekleme |
| `SOFT_DELETE_RESTRICTED` | 409 | silme (gelecekteki şirket/ilan tüketicisi) |
| `ADDRESS_SEARCH_SESSION_NOT_FOUND` | 404 | öneri, çözme |
| `ADDRESS_OUTSIDE_SERVICE_AREA` | 400 | çözme, reverse |
| `ADDRESS_LOOKUP_LIMIT_REACHED` (yeni) | 429 + `Retry-After` | oturum, öneri, çözme, reverse |
| `ADDRESS_PROVIDER_UNAVAILABLE` | 502 | oturum, öneri, çözme, reverse; yalnız sağlayıcı hatası ve yapılandırılmamış sağlayıcı |
| `CURSOR_INVALID` | 400 | liste |

### 9. Veri en aza indirme

KVKK'nın yurt dışı aktarım kararı hukuk konusu ve açık (Açık sorular 2). Bekleyen bir karar yüzünden Google'a gerekenden fazla veri gitmez:

- **Reverse'te yalnız koordinat** gider (`latlng`, dil, bölge, anahtar); kullanıcı kimliği, cihaz, istemci adresi gitmez (`TestReverse` sorgu parametrelerini sayar). Hizmet alanının bilinen sınırları dışındaki nokta hiç gitmez (Karar 3).
- **Oturum belirteci rastgele bir UUIDv4'tür** ve kullanıcıya bağlanmaz; oturumun kimliğinden de ayrıdır.
- Autocomplete gövdesi yalnız girdi, belirteç, dil/bölge ve dikdörtgen taşır (`TestAutocomplete` gövdenin anahtarlarını sayar). Çağıranın isteğinin hiçbir başlığı Google'a iletilmez.
- **Log'a sorgu metni, yer kimliği, koordinat ve adres yazılmaz;** sağlayıcı hatasında yalnız çağrı adı ve nedeni yazılır.

### 10. Kaynak kolonları

`addresses`'e iki kolon eklendi (ürün kararı 23.11.2026):

- `components_source`: `MANUAL` (kullanıcının girişi), `AUTOCOMPLETE` (bir öneri çözmesi), `REVERSE_GEOCODE` (ters arama).
- `coordinate_source`: `USER_PIN` ya da `PROVIDER`; koordinat yoksa `NULL`. `AUTOCOMPLETE`'te `PROVIDER`, diğerlerinde `USER_PIN`.

İki kolon, iki ayrı hukuk sorusuna karşılık gelir: Google bileşenlerinin süresiz saklanması (ters aramanın bileşenleri Autocomplete istisnasının metninde yok) ve Google koordinatının 30 gün kuralı. Hukukçu "saklanamaz" derse temizlik bu kolonlarla bulunur. Kaynağı istemci gönderir (`AddressInput.source`; çözme ve ters arama `ResolvedAddress.source` olarak verir). Gönderilmezse yer kimliği taşıyan adres `AUTOCOMPLETE`, taşımayan `MANUAL` sayılır. Bu varsayılan muhafazakârdır: kaynağı bilinmeyen Google kaynaklı veri temizlikten kaçmaz. Çözülen adresin pinini kullanıcı sonradan oynatırsa koordinat yine `PROVIDER` kalır; yanılgı fazla silme yönündedir, eksik silme yönünde değil (`TestAddressSources`).

## "Silinirse kırmızı olur mu?"

| Kural | Silinirse | Test |
|---|---|---|
| Ret hiçbir şey yazmaz | u1'in reddedilen istekleri adresin birimlerini yer, u2 reddedilir | `TestLookupRefusalsWriteNothing` |
| Sıra: kullanıcı → adres | İki bütçesi dolu çağıran adresin saatlik beklemesini alır | `TestLookupBudgetOrder` (`Retry-After` > 1 saat) |
| İstemci adresi bütçesi | Aynı adresteki üçüncü kullanıcı geçer | `TestLookupRefusalsWriteNothing` (u3, nat) |
| Küresel tavan, her dalda, 429 | Oturum ya da reverse tavanı aşar, ya da 502 döner | `TestLookupRefusalsWriteNothing` (`Retry-After` = 86400, defter 6) |
| Her öneri isteği bir birim | Yazım bütçeyi yemez, tavan doluyken öneri sürer | `TestSuggestionsSpendTheBudget`, `TestSessionSuggestionBound` |
| Çözme bir `DETAILS` birimi | Çözme tavanı atlatır | `TestSuggestionsSpendTheBudget` (tavanda 429, normalde bir `DETAILS` satırı) |
| Sağlayıcıya yalnız kendi türünden birimle | — | Derleyici (`spent[T]`) ve `TestOnlyReserveSpends` |
| Oturum başına 12 öneri | 13. istek geçer | `TestSessionSuggestionBound` |
| Kısa sorgu sağlayıcıya gitmez | Kırpılmış iki harf öneri alır | `TestAddressSearchOverHTTP` |
| Sınır önbelleği il başına, geri çekilme | Başarısız il her istekte birim yer | `TestFailingCityBoundsBackOff` |
| İl sınırı yalnız il tipli sonuçtan | Kasabanın sınırı ilin yerine geçer | `TestRegionBoundsNeedsTheProvince` |
| Reverse ön denetimi | Alan dışı nokta Google'a gider ve birim yer | `TestReverseOutsideTheBoundsSpendsNothing` |
| Oturum sahipliği (MED-010) | Başkası oturumu kullanır | `TestAddressSearchOverHTTP` |
| Çözme oturumu bitirir | Aynı oturumda öneri sürer | `TestAddressSearchOverHTTP` |
| Sağlayıcı hatası 502, kayıt yok, oturum açık kalır (MED-013) | Hata 500 döner, adres yazılır ya da oturum bitmiş kalır | `TestProviderFailureOverHTTP` |
| `formattedAddress` istenmez (MED-008) | Maskeye eklenirse | `TestDetails` (maske denetimi) |
| Yer kimliği yazılır (MED-008) | `provider_place_id` boş kalır | `TestAddressSearchOverHTTP` |
| Yer kimliği dışında öneri saklanmaz | `candidates` metin taşır | `TestAddressSearchOverHTTP` |
| Kaynak kolonları | Yanlış kaynak yazılır | `TestAddressSources`, `TestAddressSearchOverHTTP` |
| Koordinat tam saklanır (MED-009) | Yuvarlanarak yazılır | `TestAddressesOverHTTP` (DB'de 9 basamak), `TestRender` |
| İlk adres birincil | Profil `null` kalır | `TestAddressesOverHTTP` |
| Birincil silinince bağ ve niyet | Silme 409 alır ya da niyet yazılmaz | `TestAddressesOverHTTP` (`updated_at`) |
| Bağın değişikliği niyet yazar | Niyet yazılmaz | `TestProfilePrimaryAddressLink` |
| Başkasının adresine bağ | Bağ kurulur | `TestProfilePrimaryAddressLink` |
| Yanlış parola bağı da geri alır | Bağ yazılır | `TestProfilePrimaryAddressLink` |
| Adres sınırı (10) | 11. eklenir | `TestAddressRulesOverHTTP` |
| Sınır eşzamanlılıkta | Yarışan eklemelerden birden çoğu geçer | `TestAddressLimitHoldsUnderConcurrency` |
| Tavan eşzamanlılıkta | Yarışan başlatmalar tavanı aşar | `TestLookupCapHoldsUnderConcurrency` |
| İlçe ilinin ilçesi | Çankaya İstanbul'a eklenir | `TestAddressRulesOverHTTP` |
| Adres tüketicisi `restrict` | Şirket ya da ilan adresi `detach`/`cascade` bağlanır | `TestAddressConsumersAreRestrictEdges` |
| Hizmet alanı listeden, çok il | Ankara İstanbul alanında çözülür | `TestServiceAreaIsTheConfiguredCities`, `TestResolvedServiceArea` |
| Bilinmeyen il açılışı durdurur | Sunucu açılır | `TestServiceArea` |
| Merkez ve eski adlar | Adıyaman'ın Merkez'i, Eyüp `null` | `TestCentralDistrictMatches`, `TestPlacesMatchTheProvidersNames` |
| Hesap silmeyi `restrict` engellemez | Hesap silinemez | `TestAccountDeletionTakesItsAddresses` |
| Anahtar hataya düşmez | Hata URL taşır | `TestFailures` |
| Yönlendirme izlenmez | `CheckRedirect` silinir, istek anahtarla başka yola gider | `TestRedirectIsNotFollowed` |
| Production'da sahte sürücü | Açılır | `TestLoadRejectsBadMapsValues`, `TestServeMapsErrors` |

**Denenmeyenler:** profil yaması ile adres silmenin eşzamanlı yarışı (kilit sırası kod incelemesiyle); `Restore` bu kenarlarla; eşzamanlı rezervasyonda kullanıcı ve adres bütçesi (yalnız küresel tavan eşzamanlı sınandı); single-flight yenilemenin eşzamanlı istekleri gerçekten tek uçuşa indirmesi (testler sıralı; eşzamanlı bir sınır yenilemesi testi yok); yenilemenin isteğin iptalinden bağımsız sürmesi; il sınırı geri çekilmesinin bir günlük üst sınırı. Bu PR'daki hiçbir test yerelde koşturulmadı (yerelde ağır iş yok); hepsi CI'da koşar.

## Java paritesi

| Konu | Java | Go | Not |
|---|---|---|---|
| Adres sayısı | kullanıcı başına tek (`PUT /users/me/address`) | liste, en çok 10 | ADR-0029 kapsam defteri |
| Hizmet alanı | İstanbul, kodda sabit | yapılandırılabilir il listesi, öntanım İstanbul | Karar 3 |
| Elle adres il kısıtı | İstanbul dışı ret | yok | Karar 3 |
| Reverse ön denetimi | `IstanbulServiceArea.contains`, Google'a gitmeden | il sınırlarının birleşimi, Google'a gitmeden | Karar 3 |
| Oturum deposu | süreç belleği | tablo | Karar 6 |
| Öneri sayısı | en çok 5 | sağlayıcının verdiği (≤ 5), sözleşme ≤ 10 | — |
| Ad eşleşmesi | `canonicalName`: NFD, işaretleri at, `ROOT` büyüt, İ/ı → I; ilçe eşleşmezse ret | Türkçe büyük harf + ASCII katlama; ilçe kullanıcıya | Karar 2; `findByNameAndCityAllIgnoreCase` tanımlı ama çağrılmıyor |
| Eski adlar | Eminönü → Fatih | Eminönü, Eyüp, Kazan | Karar 2 |
| Site adı | `premise` → `siteName` | aynı | Karar 4 |
| Reverse | iki çağrı (bina, sonra alan) | tek çağrı, en ayrıntılı sonuç | maliyet |
| Reverse koordinatı | kullanıcının pini | aynı | — |
| Aday kanıtı (`candidateToken`) | ekleme kanıt ister | yok; kaynak kolonları kaydeder | Karar 10 |
| Bütçe | yok | birim defteri, üç sınır, oturum başına 12 öneri | Karar 7 |
| Birincil adres yayını | `UserManager.java:536` | `setPrimary` → `profilesync.Record` | ADR-0036 Karar 8 |

## Sözleşme değişiklikleri

Dokuz operasyonun `x-not-implemented` işareti kalktı, iskele metotları silindi. Uçlar hiçbir istemcide yok.

- `listMyAddresses`: `x-error-codes: [CURSOR_INVALID]`.
- `createMyAddress`: `ADDRESS_LIMIT_REACHED`; `201` `Idempotent-Replayed`; açıklama ilk adresi ve hizmet alanı bağımsızlığını yazar.
- `startAddressSearchSession`, `listAddressSuggestions`, `resolveAddressCandidate`, `reverseLookupAddress`: `ADDRESS_LOOKUP_LIMIT_REACHED`; çözme ve reverse'e `ADDRESS_OUTSIDE_SERVICE_AREA`; açıklamalar bütçeyi, oturumun bitişini, istemcinin 404'te yeni oturum açmasını ve veri en aza indirmeyi yazar; üç `x-no-idempotency-key` gerekçesi gerçek yan etkileri anar (Karar 6).
- `deleteMyAddress`, `replaceMyAddress`: açıklama.
- `updateMyProfile`: `ADDRESS_NOT_FOUND` listeye döndü; açıklamadaki 501 cümlesi kalktı.
- `AddressInput`: `siteName`, `block`, `source`; `postalCode` `pattern: ^[0-9]{5}$`. `Address`: `siteName`, `block`. `ResolvedAddress`: `districtId` nullable, `siteName`, `source`; açıklama "olduğu gibi" iddiasını `districtId` doluysa diye daraltır. Yeni `AddressSource` enum'u.
- `ErrorCode`: `ADDRESS_LIMIT_REACHED` (409), `ADDRESS_LOOKUP_LIMIT_REACHED` (429) eklendi; `ADDRESS_PROVIDER_UNAVAILABLE`'ın düzyazısı tavanı anmaz; `ADDRESS_OUTSIDE_SERVICE_AREA` 400 kaldı.

## Yerine geçtiği

1. **ADR-0036 Karar 5'in `addressId` maddeleri** (alan düzeyinde 501, `ADDRESS_NOT_FOUND`'un listeden çıkması, tetik testinin `addresses` satırı) → Karar 5: bağ canlı, kod listede, tetik testinden `addresses` çıktı.
2. **ADR-0036 Karar 8'in "henüz yazmayanlar"ındaki birincil adres yayını** → Karar 5: `setPrimary`.
3. **ADR-0029 Karar 8'in "Adres arama oturumu … ömrü 15 dakika"** → değişmedi; oturumun çözmeyle bitmesi ve aday kimliğinin biçimi eklendi (Karar 6).
4. **`test/scenarios/contract_test.go`'nun `boundAhead`'indeki MED satırları** → alanın son donmuş operasyonu indi; MED P0'ları artık doğrudan bağlı.
5. **`db/referencedata/sync_integration_test.go`'nun `TestMigrationGivesVersionOneItsDegree`'si** 0048'in son migration olduğunu varsayıyordu (tek adım geri); artık 0048'e kadar geri sarar.

ADR-0007, ADR-0013, ADR-0014, ADR-0021, ADR-0026, ADR-0029, ADR-0033, ADR-0034, ADR-0035 ve ADR-0036'nın kararları başka yerde değişmiyor.

## Sonuçlar

- Bağlanan P0'lar: MED-008, MED-009 (`partial: phase 3`: şirket, ilan ve harita görünümleri), MED-010. P1 MED-013 bağlandı. `media` alanında donmuş operasyon kalmadı.
- Şirket PR'ı şirket adresini bu tabloya bağlar: `addresses`'e şirket sahipliği ve `SOFT_DELETE_RESTRICTED`'in ilk gerçek tüketicisi o PR'dadır; kenarı `restrict` olmak zorundadır (`TestAddressConsumersAreRestrictEdges`).
- İlan (Faz 3) konumu hizmet alanına bağlar ve haritayı `Render(PrecisionApproximate)` ile çizer.
- Hizmet alanını yeniden başlatmadan değiştirmek ayrı bir iştir (Karar 3).
- İstemciler önerileri harita olmadan gösterirken Google Maps atfını göstermek zorundadır (politika: "Google Maps" logosu ya da metni). Sunucu bunu sağlayamaz; istemci işine not edildi. İstemci kaydederken `ResolvedAddress.source`'u geri gönderir.
- `privacy` PR'ı hesabı fiziksel olarak silmeden önce `users.address_id`'yi `NULL` yapar (Karar 5).

## Açık sorular

1. **Google içeriğinin süresiz saklanması (hukukçu teyidi).** Politika son kullanıcının Autocomplete'te seçtiği adresi içerik kısıtlarının dışında tutuyor; koordinatı 30 günle sınırlıyor; metninde ters geocoding'i anmıyor. Sorulacaklar: (a) çözülen adresin bileşenleri ve koordinatı istisnaya giriyor mu; (b) ters geocoding'in bileşenleri (koordinat kullanıcının pinidir) istisnaya giriyor mu, girmiyorsa ne kadar saklanabilir. ADR-0029 açık soru 4 ile birlikte. Teyit "saklanamaz" derse `components_source` ve `coordinate_source` (Karar 10) temizlenecek satırları bulur; o güne kadar saklamaya devam edilir.
2. **KVKK yurt dışı aktarım:** kullanıcının yazdığı metin ve pini Google'a (ABD) gider. Java'nın ADR-0007'si Google kaynaklı verinin kalıcı kullanımını bu karara bağlamıştı. Hukuk konusu; Karar 9 aktarılanı en aza indirir. Prod yok, gerçek kullanıcı yok.
3. **Değerler ölçülmedi:** 150/gün, 600/saat, 3000/gün, oturum başına 12 öneri ve fiyat varsayımları. İlk trafikte `koala_address_lookups_total` ve Google faturasıyla görülür.
