package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

//go:embed index.html
var htmlContent embed.FS

// configFile is the path to the main BroCode configuration.
func configFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".brocode", "config.jsonc")
}

// StartDashboard launches the local web server with auto-incrementing port logic
func StartDashboard() {
	startPort := 3201
	maxPort := 3210
	var ln net.Listener
	var err error

	for port := startPort; port <= maxPort; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			startPort = port
			break
		}
	}

	if err != nil {
		fmt.Printf("❌ Failed to start web dashboard, all ports %d-%d are in use.\n", 3201, maxPort)
		return
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/", startPort)

	fmt.Println("!  BROCODE_SERVER_PASSWORD is not set; server is unsecured.")
	fmt.Println()
	fmt.Println("  █▀▀▄ █▀▀█ █▀▀█ █▀▀█ █▀▀█ █▀▀▄ █▀▀")
	fmt.Println("  █▀▀▄ █▄▄▀ █  █ █    █  █ █  █ █▀▀")
	fmt.Println("  ▀▀▀  ▀ ▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀  ▀▀▀")
	fmt.Println()
	fmt.Printf("  Web interface:      %s\n", url)
	fmt.Println()

	// Handlers
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		data, _ := htmlContent.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html")
		w.Write(data)
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			data, err := os.ReadFile(configFile())
			if err != nil {
				w.Write([]byte(`{
  "$schema": "https://brocode.dev/config.schema.json",
  "providers": {}
}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		if r.Method == http.MethodPost {
			var input map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}

			// Load existing config or create new
			config := make(map[string]interface{})
			data, err := os.ReadFile(configFile())
			if err == nil {
				_ = json.Unmarshal(data, &config)
			}

			// Merge input (shallow merge at root level for now)
			for k, v := range input {
				config[k] = v
			}

			// Ensure schema
			config["$schema"] = "https://brocode.dev/config.schema.json"

			out, _ := json.MarshalIndent(config, "", "  ")
			_ = os.MkdirAll(filepath.Dir(configFile()), 0o700)
			_ = os.WriteFile(configFile(), out, 0o600)

			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"success"}`))
			return
		}
		http.Error(w, "Method Not Allowed", 405)
	})

	// Open browser
	go openBrowser(url)

	http.Serve(ln, nil)
}

func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin":
		cmd = "open"
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
	}
	args = append(args, url)
	_ = exec.Command(cmd, args...).Start()
}
