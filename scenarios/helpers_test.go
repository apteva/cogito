package scenarios

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/apteva/core"
)

func seedTodoApp(t *testing.T, dir string) {
	t.Helper()
	appDir := filepath.Join(dir, "app")
	os.MkdirAll(appDir, 0755)

	// go.mod
	os.WriteFile(filepath.Join(appDir, "go.mod"), []byte("module todo\n\ngo 1.21\n"), 0644)

	// todo.go — basic CRUD, no priority field
	os.WriteFile(filepath.Join(appDir, "todo.go"), []byte(`package todo

type Todo struct {
	ID        int    `+"`"+`json:"id"`+"`"+`
	Title     string `+"`"+`json:"title"`+"`"+`
	Completed bool   `+"`"+`json:"completed"`+"`"+`
}

var todos []Todo
var nextID = 1

func Create(title string) Todo {
	t := Todo{ID: nextID, Title: title}
	nextID++
	todos = append(todos, t)
	return t
}

func List() []Todo {
	return todos
}

func Complete(id int) bool {
	for i := range todos {
		if todos[i].ID == id {
			todos[i].Completed = true
			return true
		}
	}
	return false
}

func Delete(id int) bool {
	for i := range todos {
		if todos[i].ID == id {
			todos = append(todos[:i], todos[i+1:]...)
			return true
		}
	}
	return false
}

func Reset() {
	todos = nil
	nextID = 1
}
`), 0644)

	// todo_test.go — basic tests
	os.WriteFile(filepath.Join(appDir, "todo_test.go"), []byte(`package todo

import "testing"

func TestCreate(t *testing.T) {
	Reset()
	td := Create("Buy milk")
	if td.Title != "Buy milk" {
		t.Errorf("expected 'Buy milk', got %q", td.Title)
	}
	if td.ID != 1 {
		t.Errorf("expected ID 1, got %d", td.ID)
	}
}

func TestList(t *testing.T) {
	Reset()
	Create("Task 1")
	Create("Task 2")
	if len(List()) != 2 {
		t.Errorf("expected 2 todos, got %d", len(List()))
	}
}

func TestComplete(t *testing.T) {
	Reset()
	td := Create("Do laundry")
	if !Complete(td.ID) {
		t.Error("expected Complete to return true")
	}
	if !List()[0].Completed {
		t.Error("expected todo to be completed")
	}
}

func TestDelete(t *testing.T) {
	Reset()
	td := Create("Temp")
	if !Delete(td.ID) {
		t.Error("expected Delete to return true")
	}
	if len(List()) != 0 {
		t.Error("expected empty list after delete")
	}
}
`), 0644)

	// test.sh at root level (codebase dir) since run_tests runs from there
	os.WriteFile(filepath.Join(dir, "test.sh"), []byte("#!/bin/bash\ncd app && go test ./... 2>&1\n"), 0644)
}

func seedWebsiteBrief(t *testing.T, dir string) {
	t.Helper()

	// Design brief
	WriteJSONFile(t, dir, "brief.json", map[string]any{
		"company": "NovaPay",
		"tagline": "Payments infrastructure for the AI economy",
		"sections": []map[string]any{
			{
				"id": "hero", "heading": "Accept AI-to-AI payments",
				"subheading": "NovaPay handles billing between autonomous agents, with real-time settlement and fraud detection.",
				"cta":        "Get Started",
			},
			{
				"id": "features", "items": []map[string]string{
					{"title": "Agent Wallets", "desc": "Every AI agent gets a programmable wallet with spending limits and approval flows."},
					{"title": "Real-time Settlement", "desc": "Sub-second settlement between agents. No batching, no delays."},
					{"title": "Fraud Detection", "desc": "ML-powered anomaly detection built for machine-speed transactions."},
				},
			},
			{
				"id": "pricing", "plans": []map[string]any{
					{"name": "Starter", "price": "$0", "desc": "1,000 transactions/mo", "features": []string{"Agent wallets", "Basic analytics", "Email support"}},
					{"name": "Growth", "price": "$49/mo", "desc": "50,000 transactions/mo", "features": []string{"Everything in Starter", "Real-time dashboard", "Webhooks", "Priority support"}},
					{"name": "Enterprise", "price": "Custom", "desc": "Unlimited", "features": []string{"Everything in Growth", "SLA", "Dedicated account manager", "Custom integrations"}},
				},
			},
			{
				"id": "footer", "links": []string{"Docs", "Pricing", "Blog", "GitHub", "Twitter"},
			},
		},
		"brand": map[string]string{"primary": "#6C5CE7", "secondary": "#00CEC9", "dark": "#2D3436", "light": "#DFE6E9"},
	})

	// Assets
	WriteJSONFile(t, dir, "assets.json", []map[string]string{
		{"name": "logo", "url": "/logo.svg", "desc": "NovaPay logo"},
		{"name": "hero-bg", "url": "/hero-bg.svg", "desc": "Abstract gradient background"},
	})

	// App directory
	os.MkdirAll(filepath.Join(dir, "app", "src"), 0755)

	// test.sh — validates project structure (searches recursively)
	os.WriteFile(filepath.Join(dir, "test.sh"), []byte(`#!/bin/bash
cd app || exit 1
[ -f package.json ] || { echo "ERROR: no package.json"; exit 1; }
# Find entry point
found_entry=0
for f in src/index.tsx src/index.jsx src/main.tsx src/main.jsx; do
  [ -f "$f" ] && { found_entry=1; break; }
done
[ "$found_entry" -eq 1 ] || { echo "ERROR: no entry point (src/index.tsx or src/main.tsx)"; exit 1; }
# Find App component
found_app=0
for f in src/App.tsx src/App.jsx; do
  [ -f "$f" ] && { found_app=1; break; }
done
[ "$found_app" -eq 1 ] || { echo "ERROR: no App component (src/App.tsx)"; exit 1; }
# Check component files have exports (skip entry points)
count=0
while IFS= read -r f; do
  base=$(basename "$f")
  # Skip entry points — they render to DOM, no export needed
  case "$base" in index.tsx|index.jsx|main.tsx|main.jsx) count=$((count+1)); continue;; esac
  grep -q "export" "$f" || { echo "ERROR: $f has no export"; exit 1; }
  count=$((count + 1))
done < <(find src -name "*.tsx" -o -name "*.jsx" 2>/dev/null)
[ "$count" -ge 2 ] || { echo "ERROR: need at least 2 component files, found $count"; exit 1; }
echo "BUILD OK: $count components"
mkdir -p dist
echo "<html>bundled</html>" > dist/index.html
`), 0755)
}

func seedStoreData(t *testing.T, dir string) {
	t.Helper()

	WriteJSONFile(t, dir, "sales.json", map[string]any{
		"summary": "Revenue down 23% month-over-month. 3 of top 5 products showing sharp decline.",
		"daily": []map[string]any{
			{"date": "2026-04-01", "revenue": 3200, "orders": 42},
			{"date": "2026-04-02", "revenue": 2800, "orders": 38},
			{"date": "2026-04-03", "revenue": 2100, "orders": 29},
			{"date": "2026-04-04", "revenue": 1900, "orders": 25},
			{"date": "2026-04-05", "revenue": 1700, "orders": 22},
			{"date": "2026-04-06", "revenue": 1500, "orders": 19},
			{"date": "2026-04-07", "revenue": 1400, "orders": 17},
		},
		"by_product": []map[string]any{
			{"name": "Wireless Earbuds Pro", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "USB-C Hub 7-in-1", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "Laptop Stand Adjustable", "units_sold": 45, "revenue": 2250, "trend": "stable"},
			{"name": "Mechanical Keyboard RGB", "units_sold": 12, "revenue": 1080, "trend": "declining — was 30/week"},
			{"name": "Webcam 4K", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "Phone Case Premium", "units_sold": 89, "revenue": 1335, "trend": "stable"},
			{"name": "Desk Lamp LED", "units_sold": 34, "revenue": 680, "trend": "stable"},
		},
	})

	WriteJSONFile(t, dir, "inventory.json", map[string]any{
		"products": []map[string]any{
			{"name": "Wireless Earbuds Pro", "stock": 0, "price": 79.99, "status": "OUT OF STOCK", "last_restocked": "2026-03-01"},
			{"name": "USB-C Hub 7-in-1", "stock": 0, "price": 49.99, "status": "OUT OF STOCK", "last_restocked": "2026-03-05"},
			{"name": "Laptop Stand Adjustable", "stock": 120, "price": 49.99, "status": "in stock"},
			{"name": "Mechanical Keyboard RGB", "stock": 45, "price": 89.99, "status": "in stock"},
			{"name": "Webcam 4K", "stock": 0, "price": 129.99, "status": "OUT OF STOCK", "last_restocked": "2026-02-20"},
			{"name": "Phone Case Premium", "stock": 230, "price": 14.99, "status": "in stock"},
			{"name": "Desk Lamp LED", "stock": 67, "price": 19.99, "status": "in stock"},
		},
	})

	WriteJSONFile(t, dir, "reviews.json", map[string]any{
		"average_rating": 3.2,
		"recent": []map[string]any{
			{"product": "Wireless Earbuds Pro", "rating": 1, "text": "Wanted to buy but OUT OF STOCK for over a month! Going to Amazon instead.", "date": "2026-04-05"},
			{"product": "USB-C Hub 7-in-1", "rating": 1, "text": "Says out of stock. This was my favorite hub. Very disappointing.", "date": "2026-04-04"},
			{"product": "Mechanical Keyboard RGB", "rating": 3, "text": "Good keyboard but $89.99 is too expensive. Same one is $69 on Amazon.", "date": "2026-04-03"},
			{"product": "Webcam 4K", "rating": 1, "text": "OUT OF STOCK AGAIN. Third time I've tried to order. Lost a customer.", "date": "2026-04-06"},
			{"product": "Phone Case Premium", "rating": 5, "text": "Great case, fast shipping, good price!", "date": "2026-04-05"},
			{"product": "Laptop Stand Adjustable", "rating": 4, "text": "Solid product but shipping was slow — 8 days.", "date": "2026-04-02"},
			{"product": "Desk Lamp LED", "rating": 4, "text": "Nice lamp. Would buy again.", "date": "2026-04-01"},
		},
	})

	WriteJSONFile(t, dir, "competitors.json", map[string]any{
		"comparison": []map[string]any{
			{"product": "Wireless Earbuds Pro", "our_price": 79.99, "amazon_price": 74.99, "best_buy_price": 79.99},
			{"product": "USB-C Hub 7-in-1", "our_price": 49.99, "amazon_price": 39.99, "best_buy_price": 44.99},
			{"product": "Mechanical Keyboard RGB", "our_price": 89.99, "amazon_price": 69.99, "best_buy_price": 74.99},
			{"product": "Webcam 4K", "our_price": 129.99, "amazon_price": 109.99, "best_buy_price": 119.99},
			{"product": "Phone Case Premium", "our_price": 14.99, "amazon_price": 14.99, "best_buy_price": 16.99},
			{"product": "Laptop Stand Adjustable", "our_price": 49.99, "amazon_price": 49.99, "best_buy_price": 54.99},
		},
	})

	WriteJSONFile(t, dir, "analytics.json", map[string]any{
		"period":           "last 7 days",
		"unique_visitors":  12400,
		"page_views":       34200,
		"conversion_rate":  "1.4% (was 3.2% last month)",
		"bounce_rate":      "62% (was 45% last month)",
		"top_search_terms": []string{"wireless earbuds", "usb-c hub", "webcam 4k", "keyboard", "portable monitor usb-c"},
		"cart_abandonment": "78% (was 52% last month)",
		"note":             "Traffic is healthy but conversions dropped. Most searched products are out of stock. Unusual spike in searches for 'portable monitor usb-c' — we don't carry this product. Check traffic sources for details.",
	})

	WriteJSONFile(t, dir, "traffic.json", map[string]any{
		"period": "last 7 days",
		"sources": []map[string]any{
			{"source": "google organic", "visits": 5200, "conversion": "1.8%"},
			{"source": "direct", "visits": 3100, "conversion": "2.1%"},
			{"source": "social media", "visits": 1800, "conversion": "0.9%"},
			{"source": "techgadgetblog.com/best-usb-c-monitors-2026", "visits": 1400, "conversion": "0.1%", "note": "ANOMALY: High traffic, near-zero conversion. Blog recommends 'UltraView Portable Monitor 15.6\" USB-C' at $199 — we don't carry it. 89% of these visitors search our store for it then leave."},
			{"source": "email campaigns", "visits": 900, "conversion": "3.2%"},
		},
		"trending_searches_with_zero_results": []string{"portable monitor", "usb-c monitor", "ultraview monitor"},
	})

	WriteJSONFile(t, dir, "suppliers.json", map[string]any{
		"suppliers": []map[string]any{
			{
				"name": "TechSource Direct", "status": "DELAYED",
				"products":       []string{"Wireless Earbuds Pro", "USB-C Hub 7-in-1", "Webcam 4K"},
				"normal_lead":    "3-5 days",
				"current_lead":   "14-18 days",
				"reason":         "Warehouse fire at distribution center. Backlog expected until mid-April.",
				"reliability":    "Usually excellent — this is an unusual event",
				"recommendation": "Use alt_supplier for urgent restocks (+15% cost, 3-5 day delivery)",
			},
			{
				"name": "AltSupply Express", "status": "OPERATIONAL",
				"products":     []string{"Wireless Earbuds Pro", "USB-C Hub 7-in-1", "Webcam 4K", "UltraView Portable Monitor"},
				"normal_lead":  "3-5 days",
				"current_lead": "3-5 days",
				"surcharge":    "15%",
				"note":         "Can also supply UltraView Portable Monitor 15.6\" USB-C at wholesale $120 (MSRP $199)",
			},
			{
				"name": "GenericParts Co", "status": "OPERATIONAL",
				"products":     []string{"Laptop Stand Adjustable", "Desk Lamp LED", "Phone Case Premium"},
				"normal_lead":  "2-3 days",
				"current_lead": "2-3 days",
			},
		},
	})

	WriteJSONFile(t, dir, "segments.json", map[string]any{
		"segments": []map[string]any{
			{"name": "power_buyers", "count": 340, "avg_order": 127, "frequency": "2.3x/month", "note": "Highest value — 40% of revenue. Many have stopped buying (out-of-stock items). 78 haven't purchased in 3 weeks."},
			{"name": "deal_seekers", "count": 890, "avg_order": 34, "frequency": "1.1x/month", "note": "Price-sensitive. Respond well to promotions. Keyboard price increase lost 40% of this segment."},
			{"name": "new_visitors", "count": 1200, "avg_order": 0, "frequency": "0", "note": "1,200 new visitors this week, mostly from techgadgetblog.com. Almost none converted — they're looking for a product we don't sell."},
			{"name": "returning_loyal", "count": 460, "avg_order": 62, "frequency": "1.8x/month", "note": "Stable segment. Good retention. Would respond well to loyalty rewards."},
		},
	})
}

func writeGoProject(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "app"), 0755)
	os.WriteFile(filepath.Join(dir, "app", "main.go"), []byte(`package main

import "fmt"

func GetUser(id int) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("invalid user id")
	}
	return fmt.Sprintf("user_%d", id), nil
}

func main() {
	name, _ := GetUser(1)
	fmt.Println(name)
}
`), 0644)
	os.WriteFile(filepath.Join(dir, "app", "main_test.go"), []byte(`package main

import "testing"

func TestGetUser(t *testing.T) {
	name, err := GetUser(1)
	if err != nil || name != "user_1" {
		t.Fatalf("expected user_1, got %s err=%v", name, err)
	}
}
`), 0644)
}

type studioProject struct {
	ID    string
	Title string
	Theme string // keyword that MUST appear in every caption for this project
	Style string
}

var mediaStudioProjects = []studioProject{
	{ID: "cooking_show", Title: "Chef Aurora's Kitchen", Theme: "recipe", Style: "warm and colorful kitchen sets"},
	{ID: "virtual_influencer", Title: "Luna Dreams", Theme: "lifestyle", Style: "dreamy aesthetic vlogs"},
	{ID: "fitness_channel", Title: "Atlas Training", Theme: "workout", Style: "high-energy gym sessions"},
}

func truncForLog(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func getTestThinker(t *testing.T) *Thinker { return nil }
