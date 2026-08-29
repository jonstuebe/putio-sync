package putiosync

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/cenkalti/log"
	"github.com/putdotio/putio-sync/v2/internal/progress"
)

// statusResponse is the body of GET /status. The status field predates the
// transfers list and keeps its shape, so anything already parsing it carries
// on working.
type statusResponse struct {
	Status    string           `json:"status"`
	Transfers []transferStatus `json:"transfers"`
}

type transferStatus struct {
	Path      string `json:"path"`
	Direction string `json:"direction"`
	Bytes     int64  `json:"bytes"`
	Total     int64  `json:"total"`
	Speed     int64  `json:"speed"`
	Percent   int    `json:"percent"`
}

// currentTransfers describes what is being transferred right now.
func currentTransfers() statusResponse {
	// Never nil: an empty list encodes as [] rather than null, so clients do
	// not need a special case for an idle sync.
	resp := statusResponse{Status: currentStatus(), Transfers: []transferStatus{}}
	reg := getRegistry()
	if reg == nil {
		return resp
	}
	for _, s := range reg.Snapshot().Active {
		direction := "download"
		if s.Direction == progress.Upload {
			direction = "upload"
		}
		resp.Transfers = append(resp.Transfers, transferStatus{
			Path:      s.RelPath,
			Direction: direction,
			Bytes:     s.Offset,
			Total:     s.Size,
			Speed:     s.Rate,
			Percent:   s.Percent(),
		})
	}
	return resp
}

const (
	serverReadTimeout     = 5 * time.Second
	serverWriteTimeout    = 10 * time.Second
	serverShutdownTimeout = 5 * time.Second
)

type httpServer struct {
	srv *http.Server
}

func newServer(addr string) *httpServer {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("putio-sync")) })
	m.HandleFunc("/syncing", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(fmt.Sprintf("%v", isSyncing()))) })
	m.HandleFunc("/trigger", func(w http.ResponseWriter, r *http.Request) { triggerSync() })
	m.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(currentTransfers())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	s := &httpServer{
		srv: &http.Server{
			Addr:         addr,
			Handler:      m,
			ReadTimeout:  serverReadTimeout,
			WriteTimeout: serverWriteTimeout,
		},
	}
	return s
}

func (s *httpServer) Close() {
	s.srv.Close()
}

func (s *httpServer) Start() {
	l, err := net.Listen("tcp4", s.srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Infoln("Server is listening on", l.Addr().String())
	go func() {
		if err := s.srv.Serve(l); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
}

func (s *httpServer) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
