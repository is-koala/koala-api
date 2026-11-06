# Veritabanı: roller, yayın ve bakım

Bu sayfa yalnız açıklamadır; kararlar ve gerekçeleri ADR-0013 ve ADR-0017'dedir. Komutlar `koala` ikilisinin alt komutlarıdır, ortam değişkenlerinin **değerleri** sır deposundadır.

## Cluster ve roller

- Her ortamın (staging, production) **kendi Postgres cluster'ı** vardır. Roller cluster düzeyindedir: iki ortam aynı cluster'ı paylaşırsa `koala_app` iki ortamın tablolarına birden erişir.
- Grup rolleri (`koala_owner`, `koala_app`, `koala_maint`) giriş yapamaz. `koala bootstrap` onları yoksa kurar; her yayında çalışır ve var olan rollere dokunmaz.
- Giriş kullanıcılarını operatör açar, parolaları sır deposuna koyar ve her birini **tek** bir grup rolüne üye yapar:

| Değişken | Giriş kullanıcısı | Üyelik | Kullanan |
|---|---|---|---|
| `KOALA_DATABASE_URL` | sunucu | yalnız `koala_app` | `koala serve` (fazlası varsa açılmaz) |
| `KOALA_OWNER_DATABASE_URL` | göç ve bakım | `koala_owner` | `koala migrate`, `koala maint reindex` |
| `KOALA_MAINT_DATABASE_URL` | personel yönetimi ve denetim temizliği | `koala_maint` | `koala staff`, `koala maint audit-purge` (ADR-0026) |
| `KOALA_BOOTSTRAP_DATABASE_URL` | kurulum | `CREATEROLE` yetkili; URL'deki DB `postgres` | `koala bootstrap` |

- Hiçbir giriş süper kullanıcı değildir; `koala` komutları süper kullanıcı girişini reddeder. Sunucu girişinin `koala_app` dışında üyeliği, `BYPASSRLS`/`CREATEROLE`/`CREATEDB`/`REPLICATION` özniteliği ya da kendi tablo yetkisi olamaz.
- Bootstrap girişi rollere bağlı bir DB'ye ihtiyaç duymaz; bakım DB'si `postgres`'e bağlanır. Grup rollerini ilk kez o kurmalıdır: PG16'dan beri var olan bir rolü değiştirmek (ör. `NOLOGIN`'e çekmek) o rolde `ADMIN` hakkı ister ve kuran giriş bu hakkı kendiliğinden alır.
- Veritabanı `OWNER koala_owner` ile açılır. `CONNECT` yalnız `koala_app` ve `koala_maint`'e verilir (migration `0001`); sahibi zaten bağlanabilir.
- Production'da her URL `sslmode=require`, `verify-ca` ya da `verify-full` taşır; yoksa komut açılmaz. Bir parametre URL'de iki kez yazılamaz.
- Sunucu girişi PgBouncer'ın transaction modundan geçmez: rol ve oturum ayarları bağlantıya bağlıdır.

## Yayın

`scripts/release-db.sh <imaj>` yeni imaj trafiğe açılmadan önce çalışır: önce `koala bootstrap`, sonra `koala migrate up`. Biri başarısız olursa yayın durur. `koala migrate down` production'da reddedilir; geri dönüş ileri bir migration ya da yedekten dönüşle yapılır.

## Haftalık bakım: River indeksleri

`koala maint reindex` River'ın iş tablosu indekslerini `REINDEX INDEX CONCURRENTLY` ile yeniden kurar. Yarıda kalmış bir önceki kurulumun bıraktığı geçersiz `_ccnew`/`_ccold` indekslerini önce düşürür. `KOALA_OWNER_DATABASE_URL` ile çalışır.

- Zamanlama: haftada bir, trafiğin az olduğu saatte, zamanlanmış ops işi olarak (Dokploy schedule). Sunucu süreci bu işi yapmaz; River'ın süreç içi reindexer'ı kapalıdır.
- Başarısızlıkta çıkış kodu sıfırdan farklıdır, hata log'dadır. İş bir sonraki hafta yeniden dener; art arda iki başarısızlık incelenir.

## Personel hesabı ve denetim kaydı (ADR-0026)

- İlk süper admin: `koala staff create --email <adres> --role OWNER`. Komut `KOALA_MAINT_DATABASE_URL` ile `koala_maint` olarak çalışır, tek kullanımlık parolayı yalnız bir kez stdout'a yazar; personel ilk girişte parolasını değiştirir. Uygulama rolü personel oluşturamaz.
- Hesabı kapatma: `koala staff disable --email <adres>`. Veritabanı `token_version`'ı artırır; hesabın bütün erişim ve refresh token'ları bir sonraki kullanımda ölür.
- Her iki komut denetim kaydına önce `PENDING`, sonra sonuç satırı yazar; rol ataması `authz_change_log`'a da düşer.
- `koala maint audit-purge` ayda bir zamanlanmış ops işi olarak koşar: saklama süresi dolan denetim ayrıntılarını (6 ay), denetim satırlarını (24 ay, finansal 10 yıl) ve PII erişim kaydını (24 ay) siler. Silmeye yalnız `koala_maint` ve yalnız süresi dolmuş satırlarda veritabanı tetikleyicisi izin verir.
