package telemetry

import (
	"net/http"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/middleware"
)

func wrapWriter(w http.ResponseWriter) *middleware.ResponseWriter { return middleware.Wrap(w) }
