// Command demo fills a fresh prototype with a small demo store so listings,
// search and product pages have something to show: one approved seller and
// about two dozen products with generated photos. Brands are fictional.
// It runs once; later runs do nothing. Full demo data arrives with T6.5.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

const demoPhone = "+919999900099"

type variant struct {
	sku, colour string
	mrp, price  int64 // rupees
	stock       int32
}

type demo struct {
	cat, shape     string
	hue            float64
	name, brand    string
	local          bool
	desc, warranty string
	specs          map[string]any
	variants       []variant
}

var products = []demo{
	{"smartphones", "phone", 248, "Nova 5G (8 GB, 128 GB)", "Nova", true, "A bright 120 Hz AMOLED phone with a 5000 mAh battery and 45 W charging, assembled in India.", "1 year manufacturer warranty",
		map[string]any{"os": "Android", "ram_gb": 8.0, "storage_gb": 128.0, "display_in": 6.67, "refresh_hz": "120", "main_camera_mp": 50.0, "battery_mah": 5000.0, "charging_w": 45.0, "has_5g": true, "sim": "Dual SIM"},
		[]variant{{"NOVA5G-8-128-AUR", "Aurora", 24999, 18999, 40}, {"NOVA5G-8-128-GRA", "Graphite", 24999, 18999, 25}}},
	{"smartphones", "phone", 262, "Nova Lite 5G (4 GB, 64 GB)", "Nova", true, "An everyday 5G phone with a big screen and a two-day battery, made for first-time smartphone buyers.", "1 year manufacturer warranty",
		map[string]any{"os": "Android", "ram_gb": 4.0, "storage_gb": 64.0, "display_in": 6.5, "refresh_hz": "90", "main_camera_mp": 13.0, "battery_mah": 6000.0, "charging_w": 18.0, "has_5g": true, "sim": "Dual SIM"},
		[]variant{{"NOVALITE-4-64-BLU", "Ocean", 11999, 8999, 60}}},
	{"smartphones", "phone", 210, "Zenith Pro (12 GB, 256 GB)", "Zenith", false, "A flagship with a 200 MP camera, 144 Hz LTPO display and 100 W charging that fills up in 20 minutes.", "1 year manufacturer warranty",
		map[string]any{"os": "Android", "ram_gb": 12.0, "storage_gb": 256.0, "display_in": 6.78, "refresh_hz": "144", "main_camera_mp": 200.0, "battery_mah": 5100.0, "charging_w": 100.0, "has_5g": true, "sim": "Dual SIM + eSIM"},
		[]variant{{"ZENPRO-12-256-SLV", "Silver", 69999, 59999, 12}, {"ZENPRO-12-512-SLV", "Silver 512 GB", 79999, 69999, 0}}},
	{"smartphones", "phone", 330, "Orbit Mini (6 GB, 128 GB)", "Orbit", false, "A compact 6.1-inch phone that fits in one hand, with stereo speakers and an IP68 rating.", "1 year manufacturer warranty",
		map[string]any{"os": "Android", "ram_gb": 6.0, "storage_gb": 128.0, "display_in": 6.1, "refresh_hz": "120", "main_camera_mp": 50.0, "battery_mah": 4200.0, "charging_w": 33.0, "has_5g": true, "sim": "Dual SIM"},
		[]variant{{"ORBITMINI-6-128-PNK", "Blush", 34999, 29999, 18}}},
	{"tablets", "tablet", 200, "Lumen Tab 11", "Lumen", false, "An 11-inch 2.5K tablet with quad speakers and stylus support for notes, drawing and films.", "1 year manufacturer warranty",
		map[string]any{"os": "Android", "display_in": 11.0, "ram_gb": 8.0, "storage_gb": 128.0, "connectivity": "Wi-Fi", "stylus": true},
		[]variant{{"LUMENTAB11-8-128", "Grey", 27999, 22999, 20}}},
	{"laptops", "laptop", 214, "Aero 14 Ultrabook", "Aero", false, "A 1.2 kg 14-inch laptop with a 2.8K OLED screen, all-day battery and a backlit keyboard.", "1 year onsite warranty",
		map[string]any{"processor": "Intel Core Ultra 7 155H", "processor_brand": "Intel", "ram_gb": 16.0, "storage_type": "SSD", "storage_gb": 1024.0, "dedicated_gpu": false, "display_in": 14.0, "resolution": "3K", "weight_kg": 1.2, "os": "Windows", "battery_h": 14.0},
		[]variant{{"AERO14-16-1T", "Silver", 104999, 89999, 8}}},
	{"laptops", "laptop", 280, "Volt Strike 16 Gaming", "Volt", false, "A 16-inch gaming laptop with a 240 Hz display, RTX graphics and a cooling system that stays quiet.", "2 year warranty",
		map[string]any{"processor": "AMD Ryzen 9 8945HS", "processor_brand": "AMD", "ram_gb": 32.0, "storage_type": "SSD", "storage_gb": 1024.0, "dedicated_gpu": true, "display_in": 16.0, "resolution": "2.5K", "weight_kg": 2.4, "os": "Windows", "battery_h": 6.0},
		[]variant{{"VOLT16-32-1T", "Black", 159999, 139999, 5}}},
	{"laptops", "laptop", 160, "Kosmo Book 15 (Made in India)", "Kosmo", true, "A sturdy 15.6-inch laptop for study and office work, designed and assembled in Noida.", "1 year manufacturer warranty",
		map[string]any{"processor": "Intel Core i5-1335U", "processor_brand": "Intel", "ram_gb": 8.0, "storage_type": "SSD", "storage_gb": 512.0, "dedicated_gpu": false, "display_in": 15.6, "resolution": "Full HD", "weight_kg": 1.7, "os": "Windows", "battery_h": 9.0},
		[]variant{{"KOSMO15-8-512", "Slate", 54999, 42999, 30}}},
	{"monitors", "tv", 192, "Lumen View 27 QHD", "Lumen", false, "A 27-inch 2K IPS monitor at 165 Hz with USB-C power delivery, for work by day and games by night.", "3 year warranty",
		map[string]any{"size_in": 27.0, "resolution": "2K QHD", "refresh_hz": 165.0, "panel": "IPS", "ports": []any{"HDMI", "DisplayPort", "USB-C"}, "hdr": true},
		[]variant{{"LUMEN27Q", "Black", 29999, 21999, 14}}},
	{"tws-earbuds", "buds", 276, "Pulse Buds ANC", "Pulse", true, "Earbuds with 45 dB noise cancellation, 40 hours with the case and a low-latency mode for games.", "1 year manufacturer warranty",
		map[string]any{"anc": true, "battery_total_h": 40.0, "ip_rating": "IPX5", "codec": []any{"SBC", "AAC", "LDAC"}, "low_latency": true},
		[]variant{{"PULSEBUDS-BLK", "Midnight", 4999, 2499, 120}, {"PULSEBUDS-WHT", "Pearl", 4999, 2499, 80}}},
	{"tws-earbuds", "buds", 300, "Orbit Air", "Orbit", false, "Light open-fit earbuds with clear calls and 30 hours of music.", "6 month warranty",
		map[string]any{"anc": false, "battery_total_h": 30.0, "ip_rating": "IPX4", "codec": []any{"SBC", "AAC"}},
		[]variant{{"ORBITAIR-WHT", "White", 2999, 1499, 200}}},
	{"headphones", "headphones", 260, "Zenith Studio Wireless", "Zenith", false, "Over-ear headphones with adaptive noise cancelling, 60-hour battery and plush memory-foam cups.", "1 year manufacturer warranty",
		map[string]any{"type": "Over-ear", "connectivity": "Wireless", "anc": true, "battery_h": 60.0, "driver_mm": 40.0, "codec": []any{"SBC", "AAC", "LDAC"}},
		[]variant{{"ZENSTUDIO-BLK", "Black", 19999, 13999, 25}}},
	{"headphones", "headphones", 18, "Pulse Bass Neckband", "Pulse", true, "A neckband with punchy bass, magnetic buds and 30 hours of playback.", "1 year manufacturer warranty",
		map[string]any{"type": "Neckband", "connectivity": "Wireless", "anc": false, "battery_h": 30.0, "driver_mm": 12.0, "codec": []any{"SBC"}},
		[]variant{{"PULSENECK-RED", "Red", 2499, 1199, 150}}},
	{"bluetooth-speakers", "speaker", 192, "Volt Boom 40", "Volt", true, "A 40 W waterproof party speaker with RGB lights and 20 hours of play.", "1 year manufacturer warranty",
		map[string]any{"output_w": 40.0, "battery_h": 20.0, "ip_rating": "IP67", "bt_version": "5.3"},
		[]variant{{"VOLTBOOM40", "Black", 7999, 4999, 45}}},
	{"bluetooth-speakers", "speaker", 40, "Orbit Pebble Mini", "Orbit", false, "A pocket speaker with surprisingly full sound and a clip for your bag.", "6 month warranty",
		map[string]any{"output_w": 5.0, "battery_h": 12.0, "ip_rating": "IPX5", "bt_version": "5.3"},
		[]variant{{"PEBBLEMINI-YEL", "Sunshine", 1999, 999, 0}}},
	{"smartwatches", "watch", 330, "Pulse Fit Pro", "Pulse", true, "A 1.96-inch AMOLED smartwatch with Bluetooth calling, SpO2 and 10-day battery.", "1 year manufacturer warranty",
		map[string]any{"display_in": 1.96, "display_type": "AMOLED", "battery_days": 10.0, "gps": false, "bt_calling": true, "sensors": []any{"Heart rate", "SpO2", "Sleep"}, "compatibility": []any{"Android", "iOS"}},
		[]variant{{"PULSEFIT-BLK", "Black strap", 5999, 2999, 90}, {"PULSEFIT-ORG", "Orange strap", 5999, 2999, 40}}},
	{"smartwatches", "watch", 214, "Zenith Watch GT", "Zenith", false, "A titanium GPS watch with a sapphire screen, ECG and two weeks of battery.", "1 year manufacturer warranty",
		map[string]any{"display_in": 1.43, "display_type": "AMOLED", "battery_days": 14.0, "gps": true, "bt_calling": true, "sensors": []any{"Heart rate", "SpO2", "ECG", "Sleep"}, "compatibility": []any{"Android", "iOS"}},
		[]variant{{"ZENWATCHGT", "Titanium", 24999, 19999, 15}}},
	{"smart-tvs", "tv", 248, "Lumen Vision 55\" 4K QLED", "Lumen", false, "A 55-inch QLED TV with Dolby Vision, 120 Hz and Google TV built in.", "2 year warranty on panel",
		map[string]any{"size_in": 55.0, "resolution": "4K UHD", "panel": "QLED", "smart_os": "Google TV", "refresh_hz": "120", "hdr": []any{"HDR10", "Dolby Vision"}, "hdmi_ports": 4.0},
		[]variant{{"LUMEN55Q", "Black", 64999, 44999, 9}}},
	{"smart-tvs", "tv", 165, "Kosmo Smart 43\" Full HD", "Kosmo", true, "A 43-inch Full HD smart TV made in India, with all the streaming apps and a voice remote.", "1 year manufacturer warranty",
		map[string]any{"size_in": 43.0, "resolution": "Full HD", "panel": "LED", "smart_os": "Android TV", "refresh_hz": "60", "hdr": []any{"HDR10"}, "hdmi_ports": 3.0},
		[]variant{{"KOSMO43FHD", "Black", 24999, 17999, 22}}},
	{"power-banks", "brick", 52, "Volt Cell 20000", "Volt", true, "A 20,000 mAh power bank with 65 W USB-C that can charge a laptop.", "6 month warranty",
		map[string]any{"capacity_mah": 20000.0, "output_w": 65.0, "ports": []any{"USB-C", "USB-A"}, "fast_charge": []any{"PD", "QC"}, "weight_g": 380.0},
		[]variant{{"VOLTCELL20K", "Graphite", 4499, 2799, 70}}},
	{"power-banks", "brick", 140, "Orbit Slim 10000", "Orbit", false, "A slim 10,000 mAh pack with 22.5 W fast charging that slips into a pocket.", "6 month warranty",
		map[string]any{"capacity_mah": 10000.0, "output_w": 22.5, "ports": []any{"USB-C", "USB-A"}, "fast_charge": []any{"PD"}, "weight_g": 210.0},
		[]variant{{"ORBITSLIM10K", "Mint", 1999, 1099, 110}}},
	{"development-boards", "board", 2, "Kosmo Maker Board S3", "Kosmo", true, "A Wi-Fi and Bluetooth development board with 45 GPIO pins, designed in Bengaluru for makers and students.", "6 month warranty",
		map[string]any{"chip": "ESP32-S3", "clock_mhz": 240.0, "ram": "512 KB SRAM + 8 MB PSRAM", "gpio": 45.0, "wireless": []any{"Wi-Fi", "Bluetooth"}},
		[]variant{{"KOSMOS3", "Blue", 899, 649, 300}}},
	{"robot-cleaners", "round", 165, "Aero Clean R5", "Aero", false, "A LiDAR robot vacuum and mop with 5000 Pa suction and a self-emptying dock.", "1 year manufacturer warranty",
		map[string]any{"suction_pa": 5000.0, "runtime_min": 180.0, "mapping": "LiDAR", "mop": true, "auto_empty": true},
		[]variant{{"AEROR5", "White", 39999, 29999, 6}}},
}

func main() {
	log := logx.New("demo")
	cfg, err := config.Load()
	if err != nil {
		fail(log, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fail(log, err)
	}
	defer pool.Close()
	q := store.New(pool)
	if _, err := q.GetAccountByPhoneRole(ctx, store.GetAccountByPhoneRoleParams{PhoneE164: ptr(demoPhone), Role: store.AccountRoleSELLER}); err == nil {
		log.Info("demo store already present; nothing to do")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		fail(log, err)
	}
	media, err := storage.NewS3(ctx, storage.S3Config{
		Endpoint: cfg.StorageEndpoint, AccessKey: cfg.StorageAccessKey, SecretKey: cfg.StorageSecretKey,
		Bucket: cfg.StorageBucketMedia, Region: cfg.StorageRegion, UseSSL: cfg.StorageUseSSL,
	})
	if err != nil {
		fail(log, err)
	}
	seller, _ := uuid.NewV7()
	now := time.Now()
	if _, err := q.CreateAccount(ctx, store.CreateAccountParams{
		ID: seller, Role: store.AccountRoleSELLER, PhoneE164: ptr(demoPhone), PhoneVerifiedAt: &now,
		Name: "Siliconics Demo Store", Approval: store.ApprovalStatusAPPROVED,
	}); err != nil {
		fail(log, err)
	}
	// The pickup address lets testers ship demo orders through sim-ship.
	if _, err := pool.Exec(ctx, `INSERT INTO seller_profiles (account_id, business_type, legal_name, store_name, verified_tag, submitted_at, decided_at, pickup_address)
		VALUES ($1, 'PRIVATE_LIMITED', 'Siliconics Demo Store Pvt Ltd', 'Siliconics Demo Store', true, now(), now(),
		'{"line1":"Plot 4, Sector 63","city":"Noida","state":"Uttar Pradesh","pincode":"201301","phone":"9999900099"}')`, seller); err != nil {
		fail(log, err)
	}
	svc := &product.Service{Pool: pool, Q: q, Store: media, Log: log}
	for _, d := range products {
		if err := add(ctx, pool, svc, seller, d); err != nil {
			fail(log, fmt.Errorf("%s: %w", d.name, err))
		}
	}
	log.Info("demo store ready", "products", len(products), "seller_phone", demoPhone)
}

func add(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, svc *product.Service, seller uuid.UUID, d demo) error {
	var cat uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM categories WHERE slug = $1`, d.cat).Scan(&cat); err != nil {
		return err
	}
	v, err := svc.Create(ctx, seller, product.CreateInput{CategoryID: cat, Name: d.name, Brand: d.brand})
	if err != nil {
		return err
	}
	if v, err = svc.Update(ctx, seller, v.ID, product.UpdateInput{
		CategoryID: cat, Name: d.name, Brand: d.brand, Model: d.brand + "-" + v.Slug[len(v.Slug)-6:], Description: d.desc,
		Warranty: d.warranty, OriginCountry: "IN", IsLocal: d.local, Specs: d.specs, Version: v.Version,
	}); err != nil {
		return err
	}
	vars := make([]product.VariantInput, len(d.variants))
	for i, x := range d.variants {
		vars[i] = product.VariantInput{SKU: x.sku, Attrs: map[string]string{"Colour": x.colour}, MrpPaise: x.mrp * 100, PricePaise: x.price * 100, Stock: x.stock}
	}
	if _, err = svc.SaveVariants(ctx, seller, v.ID, vars); err != nil {
		return err
	}
	var imgs []product.ImageInput
	for i, shift := range []float64{0, 35} {
		up, err := svc.UploadImage(ctx, seller, picture(d.shape, d.hue+shift, i))
		if err != nil {
			return err
		}
		imgs = append(imgs, product.ImageInput{UploadID: up.ID, Alt: fmt.Sprintf("%s, view %d", d.name, i+1)})
	}
	if _, err = svc.SaveImages(ctx, seller, v.ID, imgs); err != nil {
		return err
	}
	_, err = svc.SetStatus(ctx, seller, v.ID, "ACTIVE")
	return err
}

// picture draws a device silhouette on a soft gradient in the category hue.
func picture(shape string, hue float64, view int) []byte {
	const n = 900
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	inShape := shapeFn(shape, view)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)/n, float64(y)/n
			t := (fx + fy) / 2
			c := hsl(hue+40*t, 0.55, 0.93-0.1*t)
			if d, edge := inShape(fx, fy); d {
				c = hsl(hue, 0.6, 0.32+0.18*(1-fy))
				if edge {
					c = hsl(hue, 0.5, 0.75)
				}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func shapeFn(shape string, view int) func(x, y float64) (bool, bool) {
	rr := func(cx, cy, w, h, r float64) func(x, y float64) (bool, bool) {
		return func(x, y float64) (bool, bool) {
			dx := math.Max(math.Abs(x-cx)-(w/2-r), 0)
			dy := math.Max(math.Abs(y-cy)-(h/2-r), 0)
			d := math.Hypot(dx, dy)
			return d <= r, d > r-0.008 && d <= r
		}
	}
	circle := func(cx, cy, r float64) func(x, y float64) (bool, bool) {
		return func(x, y float64) (bool, bool) {
			d := math.Hypot(x-cx, y-cy)
			return d <= r, d > r-0.008 && d <= r
		}
	}
	union := func(fs ...func(x, y float64) (bool, bool)) func(x, y float64) (bool, bool) {
		return func(x, y float64) (bool, bool) {
			for _, f := range fs {
				if in, e := f(x, y); in {
					return in, e
				}
			}
			return false, false
		}
	}
	tilt := float64(view) * 0.06
	switch shape {
	case "phone":
		return rr(0.5+tilt, 0.5, 0.34, 0.66, 0.06)
	case "tablet":
		return rr(0.5, 0.5, 0.56+tilt, 0.7, 0.05)
	case "laptop":
		return union(rr(0.5, 0.44, 0.62, 0.4, 0.03), rr(0.5, 0.68, 0.76, 0.05, 0.02))
	case "tv":
		return union(rr(0.5, 0.45, 0.74, 0.44, 0.02), rr(0.5, 0.74, 0.22, 0.04, 0.02))
	case "buds":
		return union(circle(0.4-tilt, 0.5, 0.13), circle(0.6+tilt, 0.5, 0.13))
	case "headphones":
		return union(circle(0.3, 0.6, 0.12), circle(0.7, 0.6, 0.12), rr(0.5, 0.33, 0.48, 0.06, 0.03))
	case "speaker":
		return rr(0.5, 0.5, 0.36+tilt, 0.56, 0.16)
	case "watch":
		return union(rr(0.5, 0.5, 0.32, 0.38, 0.08), rr(0.5, 0.5, 0.18, 0.8, 0.06))
	case "brick":
		return rr(0.5, 0.5, 0.4, 0.6, 0.07)
	case "board":
		return rr(0.5, 0.5, 0.62, 0.38, 0.03)
	default:
		return circle(0.5, 0.5, 0.3-tilt/2)
	}
}

func hsl(h, s, l float64) color.RGBA {
	h = math.Mod(h, 360) / 360
	q := l * (1 + s)
	if l >= 0.5 {
		q = l + s - l*s
	}
	p := 2*l - q
	conv := func(t float64) uint8 {
		t = math.Mod(t+1, 1)
		var v float64
		switch {
		case t < 1.0/6:
			v = p + (q-p)*6*t
		case t < 0.5:
			v = q
		case t < 2.0/3:
			v = p + (q-p)*(2.0/3-t)*6
		default:
			v = p
		}
		return uint8(math.Round(v * 255))
	}
	return color.RGBA{conv(h + 1.0/3), conv(h), conv(h - 1.0/3), 255}
}

func ptr(s string) *string { return &s }

func fail(log interface{ Error(string, ...any) }, err error) {
	log.Error("demo", "err", err)
	os.Exit(1)
}
