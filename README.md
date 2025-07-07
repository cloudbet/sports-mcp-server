# Cloudbet Sports MCP server

Single-file, minimal implementation of the [Model Context Protocol](https://modelcontextprotocol.io/introduction) (MCP) for sports data and betting tool exposure using the Cloudbet public API. This demo server follows the [MCP Server specification](https://modelcontextprotocol.io/specification/2025-03-26/server) and is designed for educational and demonstration purposes only. Please use responsibly and at your own risk.

1. **Run the Server:**

```sh
go run .
```

2. **List Tools (Describe):**

```sh
curl -X POST http://localhost:8080/ -H "Content-Type: application/json" -d '{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list",
  "params": {}
}' | jq .
```

3. **Call a Tool:**

```sh
curl -X POST http://localhost:8080/ -H "Content-Type: application/json" -d '{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "findEventsAndMarketsByCompetition",
    "arguments": {
      "competitionName": "Premier League"
    }
  }
}' | jq .
```
