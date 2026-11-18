// Command faketelegram stands in for the Telegram Bot API during end-to-end
// tests, appending every received message to a file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:18081", "listen address")
	path := flag.String("out", "telegram.log", "file that receives the messages")
	flag.Parse()

	file, err := os.OpenFile(*path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	var mu sync.Mutex
	http.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(file, "%s chat=%s %s\n", time.Now().Format(time.TimeOnly), body.ChatID, strings.ReplaceAll(body.Text, "\n", " | "))
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	server := &http.Server{Addr: *addr, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
