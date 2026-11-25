package binding

import (
	"fmt"
	"net/http"
	"strings"

	"sql-manager/internal/service"
)

// StreamBinding implements http.Handler mounted at /stream by Wails v3 ServiceOptions.
// Serves lazy-loaded BLOB and oversized text cell payloads without JSON encoding overhead.
type StreamBinding struct {
	qry *service.QueryService
}

func NewStreamBinding(qry *service.QueryService) *StreamBinding {
	return &StreamBinding{qry: qry}
}

func (s *StreamBinding) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.URL.Path, "/")
	ref = strings.TrimPrefix(ref, "stream/")
	if ref == "" {
		http.Error(w, "missing stream ref", http.StatusBadRequest)
		return
	}
	blob, ok := s.qry.GetStreamBlob(ref)
	if !ok {
		http.Error(w, "stream payload expired or not found", http.StatusNotFound)
		return
	}
	if blob.ContentType != "" {
		w.Header().Set("Content-Type", blob.ContentType)
	}
	if r.URL.Query().Get("download") == "1" && blob.Filename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", blob.Filename))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob.Data)
}
