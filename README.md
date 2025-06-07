./main.go contains a simple HTTP 2.0 static file server that can be run like:

`go run  ./main.go -addr=":8888" -docroot=/home/someUser/dirToServe`

or with TLS:

`go run  ./main.go -addr=":8888" -docroot=/home/someUser/dirToServe -tlsconfig=/home/someUser/llmahttap/example_tls_config/tls_config.json`

run

`./build_all_go_binaries.sh`

to build the full server binary, `./server`, which takes a more detailed config supporting route configurations.

The core server code, which could be used as a reference for how to use the library from other code, is in `internal/server/server.go`
