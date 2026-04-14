// MCP server for a simulated e-commerce store with discoverable problems.
// The agent receives a vague goal and must explore, diagnose, and act.
// State in STORE_DATA_DIR: sales.json, inventory.json, reviews.json, competitors.json, actions.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var dataDir string

func respond(id int64, result any) {
	data, _ := json.Marshal(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	fmt.Println(string(data))
}

func respondError(id int64, code int, msg string) {
	data, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{code, msg},
	})
	fmt.Println(string(data))
}

func textResult(id int64, text string) {
	respond(id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

func logAction(action string, details map[string]string) {
	entry := map[string]string{"time": time.Now().UTC().Format(time.RFC3339), "action": action}
	for k, v := range details {
		entry[k] = v
	}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "actions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func readFile(name string) string {
	data, _ := os.ReadFile(filepath.Join(dataDir, name))
	return string(data)
}

func handleToolCall(id int64, name string, args map[string]string) {
	logAction(name, args)

	switch name {
	case "get_sales":
		textResult(id, readFile("sales.json"))
	case "get_inventory":
		textResult(id, readFile("inventory.json"))
	case "get_reviews":
		textResult(id, readFile("reviews.json"))
	case "get_competitors":
		textResult(id, readFile("competitors.json"))
	case "get_analytics":
		textResult(id, readFile("analytics.json"))
	case "get_traffic_sources":
		textResult(id, readFile("traffic.json"))
	case "check_supplier":
		supplier := args["supplier"]
		if supplier == "" {
			supplier = "all"
		}
		textResult(id, readFile("suppliers.json"))
	case "add_product":
		product := args["name"]
		price := args["price"]
		category := args["category"]
		if product == "" || price == "" {
			respondError(id, -32602, "name and price required")
			return
		}
		if category == "" {
			category = "electronics"
		}
		textResult(id, fmt.Sprintf("New product listed: %s at $%s in %s. Live on storefront now. Needs inventory stocking.", product, price, category))
	case "adjust_price":
		product := args["product"]
		price := args["new_price"]
		if product == "" || price == "" {
			respondError(id, -32602, "product and new_price required")
			return
		}
		textResult(id, fmt.Sprintf("Price updated: %s → $%s. Change live immediately.", product, price))
	case "restock_item":
		product := args["product"]
		qty := args["quantity"]
		supplier := args["supplier"]
		if product == "" {
			respondError(id, -32602, "product required")
			return
		}
		if qty == "" {
			qty = "100"
		}
		// Check if product is from problematic supplier
		problematic := map[string]bool{"Wireless Earbuds Pro": true, "USB-C Hub 7-in-1": true, "Webcam 4K": true}
		if problematic[product] && supplier != "alt_supplier" {
			textResult(id, fmt.Sprintf("⚠️ WARNING: Restock order for %s placed with TechSource Direct (default supplier), but they have a 2-week delivery backlog. Use supplier=\"alt_supplier\" for faster delivery (3-5 days, +15%% cost). Current order ETA: 14-18 business days.", product, ))
		} else if supplier == "alt_supplier" {
			textResult(id, fmt.Sprintf("Restock via AltSupply Express: %s × %s units. Express delivery: 3-5 business days (+15%% surcharge).", product, qty))
		} else {
			textResult(id, fmt.Sprintf("Restock order placed: %s × %s units. Expected delivery: 2-3 business days.", product, qty))
		}
	case "send_promotion":
		subject := args["subject"]
		discount := args["discount"]
		target := args["target_segment"]
		if subject == "" {
			respondError(id, -32602, "subject required")
			return
		}
		if discount == "" {
			discount = "10%"
		}
		if target == "" {
			target = "all customers"
		}
		textResult(id, fmt.Sprintf("Promotion sent: \"%s\" (%s off) to %s. Estimated reach: 2,400 customers.", subject, discount, target))
	case "get_customer_segments":
		textResult(id, readFile("segments.json"))
	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("STORE_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		rid := *req.ID

		switch req.Method {
		case "initialize":
			respond(rid, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": "store", "version": "1.0.0"},
			})
		case "tools/list":
			respond(rid, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "get_sales",
						"description": "Get recent sales data with daily breakdown, product performance, and trends.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_inventory",
						"description": "Get current inventory levels for all products.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_reviews",
						"description": "Get recent customer reviews and feedback.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_competitors",
						"description": "Get competitor pricing for comparable products.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_analytics",
						"description": "Get website traffic and conversion analytics.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_traffic_sources",
						"description": "Get detailed traffic source breakdown — where visitors come from, referral URLs, trending search terms.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "check_supplier",
						"description": "Check supplier status, delivery backlog, and reliability. Reveals supply chain issues.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"supplier": map[string]string{"type": "string", "description": "Supplier name (omit for all)"},
							},
						},
					},
					{
						"name":        "get_customer_segments",
						"description": "Get customer segments with behavior data — who's buying, who stopped, who's new.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "add_product",
						"description": "List a new product on the storefront. Needs inventory stocking after listing.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":     map[string]string{"type": "string", "description": "Product name"},
								"price":    map[string]string{"type": "string", "description": "Price in dollars"},
								"category": map[string]string{"type": "string", "description": "Product category"},
							},
							"required": []string{"name", "price"},
						},
					},
					{
						"name":        "adjust_price",
						"description": "Change a product's price.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"product":   map[string]string{"type": "string", "description": "Product name"},
								"new_price": map[string]string{"type": "string", "description": "New price in dollars"},
							},
							"required": []string{"product", "new_price"},
						},
					},
					{
						"name":        "restock_item",
						"description": "Place a restock order for a product. Optionally specify a supplier.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"product":  map[string]string{"type": "string", "description": "Product to restock"},
								"quantity": map[string]string{"type": "string", "description": "Quantity to order (default 100)"},
								"supplier": map[string]string{"type": "string", "description": "Supplier name (default or alt_supplier for express)"},
							},
							"required": []string{"product"},
						},
					},
					{
						"name":        "send_promotion",
						"description": "Send a promotional email campaign to customers.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"subject":        map[string]string{"type": "string", "description": "Email subject line"},
								"discount":       map[string]string{"type": "string", "description": "Discount percentage (e.g. 15%)"},
								"target_segment": map[string]string{"type": "string", "description": "Customer segment (e.g. 'returning customers', 'all customers')"},
							},
							"required": []string{"subject"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(rid, -32602, "invalid params")
				continue
			}
			args := make(map[string]string)
			for k, v := range params.Arguments {
				switch val := v.(type) {
				case string:
					args[k] = val
				default:
					b, _ := json.Marshal(val)
					args[k] = string(b)
				}
			}
			handleToolCall(rid, params.Name, args)
		default:
			respondError(rid, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
