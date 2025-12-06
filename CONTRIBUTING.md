# Contributing

## Prerequisites

- Go 1.25.0 or later
- Docker (for building container images)
- Make

## Building

Build the conduit binary with the LocalNet importer plugin:

```bash
make conduit
```

This can then be used for any manual testing. Alternatively the docker build can be used.

## Testing

Run all tests:

```bash
make test
```

## Code Formatting

Format Go source files:

```bash
make fmt
```

## Docker

Build the Docker image (defaults to `amd64` architecture):

```bash
make docker
```

Build for a specific architecture:

```bash
make docker ARCH=arm64
```

Customize image tag:

```bash
make docker IMAGE_TAG=local
```

This docker image can then be configured to be used within LocalNet for any manual testing that is required.
Typically the easiest way to do this is build the image with the `local` tag (see above) and then modify the conduit part of your LocalNet docker compose file like below:

```yaml
conduit:
  container_name: "algokit_sandbox_conduit"
  image: neilcampbell/conduit-localnet:local # The important part is the ":local" tag
  restart: unless-stopped
  volumes:
    - type: bind
      source: ./conduit.yml
      target: /etc/algorand/conduit.yml
  depends_on:
    - indexer-db
    - algod
```

## Release

Are performed via the GitHub actions workflow.
