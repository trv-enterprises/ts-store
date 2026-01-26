// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package main is the entry point for the tsstore CLI.
package main

import (
	"context"
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
	case "help", "-h", "--help":
		printUsage()
	case "version", "-v", "--version":
		fmt.Println("tsstore v0.1.0")
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
  key       Manage API keys (requires device access)
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
  TSSTORE_SOCKET_PATH  Unix socket path (default: /var/run/tsstore/tsstore.sock)`)
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

			// WebSocket endpoints (inbound connections)
			// Auth is via query param for WebSocket connections
			storeRoutes.GET("/ws/read", wsHandler.Read)
			storeRoutes.GET("/ws/write", wsHandler.Write)

			// Outbound connection management
			wsConns := storeRoutes.Group("/ws/connections")
			{
				wsConns.GET("", wsConnHandler.List)
				wsConns.POST("", wsConnHandler.Create)
				wsConns.GET("/:id", wsConnHandler.Get)
				wsConns.DELETE("/:id", wsConnHandler.Delete)
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

	// Start HTTP server in goroutine
	go func() {
		log.Printf("Starting tsstore server on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
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
