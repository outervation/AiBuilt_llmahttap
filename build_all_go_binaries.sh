#!/usr/bin/env bash

# Exit on error
set -e

# Find all packages with `package main` and a `main()` function
for pkg in $(go list -f '{{.ImportPath}}:{{.Name}}' ./... | grep ':main' | cut -d: -f1); do
    # Derive binary name from the last path element
    BIN_NAME=$(basename "$pkg")
    echo "Building $pkg -> $BIN_NAME"
    go build -o "$BIN_NAME" "$pkg"
done

echo "✅ All binaries built t oproject root dir"
