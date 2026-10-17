package httpapi

import "net/http"

func health(service DataService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if service == nil {
			writeJSON(w, HealthReport{Status: "ok", Components: map[string]ComponentHealth{}})
			return
		}
		writeJSON(w, service.Health(r.Context()))
	}
}
