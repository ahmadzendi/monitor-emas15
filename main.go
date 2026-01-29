package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/websocket/v2"
	"github.com/patrickmn/go-cache"
	"github.com/PuerkitoBio/goquery"
)

var (
	MAX_HISTORY      = 1441
	MAX_USD_HISTORY  = 11
	API_POLL_INTERVAL = 20 * time.Millisecond
	USD_POLL_INTERVAL = 300 * time.Millisecond
	BROADCAST_DEBOUNCE = 8 * time.Millisecond
	MAX_CONNECTIONS  = 5000
	BROADCAST_CHUNK_SIZE = 50
	STATE_CACHE_TTL  = 50 * time.Millisecond
	SECRET_KEY       = getenv("ADMIN_SECRET", "indonesia")
	MIN_LIMIT        = 0
	MAX_LIMIT        = 88888
	RATE_LIMIT_SECONDS = 5
	MAX_FAILED_ATTEMPTS = 5
	BLOCK_DURATION   = 300 * time.Second
	limitBulan       = int64(8)
	lastSuccessfulCall int64
)

type HistoryItem struct {
	BuyingRate  int    `json:"buying_rate"`
	SellingRate int    `json:"selling_rate"`
	Status      string `json:"status"`
	Diff        int    `json:"diff"`
	CreatedAt   string `json:"created_at"`
}

type UsdIdrItem struct {
	Price string `json:"price"`
	Time  string `json:"time"`
}

type State struct {
	History       []map[string]interface{} `json:"history"`
	UsdIdrHistory []UsdIdrItem             `json:"usd_idr_history"`
	LimitBulan    int64                    `json:"limit_bulan"`
}

var (
	historyMu sync.RWMutex
	history   = make([]HistoryItem, 0, MAX_HISTORY)
	usdMu     sync.RWMutex
	usdIdrHistory = make([]UsdIdrItem, 0, MAX_USD_HISTORY)
	lastBuy   int64
	shownUpdates sync.Map
)

var (
	failedAttempts = cache.New(60*time.Second, 60*time.Second)
	blockedIPs     = cache.New(BLOCK_DURATION, BLOCK_DURATION)
)

var (
	stateCacheMu sync.Mutex
	stateCache   []byte
	stateCacheAt int64
	stateCacheVer int64
)

var (
	connectionsMu sync.RWMutex
	connections   = make(map[*websocket.Conn]struct{})
)

var (
	broadcastCh = make(chan []byte, 128)
)

var (
	HARI_INDO = []string{"Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu", "Minggu"}
)

func getenv(k, d string) string {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	return v
}

func main() {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		Prefork:               false,
	})
	app.Use(compress.New(compress.Config{Level: compress.LevelBestSpeed, MinLength: 500}))
	app.Get("/", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(htmlTemplate)
	})
	app.Get("/api/state", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(getStateBytes())
	})
	app.Get("/aturTS/:value", func(c *fiber.Ctx) error {
		ip := getClientIP(c)
		if isIPBlocked(ip) {
			return c.Status(429).JSON(fiber.Map{"detail": "IP diblokir sementara"})
		}
		key := c.Query("key")
		if !verifySecret(key) {
			recordFailedAttempt(ip)
			return c.Status(403).JSON(fiber.Map{"detail": "Akses ditolak"})
		}
		now := time.Now().Unix()
		if now-atomic.LoadInt64(&lastSuccessfulCall) < int64(RATE_LIMIT_SECONDS) {
			return c.Status(429).JSON(fiber.Map{"detail": "Terlalu cepat, tunggu beberapa detik"})
		}
		value, err := strconv.Atoi(c.Params("value"))
		if err != nil || value < MIN_LIMIT || value > MAX_LIMIT {
			return c.Status(400).JSON(fiber.Map{"detail": "Nilai harus 0-88888"})
		}
		atomic.StoreInt64(&limitBulan, int64(value))
		atomic.StoreInt64(&lastSuccessfulCall, now)
		invalidateStateCache()
		go scheduleBroadcast()
		return c.JSON(fiber.Map{"status": "ok", "limit_bulan": atomic.LoadInt64(&limitBulan)})
	})
	app.Get("/ws", websocket.New(func(c *websocket.Conn) {
		if !addConnection(c) {
			c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(1013, "Too many connections"))
			c.Close()
			return
		}
		defer removeConnection(c)
		c.SetReadLimit(1024)
		c.SetReadDeadline(time.Now().Add(45 * time.Second))
		c.WriteMessage(websocket.BinaryMessage, getStateBytes())
		go func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					break
				}
				if string(msg) == "ping" {
					conn.WriteMessage(websocket.BinaryMessage, []byte(`{"pong":true}`))
				}
				conn.SetReadDeadline(time.Now().Add(45 * time.Second))
			}
			conn.Close()
		}(c)
		select {}
	}))
	go apiLoop()
	go usdIdrLoop()
	go heartbeatLoop()
	go broadcastLoop()
	log.Fatal(app.Listen(":8000"))
}

func getClientIP(c *fiber.Ctx) string {
	ip := c.Get("X-Forwarded-For")
	if ip != "" {
		return strings.Split(ip, ",")[0]
	}
	ip, _, _ = net.SplitHostPort(c.Context().RemoteAddr().String())
	return ip
}

func isIPBlocked(ip string) bool {
	_, found := blockedIPs.Get(ip)
	return found
}

func recordFailedAttempt(ip string) {
	v, found := failedAttempts.Get(ip)
	var arr []int64
	if found {
		arr = v.([]int64)
	}
	arr = append(arr, time.Now().Unix())
	if len(arr) >= MAX_FAILED_ATTEMPTS {
		blockedIPs.Set(ip, true, BLOCK_DURATION)
	}
	failedAttempts.Set(ip, arr, 60*time.Second)
}

func verifySecret(key string) bool {
	return subtle.ConstantTimeCompare([]byte(key), []byte(SECRET_KEY)) == 1
}

func formatRupiah(n int) string {
	s := strconv.Itoa(n)
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func getDayTime(dateStr string) string {
	t, err := time.Parse("2006-01-02 15:04:05", dateStr)
	if err != nil {
		return dateStr
	}
	return HARI_INDO[int(t.Weekday()+6)%7] + " " + t.Format("15:04:05")
}

func formatWaktuOnly(dateStr, status string) string {
	return getDayTime(dateStr) + status
}

func formatDiffDisplay(diff int, status string) string {
	if status == "🚀" {
		return "🚀+" + formatRupiah(diff)
	} else if status == "🔻" {
		return "🔻-" + formatRupiah(int(math.Abs(float64(diff))))
	}
	return "➖tetap"
}

func formatTransactionDisplay(buy, sell, diff string) string {
	return "Beli: " + buy + "<br>Jual: " + sell + "<br>" + diff
}

var PROFIT_CONFIGS = [][]int{
	{20000000, 19314000},
	{30000000, 28980000},
	{40000000, 38652000},
	{50000000, 48325000},
}

func calcProfit(h HistoryItem, modal, pokok int) string {
	buyRate := h.BuyingRate
	sellRate := h.SellingRate
	gram := float64(modal) / float64(buyRate)
	val := int(gram*float64(sellRate)) - pokok
	gramStr := strconv.FormatFloat(gram, 'f', 4, 64)
	if val > 0 {
		return "+" + formatRupiah(val) + "🟢➺" + gramStr + "gr"
	} else if val < 0 {
		return "-" + formatRupiah(-val) + "🔴➺" + gramStr + "gr"
	}
	return formatRupiah(0) + "➖➺" + gramStr + "gr"
}

func buildSingleHistoryItem(h HistoryItem) map[string]interface{} {
	buyFmt := formatRupiah(h.BuyingRate)
	sellFmt := formatRupiah(h.SellingRate)
	diffDisplay := formatDiffDisplay(h.Diff, h.Status)
	return map[string]interface{}{
		"buying_rate":        buyFmt,
		"selling_rate":       sellFmt,
		"waktu_display":      formatWaktuOnly(h.CreatedAt, h.Status),
		"diff_display":       diffDisplay,
		"transaction_display": formatTransactionDisplay(buyFmt, sellFmt, diffDisplay),
		"created_at":         h.CreatedAt,
		"jt20":               calcProfit(h, PROFIT_CONFIGS[0][0], PROFIT_CONFIGS[0][1]),
		"jt30":               calcProfit(h, PROFIT_CONFIGS[1][0], PROFIT_CONFIGS[1][1]),
		"jt40":               calcProfit(h, PROFIT_CONFIGS[2][0], PROFIT_CONFIGS[2][1]),
		"jt50":               calcProfit(h, PROFIT_CONFIGS[3][0], PROFIT_CONFIGS[3][1]),
	}
}

func buildHistoryData() []map[string]interface{} {
	historyMu.RLock()
	defer historyMu.RUnlock()
	out := make([]map[string]interface{}, len(history))
	for i, h := range history {
		out[i] = buildSingleHistoryItem(h)
	}
	return out
}

func buildUsdIdrData() []UsdIdrItem {
	usdMu.RLock()
	defer usdMu.RUnlock()
	out := make([]UsdIdrItem, len(usdIdrHistory))
	copy(out, usdIdrHistory)
	return out
}

func getStateBytes() []byte {
	stateCacheMu.Lock()
	defer stateCacheMu.Unlock()
	now := time.Now().UnixNano()
	if stateCache != nil && now-stateCacheAt < int64(STATE_CACHE_TTL) {
		return stateCache
	}
	state := State{
		History:       buildHistoryData(),
		UsdIdrHistory: buildUsdIdrData(),
		LimitBulan:    atomic.LoadInt64(&limitBulan),
	}
	b, _ := json.Marshal(state)
	stateCache = b
	stateCacheAt = now
	return b
}

func invalidateStateCache() {
	stateCacheMu.Lock()
	stateCache = nil
	stateCacheAt = 0
	stateCacheMu.Unlock()
}

func addConnection(c *websocket.Conn) bool {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	if len(connections) >= MAX_CONNECTIONS {
		return false
	}
	connections[c] = struct{}{}
	return true
}

func removeConnection(c *websocket.Conn) {
	connectionsMu.Lock()
	delete(connections, c)
	connectionsMu.Unlock()
}

func broadcastLoop() {
	for msg := range broadcastCh {
		connectionsMu.RLock()
		conns := make([]*websocket.Conn, 0, len(connections))
		for c := range connections {
			conns = append(conns, c)
		}
		connectionsMu.RUnlock()
		for i := 0; i < len(conns); i += BROADCAST_CHUNK_SIZE {
			end := i + BROADCAST_CHUNK_SIZE
			if end > len(conns) {
				end = len(conns)
			}
			wg := sync.WaitGroup{}
			for _, c := range conns[i:end] {
				wg.Add(1)
				go func(conn *websocket.Conn) {
					defer wg.Done()
					conn.WriteMessage(websocket.BinaryMessage, msg)
				}(c)
			}
			wg.Wait()
		}
	}
}

func scheduleBroadcast() {
	time.Sleep(BROADCAST_DEBOUNCE)
	broadcastCh <- getStateBytes()
}

func apiLoop() {
	client := &http.Client{Timeout: 6 * time.Second}
	for {
		result := fetchTreasuryPrice(client)
		if result != nil {
			data := result["data"].(map[string]interface{})
			buy, _ := strconv.Atoi(toStr(data["buying_rate"]))
			sell, _ := strconv.Atoi(toStr(data["selling_rate"]))
			upd := toStr(data["updated_at"])
			if buy > 0 && sell > 0 && upd != "" {
				if _, ok := shownUpdates.Load(upd); !ok {
					var diff int
					lb := atomic.LoadInt64(&lastBuy)
					if lb == 0 {
						diff = 0
					} else {
						diff = buy - int(lb)
					}
					var status string
					if lb == 0 {
						status = "➖"
					} else if buy > int(lb) {
						status = "🚀"
					} else if buy < int(lb) {
						status = "🔻"
					} else {
						status = "➖"
					}
					historyMu.Lock()
					if len(history) >= MAX_HISTORY {
						history = history[1:]
					}
					history = append(history, HistoryItem{
						BuyingRate:  buy,
						SellingRate: sell,
						Status:      status,
						Diff:        diff,
						CreatedAt:   upd,
					})
					historyMu.Unlock()
					atomic.StoreInt64(&lastBuy, int64(buy))
					shownUpdates.Store(upd, true)
					invalidateStateCache()
					go scheduleBroadcast()
				}
			}
		}
		time.Sleep(API_POLL_INTERVAL)
	}
}

func fetchTreasuryPrice(client *http.Client) map[string]interface{} {
	req, _ := http.NewRequest("POST", "https://api.treasury.id/api/v1/antigrvty/gold/rate", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://treasury.id")
	req.Header.Set("Referer", "https://treasury.id/")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	return result
}

func usdIdrLoop() {
	client := &http.Client{Timeout: 6 * time.Second}
	for {
		price := fetchUsdIdrPrice(client)
		if price != "" {
			usdMu.Lock()
			shouldUpdate := len(usdIdrHistory) == 0 || usdIdrHistory[len(usdIdrHistory)-1].Price != price
			if shouldUpdate {
				wib := time.Now().UTC().Add(7 * time.Hour)
				if len(usdIdrHistory) >= MAX_USD_HISTORY {
					usdIdrHistory = usdIdrHistory[1:]
				}
				usdIdrHistory = append(usdIdrHistory, UsdIdrItem{
					Price: price,
					Time:  wib.Format("15:04:05"),
				})
				invalidateStateCache()
				go scheduleBroadcast()
			}
			usdMu.Unlock()
		}
		time.Sleep(USD_POLL_INTERVAL)
	}
}

func fetchUsdIdrPrice(client *http.Client) string {
	req, _ := http.NewRequest("GET", "https://www.google.com/finance/quote/USD-IDR", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.AddCookie(&http.Cookie{Name: "CONSENT", Value: "YES+cb.20231208-04-p0.en+FX+410"})
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	price := ""
	doc.Find("div.YMlKec.fxKbKc").Each(func(i int, s *goquery.Selection) {
		if price == "" {
			price = strings.TrimSpace(s.Text())
		}
	})
	return price
}

func heartbeatLoop() {
	pingMsg := []byte(`{"ping":true}`)
	for {
		time.Sleep(15 * time.Second)
		connectionsMu.RLock()
		if len(connections) > 0 {
			for c := range connections {
				c.WriteMessage(websocket.BinaryMessage, pingMsg)
			}
		}
		connectionsMu.RUnlock()
	}
}

func toStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', 0, 64)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="id">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=5">
<title>Harga Emas Treasury</title>
<link rel="stylesheet" href="https://cdn.datatables.net/1.13.6/css/jquery.dataTables.min.css"/>
<style>
*{box-sizing:border-box}
body{font-family:Arial,sans-serif;margin:0;padding:5px 20px 0 20px;background:#fff;color:#222;transition:background .3s,color .3s}
h2{margin:0 0 2px}
h3{margin:20px 0 10px}
.header{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:2px}
.title-wrap{display:flex;align-items:center;gap:10px}
.tele-link{display:inline-flex;align-items:center;gap:6px;text-decoration:none;transition:transform .2s}
.tele-link:hover{transform:scale(1.05)}
.tele-icon{display:inline-flex;align-items:center;justify-content:center;width:32px;height:32px;background:#0088cc;color:#fff;border-radius:50%;transition:background .3s}
.tele-link:hover .tele-icon{background:#006699}
.tele-text{font-size:0.95em;font-weight:bold;color:#ff1744}
.dark-mode .tele-icon{background:#29b6f6}
.dark-mode .tele-link:hover .tele-icon{background:#0288d1}
.dark-mode .tele-text{color:#00E124}
#jam{font-size:1.3em;color:#ff1744;font-weight:bold;margin-bottom:8px}
table.dataTable{width:100%!important}
table.dataTable thead th{font-weight:bold;white-space:nowrap;padding:10px 8px}
table.dataTable tbody td{padding:8px;white-space:nowrap}
th.waktu,td.waktu{width:100px;min-width:90px;max-width:1050px;text-align:left}
th.profit,td.profit{width:154px;min-width:80px;max-width:160px;text-align:left}
.theme-toggle-btn{padding:0;border:none;border-radius:50%;background:#222;color:#fff;cursor:pointer;font-size:1.5em;width:44px;height:44px;display:flex;align-items:center;justify-content:center;transition:background .3s}
.theme-toggle-btn:hover{background:#444}
.dark-mode{background:#181a1b!important;color:#e0e0e0!important}
.dark-mode #jam{color:#ffb300!important}
.dark-mode table.dataTable,.dark-mode table.dataTable thead th,.dark-mode table.dataTable tbody td{background:#23272b!important;color:#e0e0e0!important}
.dark-mode table.dataTable thead th{color:#ffb300!important}
.dark-mode .theme-toggle-btn{background:#ffb300;color:#222}
.dark-mode .theme-toggle-btn:hover{background:#ffd54f}
.container-flex{display:flex;gap:15px;flex-wrap:wrap;margin-top:10px}
.card{border:1px solid #ccc;border-radius:6px;padding:10px}
.card-usd{width:248px;height:370px;overflow-y:auto}
.card-chart{flex:1;min-width:400px;height:370px;overflow:hidden}
.card-calendar{width:100%;max-width:750px;height:460px;overflow:hidden;display:flex;flex-direction:column}
#priceList{list-style:none;padding:0;margin:0;max-height:275px;overflow-y:auto}
#priceList li{margin-bottom:1px}
.time{color:gray;font-size:.9em;margin-left:10px}
#currentPrice{color:red;font-weight:bold}
.dark-mode #currentPrice{color:#00E124;text-shadow:1px 1px #00B31C}
#tabel tbody tr:first-child td{color:red!important;font-weight:bold}
.dark-mode #tabel tbody tr:first-child td{color:#00E124!important}
#footerApp{width:100%;position:fixed;bottom:0;left:0;background:transparent;text-align:center;z-index:100;padding:8px 0}
.marquee-text{display:inline-block;color:#F5274D;animation:marquee 70s linear infinite;font-weight:bold}
.dark-mode .marquee-text{color:#B232B2}
@keyframes marquee{0%{transform:translateX(100vw)}100%{transform:translateX(-100%)}}
.loading-text{color:#999;font-style:italic}
.tbl-wrap{width:100%;overflow-x:auto;-webkit-overflow-scrolling:touch}
.dataTables_wrapper{position:relative}
.dt-top-controls{display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px;margin-bottom:0!important;padding:8px 0;padding-bottom:0!important}
.dataTables_wrapper .dataTables_length{margin:0!important;float:none!important;margin-bottom:0!important;padding-bottom:0!important}
.dataTables_wrapper .dataTables_filter{margin:0!important;float:none!important}
.dataTables_wrapper .dataTables_info{display:none!important}
.dataTables_wrapper .dataTables_paginate{margin-top:10px!important;text-align:center!important}
.tbl-wrap{margin-top:0!important;padding-top:0!important}
#tabel.dataTable{margin-top:0!important}
#tabel tbody td.transaksi{line-height:1.3;padding:6px 8px}
#tabel tbody td.transaksi .harga-beli{display:block;margin-bottom:2px}
#tabel tbody td.transaksi .harga-jual{display:block;margin-bottom:2px}
#tabel tbody td.transaksi .selisih{display:block;font-weight:bold}
.profit-order-btns{display:none;gap:2px;align-items:center;margin-right:6px}
.profit-btn{padding:4px 7px;border:1px solid #aaa;background:#f0f0f0;border-radius:4px;font-size:11px;cursor:pointer;font-weight:bold;transition:all .2s}
.profit-btn:hover{background:#ddd}
.profit-btn.active{background:#007bff;color:#fff;border-color:#007bff}
.dark-mode .profit-btn{background:#333;border-color:#555;color:#ccc}
.dark-mode .profit-btn:hover{background:#444}
.dark-mode .profit-btn.active{background:#ffb300;color:#222;border-color:#ffb300}
.filter-wrap{display:flex;align-items:center}
.tradingview-wrapper{height:100%;width:100%;overflow:hidden}
.calendar-section{width:100%;margin-top:20px;margin-bottom:60px}
.calendar-section h3{margin:0 0 10px}
.calendar-wrap{width:100%;overflow-x:auto;-webkit-overflow-scrolling:touch}
.calendar-iframe{border:0;width:100%;height:420px;min-width:700px;display:block}
.chart-header{display:flex;justify-content:space-between;align-items:center;margin-top:0;margin-bottom:10px}
.chart-header h3{margin:0}
.limit-label{font-size:0.95em;font-weight:bold;color:#ff1744}
.limit-label .limit-num{font-size:1.1em;padding:2px 8px;background:#ff1744;color:#fff;border-radius:4px;margin-left:4px}
.dark-mode .limit-label{color:#00E124}
.dark-mode .limit-label .limit-num{background:#00E124;color:#181a1b}
.dark-mode .card{border-color:#444}
.dark-mode .card-calendar{background:#23272b}
@media(max-width:768px){
body{padding:12px;padding-bottom:50px}
h2{font-size:1.1em}
h3{font-size:1em;margin:15px 0 8px}
.header{margin-bottom:2px}
.tele-icon{width:28px;height:28px}
.tele-icon svg{width:16px;height:16px}
.tele-text{font-size:0.85em}
#jam{font-size:1.5em;margin-bottom:6px}
table.dataTable{font-size:13px;min-width:620px}
table.dataTable thead th{padding:8px 6px}
table.dataTable tbody td{padding:6px}
.theme-toggle-btn{width:40px;height:40px;font-size:1.3em}
.container-flex{flex-direction:column;gap:15px}
.card-usd,.card-chart{width:100%!important;max-width:100%!important;min-width:0!important}
.card-usd{height:auto;min-height:320px}
.card-chart{height:380px}
.card-calendar{max-width:100%;height:auto;padding:0}
.calendar-section{margin-bottom:50px}
.calendar-wrap{margin:0 -12px;padding:0 12px;width:calc(100% + 24px)}
.calendar-iframe{height:380px;min-width:650px}
.dt-top-controls{flex-direction:row;justify-content:space-between;gap:5px;margin-bottom:8px;padding:5px 0}
.dataTables_wrapper .dataTables_length{font-size:12px!important}
.dataTables_wrapper .dataTables_filter{font-size:12px!important}
.dataTables_wrapper .dataTables_filter input{width:80px!important;font-size:12px!important;padding:4px 6px!important}
.dataTables_wrapper .dataTables_length select{font-size:12px!important;padding:3px!important}
.dataTables_wrapper .dataTables_paginate .paginate_button{padding:4px 10px!important;font-size:12px!important;min-width:auto!important}
#tabel{min-width:580px!important}
#tabel tbody td{font-size:12px!important;padding:5px 4px!important}
#tabel tbody td.waktu{width:85px!important;min-width:85px!important;max-width:85px!important}
#tabel tbody td.transaksi{width:140px!important;min-width:140px!important;max-width:140px!important}
#tabel tbody td.profit{width:120px!important;min-width:120px!important;max-width:120px!important}
#tabel tbody td.transaksi .harga-beli,#tabel tbody td.transaksi .harga-jual,#tabel tbody td.transaksi .selisih{font-size:11px!important;margin-bottom:1px!important}
.profit-order-btns{display:flex}
.filter-wrap{flex-wrap:nowrap}
.chart-header{flex-direction:row;gap:8px}
.chart-header h3{font-size:0.95em}
.limit-label{font-size:0.85em}
}
@media(max-width:480px){
body{padding:10px;padding-bottom:45px}
h2{font-size:1em}
h3{font-size:0.95em;margin:12px 0 8px}
.header{margin-bottom:1px}
.title-wrap{gap:6px}
.tele-icon{width:24px;height:24px}
.tele-icon svg{width:14px;height:14px}
.tele-text{font-size:0.8em}
#jam{font-size:1.3em;margin-bottom:5px}
table.dataTable{font-size:12px;min-width:560px}
table.dataTable thead th{padding:6px 4px}
table.dataTable tbody td{padding:5px 4px}
th.waktu,td.waktu{width:60px;min-width:50px;max-width:70px}
.theme-toggle-btn{width:36px;height:36px;font-size:1.2em}
.container-flex{gap:12px}
.card{padding:8px}
.card-usd{min-height:280px}
.card-chart{height:340px}
.card-calendar{height:auto;padding:0}
.calendar-section{margin:20px 0 45px 0}
.calendar-wrap{margin:0 -10px;padding:0 10px;width:calc(100% + 20px)}
.calendar-iframe{height:350px;min-width:600px}
#footerApp{padding:5px 0}
.marquee-text{font-size:12px}
.dt-top-controls{gap:3px;margin-bottom:6px}
.dataTables_wrapper .dataTables_length,.dataTables_wrapper .dataTables_filter{font-size:11px!important}
.dataTables_wrapper .dataTables_filter input{width:65px!important;font-size:11px!important}
.dataTables_wrapper .dataTables_length select{font-size:11px!important}
.dataTables_wrapper .dataTables_paginate .paginate_button{padding:3px 8px!important;font-size:11px!important}
#priceList{max-height:200px}
#tabel{min-width:540px!important}
#tabel tbody td{font-size:11px!important;padding:4px 3px!important}
#tabel tbody td.waktu{width:80px!important;min-width:80px!important;max-width:80px!important}
#tabel tbody td.transaksi{width:130px!important;min-width:130px!important;max-width:130px!important}
#tabel tbody td.profit{width:110px!important;min-width:110px!important;max-width:110px!important}
#tabel tbody td.transaksi .harga-beli,#tabel tbody td.transaksi .harga-jual,#tabel tbody td.transaksi .selisih{font-size:10px!important;margin-bottom:0!important}
.profit-btn{padding:3px 5px;font-size:10px}
.chart-header h3{font-size:0.9em}
.limit-label{font-size:0.8em}
.limit-label .limit-num{font-size:1em;padding:1px 6px}
}
</style>
</head>
<body>
<div class="header">
<div class="title-wrap">
<h2>Harga Emas Treasury  ➺ </h2>
<a href="https://t.me/+FLtJjyjVV8xlM2E1" target="_blank" class="tele-link" title="Join Telegram"><span class="tele-icon"><svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor"><path d="M11.944 0A12 12 0 0 0 0 12a12 12 0 0 0 12 12 12 12 0 0 0 12-12A12 12 0 0 0 12 0a12 12 0 0 0-.056 0zm4.962 7.224c.1-.002.321.023.465.14a.506.506 0 0 1 .171.325c.016.093.036.306.02.472-.18 1.898-.962 6.502-1.36 8.627-.168.9-.499 1.201-.82 1.23-.696.065-1.225-.46-1.9-.902-1.056-.693-1.653-1.124-2.678-1.8-1.185-.78-.417-1.21.258-1.91.177-.184 3.247-2.977 3.307-3.23.007-.032.014-.15-.056-.212s-.174-.041-.249-.024c-.106.024-1.793 1.14-5.061 3.345-.48.33-.913.49-1.302.48-.428-.008-1.252-.241-1.865-.44-.752-.245-1.349-.374-1.297-.789.027-.216.325-.437.893-.663 3.498-1.524 5.83-2.529 6.998-3.014 3.332-1.386 4.025-1.627 4.476-1.635z"/></svg></span><span class="tele-text">Telegram</span></a>
</div>
<button class="theme-toggle-btn" id="themeBtn" onclick="toggleTheme()" title="Ganti Tema">🌙</button>
</div>
<div id="jam"></div>
<div class="tbl-wrap">
<table id="tabel" class="display">
<thead>
<tr>
<th class="waktu">Waktu</th>
<th>Data Transaksi</th>
<th class="profit" id="thP1">Est. cuan 20 JT ➺ gr</th>
<th class="profit" id="thP2">Est. cuan 30 JT ➺ gr</th>
<th class="profit" id="thP3">Est. cuan 40 JT ➺ gr</th>
<th class="profit" id="thP4">Est. cuan 50 JT ➺ gr</th>
</tr>
</thead>
<tbody></tbody>
</table>
</div>
<div class="container-flex">
<div style="flex:1;min-width:400px">
<div class="chart-header">
<h3>Chart Harga Emas (XAU/USD)</h3>
<span class="limit-label">Limit Bulan ini:<span class="limit-num" id="limitBulan">88888</span></span>
</div>
<div class="card card-chart">
<div class="tradingview-wrapper" id="tradingview_chart"></div>
</div>
</div>
<div>
<h3 style="margin-top:0">Harga USD/IDR Google Finance</h3>
<div class="card card-usd">
<p>Harga saat ini: <span id="currentPrice" class="loading-text">Memuat data...</span></p>
<h4>Harga Terakhir:</h4>
<ul id="priceList"><li class="loading-text">Menunggu data...</li></ul>
</div>
</div>
</div>
<div class="calendar-section">
<h3>Kalender Ekonomi</h3>
<div class="card card-calendar">
<div class="calendar-wrap">
<iframe class="calendar-iframe" src="https://sslecal2.investing.com?columns=exc_flags,exc_currency,exc_importance,exc_actual,exc_forecast,exc_previous&category=_employment,_economicActivity,_inflation,_centralBanks,_confidenceIndex&importance=3&features=datepicker,timezone,timeselector,filters&countries=5,37,48,35,17,36,26,12,72&calType=week&timeZone=27&lang=54" loading="lazy"></iframe>
</div>
</div>
</div>
<footer id="footerApp"><span class="marquee-text">&copy;2026 ~ahmadkholil~</span></footer>
<script src="https://code.jquery.com/jquery-3.7.0.min.js"></script>
<script src="https://cdn.datatables.net/1.13.6/js/jquery.dataTables.min.js"></script>
<script src="https://s3.tradingview.com/tv.js"></script>
<script>
/* ... (HTML JS sama persis dengan Python, tidak diubah) ... */
</script>
</body>
</html>`
