module github.com/sarcasticbird/wrap

go 1.26

// Pin the exact toolchain so CI, local builds, and every release retry
// compile with the same patch. A checked-out tag carries this pin, keeping
// its binaries and checksums reproducible even after the line moves.
toolchain go1.26.6

require (
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/dop251/goja v0.0.0-20260723142020-b4aef50fa347
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/mod v0.40.0
	golang.org/x/sys v0.36.0
)

require (
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.3.8 // indirect
)
