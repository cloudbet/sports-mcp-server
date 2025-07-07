package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const MCPProtocolVersion = "2025-06-18"

const UnauthenticatedApiBaseURL = "https://www.cloudbet.com/sports-api/c/v6/sports"

var primaryMarketsBySport = map[string][]string{
	"american-football": {"american_football.handicap", "american_football.moneyline", "american_football.totals"},
	"aussie-rules":      {"aussie_rules.handicap", "aussie_rules.match_odds", "aussie_rules.totals"},
	"baseball":          {"baseball.moneyline", "baseball.run_line", "baseball.totals"},
	"basketball":        {"basketball.handicap", "basketball.moneyline", "basketball.totals"},
	"boxing":            {"boxing.totals", "boxing.winner"},
	"cricket":           {"cricket.winner"},
	"esport-fifa":       {"esport_fifa.asian_handicap", "esport_fifa.match_odds", "esport_fifa.total_goals"},
	"ice-hockey":        {"ice_hockey.handicap", "ice_hockey.totals", "ice_hockey.winner"},
	"mma":               {"mma.totals", "mma.winner"},
	"soccer":            {"soccer.asian_handicap", "soccer.match_odds", "soccer.total_goals"},
	"tennis":            {"tennis.game_handicap", "tennis.total_games", "tennis.winner"},
}

type APIClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func (c *APIClient) FetchEventsForCompetition(competitionKey, sportKey string) (*CompetitionEventsResponse, error) {
	baseURL := fmt.Sprintf("%s/competitions/%s/events?include-pretrading=true&locale=en", c.BaseURL, competitionKey)
	var urlBuilder strings.Builder
	urlBuilder.WriteString(baseURL)
	if markets, ok := primaryMarketsBySport[sportKey]; ok {
		for _, market := range markets {
			urlBuilder.WriteString("&markets=")
			urlBuilder.WriteString(market)
		}
	}

	finalURL := urlBuilder.String()

	req, err := http.NewRequest("GET", finalURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("API returned non-200 status for competition events: %d - %s", res.StatusCode, string(body))
	}
	var competitionEvents CompetitionEventsResponse
	if err := json.NewDecoder(res.Body).Decode(&competitionEvents); err != nil {
		return nil, err
	}
	return &competitionEvents, nil
}

type CompetitionInfo struct{ Name, Key, SportKey string }

type SearchIndex struct {
	mu           sync.RWMutex
	competitions []CompetitionInfo
}

func buildSearchIndex(client *APIClient) (*SearchIndex, error) {
	sportsListReq, err := http.NewRequest("GET", client.BaseURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.HTTPClient.Do(sportsListReq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sports API returned non-200 for sports list: %d", res.StatusCode)
	}
	var sportsListResponse struct {
		Sports []struct {
			Key string `json:"key"`
		} `json:"sports"`
	}
	if err := json.NewDecoder(res.Body).Decode(&sportsListResponse); err != nil {
		return nil, err
	}

	log.Printf("Indexer: Found %d sports.", len(sportsListResponse.Sports))
	var wg sync.WaitGroup
	var mu sync.Mutex
	allCompetitions := make([]CompetitionInfo, 0)
	for _, sport := range sportsListResponse.Sports {
		wg.Add(1)
		go func(sportKey string) {
			defer wg.Done()
			sportDetailURL := fmt.Sprintf("%s/%s", client.BaseURL, sportKey)
			req, err := http.NewRequest("GET", sportDetailURL, nil)
			if err != nil {
				return
			}
			res, err := client.HTTPClient.Do(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return
			}
			var sportDetail SportDetail
			if err := json.NewDecoder(res.Body).Decode(&sportDetail); err != nil {
				return
			}
			mu.Lock()
			for _, category := range sportDetail.Categories {
				for _, competition := range category.Competitions {
					allCompetitions = append(allCompetitions, CompetitionInfo{Name: competition.Name, Key: competition.Key, SportKey: sportKey})
				}
			}
			mu.Unlock()
		}(sport.Key)
	}
	wg.Wait()
	index := &SearchIndex{competitions: allCompetitions}
	log.Printf("Bootstrap complete: Indexed %d competitions for search.", len(index.competitions))
	return index, nil
}

func (si *SearchIndex) Replace(data []CompetitionInfo) {
	si.mu.Lock()
	defer si.mu.Unlock()
	si.competitions = data
}

func (si *SearchIndex) Snapshot() []CompetitionInfo {
	si.mu.RLock()
	defer si.mu.RUnlock()
	return si.competitions
}

func (si *SearchIndex) FindBestMatch(query string) (CompetitionInfo, bool) {
	comps := si.Snapshot()
	bestMatch := CompetitionInfo{}
	minDistance := math.MaxInt32
	lowerQuery := strings.ToLower(query)

	for _, comp := range comps {
		dist := calculateLevenshtein(lowerQuery, strings.ToLower(comp.Name))
		if dist < minDistance {
			minDistance = dist
			bestMatch = comp
		}
	}
	if minDistance > (len(lowerQuery)/3 + 1) {
		return CompetitionInfo{}, false
	}
	return bestMatch, true
}

func calculateLevenshtein(a, b string) int {
	f := make([]int, len(b)+1)
	for j := range f {
		f[j] = j
	}
	for _, ca := range a {
		j, fj1 := 1, f[0]
		f[0]++
		for _, cb := range b {
			mn := f[j] + 1
			if cb == ca {
				mn = fj1
			}
			if f[j-1]+1 < mn {
				mn = f[j-1] + 1
			}
			fj1, f[j] = f[j], mn
			j++
		}
	}
	return f[len(b)]
}

// JSON-RPC generic structures
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP Tool-specific structures
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}
type ToolDefinition struct {
	Version     string           `json:"version,omitempty"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema any              `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}
type ToolResult struct {
	Content           []ContentItem `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Tool struct {
	Def     ToolDefinition
	Handler func(params json.RawMessage) (any, error) // Return structured content and an execution error
}
type ToolRegistry map[string]Tool

// Structs for decoding API data
type SportDetail struct {
	Categories []Category `json:"categories"`
}
type Category struct {
	Competitions []Competition `json:"competitions"`
}
type Competition struct{ Name, Key string }
type RawCloudbetEvent struct {
	ID   int64 `json:"id"`
	Home struct {
		Name string `json:"name"`
	} `json:"home"`
	Away struct {
		Name string `json:"name"`
	} `json:"away"`
	Type       string            `json:"type"`
	CutoffTime string            `json:"cutoffTime"`
	Markets    map[string]Market `json:"markets"`
}
type MCPEventResponse struct {
	ID               int64 `json:"id"`
	Name, CutoffTime string
	Markets          map[string]Market `json:"markets"`
}
type CompetitionEventsResponse struct {
	Events []RawCloudbetEvent `json:"events"`
}
type Market struct {
	Submarkets map[string]Submarket `json:"submarkets"`
}
type Submarket struct {
	Selections []Selection `json:"selections"`
}
type Selection struct {
	Outcome, Params, MarketURL string
	Price, MaxStake            float64
}

func createJSONRPCHandler(registry ToolRegistry, includeTextContent bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST requests are accepted", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}

		protocolVersion := r.Header.Get("MCP-Protocol-Version")
		if protocolVersion == "" {
			protocolVersion = "unspecified"
		}
		// log.Printf("Received request with MCP-Protocol-Version: %s", protocolVersion)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			sendJSONRPCError(w, nil, -32700, "Parse error")
			return
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			sendJSONRPCError(w, nil, -32700, "Parse error")
			return
		}

		switch req.Method {
		case "initialize":
			handleInitialize(w, req)
		case "describe":
			handleDescribe(w, req, registry)
		case "tools/list":
			handleListTools(w, req, registry)
		case "tools/call":
			handleCallTool(w, req, registry, includeTextContent)
		case "tools/observe":
			sendJSONRPCError(w, req.ID, -32601, "tools/observe not implemented")

		default:
			sendJSONRPCError(w, req.ID, -32601, "Method not found")
		}
	}
}

func handleInitialize(w http.ResponseWriter, req JSONRPCRequest) {
	response := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"capabilities": map[string]bool{
				"tools":     true,
				"prompts":   false,
				"resources": false,
			},
			"methods": []string{
				"initialize",
				"describe",
				"tools/list",
				"tools/call",
			},
			"server": map[string]any{
				"name":    "Cloudbet MCP Server",
				"version": "1.0.0",
			},
		},
	}
	writeJSON(w, http.StatusOK, response)
}

func handleDescribe(w http.ResponseWriter, req JSONRPCRequest, registry ToolRegistry) {
	var toolDefs []ToolDefinition
	for _, tool := range registry {
		toolDefs = append(toolDefs, tool.Def)
	}

	response := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"tools":     toolDefs,
			"resources": []any{},
			"prompts":   []any{},
		},
	}
	writeJSON(w, http.StatusOK, response)
}

func handleListTools(w http.ResponseWriter, req JSONRPCRequest, registry ToolRegistry) {
	var toolDefs []ToolDefinition
	for _, tool := range registry {
		toolDefs = append(toolDefs, tool.Def)
	}
	response := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": toolDefs},
	}
	writeJSON(w, http.StatusOK, response)
}

func handleCallTool(w http.ResponseWriter, req JSONRPCRequest, registry ToolRegistry, includeTextContent bool) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		sendJSONRPCError(w, req.ID, -32602, "Invalid params")
		return
	}

	tool, ok := registry[params.Name]
	if !ok {
		sendJSONRPCError(w, req.ID, -32602, fmt.Sprintf("Unknown tool: %s", params.Name))
		return
	}

	// Execute the tool handler.
	structuredResult, execErr := tool.Handler(params.Arguments)

	// Build the ToolResult based on the execution outcome.
	toolResult := ToolResult{}
	if execErr != nil {
		log.Printf("Tool '%s' execution error: %v", params.Name, execErr)
		toolResult.IsError = true
		toolResult.Content = []ContentItem{{Type: "text", Text: execErr.Error()}}
	} else {
		toolResult.IsError = false
		toolResult.StructuredContent = structuredResult
		// For backwards compatibility, also provide a text representation if enabled.
		if includeTextContent {
			textContent, _ := json.MarshalIndent(structuredResult, "", "  ")
			toolResult.Content = []ContentItem{{Type: "text", Text: string(textContent)}}
		}
	}

	response := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  toolResult,
	}
	writeJSON(w, http.StatusOK, response)
}

func makeFindEventsTool(client *APIClient, index *SearchIndex) Tool {
	return Tool{
		Def: ToolDefinition{
			Version:     "1.0.0",
			Name:        "findEventsAndMarketsByCompetition",
			Description: "Finds all upcoming events and their primary markets for a given competition using fuzzy search. This is the most efficient tool to get odds for a match.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"competitionName": map[string]string{"type": "string", "description": "e.g. 'Premier League', 'NBA', 'UFC', etc."},
					"limit":           map[string]string{"type": "integer", "description": "Max events to return"},
				},
				"required": []string{"competitionName"},
			},
			Annotations: &ToolAnnotations{Title: "Find Events by Competition", ReadOnlyHint: true, OpenWorldHint: true},
		},
		Handler: func(args json.RawMessage) (any, error) {
			var parsedArgs struct {
				CompetitionName string `json:"competitionName"`
				Limit           int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &parsedArgs); err != nil || parsedArgs.CompetitionName == "" {
				return nil, fmt.Errorf("invalid or missing 'competitionName' argument")
			}
			competitionInfo, found := index.FindBestMatch(parsedArgs.CompetitionName)
			if !found {
				return nil, fmt.Errorf("competition '%s' not found", parsedArgs.CompetitionName)
			}

			// log.Printf("Search success: Mapped '%s' to '%s' (key: %s, sport: %s)", parsedArgs.CompetitionName, competitionInfo.Name, competitionInfo.Key, competitionInfo.SportKey)
			rawCompetitionEvents, err := client.FetchEventsForCompetition(competitionInfo.Key, competitionInfo.SportKey)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch event data from the provider: %w", err)
			}
			var cleanEvents []MCPEventResponse
			for _, event := range rawCompetitionEvents.Events {
				if event.Type == "EVENT_TYPE_EVENT" {
					cleanEvent := MCPEventResponse{ID: event.ID, Name: fmt.Sprintf("%s vs. %s", event.Home.Name, event.Away.Name), CutoffTime: event.CutoffTime, Markets: event.Markets}
					cleanEvents = append(cleanEvents, cleanEvent)
					if parsedArgs.Limit > 0 && len(cleanEvents) >= parsedArgs.Limit {
						break
					}
				}
			}

			// log.Printf("Fetched %d raw events, returning %d after filtering for EVENT_TYPE_EVENT.", len(rawCompetitionEvents.Events), len(cleanEvents))
			return cleanEvents, nil
		},
	}
}

// makePlaceBetTool creates a tool for placing bets, requiring authentication via API key.
// If you self-host this server, you can set the API_KEY environment variable and implement the actual bet placement logic.
func makePlaceBetTool(_ string) Tool {
	return Tool{
		Def: ToolDefinition{
			Name: "placeBet", Description: "Places a bet on a specific market. Requires authentication.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"marketUrl": map[string]string{"type": "string"}, "stake": map[string]string{"type": "string"}}},
			Annotations: &ToolAnnotations{Title: "Place Bet", ReadOnlyHint: false, DestructiveHint: true, OpenWorldHint: true},
		},
		Handler: func(args json.RawMessage) (any, error) {
			log.Printf("Authenticated tool 'placeBet' not implemented yet.")
			return nil, fmt.Errorf("bet placement tool is defined but not yet implemented")
		},
	}
}

func sendJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	errResp := JSONRPCResponse{JSONRPC: "2.0", ID: id, Error: &JSONRPCError{Code: code, Message: message}}
	writeJSON(w, http.StatusOK, errResp) // Per spec, protocol errors are still 200 OK xD
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("Error writing JSON response: %v", err)
	}
}

func main() {
	includeTextContent := flag.Bool("include-text-content", false, "Include a stringified JSON in the 'content' field for backwards compatibility")
	port := flag.String("port", "8080", "Port to run the MCP server on")
	flag.Parse()

	if *includeTextContent {
		log.Println("Configuration: Backwards compatibility text content will be INCLUDED in tool call responses.")
	} else {
		log.Println("Configuration: Backwards compatibility text content will be OMITTED by default. Use -include-text-content to enable.")
	}

	client := &APIClient{BaseURL: UnauthenticatedApiBaseURL, HTTPClient: &http.Client{Timeout: 15 * time.Second}}

	index, err := buildSearchIndex(client)
	if err != nil {
		log.Fatalf("FATAL: Could not build search index on startup: %v", err)
	}

	// Reindex every hour to keep the search index up-to-date.
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for range t.C {
			newIndex, err := buildSearchIndex(client)
			if err != nil {
				log.Printf("Reindexing failed: %v", err)
				continue
			}
			index.Replace(newIndex.competitions)
		}
	}()

	registry := make(ToolRegistry)
	registry["findEventsAndMarketsByCompetition"] = makeFindEventsTool(client, index)

	if apiKey := os.Getenv("API_KEY"); apiKey != "" {
		log.Println("API_KEY detected. Enabling authenticated tools.")
		registry["placeBet"] = makePlaceBetTool(apiKey)
	} else {
		log.Println("No API_KEY configured. Running in unauthenticated mode.")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", createJSONRPCHandler(registry, *includeTextContent))
	mux.HandleFunc("/.well-known/mcp-server.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"methods": []string{
				"initialize", "describe", "tools/list", "tools/call",
			},
			"capabilities": map[string]bool{
				"tools": true, "resources": false, "prompts": false,
			},
		})
	})

	log.Printf("Starting MCP demo server on :%s", *port)

	server := &http.Server{Addr: fmt.Sprintf(":%s", *port), Handler: mux}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
