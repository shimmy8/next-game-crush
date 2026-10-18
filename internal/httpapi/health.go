package httpapi

import "net/http"

func health(service DataService, demoEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if service == nil {
			writeJSON(w, HealthReport{Status: "ok", Components: map[string]ComponentHealth{}, DemoEnabled: demoEnabled})
			return
		}
		report := service.Health(r.Context())
		report.DemoEnabled = demoEnabled
		writeJSON(w, report)
	}
}
