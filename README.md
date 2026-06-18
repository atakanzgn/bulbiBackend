# Bulbi Backend

Bulbi mobil uygulamasının Go backend'i: **içerik servisi + global liderlik tablosu (PostgreSQL) + push bildirim (FCM)**. Tek statik binary (cgo yok).

Tasarım çevrimdışı-önceliklidir: uygulama içeriği indirip önbelleğe alır, internet yokken paketteki varsayılana düşer. Bulmacalar yine tarihten türetilir.

## Çalıştırma (yerel)

PostgreSQL gerekir. Örnek:

```bash
docker run -d --name bulbi-pg -e POSTGRES_USER=bulbi -e POSTGRES_PASSWORD=secret \
  -e POSTGRES_DB=bulbi -p 5432:5432 postgres:17-alpine

cd bulbi-backend
$env:DATABASE_URL="postgres://bulbi:secret@localhost:5432/bulbi?sslmode=disable"  # PowerShell
go run ./cmd/server
```

Varsayılan adres: `:8080`. İçerik: `data/content.json`.

## Ortam değişkenleri

| Değişken | Varsayılan | Açıklama |
|---|---|---|
| `ADDR` | `:8080` | Dinlenecek adres |
| `DATABASE_URL` | **(zorunlu)** | `postgres://kullanıcı:şifre@host:5432/bulbi?sslmode=disable` |
| `CONTENT_PATH` | `data/content.json` | İçerik dosyası |
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

`data/content.json` içine kelime/soru ekle, `version` alanını artır, push'la → CI yeni imaj üretir → sunucuda `docker pull` + yeniden başlat. İçerik imaja gömülüdür; uygulama sürüm değişimini görünce yeni paketi indirir, **mağaza güncellemesi gerekmez.**

## Push bildirim (Firebase)

1. Firebase projesi + FCM. Servis hesabı anahtarını (JSON) sunucuya koy (`/root/bulbi/fcm.json`).
2. `FCM_CREDENTIALS` ve `FCM_PROJECT_ID` ver. Flutter token'ı `/api/v1/devices`'a yollar.

## CI/CD — her push'ta GHCR imajı

`.github/workflows/docker.yml` her `main` push'unda imajı derleyip **GHCR**'a `ghcr.io/atakanzgn/bulbibackend` olarak yollar (`latest` + kısa SHA). Ekstra secret gerekmez (`GITHUB_TOKEN`).

> Paket ilk başta **private** gelir. Sunucudan çekmek için ya paketi public yap (GitHub → Packages → Package settings → Change visibility) ya da sunucuda `docker login ghcr.io` yap.

## Sunucuda deploy (docker compose + Nginx Proxy Manager)

Compose dosyası repoda **tutulmaz**; sunucuda (`/root/bulbi/docker-compose.yml`) sen oluşturursun. Container'lar NPM'in `proxy_net` ağına bağlanır; HTTPS/alan adını **Nginx Proxy Manager** yönetir (NPM'de Proxy Host → forward: `bulbi-backend` port `8080`).

Örnek (sunucuda oluştur):

```yaml
services:
  backend:
    image: ghcr.io/atakanzgn/bulbibackend:latest
    container_name: bulbi-backend
    restart: unless-stopped
    environment:
      DATABASE_URL: "postgres://bulbi:SIFRE@db:5432/bulbi?sslmode=disable"
      FCM_CREDENTIALS: "/run/secrets/fcm.json"
      FCM_PROJECT_ID: "<firebase-proje-id>"
      PUSH_HOUR: "10"
    volumes:
      - /root/bulbi/fcm.json:/run/secrets/fcm.json:ro
    networks: [proxy_net, internal]
    depends_on: [db]
  db:
    image: postgres:17-alpine
    container_name: bulbi-db
    restart: unless-stopped
    environment:
      POSTGRES_USER: bulbi
      POSTGRES_PASSWORD: SIFRE
      POSTGRES_DB: bulbi
    volumes:
      - bulbi-pgdata:/var/lib/postgresql/data
    networks: [internal]

networks:
  proxy_net:
    external: true   # NPM'in ağı
  internal:

volumes:
  bulbi-pgdata:
```

Güncelleme:

```bash
cd /root/bulbi
docker compose pull
docker compose up -d
```
