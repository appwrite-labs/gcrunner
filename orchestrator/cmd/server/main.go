package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	orchestrator "github.com/camdenclark/gcrunner/orchestrator"
)

// Cloud Run gives an instance ten seconds after SIGTERM before it is gone.
// Requests in flight get most of it; the final metrics push gets its own
// share so a slow VM operation cannot use it up.
const (
	drainTimeout = 6 * time.Second
	flushTimeout = 3 * time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/task/", orchestrator.HandleTask)
	http.HandleFunc("/", orchestrator.HandleWebhook)

	server := &http.Server{Addr: ":" + port}
	go func() {
		log.Printf("gcrunner listening on :%s", port)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop

	drain, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()
	if err := server.Shutdown(drain); err != nil {
		log.Printf("ERROR: shutdown: %v", err)
	}

	flush, cancelFlush := context.WithTimeout(context.Background(), flushTimeout)
	defer cancelFlush()
	orchestrator.ShutdownTelemetry(flush)
}
