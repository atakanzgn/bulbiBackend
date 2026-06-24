# Bulbi — Uygulama İçi Coin Satın Alma (IAP) Kurulum & Güvenlik Rehberi

Bu rehber, App Store ve Google Play üzerinden **coin satın almayı** ve bunun
**sunucu taraflı güvenli doğrulamasını** açıklar.

## Güvenlik modeli (önemli)

- İstemci bir satın alma yaptığında coin'i **kendisi vermez**. Makbuz/token
  backend'e gönderilir → backend **doğrular** (iOS: StoreKit 2 JWS imzasını
  çevrimdışı; Android: Play API) → işlemi **idempotent** kaydeder → istemciye
  **kaç coin verileceğini** söyler.
- Coin miktarı **sunucudaki katalogda** bellidir (`productCoins`,
  `internal/server/server.go`). İstemci miktar gönderemez/değiştiremez.
- Aynı işlem (`transaction_id`) **iki kez coin kazandırmaz** (`purchases` tablosu,
  `ON CONFLICT DO NOTHING`).
- **iOS doğrulaması her zaman açıktır:** gömülü **Apple Root CA G3** ile JWS
  imzası ve sertifika zinciri çevrimdışı denetlenir (Apple'a ağ isteği / gizli
  anahtar gerekmez). **Android**, servis hesabı yoksa **reddedilir** (503). Yani
  yanlışlıkla "doğrulamasız coin" mümkün değildir.

> Not: Oyun-içi kazanılan (ücretsiz) coin'ler istemci tarafında tutulur — bunlar
> tek-oyunculu ve düşük riskli. Sunucu doğrulaması yalnızca **parayla alınan**
> coin'leri korur (sahte makbuzla coin alınamaz).

## 1) Ürünleri tanımla (productId'ler EŞLEŞMELİ)

Aşağıdaki **tüketilebilir (consumable)** ürünleri hem App Store Connect hem Play
Console'da **birebir aynı** kimlikle oluştur:

| productId    | Coin  | Not          |
|--------------|-------|--------------|
| `coins_100`  | 100   | —            |
| `coins_500`  | 550   | %10 bonus    |
| `coins_1200` | 1400  | ~%17 bonus   |

- **App Store Connect:** Features → In-App Purchases → **Consumable**.
- **Play Console:** Monetize → Products → **In-app products** → tür: tüketilebilir.
- Fiyatları kendi belirlersin; uygulama fiyatı mağazadan (`product.price`) okur.
- Coin miktarını değiştirmek/ürün eklemek için **sunucudaki** `productCoins`
  haritasını ve uygulamadaki `kCoinProducts`'ı (`lib/services/iap_service.dart`)
  birlikte güncelle.

## 2) Backend ortam değişkenleri

`internal/server` IAP doğrulamasını şu env'lerle açar (hiçbiri yoksa IAP kapalı):

```
# iOS — gizli anahtar/secret GEREKMEZ. (İsteğe bağlı ama önerilir) bundle id pinle:
IOS_BUNDLE_ID=com.atakanzgn.bulbi

# Android (Play Developer API servis hesabı JSON dosya yolu)
PLAY_SERVICE_ACCOUNT=/run/secrets/play-sa.json
ANDROID_PACKAGE_NAME=com.atakanzgn.bulbi   # uygulamanın paket adı
```

- **iOS her zaman açık** (gömülü Apple Root CA G3). `IOS_BUNDLE_ID` verirsen
  makbuzdaki bundle id de denetlenir — başka uygulamanın makbuzu kabul edilmez.
- **Android**, `PLAY_SERVICE_ACCOUNT` yoksa `ErrNotConfigured` ile reddedilir.
- Servis hesabı JSON'unu **repoya koyma** (zaten gitignore'da). FCM JSON'unu
  mount ettiğin yöntemle (Docker secret/volume) aynı şekilde mount et.

### iOS (StoreKit 2 — anahtar/ağ gerekmez)
Uygulama StoreKit 2 kullanır; satın almada **imzalı bir JWS** gönderir. Backend
bunu **çevrimdışı** doğrular: `internal/iap/storekit2.go` içindeki gömülü **Apple
Root CA G3** ile sertifika zincirini ve **ES256** imzasını denetler. Apple'a istek
atılmaz, paylaşılan sır gerekmez (`APPSTORE_SHARED_SECRET` **artık kullanılmıyor**).

### Android servis hesabı
1. Play Console → **Setup → API access** → bir Google Cloud projesi bağla.
2. Cloud Console → **IAM → Service Accounts** → yeni servis hesabı → JSON anahtarı
   indir (bu dosya `PLAY_SERVICE_ACCOUNT`).
3. Play Console → API access → bu servis hesabına **"View financial data /
   Manage orders"** (en azından satın almaları görme) yetkisini ver.
4. `androidpublisher` API'si Cloud projesinde **etkin** olmalı.

## 3) Uygulama tarafı

- `in_app_purchase` paketi eklendi (pubspec).
- `IapService` (`lib/services/iap_service.dart`): ürünleri sorgular, satın alır,
  `purchaseStream`'i dinler; satın almada makbuzu `ApiClient.verifyPurchase` ile
  backend'e gönderir, dönen `granted` kadar coin ekler, sonra `completePurchase`.
- Dükkan ekranında (`shop_screen.dart`) "Coin paketleri" bölümü ürünleri listeler.
- iOS: `ios/Runner` için **StoreKit** otomatik gelir; **Sandbox** test kullanıcısı
  ile dene (Settings → App Store → Sandbox Account).
- Android: uygulamayı **internal testing** track'ine yükle, lisanslı test
  hesabıyla dene (gerçek kart çekilmez).

## 4) Akış (özet)

```
Kullanıcı "Satın al" → mağaza ödeme alır
  → in_app_purchase purchaseStream(purchased)
    → ApiClient.verifyPurchase(platform, productId, receipt/token)
      → POST /api/v1/iap/verify
        → iOS: StoreKit2 JWS imza/zincir | Android: Play products.get  (doğrulama)
        → purchases tablosuna idempotent kayıt
        → { granted: <coin> }  (tekrar ise granted=0)
    → GameStorage.addCoins(granted) + completePurchase
```

## 5) Test kontrol listesi

- [ ] Ürünler her iki mağazada da `coins_100/500/1200` kimliğiyle **onaylı**.
- [ ] Loglarda `iap: iOS(StoreKit2) ...` görünüyor.
- [ ] Sandbox/internal-test satın alma → coin geliyor.
- [ ] Aynı satın almayı tekrar doğrulatınca **coin tekrar gelmiyor** (idempotent).
- [ ] Geçersiz/sahte JWS → coin **gelmiyor** (400).
- [ ] (Android) servis hesabı yoksa → istek **503**, coin yok.
```
