// Package web is the local control UI and its JSON API.
package web

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"teufelcast/internal/cast"
)

//go:embed index.html
var index []byte

func Handler(svc *cast.Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		names, err := svc.Devices(r.Context(), r.URL.Query().Get("refresh") == "1")
		reply(w, names, err)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, svc.Status(), nil)
	})

	mux.HandleFunc("POST /api/start", func(w http.ResponseWriter, r *http.Request) {
		opts := svc.Status().Options // missing fields keep the last settings
		if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := svc.Start(opts)
		reply(w, svc.Status(), err)
	})

	mux.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		svc.Stop()
		reply(w, svc.Status(), nil)
	})

	mux.HandleFunc("GET /api/volume", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.Volume(r.Context(), r.URL.Query().Get("device"))
		reply(w, map[string]int{"volume": v}, err)
	})

	mux.HandleFunc("POST /api/volume", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Device string `json:"device"`
			Volume int    `json:"volume"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := svc.SetVolume(r.Context(), body.Device, body.Volume)
		reply(w, map[string]int{"volume": body.Volume}, err)
	})

	return mux
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		v = map[string]string{"error": err.Error()}
	}
	json.NewEncoder(w).Encode(v)
}
