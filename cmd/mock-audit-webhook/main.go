package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const (
	defaultMockListenAddress = ":9444"
	defaultMaxStoredEvents   = 200
	defaultReadTimeout       = 15 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

type config struct {
	listenAddress string
	maxStored     int
}

type storedEvent struct {
	Path      string             `json:"path"`
	RawBody   string             `json:"rawBody"`
	EventList auditv1.EventList  `json:"eventList"`
	Received  metav1TimeEnvelope `json:"received"`
}

type metav1TimeEnvelope struct {
	Time time.Time `json:"time"`
}

type eventStore struct {
	mu     sync.RWMutex
	events []storedEvent
	limit  int
}

func newEventStore(limit int) *eventStore {
	if limit <= 0 {
		limit = defaultMaxStoredEvents
	}

	return &eventStore{limit: limit}
}

func (s *eventStore) append(event storedEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, event)
	if len(s.events) > s.limit {
		s.events = append([]storedEvent(nil), s.events[len(s.events)-s.limit:]...)
	}
}

func (s *eventStore) list() []storedEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return append([]storedEvent(nil), s.events...)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		logger.Error("invalid flags", "error", err)
		os.Exit(2)
	}

	store := newEventStore(cfg.maxStored)

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", handleHealth)
	mux.HandleFunc("/readyz", handleHealth)
	mux.HandleFunc("/events", serveEvents(store))
	mux.HandleFunc("/", handleWebhook(store, logger))

	server := &http.Server{
		Addr:         cfg.listenAddress,
		Handler:      mux,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info(
		"starting mock audit webhook",
		"listen_address", cfg.listenAddress,
		"max_stored_events", cfg.maxStored,
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	cfg := config{}

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.listenAddress, "listen-address", defaultMockListenAddress, "Address for the mock audit webhook.")
	fs.IntVar(&cfg.maxStored, "max-stored-events", defaultMaxStoredEvents, "Maximum number of webhook payloads kept in memory.")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.maxStored <= 0 {
		return config{}, fmt.Errorf("--max-stored-events must be greater than zero")
	}

	return cfg, nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func serveEvents(store *eventStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		payload := struct {
			Items []storedEvent `json:"items"`
		}{
			Items: store.list(),
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}
}

func handleWebhook(store *eventStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var eventList auditv1.EventList
		if err := json.Unmarshal(body, &eventList); err != nil {
			http.Error(w, "invalid audit EventList: "+err.Error(), http.StatusBadRequest)
			return
		}

		store.append(storedEvent{
			Path:      r.URL.Path,
			RawBody:   string(body),
			EventList: eventList,
			Received: metav1TimeEnvelope{
				Time: time.Now().UTC(),
			},
		})

		logger.Info("stored webhook event list", "path", r.URL.Path, "events", len(eventList.Items))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
