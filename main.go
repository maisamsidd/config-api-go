package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// MetadataStore holds env_file_name -> key -> value, in memory,
// mirrored to a JSON file on disk for restart-safety.
type MetadataStore struct {
	mu   sync.RWMutex
	data map[string]map[string]string
	file string
}

func NewMetadataStore(file string) *MetadataStore {
	s := &MetadataStore{
		data: make(map[string]map[string]string),
		file: file,
	}
	s.load()
	return s
}

// load reads the persisted JSON file (if any) into memory at startup.
func (s *MetadataStore) load() {
	if _, err := os.Stat(s.file); os.IsNotExist(err) {
		log.Printf("no existing data file at %s, starting fresh", s.file)
		return
	}
	raw, err := os.ReadFile(s.file)
	if err != nil {
		log.Printf("warning: could not read data file: %v", err)
		return
	}
	if len(raw) == 0 {
		return
	}
	var loaded map[string]map[string]string
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Printf("warning: could not parse data file: %v", err)
		return
	}
	s.mu.Lock()
	s.data = loaded
	s.mu.Unlock()
	log.Printf("loaded %d env_file_name group(s) from %s", len(loaded), s.file)
}

// persist writes the current in-memory state to disk atomically
// (write to tmp file, then rename) so a crash mid-write can't corrupt data.
func (s *MetadataStore) persist() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

func (s *MetadataStore) Set(envFileName, key, value string) error {
	s.mu.Lock()
	if _, ok := s.data[envFileName]; !ok {
		s.data[envFileName] = make(map[string]string)
	}
	s.data[envFileName][key] = value
	s.mu.Unlock()
	return s.persist()
}

func (s *MetadataStore) Get(envFileName string) (map[string]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[envFileName]
	return v, ok
}

// ---- request payloads ----

// Single key/value add:
// {"env_file_name": "production", "key": "DB_HOST", "value": "localhost"}
type addOne struct {
	EnvFileName string `json:"env_file_name"`
	Key         string `json:"key"`
	Value       string `json:"value"`
}

// Bulk add (recommended for teams pushing a whole .env at once):
// {"env_file_name": "production", "data": {"DB_HOST": "localhost", "DB_PORT": "5432"}}
type addBulk struct {
	EnvFileName string            `json:"env_file_name"`
	Data        map[string]string `json:"data"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func main() {
	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "/mnt/data/verseye/api-config/metadata.json"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8060"
	}

	store := NewMetadataStore(dataFile)
	mux := http.NewServeMux()

	// ---------------------------------------------------------------
	// Endpoint 1: POST /api/metadata
	// Adds/updates key-value metadata under an env_file_name.
	// Accepts either a single key/value or a bulk "data" map.
	// ---------------------------------------------------------------
	mux.HandleFunc("/api/metadata", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "only POST is allowed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "cannot read request body")
			return
		}

		// Try bulk shape first ("data" map present).
		var bulk addBulk
		if err := json.Unmarshal(body, &bulk); err == nil && bulk.EnvFileName != "" && len(bulk.Data) > 0 {
			for k, v := range bulk.Data {
				if err := store.Set(bulk.EnvFileName, k, v); err != nil {
					writeErr(w, http.StatusInternalServerError, "persist error: "+err.Error())
					return
				}
			}
			writeJSON(w, http.StatusCreated, map[string]interface{}{
				"status":        "ok",
				"env_file_name": bulk.EnvFileName,
				"keys_written":  len(bulk.Data),
			})
			return
		}

		// Fall back to single key/value shape.
		var one addOne
		if err := json.Unmarshal(body, &one); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if one.EnvFileName == "" || one.Key == "" {
			writeErr(w, http.StatusBadRequest, "env_file_name and key are required")
			return
		}
		if err := store.Set(one.EnvFileName, one.Key, one.Value); err != nil {
			writeErr(w, http.StatusInternalServerError, "persist error: "+err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"status":        "ok",
			"env_file_name": one.EnvFileName,
			"key":           one.Key,
		})
	})

	// ---------------------------------------------------------------
	// Endpoint 2: GET /api/getmetadata/{env_file_name}
	// Lists all key-value metadata for that env_file_name as flat JSON,
	// e.g. {"DB_HOST":"localhost","DB_PORT":"5432"} — ready for `jq`.
	// ---------------------------------------------------------------
	mux.HandleFunc("/api/getmetadata/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "only GET is allowed")
			return
		}
		envFileName := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/getmetadata/"), "/")
		if envFileName == "" {
			writeErr(w, http.StatusBadRequest, "env_file_name is required in the path")
			return
		}
		data, ok := store.Get(envFileName)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("no metadata found for env_file_name %q", envFileName))
			return
		}
		writeJSON(w, http.StatusOK, data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Printf("metadata API listening on :%s (persisting to %s)", port, dataFile)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
