module github.com/chargetronic/imatra.iso15118_server/engine

go 1.26.11

// golang.org/x/* from their official GitHub mirrors (same code; builds in networks
// that reach GitHub but not golang.org).
replace golang.org/x/net => github.com/golang/net v0.44.0

replace golang.org/x/sync => github.com/golang/sync v0.17.0

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/lib/pq v1.12.3
)

require (
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/redis/go-redis/v9 v9.23.0
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
)
