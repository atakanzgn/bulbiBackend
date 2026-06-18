# Bulbi Backend

Bulbi mobil uygulamasının Go backend'i: **içerik servisi + global liderlik tablosu + push bildirim**. Tek statik binary, cgo gerektirmez (SQLite için `modernc.org/sqlite`).

Tasarım çevrimdışı-önceliklidir: uygulama içeriği indirip önbelleğe alır, internet yokken paketteki varsayılana düşer. Bulmacalar yine tarihten türetilir.

## Çalıştırma

```bash
cd bulbi-backend
go run ./cmd/server
# veya derle:
go build -o bulbi-backend ./cmd/server
./bulbi-backend
```

Varsayılan adres: `:8080`. İçerik `data/content.json`, veritabanı `data/bulbi.db`.

## Ortam değişkenleri

| Değişken | Varsayılan | Açıklama |
|---|---|---|
| `ADDR` | `:8080` | Dinlenecek adres |
| `DATA_DIR` | `data` | İçerik + db klasörü |
| `CONTENT_PATH` | `data/content.json` | İçerik dosyası |
| `DB_PATH` | `data/bulbi.db` | SQLite dosyası |
| `FCM_CREDENTIALS` | (boş) | Firebase servis hesabı JSON yolu — verilmezse push kapalı |
| `FCM_PROJECT_ID` | (servis hesabından) | Firebase proje id |
| `PUSH_HOUR` | `10` | Günlük bildirim saati (sunucu saati) |

## API uçları

| Method | Yol | Açıklama |
|---|---|---|
| GET | `/healthz` | Sağlık kontrolü |
| GET | `/api/v1/content` | Güncel içerik paketi (`version`, `words`, `questions`) |
| GET | `/api/v1/content/version` | Sadece güncel sürüm (ucuz kontrol) |
| POST | `/api/v1/scores` | Skor gönder `{deviceId,name,puzzle,day,score}` → `{rank,score}` |
| GET | `/api/v1/leaderboard?puzzle=&day=&deviceId=&limit=` | İlk N + benim sıram |
| POST | `/api/v1/devices` | Push token kaydet `{deviceId,token,platform}` |

`puzzle` ∈ `{word, number, quiz}`.

## İçeriği büyütme

`data/content.json` içine kelime/soru ekle, `version` alanını artır. Uygulama sürüm değişimini görünce yeni paketi indirir — **uygulama güncellemesi gerekmez.**

## Push bildirim (Firebase)

1. Firebase projesi oluştur, FCM'i etkinleştir.
2. Servis hesabı anahtarı (JSON) indir, sunucuya koy, `FCM_CREDENTIALS` ile yolunu ver.
3. Flutter tarafında `firebase_messaging` ekle, token'ı `/api/v1/devices`'a gönder.

## Deploy (özet)

Tek binary'yi sunucuya kopyala, `systemd` servisi olarak çalıştır, önüne `Caddy` koy (otomatik HTTPS) ve alan adına bağla:

```
api.alanadin.com {
    reverse_proxy localhost:8080
}
```
