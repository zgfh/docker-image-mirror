package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gorilla/mux"

	"docker-image-mirror/internal/storage"
)

type Server struct {
	storage *storage.Storage
}

func NewServer(stor *storage.Storage) *Server {
	return &Server{storage: stor}
}

func (s *Server) SetupRouter() *mux.Router {
	router := mux.NewRouter()

	router.HandleFunc("/v2/", s.handleV2Base).Methods("GET")
	router.HandleFunc("/v2/{image:.+}/manifests/{reference}", s.handleManifest).Methods("GET", "HEAD", "PUT")
	router.HandleFunc("/v2/{image:.+}/blobs/{digest}", s.handleBlob).Methods("GET", "HEAD")
	router.HandleFunc("/health", s.handleHealth).Methods("GET")

	return router
}

func (s *Server) handleV2Base(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "2.0")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	imagePath := vars["image"]
	reference := vars["reference"]

	route, err := ParseRoute("/v2/" + imagePath + "/manifests/" + reference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "GET", "HEAD":
		s.handleManifestGet(w, r, route)
	case "PUT":
		s.handleManifestPut(w, r, route)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleManifestGet(w http.ResponseWriter, r *http.Request, route *RouteInfo) {
	cachePath := route.GetFullPath() + "/manifests/" + route.Tag

	if s.storage.Exists(cachePath) {
		log.Printf("Cache hit: %s", cachePath)
		reader, err := s.storage.Get(cachePath)
		if err == nil {
			defer reader.Close()
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			io.Copy(w, reader)
			return
		}
	}

	// 如果缓存不存在，返回 404
	http.Error(w, "Manifest not found", http.StatusNotFound)
}

func (s *Server) handleManifestPut(w http.ResponseWriter, r *http.Request, route *RouteInfo) {
	cachePath := route.GetFullPath() + "/manifests/" + route.Tag

	if err := s.storage.Put(cachePath, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Saved manifest: %s", cachePath)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	imagePath := vars["image"]
	digest := vars["digest"]

	route, err := ParseRoute("/v2/" + imagePath + "/blobs/" + digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cachePath := route.GetFullPath() + "/blobs/" + digest

	if s.storage.Exists(cachePath) {
		log.Printf("Cache hit: %s", cachePath)
		reader, err := s.storage.Get(cachePath)
		if err == nil {
			defer reader.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			io.Copy(w, reader)
			return
		}
	}

	http.Error(w, "Blob not found", http.StatusNotFound)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}
