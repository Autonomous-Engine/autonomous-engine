#!/bin/sh
# Generate self-signed TLS certificates for development
mkdir -p /certs
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout /certs/server.key \
  -out /certs/server.crt \
  -subj "/CN=autonomous-engine.local"
echo "Certificates generated in /certs/"
