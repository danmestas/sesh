module github.com/danmestas/sesh

go 1.26.0

require (
	github.com/a2aproject/a2a-go/v2 v2.3.1
	github.com/alecthomas/kong v1.15.0
	github.com/danmestas/sesh-ops v0.0.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/nats-io/nats-server/v2 v2.14.1
	github.com/nats-io/nats.go v1.51.0
	github.com/oklog/ulid/v2 v2.1.0
	golang.org/x/term v0.43.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/danmestas/sesh-ops => ../sesh-ops
