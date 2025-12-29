#!/bin/bash
set -e

# Build Docker image for doltswarm integration tests
# This script creates a temporary build context that includes doltswarm and its dependencies

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INTEGRATION_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DOLTSWARM_DIR="$(cd "$INTEGRATION_DIR/.." && pwd)"
CODE_DIR="$(cd "$DOLTSWARM_DIR/.." && pwd)"

BUILD_DIR=$(mktemp -d)

echo "Creating build context in $BUILD_DIR"
echo "  Code directory: $CODE_DIR"
echo "  Doltswarm directory: $DOLTSWARM_DIR"

# Copy doltswarm (includes integration/)
cp -r "$DOLTSWARM_DIR" "$BUILD_DIR/doltswarm"

# Copy dolt (required by replace directive)
if [ -d "$CODE_DIR/dolt" ]; then
    echo "  Copying dolt..."
    cp -r "$CODE_DIR/dolt" "$BUILD_DIR/dolt"
else
    echo "Error: dolt directory not found at $CODE_DIR/dolt"
    exit 1
fi

# Copy doltsqldriver (required by replace directive)
if [ -d "$CODE_DIR/doltsqldriver" ]; then
    echo "  Copying doltsqldriver..."
    cp -r "$CODE_DIR/doltsqldriver" "$BUILD_DIR/doltsqldriver"
else
    echo "Error: doltsqldriver directory not found at $CODE_DIR/doltsqldriver"
    exit 1
fi

# Build Docker image
echo "Building Docker image..."
docker build -t doltswarmdemo -f "$BUILD_DIR/doltswarm/integration/docker/Dockerfile" "$BUILD_DIR"

# Cleanup
echo "Cleaning up build context..."
rm -rf "$BUILD_DIR"

echo "Done! Image built successfully."
