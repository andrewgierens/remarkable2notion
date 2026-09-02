// notion-bridge is the on-device daemon for rm-notion. It owns everything
// that needs network, TLS, or file parsing, and exposes a JSON-RPC surface
// over a unix socket for the QML layer.
//
// It doubles as its own client: `notion-bridge -call <method> [-params JSON]`
// connects to the socket, performs one call, and prints the JSON response.
// The QML patch shells out to that via qt-command-executor.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/bridge"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/pair"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/socket"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/store"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/xochitl"
)

// version is stamped by the release build via -ldflags.
var version = "dev"

// defaultBroker is the hosted pairing broker; overridable for self-hosters.
const defaultBroker = "https://rmk2notion.tonytheprwn.dev"

func main() {
	var (
		socketPath  = flag.String("socket", "/run/notion-bridge.sock", "unix socket path")
		configDir   = flag.String("config-dir", defaultConfigDir(), "state directory (token, recents, QR)")
		dataDir     = flag.String("data-dir", xochitl.DefaultDir, "xochitl notebook store")
		brokerURL   = flag.String("broker", envOr("NOTION_BRIDGE_BROKER", defaultBroker), "pairing broker base URL")
		call        = flag.String("call", "", "client mode: method to call on the running daemon")
		params      = flag.String("params", "{}", "client mode: JSON params for -call")
		setToken    = flag.String("set-token", "", `store an integration token and exit; "-" reads it from stdin (paste-a-token fallback)`)
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *call != "" {
		if err := clientCall(*socketPath, *call, *params); err != nil {
			fmt.Fprintf(os.Stderr, `{"error":%q}`+"\n", err.Error())
			os.Exit(1)
		}
		return
	}

	st, err := store.New(*configDir)
	if err != nil {
		log.Fatalf("notion-bridge: config dir: %v", err)
	}

	if *setToken != "" {
		token := *setToken
		if token == "-" {
			// A token on the command line is visible in the process list and
			// in shell history, so "-" reads it from stdin instead. Prefer it.
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
			if err != nil {
				log.Fatalf("notion-bridge: read token from stdin: %v", err)
			}
			token = strings.TrimSpace(string(b))
			if token == "" {
				log.Fatal("notion-bridge: no token on stdin")
			}
		} else {
			fmt.Fprintln(os.Stderr,
				"notion-bridge: warning: a token passed as an argument is visible to other processes; use -set-token - to read it from stdin")
		}
		acc, err := st.AddAccount(token, "")
		if err != nil {
			log.Fatalf("notion-bridge: %v", err)
		}
		fmt.Printf("token stored as account %s\n", acc.ID)
		return
	}

	b := bridge.New(
		st,
		&xochitl.Store{Dir: *dataDir},
		pair.New(*brokerURL),
		bridge.DefaultQRPath(*configDir),
	)
	srv := socket.NewServer(*socketPath)
	b.RegisterAll(srv)
	if err := srv.Start(); err != nil {
		log.Fatalf("notion-bridge: %v", err)
	}
	log.Printf("notion-bridge %s listening on %s", version, *socketPath)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	if err := srv.Close(); err != nil {
		log.Printf("notion-bridge: shutdown: %v", err)
	}
}

// clientCall performs one request against the running daemon and prints the
// raw JSON response to stdout.
func clientCall(socketPath, method, params string) error {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("daemon not running at %s: %w", socketPath, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	if _, err := fmt.Fprintf(conn, `{"method":%q,"params":%s}`+"\n", method, params); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	_, err = os.Stdout.WriteString(line)
	return err
}

func defaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/root"
	}
	return filepath.Join(home, ".config", "notion-bridge")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
