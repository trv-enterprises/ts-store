// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package main is the entry point for the tsstore CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/handlers"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/internal/unixsock"
	"github.com/tviviano/ts-store/pkg/store"
)

const (
	defaultConfigPath = "config.json"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "serve":
		runServer(os.Args[2:])
	case "create":
		runCreateCommand(os.Args[2:])
	case "key":
		if len(os.Args) < 3 {
			printKeyUsage()
			os.Exit(1)
		}
		runKeyCommand(os.Args[2:])
	case "swagger":
		runSwaggerCommand()
	case "calc":
		runCalcCommand(os.Args[2:])
	case "status":
		runStatusCommand(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	case "version", "-v", "--version":
		fmt.Println("tsstore v0.3.1-rc1")
	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`tsstore - Time Series Store Server

Usage:
  tsstore <command> [arguments]

Commands:
  serve     Start the API server
  create    Create a new store
  status    Show status of all stores
  key       Manage API keys (requires device access)
  calc      Calculate storage footprint
  swagger   Open Swagger UI in browser to explore the API
  help      Show this help message
  version   Show version

Use "tsstore <command> -h" for more information about a command.`)
}

func printServeUsage() {
	fmt.Println(`tsstore serve - Start the API server

Usage:
  tsstore serve [options]

Options:
  --no-socket    Disable Unix socket listener
  --socket <path> Override Unix socket path

Environment Variables:
  TSSTORE_ADMIN_KEY    Admin key for store creation (required, min 20 chars)
  TSSTORE_HOST         Server host (default: 0.0.0.0)
  TSSTORE_PORT         Server port (default: 21080)
  TSSTORE_MODE         Server mode: debug or release (default: release)
  TSSTORE_DATA_PATH    Base path for store data (default: ./data)
  TSSTORE_SOCKET_PATH  Unix socket path (default: /var/run/tsstore/tsstore.sock)
  TSSTORE_TLS_CERT     Path to TLS certificate file (enables HTTPS if set with TLS_KEY)
  TSSTORE_TLS_KEY      Path to TLS private key file (enables HTTPS if set with TLS_CERT)

TLS:
  If both TSSTORE_TLS_CERT and TSSTORE_TLS_KEY are provided, the server will use
  HTTPS. Otherwise, it falls back to HTTP. WebSocket connections (ws://, wss://)
  automatically use the same protocol as the HTTP server.`)
}

func printCreateUsage() {
	fmt.Println(`tsstore create - Create a new store

Usage:
  tsstore create <store-name> [options]

Options:
  --blocks <n>       Number of primary blocks (default: 1024)
  --data-size <n>    Data block size in bytes, must be power of 2 (default: 4096)
  --index-size <n>   Index block size in bytes, must be power of 2 (default: 4096)
  --path <dir>       Base directory for stores (default: ./data or TSSTORE_DATA_PATH)
  --type <type>      Data type: binary, text, json, schema (default: json)

Examples:
  tsstore create my-store
  tsstore create sensors --blocks 10000 --data-size 8192
  tsstore create logs --path /var/tsstore
  tsstore create metrics --type schema`)
}

func printKeyUsage() {
	fmt.Println(`tsstore key - Manage API keys

Usage:
  tsstore key <subcommand> [arguments]

Subcommands:
  regenerate <store-name>  Regenerate API key for a store (revokes all existing keys)
  list <store-name>        List API keys for a store (shows IDs, not keys)
  revoke <store-name> <key-id>  Revoke a specific key by ID

Examples:
  tsstore key regenerate my-store
  tsstore key list my-store
  tsstore key revoke my-store a1b2c3d4`)
}

func runServer(args []string) {
	// Parse serve options
	noSocket := false
	socketPathOverride := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			printServeUsage()
			return
		case "--no-socket":
			noSocket = true
		case "--socket":
			if i+1 < len(args) {
				i++
				socketPathOverride = args[i]
			}
		}
	}

	// Load configuration
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Apply command-line overrides
	if noSocket {
		cfg.Server.SocketPath = ""
	} else if socketPathOverride != "" {
		cfg.Server.SocketPath = socketPathOverride
	}

	// Validate admin key
	if cfg.Server.AdminKey == "" {
		log.Fatal("Admin key required: set TSSTORE_ADMIN_KEY environment variable or admin_key in config")
	}
	if len(cfg.Server.AdminKey) < 20 {
		log.Fatal("Admin key must be at least 20 characters")
	}

	// Validate TLS configuration if partially provided
	if (cfg.Server.TLS.CertFile != "") != (cfg.Server.TLS.KeyFile != "") {
		log.Fatal("TLS requires both cert and key: set both TSSTORE_TLS_CERT and TSSTORE_TLS_KEY")
	}
	if cfg.TLSEnabled() {
		// Verify cert and key files exist
		if _, err := os.Stat(cfg.Server.TLS.CertFile); os.IsNotExist(err) {
			log.Fatalf("TLS certificate file not found: %s", cfg.Server.TLS.CertFile)
		}
		if _, err := os.Stat(cfg.Server.TLS.KeyFile); os.IsNotExist(err) {
			log.Fatalf("TLS key file not found: %s", cfg.Server.TLS.KeyFile)
		}
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.Store.BasePath, 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// Initialize components
	keyManager := apikey.NewManager(cfg.Store.BasePath)
	storeService := service.NewStoreService(cfg, keyManager)

	// Setup Gin
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.CORS())
	router.Use(middleware.RequestLogger())

	// Health check (no auth required)
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Initialize handlers
	storeHandler := handlers.NewStoreHandler(storeService)
	unifiedHandler := handlers.NewUnifiedHandler(storeService)
	schemaHandler := handlers.NewSchemaHandler(storeService)
	wsHandler := handlers.NewWSHandler(storeService)
	wsConnHandler := handlers.NewWSConnectionsHandler(storeService.GetWSManager)
	mqttHandler := handlers.NewMQTTHandler(storeService.GetMQTTManager)

	// API routes
	api := router.Group("/api")
	{
		// Store management
		stores := api.Group("/stores")
		{
			stores.POST("", middleware.AdminAuth(cfg.Server.AdminKey), storeHandler.Create) // Create new store (requires admin key)
			stores.GET("", storeHandler.List)                                               // List open stores (no auth)
		}

		// Store-specific operations (require auth)
		storeRoutes := stores.Group("/:store")
		storeRoutes.Use(middleware.Auth(keyManager))
		{
			storeRoutes.DELETE("", storeHandler.Delete)
			storeRoutes.POST("/reset", storeHandler.Reset)
			storeRoutes.GET("/stats", storeHandler.Stats)

			// Unified data endpoint
			// Content-Type determines format:
			//   - application/octet-stream: binary data
			//   - text/plain: UTF-8 text
			//   - application/json: JSON data
			data := storeRoutes.Group("/data")
			{
				data.POST("", unifiedHandler.Put)
				data.GET("/time/:timestamp", unifiedHandler.GetByTime)
				data.DELETE("/time/:timestamp", unifiedHandler.DeleteByTime)
				data.GET("/oldest", unifiedHandler.ListOldest)
				data.GET("/newest", unifiedHandler.ListNewest) // Supports ?since=2h
				data.GET("/range", unifiedHandler.ListRange)   // Supports ?since=2h or ?start_time=X&end_time=Y
			}

			// Schema endpoint (only for schema-type stores)
			storeRoutes.GET("/schema", schemaHandler.Get)
			storeRoutes.PUT("/schema", schemaHandler.Put)

			// WebSocket endpoint (inbound connections)
			// Auth is via query param for WebSocket connections
			storeRoutes.GET("/ws/write", wsHandler.Write)

			// Outbound connection management
			wsConns := storeRoutes.Group("/ws/connections")
			{
				wsConns.GET("", wsConnHandler.List)
				wsConns.POST("", wsConnHandler.Create)
				wsConns.GET("/:id", wsConnHandler.Get)
				wsConns.DELETE("/:id", wsConnHandler.Delete)
			}

			// MQTT sink connections
			mqttConns := storeRoutes.Group("/mqtt/connections")
			{
				mqttConns.GET("", mqttHandler.List)
				mqttConns.POST("", mqttHandler.Create)
				mqttConns.GET("/:id", mqttHandler.Get)
				mqttConns.DELETE("/:id", mqttHandler.Delete)
			}
		}
	}

	// Create server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine (HTTPS if TLS configured, HTTP otherwise)
	go func() {
		if cfg.TLSEnabled() {
			log.Printf("Starting tsstore server on %s (HTTPS)", addr)
			if err := srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error: %v", err)
			}
		} else {
			log.Printf("Starting tsstore server on %s (HTTP)", addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error: %v", err)
			}
		}
	}()

	// Start Unix socket listener if configured
	var sockListener *unixsock.Listener
	if cfg.Server.SocketPath != "" {
		sockListener = unixsock.NewListener(cfg.Server.SocketPath, storeService, keyManager)
		if err := sockListener.Start(); err != nil {
			log.Printf("Warning: Unix socket listener failed to start: %v", err)
			sockListener = nil
		}
	}

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Stop Unix socket listener
	if sockListener != nil {
		if err := sockListener.Stop(); err != nil {
			log.Printf("Error stopping Unix socket listener: %v", err)
		}
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	// Close all stores
	if err := storeService.CloseAll(); err != nil {
		log.Printf("Error closing stores: %v", err)
	}

	log.Println("Server stopped")
}

func runCreateCommand(args []string) {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" {
		printCreateUsage()
		if len(args) < 1 {
			os.Exit(1)
		}
		return
	}

	storeName := args[0]

	// Parse options
	numBlocks := uint32(1024)
	dataBlockSize := uint32(4096)
	indexBlockSize := uint32(4096)
	basePath := ""
	dataType := "json"

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--blocks":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					numBlocks = uint32(n)
				}
			}
		case "--data-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					dataBlockSize = uint32(n)
				}
			}
		case "--index-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					indexBlockSize = uint32(n)
				}
			}
		case "--path":
			if i+1 < len(args) {
				i++
				basePath = args[i]
			}
		case "--type":
			if i+1 < len(args) {
				i++
				dataType = args[i]
			}
		}
	}

	// Load config for defaults
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Override base path if specified
	if basePath != "" {
		cfg.Store.BasePath = basePath
	}

	// Ensure base directory exists
	if err := os.MkdirAll(cfg.Store.BasePath, 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// Create the store
	keyManager := apikey.NewManager(cfg.Store.BasePath)
	storeService := service.NewStoreService(cfg, keyManager)

	req := &service.CreateStoreRequest{
		Name:           storeName,
		NumBlocks:      numBlocks,
		DataBlockSize:  dataBlockSize,
		IndexBlockSize: indexBlockSize,
		DataType:       dataType,
	}

	resp, err := storeService.Create(req)
	if err != nil {
		log.Fatalf("Failed to create store: %v", err)
	}

	// Close the store
	storeService.CloseAll()

	fmt.Println("=== STORE CREATED ===")
	fmt.Printf("Name:        %s\n", resp.Name)
	fmt.Printf("Path:        %s/%s\n", cfg.Store.BasePath, resp.Name)
	fmt.Printf("Data Type:   %s\n", dataType)
	fmt.Printf("Blocks:      %d\n", numBlocks)
	fmt.Printf("Data Size:   %d bytes\n", dataBlockSize)
	fmt.Printf("Index Size:  %d bytes\n", indexBlockSize)
	fmt.Println("")
	fmt.Printf("Key ID:      %s\n", resp.KeyID)
	fmt.Printf("API Key:     %s\n", resp.APIKey)
	fmt.Println("")
	fmt.Println("WARNING: The API key is shown only once. Save it securely!")
}

func runKeyCommand(args []string) {
	if len(args) < 1 {
		printKeyUsage()
		os.Exit(1)
	}

	// Load config to get base path
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	keyManager := apikey.NewManager(cfg.Store.BasePath)

	subCommand := args[0]
	switch subCommand {
	case "regenerate":
		if len(args) < 2 {
			fmt.Println("Error: store name required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]

		// Regenerate key
		newKey, entry, err := keyManager.Regenerate(storeName, "Regenerated via CLI")
		if err != nil {
			log.Fatalf("Failed to regenerate key: %v", err)
		}

		fmt.Println("=== NEW API KEY ===")
		fmt.Printf("Store:   %s\n", storeName)
		fmt.Printf("Key ID:  %s\n", entry.ID)
		fmt.Printf("API Key: %s\n", newKey)
		fmt.Println("")
		fmt.Println("WARNING: This key is shown only once. Save it securely!")
		fmt.Println("All previous keys have been revoked.")

	case "list":
		if len(args) < 2 {
			fmt.Println("Error: store name required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]

		keys, err := keyManager.List(storeName)
		if err != nil {
			log.Fatalf("Failed to list keys: %v", err)
		}

		if len(keys) == 0 {
			fmt.Printf("No API keys found for store '%s'\n", storeName)
			return
		}

		fmt.Printf("API keys for store '%s':\n", storeName)
		fmt.Println("ID        Created                    Note")
		fmt.Println("--------  -------------------------  ----")
		for _, k := range keys {
			fmt.Printf("%-8s  %-25s  %s\n",
				k.ID,
				k.CreatedAt.Format("2006-01-02 15:04:05 MST"),
				k.Note)
		}

	case "revoke":
		if len(args) < 3 {
			fmt.Println("Error: store name and key ID required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]
		keyID := args[2]

		if err := keyManager.Revoke(storeName, keyID); err != nil {
			log.Fatalf("Failed to revoke key: %v", err)
		}

		fmt.Printf("Key '%s' revoked for store '%s'\n", keyID, storeName)

	default:
		fmt.Printf("Unknown key subcommand: %s\n", subCommand)
		printKeyUsage()
		os.Exit(1)
	}
}

func runSwaggerCommand() {
	const swaggerPort = 21090
	const swaggerEditorURL = "https://editor.swagger.io"

	// Find swagger.yaml - check current dir, then relative to executable
	swaggerPath := "swagger.yaml"
	if _, err := os.Stat(swaggerPath); os.IsNotExist(err) {
		// Try relative to executable
		execPath, _ := os.Executable()
		if execPath != "" {
			swaggerPath = filepath.Join(filepath.Dir(execPath), "swagger.yaml")
		}
	}

	swaggerContent, err := os.ReadFile(swaggerPath)
	if err != nil {
		log.Fatalf("Failed to read swagger.yaml: %v\nMake sure swagger.yaml is in the current directory or next to the executable.", err)
	}

	// Create HTTP server with CORS
	mux := http.NewServeMux()
	mux.HandleFunc("/swagger.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "application/yaml")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Write(swaggerContent)
	})

	addr := fmt.Sprintf("localhost:%d", swaggerPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server in background
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Swagger server error: %v", err)
		}
	}()

	// Give server a moment to start
	time.Sleep(100 * time.Millisecond)

	// Build URL with spec location
	specURL := fmt.Sprintf("http://localhost:%d/swagger.yaml", swaggerPort)
	editorURL := fmt.Sprintf("%s/?url=%s", swaggerEditorURL, specURL)

	fmt.Printf("Serving swagger.yaml on http://%s/swagger.yaml\n", addr)
	fmt.Printf("Opening Swagger Editor...\n")
	fmt.Printf("Press Ctrl+C to stop\n\n")

	// Open browser
	openBrowser(editorURL)

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("\nShutting down swagger server...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// openBrowser opens the specified URL in the default browser
func openBrowser(url string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		fmt.Printf("Please open manually: %s\n", url)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Printf("Failed to open browser: %v\nPlease open manually: %s\n", err, url)
	}
}

func printCalcUsage() {
	fmt.Println(`tsstore calc - Calculate storage footprint

Usage:
  tsstore calc [options]

Options:
  --blocks <n>       Number of blocks (default: from config or 1024)
  --block-size <n>   Data block size in bytes (default: from config or 4096)
  --index-size <n>   Index block size in bytes (default: from config or 4096)
  --object-size <n>  Calculate capacity for specific object size in bytes

If no options provided, reads defaults from config file.

Examples:
  tsstore calc --blocks 10000 --block-size 4096
  tsstore calc --object-size 200
  tsstore calc`)
}

func runCalcCommand(args []string) {
	// Parse options
	numBlocks := uint32(0)
	blockSize := uint32(0)
	indexBlockSize := uint32(0)
	objectSize := uint32(0)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			printCalcUsage()
			return
		case "--blocks":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					numBlocks = uint32(n)
				}
			}
		case "--block-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					blockSize = uint32(n)
				}
			}
		case "--index-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					indexBlockSize = uint32(n)
				}
			}
		case "--object-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					objectSize = uint32(n)
				}
			}
		}
	}

	// Load config for defaults
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Apply defaults from config if not specified
	if numBlocks == 0 {
		numBlocks = cfg.Store.NumBlocks
	}
	if blockSize == 0 {
		blockSize = cfg.Store.DataBlockSize
	}
	if indexBlockSize == 0 {
		indexBlockSize = cfg.Store.IndexBlockSize
	}

	// Constants
	const indexEntrySize = 16 // 8-byte timestamp + 4-byte block num + 4-byte reserved
	const metadataSize = 64   // meta.tsdb size

	// Calculate sizes
	dataFileSize := uint64(numBlocks) * uint64(blockSize)
	indexFileSize := uint64(numBlocks) * uint64(indexEntrySize)
	totalSize := dataFileSize + indexFileSize + metadataSize

	// Print results
	fmt.Println("=== Storage Footprint ===")
	fmt.Println()
	fmt.Printf("Configuration:\n")
	fmt.Printf("  Blocks:           %s\n", formatNumber(uint64(numBlocks)))
	fmt.Printf("  Data block size:  %s bytes\n", formatNumber(uint64(blockSize)))
	fmt.Printf("  Index entry size: %d bytes\n", indexEntrySize)
	fmt.Println()
	fmt.Printf("Files:\n")
	fmt.Printf("  data.tsdb:   %s × %s = %s (%s)\n",
		formatNumber(uint64(numBlocks)),
		formatNumber(uint64(blockSize)),
		formatNumber(dataFileSize),
		formatBytes(dataFileSize))
	fmt.Printf("  index.tsdb:  %s × %d = %s (%s)\n",
		formatNumber(uint64(numBlocks)),
		indexEntrySize,
		formatNumber(indexFileSize),
		formatBytes(indexFileSize))
	fmt.Printf("  meta.tsdb:   %d bytes\n", metadataSize)
	fmt.Println()
	fmt.Printf("Total footprint: %s (%s)\n", formatNumber(totalSize), formatBytes(totalSize))

	// Object capacity estimates
	// Each object has a 24-byte ObjectHeader, and blocks have 24-byte BlockHeader
	// Usable space per block = blockSize - 24 (BlockHeader)
	// Each object needs: ObjectHeader (24) + data
	const blockHeaderSize = 24
	const objectHeaderSize = 24
	usablePerBlock := blockSize - blockHeaderSize

	// Object capacity calculation
	fmt.Println()
	if objectSize > 0 {
		// Single object size specified
		fmt.Printf("Object capacity for %d byte objects:\n", objectSize)
		totalObjSize := objectSize + objectHeaderSize
		if totalObjSize > usablePerBlock {
			firstBlockData := usablePerBlock - objectHeaderSize
			remaining := objectSize - firstBlockData
			contBlocks := (remaining + usablePerBlock - 1) / usablePerBlock
			blocksPerObject := 1 + contBlocks
			totalObjects := uint64(numBlocks) / uint64(blocksPerObject)
			fmt.Printf("  Blocks per object: %d\n", blocksPerObject)
			fmt.Printf("  Total objects:     %s\n", formatNumber(totalObjects))
		} else {
			objectsPerBlock := usablePerBlock / totalObjSize
			totalObjects := uint64(numBlocks) * uint64(objectsPerBlock)
			fmt.Printf("  Objects per block: %d\n", objectsPerBlock)
			fmt.Printf("  Total objects:     %s\n", formatNumber(totalObjects))
		}
	} else {
		// Show table of common sizes
		fmt.Println("Estimated object capacity:")
		fmt.Println("  Object Size    Objects/Block    Total Objects")
		fmt.Println("  -----------    -------------    -------------")

		objectSizes := []uint32{64, 128, 256, 512, 1024, 2048}
		for _, objSize := range objectSizes {
			totalObjSize := objSize + objectHeaderSize
			if totalObjSize > usablePerBlock {
				firstBlockData := usablePerBlock - objectHeaderSize
				remaining := objSize - firstBlockData
				contBlocks := (remaining + usablePerBlock - 1) / usablePerBlock
				blocksPerObject := 1 + contBlocks
				totalObjects := uint64(numBlocks) / uint64(blocksPerObject)
				fmt.Printf("  %5d bytes    %13s    %13s (spans %d blocks)\n",
					objSize, "<1", formatNumber(totalObjects), blocksPerObject)
			} else {
				objectsPerBlock := usablePerBlock / totalObjSize
				totalObjects := uint64(numBlocks) * uint64(objectsPerBlock)
				fmt.Printf("  %5d bytes    %13d    %13s\n",
					objSize, objectsPerBlock, formatNumber(totalObjects))
			}
		}
	}
}

// formatNumber formats a number with comma separators
func formatNumber(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// formatBytes formats bytes as human-readable (KB, MB, GB)
func formatBytes(bytes uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// mqttConnectionForStatus is a minimal struct for reading MQTT config
type mqttConnectionForStatus struct {
	ID        string `json:"id"`
	BrokerURL string `json:"broker_url"`
	Topic     string `json:"topic"`
}

// loadMQTTConnections reads MQTT connections config from a store directory.
func loadMQTTConnections(storePath string) []mqttConnectionForStatus {
	configPath := filepath.Join(storePath, "mqtt_connections.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}

	var config struct {
		Connections []mqttConnectionForStatus `json:"connections"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil
	}
	return config.Connections
}

func printStatusUsage() {
	fmt.Println(`tsstore status - Show status of all stores

Usage:
  tsstore status [options]

Options:
  --path <dir>   Base directory for stores (default: ./data or TSSTORE_DATA_PATH)
  --json         Output in JSON format
  --offline      Skip server check, only read files directly

The command will attempt to connect to the running server (via config) to get
runtime information like active connections. If the server is not running,
it falls back to reading store files directly.

Examples:
  tsstore status
  tsstore status --path /var/tsstore
  tsstore status --json
  tsstore status --offline`)
}

func runStatusCommand(args []string) {
	// Parse options
	basePath := ""
	jsonOutput := false
	offlineMode := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			printStatusUsage()
			return
		case "--path":
			if i+1 < len(args) {
				i++
				basePath = args[i]
			}
		case "--json":
			jsonOutput = true
		case "--offline":
			offlineMode = true
		}
	}

	// Load config for defaults
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Override base path if specified
	if basePath != "" {
		cfg.Store.BasePath = basePath
	}

	// Try to get status from running server
	if !offlineMode {
		if statusFromServer(cfg, jsonOutput) {
			return
		}
	}

	// Fall back to reading files directly
	statusFromFiles(cfg, jsonOutput)
}

// statusFromServer attempts to get status from the running server.
// Returns true if successful, false if server is not reachable.
func statusFromServer(cfg *config.Config, jsonOutput bool) bool {
	// Build server URL
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	host := cfg.Server.Host
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, host, cfg.Server.Port)

	// Check if server is running with a health check
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Get list of stores
	resp, err = client.Get(baseURL + "/api/stores")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var storesResp struct {
		Stores []string `json:"stores"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&storesResp); err != nil {
		return false
	}

	type connectionInfo struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		URL    string `json:"url,omitempty"`
		Broker string `json:"broker_url,omitempty"`
		Topic  string `json:"topic,omitempty"`
	}

	type storeStatus struct {
		Name            string           `json:"name"`
		DataType        string           `json:"data_type"`
		NumBlocks       uint32           `json:"num_blocks"`
		DataBlockSize   uint32           `json:"data_block_size"`
		ActiveBlocks    uint32           `json:"active_blocks"`
		UsagePercent    float64          `json:"usage_percent"`
		OldestTime      string           `json:"oldest_time,omitempty"`
		NewestTime      string           `json:"newest_time,omitempty"`
		FileSizeBytes   uint64           `json:"file_size_bytes"`
		FileSizeHuman   string           `json:"file_size_human"`
		WSConnections   []connectionInfo `json:"ws_connections,omitempty"`
		MQTTConnections []connectionInfo `json:"mqtt_connections,omitempty"`
		Error           string           `json:"error,omitempty"`
	}

	stores := make([]storeStatus, 0)

	// For each store, we need to read files for size (API doesn't expose this)
	// and query API for stats and connections
	for _, storeName := range storesResp.Stores {
		status := storeStatus{Name: storeName}

		// Get stats from API (requires auth, so read from files instead)
		st, err := store.Open(cfg.Store.BasePath, storeName)
		if err != nil {
			status.Error = err.Error()
			stores = append(stores, status)
			continue
		}

		stats := st.Stats()
		st.Close()

		status.DataType = stats.DataType
		status.NumBlocks = stats.NumBlocks
		status.DataBlockSize = stats.DataBlockSize
		status.ActiveBlocks = stats.ActiveBlocks
		if stats.NumBlocks > 0 {
			status.UsagePercent = float64(stats.ActiveBlocks) / float64(stats.NumBlocks) * 100
		}
		status.OldestTime = stats.OldestTime
		status.NewestTime = stats.NewestTime

		// Calculate file size
		metaPath := filepath.Join(cfg.Store.BasePath, storeName, "meta.tsdb")
		dataPath := filepath.Join(cfg.Store.BasePath, storeName, "data.tsdb")
		indexPath := filepath.Join(cfg.Store.BasePath, storeName, "index.tsdb")
		var totalSize uint64
		if fi, err := os.Stat(dataPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		if fi, err := os.Stat(indexPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		if fi, err := os.Stat(metaPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		status.FileSizeBytes = totalSize
		status.FileSizeHuman = formatBytes(totalSize)

		// Load WS connections from file (API requires auth)
		if wsConns, err := st.LoadWSConnections(); err == nil && wsConns != nil {
			for _, conn := range wsConns.Connections {
				status.WSConnections = append(status.WSConnections, connectionInfo{
					ID:     conn.ID,
					Status: "configured",
					URL:    conn.URL,
				})
			}
		}

		// Load MQTT connections from file
		mqttConns := loadMQTTConnections(filepath.Join(cfg.Store.BasePath, storeName))
		for _, conn := range mqttConns {
			status.MQTTConnections = append(status.MQTTConnections, connectionInfo{
				ID:     conn.ID,
				Status: "configured",
				Broker: conn.BrokerURL,
				Topic:  conn.Topic,
			})
		}

		stores = append(stores, status)
	}

	if jsonOutput {
		output, _ := json.MarshalIndent(struct {
			ServerRunning bool          `json:"server_running"`
			ServerURL     string        `json:"server_url"`
			Stores        []storeStatus `json:"stores"`
		}{
			ServerRunning: true,
			ServerURL:     baseURL,
			Stores:        stores,
		}, "", "  ")
		fmt.Println(string(output))
		return true
	}

	// Text output
	fmt.Printf("=== Store Status ===\n")
	fmt.Printf("Server: %s (running)\n\n", baseURL)

	if len(stores) == 0 {
		fmt.Println("No stores open")
		return true
	}

	for _, s := range stores {
		fmt.Printf("Store: %s\n", s.Name)
		if s.Error != "" {
			fmt.Printf("  Error: %s\n", s.Error)
			fmt.Println()
			continue
		}
		fmt.Printf("  Type:         %s\n", s.DataType)
		fmt.Printf("  Blocks:       %s / %s (%.1f%% used)\n",
			formatNumber(uint64(s.ActiveBlocks)),
			formatNumber(uint64(s.NumBlocks)),
			s.UsagePercent)
		fmt.Printf("  Block size:   %s\n", formatBytes(uint64(s.DataBlockSize)))
		fmt.Printf("  Total size:   %s\n", s.FileSizeHuman)
		if s.OldestTime != "" {
			fmt.Printf("  Time range:   %s to %s\n", s.OldestTime, s.NewestTime)
		} else {
			fmt.Printf("  Time range:   (empty)\n")
		}
		if len(s.WSConnections) > 0 {
			fmt.Printf("  WS connections: %d\n", len(s.WSConnections))
			for _, c := range s.WSConnections {
				fmt.Printf("    - %s: %s\n", c.ID[:8], c.URL)
			}
		}
		if len(s.MQTTConnections) > 0 {
			fmt.Printf("  MQTT connections: %d\n", len(s.MQTTConnections))
			for _, c := range s.MQTTConnections {
				fmt.Printf("    - %s: %s -> %s\n", c.ID[:8], c.Broker, c.Topic)
			}
		}
		fmt.Println()
	}

	return true
}

// statusFromFiles reads store status directly from files.
func statusFromFiles(cfg *config.Config, jsonOutput bool) {
	// Discover stores by looking for directories with meta.tsdb
	entries, err := os.ReadDir(cfg.Store.BasePath)
	if err != nil {
		if os.IsNotExist(err) {
			if jsonOutput {
				fmt.Println(`{"server_running": false, "stores": []}`)
			} else {
				fmt.Printf("Server: not running\n")
				fmt.Printf("No stores found (data path: %s)\n", cfg.Store.BasePath)
			}
			return
		}
		log.Fatalf("Failed to read data directory: %v", err)
	}

	type connectionInfo struct {
		ID     string `json:"id"`
		URL    string `json:"url,omitempty"`
		Broker string `json:"broker_url,omitempty"`
		Topic  string `json:"topic,omitempty"`
	}

	type storeStatus struct {
		Name            string           `json:"name"`
		DataType        string           `json:"data_type"`
		NumBlocks       uint32           `json:"num_blocks"`
		DataBlockSize   uint32           `json:"data_block_size"`
		ActiveBlocks    uint32           `json:"active_blocks"`
		UsagePercent    float64          `json:"usage_percent"`
		OldestTime      string           `json:"oldest_time,omitempty"`
		NewestTime      string           `json:"newest_time,omitempty"`
		FileSizeBytes   uint64           `json:"file_size_bytes"`
		FileSizeHuman   string           `json:"file_size_human"`
		WSConnections   []connectionInfo `json:"ws_connections,omitempty"`
		MQTTConnections []connectionInfo `json:"mqtt_connections,omitempty"`
		Error           string           `json:"error,omitempty"`
	}

	stores := make([]storeStatus, 0)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		storeName := entry.Name()
		metaPath := filepath.Join(cfg.Store.BasePath, storeName, "meta.tsdb")

		// Check if this is a valid store directory
		if _, err := os.Stat(metaPath); os.IsNotExist(err) {
			continue
		}

		status := storeStatus{Name: storeName}

		// Open the store to get stats
		st, err := store.Open(cfg.Store.BasePath, storeName)
		if err != nil {
			status.Error = err.Error()
			stores = append(stores, status)
			continue
		}

		stats := st.Stats()

		status.DataType = stats.DataType
		status.NumBlocks = stats.NumBlocks
		status.DataBlockSize = stats.DataBlockSize
		status.ActiveBlocks = stats.ActiveBlocks
		if stats.NumBlocks > 0 {
			status.UsagePercent = float64(stats.ActiveBlocks) / float64(stats.NumBlocks) * 100
		}
		status.OldestTime = stats.OldestTime
		status.NewestTime = stats.NewestTime

		// Calculate file size
		dataPath := filepath.Join(cfg.Store.BasePath, storeName, "data.tsdb")
		indexPath := filepath.Join(cfg.Store.BasePath, storeName, "index.tsdb")
		var totalSize uint64
		if fi, err := os.Stat(dataPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		if fi, err := os.Stat(indexPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		if fi, err := os.Stat(metaPath); err == nil {
			totalSize += uint64(fi.Size())
		}
		status.FileSizeBytes = totalSize
		status.FileSizeHuman = formatBytes(totalSize)

		// Load WS connections from file
		if wsConns, err := st.LoadWSConnections(); err == nil && wsConns != nil {
			for _, conn := range wsConns.Connections {
				status.WSConnections = append(status.WSConnections, connectionInfo{
					ID:  conn.ID,
					URL: conn.URL,
				})
			}
		}

		// Load MQTT connections from file
		mqttConns := loadMQTTConnections(filepath.Join(cfg.Store.BasePath, storeName))
		for _, conn := range mqttConns {
			status.MQTTConnections = append(status.MQTTConnections, connectionInfo{
				ID:     conn.ID,
				Broker: conn.BrokerURL,
				Topic:  conn.Topic,
			})
		}

		st.Close()
		stores = append(stores, status)
	}

	if jsonOutput {
		output, _ := json.MarshalIndent(struct {
			ServerRunning bool          `json:"server_running"`
			Stores        []storeStatus `json:"stores"`
		}{
			ServerRunning: false,
			Stores:        stores,
		}, "", "  ")
		fmt.Println(string(output))
		return
	}

	// Text output
	fmt.Printf("=== Store Status ===\n")
	fmt.Printf("Server: not running\n\n")

	if len(stores) == 0 {
		fmt.Printf("No stores found (data path: %s)\n", cfg.Store.BasePath)
		return
	}

	for _, s := range stores {
		fmt.Printf("Store: %s\n", s.Name)
		if s.Error != "" {
			fmt.Printf("  Error: %s\n", s.Error)
			fmt.Println()
			continue
		}
		fmt.Printf("  Type:         %s\n", s.DataType)
		fmt.Printf("  Blocks:       %s / %s (%.1f%% used)\n",
			formatNumber(uint64(s.ActiveBlocks)),
			formatNumber(uint64(s.NumBlocks)),
			s.UsagePercent)
		fmt.Printf("  Block size:   %s\n", formatBytes(uint64(s.DataBlockSize)))
		fmt.Printf("  Total size:   %s\n", s.FileSizeHuman)
		if s.OldestTime != "" {
			fmt.Printf("  Time range:   %s to %s\n", s.OldestTime, s.NewestTime)
		} else {
			fmt.Printf("  Time range:   (empty)\n")
		}
		if len(s.WSConnections) > 0 {
			fmt.Printf("  WS connections: %d (configured)\n", len(s.WSConnections))
			for _, c := range s.WSConnections {
				fmt.Printf("    - %s: %s\n", c.ID[:8], c.URL)
			}
		}
		if len(s.MQTTConnections) > 0 {
			fmt.Printf("  MQTT connections: %d (configured)\n", len(s.MQTTConnections))
			for _, c := range s.MQTTConnections {
				fmt.Printf("    - %s: %s -> %s\n", c.ID[:8], c.Broker, c.Topic)
			}
		}
		fmt.Println()
	}
}
