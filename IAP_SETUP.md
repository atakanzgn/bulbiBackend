# Bulbi — Uygulama İçi Coin Satın Alma (IAP) Kurulum & Güvenlik Rehberi

Bu rehber, App Store ve Google Play üzerinden **coin satın almayı** ve bunun
**sunucu taraflı güvenli doğrulamasını** açıklar.

## Güvenlik modeli (önemli)

- İstemci bir satın alma yaptığında coin'i **kendisi vermez**. Makbuz/token
  backend'e gönderilir → backend **Apple/Google ile doğrular** → işlemi
  **idempotent** kaydeder → istemciye **kaç coin verileceğini** söyler.
- Coin miktarı **sunucudaki katalogda** bellidir (`productCoins`,
  `internal/server/server.go`). İstemci miktar gönderemez/değiştiremez.
- Aynı işlem (`transaction_id`) **iki kez coin kazandırmaz** (`purchases` tablosu,
  `ON CONFLICT DO NOTHING`).
- **Güvenli varsayılan:** İlgili platform için doğrulama yapılandırılmamışsa
  (paylaşılan sır / servis hesabı yoksa) istek **reddedilir** (503), coin
  verilmez. Yani yanlışlıkla "doğrulamasız coin" mümkün değildir.

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
# iOS (App Store paylaşılan sırrı — App Store Connect > App > App-Specific Shared Secret)
APPSTORE_SHARED_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

# Android (Play Developer API servis hesabı JSON dosya yolu)
PLAY_SERVICE_ACCOUNT=/run/secrets/play-sa.json
ANDROID_PACKAGE_NAME=com.bulbi.app   # uygulamanın paket adı
```

- En az biri verilirse IAP etkinleşir; verilmeyen platform `ErrNotConfigured`
  ile reddedilir.
- Secret/JSON dosyalarını **repoya koyma** (zaten `*service-account*.json`
  gitignore'da). FCM JSON'unu mount ettiğin yöntemle (Docker secret/volume) aynı
  şekilde mount et.

### iOS paylaşılan sırrı
App Store Connect → **Users and Access → Integrations → In-App Purchase** (veya
App → App Information) → **App-Specific Shared Secret** üret → `APPSTORE_SHARED_SECRET`.

> Bu rehber **legacy `verifyReceipt`** uç noktasını kullanır (basit, paylaşılan
> sır yeterli, sandbox'a otomatik düşer). Apple bunu uzun vadede App Store Server
> API (StoreKit 2, JWS) lehine bırakıyor; ileride geçmek istersen `internal/iap`
> içindeki `VerifyApple`'ı değiştirebilirsin.

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
        → iOS: verifyReceipt | Android: Play products.get  (gerçek doğrulama)
        → purchases tablosuna idempotent kayıt
        → { granted: <coin> }  (tekrar ise granted=0)
    → GameStorage.addCoins(granted) + completePurchase
```

## 5) Test kontrol listesi

- [ ] Ürünler her iki mağazada da `coins_100/500/1200` kimliğiyle **onaylı**.
- [ ] Backend env'leri ayarlı; loglarda `iap: etkin` görünüyor.
- [ ] Sandbox/internal-test satın alma → coin geliyor.
- [ ] Aynı satın almayı tekrar doğrulatınca **coin tekrar gelmiyor** (idempotent).
- [ ] Geçersiz/sahte makbuz → coin **gelmiyor** (400).
- [ ] Env'siz sunucu → istek **503**, coin yok (güvenli varsayılan).
```
