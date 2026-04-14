// MCP server for warehouse/inventory with hidden business rules.
// The agent must discover rules through trial and error.
// State in WAREHOUSE_DATA_DIR: stock.json, orders.json, shipments.json, audit.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

type Order struct {
	ID        string `json:"id"`
	Item      string `json:"item"`
	Qty       int    `json:"qty"`
	Certified bool   `json:"certified"`
	Status    string `json:"status"` // pending, fulfilled, failed
	Error     string `json:"error,omitempty"`
}

type Shipment struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	Destination string `json:"destination"`
	Weight      int    `json:"weight_kg"`
	CustomsForm bool   `json:"customs_form"`
	Status      string `json:"status"` // shipped, failed
	Error       string `json:"error,omitempty"`
}

var (
	dataDir   string
	stock     map[string]int
	orders    []Order
	shipments []Shipment
	nextOrder int
	nextShip  int
)

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

func audit(tool string, args map[string]string, result string) {
	entry := map[string]string{"time": time.Now().UTC().Format(time.RFC3339), "tool": tool, "result": result}
	for k, v := range args {
		entry["arg_"+k] = v
	}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadAll() {
	if d, err := os.ReadFile(filepath.Join(dataDir, "stock.json")); err == nil {
		json.Unmarshal(d, &stock)
	}
	if d, err := os.ReadFile(filepath.Join(dataDir, "orders.json")); err == nil {
		json.Unmarshal(d, &orders)
	}
	if d, err := os.ReadFile(filepath.Join(dataDir, "shipments.json")); err == nil {
		json.Unmarshal(d, &shipments)
	}
	nextOrder = len(orders) + 1
	nextShip = len(shipments) + 1
}

func saveOrders() {
	d, _ := json.MarshalIndent(orders, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "orders.json"), d, 0644)
}

func saveShipments() {
	d, _ := json.MarshalIndent(shipments, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "shipments.json"), d, 0644)
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "check_stock":
		item := args["item"]
		if item == "" {
			// Return all stock
			d, _ := json.Marshal(stock)
			audit(name, args, "ok")
			textResult(id, string(d))
			return
		}
		qty, ok := stock[item]
		if !ok {
			audit(name, args, "not found")
			textResult(id, fmt.Sprintf("item %q not found in stock", item))
			return
		}
		audit(name, args, "ok")
		textResult(id, fmt.Sprintf("%s: %d in stock", item, qty))

	case "place_order":
		item := args["item"]
		qtyStr := args["qty"]
		certified := args["certified"] == "true"

		if item == "" || qtyStr == "" {
			respondError(id, -32602, "item and qty are required")
			return
		}
		qty, _ := strconv.Atoi(qtyStr)

		// HIDDEN RULE 1: max 100 per order
		if qty > 100 {
			order := Order{ID: fmt.Sprintf("ORD-%03d", nextOrder), Item: item, Qty: qty, Status: "failed",
				Error: "REJECTED: Maximum quantity per order is 100 units. Split into multiple orders."}
			nextOrder++
			orders = append(orders, order)
			saveOrders()
			audit(name, args, "failed: qty>100")
			textResult(id, fmt.Sprintf("ORDER FAILED: %s — %s", order.ID, order.Error))
			return
		}

		// HIDDEN RULE 2: hazardous items need certified=true
		hazardous := map[string]bool{"chemicals": true, "batteries": true, "solvents": true, "explosives": true}
		itemLower := strings.ToLower(item)
		if hazardous[itemLower] && !certified {
			order := Order{ID: fmt.Sprintf("ORD-%03d", nextOrder), Item: item, Qty: qty, Status: "failed",
				Error: "REJECTED: Hazardous item requires certified=true. Contact safety team for certification."}
			nextOrder++
			orders = append(orders, order)
			saveOrders()
			audit(name, args, "failed: hazardous uncertified")
			textResult(id, fmt.Sprintf("ORDER FAILED: %s — %s", order.ID, order.Error))
			return
		}

		// Success
		order := Order{ID: fmt.Sprintf("ORD-%03d", nextOrder), Item: item, Qty: qty, Certified: certified, Status: "fulfilled"}
		nextOrder++
		orders = append(orders, order)
		saveOrders()
		audit(name, args, "ok")
		d, _ := json.Marshal(order)
		textResult(id, string(d))

	case "ship_item":
		orderID := args["order_id"]
		destination := args["destination"]
		weightStr := args["weight_kg"]
		customsForm := args["customs_form"] == "true"

		if orderID == "" || destination == "" {
			respondError(id, -32602, "order_id and destination are required")
			return
		}
		weight, _ := strconv.Atoi(weightStr)
		if weight == 0 {
			weight = 10 // default
		}

		// HIDDEN RULE 3: international needs customs_form=true
		international := map[string]bool{"japan": true, "germany": true, "brazil": true, "uk": true, "france": true, "china": true, "india": true}
		destLower := strings.ToLower(destination)
		if international[destLower] && !customsForm {
			ship := Shipment{ID: fmt.Sprintf("SHIP-%03d", nextShip), OrderID: orderID, Destination: destination,
				Weight: weight, Status: "failed",
				Error: "REJECTED: International shipments require customs_form=true."}
			nextShip++
			shipments = append(shipments, ship)
			saveShipments()
			audit(name, args, "failed: no customs form")
			textResult(id, fmt.Sprintf("SHIPMENT FAILED: %s — %s", ship.ID, ship.Error))
			return
		}

		// HIDDEN RULE 4: max weight 50kg
		if weight > 50 {
			ship := Shipment{ID: fmt.Sprintf("SHIP-%03d", nextShip), OrderID: orderID, Destination: destination,
				Weight: weight, Status: "failed",
				Error: fmt.Sprintf("REJECTED: Maximum shipment weight is 50kg. Your shipment weighs %dkg. Split into multiple shipments.", weight)}
			nextShip++
			shipments = append(shipments, ship)
			saveShipments()
			audit(name, args, "failed: overweight")
			textResult(id, fmt.Sprintf("SHIPMENT FAILED: %s — %s", ship.ID, ship.Error))
			return
		}

		// Success
		ship := Shipment{ID: fmt.Sprintf("SHIP-%03d", nextShip), OrderID: orderID, Destination: destination,
			Weight: weight, CustomsForm: customsForm, Status: "shipped"}
		nextShip++
		shipments = append(shipments, ship)
		saveShipments()
		audit(name, args, "ok")
		d, _ := json.Marshal(ship)
		textResult(id, string(d))

	case "get_rules":
		audit(name, args, "ok")
		textResult(id, "No formal rules documented. Policies are enforced at order/shipment time. Learn by doing — if an action is rejected, the error message will explain why.")

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("WAREHOUSE_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	stock = make(map[string]int)
	loadAll()

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
		id := *req.ID

		switch req.Method {
		case "initialize":
			respond(id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": "warehouse", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "check_stock",
						"description": "Check inventory stock levels. Pass an item name to check one, or omit for all.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"item": map[string]string{"type": "string", "description": "Item name (optional — omit for all)"},
							},
						},
					},
					{
						"name":        "place_order",
						"description": "Place an order for items from the warehouse.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"item":      map[string]string{"type": "string", "description": "Item to order"},
								"qty":       map[string]string{"type": "string", "description": "Quantity"},
								"certified": map[string]string{"type": "string", "description": "Safety certification (true/false)"},
							},
							"required": []string{"item", "qty"},
						},
					},
					{
						"name":        "ship_item",
						"description": "Ship a fulfilled order to a destination.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"order_id":     map[string]string{"type": "string", "description": "Order ID to ship"},
								"destination":  map[string]string{"type": "string", "description": "Destination (city or country name)"},
								"weight_kg":    map[string]string{"type": "string", "description": "Package weight in kg"},
								"customs_form": map[string]string{"type": "string", "description": "Include customs form (true/false)"},
							},
							"required": []string{"order_id", "destination"},
						},
					},
					{
						"name":        "get_rules",
						"description": "Get warehouse business rules and policies.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(id, -32602, "invalid params")
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
			handleToolCall(id, params.Name, args)
		default:
			respondError(id, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
