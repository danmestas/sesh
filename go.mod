module github.com/danmestas/sesh

go 1.26.0

require (
	github.com/a2aproject/a2a-go/v2 v2.3.1
	github.com/alecthomas/kong v1.15.0
	github.com/danmestas/EdgeSync v0.0.20
	github.com/danmestas/libfossil v0.6.3
	github.com/danmestas/sesh-ops v0.0.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/nats-io/nats-server/v2 v2.14.1
	github.com/nats-io/nats.go v1.51.0
	github.com/oklog/ulid/v2 v2.1.0
	golang.org/x/term v0.43.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/danmestas/libfossil/db/driver/modernc v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.46.1 // indirect
)

replace github.com/danmestas/EdgeSync => ../EdgeSync

replace github.com/danmestas/sesh-ops => ../sesh-ops
