./main.go contains a simple HTTP 2.0 static file server that can be run like:

`go run  ./main.go -addr=":8888" -docroot=/home/someUser/dirToServe`

or with TLS:

`go run  ./main.go -addr=":8888" -docroot=/home/someUser/dirToServe -tlsconfig=/home/someUser/llmahttap/example_tls_config/tls_config.json`

run

`./build_all_go_binaries.sh`

to build the full server binary, ./server, which takes a more detailed config supporting route configurations.

./internal/server/server.go contains the core server code.
