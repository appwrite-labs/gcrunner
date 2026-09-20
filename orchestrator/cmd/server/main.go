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
const shutdownTimeout = 8 * time.Second

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

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("ERROR: shutdown: %v", err)
	}
	orchestrator.ShutdownTelemetry(ctx)
}
