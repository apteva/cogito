// MCP server for simulated market data, portfolio management, and trading.
// State in MARKET_DATA_DIR: prices.json, portfolio.json, orders.json
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

type PricePoint struct {
	Symbol    string  `json:"symbol"`
	Price     float64 `json:"price"`
	Timestamp string  `json:"timestamp"`
}

type Holding struct {
	Symbol   string  `json:"symbol"`
	Qty      float64 `json:"qty"`
	AvgCost  float64 `json:"avg_cost"`
	StopLoss float64 `json:"stop_loss,omitempty"`
}

type Order struct {
	ID        string  `json:"id"`
	Symbol    string  `json:"symbol"`
	Side      string  `json:"side"` // buy, sell
	Qty       float64 `json:"qty"`
	Price     float64 `json:"price"`
	Status    string  `json:"status"` // filled, open, cancelled
	Timestamp string  `json:"timestamp"`
	PnL       float64 `json:"pnl,omitempty"`
}

var (
	dataDir   string
	prices    map[string]float64   // current prices
	history   []PricePoint
	portfolio map[string]*Holding
	orders    []Order
	cash      float64
	orderSeq  int
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

func saveJSON(name string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(filepath.Join(dataDir, name), data, 0644)
}

func loadJSON(name string, v any) {
	data, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		return
	}
	json.Unmarshal(data, v)
}

func savePrices()    { saveJSON("prices.json", prices) }
func savePortfolio() {
	state := map[string]any{"cash": cash, "holdings": portfolio}
	saveJSON("portfolio.json", state)
}
func saveOrders()    { saveJSON("orders.json", orders) }
func saveHistory()   { saveJSON("history.json", history) }

func loadAll() {
	loadJSON("prices.json", &prices)
	loadJSON("orders.json", &orders)
	orderSeq = len(orders)

	// Load portfolio with cash
	var state struct {
		Cash     float64             `json:"cash"`
		Holdings map[string]*Holding `json:"holdings"`
	}
	loadJSON("portfolio.json", &state)
	if state.Holdings != nil {
		portfolio = state.Holdings
	}
	if state.Cash > 0 {
		cash = state.Cash
	}

	// Load history
	loadJSON("history.json", &history)
}

// checkStopLosses triggers sells for holdings below stop-loss.
func checkStopLosses() []string {
	var triggered []string
	for symbol, h := range portfolio {
		if h.StopLoss <= 0 || h.Qty <= 0 {
			continue
		}
		price, ok := prices[symbol]
		if !ok {
			continue
		}
		if price <= h.StopLoss {
			// Auto-sell
			pnl := (price - h.AvgCost) * h.Qty
			orderSeq++
			orders = append(orders, Order{
				ID: fmt.Sprintf("O-%d", orderSeq), Symbol: symbol, Side: "sell",
				Qty: h.Qty, Price: price, Status: "filled",
				Timestamp: time.Now().UTC().Format(time.RFC3339), PnL: pnl,
			})
			cash += price * h.Qty
			triggered = append(triggered, fmt.Sprintf("STOP-LOSS: sold %.2f %s @ $%.2f (P&L: $%.2f)", h.Qty, symbol, price, pnl))
			delete(portfolio, symbol)
		}
	}
	if len(triggered) > 0 {
		savePortfolio()
		saveOrders()
	}
	return triggered
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "get_prices":
		symbols := args["symbols"] // comma-separated or empty for all
		result := make(map[string]float64)
		if symbols == "" {
			result = prices
		} else {
			for _, s := range splitCSV(symbols) {
				if p, ok := prices[s]; ok {
					result[s] = p
				}
			}
		}
		// Check stop-losses on price read
		triggered := checkStopLosses()
		data, _ := json.Marshal(result)
		text := string(data)
		if len(triggered) > 0 {
			for _, t := range triggered {
				text += "\n" + t
			}
		}
		textResult(id, text)

	case "get_history":
		symbol := args["symbol"]
		periods, _ := strconv.Atoi(args["periods"])
		if symbol == "" {
			respondError(id, -32602, "symbol is required")
			return
		}
		if periods <= 0 {
			periods = 20
		}
		var result []PricePoint
		for _, pt := range history {
			if pt.Symbol == symbol {
				result = append(result, pt)
			}
		}
		if len(result) > periods {
			result = result[len(result)-periods:]
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	case "place_order":
		symbol := args["symbol"]
		side := args["side"]
		qty, _ := strconv.ParseFloat(args["qty"], 64)
		if symbol == "" || side == "" || qty <= 0 {
			respondError(id, -32602, "symbol, side, and qty are required")
			return
		}
		price, ok := prices[symbol]
		if !ok {
			textResult(id, fmt.Sprintf("ERROR: unknown symbol %s", symbol))
			return
		}

		orderSeq++
		order := Order{
			ID: fmt.Sprintf("O-%d", orderSeq), Symbol: symbol, Side: side,
			Qty: qty, Price: price, Status: "filled",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}

		if side == "buy" {
			cost := price * qty
			if cost > cash {
				textResult(id, fmt.Sprintf("ERROR: insufficient cash ($%.2f needed, $%.2f available)", cost, cash))
				return
			}
			cash -= cost
			h, exists := portfolio[symbol]
			if exists {
				totalQty := h.Qty + qty
				h.AvgCost = (h.AvgCost*h.Qty + price*qty) / totalQty
				h.Qty = totalQty
			} else {
				portfolio[symbol] = &Holding{Symbol: symbol, Qty: qty, AvgCost: price}
			}
		} else if side == "sell" {
			h, exists := portfolio[symbol]
			if !exists || h.Qty < qty {
				textResult(id, fmt.Sprintf("ERROR: insufficient holdings (have %.2f, selling %.2f)", 0.0, qty))
				return
			}
			order.PnL = (price - h.AvgCost) * qty
			cash += price * qty
			h.Qty -= qty
			if h.Qty <= 0 {
				delete(portfolio, symbol)
			}
		}

		orders = append(orders, order)
		saveOrders()
		savePortfolio()
		textResult(id, fmt.Sprintf("OK: %s %.2f %s @ $%.2f (order %s, cash=$%.2f)", side, qty, symbol, price, order.ID, cash))

	case "get_portfolio":
		var totalValue float64
		var totalPnL float64
		var holdings []map[string]any
		for _, h := range portfolio {
			price := prices[h.Symbol]
			value := price * h.Qty
			pnl := (price - h.AvgCost) * h.Qty
			totalValue += value
			totalPnL += pnl
			holdings = append(holdings, map[string]any{
				"symbol": h.Symbol, "qty": h.Qty, "avg_cost": h.AvgCost,
				"current_price": price, "value": value, "pnl": pnl,
				"stop_loss": h.StopLoss,
			})
		}
		result := map[string]any{
			"cash": cash, "holdings": holdings,
			"total_value": totalValue + cash, "unrealized_pnl": totalPnL,
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	case "get_orders":
		data, _ := json.Marshal(orders)
		textResult(id, string(data))

	case "set_stop_loss":
		symbol := args["symbol"]
		price, _ := strconv.ParseFloat(args["price"], 64)
		if symbol == "" {
			respondError(id, -32602, "symbol is required")
			return
		}
		h, ok := portfolio[symbol]
		if !ok {
			textResult(id, fmt.Sprintf("ERROR: no holding for %s", symbol))
			return
		}
		h.StopLoss = price
		savePortfolio()
		textResult(id, fmt.Sprintf("OK: stop-loss for %s set at $%.2f", symbol, price))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func splitCSV(s string) []string {
	var result []string
	for _, p := range splitOn(s, ',') {
		p = trim(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func splitOn(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func main() {
	dataDir = os.Getenv("MARKET_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	prices = make(map[string]float64)
	portfolio = make(map[string]*Holding)
	cash = 10000 // default starting cash
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
				"serverInfo":     map[string]string{"name": "market", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "get_prices",
						"description": "Get current prices for symbols. Empty for all. Also checks stop-losses.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"symbols": map[string]string{"type": "string", "description": "Comma-separated symbols (empty for all)"},
							},
						},
					},
					{
						"name":        "get_history",
						"description": "Get price history for a symbol (last N periods).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"symbol":  map[string]string{"type": "string", "description": "Symbol"},
								"periods": map[string]string{"type": "string", "description": "Number of periods (default 20)"},
							},
							"required": []string{"symbol"},
						},
					},
					{
						"name":        "place_order",
						"description": "Place a buy or sell order. Executes immediately at current price.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"symbol": map[string]string{"type": "string", "description": "Symbol to trade"},
								"side":   map[string]string{"type": "string", "description": "buy or sell"},
								"qty":    map[string]string{"type": "string", "description": "Quantity"},
							},
							"required": []string{"symbol", "side", "qty"},
						},
					},
					{
						"name":        "get_portfolio",
						"description": "Get current portfolio: cash, holdings, total value, unrealized P&L.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_orders",
						"description": "Get order history.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "set_stop_loss",
						"description": "Set a stop-loss price for a holding. Auto-sells if price drops to this level.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"symbol": map[string]string{"type": "string", "description": "Symbol"},
								"price":  map[string]string{"type": "string", "description": "Stop-loss trigger price"},
							},
							"required": []string{"symbol", "price"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(id, -32602, "invalid params")
				continue
			}
			handleToolCall(id, params.Name, params.Arguments)
		default:
			respondError(id, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
