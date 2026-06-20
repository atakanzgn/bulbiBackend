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
| `CONTENT_PATH` | `data/content.json` | İlk seed dosyası (tablolar boşsa DB'ye aktarılır) |
| `FCM_CREDENTIALS` | (boş) | Firebase servis hesabı JSON yolu — verilmezse push kapalı |
| `FCM_PROJECT_ID` | (servis hesabından) | Firebase proje id |
| `PUSH_HOUR` | `10` | Günlük bildirim saati (sunucu saati) |
| `ADMIN_PASSWORD` | (boş) | `/admin` paneli parolası (kullanıcı `admin`) — boşsa panel kapalı |
| `REDIS_URL` | (boş) | `redis://:şifre@redis:6379/0` — boşsa in-memory yedek (tek instance için yeterli) |
| `RATE_LIMIT_PER_MIN` | `120` | IP başına dakikalık istek limiti (`/api/*`) |
| `MIN_APP_BUILD` | `1` | Splash'te gerekli minimum uygulama build numarası |
| `UPLOAD_DIR` | `data/uploads` | Bildirim görselleri klasörü (kalıcılık için volume bağla) |
| `PUBLIC_BASE_URL` | (boş) | Görsel URL tabanı (boşsa istek Host'undan); örn. `https://bulbi.atakanzgn.com.tr` |
| `UPLOAD_RETENTION_DAYS` | `30` | Yüklenen bildirim görsellerinin saklanma süresi (gün); eskiler otomatik silinir (`0` = kapalı) |

## API uçları

| Method | Yol | Açıklama |
|---|---|---|
| GET | `/healthz` | Sağlık kontrolü |
| GET | `/api/v1/content` | Güncel içerik paketi (`version`, `words`, `questions`) |
| GET | `/api/v1/content/version` | Sadece güncel sürüm (ucuz kontrol) |
| POST | `/api/v1/scores` | Skor gönder `{deviceId,name,puzzle,day,score}` → `{rank,score}` |
| GET | `/api/v1/leaderboard?puzzle=&day=&deviceId=&limit=` | İlk N + benim sıram |
| POST | `/api/v1/devices` | Push token kaydet `{deviceId,token,platform}` |

`puzzle` ∈ `{word, number, quiz}`. Ayrıca `GET /api/v1/app` → `{minBuild, contentVersion}` (splash sürüm kontrolü).

## Güvenlik & performans

- **IP rate limit:** `/api/*` için IP başına dakikalık limit (varsayılan 120) — aşılırsa `429`. Redis varsa Redis'te sayılır, yoksa in-memory.
- **Admin brute-force:** 15 dk içinde 5 hatalı şifre → o IP geçici bloklanır (`429`).
- **Gerçek IP:** Cloudflare turuncu bulut + NPM arkasında istemci IP'si `CF-Connecting-IP` (yoksa `X-Forwarded-For`) başlığından alınır.
- **İçerik cache:** `/api/v1/content` Redis'te cache'lenir; admin değişikliğinde geçersiz kılınır ve her gün **TR 00:00**'da yeniden ısıtılır.

## İçerik yönetimi — Admin paneli

İçerik (kelime + soru) **PostgreSQL**'de tutulur. İlk açılışta tablolar boşsa `data/content.json`'dan **seed** edilir; sonrası **`/admin`** panelinden yönetilir (uygulamada gömülü veri yoktur).

- `https://bulbi.atakanzgn.com.tr/admin` → kullanıcı `admin`, parola `ADMIN_PASSWORD`.
- Kelime ekle/sil (harf sayısı otomatik) ve soru ekle/sil (tür + zorluk seçimiyle).
- Her değişiklikte içerik sürümü otomatik artar; uygulama yeni içeriği indirir — **mağaza güncellemesi gerekmez.**

> NPM zaten tüm alan adını backend'e proxylediği için panel `https://<alan-adı>/admin` adresinden erişilebilir.

## Push bildirim (Firebase)

1. Firebase projesi + FCM. Servis hesabı anahtarını (JSON) sunucuya koy (`/root/bulbi/fcm.json`).
2. `FCM_CREDENTIALS` ve `FCM_PROJECT_ID` ver. Flutter token'ı `/api/v1/devices`'a yollar.

## CI/CD — her push'ta Docker Hub imajı

`.github/workflows/docker.yml` her `main` push'unda imajı derleyip **Docker Hub**'a `docker.io/<kullanıcı>/bulbibackend` olarak yollar (`latest` + kısa SHA).

GitHub repo → Settings → Secrets and variables → Actions'a iki secret ekle:
- `DOCKERHUB_USERNAME` — Docker Hub kullanıcı adın
- `DOCKERHUB_TOKEN` — Docker Hub → Account settings → Personal access tokens → **Read & Write** token

> Neden Docker Hub: ghcr.io bazı bölgelerden (TR) çok yavaş indiriyor; docker.io hızlı ve sorunsuz çekiliyor.

## Sunucuda deploy (docker compose + Nginx Proxy Manager)

Compose dosyası repoda **tutulmaz**; sunucuda (`/root/bulbi/docker-compose.yml`) sen oluşturursun. Container'lar NPM'in `proxy_net` ağına bağlanır; HTTPS/alan adını **Nginx Proxy Manager** yönetir (NPM'de Proxy Host: `bulbi.atakanzgn.com.tr` → `bulbi-backend:8080`, SSL'i NPM verir).

Örnek (sunucuda oluştur):

```yaml
services:
  backend:
    image: docker.io/<kullanıcı>/bulbibackend:latest
    container_name: bulbi-backend
    restart: unless-stopped
    environment:
      DATABASE_URL: "postgres://bulbi:SIFRE@db:5432/bulbi?sslmode=disable"
      REDIS_URL: "redis://redis:6379/0"
      ADMIN_PASSWORD: "<ADMIN_SIFRE>"
      FCM_CREDENTIALS: "/run/secrets/fcm.json"
      FCM_PROJECT_ID: "<firebase-proje-id>"
      PUSH_HOUR: "10"
      MIN_APP_BUILD: "1"
      PUBLIC_BASE_URL: "https://bulbi.atakanzgn.com.tr"
      TZ: "Europe/Istanbul"
    volumes:
      - /root/bulbi/fcm.json:/run/secrets/fcm.json:ro
      - bulbi-uploads:/app/data/uploads
    networks: [proxy_net, internal]
    depends_on: [db, redis]
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
  redis:
    image: redis:7-alpine
    container_name: bulbi-redis
    restart: unless-stopped
    networks: [internal]

networks:
  proxy_net:
    external: true   # NPM'in ağı
  internal:

volumes:
  bulbi-pgdata:
  bulbi-uploads:
```

Güncelleme:

```bash
cd /root/bulbi
docker compose pull
docker compose up -d
```
